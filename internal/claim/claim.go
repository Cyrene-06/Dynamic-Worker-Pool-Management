// Package claim 实现"把池中某个空闲沙箱排他地绑定给一次会话"。
//
// # 为什么它必须用 CAS（乐观锁）
//
// 这是整个项目**唯一的强一致临界区**。
//
// 池化把"创建沙箱"的成本降到了接近零，代价是把竞争从 Kubernetes 调度器
// 转移到了"谁拿到这个库存"上。若这里出错，两个申请会同时成功并拿到同一个
// 沙箱 —— 那是跨租户数据泄漏级别的故障，不是性能问题。
//
// 我们刻意不使用中心化分配器（一个分配服务维护"谁拿了哪个"）：它会成为
// 吞吐瓶颈与单点，而且要在它内部重新实现一套状态机。用 API Server 的
// resourceVersion 做乐观锁则天然分布式、可水平扩展，且复用 Kubernetes
// 本身已被验证过的一致性机制。
//
// # 交互契约
//
// 本包只改 `spec.claim` 与 metadata（label），**不改 status**。
// `status.phase` 由 SandboxController 在下一个 reconcile 里推进
// （Ready → Running）。两个写入者同时改 status 会互相覆盖 ——
// 这类 bug 表现为"状态偶尔跳变"，极难定位。
package claim

import (
	"context"
	"errors"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
)

// 认领路径。它进指标 label，因此是枚举。
const (
	// PathWarm 表示命中库存（热路径），这是常态。
	PathWarm = "warm"
	// PathCold 表示池空，需要新建（冷路径）。它应由 gateway 在
	// 收到 ErrPoolExhausted 后走单独流程处理，不在本包的职责内。
	PathCold = "cold"
)

// ErrPoolExhausted 表示候选库存已经**全部**确认不可用。
//
// 它是**期望内的结果**而不是异常：gateway 据此返回 503 + Retry-After，
// 或者对高优先级申请走冷路径。必须与"认领过程出错"严格区分 ——
// 把两者混为一谈会让"池空了"表现为"系统故障"，触发错误的告警与重试策略。
var ErrPoolExhausted = errors.New("sandbox pool exhausted")

// ErrContended 表示竞争太激烈：尝试预算用尽时库存**可能还有剩余**。
//
// 它与 ErrPoolExhausted 必须分开，理由很具体：若把竞争失败当成池空，
// 调用方就会去走冷路径新建沙箱 —— 而库里其实还躺着库存。
// 后果是双重的：白花一次冷启动的成本，并且让命中率指标偏低，
// 进而误导池水位参数调优（水位会为了一个不存在的问题而调高）。
//
// 调用方应对它做**退避重试**，而不是降级到冷路径。
var ErrContended = errors.New("sandbox pool contended, retry")

// DefaultMaxAttempts 是单次认领最多尝试多少个候选。
//
// 设上限而不是"一直试到成功"：池被高并发抢购时，候选列表可能每一个
// 都已被别人拿走。无上限地重试会把请求时间拖到客户端超时，
// 而超时又会触发客户端重试，形成放大。快速失败让上层能正确退避。
const DefaultMaxAttempts = 16

// Request 是一次认领请求。
type Request struct {
	// Pool 是目标池名（对应 SandboxPool.metadata.name）。
	Pool string
	// Tenant 是租户标识。
	Tenant string
	// SessionID / Principal 用于审计与问题定位。
	SessionID string
	Principal string
	// RequestID 用于幂等与全链路追踪。
	//
	// 注意：它会被写进 label，因此必须是**合法的 label 值**
	// （≤63 字符，仅含字母数字与 `-_.`）。这是显式契约而不是实现细节 ——
	// 如果传超长 UUID，本包会直接拒绝而不是静默截断（静默截断会让幂等查找失效，
	// 表现为"重试时又吃掉了第二个库存"）。
	RequestID string
	// PriorityClassName 决定该沙箱的抢占关系。
	PriorityClassName string
	// HardDeadline 是业务侧的硬期限，优先级高于平台推导的 TTL。
	HardDeadline *metav1.Time
}

// Result 是一次认领的结果。
type Result struct {
	Sandbox *sandboxv1alpha1.AgentSandbox
	// Attempts 是实际尝试过的候选数。
	Attempts int
	// Conflicts 是其中因乐观锁冲突被拒的次数。
	//
	// 它直接进指标：冲突率高说明池被抢得很厉害（需要调整池划分或扩容），
	// 而不是"重试一下就好"。没有这个数字，这类问题只会在 P99 延迟上体现，
	// 而 P99 无法告诉你是哪个环节慢。
	Conflicts int
	// Idempotent 为 true 表示命中幂等路径：同一个 RequestID 之前已经认领成功。
	Idempotent bool
	// Path 是 warm / cold。
	Path string
}

// Claimer 执行 CAS 认领。
type Claimer struct {
	Client client.Client
	// Namespace 是库存所在命名空间。空则用 sandbox-pool。
	Namespace string
	// MaxAttempts 覆盖 DefaultMaxAttempts。
	MaxAttempts int
}

// Claim 从池中认领一个沙箱。
//
// 返回值：
//   - err == nil                            → 认领成功
//   - errors.Is(err, ErrPoolExhausted)      → 候选已全部确认不可用（可走冷路径）
//   - errors.Is(err, ErrContended)          → 竞争太激烈，应退避重试（**不要**走冷路径）
//   - 其它                                  → 流程异常
//
// 注意：即使返回错误，res 也可能非 nil —— 调用方可以从中读取 Conflicts
// 以观察竞争强度。把冲突计数只放在成功路径上会让最需要它的场景（高竞争）
// 正好拿不到这个数字。
func (c *Claimer) Claim(ctx context.Context, req Request) (*Result, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}

	// ---- 第一步：幂等 ----
	//
	// 客户端超时重试是常态而非例外。没有这一步，一次重试会白吃掉一个库存，
	// 而且业务侧会拿到两个不同的沙箱 —— 第一个被泄漏，直到 TTL 兜底才回收。
	existing, err := c.findByRequestID(ctx, req)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return &Result{Sandbox: existing, Idempotent: true, Path: PathWarm}, nil
	}

	// ---- 第二步：取候选并逐个 CAS ----
	candidates, err := c.candidates(ctx, req)
	if err != nil {
		return nil, err
	}

	res := &Result{}
	limit := c.maxAttempts()
	examined := 0
	for i := range candidates {
		if res.Attempts >= limit {
			break
		}
		cand := &candidates[i]
		examined++
		if !claimable(cand) {
			// 缓存可能落后于 API Server，这里再筛一次是权威判断。
			continue
		}
		res.Attempts++

		if err := c.bind(ctx, cand, req); err != nil {
			if apierrors.IsConflict(err) {
				// 被别人抢走了 —— 顺延下一个候选，而不是重试同一个。
				// 重试同一个在竞争下只会反复撞锁，浪费时间预算。
				res.Conflicts++
				continue
			}
			return nil, fmt.Errorf("认领沙箱 %s/%s 失败: %w", cand.Namespace, cand.Name, err)
		}
		res.Sandbox = cand
		res.Path = PathWarm
		return res, nil
	}

	// 还没看完候选就用尽了尝试预算 —— 库存可能还有剩余。
	// 这种情况必须报 ErrContended 而不是 ErrPoolExhausted，
	// 否则调用方会在库存在的情况下转去新建沙箱。
	if examined < len(candidates) {
		return res, ErrContended
	}
	return res, ErrPoolExhausted
}

// ---------------------------------------------------------------------------
// 内部步骤
// ---------------------------------------------------------------------------

// candidates 列出候选库存并排序。
func (c *Claimer) candidates(ctx context.Context, req Request) ([]sandboxv1alpha1.AgentSandbox, error) {
	var list sandboxv1alpha1.AgentSandboxList
	if err := c.Client.List(ctx, &list,
		client.InNamespace(c.namespace()),
		client.MatchingLabels{
			sandboxv1alpha1.LabelPool:    req.Pool,
			sandboxv1alpha1.LabelClaimed: "false",
			sandboxv1alpha1.LabelRole:    sandboxv1alpha1.RoleSandbox,
		},
	); err != nil {
		return nil, fmt.Errorf("列出池 %s 的库存失败: %w", req.Pool, err)
	}

	items := list.Items
	// 排序规则：先同租户复用，再按创建时间 FIFO。
	//
	// FIFO 不只是公平性：它还保证库存会被轮换掉。如果总是挑最新创建的，
	// 最老的那些会一直躺在池里直到被 maxStockAgeSeconds 强制销毁 ——
	// 既浪费资源，又让"库存年龄"这个指标失去意义。
	//
	// 同租户优先的价值在于避免一次冷启动，同时避开跨租户复用
	// （后者被硬编码禁止，见 docs/08 §9）。
	sort.SliceStable(items, func(i, j int) bool {
		si := isSameTenant(&items[i], req.Tenant)
		sj := isSameTenant(&items[j], req.Tenant)
		if si != sj {
			return si
		}
		return items[i].CreationTimestamp.Before(&items[j].CreationTimestamp)
	})
	return items, nil
}

// claimable 是最后一道防线。
//
// 即使 label selector 已经过滤过一轮，这里也必须重新判断：
// **列表来自 informer 缓存，可能落后于 API Server**。
// 缓存过滤只能减少候选数量，不能替代权威判断。
// 真正保证不重复认领的是 bind() 里的乐观锁，而不是这里的检查。
func claimable(sbx *sandboxv1alpha1.AgentSandbox) bool {
	if sbx == nil {
		return false
	}
	if sbx.DeletionTimestamp != nil {
		return false
	}
	if sbx.Spec.Claim != nil {
		return false
	}
	// 只有 Ready 的库存可用：Pending/Provisioning 的还没就绪，
	// 认领了只会让业务拿到一个连不上的沙箱。
	return sbx.Status.Phase == sandboxv1alpha1.PhaseReady
}

// bind 以乐观锁写入认领信息。
func (c *Claimer) bind(ctx context.Context, cand *sandboxv1alpha1.AgentSandbox, req Request) error {
	base := cand.DeepCopy()

	cand.Spec.Claim = &sandboxv1alpha1.ClaimSpec{
		RequestedBy: sandboxv1alpha1.ClaimRequestedBy{
			Tenant:    req.Tenant,
			SessionID: req.SessionID,
			Principal: req.Principal,
			RequestID: req.RequestID,
		},
		PriorityClassName: req.PriorityClassName,
		HardDeadline:      req.HardDeadline,
	}

	if cand.Labels == nil {
		cand.Labels = map[string]string{}
	}
	// 这三个 label 同时服务三个目的：状态查询、幂等查找、审计追溯。
	cand.Labels[sandboxv1alpha1.LabelClaimed] = "true"
	cand.Labels[sandboxv1alpha1.LabelTenant] = req.Tenant
	if req.RequestID != "" {
		cand.Labels[sandboxv1alpha1.LabelRequestID] = req.RequestID
	}

	// MergeFromWithOptimisticLock 会把 resourceVersion 带进 patch body，
	// API Server 在不匹配时返回 409。
	//
	// **少了它，两个并发申请会同时成功** —— 这是本包存在的全部意义。
	// 一个容易忽略的细节：只加 `client.MergeFrom` 是不够的，
	// 它生成的 patch 不含 resourceVersion，等价于无条件覆盖。
	patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
	return c.Client.Patch(ctx, cand, patch)
}

// findByRequestID 按 RequestID 查找已认领的沙箱，实现幂等。
func (c *Claimer) findByRequestID(ctx context.Context, req Request) (*sandboxv1alpha1.AgentSandbox, error) {
	if req.RequestID == "" {
		return nil, nil
	}
	var list sandboxv1alpha1.AgentSandboxList
	if err := c.Client.List(ctx, &list,
		client.InNamespace(c.namespace()),
		client.MatchingLabels{sandboxv1alpha1.LabelRequestID: req.RequestID},
	); err != nil {
		return nil, fmt.Errorf("按 RequestID 查找沙箱失败: %w", err)
	}
	// 必须同时比对租户：label 是集群可见的，只按 RequestID 匹配
	// 会让另一个租户猜中 ID 就能拿到别人的沙箱句柄。
	for i := range list.Items {
		s := &list.Items[i]
		if s.Labels[sandboxv1alpha1.LabelTenant] == req.Tenant {
			return s, nil
		}
	}
	return nil, nil
}

func isSameTenant(sbx *sandboxv1alpha1.AgentSandbox, tenant string) bool {
	return tenant != "" && sbx.Labels[sandboxv1alpha1.LabelTenant] == tenant
}

func (c *Claimer) namespace() string {
	if c.Namespace != "" {
		return c.Namespace
	}
	return sandboxv1alpha1.NamespacePool
}

func (c *Claimer) maxAttempts() int {
	if c.MaxAttempts > 0 {
		return c.MaxAttempts
	}
	return DefaultMaxAttempts
}

func (r Request) validate() error {
	if r.Pool == "" {
		return errors.New("claim: Pool 不能为空")
	}
	if r.Tenant == "" {
		return errors.New("claim: Tenant 不能为空（没有租户归属的沙箱无法做配额与审计）")
	}
	if r.RequestID != "" {
		if errs := validation.IsValidLabelValue(r.RequestID); len(errs) > 0 {
			return fmt.Errorf("claim: RequestID %q 不能用作 label 值（%v）；"+
				"它会被写入 label 用于幂等查找，请改用 ≤63 字符的短标识", r.RequestID, errs)
		}
	}
	return nil
}
