package controller

import (
	"math"
	"time"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
)

// 保护模式的原因码。必须是枚举：它们会进 status 与指标 label（docs/08 §2.1）。
const (
	// ProtectReasonNone 表示无保护（正常态）。
	ProtectReasonNone = ""
	// ProtectReasonProvisionFailureRate 表示因供给失败率超阈值进入保护。
	ProtectReasonProvisionFailureRate = "ProvisionFailureRate"
	// ProtectReasonApiserverLatency 表示因 API Server P99 超阈值进入保护。
	ProtectReasonApiserverLatency = "ApiserverLatency"
	// ProtectReasonHold 表示触发条件已消失、但滞回期未过。
	//
	// 单独给一个原因码是必要的：它与"仍在故障中"在 active 上长得一样，
	// 但运维要做的动作完全不同 —— 一个是继续等，一个是去查为什么还没恢复。
	ProtectReasonHold = "HoldActive"
)

// 保护模式的代码侧默认值（CRD 默认值的镜像，由测试守着两者不漂移）。
const (
	DefaultProtectFailureRatePercent = 30
	DefaultProtectMinSamples         = 10
	DefaultProtectLatencyThresholdMs = 2000
	DefaultProtectHoldSeconds        = 120
	DefaultProtectScaleUpThrottle    = 500
	DefaultProtectMaxQueueDepth      = 200
)

// ProtectObservation 是保护模式判定所需的全部外部事实。
//
// 与 Observation / ScaleObservation 同样的思路：把集群事实收敛成扁平结构，
// 让 EvaluateProtectMode 成为纯函数。雪崩保护最怕的是"阈值在正常情况下
// 被触发"或"该触发时没触发"，而这两种情况都只取决于这里的几个数字。
type ProtectObservation struct {
	Now time.Time

	// Inflight / Failed 来自池水位统计（countStock）。
	Inflight int32
	Failed   int32

	// ApiserverLatencyP99Ms 与 LatencyKnown 描述 API Server 的响应情况。
	//
	// 用两个字段而不是一个"未知时为 0"的字段，是因为**必须能区分
	// "很健康"与"不知道"**：把未知当成健康，保护模式会在监控断掉时失灵；
	// 把未知当成病态，一次监控故障就会变成一次全量拒绝。
	// 与 ScaleObservation.HitRatioPermille 用 -1 表示未知是同一个理由。
	ApiserverLatencyP99Ms int32
	LatencyKnown          bool

	// Prev* 来自上一轮写进 status 的状态，用于滞回与计数。
	PrevActive bool
	PrevSince  time.Time
	PrevUntil  time.Time
	PrevTrips  int64
}

// ProtectDecision 是保护模式的判定结果。
type ProtectDecision struct {
	Active bool
	Reason string
	Since  time.Time
	Until  time.Time
	Trips  int64

	// FailureRatePermille 是本次判定用的失败率，写进 status 供事后复盘
	// "当时到底多糟"。只留一个 active 布尔值的话，复盘时无从判断
	// 是"刚好越过 30%"还是"已经 100% 失败"。
	FailureRatePermille int32

	// TrippedNow / RecoveredNow 表示本轮发生了**状态变化**，用于发事件。
	// 只在变化时发事件：每轮都发会让一场持续 10 分钟的危机刷出几百条
	// 相同事件，把真正需要看的东西挤出事件流。
	TrippedNow   bool
	RecoveredNow bool

	// ThrottlePermille 是生效的扩容限速比例（千分比）。
	// 未进入保护时为 1000（不限速）—— 这样调用方不需要在别处再判一次。
	ThrottlePermille int32
}

// protectConfig 是归一化之后的保护参数：所有默认值已填好，
// 后续逻辑不再需要区分"零值"与"显式指定"。
type protectConfig struct {
	enabled                 bool
	failureRatePercent      int32
	minSamples              int32
	latencyThresholdMs      int32
	hold                    time.Duration
	scaleUpThrottlePermille int32
	maxQueueDepth           int32
}

// effectiveProtectConfig 归一化保护配置。
func effectiveProtectConfig(s sandboxv1alpha1.ProtectModeSpec) protectConfig {
	// Enabled 是 *bool 而不是 bool，正是为了这一行。
	//
	// 若用 bool，零值（未设置）与显式 false 无法区分，而这个字段的默认值
	// 必须是 true。后果很具体：任何不经过 API Server 默认化的写入
	// （单测构造的对象、某些 SSA 场景、直接用 kubectl 改对象）
	// 都会静默把雪崩保护关掉 —— 而"安全机制被静默关闭"是最不该发生的一类问题。
	enabled := true
	if s.Enabled != nil {
		enabled = *s.Enabled
	}

	c := protectConfig{
		enabled:                 enabled,
		failureRatePercent:      s.FailureRatePercent,
		minSamples:              s.MinSamples,
		latencyThresholdMs:      s.ApiserverLatencyThresholdMs,
		scaleUpThrottlePermille: s.ScaleUpThrottlePermille,
		maxQueueDepth:           s.MaxQueueDepth,
	}
	if c.failureRatePercent <= 0 {
		c.failureRatePercent = DefaultProtectFailureRatePercent
	}
	if c.minSamples <= 0 {
		c.minSamples = DefaultProtectMinSamples
	}
	if c.latencyThresholdMs <= 0 {
		c.latencyThresholdMs = DefaultProtectLatencyThresholdMs
	}
	if c.scaleUpThrottlePermille <= 0 {
		c.scaleUpThrottlePermille = DefaultProtectScaleUpThrottle
	}
	if c.maxQueueDepth <= 0 {
		c.maxQueueDepth = DefaultProtectMaxQueueDepth
	}
	hold := s.HoldSeconds
	if hold <= 0 {
		hold = DefaultProtectHoldSeconds
	}
	c.hold = time.Duration(hold) * time.Second
	return c
}

// EffectiveProtectThrottlePermille 返回保护生效时的扩容限速比例。
//
// 单独导出：pool_scaling.go 需要在"保护中"时把扩容限速乘上它，
// 而它不应该去关心其余的保护参数。
func EffectiveProtectThrottlePermille(s sandboxv1alpha1.ProtectModeSpec) int32 {
	return effectiveProtectConfig(s).scaleUpThrottlePermille
}

// EvaluateProtectMode 是保护模式判定的唯一决策点，纯函数。
//
// 触发条件（docs/05 §5）：provision 失败率超阈值 **或** API Server P99 超阈值。
// 进入后至少保持 HoldSeconds（滞回），触发条件持续存在则不断续期。
func EvaluateProtectMode(spec sandboxv1alpha1.ProtectModeSpec, o ProtectObservation) ProtectDecision {
	cfg := effectiveProtectConfig(spec)

	d := ProtectDecision{
		Trips:            o.PrevTrips,
		ThrottlePermille: 1000,
	}

	if !cfg.enabled {
		// 显式关闭：不判定、不滞回。但仍要如实报告"本轮从 active 变成
		// inactive"，否则关闭配置之后上一轮留下的告警会永远挂着。
		d.RecoveredNow = o.PrevActive
		return d
	}

	ratePermille, attempts := provisionFailureRate(o.Inflight, o.Failed)
	d.FailureRatePermille = ratePermille

	reason := ProtectReasonNone

	// 样本不足时不判定失败率。这是本函数最容易写错的一处：
	// 样本不足时 rate 依然是一个"看起来完全合理"的数字
	// （1 次尝试 1 次失败 = 1000‰），照它判定就会误触发。
	if attempts >= cfg.minSamples && ratePermille >= cfg.failureRatePercent*10 {
		reason = ProtectReasonProvisionFailureRate
	}
	if reason == ProtectReasonNone && o.LatencyKnown && o.ApiserverLatencyP99Ms >= cfg.latencyThresholdMs {
		reason = ProtectReasonApiserverLatency
	}

	if reason != ProtectReasonNone {
		// 触发中：进入或**续期**。
		//
		// 续期是必须的：若只在"首次触发"时设一次 until，一场持续 10 分钟的
		// 危机会在 120s 后自动解除保护 —— 恰好在最需要它的时候。
		d.Active = true
		d.Reason = reason
		d.Until = o.Now.Add(cfg.hold)
		d.ThrottlePermille = cfg.scaleUpThrottlePermille
		d.Since = o.PrevSince
		if d.Since.IsZero() {
			d.Since = o.Now
		}
		if !o.PrevActive {
			d.Trips = o.PrevTrips + 1
			d.TrippedNow = true
		}
		return d
	}

	// 触发条件已消失：滞回期内继续保护。
	if o.PrevActive && o.PrevUntil.After(o.Now) {
		d.Active = true
		d.Reason = ProtectReasonHold
		d.Since = o.PrevSince
		d.Until = o.PrevUntil
		// 滞回期内仍处于保护中，因此限速同样生效 ——
		// 少这一行会让"滞回期"变成一个只写 status、不限速的空壳。
		d.ThrottlePermille = cfg.scaleUpThrottlePermille
		return d
	}

	// 恢复。
	d.RecoveredNow = o.PrevActive
	d.Since = o.PrevSince
	return d
}

// provisionFailureRate 返回失败率（千分比）与参与判定的样本数。
//
// 分母刻意取 inflight+failed，而不是"池里的全部沙箱"：
// 用全量做分母时，一个 5000 实例的健康池即使**新建全部失败**，
// 失败率也只有约 0.1% —— 保护模式永远不会触发，而这恰恰是它唯一要应对的场景。
// inflight+failed 是"尚未确认成功 + 已经失败"的那一批，它的比例在正常时
// 接近 0、在故障时迅速逼近 1，因此是一个会真正动的信号。
//
// 已知边界（写在这里而不是留给人猜）：这**不是**严格的 60s 滑动窗口。
// 严格滑窗需要控制器持有内存时间序列，而那种计数器会在重启后清零 ——
// 恰好在最需要保护的时刻失效。从对象状态推导则没有这个性质，
// 代价是它只能看到"当前还处于 inflight/failed 的对象"。
// 这个取舍与 Sweeper 从集群现状对账（而不是维护自己的账本）是同一类选择。
func provisionFailureRate(inflight, failed int32) (permille int32, attempts int32) {
	attempts = inflight + failed
	if attempts <= 0 {
		return 0, 0
	}
	return int32(math.Round(float64(failed) * 1000 / float64(attempts))), attempts
}
