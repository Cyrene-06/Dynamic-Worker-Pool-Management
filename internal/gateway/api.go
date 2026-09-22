// Package gateway 实现业务接入层（docs/04 §8）。
//
// # 为什么业务不直接操作 Kubernetes
//
// Kubernetes RBAC 无法表达"只能看到属于自己租户的对象"。把多租户授权
// 放在 API Server 上做不到，只能放在一个能理解租户语义的中间层 ——
// 那就是本包。业务拿到的是一组窄接口（申请/续租/释放/查询），
// 而 CRD 保持为**平台内部对象**。
//
// # 它在延迟路径上的位置
//
// 热路径（池命中）的目标是 P95 < 800ms，而认领必须在这个时间内完成。
// 因此本包的职责被刻意压缩成三件事：鉴权、配额预检、CAS 绑定。
// 任何需要"等一等"的逻辑（补货、收敛、清理）都必须留给异步的控制器 ——
// 把它们放进请求路径是超出 800ms 预算最常见的原因。
package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// APIError 是业务可见的错误。
//
// # 错误语义是这个 API 最重要的部分
//
// 业务需要的不是一个"失败了"的信号，而是**该做什么**。因此每个错误都携带
// 可执行的动作：重试（带退避时长）、立即重试、修正请求、或放弃并重建。
// 一个只会返回 500 的 API 会把所有故障都变成"客户端自己猜"，
// 而客户端的猜法（无脑重试或直接放弃）通常都与实际需要的相反。
type APIError struct {
	// Status 是 HTTP 状态码。
	Status int
	// Code 是稳定的机器可读标识。它不随措辞变化，业务可以据此分支。
	Code string
	// Message 是给人看的一句话。
	Message string
	// RetryAfter > 0 时会带上 Retry-After 头（秒）。
	RetryAfter time.Duration
	// Details 是可选的补充字段（如 recycleReason）。
	Details map[string]string
}

// Error 实现 error。
func (e *APIError) Error() string {
	return fmt.Sprintf("gateway: %d %s: %s", e.Status, e.Code, e.Message)
}

// 稳定错误码。列出全部取值而不是散落在各处内联字符串：
// 这些标识是对外契约，业务会按它写分支逻辑，改一个字母就是破坏兼容。
const (
	CodeUnauthorized     = "unauthenticated"
	CodeForbidden        = "forbidden"
	CodeInvalidSpec      = "invalid_spec"
	CodeNotFound         = "not_found"
	CodeGone             = "gone"
	CodeContended        = "contended"
	CodeQuotaExceeded    = "quota_exceeded"
	CodeRateLimited      = "rate_limited"
	CodePoolExhausted    = "pool_exhausted"
	CodeInsufficientCap  = "insufficient_capacity"
	CodeInternal         = "internal"
	CodeIdempotencyReuse = "idempotency_key_reuse"
)

// errUnauthorized 构造 401。
//
// 401 与 403 的区别必须严格保持：前者是"你没证明你是谁"（换 token 就行），
// 后者是"你是谁我清楚了，但你不能做这件事"（换 token 没用）。
// 混用会让业务侧的错误处理（重试 vs 报权限问题）完全错位。
func errUnauthorized(msg string) *APIError {
	return &APIError{Status: http.StatusUnauthorized, Code: CodeUnauthorized, Message: msg}
}

// errForbidden 构造 403。
func errForbidden(msg string) *APIError {
	return &APIError{Status: http.StatusForbidden, Code: CodeForbidden, Message: msg}
}

// errInvalidSpec 构造 422。
//
// 用 422 而不是 400：请求语法是对的（JSON 可解析、字段名正确），
// 但语义上不允许（档位不存在、TTL 超上限）。这个区别对业务很重要 ——
// 400 会让人去查"我的请求格式哪里错了"，而问题其实在参数取值。
// 两者都不应重试，因此共用 422 是可接受的简化。
func errInvalidSpec(msg string) *APIError {
	return &APIError{Status: http.StatusUnprocessableEntity, Code: CodeInvalidSpec, Message: msg}
}

// errNotFound 构造 404。
func errNotFound(msg string) *APIError {
	return &APIError{Status: http.StatusNotFound, Code: CodeNotFound, Message: msg}
}

// errGone 构造 410，携带回收原因。
//
// 410 而不是 404 是刻意的：业务需要区分"这个 ID 从来不存在"（可能是自己
// 拼错了）与"它存在过但已被平台回收"（应当重建并恢复状态）。
// 后者的正确反应是恢复流程，前者的正确反应是不要重试。
func errGone(reason string) *APIError {
	return &APIError{
		Status:  http.StatusGone,
		Code:    CodeGone,
		Message: "沙箱已被回收",
		Details: map[string]string{"recycleReason": reason},
	}
}

// errContended 构造 409。
//
// 409 与 503 的区别决定了业务会做什么，而两者都表示"现在拿不到"：
//
//   - 409（竞争激烈，库存可能还在）→ 业务应当**立即**重试（幂等键保证安全）
//   - 503（池确实空了）→ 业务应当退避，或降级到自有环境
//
// 把它们混为一谈，会在池里还有库存时让业务退避（白等），
// 或者在池真的空了时让业务疯狂重试（放大故障）。
func errContended(retry time.Duration) *APIError {
	return &APIError{
		Status:     http.StatusConflict,
		Code:       CodeContended,
		Message:    "认领冲突，内部重试已耗尽",
		RetryAfter: retry,
	}
}

// errQuotaExceeded 构造 429（并发配额）。
func errQuotaExceeded(msg string, retry time.Duration) *APIError {
	return &APIError{
		Status:     http.StatusTooManyRequests,
		Code:       CodeQuotaExceeded,
		Message:    msg,
		RetryAfter: retry,
	}
}

// errRateLimited 构造 429（创建速率）。
//
// 与 errQuotaExceeded 同为 429 但码不同：两者对业务的正确反应是一样的
// （退避重试），但**对运维的排查方向完全相反**——一个要调配额，
// 一个要查是不是有失控的循环在刷接口。用同一个码就会让这两件事
// 在告警里混为一谈。
func errRateLimited(msg string, retry time.Duration) *APIError {
	return &APIError{
		Status:     http.StatusTooManyRequests,
		Code:       CodeRateLimited,
		Message:    msg,
		RetryAfter: retry,
	}
}

// errPoolExhausted 构造 503。
func errPoolExhausted(retry time.Duration) *APIError {
	return &APIError{
		Status:     http.StatusServiceUnavailable,
		Code:       CodePoolExhausted,
		Message:    "池中无可用库存且未启用冷路径",
		RetryAfter: retry,
	}
}

// errInsufficientCapacity 构造 507。
//
// 507 与 503 的差别是容量问题的**层级**：503 是"这个池暂时没货"，
// 507 是"集群整体加不出新机器了"。运维需要按这个区分去排查
// （调池参数 vs 查 Karpenter / 配额 / 云上容量），因此必须是不同的码。
func errInsufficientCapacity(msg string) *APIError {
	return &APIError{Status: http.StatusInsufficientStorage, Code: CodeInsufficientCap, Message: msg}
}

// errInternal 构造 500。
func errInternal(msg string) *APIError {
	return &APIError{Status: http.StatusInternalServerError, Code: CodeInternal, Message: msg}
}

// errorBody 是错误响应体。
type errorBody struct {
	Error struct {
		Code    string            `json:"code"`
		Message string            `json:"message"`
		Retry   int               `json:"retryAfterSeconds,omitempty"`
		Details map[string]string `json:"details,omitempty"`
	} `json:"error"`
	// RequestID 回显给业务，用于把一次失败对上平台侧的日志与追踪。
	RequestID string `json:"requestId,omitempty"`
}

// writeError 把 APIError 写成 HTTP 响应。
func writeError(w http.ResponseWriter, requestID string, err *APIError) {
	var body errorBody
	body.Error.Code = err.Code
	body.Error.Message = err.Message
	body.Error.Details = err.Details
	body.RequestID = requestID

	if err.RetryAfter > 0 {
		// 向上取整到秒：向上是安全方向 —— Retry-After 报小了会让客户端
		// 在服务端还没准备好时又打回来，形成额外的负载。
		secs := int((err.RetryAfter + time.Second - 1) / time.Second)
		body.Error.Retry = secs
		// Retry-After 头同时用秒数形式给出：部分客户端只认头，不认 body。
		w.Header().Set("Retry-After", strconv.Itoa(secs))
	}
	writeJSON(w, err.Status, body)
}

// writeJSON 写出 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// 编码失败时响应已开始写出，无法再改状态码。这里只能忽略 ——
	// 但入参全部是本包定义的 DTO，编码失败意味着代码有 bug，
	// 而那种情况会在单元测试里以"body 为空"的形式暴露，不会被漏掉。
	_ = json.NewEncoder(w).Encode(payload)
}
