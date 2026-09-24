package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
	"github.com/Cyrene-06/Dynamic-Worker-Pool-Management/internal/claim"
)

// ---- 请求 / 响应 DTO ----

// CreateRequest 是申请沙箱的请求体（docs/04 §8）。
type CreateRequest struct {
	// Pool 是目标池名。必填。
	Pool string `json:"pool"`
	// Tier / Isolation 为空时取池的取值。
	//
	// 允许业务指定这两个字段是刻意的：池按"隔离级别 + 档位"划分，
	// 而业务对自己的负载需求（内存大小、是否需要强隔离）最清楚。
	// 但**不允许**越过池的允许范围 —— 那由池与模板的一致性校验兜住。
	Tier      string `json:"tier,omitempty"`
	Isolation string `json:"isolation,omitempty"`

	SessionID string `json:"sessionId,omitempty"`

	// TTLSeconds 是期望的存活时长。为空时取模板默认值。
	TTLSeconds int32 `json:"ttlSeconds,omitempty"`
	// HardDeadline 是业务侧 SLA 兜底，优先于平台推导的 TTL。
	HardDeadline *time.Time `json:"hardDeadline,omitempty"`

	PriorityClassName string `json:"priorityClassName,omitempty"`

	// AllowColdPath 为 nil 时按 true 处理（业务默认希望拿到沙箱）。
	//
	// 允许业务显式关闭冷路径是有意义的：批处理类负载在池空时
	// 宁可直接失败去用自有环境，也不愿意等一次 2.5s 的冷启动 +
	// 可能长达数分钟的节点扩容。
	AllowColdPath *bool `json:"allowColdPath,omitempty"`
}

// Access 是沙箱的接入信息。
type Access struct {
	Endpoint string `json:"endpoint"`
	// Token 是短期接入令牌。
	//
	// 需要明确：**当前仓库里还没有校验它的数据面代理**。
	// 因此它现在是一份供业务自建代理时使用的凭据，而不是一道已生效的防线。
	// 把它当作"已经安全了"是危险的，所以这里如实说明而不是含糊过去。
	Token    string `json:"token"`
	Protocol string `json:"protocol"`
}

// SandboxResponse 是沙箱的对外表示。
type SandboxResponse struct {
	// ID 是平台对象名，也是后续所有操作的路径参数。
	//
	// 它同时是接入令牌绑定的对象（token 的 binding 对应本字段），
	// 因为它在你拿到响应的那一刻就一定存在。
	ID string `json:"id"`
	// SandboxID 是控制面生成的稳定句柄（status.sandboxID），用于审计与
	// 业务侧日志关联。
	//
	// 它在**职责上**与 ID 分开：池中库存转正时两者都不变，因此业务
	// 不需要在认领后更新自己持有的任何东西。但它在时刻上有差别 ——
	// 控制器写入 status 需要一次 reconcile，因此刚认领时它可能还是空的，
	// 所以后续请求请一律使用 ID。
	SandboxID string `json:"sandboxId,omitempty"`
	Phase     string `json:"phase,omitempty"`

	Access Access `json:"access"`

	Isolation string `json:"isolation,omitempty"`
	Tier      string `json:"tier,omitempty"`

	ExpiresAt *time.Time `json:"expiresAt,omitempty"`

	// Path 是 warm / cold，用于业务侧统计自己的命中率。
	Path string `json:"path,omitempty"`
	// ClaimLatencyMs 是认领耗时。业务能看到它才能判断自己的退避是否合理。
	ClaimLatencyMs int64 `json:"claimLatencyMs"`

	// RenewIntervalSeconds 是期望的续租间隔，业务按它设置心跳定时器。
	RenewIntervalSeconds int `json:"renewIntervalSeconds,omitempty"`

	// RecycleReason 仅在沙箱已被回收（410）时填充。
	RecycleReason string `json:"recycleReason,omitempty"`
}

// PoolResponse 是池水位的对外表示（管理员接口）。
type PoolResponse struct {
	Name               string `json:"name"`
	Isolation          string `json:"isolation,omitempty"`
	Tier               string `json:"tier,omitempty"`
	Warm               int32  `json:"warm"`
	Inflight           int32  `json:"inflight"`
	Claimed            int32  `json:"claimed"`
	Failed             int32  `json:"failed"`
	Draining           int32  `json:"draining"`
	Target             int32  `json:"target"`
	SaturationPermille int32  `json:"saturationPermille"`
	HitRatioPermille   int32  `json:"hitRatioPermille"`
	DrainingEnabled    bool   `json:"drainEnabled"`
}

// ---- Store ----

// Store 是 gateway 与集群交互的唯一入口。
//
// 把所有集群读写集中在一个类型里，是为了让"gateway 会改哪些东西"
// 成为一个可以一眼看完的问题。分散在各 handler 里的 client 调用
// 会让这件事只能靠全局搜索来回答 —— 而权限审计恰恰需要这个答案。
type Store struct {
	Client client.Client
	// Namespace 是沙箱所在命名空间，默认 sandbox-pool。
	Namespace string
	// Claimer 执行 CAS 认领。
	Claimer *claim.Claimer
	// Issuer 签发接入令牌。可为空（则返回空令牌）。
	Issuer *AccessTokenIssuer
	// SandboxPort / Protocol 描述业务应该连到沙箱的哪个端口。
	//
	// 用配置而不是从 CRD 读：数据面的端口约定属于接入层，
	// 把它塞进 CRD 会让"改个端口"变成一次 CRD 变更与滚动升级。
	SandboxPort int
	Protocol    string

	// HeartbeatIntervalSeconds 是返回给业务的心跳间隔建议。
	//
	// 由 main 从 controller.HeartbeatLeaseDurationSeconds 注入，而不是
	// 在本包再定义一份：那是同一个契约的两端，两处定义必然会在某次
	// 改动后不一致，而症状是"客户端按 60s 续租、服务端 30s 就判离线"——
	// 表现为随机时刻的沙箱被误回收，极难定位到常量不一致上。
	HeartbeatIntervalSeconds int32

	Now func() time.Time
}

// NewStore 用合理默认值构造 Store。
func NewStore(c client.Client, issuer *AccessTokenIssuer) *Store {
	return &Store{
		Client:      c,
		Namespace:   sandboxv1alpha1.NamespacePool,
		Issuer:      issuer,
		SandboxPort: 7788,
		Protocol:    "ws",
	}
}

func (s *Store) ns() string {
	if s.Namespace != "" {
		return s.Namespace
	}
	return sandboxv1alpha1.NamespacePool
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// ClaimWarm 尝试从池中认领库存。
//
// 返回值的 nil error 但非 nil APIError 表示"期望内的失败"（池空/竞争），
// 这类失败必须与真正的异常分开：异常要 500 并告警，期望内的失败要
// 让业务知道该退避还是该立即重试。
func (s *Store) ClaimWarm(ctx context.Context, p Principal, req CreateRequest, requestID string) (*SandboxResponse, *APIError) {
	started := s.now()

	// 雪崩保护优先于认领：保护期内低优申请一律不受理，包括从池里认领。
	// 库存要留给高优业务 —— "池里还有货"并不能回答"现在是不是该继续放量"。
	if aerr := s.protectGate(ctx, req.Pool, req.PriorityClassName); aerr != nil {
		return nil, aerr
	}

	var hardDeadline *metav1.Time
	if req.HardDeadline != nil {
		hd := metav1.NewTime(*req.HardDeadline)
		hardDeadline = &hd
	}

	res, err := s.claimer().Claim(ctx, claim.Request{
		Pool:              req.Pool,
		Tenant:            p.Tenant,
		SessionID:         req.SessionID,
		Principal:         p.Subject,
		RequestID:         requestID,
		PriorityClassName: req.PriorityClassName,
		HardDeadline:      hardDeadline,
	})
	latency := s.now().Sub(started).Milliseconds()

	switch {
	case err == nil:
		out, aerr := s.describe(ctx, res.Sandbox, claim.PathWarm, latency)
		if aerr != nil {
			return nil, aerr
		}
		return out, nil

	case errors.Is(err, claim.ErrContended):
		// 竞争太激烈：库存**可能还在**，因此绝不能引导业务去走冷路径。
		// 让业务立即重试（幂等键保证安全）比让它退避更有效 ——
		// 冲突通常源于瞬时争抢，退避只会把延迟拉长。
		return nil, errContended(200 * time.Millisecond)

	case errors.Is(err, claim.ErrPoolExhausted):
		// 池确实空了。这一条由调用方决定是否走冷路径。
		return nil, errPoolExhausted(2 * time.Second)
	}
	return nil, errInternal("认领失败: " + err.Error())
}

func (s *Store) claimer() *claim.Claimer {
	if s.Claimer != nil {
		return s.Claimer
	}
	return &claim.Claimer{Client: s.Client, Namespace: s.ns()}
}

// protectRetryAfter 是保护模式期间建议的退避时长（文档值：Retry-After: 10）。
const protectRetryAfter = 10 * time.Second

// protectGate 在池处于雪崩保护时拒绝低优申请（docs/05 §5），返回 nil 表示放行。
//
// 判定读的是**池的 status**，而不是在接入层自己算一遍：同一个事实只能有一个
// 来源。两边各算必然会在某一轮出现分歧，而分歧的表现形式（控制器认为已恢复、
// gateway 仍在拒绝）从任一组件都解释不了，而排查它要花掉危机里最宝贵的时间。
//
// 它能放在热路径上的前提：读池走的是 manager 的**缓存** client（cmd/gateway
// 用 manager 而不是裸 client 就是为了拿到带缓存的读），因此这里不会给
// API Server 增加每请求一次的额外压力 —— 而那恰恰是保护模式要缓解的东西。
//
// 失败方向刻意是**开放**的：读不到池、状态未知、优先级未知，一律放行。
// 误拒一个正常业务（可用性事故）比放行一个批处理（多占一点冷路径容量）
// 严重得多，因此这里与"跨租户复用硬编码禁止"那类安全边界取相反方向。
func (s *Store) protectGate(ctx context.Context, poolName, priorityClass string) *APIError {
	if !sandboxv1alpha1.IsLowPriorityClass(priorityClass) {
		return nil
	}

	var pool sandboxv1alpha1.SandboxPool
	if err := s.Client.Get(ctx, types.NamespacedName{Name: poolName}, &pool); err != nil {
		// 读不到（含 NotFound）时放行：让下游路径去给出"池不存在"这类准确的错误，
		// 而不是在这里把两种原因混成一个 503。
		return nil
	}
	if !pool.Status.ProtectMode.Active {
		return nil
	}
	return errProtectMode(protectRetryAfter, pool.Status.ProtectMode.Reason)
}

// CreateCold 创建一个直接归属于业务的新沙箱（冷路径）。
//
// 它与热路径的关键差别：**不经过 Ready 阶段**。控制器看到 claim 非空
// 就会把它推进到 Running，因此不存在"刚建好就被别人认领"的窗口。
func (s *Store) CreateCold(ctx context.Context, p Principal, req CreateRequest, requestID string) (*SandboxResponse, *APIError) {
	started := s.now()

	// 冷路径是保护模式最想拦住的那条路：它会在容量危机里继续向 API Server
	// 与节点施压，而正是这个压力让系统自我放大。
	if aerr := s.protectGate(ctx, req.Pool, req.PriorityClassName); aerr != nil {
		return nil, aerr
	}

	var pool sandboxv1alpha1.SandboxPool
	if err := s.Client.Get(ctx, types.NamespacedName{Name: req.Pool}, &pool); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errInvalidSpec(fmt.Sprintf("池 %q 不存在", req.Pool))
		}
		return nil, errInternal("读取池失败: " + err.Error())
	}
	if pool.Spec.Drain.Enabled {
		// 排空中的池不接受新申请。若在这里放行，排空就永远无法完成 ——
		// 一边在回收、一边在新建，水位永远降不下去。
		return nil, errPoolExhausted(30 * time.Second)
	}
	if pool.Spec.Degradation.DisableColdPath {
		// 池显式禁用了冷路径（例如节点容量不足时，新建只会让 Pod 卡在 Pending）。
		// 返回 507 而不是 503：这不是"暂时没货"，而是"现在加不出来"，
		// 运维需要按容量问题去排查，两者不该混为一谈。
		return nil, errInsufficientCapacity(
			fmt.Sprintf("池 %q 已禁用冷路径（degradation.disableColdPath=true）", req.Pool))
	}

	tier := pool.Spec.Tier
	if req.Tier != "" {
		tier = req.Tier
	}
	isolation := pool.Spec.Isolation
	if req.Isolation != "" {
		isolation = sandboxv1alpha1.IsolationLevel(req.Isolation)
	}
	runtimeClassName := ""
	if isolation == pool.Spec.Isolation {
		runtimeClassName = pool.Spec.RuntimeClassName
	}

	deadline := req.HardDeadline
	if deadline == nil && req.TTLSeconds > 0 {
		// 把 TTL 同时表达为硬期限：冷路径的沙箱是"为这次申请专门建的"，
		// 业务给出的时长就是它的契约，不存在"留给下一位使用者"的余地。
		d := started.Add(time.Duration(req.TTLSeconds) * time.Second)
		deadline = &d
	}

	var hardDeadline *metav1.Time
	if deadline != nil {
		hd := metav1.NewTime(*deadline)
		hardDeadline = &hd
	}

	lifecycle := &sandboxv1alpha1.LifecycleSpec{}
	if req.TTLSeconds > 0 {
		lifecycle.TTLSecondsAfterCreation = req.TTLSeconds
	}

	sbx := &sandboxv1alpha1.AgentSandbox{
		ObjectMeta: metav1.ObjectMeta{
			// GenerateName 而不是自己拼名字：名字唯一性交给 API Server 保证，
			// 自己做就必然要处理"重名重试"，而那是一个纯自找的竞态。
			GenerateName: req.Pool + "-",
			Namespace:    s.ns(),
			Labels: map[string]string{
				sandboxv1alpha1.LabelPool:      req.Pool,
				sandboxv1alpha1.LabelTier:      tier,
				sandboxv1alpha1.LabelTemplate:  pool.Spec.TemplateRef.Name,
				sandboxv1alpha1.LabelIsolation: string(isolation),
				sandboxv1alpha1.LabelRole:      sandboxv1alpha1.RoleSandbox,
				// 一开始就带上租户标签：冷路径不存在"先库存后认领"的过程，
				// 因此 claimed=true 与 tenant 必须在创建时就正确 ——
				// 否则配额计数会在一个窗口内看不到这个沙箱。
				sandboxv1alpha1.LabelClaimed: "true",
				sandboxv1alpha1.LabelTenant:  p.Tenant,
			},
		},
		Spec: sandboxv1alpha1.AgentSandboxSpec{
			PoolRef:          sandboxv1alpha1.NameRef{Name: req.Pool},
			TemplateRef:      sandboxv1alpha1.NameRef{Name: pool.Spec.TemplateRef.Name},
			Tier:             tier,
			Isolation:        isolation,
			RuntimeClassName: runtimeClassName,
			Lifecycle:        lifecycle,
			Claim: &sandboxv1alpha1.ClaimSpec{
				RequestedBy: sandboxv1alpha1.ClaimRequestedBy{
					Tenant:    p.Tenant,
					SessionID: req.SessionID,
					Principal: p.Subject,
					RequestID: requestID,
				},
				PriorityClassName: req.PriorityClassName,
				HardDeadline:      hardDeadline,
			},
		},
	}
	if requestID != "" {
		sbx.Labels[sandboxv1alpha1.LabelRequestID] = requestID
	}

	if err := s.Client.Create(ctx, sbx); err != nil {
		// 对象已存在说明幂等键命中了同一个名字 —— 这在 GenerateName 下
		// 几乎不可能发生，因此归类为异常而不是静默复用。
		return nil, errInternal("创建沙箱失败: " + err.Error())
	}
	return s.describe(ctx, sbx, claim.PathCold, s.now().Sub(started).Milliseconds())
}

// Get 按对象名查询沙箱。
//
// # 跨租户返回 404 而不是 403
//
// 403 会确认"这个 ID 存在，只是不归你"，那等于提供了一个枚举他人沙箱 ID
// 的接口。在一个多租户平台上，对象**存在性**本身就是需要保护的信息
// （沙箱数量、命名规律、某个租户是否在跑任务）。
func (s *Store) Get(ctx context.Context, p Principal, id string) (*SandboxResponse, *APIError) {
	sbx, aerr := s.ownedSandbox(ctx, p, id)
	if aerr != nil {
		return nil, aerr
	}
	return s.describe(ctx, sbx, "", 0)
}

// ownedSandbox 读取沙箱并校验租户归属。
func (s *Store) ownedSandbox(ctx context.Context, p Principal, id string) (*sandboxv1alpha1.AgentSandbox, *APIError) {
	if id == "" {
		return nil, errInvalidSpec("沙箱 ID 不能为空")
	}
	var sbx sandboxv1alpha1.AgentSandbox
	err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.ns(), Name: id}, &sbx)
	if apierrors.IsNotFound(err) {
		return nil, errNotFound("沙箱 " + id + " 不存在")
	}
	if err != nil {
		return nil, errInternal("读取沙箱失败: " + err.Error())
	}

	// 已回收的对象在删除完成前仍可能被读到。返回 410 让业务能走
	// "重建并恢复状态"的流程，而不是把它当成"ID 打错了"。
	if reason := s.goneReason(&sbx); reason != "" {
		return nil, errGone(reason)
	}

	if sbx.Labels[sandboxv1alpha1.LabelTenant] != p.Tenant {
		return nil, errNotFound("沙箱 " + id + " 不存在")
	}
	return &sbx, nil
}

// goneReason 判断沙箱是否已被回收，并返回原因码。
func (s *Store) goneReason(sbx *sandboxv1alpha1.AgentSandbox) string {
	if !sbx.DeletionTimestamp.IsZero() {
		if r := sbx.Status.Metrics.RecycleReason; r != "" {
			return string(r)
		}
		return "Terminating"
	}
	switch sbx.Status.Phase {
	case sandboxv1alpha1.PhaseTerminating:
		return string(sbx.Status.Metrics.RecycleReason)
	case sandboxv1alpha1.PhaseFailed:
		if r := sbx.Status.Metrics.RecycleReason; r != "" {
			return string(r)
		}
		return string(sandboxv1alpha1.RecycleRuntimeError)
	case sandboxv1alpha1.PhaseSucceeded:
		return string(sbx.Status.Metrics.RecycleReason)
	}
	return ""
}

// Release 显式释放沙箱。
//
// 实现方式是**清空 spec.claim**，而不是删除对象。这个区别很重要：
// 释放是"我不要了"，而不是"请你立刻销毁" —— 由控制器决定何时真正回收
// （它还要处理 Finalizer 链、状态落盘、指标上报）。
// 让 gateway 直接删对象会绕过整套清理流程。
func (s *Store) Release(ctx context.Context, p Principal, id string) (*SandboxResponse, *APIError) {
	sbx, aerr := s.ownedSandbox(ctx, p, id)
	if aerr != nil {
		return nil, aerr
	}
	if sbx.Spec.Claim == nil {
		// 已释放：幂等成功而不是报错。业务重试释放是常态
		// （例如它没收到第一次响应），报错会制造出无意义的告警。
		return s.describe(ctx, sbx, "", 0)
	}

	base := sbx.DeepCopy()
	sbx.Spec.Claim = nil
	// 用乐观锁：并发释放 + 控制器改写 spec 时，无条件覆盖会丢掉控制器的改动。
	patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
	if err := s.Client.Patch(ctx, sbx, patch); err != nil {
		if apierrors.IsConflict(err) {
			// 冲突说明状态刚变过。让业务重试比在这里循环重试更好：
			// handler 里循环会占用连接，而业务重试释放是廉价且幂等的。
			return nil, errContended(100 * time.Millisecond)
		}
		return nil, errInternal("释放沙箱失败: " + err.Error())
	}
	sbx.Spec.Claim = nil
	return s.describe(ctx, sbx, "", 0)
}

// Renew 续租：刷新心跳 Lease 的续租时间。
//
// # 为什么只改 Lease 而不改沙箱
//
// 心跳的语义是"客户端还活着"。把它写进沙箱对象会带来两个问题：
// 一是沙箱对象成为高频写热点（5k 沙箱 × 每 60s 一次 = 83 写/秒，
// 且全部落在同一个 CRD 上，风险表 R7 正是这一点）；
// 二是它会让控制器的 status 写入与心跳写入互相冲突。
// Lease 是为此专门存在的对象，用它就对了。
func (s *Store) Renew(ctx context.Context, p Principal, id string) (*SandboxResponse, *APIError) {
	sbx, aerr := s.ownedSandbox(ctx, p, id)
	if aerr != nil {
		return nil, aerr
	}
	if sbx.Spec.Claim == nil {
		// 已释放的沙箱没有心跳可续。返回 410 而不是 404：
		// 它不是不存在，而是已经结束 —— 业务应当停止心跳定时器，
		// 而不是去排查自己是不是打错了 ID。
		return nil, errGone("已释放")
	}

	var lease coordinationv1.Lease
	key := types.NamespacedName{Namespace: sbx.Namespace, Name: leaseName(sbx)}
	if err := s.Client.Get(ctx, key, &lease); err != nil {
		if apierrors.IsNotFound(err) {
			// Lease 还没被控制器创建出来（认领与建 Lease 之间有一个短暂窗口）。
			// 这不是错误：业务应当稍后重试续租，而不是认为沙箱挂了。
			// 用 409 而不是 503 —— 它能在极短时间内自愈，业务立即重试即可。
			return nil, errContended(200 * time.Millisecond)
		}
		return nil, errInternal("读取心跳 Lease 失败: " + err.Error())
	}

	// 校验 Lease 归属：Lease 名与沙箱名相同且可预测，若不校验，
	// 租户 A 只要能猜到名字就能续租租户 B 的心跳 —— 那等于让 A
	// 把自己的沙箱续死（B 永远被判为"还活着"）。
	if lease.Labels[sandboxv1alpha1.LabelTenant] != p.Tenant {
		return nil, errNotFound("沙箱 " + id + " 不存在")
	}

	now := metav1.NewMicroTime(s.now())
	lease.Spec.RenewTime = &now
	// 续租不加乐观锁：心跳是幂等的"告诉平台我还活着"，
	// 两个并发的续租谁赢都一样，加锁只会让心跳在竞争下失败 ——
	// 而心跳失败会被判为"客户端离线"，那是最不该被竞争影响的判定。
	if err := s.Client.Update(ctx, &lease); err != nil && !apierrors.IsConflict(err) {
		return nil, errInternal("续租失败: " + err.Error())
	}

	return s.describe(ctx, sbx, "", 0)
}

// SetHibernation 处理手工休眠/唤醒请求。
func (s *Store) SetHibernation(ctx context.Context, p Principal, id string, hibernate bool) (*SandboxResponse, *APIError) {
	sbx, aerr := s.ownedSandbox(ctx, p, id)
	if aerr != nil {
		return nil, aerr
	}
	if sbx.Spec.Claim == nil {
		return nil, errGone("已释放")
	}

	base := sbx.DeepCopy()
	if sbx.Annotations == nil {
		sbx.Annotations = map[string]string{}
	}
	if hibernate {
		sbx.Annotations[sandboxv1alpha1.AnnoHibernateRequested] = "true"
		// 两个标记互斥：同时存在时控制器以唤醒为准（见 observe 的说明），
		// 但留着矛盾的标记会让 status 与注解读起来互相打架，
		// 排障时第一件事就是困惑"到底哪个算数"。
		delete(sbx.Annotations, sandboxv1alpha1.AnnoWakeRequested)
	} else {
		sbx.Annotations[sandboxv1alpha1.AnnoWakeRequested] = "true"
		delete(sbx.Annotations, sandboxv1alpha1.AnnoHibernateRequested)
	}
	if err := s.Client.Patch(ctx, sbx, client.MergeFrom(base)); err != nil {
		if apierrors.IsConflict(err) {
			return nil, errContended(100 * time.Millisecond)
		}
		return nil, errInternal("写入休眠请求失败: " + err.Error())
	}
	return s.describe(ctx, sbx, "", 0)
}

// ListPools 返回池水位。
func (s *Store) ListPools(ctx context.Context) ([]PoolResponse, *APIError) {
	var list sandboxv1alpha1.SandboxPoolList
	if err := s.Client.List(ctx, &list); err != nil {
		return nil, errInternal("列出池失败: " + err.Error())
	}
	out := make([]PoolResponse, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, poolView(&list.Items[i]))
	}
	return out, nil
}

// DrainPool 请求排空池。
//
// 写 spec.drain 而不是注解：控制器已经在处理这个字段，
// 再用一个注解表达同一件事会产生"两个真相"，而它们迟早会不一致。
func (s *Store) DrainPool(ctx context.Context, name, reason string) (*PoolResponse, *APIError) {
	var pool sandboxv1alpha1.SandboxPool
	if err := s.Client.Get(ctx, types.NamespacedName{Name: name}, &pool); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errNotFound("池 " + name + " 不存在")
		}
		return nil, errInternal("读取池失败: " + err.Error())
	}
	if pool.Spec.Drain.Enabled {
		v := poolView(&pool)
		return &v, nil // 幂等
	}

	base := pool.DeepCopy()
	pool.Spec.Drain.Enabled = true
	pool.Spec.Drain.Reason = reason
	if err := s.Client.Patch(ctx, &pool, client.MergeFrom(base)); err != nil {
		if apierrors.IsConflict(err) {
			return nil, errContended(100 * time.Millisecond)
		}
		return nil, errInternal("排空池失败: " + err.Error())
	}
	v := poolView(&pool)
	return &v, nil
}

func poolView(pool *sandboxv1alpha1.SandboxPool) PoolResponse {
	return PoolResponse{
		Name:               pool.Name,
		Isolation:          string(pool.Spec.Isolation),
		Tier:               pool.Spec.Tier,
		Warm:               pool.Status.Warm,
		Inflight:           pool.Status.Inflight,
		Claimed:            pool.Status.Claimed,
		Failed:             pool.Status.Failed,
		Draining:           pool.Status.Draining,
		Target:             pool.Status.Target,
		SaturationPermille: pool.Status.SaturationPermille,
		HitRatioPermille:   pool.Status.HitRatio1hPermille,
		DrainingEnabled:    pool.Spec.Drain.Enabled,
	}
}

// describe 把沙箱对象组装成对外表示。
//
// 它会尝试解析 Pod IP 以给出 endpoint。解析失败时 endpoint 留空而不是
// 编造一个：一个连不上的 endpoint 比一个空 endpoint 更难排查 ——
// 空值会被业务立刻发现"接入信息缺失"，假地址则会让它去查网络。
func (s *Store) describe(
	ctx context.Context,
	sbx *sandboxv1alpha1.AgentSandbox,
	path string,
	claimLatencyMs int64,
) (*SandboxResponse, *APIError) {
	out := &SandboxResponse{
		ID:                   sbx.Name,
		SandboxID:            sbx.Status.SandboxID,
		Phase:                strings.ToLower(string(sbx.Status.Phase)),
		Isolation:            string(sbx.Status.IsolationLevel),
		Tier:                 sbx.Spec.Tier,
		Path:                 path,
		ClaimLatencyMs:       claimLatencyMs,
		RenewIntervalSeconds: int(s.heartbeatInterval()),
		RecycleReason:        string(sbx.Status.Metrics.RecycleReason),
	}
	if out.Isolation == "" {
		out.Isolation = string(sbx.Spec.Isolation)
	}
	out.Access.Protocol = s.Protocol

	if sbx.Status.ClaimRef != nil && sbx.Status.ClaimRef.HardDeadline != nil {
		t := sbx.Status.ClaimRef.HardDeadline.Time
		out.ExpiresAt = &t
	} else if sbx.Spec.Claim != nil && sbx.Spec.Claim.HardDeadline != nil {
		t := sbx.Spec.Claim.HardDeadline.Time
		out.ExpiresAt = &t
	}

	// 接入令牌绑定**对象名**而不是 status.sandboxID。
	//
	// 原实现绑定 sandboxID，而那个字段要等控制器在下一轮 reconcile
	// 才写进 status —— 于是申请响应里根本没有令牌，业务拿到了一个
	// 无法接入的沙箱。而"令牌必须立刻可用"是热路径的全部意义。
	//
	// 对象名满足同样的两个要求：认领当刻就存在，且池中库存转正时
	// 就地不变（INV-5 的实质），因此它是一个合格的绑定对象。
	// 数据面验证令牌时的规则由此明确为：binding 对应 metadata.name。
	if s.Issuer != nil {
		tenant := sbx.Labels[sandboxv1alpha1.LabelTenant]
		token, _, err := s.Issuer.Issue(tenant, sbx.Name)
		if err == nil {
			out.Access.Token = token
		}
	}

	if sbx.Status.PodName != "" {
		var pod corev1.Pod
		if err := s.Client.Get(ctx,
			types.NamespacedName{Namespace: sbx.Namespace, Name: sbx.Status.PodName}, &pod); err == nil {
			if pod.Status.PodIP != "" && s.SandboxPort > 0 {
				out.Access.Endpoint = fmt.Sprintf("%s:%d", pod.Status.PodIP, s.SandboxPort)
			}
		}
	}
	return out, nil
}

// leaseName 与控制器侧的命名约定必须一致（与沙箱同名）。
func leaseName(sbx *sandboxv1alpha1.AgentSandbox) string { return sbx.Name }

// heartbeatInterval 返回心跳间隔，未注入时用 60s 兜底。
func (s *Store) heartbeatInterval() int32 {
	if s.HeartbeatIntervalSeconds > 0 {
		return s.HeartbeatIntervalSeconds
	}
	return 60
}
