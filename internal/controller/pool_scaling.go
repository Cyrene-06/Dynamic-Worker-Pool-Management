package controller

import (
	"fmt"
	"math"
	"time"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
)

// ScaleAction 是水位决策的三种结果。
type ScaleAction string

const (
	ScaleUp   ScaleAction = "ScaleUp"
	ScaleDown ScaleAction = "ScaleDown"
	ScaleHold ScaleAction = "ScaleHold"
)

// 水位决策的原因码。必须是枚举：会进入 Conditions 与指标 label。
const (
	ScaleReasonDemandUp       = "DemandUp"
	ScaleReasonDemandDown     = "DemandDown"
	ScaleReasonColdStart      = "ColdStart"
	ScaleReasonAtTarget       = "AtTarget"
	ScaleReasonCooldown       = "CooldownActive"
	ScaleReasonHysteresis     = "WithinHysteresisBand"
	ScaleReasonOscillation    = "OscillationDetected"
	ScaleReasonPaused         = "ScalingPaused"
	ScaleReasonDraining       = "PoolDraining"
	ScaleReasonNodeCapacity   = "InsufficientNodeCapacity"
	ScaleReasonMetricsMissing = "MetricsUnavailableNeutralFeedback"
	ScaleReasonMaxWarmReached = "MaxWarmReached"
	ScaleReasonMinWarmReached = "MinWarmReached"
	// ScaleReasonProtectMode 表示扩容幅度被雪崩保护压低（docs/05 §5）。
	//
	// 它必须在**限速真的生效时**才被置上：只因为“处于保护中”就置码，
	// 会让这个原因码无法回答“保护模式到底起没起作用”—— 而那是排查
	// 容量危机时最需要回答的问题之一。
	ScaleReasonProtectMode = "ProtectModeThrottled" // ScaleReasonProvisioningPending 表示"本轮不需要动作，因为已有库存在创建中"。
	//
	// 单独给出这个原因码是有意的：它让运维能在生产里**直接验证**
	// "防重复扩容"确实在生效（该计数应占 delta==0 的相当比例），
	// 而不是只能从"池没有超额"这个结果反推。
	ScaleReasonProvisioningPending = "ProvisioningPending"
)

// feedbackKp 是命中率反馈项的比例增益。
//
// 为何只有 P 没有 I：积分项需要持久化状态与抗积分饱和（anti-windup），
// 否则一次长时间故障会把积分顶到上限，故障恢复后要很久才回落。
// 前馈项（EWMA 需求预测）已经承担了主要调节作用，
// 加一个没有抗饱和的积分器是净负收益。M2 若确实需要，再连同
// 持久化积分器与 anti-windup 一起做。
const feedbackKp = 2.0

// ScaleObservation 是池水位决策所需的全部外部事实。
//
// 与 Observation 同样的思路：把集群事实收敛成扁平结构，让 DecideScaling
// 成为纯函数。池化最容易翻车的两件事 —— 重复扩容与扩缩振荡 ——
// 恰恰都只依赖这里的几个数字，因此可以在没有集群的情况下被穷举验证。
type ScaleObservation struct {
	Now time.Time

	// Warm 是可用库存：phase=Ready 且未被认领且未标记 draining。
	Warm int32
	// Inflight 是正在创建的库存（Pending + Provisioning）。
	//
	// 它必须参与"缺口"计算，否则每个 reconcile 都会重复扩容一次 ——
	// 上一轮的库存还在创建中，控制器却看不到它，于是再补一批。
	// 这是池化实现里最典型的一类超额扩容。
	Inflight int32
	// Draining 是已标记排空、等待删除的库存。
	// 它们**不计入 Warm**，这正是两阶段缩容的关键（见 docs/05 §4）。
	Draining int32
	Claimed  int32
	Failed   int32

	// PredictedDemand 是上一轮写回 status 的 EWMA 载体。
	//
	// 把平滑器的状态放在 status 而不是控制器内存里，换来两个性质：
	//   1. 控制器无状态，多副本/重启后行为一致
	//   2. 重启后不会因为"忘记了需求"而立刻做一次错误决策
	PredictedDemand int32

	// ClaimRatePerSecond 是最近窗口的认领速率。未知时为 0。
	ClaimRatePerSecond float64

	// HitRatioPermille 是 1h 滑动窗口的热路径命中率。
	//
	// **未知用 -1 表示，必须与"命中率真的是 0"区分**：
	// 若指标源中断时按 0 计算，反馈项会把目标水位推向 1.8 倍上限，
	// 一次监控故障就会放大成一次大规模扩容。
	HitRatioPermille int32

	// LastScaleAt / LastScaleDelta 来自 status，用于冷却与滞回判定。
	// 放在 status 而非内存，是为了控制器重启后不会立刻再动一次 ——
	// 重启后立刻缩容会放大故障。
	LastScaleAt    time.Time
	LastScaleDelta int32

	// OscillationReversals 是最近窗口内扩缩方向反转的次数（内存态，尽力而为）。
	OscillationReversals int

	// NodeHeadroom 是当前节点容量还能容纳多少库存。
	//
	// 负数表示未知/无限制。之所以要这个字段：池扩容是秒级、节点扩容是分钟级
	// （docs/05 §6.1 的时间常数不匹配）。若不限制，控制器会持续创建
	// 调度不上的 Pending 库存，把"节点不够"伪装成"控制器行为异常"。
	NodeHeadroom int32

	// ProtectActive 表示该池当前处于雪崩保护中（docs/05 §5）。
	//
	// 取值来自池的 status（由 EvaluateProtectMode 判定后写入），
	// 而不是在这里重新算一遍 —— 同一个事实只能有一个来源，
	// 否则接入层看到的与控制器用的会是两个可能不一致的判断。
	ProtectActive bool
}

// ScaleDecision 是水位决策的结果。
type ScaleDecision struct {
	Action ScaleAction
	// Target 是本轮算出的目标库存水位。
	Target int32
	// Delta 是要创建的（正）或要标记排空的（负）库存数量。
	Delta int32
	// Reason 必须是枚举码，描述本轮决策的主导原因。
	Reason string
	// PredictedDemand 是要写回 status 的新 EWMA 值。
	PredictedDemand int32

	// Suppressed 表示"本应动作但被保护机制完全压住"（delta 被置 0）。
	//
	// 这个标志必须上报指标：否则运维只会看到"水位长期不达标"，
	// 却不知道是冷却/滞回/振荡保护在起作用，于是去调错参数。
	//
	// 注意：pauseScaling / drain 这类**运维闸门**不置此标志 ——
	// 它们是主动停止动作，不是保护机制压住了动作。
	Suppressed bool
	// SuppressReason 仅在 Suppressed 为 true 时有意义，且与 Reason 取值一致。
	SuppressReason string

	// MetricsUnavailable 表示本轮是在"命中率指标不可用"的前提下算的。
	//
	// 它是**输入条件**而不是决策结果，因此单独列出：
	// 控制器需要据此挂一个 Condition，让"监控断了"这件事在集群里可见，
	// 而不是只能从水位异常反推。
	MetricsUnavailable bool
}

// DecideScaling 是池水位决策的唯一决策点，纯函数。
func DecideScaling(pool *sandboxv1alpha1.SandboxPool, o ScaleObservation) ScaleDecision {
	if pool == nil {
		return hold(ScaleReasonAtTarget, o.PredictedDemand)
	}
	sc := pool.Spec.Scaling

	// ---- 1. 闸门：排空与熔断优先级最高 ----
	//
	// 注意这里刻意**不置 Suppressed**：闸门是运维主动停止动作，
	// 而 Suppressed 的语义是"保护机制压住了本该发生的动作"。
	// 两者混为一谈的代价很具体：计划内维护（drain）期间会持续触发
	// "池被抑制"的告警，把真实问题淹没在噪音里。
	if pool.Spec.Drain.Enabled {
		return hold(ScaleReasonDraining, o.PredictedDemand)
	}
	if sc.PauseScaling {
		return hold(ScaleReasonPaused, o.PredictedDemand)
	}

	// ---- 2. 前馈：EWMA 需求预测 ----
	// 预测的是"补货时间之内将到来的申请量"，因此要乘 ReplenishSeconds。
	replenish := float64(effectiveReplenishSeconds(sc))
	instant := o.ClaimRatePerSecond * replenish
	alpha := clampF(float64(sc.Dampening.EWMAAlphaPermille)/1000, 0.05, 1.0)
	demand := alpha*instant + (1-alpha)*float64(o.PredictedDemand)

	// ---- 3. 反馈：命中率缺口 ----
	feedback := 1.0
	metricsMissing := o.HitRatioPermille < 0
	if !metricsMissing {
		gap := float64(sc.TargetHitRatioPermille-o.HitRatioPermille) / 1000
		feedback = clampF(1+feedbackKp*gap, 0.8, 1.8)
	}

	// ---- 4. 目标水位 ----
	//
	// 这里刻意用配置的固定缓冲，而没有直接套 docs/05 §3.2 的平方根安全库存公式：
	// 那个公式需要 z（服务水平系数）与 CV（需求变异系数），而这两个量必须有
	// 真实的到达分布才能标定。在拿到实测数据之前硬编码一组"看起来专业"的常数，
	// 只会得到一个无法解释、也无法调优的控制器。
	// 公式与标定方法保留在文档里，M3 接入指标管线后再落地。
	target := math.Max(float64(sc.MinWarm), demand*feedback) + float64(EffectiveTargetWarmBuffer(sc))
	// maxWarm 的钳制发生在**这里**，而不是算完 delta 之后。
	//
	// 原因：delta 定义为 target - (warm+inflight)，所以 warm+inflight+delta
	// 恒等于 target。在 delta 之后再判一次 maxWarm 是一段永远不成立的死代码 ——
	// 而"看起来像兜底"的死代码最危险：它会让后来者以为这里已经防住了越界。
	clampedByMaxWarm := false
	if sc.MaxWarm > 0 && target > float64(sc.MaxWarm) {
		target = float64(sc.MaxWarm)
		clampedByMaxWarm = true
	}
	targetI := int32(math.Round(target))

	// ---- 5. 缺口：必须减去 inflight ----
	delta := targetI - (o.Warm + o.Inflight)

	d := ScaleDecision{
		Target:             targetI,
		Delta:              delta,
		PredictedDemand:    int32(math.Round(demand)),
		Reason:             ScaleReasonAtTarget,
		Action:             ScaleHold,
		MetricsUnavailable: metricsMissing,
	}

	// ---- 6. 振荡保护：三条抑制机制里最强的一条 ----
	//
	// 顺序上先做振荡再判冷却，是因为振荡说明参数本身有问题，
	// 继续按冷却期小步动作只会让振荡持续更久。
	if o.OscillationReversals >= maxReversalsInWindow {
		return suppressed(d, ScaleReasonOscillation)
	}

	// ---- 7. 零缺口：区分"已达目标"与"正在补货" ----
	//
	// 后者是防重复扩容机制正在工作的证据，必须与"已达目标"区分开，
	// 否则无法在指标上验证这套机制。
	if delta == 0 {
		d.Action = ScaleHold
		if o.Inflight > 0 && targetI > o.Warm {
			d.Reason = ScaleReasonProvisioningPending
		}
		return d
	}

	// ---- 8. 滞回 ----
	if withinHysteresis(delta, targetI, sc.Dampening.HysteresisRatioPermille) {
		return suppressed(d, ScaleReasonHysteresis)
	}

	// ---- 8. 冷却期（非对称：扩容快、缩容慢）----
	direction := ScaleUp
	if delta < 0 {
		direction = ScaleDown
	}
	if cooldownActive(sc, o, direction) {
		return suppressed(d, ScaleReasonCooldown)
	}

	// ---- 9. 节点容量约束 ----
	//
	// 只限制扩容方向：缩容不受节点容量影响。
	// 节点不够时如实上报，让运维去查 Karpenter/CA，而不是让控制器反复创建
	// 调度不上的 Pending 库存。
	if delta > 0 && o.NodeHeadroom >= 0 && delta > o.NodeHeadroom {
		if o.NodeHeadroom == 0 {
			d.Delta = 0
			d.Action = ScaleHold
			return suppressed(d, ScaleReasonNodeCapacity)
		}
		delta = o.NodeHeadroom
		// 部分截断不是"被抑制"，而是"能扩多少扩多少"：
		// 保留 delta，但把原因码改成容量不足，让告警指向节点而不是控制器。
		d.Reason = ScaleReasonNodeCapacity
	}

	// ---- 10. 限速（保护模式下扩容限速额外压低）----
	//
	// 只压**扩容**方向：保护模式要打断的正反馈是"扩容/重试压垮 API Server"，
	// 而缩容不产生这类压力 —— 反过来，保护期恰恰是应当允许回收资源的时候。
	preClamp := delta
	limitUp := int32(effectiveMaxProvision(sc))
	if o.ProtectActive {
		limitUp = throttleScaleUp(limitUp, EffectiveProtectThrottlePermille(pool.Spec.ProtectMode))
	}
	delta = clamp32(delta, -int32(effectiveMaxReclaim(sc)), limitUp)
	if clampedByMaxWarm && delta > 0 && delta == preClamp {
		// 只有在限速**没有**生效时才把原因归于 maxWarm。
		// 否则会把"被限速"误报成"已达上限"，把运维引向错误的参数。
		d.Reason = ScaleReasonMaxWarmReached
	} else if o.ProtectActive && delta > 0 && delta < preClamp {
		// 仅当限速确实把扩容幅度压小了才改原因码（见 ScaleReasonProtectMode 注释）。
		d.Reason = ScaleReasonProtectMode
	}

	// ---- 11. 保底水位 ----
	if delta < 0 && o.Warm+delta < sc.MinWarm {
		// 缩容不得把可用库存压到 minWarm 之下：低峰也要保留热路径能力。
		//
		// 注意这里算的是"最大可缩量"而不是直接写 delta = warm - minWarm：
		// 后者在水位已经低于 minWarm 时是个负数，会**反向加大缩容** ——
		// 一个看起来很直观、实际方向相反的表达式。
		maxShrink := o.Warm - sc.MinWarm
		if maxShrink <= 0 {
			// 水位已经低于保底线，不应该再缩。
			delta = 0
		} else if -delta > maxShrink {
			delta = -maxShrink
		}
		d.Reason = ScaleReasonMinWarmReached
	}

	d.Delta = delta
	switch {
	case delta > 0:
		d.Action = ScaleUp
		if d.Reason == ScaleReasonAtTarget {
			d.Reason = ScaleReasonDemandUp
		}
	case delta < 0:
		d.Action = ScaleDown
		if d.Reason == ScaleReasonAtTarget {
			d.Reason = ScaleReasonDemandDown
		}
	default:
		d.Action = ScaleHold
		if d.Reason == ScaleReasonAtTarget {
			d.Reason = ScaleReasonAtTarget
		}
	}
	return d
}

// maxReversalsInWindow 是窗口内允许的扩缩方向反转次数上限。
// 超过它就冻结决策并告警 —— 与其让振荡持续，不如显式暴露参数问题。
const maxReversalsInWindow = 3

// ---------------------------------------------------------------------------
// 抑制判定
// ---------------------------------------------------------------------------

func suppressed(d ScaleDecision, reason string) ScaleDecision {
	d.Delta = 0
	d.Action = ScaleHold
	d.Suppressed = true
	d.SuppressReason = reason
	// Reason 取抑制原因而不是保留 AtTarget：运维最需要知道的是
	// "为什么没动作"，而不是"本来目标是多少"。
	d.Reason = reason
	return d
}

// withinHysteresis 判断缺口是否落在滞回带内（即"不值得为这么小的偏差动作"）。
func withinHysteresis(delta, target, hysteresisPermille int32) bool {
	if delta == 0 {
		return true
	}
	if hysteresisPermille <= 0 || target <= 0 {
		return false
	}
	threshold := int32(math.Ceil(float64(target) * float64(hysteresisPermille) / 1000))
	if threshold < 1 {
		threshold = 1
	}
	return abs32(delta) < threshold
}

// cooldownActive 判断指定方向是否仍在冷却期内。
//
// 非对称是关键：扩容冷却远小于缩容冷却（默认 15s vs 180s）。
// 扩容慢了会让热路径退化成冷路径（用户可感知）；
// 缩容慢了只是多占一会儿资源（无人感知）。两者代价不对称，
// 因此参数也不应该对称。
func cooldownActive(sc sandboxv1alpha1.SandboxPoolScaling, o ScaleObservation, dir ScaleAction) bool {
	if o.LastScaleAt.IsZero() {
		return false
	}
	// 上一次决策的方向与本次相反时，冷却必须重新开始计时，
	// 否则"刚扩容完立刻缩容"会被上一条扩容记录放行。
	if dir == ScaleUp && o.LastScaleDelta <= 0 {
		return false
	}
	if dir == ScaleDown && o.LastScaleDelta >= 0 {
		return false
	}

	cd := sc.Dampening.ScaleUpCooldownSeconds
	if dir == ScaleDown {
		cd = sc.Dampening.ScaleDownCooldownSeconds
	}
	if cd <= 0 {
		return false
	}
	return o.Now.Sub(o.LastScaleAt) < time.Duration(cd)*time.Second
}

// ---------------------------------------------------------------------------
// 默认值与工具
// ---------------------------------------------------------------------------

const (
	defaultReplenishSeconds = 90
	defaultMaxProvision     = 50
	defaultMaxReclaim       = 30
	// DefaultTargetWarmBuffer 是 CRD 默认值在代码侧的镜像，
	// 由 TestEffectivePoolDefaults 守着两者不漂移。
	DefaultTargetWarmBuffer = 10
)

func effectiveReplenishSeconds(sc sandboxv1alpha1.SandboxPoolScaling) int32 {
	if sc.ReplenishSeconds > 0 {
		return sc.ReplenishSeconds
	}
	return defaultReplenishSeconds
}

func effectiveMaxProvision(sc sandboxv1alpha1.SandboxPoolScaling) int32 {
	if sc.MaxProvisionPerSecond > 0 {
		return sc.MaxProvisionPerSecond
	}
	return defaultMaxProvision
}

func effectiveMaxReclaim(sc sandboxv1alpha1.SandboxPoolScaling) int32 {
	if sc.MaxReclaimPerSecond > 0 {
		return sc.MaxReclaimPerSecond
	}
	return defaultMaxReclaim
}

// throttleScaleUp 把扩容限速比例乘上限值。
//
// 结果至少为 1：限速的目的是**变慢**，不是**停住**。
// 取整后落到 0 会把保护模式变成另一个功能（停止扩容），
// 而那个功能已经有专门的开关（degradation.onRuntimeUnavailable=PauseScaling，
// 以及 spec.scaling.pauseScaling），两者不应该被一个四舍五入合并掉。
func throttleScaleUp(limit, permille int32) int32 {
	if permille <= 0 || permille >= 1000 {
		return limit
	}
	throttled := int32(math.Ceil(float64(limit) * float64(permille) / 1000))
	if throttled < 1 {
		return 1
	}
	if throttled > limit {
		return limit
	}
	return throttled
}

// EffectiveTargetWarmBuffer 返回生效的缓冲值。
func EffectiveTargetWarmBuffer(sc sandboxv1alpha1.SandboxPoolScaling) int32 {
	if sc.TargetWarmBuffer > 0 {
		return sc.TargetWarmBuffer
	}
	return DefaultTargetWarmBuffer
}

func hold(reason string, demand int32) ScaleDecision {
	return ScaleDecision{Action: ScaleHold, Reason: reason, PredictedDemand: demand}
}

func clampF(v, lo, hi float64) float64 { return math.Min(math.Max(v, lo), hi) }

func clamp32(v, lo, hi int32) int32 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

// DescribeScaleDecision 生成人类可读的决策摘要，用于事件与日志。
func DescribeScaleDecision(d ScaleDecision) string {
	s := fmt.Sprintf("action=%s target=%d delta=%d reason=%s", d.Action, d.Target, d.Delta, d.Reason)
	if d.Suppressed {
		s += fmt.Sprintf(" suppressed=%s", d.SuppressReason)
	}
	return s
}
