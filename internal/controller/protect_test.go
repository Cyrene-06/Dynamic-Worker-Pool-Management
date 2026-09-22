package controller

import (
	"testing"
	"time"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
)

func boolPtr(b bool) *bool { return &b }

var protectT0 = time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

// TestEvaluateProtectMode 穷举保护模式的判定。
//
// 这个函数值得穷举的理由：它的两个错误方向代价都很大且方向相反 ——
//   - 误触发 → 正常业务被拒绝（可用性事故）
//   - 该触发没触发 → 系统在容量危机里自我放大（雪崩）
//
// 因此每个阈值边界、每个"未知"取值都必须有明确用例。
func TestEvaluateProtectMode(t *testing.T) {
	tests := []struct {
		name string
		spec sandboxv1alpha1.ProtectModeSpec
		obs  ProtectObservation

		wantActive    bool
		wantReason    string
		wantTripped   bool
		wantRecovered bool
		wantTrips     int64
		wantThrottle  int32
	}{
		{
			name: "正常态（有在途、无失败）不触发",
			obs:  ProtectObservation{Now: protectT0, Inflight: 30, Failed: 0},
			// 分母是 inflight+failed，因此"健康"的形态是失败为 0。
			wantActive: false, wantThrottle: 1000,
		},
		{
			name: "失败率恰好等于阈值（300‰）即触发",
			obs:  ProtectObservation{Now: protectT0, Inflight: 7, Failed: 3},
			// 边界取 >=：阈值是"达到这个失败率就该保护"，
			// 取 > 会让配置成 30 的人实际得到 31 才触发，而这一点没人会去核对。
			wantActive: true, wantReason: ProtectReasonProvisionFailureRate,
			wantTripped: true, wantTrips: 1, wantThrottle: 500,
		},
		{
			name:       "失败率低于阈值不触发",
			obs:        ProtectObservation{Now: protectT0, Inflight: 8, Failed: 2},
			wantActive: false, wantThrottle: 1000,
		},
		{
			name: "样本不足时不触发（否则 1/1 就是 100%）",
			obs:  ProtectObservation{Now: protectT0, Inflight: 0, Failed: 3},
			// 这是最容易写错的一条：3/(0+3)=1000‰ 看起来完全合理，
			// 但样本量只有 3。没有 MinSamples 就会出现"任何一次失败都触发保护"。
			wantActive: false, wantThrottle: 1000,
		},
		{
			name: "持续触发时续期，但不重复计次",
			spec: sandboxv1alpha1.ProtectModeSpec{},
			obs: ProtectObservation{
				Now: protectT0, Inflight: 0, Failed: 20,
				PrevActive: true, PrevTrips: 1, PrevSince: protectT0.Add(-time.Minute),
			},
			// Trips 保持不变很重要：它在 status 里是"累计进入次数"，
			// 每轮都自增会让"抖动"与"长期故障"变得无法区分。
			wantActive: true, wantReason: ProtectReasonProvisionFailureRate,
			wantTripped: false, wantTrips: 1, wantThrottle: 500,
		},
		{
			name: "触发条件消失但滞回期未过 → 保持保护",
			obs: ProtectObservation{
				Now: protectT0, Inflight: 30, Failed: 0,
				PrevActive: true, PrevTrips: 2,
				PrevSince: protectT0.Add(-10 * time.Minute),
				PrevUntil: protectT0.Add(30 * time.Second),
			},
			wantActive: true, wantReason: ProtectReasonHold,
			wantTrips: 2, wantThrottle: 500,
		},
		{
			name: "滞回期结束 → 恢复",
			obs: ProtectObservation{
				Now: protectT0, Inflight: 30, Failed: 0,
				PrevActive: true, PrevTrips: 2,
				PrevSince: protectT0.Add(-10 * time.Minute),
				PrevUntil: protectT0.Add(-time.Second),
			},
			wantActive: false, wantRecovered: true, wantTrips: 2, wantThrottle: 1000,
		},
		{
			name: "API Server 延迟超阈值触发",
			obs: ProtectObservation{
				Now: protectT0, Inflight: 30,
				LatencyKnown: true, ApiserverLatencyP99Ms: 2500,
			},
			wantActive: true, wantReason: ProtectReasonApiserverLatency,
			wantTripped: true, wantTrips: 1, wantThrottle: 500,
		},
		{
			name: "延迟恰好等于阈值（2000ms）即触发",
			obs: ProtectObservation{
				Now: protectT0, Inflight: 30,
				LatencyKnown: true, ApiserverLatencyP99Ms: 2000,
			},
			wantActive: true, wantReason: ProtectReasonApiserverLatency,
			wantTripped: true, wantTrips: 1, wantThrottle: 500,
		},
		{
			name: "延迟未知时不触发",
			obs: ProtectObservation{
				Now: protectT0, Inflight: 30,
				LatencyKnown: false, ApiserverLatencyP99Ms: 9000,
			},
			// 未知不能当病态：一次监控故障会变成一次全量拒绝。
			// 但也不能当健康（那会让保护在监控断掉时失灵）——
			// 因此它的语义是"这条判据本轮弃权"，另一个判据仍然生效。
			wantActive: false, wantThrottle: 1000,
		},
		{
			name: "两个判据同时满足时报失败率（更具体的那个）",
			obs: ProtectObservation{
				Now: protectT0, Inflight: 0, Failed: 20,
				LatencyKnown: true, ApiserverLatencyP99Ms: 5000,
			},
			wantActive: true, wantReason: ProtectReasonProvisionFailureRate,
			wantTripped: true, wantTrips: 1, wantThrottle: 500,
		},
		{
			name: "显式关闭时不判定（上一轮处于保护 → 报恢复）",
			spec: sandboxv1alpha1.ProtectModeSpec{Enabled: boolPtr(false)},
			obs: ProtectObservation{
				Now: protectT0, Inflight: 0, Failed: 100,
				PrevActive: true, PrevTrips: 3,
			},
			// 关闭后仍然要如实报告"本轮不再处于保护"，
			// 否则上一轮留下的告警会永远挂着（关了配置却关不掉告警）。
			wantActive: false, wantRecovered: true, wantTrips: 3, wantThrottle: 1000,
		},
		{
			name:       "显式关闭且此前未进入 → 不报恢复事件",
			spec:       sandboxv1alpha1.ProtectModeSpec{Enabled: boolPtr(false)},
			obs:        ProtectObservation{Now: protectT0, Inflight: 0, Failed: 100},
			wantActive: false, wantRecovered: false, wantThrottle: 1000,
		},
		{
			name: "自定义阈值与限速",
			spec: sandboxv1alpha1.ProtectModeSpec{
				FailureRatePercent:      50,
				MinSamples:              4,
				HoldSeconds:             10,
				ScaleUpThrottlePermille: 250,
			},
			obs:        ProtectObservation{Now: protectT0, Inflight: 2, Failed: 2},
			wantActive: true, wantReason: ProtectReasonProvisionFailureRate,
			wantTripped: true, wantTrips: 1, wantThrottle: 250,
		},
		{
			name: "未配置 Enabled 时默认开启",
			spec: sandboxv1alpha1.ProtectModeSpec{},
			obs:  ProtectObservation{Now: protectT0, Inflight: 0, Failed: 20},
			// 这条是 API 用 *bool 的理由：若用 bool，零值与显式 false 无法区分，
			// 于是"忘记设置"会静默关掉保护 —— 而这是最不该发生的一类问题。
			wantActive: true, wantReason: ProtectReasonProvisionFailureRate,
			wantTripped: true, wantTrips: 1, wantThrottle: 500,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateProtectMode(tc.spec, tc.obs)

			if got.Active != tc.wantActive {
				t.Fatalf("Active = %v，期望 %v（reason=%s）", got.Active, tc.wantActive, got.Reason)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("Reason = %q，期望 %q", got.Reason, tc.wantReason)
			}
			if got.TrippedNow != tc.wantTripped {
				t.Errorf("TrippedNow = %v，期望 %v", got.TrippedNow, tc.wantTripped)
			}
			if got.RecoveredNow != tc.wantRecovered {
				t.Errorf("RecoveredNow = %v，期望 %v", got.RecoveredNow, tc.wantRecovered)
			}
			if got.Trips != tc.wantTrips {
				t.Errorf("Trips = %d，期望 %d", got.Trips, tc.wantTrips)
			}
			if got.ThrottlePermille != tc.wantThrottle {
				t.Errorf("ThrottlePermille = %d，期望 %d", got.ThrottlePermille, tc.wantThrottle)
			}

			// 不变量：active 与 throttle 必须一致。
			// 一个"保护中但没限速"的组合说明实现漏了一处，
			// 而它在集群里表现为"保护模式看似生效、扩容却照常全速"。
			if got.Active && got.ThrottlePermille >= 1000 {
				t.Errorf("保护生效时限速必须小于 1000，实际 %d", got.ThrottlePermille)
			}
			if !got.Active && got.ThrottlePermille != 1000 {
				t.Errorf("未保护时限速必须是 1000，实际 %d", got.ThrottlePermille)
			}
		})
	}
}

// TestEvaluateProtectMode_HoldIsNotRenewedByHold 守住"滞回期不会被自己续期"。
//
// 如果续期逻辑写成"只要 active 就延长 until"，保护模式永远不会退出 ——
// 成了一个需要人工介入才能解除的锁存器，而它的告警会一直响到人去处理。
func TestEvaluateProtectMode_HoldIsNotRenewedByHold(t *testing.T) {
	spec := sandboxv1alpha1.ProtectModeSpec{}
	until := protectT0.Add(30 * time.Second)
	got := EvaluateProtectMode(spec, ProtectObservation{
		Now: protectT0, Inflight: 30,
		PrevActive: true, PrevUntil: until, PrevSince: protectT0.Add(-time.Minute),
	})
	if !got.Active {
		t.Fatal("滞回期内必须保持保护")
	}
	if !got.Until.Equal(until) {
		t.Fatalf("滞回期内 Until 必须保持原值，实际 %s（期望 %s）", got.Until, until)
	}
}

// TestEvaluateProtectMode_SinceIsPreserved 守住"进入时刻不被刷新"。
//
// Since 是运维判断"这场危机已经持续多久"的唯一依据。
// 每轮刷新它会让持续 20 分钟的故障看起来刚刚发生。
func TestEvaluateProtectMode_SinceIsPreserved(t *testing.T) {
	since := protectT0.Add(-20 * time.Minute)
	got := EvaluateProtectMode(sandboxv1alpha1.ProtectModeSpec{}, ProtectObservation{
		Now: protectT0, Inflight: 0, Failed: 20,
		PrevActive: true, PrevSince: since, PrevTrips: 3,
		PrevUntil: protectT0.Add(-time.Minute),
	})
	if !got.Active {
		t.Fatal("仍在失败中必须持续保护")
	}
	if !got.Since.Equal(since) {
		t.Fatalf("Since 被刷新为 %s，期望保持 %s", got.Since, since)
	}
}

func TestProvisionFailureRate(t *testing.T) {
	tests := []struct {
		inflight, failed int32
		wantPermille     int32
		wantAttempts     int32
	}{
		// 分母是 inflight+failed：inflight 尚未确认成功，failed 已经失败。
		// 注意 {5,5} 是 50%（500‰）而不是 100% —— 那 5 个还在创建中，不是失败。
		{0, 0, 0, 0},
		{30, 0, 0, 30},
		{7, 3, 300, 10},
		{5, 5, 500, 10},
		{0, 10, 1000, 10},
		{9, 1, 100, 10},
		// 分母刻意**不包含** Healthy/Running 的沙箱：
		// 一个 5000 实例的健康池若新建全部失败，用全量做分母只有 0.1%，
		// 保护模式永远不会触发 —— 而那正是它唯一要应对的场景。
		{0, 3, 1000, 3},
	}
	for _, tc := range tests {
		gotPermille, gotAttempts := provisionFailureRate(tc.inflight, tc.failed)
		if gotPermille != tc.wantPermille || gotAttempts != tc.wantAttempts {
			t.Errorf("provisionFailureRate(%d, %d) = (%d, %d)，期望 (%d, %d)",
				tc.inflight, tc.failed, gotPermille, gotAttempts, tc.wantPermille, tc.wantAttempts)
		}
	}
}

func TestThrottleScaleUp(t *testing.T) {
	tests := []struct {
		limit, permille, want int32
	}{
		{100, 500, 50},
		{100, 1000, 100},
		{100, 0, 100}, // 0 视为"不限速"，而不是"限到 0"
		{1, 500, 1},   // 至少保留 1：限速不是停止扩容
		{3, 100, 1},   // 0.3 → 向上取整为 1
		// 非法大值不得把限速反向放大。
		{100, 2000, 100},
	}
	for _, tc := range tests {
		if got := throttleScaleUp(tc.limit, tc.permille); got != tc.want {
			t.Errorf("throttleScaleUp(%d, %d) = %d，期望 %d", tc.limit, tc.permille, got, tc.want)
		}
	}
}

// TestDecideScaling_ProtectModeThrottlesScaleUp 守住"保护模式确实压低了扩容幅度"。
//
// 只测 EvaluateProtectMode 是不够的：判定正确但没接进限速，
// 保护模式就只是一个会写 status 的开关 —— 看起来一切正常，实际毫无作用。
func TestDecideScaling_ProtectModeThrottlesScaleUp(t *testing.T) {
	pool := newPool(0, 200)
	pool.Spec.Scaling.MaxProvisionPerSecond = 100

	o := scaleObs()
	o.ClaimRatePerSecond = 1000 // 需求远超上限，确保撞到限速
	o.NodeHeadroom = -1

	base := DecideScaling(pool, o)
	if base.Delta != 100 {
		t.Fatalf("未保护时扩容应被限速到 100，实际 %d", base.Delta)
	}

	o.ProtectActive = true
	got := DecideScaling(pool, o)

	if got.Delta != 50 {
		t.Fatalf("保护模式下扩容应降至 50，实际 %d", got.Delta)
	}
	if got.Reason != ScaleReasonProtectMode {
		t.Errorf("原因码应为 %q，实际 %q", ScaleReasonProtectMode, got.Reason)
	}
	if got.Action != ScaleUp {
		t.Errorf("仍应是扩容动作（限速不是停止），实际 %v", got.Action)
	}
	if got.Suppressed {
		t.Error("限速不是抑制：扩容仍然发生了，只是幅度更小")
	}
}

// TestDecideScaling_ProtectModeDoesNotThrottleScaleDown 守住"只压扩容方向"。
//
// 保护期恰恰是应当允许回收资源的时候；把缩容也压住会让危机中的
// 资源释放变慢，方向与保护的目的相反。
func TestDecideScaling_ProtectModeDoesNotThrottleScaleDown(t *testing.T) {
	pool := newPool(0, 200)
	o := scaleObs()
	o.Warm = 100 // 远超目标 → 缩容
	o.ProtectActive = true

	got := DecideScaling(pool, o)
	if got.Action != ScaleDown {
		t.Fatalf("应缩容，实际 %v（reason=%s）", got.Action, got.Reason)
	}
	if got.Delta == 0 {
		t.Fatal("保护模式不得把缩容压到 0")
	}
}

// TestProtectDefaults_MatchCRDMarkers 守护"代码默认值"与"CRD 默认值"不漂移。
// 两处不一致的表现是"单测通过但线上行为不同"，几乎无法从日志里看出来。
func TestProtectDefaults_MatchCRDMarkers(t *testing.T) {
	spec := sandboxv1alpha1.ProtectModeSpec{}
	cfg := effectiveProtectConfig(spec)

	if !cfg.enabled {
		t.Error("未配置时必须默认开启保护模式（+kubebuilder:default=true）")
	}
	if cfg.failureRatePercent != 30 || DefaultProtectFailureRatePercent != 30 {
		t.Errorf("FailureRatePercent 默认值应为 30，实际 %d / 常量 %d",
			cfg.failureRatePercent, DefaultProtectFailureRatePercent)
	}
	if cfg.minSamples != 10 || DefaultProtectMinSamples != 10 {
		t.Errorf("MinSamples 默认值应为 10，实际 %d / 常量 %d", cfg.minSamples, DefaultProtectMinSamples)
	}
	if cfg.latencyThresholdMs != 2000 || DefaultProtectLatencyThresholdMs != 2000 {
		t.Errorf("ApiserverLatencyThresholdMs 默认值应为 2000，实际 %d / 常量 %d",
			cfg.latencyThresholdMs, DefaultProtectLatencyThresholdMs)
	}
	if cfg.hold != 120*time.Second || DefaultProtectHoldSeconds != 120 {
		t.Errorf("HoldSeconds 默认值应为 120s，实际 %s / 常量 %d", cfg.hold, DefaultProtectHoldSeconds)
	}
	if cfg.scaleUpThrottlePermille != 500 || DefaultProtectScaleUpThrottle != 500 {
		t.Errorf("ScaleUpThrottlePermille 默认值应为 500，实际 %d / 常量 %d",
			cfg.scaleUpThrottlePermille, DefaultProtectScaleUpThrottle)
	}
	if cfg.maxQueueDepth != 200 || DefaultProtectMaxQueueDepth != 200 {
		t.Errorf("MaxQueueDepth 默认值应为 200，实际 %d / 常量 %d",
			cfg.maxQueueDepth, DefaultProtectMaxQueueDepth)
	}
	if got := EffectiveProtectThrottlePermille(spec); got != 500 {
		t.Errorf("EffectiveProtectThrottlePermille 未配置时应为 500，实际 %d", got)
	}
}
