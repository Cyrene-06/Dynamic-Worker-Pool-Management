package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 请求体上限。64 KiB 远超任何合法请求（最长的字段是 egress 列表），
// 而它挡住了"用超大 body 把内存吃光"这一类最廉价的攻击。
const maxBodyBytes int64 = 64 << 10

// 请求路径前缀。
const apiPrefix = "/v1/sandboxes"

// Server 是接入层的 HTTP 处理器。
type Server struct {
	Store        *Store
	Auth         Authenticator
	Quotas       QuotaTable
	Limiter      *RateLimiter
	QuotaChecker *QuotaChecker
	Metrics      *Metrics

	// CreateTimeout 是申请请求的整体超时。
	//
	// 必须有：申请包含一次集群写（CAS），而集群写入会因 API Server 限流
	// 而长时间挂起。没有超时的 handler 会一直占用连接，
	// 在过载时表现为"连接数涨满、所有请求都变慢"，也就是雪崩的起点。
	CreateTimeout time.Duration

	// IsAdmin 判定一个身份是否有管理员权限。
	//
	// 池水位与排空是**管理员**操作，而不是租户操作：租户只应当能操作
	// 自己的沙箱。把它们与租户接口混在同一个鉴权下，会让任何租户
	// 都能排空整个池 —— 那是一个能造成全平台故障的越权。
	// 为空时管理员接口一律拒绝（默认拒绝，而不是默认放行）。
	IsAdmin func(Principal) bool
}

// NewServer 用合理默认值构造。
func NewServer(store *Store) *Server {
	return &Server{
		Store:         store,
		Limiter:       NewRateLimiter(),
		Quotas:        &StaticQuotaTable{},
		Metrics:       NewMetrics(),
		CreateTimeout: 5 * time.Second,
	}
}

// Handler 返回带中间件的处理器。
func (s *Server) Handler() http.Handler {
	return s.recoverMiddleware(s.logMiddleware(s.route))
}

// route 是路由分发。
//
// # 为什么自己解析路径而不是用 http.ServeMux
//
// API 契约（docs/04 §8）里的动作式路径是 `POST /v1/sandboxes/{id}:renew`。
// Go 1.22 的 ServeMux 要求通配符占满整个路径段，因此无法表达
// "{id} 后面紧接一个冒号后缀"。契约是对外的、已写进文档，不能因为
// 实现方便就改 —— 那需要一次业务侧同步修改，而收益只是少写这几十行。
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")

	// 探针与指标不需要鉴权：它们不暴露任何租户数据，
	// 而要求鉴权会让 kubelet 的探针与 Prometheus 抓取都需要配凭据 ——
	// 那些凭据最终会被写进各种配置文件，得不偿失。
	switch path {
	case "/healthz":
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	case "/readyz":
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
		return
	case "/metrics":
		s.Metrics.ServeHTTP(w, r)
		return
	}

	requestID := requestIDOf(r)
	w.Header().Set("X-Request-Id", requestID)

	// 鉴权必须在任何业务逻辑之前，且失败默认拒绝。
	principal, err := s.Auth.Authenticate(headerView{req: r})
	if err != nil {
		s.Metrics.Observe("unauthorized", 0)
		writeError(w, requestID, errUnauthorized(err.Error()))
		return
	}

	var aerr *APIError
	switch {
	case path == apiPrefix:
		if r.Method != http.MethodPost {
			aerr = errInvalidSpec("申请沙箱请使用 POST /v1/sandboxes")
			break
		}
		aerr = s.handleCreate(w, r, principal, requestID)

	case strings.HasPrefix(path, apiPrefix+"/"):
		rest := strings.TrimPrefix(path, apiPrefix+"/")
		id, action := splitAction(rest)
		if id == "" {
			aerr = errInvalidSpec("缺少沙箱 ID")
			break
		}
		aerr = s.handleSandboxAction(w, r, principal, requestID, id, action)

	case path == "/v1/pools":
		if r.Method != http.MethodGet {
			aerr = errInvalidSpec("查看池水位请使用 GET /v1/pools")
			break
		}
		aerr = s.callAdmin(principal)
		if aerr == nil {
			aerr = s.handleListPools(w)
		}

	case strings.HasPrefix(path, "/v1/pools/"):
		rest := strings.TrimPrefix(path, "/v1/pools/")
		name, action := splitAction(rest)
		if name == "" || action != "drain" {
			aerr = errInvalidSpec("仅支持 POST /v1/pools/{name}:drain")
			break
		}
		if r.Method != http.MethodPost {
			aerr = errInvalidSpec("排空池请使用 POST")
			break
		}
		aerr = s.callAdmin(principal)
		if aerr == nil {
			aerr = s.handleDrainPool(w, r, name)
		}

	default:
		aerr = errNotFound("未知路径 " + path)
	}

	if aerr != nil {
		writeError(w, requestID, aerr)
	}
}

// callAdmin 校验管理员权限。
//
// 判据来自配置而不是"租户名是不是 admin"：后者会把一个特殊字符串
// 变成权限，而字符串是会被人猜到的。权限必须来自显式的授权数据。
func (s *Server) callAdmin(p Principal) *APIError {
	if s.IsAdmin == nil {
		return errForbidden("管理员接口未配置授权")
	}
	if !s.IsAdmin(p) {
		return errForbidden("该操作需要管理员权限")
	}
	return nil
}

// splitAction 把 "abc:renew" 拆成 ("abc", "renew")；没有冒号时 action 为空。
//
// 只在**最后一个**冒号处拆分：沙箱 ID 里可能含有冒号（GenerateName 生成的
// 名字不会，但对象名是人类可读的，未来可能变化），从第一个冒号拆会把
// 一个含冒号的 ID 误判成"ID + 动作"，从而把一个合法请求变成 404。
func splitAction(rest string) (id, action string) {
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		return rest[:i], rest[i+1:]
	}
	return rest, ""
}

// handleSandboxAction 分发单个沙箱上的操作。
func (s *Server) handleSandboxAction(
	w http.ResponseWriter, r *http.Request, p Principal, requestID, id, action string,
) *APIError {
	ctx := r.Context()

	switch action {
	case "":
		if r.Method != http.MethodGet {
			return errInvalidSpec("查询沙箱请使用 GET")
		}
		resp, aerr := s.Store.Get(ctx, p, id)
		if aerr != nil {
			return aerr
		}
		writeJSON(w, http.StatusOK, resp)
		return nil

	case "renew":
		if r.Method != http.MethodPost {
			return errInvalidSpec("续租请使用 POST")
		}
		resp, aerr := s.Store.Renew(ctx, p, id)
		if aerr != nil {
			return aerr
		}
		writeJSON(w, http.StatusOK, resp)
		return nil

	case "release":
		if r.Method != http.MethodPost {
			return errInvalidSpec("释放请使用 POST")
		}
		resp, aerr := s.Store.Release(ctx, p, id)
		if aerr != nil {
			return aerr
		}
		writeJSON(w, http.StatusOK, resp)
		return nil

	case "hibernate", "wake":
		if r.Method != http.MethodPost {
			return errInvalidSpec("休眠/唤醒请使用 POST")
		}
		resp, aerr := s.Store.SetHibernation(ctx, p, id, action == "hibernate")
		if aerr != nil {
			return aerr
		}
		writeJSON(w, http.StatusOK, resp)
		return nil

	default:
		return errNotFound("未知动作 :" + action)
	}
}

// handleCreate 处理申请沙箱。
func (s *Server) handleCreate(
	w http.ResponseWriter, r *http.Request, p Principal, requestID string,
) *APIError {
	// Idempotency-Key 是**可选**的，但它的存在与否语义完全不同，必须说清：
	//
	//	提供 → 平台保证同一键只产生一次认领（重试安全）
	//	不提供 → 每次请求都是一次新的申请，重试会拿到**两个**沙箱
	//
	// 我们用"空 RequestID"表示第二种情况，而不是自动生成一个随机值。
	// 自动生成会让 label 上出现每次请求都不同的值 —— 那是 label 基数爆炸，
	// 而且它还会让幂等看起来"有"（实际并没有），比没有更糟。
	idemKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))

	var req CreateRequest
	if aerr := decodeBody(r, &req); aerr != nil {
		return aerr
	}
	if req.Pool == "" {
		return errInvalidSpec("pool 为必填")
	}
	if req.TTLSeconds > 0 && req.TTLSeconds < 30 {
		// 30s 是 CRD 的 Minimum。提前在这里拒绝，能让业务拿到
		// "参数不对，别重试"而不是一个来自 API Server 的原始校验错误 ——
		// 后者会被业务当成平台故障并触发重试。
		return errInvalidSpec("ttlSeconds 最小为 30")
	}
	if aerr := validateIdempotencyKey(idemKey); aerr != nil {
		return aerr
	}

	// 配额与限流的顺序是有讲究的：**先限流再查配额**。
	//
	// 配额检查需要一次集群读取，而限流是纯内存操作。把便宜的放前面，
	// 就能让被限流的请求根本不产生那次读取 —— 在过载场景下，
	// 这恰恰是最需要省下的东西。
	quota, _ := s.Quotas.Quota(p.Tenant)
	if ok, wait := s.Limiter.Allow(p.Tenant, quota); !ok {
		s.Metrics.Observe("rate_limited", 0)
		return errRateLimited(
			fmt.Sprintf("租户 %s 的创建速率超限", p.Tenant), wait)
	}
	if s.QuotaChecker != nil {
		if aerr := s.QuotaChecker.Check(r.Context(), p.Tenant, quota); aerr != nil {
			s.Metrics.Observe("quota_exceeded", 0)
			return aerr
		}
	}

	// 认领是有网络往返的操作，必须有自己的超时；超时后由业务按
	// 错误语义重试（幂等键保证重试是安全的）。
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout())
	defer cancel()

	resp, aerr := s.Store.ClaimWarm(ctx, p, req, idemKey)
	if aerr == nil {
		s.Metrics.Observe("created", http.StatusCreated)
		writeJSON(w, http.StatusCreated, resp)
		return nil
	}

	// 只有"池确实空了"才考虑冷路径。竞争（409）时库存可能还在，
	// 走冷路径会白花一次冷启动并让命中率指标偏低 —— 那会误导池水位调参
	// 去解决一个并不存在的问题。
	if aerr.Code != CodePoolExhausted {
		s.Metrics.Observe("failed", aerr.Status)
		return aerr
	}
	if !allowColdPath(req) {
		s.Metrics.Observe("pool_exhausted", http.StatusServiceUnavailable)
		return aerr
	}

	cold, cerr := s.Store.CreateCold(ctx, p, req, idemKey)
	if cerr != nil {
		s.Metrics.Observe("failed", cerr.Status)
		return cerr
	}
	s.Metrics.Observe("created", http.StatusCreated)
	writeJSON(w, http.StatusCreated, cold)
	return nil
}

// allowColdPath 解析冷路径意愿，缺省为允许。
func allowColdPath(req CreateRequest) bool {
	return req.AllowColdPath == nil || *req.AllowColdPath
}

// handleListPools 返回全部池水位。
func (s *Server) handleListPools(w http.ResponseWriter) *APIError {
	pools, aerr := s.Store.ListPools(context.Background())
	if aerr != nil {
		return aerr
	}
	writeJSON(w, http.StatusOK, map[string]any{"pools": pools})
	return nil
}

// handleDrainPool 排空池。
func (s *Server) handleDrainPool(w http.ResponseWriter, r *http.Request, name string) *APIError {
	var body struct {
		Reason string `json:"reason"`
	}
	// body 可以为空（只发一个 POST 也算合法请求），因此解码失败不报错。
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes)).Decode(&body)
	}
	if body.Reason == "" {
		body.Reason = "未说明原因"
	}
	resp, aerr := s.Store.DrainPool(r.Context(), name, body.Reason)
	if aerr != nil {
		return aerr
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

func (s *Server) timeout() time.Duration {
	if s.CreateTimeout > 0 {
		return s.CreateTimeout
	}
	return 5 * time.Second
}

// ---- 中间件 ----

// logMiddleware 记录访问日志。
func (s *Server) logMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next(rec, r)
		s.Metrics.ObserveRequest(r.URL.Path, rec.status, time.Since(started))
	}
}

// recoverMiddleware 兜住 panic。
//
// 一个 handler 里的 nil 解引用如果没被兜住，会杀掉整个进程 ——
// 也就是说一个租户能用一个畸形请求让所有租户的服务中断。
// 兜住之后它只是一个 500，而且堆栈会被记录下来用于修复。
func (s *Server) recoverMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				// 刻意不把 panic 内容返回给客户端：它可能包含内部结构信息。
				writeError(w, requestIDOf(r), errInternal("内部错误"))
			}
		}()
		next(w, r)
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// headerView 让鉴权只依赖"取一个头"这个能力（见 httpRequest 的说明）。
type headerView struct{ req *http.Request }

func (h headerView) Header(name string) string { return h.req.Header.Get(name) }

// requestIDOf 取或生成请求 ID。
//
// 优先回显业务提供的 X-Request-Id：这样才能把业务侧的日志与平台侧的
// 日志对上，而这正是"全链路追踪"在排障时唯一的实际用法。
func requestIDOf(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Request-Id")); v != "" {
		return v
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 随机失败时退回时间戳：请求 ID 的唯一性要求很低（只是便于检索），
		// 而"没有 ID"会让日志无法关联，代价更大。
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// validateIdempotencyKey 校验幂等键可用作 label 值。
//
// 必须**显式拒绝**而不是截断或忽略：它会写进对象 label，同时又是幂等
// 查找的键。静默截断会让两个不同的键变成同一个（重试吃掉第二个沙箱），
// 静默忽略则会让"我提供了幂等键"变成一句谎话。
func validateIdempotencyKey(key string) *APIError {
	if key == "" {
		return nil
	}
	if len(key) > 63 {
		return errInvalidSpec("Idempotency-Key 最长 63 字符（会作为 Kubernetes label 值）")
	}
	for _, ch := range key {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
		case ch == '-' || ch == '_' || ch == '.':
		default:
			return errInvalidSpec("Idempotency-Key 只允许字母、数字与 -_.")
		}
	}
	return nil
}

// decodeBody 解析请求体。
func decodeBody(r *http.Request, out any) *APIError {
	if r.Body == nil {
		return errInvalidSpec("请求体不能为空")
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
	// 拒绝未知字段：拼错的字段名（如 ttlsecond）如果不报错，
	// 就会被静默忽略，而业务会以为自己设置了 TTL —— 直到沙箱按默认值
	// 提前消失，那时才发现字段名拼错了。早报错胜过晚困惑。
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return errInvalidSpec("请求体过大")
		}
		return errInvalidSpec("请求体不是合法 JSON 或包含未知字段: " + err.Error())
	}
	return nil
}
