package controller

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
)

// SandboxIDPrefix 是业务侧句柄的前缀。
// 前缀参与对外契约（业务日志、SDK 校验），因此是常量而不是内联字符串。
const SandboxIDPrefix = "sbx-"

// sandboxIDSourceLen 是从 UID 派生 ID 时取用的前缀长度。
// 12 个字符（48 bit）在单集群的沙箱生命周期内碰撞概率可忽略，
// 同时让 ID 短到能直接出现在日志与 URL 里。
const sandboxIDSourceLen = 12

// ensureSandboxID 生成一次且此后永不变更（INV-5）。
//
// 从 UID **派生**而不是取随机数，这一点是刻意的：
//
//	随机 ID 只在"status 一定写得成功"的假设下才不变。一旦 status 写入失败
//	（网络抖动、冲突、或 etcd 从备份恢复回旧版本），下一轮就会生成第二个 ID，
//	而业务手里握着的是第一个。症状是"用 ID 查沙箱返回 404，但它明明还在跑，
//	而且日志里这个 ID 确实存在过" —— 这类问题几乎无法从现象反推原因。
//
// UID 由 API Server 保证唯一且不可变，因此派生值同样唯一且不可变，
// 且这个不变性不依赖任何一次写入是否成功。
func ensureSandboxID(sbx *sandboxv1alpha1.AgentSandbox) bool {
	if sbx.Status.SandboxID != "" || sbx.UID == "" {
		return false
	}
	uid := string(sbx.UID)
	if len(uid) > sandboxIDSourceLen {
		uid = uid[:sandboxIDSourceLen]
	}
	sbx.Status.SandboxID = SandboxIDPrefix + uid
	return true
}

// claimBinding 描述 bindClaimStatus 的结果。
type claimBinding struct {
	// IDChanged 表示本次生成了 sandboxID。
	IDChanged bool
	// NewlyBound 表示这是一次**新的**认领（而不是同一次认领的重复观察）。
	NewlyBound bool
}

// Changed 报告是否有任何需要持久化的改动。
func (b claimBinding) Changed() bool { return b.IDChanged || b.NewlyBound }

// bindClaimStatus 把 spec.claim 的**快照**写进 status.claimRef。
//
// 为什么必须有这一步 —— 它同时修掉了三个各自独立的问题：
//
//  1. 空闲判定失效（最严重）。idleFor 以 claimRef.claimedAt 为基准之一。
//     缺了它，基准只能退化成 CR 的创建时间，于是一个在池里躺了 1 小时的库存
//     会在被认领后的**第一次** reconcile 就被判定"已空闲 1 小时"并立即回收。
//     整个池化机制（本项目最核心的机制）会以"刚认领就被销毁"的形式完全失效，
//     而日志上只会留下一次看起来完全正常的 IdleTimeout 回收。
//
//  2. 契约不可追溯。spec.claim 是"意图"，随时可能被改写；status.claimRef 是
//     "平台当时承诺了什么"。hardDeadline 若只存在于 spec，事后被改掉就再也无法
//     证明当初约定的是什么。
//
//  3. 复用计数漏计/重复计。靠 claimRef.requestId 判定"这次认领是否已经记过账"，
//     否则 maxClaimCount 会形同虚设（见 ClaimStatus.RequestID 的说明）。
//
// 判定"新认领"的依据是 requestId 变化，而不是"claimRef 是否为空"：
// 后者在 release 后重认领同一实例时会漏计，前者不会。
func (r *SandboxReconciler) bindClaimStatus(sbx *sandboxv1alpha1.AgentSandbox, now time.Time) claimBinding {
	b := claimBinding{IDChanged: ensureSandboxID(sbx)}

	if sbx.Spec.Claim == nil {
		// 已释放。**保留** claimRef：它是这次会话的审计记录，而回收后
		// status 会随对象一起消失，保留它意味着 finalizer 与事件里
		// 仍能追溯到"这个沙箱属于谁"，成本核算才有依据。
		return b
	}

	tenant := sbx.Spec.Claim.RequestedBy.Tenant
	reqID := sbx.Spec.Claim.RequestedBy.RequestID
	if cur := sbx.Status.ClaimRef; cur != nil && cur.ClaimedAt != nil &&
		cur.Tenant == tenant && cur.RequestID == reqID {
		// 同一次认领的重复观察：不记账、不刷新 claimedAt。
		// 刷新 claimedAt 会让空闲计时被无限重置 —— 那等于彻底关闭空闲回收。
		return b
	}

	at := metav1.NewTime(now)
	ref := &sandboxv1alpha1.ClaimStatus{
		Tenant:    tenant,
		SessionID: sbx.Spec.Claim.RequestedBy.SessionID,
		RequestID: reqID,
		ClaimedAt: &at,
		// Lease 命名约定为与沙箱同名，这里记下来是为了让排障时
		// 不必再去猜命名规则。
		LeaseName: sbxLeaseName(sbx),
	}
	if sbx.Spec.Claim.HardDeadline != nil {
		// 复制而不是共享指针：spec 之后被改写不应影响这份契约快照。
		dl := *sbx.Spec.Claim.HardDeadline
		ref.HardDeadline = &dl
	}
	sbx.Status.ClaimRef = ref
	b.NewlyBound = true

	// 复用次数是**生命周期累计值**，只增不减 —— 它回答的问题是
	// "这个实例服务过多少次会话"，而不是"现在有没有被占用"。
	sbx.Status.Metrics.ClaimedCount++

	// 冷路径判定：认领发生在 Ready 阶段之外，说明这个沙箱是为此申请专门建的，
	// 没有命中库存。这个标记直接进命中率指标，是池水位调参的输入。
	if sbx.Status.Phase != sandboxv1alpha1.PhaseReady {
		sbx.Status.Metrics.ColdPath = true
	}
	return b
}

// markProvisioned 记录 Pod 首次就绪的时间。
//
// 单独记这个时间而不是复用 claim 时间：冷路径的"启动延迟 SLO"（P95 ≤ 2.5s）
// 衡量的是 Pod 就绪耗时，若用认领时间做基准，会把排队与重试时间也算进去，
// 指标就不再指向真正要优化的环节。
func markProvisioned(sbx *sandboxv1alpha1.AgentSandbox, now time.Time) bool {
	if sbx.Status.Metrics.ProvisionedAt != nil {
		return false
	}
	t := metav1.NewTime(now)
	sbx.Status.Metrics.ProvisionedAt = &t
	return true
}
