package controller

import (
	"time"

	corev1 "k8s.io/api/core/v1"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
)

// Observation 是状态机决策所需的全部外部事实。
//
// 刻意不直接传 Pod / Lease 对象，而是收敛成这个扁平结构。这样 nextPhase 成为
// **纯函数**：可以穷举"全部 Phase × 观测组合"，不需要集群、不需要 envtest。
// 控制面最容易出错的恰恰是迁移逻辑（卡在某个 Phase、该回收却没回收、
// 心跳丢了误杀长任务），把它做成纯函数是让这部分可证伪的最低成本方式。
type Observation struct {
	Now time.Time

	// ---- Pod 相关 ----
	PodExists    bool
	PodPhase     corev1.PodPhase
	PodReady     bool
	PodCreatedAt time.Time

	// ---- 节点相关 ----
	// NodeName 是 Pod 被调度到的节点，用于读取节点健康状态。
	NodeName string

	// NodeReady 为 false 表示承载该沙箱的节点已失联：沙箱实际已经不存在了，
	// 越早承认越好，不要等 TTL。
	NodeReady bool

	// ---- 心跳（主判据）----
	// LeaseHealthy 表示心跳仍在有效期内（Lease 未过期 + 已过 grace）。
	LeaseHealthy bool

	// ---- 活动（辅助判据）----
	// 这三个信号用于处理"心跳丢失但沙箱确实在被使用"的矛盾情况。
	// 此时必须保守地选择"不回收"（docs/03 §4.3）。
	NetworkActivity   bool
	CPUIncrementMilli int64
	ActiveConnections int32

	// ---- 休眠相关 ----
	Frozen  bool // 冻结/快照已完成
	Resumed bool // 唤醒已完成
}

// ConditionReason 是决策原因码。
//
// 必须是枚举：它会进入 Conditions 与指标 label，自由文本会让基数失控。
type ConditionReason string

const (
	ReasonPodCreating           ConditionReason = "PodCreating"
	ReasonWaitingPodReady       ConditionReason = "WaitingPodReady"
	ReasonPodMissing            ConditionReason = "PodMissing"
	ReasonPodTerminated         ConditionReason = "PodTerminated"
	ReasonProvisionTimeout      ConditionReason = "ProvisionTimeout"
	ReasonStockReady            ConditionReason = "StockReady"
	ReasonColdPathClaimed       ConditionReason = "ColdPathClaimed"
	ReasonClaimed               ConditionReason = "Claimed"
	ReasonStockRotation         ConditionReason = "StockRotation"
	ReasonReuseLimitReached     ConditionReason = "ReuseLimitReached"
	ReasonHardDeadline          ConditionReason = "HardDeadline"
	ReasonNodeLost              ConditionReason = "NodeLost"
	ReasonTTLExpired            ConditionReason = "TTLExpired"
	ReasonIdleDetectionConflict ConditionReason = "IdleDetectionConflict"
	ReasonHeartbeatLost         ConditionReason = "HeartbeatLost"
	ReasonIdleHibernate         ConditionReason = "IdleHibernate"
	ReasonIdleTimeout           ConditionReason = "IdleTimeout"
	ReasonHibernateFailed       ConditionReason = "HibernateFailed"
	ReasonHibernated            ConditionReason = "Hibernated"
	ReasonResuming              ConditionReason = "Resuming"
	ReasonResumed               ConditionReason = "Resumed"
	ReasonTerminal              ConditionReason = "Terminal"
	ReasonDeleting              ConditionReason = "Deleting"
	ReasonUnexpectedPhase       ConditionReason = "UnexpectedPhase"
)

// Decision 是状态机的一次处置。
type Decision struct {
	// Next 为空表示不迁移阶段，只按 RequeueAfter 重新评估。
	Next sandboxv1alpha1.Phase

	// RecycleReason 仅在进入回收时填写；会作为指标 label，因此是枚举。
	RecycleReason sandboxv1alpha1.RecycleReason

	Reason ConditionReason

	// Message 是人类可读的补充说明，用于事件与 Condition。
	Message string

	// RequeueAfter 是下次评估间隔。
	//
	// 一律用 RequeueAfter，不用 Requeue: true —— 后者会把 workqueue 打爆，
	// 在 5k 沙箱规模下表现为控制面 CPU 100% 且 reconcile 延迟飙升。
	RequeueAfter time.Duration

	// Reclaim 为 true 表示这次决策要把沙箱推进到 Terminating。
	// 单独给出这个布尔量，是为了让调用方不需要去猜某个 Reason 是否属于回收类。
	Reclaim bool
}

// Transition 是迁移到某个阶段的便捷构造。
func Transition(next sandboxv1alpha1.Phase, reason ConditionReason) Decision {
	return Decision{Next: next, Reason: reason}
}

// Stay 表示原地不动，等待下一次评估。
func Stay(reason ConditionReason, after time.Duration) Decision {
	return Decision{Reason: reason, RequeueAfter: after}
}

// Reclaim 构造一次回收决策。
func ReclaimDecision(phase sandboxv1alpha1.Phase, reason ConditionReason, why sandboxv1alpha1.RecycleReason) Decision {
	return Decision{
		Next:          phase,
		Reason:        reason,
		RecycleReason: why,
		Reclaim:       true,
	}
}

// NextPhase 是状态机的唯一决策点。
//
// 它是纯函数：不读集群、不写集群、不看时钟（时间来自 Observation）。
// 因此可以被完整穷举测试，也因此在被调用上千次时结果稳定。
func NextPhase(sbx *sandboxv1alpha1.AgentSandbox, o Observation) Decision {
	if sbx == nil {
		return Decision{Reason: ReasonUnexpectedPhase}
	}
	if !sbx.DeletionTimestamp.IsZero() {
		// 删除路径不走状态机，由 Finalizer 链处理。
		return Decision{Reason: ReasonDeleting}
	}
	if sbx.Status.Phase.IsTerminal() {
		return Decision{Reason: ReasonTerminal}
	}

	lc := EffectiveLifecycle(sbx)

	switch sbx.Status.Phase {
	case "", sandboxv1alpha1.PhasePending:
		return Transition(sandboxv1alpha1.PhaseProvisioning, ReasonPodCreating)

	case sandboxv1alpha1.PhaseProvisioning:
		return decideProvisioning(sbx, lc, o)

	case sandboxv1alpha1.PhaseReady:
		return decideStock(sbx, lc, o)

	case sandboxv1alpha1.PhaseRunning, sandboxv1alpha1.PhaseIdle:
		return decideClaimed(sbx, lc, o)

	case sandboxv1alpha1.PhaseHibernating:
		if o.Frozen {
			return Decision{Next: sandboxv1alpha1.PhaseHibernated, Reason: ReasonHibernated}
		}
		// 冻结/快照长时间无果：退回 Idle 而不是硬撑，避免卡死在中间态。
		if !o.PodExists || !o.NodeReady {
			return ReclaimDecision(sandboxv1alpha1.PhaseTerminating, ReasonNodeLost,
				sandboxv1alpha1.RecycleNodeLost)
		}
		return Stay(ReasonIdleHibernate, 15*time.Second)

	case sandboxv1alpha1.PhaseHibernated:
		// 休眠不是永久状态：硬期限、节点失联、TTL 仍然优先。
		if d := reclaimTrigger(lc, sbx, o); d.Reclaim {
			d.Next = sandboxv1alpha1.PhaseTerminating
			return d
		}
		if activityObserved(lc, o) {
			return Decision{Next: sandboxv1alpha1.PhaseResuming, Reason: ReasonResuming}
		}
		// 冻结只释放了 CPU，内存仍在占用。若已达回收阈值就必须真正销毁，
		// 否则“内存迟迟不归还”会变成一个极难定位的问题（状态看起来一切正常）。
		if lc.IdleTimeoutSeconds > 0 &&
			idleFor(sbx, o) >= time.Duration(lc.IdleTimeoutSeconds)*time.Second {
			return ReclaimDecision(sandboxv1alpha1.PhaseTerminating, ReasonIdleTimeout,
				sandboxv1alpha1.RecycleIdleTimeout)
		}
		return Stay(ReasonHibernated, 60*time.Second)

	case sandboxv1alpha1.PhaseResuming:
		if o.Resumed {
			return Decision{Next: sandboxv1alpha1.PhaseRunning, Reason: ReasonResumed}
		}
		if !o.PodExists || !o.NodeReady {
			return ReclaimDecision(sandboxv1alpha1.PhaseTerminating, ReasonNodeLost,
				sandboxv1alpha1.RecycleNodeLost)
		}
		return Stay(ReasonResuming, 5*time.Second)

	default:
		return Stay(ReasonUnexpectedPhase, 30*time.Second)
	}
}

// ---------------------------------------------------------------------------
// 各阶段决策
// ---------------------------------------------------------------------------

func decideProvisioning(sbx *sandboxv1alpha1.AgentSandbox, lc Lifecycle, o Observation) Decision {
	if !o.NodeReady {
		return ReclaimDecision(sandboxv1alpha1.PhaseFailed, ReasonNodeLost,
			sandboxv1alpha1.RecycleNodeLost)
	}
	if !o.PodExists {
		// Pod 可能正在被创建，也可能被人手删了。两种都以"等待 + 超时兜底"处理，
		// 不做猜测 —— 猜测会导致同一状态在不同时间产生不同结果。
		return Stay(ReasonPodMissing, 10*time.Second)
	}

	switch o.PodPhase {
	case corev1.PodRunning:
		if !o.PodReady {
			// 就绪探针还没过。这是冷路径延迟的主要构成之一。
			return Stay(ReasonWaitingPodReady, 2*time.Second)
		}
		if sbx.Spec.Claim == nil {
			// 池中库存就绪 —— 池水位（count(phase=Ready)）由此产生。
			return Transition(sandboxv1alpha1.PhaseReady, ReasonStockReady)
		}
		// 冷路径：业务直接持有，无需经过 Ready。
		return Transition(sandboxv1alpha1.PhaseRunning, ReasonColdPathClaimed)

	case corev1.PodFailed, corev1.PodSucceeded:
		return ReclaimDecision(sandboxv1alpha1.PhaseFailed, ReasonPodTerminated,
			sandboxv1alpha1.RecycleRuntimeError)

	default:
		// PodPending（调度中）等中间态。
		if provisionTimedOut(sbx, lc, o) {
			return ReclaimDecision(sandboxv1alpha1.PhaseFailed, ReasonProvisionTimeout,
				sandboxv1alpha1.RecycleRuntimeError)
		}
		return Stay(ReasonWaitingPodReady, 2*time.Second)
	}
}

func decideStock(sbx *sandboxv1alpha1.AgentSandbox, lc Lifecycle, o Observation) Decision {
	// 库存被认领：原地转正。sandboxID 不变，业务持有的句柄不会失效。
	if sbx.Spec.Claim != nil {
		return Transition(sandboxv1alpha1.PhaseRunning, ReasonClaimed)
	}

	// 库存的承载能力丢了（Pod 被删、节点失联）→ 直接判失败，让池补货。
	if !o.NodeReady || !o.PodExists {
		return ReclaimDecision(sandboxv1alpha1.PhaseFailed, ReasonNodeLost,
			sandboxv1alpha1.RecycleNodeLost)
	}

	// 轮换：库存长期无人认领就销毁重建，避免镜像过期与长驻实例的内存缓慢增长。
	if stockTooOld(lc, sbx, o) {
		return ReclaimDecision(sandboxv1alpha1.PhaseTerminating, ReasonStockRotation,
			sandboxv1alpha1.RecycleStockRotation)
	}

	// 库存在池里通常等几分钟才被认领，不必秒级轮询。
	return Stay(ReasonStockReady, 30*time.Second)
}

// decideClaimed 是"已认领"状态（Running / Idle）的决策。
//
// 回收判据的顺序**本身就是设计**：按"业务破坏性从小到大、证据强度从高到低"排列。
// 先问最确凿、最不可争辩的问题（硬期限、节点失联），再问需要推断的问题（空闲）。
func decideClaimed(sbx *sandboxv1alpha1.AgentSandbox, lc Lifecycle, o Observation) Decision {
	// 1. 硬期限：业务自己给的 SLA 兜底，优先级高于平台推导的一切。
	if hardDeadlinePassed(sbx, o.Now) {
		return ReclaimDecision(sandboxv1alpha1.PhaseTerminating, ReasonHardDeadline,
			sandboxv1alpha1.RecycleHardDeadline)
	}

	// 2. 节点失联：沙箱实际已经不存在，越早承认越好，不要等 TTL 兜底。
	if !o.NodeReady || !o.PodExists {
		return ReclaimDecision(sandboxv1alpha1.PhaseTerminating, ReasonNodeLost,
			sandboxv1alpha1.RecycleNodeLost)
	}

	// 3. TTL：平台侧的兜底上限，任何情况下都不得超过，这是防泄漏的最终保险。
	if ttlExpired(lc, sbx, o.Now) {
		return ReclaimDecision(sandboxv1alpha1.PhaseTerminating, ReasonTTLExpired,
			sandboxv1alpha1.RecycleTTLExpired)
	}

	// 4. 心跳丢失但观测到活动 —— 信号矛盾。
	//
	// 这是整个状态机最需要克制的地方：宁可多留一会儿，也不能误杀一个正在跑
	// 长任务的会话。误杀的业务代价远高于多占用一会儿资源的成本。
	// 同时上报冲突指标，用于反向调参（说明 idleTimeout 设得太激进）。
	if !o.LeaseHealthy && activityObserved(lc, o) {
		return Stay(ReasonIdleDetectionConflict, 30*time.Second)
	}

	// 5. 心跳丢失且确认无活动。
	if !o.LeaseHealthy {
		switch lc.OnHeartbeatLoss {
		case sandboxv1alpha1.OnHeartbeatLossReclaim:
			return ReclaimDecision(sandboxv1alpha1.PhaseTerminating, ReasonHeartbeatLost,
				sandboxv1alpha1.RecycleHeartbeatLost)
		case sandboxv1alpha1.OnHeartbeatLossIgnore:
			// 忽略心跳，只用 TTL 与空闲检测兜底（适用于心跳不可靠的客户端）。
		default: // OnHeartbeatLossGrace
			if sbx.Status.Phase == sandboxv1alpha1.PhaseRunning {
				return Transition(sandboxv1alpha1.PhaseIdle, ReasonHeartbeatLost)
			}
			// 已在 Idle 且仍未恢复 → 视为真的没了。
			return ReclaimDecision(sandboxv1alpha1.PhaseTerminating, ReasonHeartbeatLost,
				sandboxv1alpha1.RecycleHeartbeatLost)
		}
	}

	// 6. 空闲判定（辅助判据）。顺序很重要：**先冻结，再回收**。
	if lc.IdleTimeoutSeconds > 0 {
		idle := idleFor(sbx, o)

		// 6a. 冻结：一次 cgroup 写操作就能释放全部 CPU，成本极低且完全可逆。
		//     这是本项目性价比最高的一项优化，收益通常在 70% 量级（docs/07 §5.3）。
		if hibernateDue(lc, sbx, idle) {
			return Decision{Next: sandboxv1alpha1.PhaseHibernating, Reason: ReasonIdleHibernate}
		}
		// 6b. 回收。
		if idle >= time.Duration(lc.IdleTimeoutSeconds)*time.Second {
			return ReclaimDecision(sandboxv1alpha1.PhaseTerminating, ReasonIdleTimeout,
				sandboxv1alpha1.RecycleIdleTimeout)
		}
	}

	// 7. 一切正常。
	phase := sbx.Status.Phase
	if phase == sandboxv1alpha1.PhaseIdle && activityObserved(lc, o) {
		return Transition(sandboxv1alpha1.PhaseRunning, ReasonClaimed)
	}
	return Stay(ReasonClaimed, nextIdleCheck(lc, sbx, o))
}

// hibernateDue 判断是否该进入冻结。
//
// 冻结阈值必须**小于**回收阈值（默认 120s vs 300s），否则冻结会吃掉回收时机：
// 沙箱被冻住却永远不被销毁，内存再也不归还。这种配置笔误的表现只是
// "内存占用迟迟不下降"，极难定位 —— 所以这里显式防护，并选择"不冻结"
// 而不是报错：一个参数写错不应让沙箱完全失去回收能力。
func hibernateDue(lc Lifecycle, sbx *sandboxv1alpha1.AgentSandbox, idle time.Duration) bool {
	if lc.HibernateAfterIdleSeconds <= 0 {
		return false
	}
	if sbx.Spec.Hibernation == nil || !sbx.Spec.Hibernation.Enabled {
		return false
	}
	if sbx.Status.Hibernation.State == sandboxv1alpha1.RuntimeFrozen {
		return false
	}
	if lc.IdleTimeoutSeconds > 0 && lc.HibernateAfterIdleSeconds >= lc.IdleTimeoutSeconds {
		return false
	}
	return idle >= time.Duration(lc.HibernateAfterIdleSeconds)*time.Second
}

// reclaimTrigger 把"是否该回收"的判断集中到一处，
// 供休眠态等非 Running 路径复用，避免两处规则漂移。
func reclaimTrigger(lc Lifecycle, sbx *sandboxv1alpha1.AgentSandbox, o Observation) Decision {
	switch {
	case hardDeadlinePassed(sbx, o.Now):
		return ReclaimDecision(sandboxv1alpha1.PhaseTerminating, ReasonHardDeadline,
			sandboxv1alpha1.RecycleHardDeadline)
	case !o.NodeReady || !o.PodExists:
		return ReclaimDecision(sandboxv1alpha1.PhaseTerminating, ReasonNodeLost,
			sandboxv1alpha1.RecycleNodeLost)
	case ttlExpired(lc, sbx, o.Now):
		return ReclaimDecision(sandboxv1alpha1.PhaseTerminating, ReasonTTLExpired,
			sandboxv1alpha1.RecycleTTLExpired)
	default:
		return Decision{RequeueAfter: 60 * time.Second}
	}
}

// activityObserved 判断是否观测到活动。
//
// 三个信号取"或"：任何一个为真都算在活动。这是刻意保守的选择 ——
// 漏判活动会误杀会话，误判活动只是多占用一会儿资源，两者代价不对称。
func activityObserved(lc Lifecycle, o Observation) bool {
	if !lc.TreatNetworkActivityAsActive && o.CPUIncrementMilli == 0 && o.ActiveConnections == 0 {
		return false
	}
	if lc.TreatNetworkActivityAsActive && o.NetworkActivity {
		return true
	}
	if o.ActiveConnections > 0 {
		return true
	}
	if lc.TreatCPUAboveMilli > 0 && o.CPUIncrementMilli > lc.TreatCPUAboveMilli {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// 时间判定
// ---------------------------------------------------------------------------

func idleFor(sbx *sandboxv1alpha1.AgentSandbox, o Observation) time.Duration {
	last := sbx.CreationTimestamp.Time
	if sbx.Status.Activity.LastActiveAt != nil {
		last = sbx.Status.Activity.LastActiveAt.Time
	} else if sbx.Status.ClaimRef != nil && sbx.Status.ClaimRef.ClaimedAt != nil {
		// 认领即视为一次活动，避免"刚认领就被判空闲"。
		last = sbx.Status.ClaimRef.ClaimedAt.Time
	}
	if o.Now.Before(last) {
		return 0
	}
	return o.Now.Sub(last)
}

func ttlExpired(lc Lifecycle, sbx *sandboxv1alpha1.AgentSandbox, now time.Time) bool {
	if lc.TTLSecondsAfterCreation <= 0 {
		return false
	}
	return now.Sub(sbx.CreationTimestamp.Time) >= time.Duration(lc.TTLSecondsAfterCreation)*time.Second
}

func hardDeadlinePassed(sbx *sandboxv1alpha1.AgentSandbox, now time.Time) bool {
	var dl *time.Time
	if sbx.Status.ClaimRef != nil && sbx.Status.ClaimRef.HardDeadline != nil {
		// 优先用认领时的副本：spec 可能被改过，但契约不能被追溯修改。
		t := sbx.Status.ClaimRef.HardDeadline.Time
		dl = &t
	} else if sbx.Spec.Claim != nil && sbx.Spec.Claim.HardDeadline != nil {
		t := sbx.Spec.Claim.HardDeadline.Time
		dl = &t
	}
	return dl != nil && !now.Before(*dl)
}

func stockTooOld(lc Lifecycle, sbx *sandboxv1alpha1.AgentSandbox, o Observation) bool {
	if lc.MaxStockAgeSeconds <= 0 {
		return false
	}
	return o.Now.Sub(sbx.CreationTimestamp.Time) >= time.Duration(lc.MaxStockAgeSeconds)*time.Second
}

func provisionTimedOut(sbx *sandboxv1alpha1.AgentSandbox, lc Lifecycle, o Observation) bool {
	if lc.ProvisionTimeoutSeconds <= 0 {
		return false
	}
	ref := o.PodCreatedAt
	if ref.IsZero() {
		// Pod 还没出现，用 CR 创建时间兜底。
		ref = sbx.CreationTimestamp.Time
	}
	return o.Now.Sub(ref) >= time.Duration(lc.ProvisionTimeoutSeconds)*time.Second
}

// nextIdleCheck 返回下一次空闲评估的间隔。
//
// 主动把评估频率限制在"剩余时间的一个比例"上，而不是固定每秒轮询：
// 5k 沙箱 × 每秒一次 = 5k QPS 的无效 reconcile，纯属自伤。
func nextIdleCheck(lc Lifecycle, sbx *sandboxv1alpha1.AgentSandbox, o Observation) time.Duration {
	if lc.IdleTimeoutSeconds <= 0 {
		return 5 * time.Minute
	}
	remaining := time.Duration(lc.IdleTimeoutSeconds)*time.Second - idleFor(sbx, o)
	switch {
	case remaining <= 0:
		return 0
	case remaining < 30*time.Second:
		return remaining
	default:
		return 30 * time.Second
	}
}

// ---------------------------------------------------------------------------
// 生效的生命周期参数
// ---------------------------------------------------------------------------

// Lifecycle 是"实际生效"的生命周期参数。
//
// 为什么不直接用 spec.Lifecycle：CRD 的 default 只在创建时生效，
// 而字段可能为空（直接构造的测试对象、老版本升级上来的对象）。把取值逻辑
// 收敛到 EffectiveLifecycle，可以保证状态机在任何输入下都有确定的默认值，
// 而不是零值（比如 IdleTimeoutSeconds=0 会被误判成"禁用空闲回收"）。
type Lifecycle struct {
	TTLSecondsAfterCreation       int32
	IdleTimeoutSeconds            int32
	HeartbeatGraceSeconds         int32
	HibernateAfterIdleSeconds     int32
	ProvisionTimeoutSeconds       int32
	TerminationGracePeriodSeconds int32
	// MaxStockAgeSeconds 由 SandboxPool.spec.stockPolicy 注入（不在沙箱 spec 上）。
	MaxStockAgeSeconds           int32
	OnHeartbeatLoss              sandboxv1alpha1.OnHeartbeatLoss
	ReclaimPolicy                sandboxv1alpha1.ReclaimPolicy
	TreatNetworkActivityAsActive bool
	TreatCPUAboveMilli           int64
}

// 代码里的默认值必须与 CRD 的 +kubebuilder:default 保持一致。
// 两处漂移会表现为"测试通过但线上行为不同"，因此这里有单测守着（见 phases_test.go）。
const (
	DefaultTTLSeconds              int32 = 7200
	DefaultIdleTimeoutSeconds      int32 = 300
	DefaultHeartbeatGraceSecs      int32 = 60
	DefaultHibernateAfterIdle      int32 = 120
	DefaultProvisionTimeout        int32 = 120
	DefaultTerminationGraceSeconds int32 = 30
	DefaultMaxStockAgeSeconds      int32 = 3600
	DefaultCPUAboveMilli           int64 = 50
)

// EffectiveLifecycle 把 spec 与默认值合成实际生效的参数。
func EffectiveLifecycle(sbx *sandboxv1alpha1.AgentSandbox) Lifecycle {
	lc := Lifecycle{
		TTLSecondsAfterCreation:       DefaultTTLSeconds,
		IdleTimeoutSeconds:            DefaultIdleTimeoutSeconds,
		HeartbeatGraceSeconds:         DefaultHeartbeatGraceSecs,
		HibernateAfterIdleSeconds:     DefaultHibernateAfterIdle,
		ProvisionTimeoutSeconds:       DefaultProvisionTimeout,
		TerminationGracePeriodSeconds: DefaultTerminationGraceSeconds,
		MaxStockAgeSeconds:            DefaultMaxStockAgeSeconds,
		OnHeartbeatLoss:               sandboxv1alpha1.OnHeartbeatLossGrace,
		ReclaimPolicy:                 sandboxv1alpha1.ReclaimReturnToPool,
		TreatNetworkActivityAsActive:  true,
		TreatCPUAboveMilli:            DefaultCPUAboveMilli,
	}

	if sbx == nil {
		return lc
	}
	if s := sbx.Spec.Lifecycle; s != nil {
		if s.TTLSecondsAfterCreation > 0 {
			lc.TTLSecondsAfterCreation = s.TTLSecondsAfterCreation
		}
		lc.IdleTimeoutSeconds = s.IdleTimeoutSeconds
		if s.HeartbeatGraceSeconds > 0 {
			lc.HeartbeatGraceSeconds = s.HeartbeatGraceSeconds
		}
		lc.HibernateAfterIdleSeconds = s.HibernateAfterIdleSeconds
		if s.ProvisionTimeoutSeconds > 0 {
			lc.ProvisionTimeoutSeconds = s.ProvisionTimeoutSeconds
		}
		if s.TerminationGracePeriodSeconds > 0 {
			lc.TerminationGracePeriodSeconds = s.TerminationGracePeriodSeconds
		}
		if s.OnHeartbeatLoss != "" {
			lc.OnHeartbeatLoss = s.OnHeartbeatLoss
		}
		if s.ReclaimPolicy != "" {
			lc.ReclaimPolicy = s.ReclaimPolicy
		}
	}
	return lc
}
