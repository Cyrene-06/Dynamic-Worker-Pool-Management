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

	// LastLeaseRenewAt 是心跳 Lease 最近一次续租时间。
	//
	// 它同时承担两个职责，这也是它必须进入 Observation 的原因：
	//  1. 它是**唯一**由客户端主动产生的活性信号 —— 节点侧活动采集（M2）
	//     尚未落地时，这是空闲判定唯一可用的依据；
	//  2. 它让"续租"真正生效：不把续租算作活动，客户端即使每 20s 续租一次，
	//     仍会在 idleTimeout 后被当作空闲回收。
	LastLeaseRenewAt time.Time

	// ---- 活动（辅助判据）----
	// 这三个信号用于处理"心跳丢失但沙箱确实在被使用"的矛盾情况。
	// 此时必须保守地选择"不回收"（docs/03 §4.3）。
	NetworkActivity   bool
	CPUIncrementMilli int64
	ActiveConnections int32

	// ---- 休眠相关 ----
	Frozen  bool // 冻结/快照已完成
	Resumed bool // 唤醒已完成

	// ---- 人工干预 ----
	//
	// 这两个值来自 gateway 写入的注解，是单向契约：gateway 写、控制器只读。
	// 解除请求也由 gateway 完成（:wake 同时清掉 hibernate 标记），
	// 而不是由控制器在动作完成后回写 —— 否则就变成两个写入者，
	// 而它们互相覆盖是这类 bug 最常见的成因。
	ManualWake      bool
	ManualHibernate bool
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
	ReasonClientReleased        ConditionReason = "ClientReleased"
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

// NextPhase 是状态机的唯一决策点，使用"仅由沙箱自身推导"的生命周期参数。
//
// 它是纯函数：不读集群、不写集群、不看时钟（时间来自 Observation）。
// 因此可以被完整穷举测试，也因此在被调用上千次时结果稳定。
//
// 需要用到 SandboxTemplate 默认值（如 maxClaimCount）的调用方应当用
// NextPhaseWithLifecycle，并把 ApplyTemplateDefaults 的结果传进去。
// 两段式而不是把模板作为参数塞进来：后者会让这个纯函数的入参多一个
// 需要 IO 才能拿到的对象，而它的价值恰恰来自不需要 IO。
func NextPhase(sbx *sandboxv1alpha1.AgentSandbox, o Observation) Decision {
	return NextPhaseWithLifecycle(sbx, EffectiveLifecycle(sbx), o)
}

// NextPhaseWithLifecycle 用**调用方给定的**生命周期参数做决策。
//
// # 为什么必须能传入而不是内部重算
//
// 原实现内部调用 EffectiveLifecycle(sbx)，而它只能看到沙箱自身。
// 于是任何来自 SandboxTemplate 的取值（maxClaimCount、模板级空闲阈值）
// 在状态机里都会被重算成零值 —— 表现出来就是"模板里配了 maxClaimCount，
// 但库存永远不会轮换"，而且配置看起来完全正确，没有任何线索。
func NextPhaseWithLifecycle(
	sbx *sandboxv1alpha1.AgentSandbox,
	lc Lifecycle,
	o Observation,
) Decision {
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
		if activityObserved(lc, o) || o.ManualWake {
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

	// 超时判定必须放在所有"继续等待"分支**之前**。
	//
	// 原实现把它写在PodPending的 default 分支里，于是两条最需要兜底的路径
	// 反而没有超时：Pod 始终没被创建（PodExists=false），以及 Pod 起了但
	// 就绪探针永不通过。这两种情况下状态机会无限返回 Stay，
	// 表现出来就是"沙箱一直卡在 Provisioning"，而且没有任何报错、
	// 没有任何告警 —— 它看起来只是在等。
	if provisionTimedOut(sbx, lc, o) {
		return ReclaimDecision(sandboxv1alpha1.PhaseFailed, ReasonProvisionTimeout,
			sandboxv1alpha1.RecycleRuntimeError)
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
		// PodPending（调度中）等中间态，等待并由上方的超时兜底。
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

	// 复用次数上限：该实例已经服务过 maxClaimCount 次会话，不再交给下一位租户。
	//
	// 这是"库存轮换"的另一种触发（与年龄无关）。放在库存分支而不是认领分支，
	// 是因为在这一刻沙箱是空闲的：回收它不会打断任何正在进行的会话。
	// 若放到认领之后就变成"交给业务后再抢回来"。
	// 注意这是**防御性**的：gateway 在挑选候选时也会跳过超限实例，
	// 两处都做是因为 gateway 的候选列表来自缓存，可能滞后。
	if reuseLimitReached(lc, sbx) {
		return ReclaimDecision(sandboxv1alpha1.PhaseTerminating, ReasonReuseLimitReached,
			sandboxv1alpha1.RecycleReuseLimitReached)
	}

	// 库存在池里通常等几分钟才被认领，不必秒级轮询。
	return Stay(ReasonStockReady, 30*time.Second)
}

// decideClaimed 是"已认领"状态（Running / Idle）的决策。
//
// 回收判据的顺序**本身就是设计**：按"业务破坏性从小到大、证据强度从高到低"排列。
// 先问最确凿、最不可争辩的问题（硬期限、节点失联），再问需要推断的问题（空闲）。
func decideClaimed(sbx *sandboxv1alpha1.AgentSandbox, lc Lifecycle, o Observation) Decision {
	// 0. 业务已释放：spec.claim 被清空。
	//
	// 这是**最常见的正常回收路径**，必须显式处理。原实现只在 decideStock
	// 里检查该字段，于是已认领的沙箱被释放后会一直停在 Running，
	// 直到 TTL 兜底 —— 而 TTL 可能是 2 小时。
	// 症状是“资源迟迟不释放”且没有任何报错，极难定位。
	//
	// 关于 ReturnToPool：本设计里它**当前与 Destroy 行为一致**（都回收）。
	// 同租户原地复用必须先执行 SandboxTemplate 的 resetHook 清理工作区、
	// 杀残留进程、轮换凭据 —— 在那套机制（M2）落地之前，宁可少省一次冷启动，
	// 也不能把一个可能残留上一位用户数据的沙箱交出去。
	// 这是安全边界，不是一个可以“先跑起来再说”的优化项。
	if sbx.Spec.Claim == nil {
		return ReclaimDecision(sandboxv1alpha1.PhaseTerminating, ReasonClientReleased,
			sandboxv1alpha1.RecycleClientReleased)
	}

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
		//
		//     人工请求也走同一分支：把"人工休眠"实现成一套独立流程，
		//     就会有两份"什么条件下可以冻结"的规则，而它们迟早会不一致。
		if hibernateDue(lc, sbx, idle) || o.ManualHibernate {
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

// idleFor 返回"距最后一次可观测活动过了多久"。
//
// 基准必须是"最后一次活动"，而**不是** CR 的创建时间。这个区别是致命的：
// 池中库存完全可能躺了 1 小时才被认领，若以创建时间为基准，
// 它会在认领后的第一次 reconcile 就被判定"已空闲 1 小时"而立即回收 ——
// 池化（本项目最核心的机制）会以"刚认领就被销毁"的形式完全失效，
// 而日志上只会显示一次看起来毫无异常的 IdleTimeout 回收。
//
// 因此取以下四者的**最大值**（即最晚的那个）：
//   - CreationTimestamp：兜底下限，保证结果永不为负
//   - ClaimRef.ClaimedAt：认领本身就是一次活动
//   - Activity.LastActiveAt：节点侧观测到的活动
//   - LastLeaseRenewAt：客户端主动续租，最强的"还活着"证据
func idleFor(sbx *sandboxv1alpha1.AgentSandbox, o Observation) time.Duration {
	last := sbx.CreationTimestamp.Time
	if c := sbx.Status.ClaimRef; c != nil && c.ClaimedAt != nil && c.ClaimedAt.After(last) {
		last = c.ClaimedAt.Time
	}
	if a := sbx.Status.Activity.LastActiveAt; a != nil && a.After(last) {
		last = a.Time
	}
	if !o.LastLeaseRenewAt.IsZero() && o.LastLeaseRenewAt.After(last) {
		last = o.LastLeaseRenewAt
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

// reuseLimitReached 判断库存是否已服务过足够多次会话。
//
// MaxClaimCount <= 0 表示不限制（默认）。之所以默认不限：把默认值设成 1 会让
// 所有池化收益归零（每次复用都要重建），而这是个性能开关而非安全开关 ——
// 安全边界是 resetHook，与它无关。
func reuseLimitReached(lc Lifecycle, sbx *sandboxv1alpha1.AgentSandbox) bool {
	if lc.MaxClaimCount <= 0 {
		return false
	}
	return sbx.Status.Metrics.ClaimedCount >= lc.MaxClaimCount
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
	// MaxClaimCount 是同一实例允许服务多少次会话（来自 SandboxTemplate）。
	// <=0 表示不限制。见 reuseLimitReached。
	MaxClaimCount int32
	// AllowSameTenantReuse 控制释放后是否允许同租户原地复用
	// （跨租户复用恒不充许，见 docs/08 §9）。
	AllowSameTenantReuse bool
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

// ApplyTemplateDefaults 把 SandboxTemplate 提供的默认值合入生效参数。
//
// 为什么不直接写进 EffectiveLifecycle：那个函数只依赖 sbx，因此是**纯函数**，
// 能在没有集群、没有模板对象的测试里被穷举调用。模板是另一个对象，
// 让 EffectiveLifecycle 去读它就会把纯函数变成需要 IO 的函数 ——
// 而状态机测试的全部价值恰恰来自"不需要集群"。
//
// 优先级：沙箱显式值 > 模板默认值 > 代码默认值（EffectiveLifecycle 已展开）。
// 三者关系必须确定，否则会出现"改了模板却不生效"这种无从下手的现象。
//
// "显式值"的判定：spec.Lifecycle 为 nil，或对应字段为零值（字符串为空）。
// 这对走经过 API Server 的对象是准确的：CRD 的 +kubebuilder:default 会把
// 未填字段写成默认值，因此"零值"确实意味着用户显式写了 0。
func ApplyTemplateDefaults(sbx *sandboxv1alpha1.AgentSandbox, tmpl *sandboxv1alpha1.SandboxTemplate) Lifecycle {
	lc := EffectiveLifecycle(sbx)
	if tmpl == nil {
		return lc
	}
	d := tmpl.Spec.Defaults
	spec := sbx.Spec.Lifecycle

	unsetTTL := spec == nil || spec.TTLSecondsAfterCreation == 0
	if unsetTTL && d.TTLSecondsAfterCreation > 0 {
		lc.TTLSecondsAfterCreation = d.TTLSecondsAfterCreation
	}
	// IdleTimeoutSeconds 与 HibernateAfterIdleSeconds 的特殊之处：0 是**语义有效值**
	// （"禁用空闲回收"，长任务场景会用到）。因此只在 spec.Lifecycle 整个缺省时才
	// 用模板值，否则会把一次有意的"禁用"悄悄改回"5 分钟"。
	if spec == nil {
		if d.IdleTimeoutSeconds > 0 {
			lc.IdleTimeoutSeconds = d.IdleTimeoutSeconds
		}
	}
	// HeartbeatGraceSeconds 没有"0 有效"的含义（CRD Minimum=10），
	// 因此零值只能意味着未设置 —— 它可以单独回退到模板值，
	// 而不需要像 idleTimeout 那样要求整个 spec.Lifecycle 缺省。
	// 把这两个字段用同一条规则处理，会让"只想覆盖 TTL"的沙箱
	// 意外地把模板的心跳宽限期一起丢掉，后果是客户端被过早判为离线。
	if spec == nil || spec.HeartbeatGraceSeconds == 0 {
		if d.HeartbeatGraceSeconds > 0 {
			lc.HeartbeatGraceSeconds = d.HeartbeatGraceSeconds
		}
	}
	if (spec == nil || spec.ReclaimPolicy == "") && d.ReclaimPolicy != "" {
		if p := sandboxv1alpha1.ReclaimPolicy(d.ReclaimPolicy); p == sandboxv1alpha1.ReclaimReturnToPool ||
			p == sandboxv1alpha1.ReclaimDestroy {
			lc.ReclaimPolicy = p
		}
	}

	// 这两项没有沙箱级字段，取值即为模板值（模板未设则保持代码默认）。
	lc.MaxClaimCount = d.MaxClaimCount
	lc.AllowSameTenantReuse = d.AllowSameTenantReuse
	return lc
}
