package controller

import (
	"fmt"
	"testing"
	"time"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
)

var poolT0 = time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

// newPool 构造一个"参数齐全"的池，避免每个用例都重复填默认值。
func newPool(minWarm, maxWarm int32) *sandboxv1alpha1.SandboxPool {
	return &sandboxv1alpha1.SandboxPool{
		Spec: sandboxv1alpha1.SandboxPoolSpec{
			Scaling: sandboxv1alpha1.SandboxPoolScaling{
				MinWarm:                minWarm,
				MaxWarm:                maxWarm,
				TargetWarmBuffer:       5,
				MaxProvisionPerSecond:  10,
				MaxReclaimPerSecond:    5,
				ReplenishSeconds:       60,
				TargetHitRatioPermille: 900,
				Dampening: sandboxv1alpha1.DampeningSpec{
					ScaleUpCooldownSeconds:   15,
					ScaleDownCooldownSeconds: 180,
					HysteresisRatioPermille:  100,
					EWMAAlphaPermille:        300,
				},
			},
		},
	}
}

// newWidePool 关闭限速与上限，用于观察决策的"原始"取值 ——
// 否则用例测到的只是限速器，而不是算法本身。
func newWidePool() *sandboxv1alpha1.SandboxPool {
	p := newPool(0, 200)
	p.Spec.Scaling.MaxProvisionPerSecond = 1000
	p.Spec.Scaling.MaxReclaimPerSecond = 1000
	p.Spec.Scaling.TargetWarmBuffer = 5
	return p
}

func scaleObs() ScaleObservation {
	return ScaleObservation{
		Now:                poolT0,
		ClaimRatePerSecond: 0,
		HitRatioPermille:   900, // 恰好等于目标 → 反馈中性
		NodeHeadroom:       -1,  // 未知/无限制
	}
}

func TestDecideScaling(t *testing.T) {
	tests := []struct {
		name         string
		pool         *sandboxv1alpha1.SandboxPool
		mutate       func(*ScaleObservation)
		wantAction   ScaleAction
		wantDelta    int32
		wantReason   string
		wantSuppress bool
	}{
		{
			name: "空池且有需求 → 扩容",
			pool: newPool(2, 100),
			mutate: func(o *ScaleObservation) {
				o.ClaimRatePerSecond = 1 // × ReplenishSeconds 60 = 60 个待补
			},
			wantAction: ScaleUp,
			// demand = 0.3*60 + 0.7*0 = 18；target = max(2,18)+5 = 23
			// delta = 23，被 maxProvisionPerSecond=10 限速
			wantDelta:  10,
			wantReason: ScaleReasonDemandUp,
		},
		{
			// 这是池化实现里最典型的一类超额扩容：上一轮的库存还在创建中，
			// 控制器看不到它们，于是每个 reconcile 再补一批。
			name: "Inflight 已参与计算 → 不重复扩容",
			pool: newPool(2, 100),
			mutate: func(o *ScaleObservation) {
				o.ClaimRatePerSecond = 1
				o.Warm = 0
				o.Inflight = 23 // 已经把 23 个水位补上了
			},
			wantAction: ScaleHold,
			wantDelta:  0,
			// 独立原因码：它让运维能在生产里直接验证"防重复扩容"确实在生效
			wantReason: ScaleReasonProvisioningPending,
		},
		{
			name: "低峰无需求 → 保底到 minWarm",
			pool: newPool(5, 100),
			mutate: func(o *ScaleObservation) {
				o.ClaimRatePerSecond = 0
				o.PredictedDemand = 0
			},
			wantAction: ScaleUp, // target = max(5,0)+5 = 10
			wantDelta:  10,
		},
		{
			name: "已超目标 → 缩容",
			pool: newPool(2, 100),
			mutate: func(o *ScaleObservation) {
				o.Warm = 60 // target = max(2,0)+5 = 7
			},
			wantAction: ScaleDown,
			wantDelta:  -5, // target-warm = -53，被 maxReclaimPerSecond=5 限速
		},
		{
			// maxReclaim 足以一次缩过头，但保底线必须拦住它。
			name: "缩容被 minWarm 拦住（不会一次缩过头）",
			pool: func() *sandboxv1alpha1.SandboxPool {
				p := newPool(50, 100)
				p.Spec.Scaling.MaxReclaimPerSecond = 20
				return p
			}(),
			mutate: func(o *ScaleObservation) {
				o.Warm = 60
				o.Inflight = 30 // target=55；缺口 = 55-90 = -35 → 限速到 -20 → 再被 minWarm 收到 -10
			},
			wantAction: ScaleDown,
			wantDelta:  -10,
			wantReason: ScaleReasonMinWarmReached,
		},
		{
			// 水位已经低于保底线时**不得**再缩。
			// 原实现写成 delta = warm - minWarm，此时得到负值，会反向加大缩容。
			name: "水位已低于 minWarm → 不缩容",
			pool: func() *sandboxv1alpha1.SandboxPool {
				p := newPool(50, 100)
				p.Spec.Scaling.MaxReclaimPerSecond = 20
				return p
			}(),
			mutate: func(o *ScaleObservation) {
				o.Warm = 40     // 低于 minWarm=50
				o.Inflight = 30 // target=55；缺口 = 55-70 = -15（在限速内）
			},
			wantAction: ScaleHold,
			wantDelta:  0,
			wantReason: ScaleReasonMinWarmReached,
		},
		{
			name: "命中率未知 → 反馈中性（不按 0 计算）",
			pool: newWidePool(),
			mutate: func(o *ScaleObservation) {
				o.ClaimRatePerSecond = 1
				o.HitRatioPermille = -1 // 未知
			},
			// demand = 0.3*60 = 18；feedback = 1.0 → target = 18+5 = 23
			wantAction: ScaleUp,
			wantDelta:  23,
		},
		{
			name: "命中率真的为 0 → 反馈上调到上限",
			pool: newWidePool(),
			mutate: func(o *ScaleObservation) {
				o.ClaimRatePerSecond = 1
				o.HitRatioPermille = 0 // 真的是 0，必须区别于"未知"
			},
			// gap = 0.9 → feedback = clamp(1+1.8) = 1.8 → target = 32.4+5 = 37.4 → 37
			wantAction: ScaleUp,
			wantDelta:  37,
		},
		{
			name: "命中率高于目标 → 反馈下调",
			pool: newWidePool(),
			mutate: func(o *ScaleObservation) {
				o.ClaimRatePerSecond = 1
				o.HitRatioPermille = 1000 // gap = -0.1 → feedback = 0.8
			},
			// target = 18*0.8 + 5 = 19.4 → 19
			wantAction: ScaleUp,
			wantDelta:  19,
		},
		{
			name: "冷却期内（同方向）→ 抑制",
			pool: newPool(2, 100),
			mutate: func(o *ScaleObservation) {
				o.ClaimRatePerSecond = 1
				o.LastScaleAt = poolT0.Add(-5 * time.Second) // 扩容冷却 15s 未过
				o.LastScaleDelta = 8                         // 同方向
			},
			wantAction:   ScaleHold,
			wantDelta:    0,
			wantReason:   ScaleReasonCooldown,
			wantSuppress: true,
		},
		{
			name: "方向反转 → 冷却重新计时（不被上一条记录放行）",
			pool: newPool(2, 100),
			mutate: func(o *ScaleObservation) {
				o.ClaimRatePerSecond = 1
				o.LastScaleAt = poolT0.Add(-5 * time.Second)
				o.LastScaleDelta = -8 // 上一条是缩容，本次是扩容 → 冷却不适用
			},
			wantAction: ScaleUp,
			wantDelta:  10,
		},
		{
			name: "缺口落在滞回带内 → 不动作",
			pool: func() *sandboxv1alpha1.SandboxPool {
				p := newWidePool()
				p.Spec.Scaling.ReplenishSeconds = 1
				p.Spec.Scaling.TargetWarmBuffer = 1
				p.Spec.Scaling.Dampening.EWMAAlphaPermille = 1000 // 关掉平滑，方便精确构造
				return p
			}(),
			mutate: func(o *ScaleObservation) {
				o.ClaimRatePerSecond = 100 // demand = 100 → target = 101
				o.Warm = 95                // 缺口 6，滞回阈值 = ceil(101×0.1) = 11
			},
			wantAction:   ScaleHold,
			wantDelta:    0,
			wantReason:   ScaleReasonHysteresis,
			wantSuppress: true,
		},
		{
			name: "缺口超出滞回带 → 动作",
			pool: func() *sandboxv1alpha1.SandboxPool {
				p := newWidePool()
				p.Spec.Scaling.ReplenishSeconds = 1
				p.Spec.Scaling.TargetWarmBuffer = 1
				p.Spec.Scaling.Dampening.EWMAAlphaPermille = 1000
				return p
			}(),
			mutate: func(o *ScaleObservation) {
				o.ClaimRatePerSecond = 100
				o.Warm = 50 // 缺口 51，远超阈值 11
			},
			wantAction: ScaleUp,
			wantDelta:  51,
		},
		{
			name: "振荡超阈 → 冻结决策",
			pool: newPool(2, 100),
			mutate: func(o *ScaleObservation) {
				o.ClaimRatePerSecond = 1
				o.OscillationReversals = maxReversalsInWindow
			},
			wantAction:   ScaleHold,
			wantDelta:    0,
			wantReason:   ScaleReasonOscillation,
			wantSuppress: true,
		},
		{
			// 需求远大于上限时，目标水位被 maxWarm 钳住。
			// 必须把限速抬高，否则测到的是限速器而不是上限。
			name: "maxWarm 硬上限",
			pool: func() *sandboxv1alpha1.SandboxPool {
				p := newPool(2, 20)
				p.Spec.Scaling.MaxProvisionPerSecond = 1000
				return p
			}(),
			mutate: func(o *ScaleObservation) {
				o.ClaimRatePerSecond = 5 // 需求远大于上限
			},
			wantAction: ScaleUp,
			wantDelta:  20, // 恰好补到 maxWarm，而不是按需求扩到 95
			wantReason: ScaleReasonMaxWarmReached,
		},
		{
			// 运维闸门：停止动作，但**不算被抑制** ——
			// 否则计划内维护期间会持续触发"池被抑制"的告警。
			name: "pauseScaling → 完全不动",
			pool: func() *sandboxv1alpha1.SandboxPool {
				p := newPool(2, 100)
				p.Spec.Scaling.PauseScaling = true
				return p
			}(),
			mutate:       func(o *ScaleObservation) { o.ClaimRatePerSecond = 5 },
			wantAction:   ScaleHold,
			wantDelta:    0,
			wantReason:   ScaleReasonPaused,
			wantSuppress: false,
		},
		{
			name: "pool drain → 停止扩容（回收由 drain 分支负责）",
			pool: func() *sandboxv1alpha1.SandboxPool {
				p := newPool(2, 100)
				p.Spec.Drain = sandboxv1alpha1.PoolDrain{Enabled: true, Reason: "kata upgrade"}
				return p
			}(),
			mutate:       func(o *ScaleObservation) { o.ClaimRatePerSecond = 5 },
			wantAction:   ScaleHold,
			wantDelta:    0,
			wantReason:   ScaleReasonDraining,
			wantSuppress: false,
		},
		{
			name: "节点容量为 0 → 抑制并把原因指向节点",
			pool: newPool(2, 100),
			mutate: func(o *ScaleObservation) {
				o.ClaimRatePerSecond = 1
				o.NodeHeadroom = 0
			},
			wantAction:   ScaleHold,
			wantDelta:    0,
			wantReason:   ScaleReasonNodeCapacity,
			wantSuppress: true,
		},
		{
			name: "节点容量不足 → 扩多少算多少，原因码指向节点",
			pool: newPool(2, 100),
			mutate: func(o *ScaleObservation) {
				o.ClaimRatePerSecond = 1
				o.NodeHeadroom = 4
			},
			wantAction: ScaleUp,
			wantDelta:  4, // 需求是 23，但节点只放得下 4
			wantReason: ScaleReasonNodeCapacity,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := scaleObs()
			if tc.mutate != nil {
				tc.mutate(&o)
			}
			got := DecideScaling(tc.pool, o)

			if got.Action != tc.wantAction {
				t.Errorf("Action = %s, 期望 %s (reason=%s)", got.Action, tc.wantAction, got.Reason)
			}
			if got.Delta != tc.wantDelta {
				t.Errorf("Delta = %d, 期望 %d (reason=%s target=%d)",
					got.Delta, tc.wantDelta, got.Reason, got.Target)
			}
			if tc.wantReason != "" && got.Reason != tc.wantReason {
				t.Errorf("Reason = %s, 期望 %s", got.Reason, tc.wantReason)
			}
			if got.Suppressed != tc.wantSuppress {
				t.Errorf("Suppressed = %v, 期望 %v", got.Suppressed, tc.wantSuppress)
			}
		})
	}
}

// TestDecideScaling_MetricsUnknownIsNeutral 是一条安全不变式：
// "指标不可用"必须与"命中率恰好达标"产生**完全相同**的目标水位。
//
// 若两者不同（例如把未知当成 0），那么一次监控故障就会让反馈项把目标
// 推向 1.8 倍上限 —— 监控断了反而触发了大规模扩容，这是最难排查的一类故障。
func TestDecideScaling_MetricsUnknownIsNeutral(t *testing.T) {
	pool := newWidePool()

	unknown := scaleObs()
	unknown.ClaimRatePerSecond = 1
	unknown.HitRatioPermille = -1

	atTarget := scaleObs()
	atTarget.ClaimRatePerSecond = 1
	atTarget.HitRatioPermille = pool.Spec.Scaling.TargetHitRatioPermille

	a := DecideScaling(pool, unknown)
	b := DecideScaling(pool, atTarget)

	if a.Target != b.Target || a.Delta != b.Delta {
		t.Fatalf("指标未知时的决策必须与命中率达标一致：unknown(target=%d delta=%d) vs atTarget(target=%d delta=%d)",
			a.Target, a.Delta, b.Target, b.Delta)
	}
	if !a.MetricsUnavailable {
		t.Error("指标未知时必须置 MetricsUnavailable，控制器要据此挂 Condition")
	}
	if b.MetricsUnavailable {
		t.Error("指标可用时不应置 MetricsUnavailable")
	}
}

// TestDecideScaling_Invariants 是守护性测试：在一个输入网格上穷举，
// 验证决策永远不违反这几条不变量。
//
// 池化最容易出的两类问题（重复扩容、扩缩振荡）都表现为"某个组合下
// delta 的符号或量级不对"，而这类组合往往不在手写用例的覆盖里。
func TestDecideScaling_Invariants(t *testing.T) {
	pools := []*sandboxv1alpha1.SandboxPool{
		newPool(0, 0), // 未配置上限
		newPool(2, 50),
		newPool(10, 10), // min == max
		newWidePool(),
	}
	warmValues := []int32{0, 1, 5, 50, 200}
	inflightValues := []int32{0, 1, 20, 100}
	rates := []float64{0, 0.1, 1, 10}
	hitRatios := []int32{-1, 0, 500, 900, 1000}
	reversals := []int{0, 3}

	for _, pool := range pools {
		for _, warm := range warmValues {
			for _, inflight := range inflightValues {
				for _, rate := range rates {
					for _, hr := range hitRatios {
						for _, rev := range reversals {
							o := ScaleObservation{
								Now:                  poolT0,
								Warm:                 warm,
								Inflight:             inflight,
								ClaimRatePerSecond:   rate,
								HitRatioPermille:     hr,
								PredictedDemand:      warm,
								OscillationReversals: rev,
								NodeHeadroom:         -1,
							}
							d := DecideScaling(pool, o)
							ctx := scenario(pool, o)

							// 1. 动作与 delta 的符号必须自洽。
							switch d.Action {
							case ScaleUp:
								if d.Delta <= 0 {
									t.Fatalf("%s: ScaleUp 但 delta=%d", ctx, d.Delta)
								}
							case ScaleDown:
								if d.Delta >= 0 {
									t.Fatalf("%s: ScaleDown 但 delta=%d", ctx, d.Delta)
								}
							case ScaleHold:
								if d.Delta != 0 {
									t.Fatalf("%s: ScaleHold 但 delta=%d", ctx, d.Delta)
								}
							default:
								t.Fatalf("%s: 未知动作 %q", ctx, d.Action)
							}

							// 2. 被抑制就必须不动。
							if d.Suppressed && d.Delta != 0 {
								t.Fatalf("%s: Suppressed 但仍要动 delta=%d", ctx, d.Delta)
							}

							// 3. 限速不可被绕过。
							sc := pool.Spec.Scaling
							if d.Delta > effectiveMaxProvision(sc) {
								t.Fatalf("%s: delta=%d 超过扩容限速 %d",
									ctx, d.Delta, effectiveMaxProvision(sc))
							}
							if d.Delta < -effectiveMaxReclaim(sc) {
								t.Fatalf("%s: delta=%d 超过缩容限速 %d",
									ctx, d.Delta, -effectiveMaxReclaim(sc))
							}

							// 4. 目标水位不得越界。
							if sc.MaxWarm > 0 && d.Target > sc.MaxWarm {
								t.Fatalf("%s: target=%d 超过 maxWarm=%d", ctx, d.Target, sc.MaxWarm)
							}
							if d.Target < sc.MinWarm {
								t.Fatalf("%s: target=%d 低于 minWarm=%d", ctx, d.Target, sc.MinWarm)
							}

							// 5. 缩容不得把水位压到 minWarm 之下。
							if d.Delta < 0 && warm+d.Delta < sc.MinWarm {
								t.Fatalf("%s: 缩容后水位 %d 低于 minWarm=%d",
									ctx, warm+d.Delta, sc.MinWarm)
							}

							// 6. 原因码必须非空（它要进指标 label）。
							if d.Reason == "" {
								t.Fatalf("%s: 原因码为空", ctx)
							}
						}
					}
				}
			}
		}
	}
}

func scenario(pool *sandboxv1alpha1.SandboxPool, o ScaleObservation) string {
	sc := pool.Spec.Scaling
	return fmt.Sprintf("pool(min=%d,max=%d) warm=%d inflight=%d rate=%g hit=%d rev=%d headroom=%d",
		sc.MinWarm, sc.MaxWarm, o.Warm, o.Inflight, o.ClaimRatePerSecond,
		o.HitRatioPermille, o.OscillationReversals, o.NodeHeadroom)
}

// TestDecideScaling_Idempotent 确认同一输入重复决策结果一致 ——
// reconcile 会被以任意顺序重复调用，幂等是它的基本前提。
func TestDecideScaling_Idempotent(t *testing.T) {
	pool := newPool(2, 100)
	o := scaleObs()
	o.ClaimRatePerSecond = 1

	first := DecideScaling(pool, o)
	for i := 0; i < 50; i++ {
		if got := DecideScaling(pool, o); got != first {
			t.Fatalf("第 %d 次决策不同: %+v vs %+v", i, got, first)
		}
	}
}

func TestDecideScaling_NilPool(t *testing.T) {
	got := DecideScaling(nil, scaleObs())
	if got.Action != ScaleHold || got.Delta != 0 {
		t.Fatalf("nil 池必须安全地不动作，实际 %+v", got)
	}
}

// TestPoolDefaults_MatchCRDMarkers 守护"代码默认值"与"CRD 默认值"不漂移。
// 两处不一致的表现是"单测通过但线上行为不同"，几乎无法从日志里看出来。
func TestPoolDefaults_MatchCRDMarkers(t *testing.T) {
	if DefaultTargetWarmBuffer != 10 {
		t.Errorf("DefaultTargetWarmBuffer=%d，必须与 SandboxPoolScaling.TargetWarmBuffer "+
			"的 +kubebuilder:default=10 保持一致", DefaultTargetWarmBuffer)
	}
	if got := EffectiveTargetWarmBuffer(sandboxv1alpha1.SandboxPoolScaling{}); got != 10 {
		t.Errorf("未配置时应回落到 10，实际 %d", got)
	}
	if got := effectiveReplenishSeconds(sandboxv1alpha1.SandboxPoolScaling{}); got != 90 {
		t.Errorf("ReplenishSeconds 未配置时应回落到 90，实际 %d", got)
	}
	if got := effectiveMaxProvision(sandboxv1alpha1.SandboxPoolScaling{}); got != 50 {
		t.Errorf("MaxProvisionPerSecond 未配置时应回落到 50，实际 %d", got)
	}
	if got := effectiveMaxReclaim(sandboxv1alpha1.SandboxPoolScaling{}); got != 30 {
		t.Errorf("MaxReclaimPerSecond 未配置时应回落到 30，实际 %d", got)
	}
}
