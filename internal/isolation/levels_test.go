package isolation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
)

// ---------------------------------------------------------------------------
// 测试替身
// ---------------------------------------------------------------------------

// countingProbe 统计探测次数，用于验证缓存生效（否则每次 reconcile 都打 API Server）。
type countingProbe struct {
	answers map[string]bool
	calls   int
}

func (p *countingProbe) Exists(_ context.Context, name string) (bool, error) {
	p.calls++
	return p.answers[name], nil
}

// errProbe 模拟 API Server 抖动。
type errProbe struct{}

func (errProbe) Exists(context.Context, string) (bool, error) {
	return false, errors.New("apiserver unavailable")
}

// ---------------------------------------------------------------------------
// 配置校验
// ---------------------------------------------------------------------------

func TestDefaultConfig_IsValidAndAligned(t *testing.T) {
	cfg := DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("内置兜底配置必须自洽，否则控制器在缺配置时无法启动: %v", err)
	}
	// 单独断言粒度对齐：这是 docs/07 §3.2 规则 R1，破坏它会系统性推高碎片率。
	for name, spec := range cfg.Levels {
		if !spec.Overhead.IsAlignedToGranularity() {
			t.Errorf("级别 %s 的 overhead %+v 与粒度 (cpu=%dm/mem=%dMi) 不对齐",
				name, spec.Overhead, GranularityCPUMilli, GranularityMemMiB)
		}
	}
}

func TestParseConfig_Valid(t *testing.T) {
	data := []byte(`
levels:
  simulated:
    runtimeClassName: ""
    overhead: {cpuMilli: 0, memMiB: 0}
  kata-fc:
    runtimeClassName: kata-fc
    requiresKvm: true
    allowOvercommit: true
    overhead: {cpuMilli: 300, memMiB: 192}
    fallbacks: [runc]
  runc:
    runtimeClassName: ""
    allowOvercommit: true
    overhead: {cpuMilli: 0, memMiB: 0}
`)
	cfg, err := ParseConfig(data)
	if err != nil {
		t.Fatalf("解析合法配置不应报错: %v", err)
	}
	if got := len(cfg.Levels); got != 3 {
		t.Fatalf("级别数 = %d, 期望 3", got)
	}
	if spec, ok := cfg.Level(KataFC); !ok || spec.RuntimeClassName != "kata-fc" || !spec.RequiresKVM {
		t.Fatalf("kata-fc 解析结果不符: %+v (ok=%v)", spec, ok)
	}
}

func TestParseConfig_RejectsBadConfigs(t *testing.T) {
	cases := []struct {
		name      string
		yaml      string
		wantInMsg string
	}{
		{
			name: "空配置",
			yaml: `levels: {}`,
			// ParseConfig 对空 levels 直接拒绝，避免"配置挂了但看起来正常"。
			wantInMsg: "没有任何隔离级别",
		},
		{
			// 关键用例：kata-fc 不写 runtimeClassName 会被静默当成 runc 用，
			// 隔离强度无声消失 —— 这是最危险的一类配置错误。
			name: "VM 级隔离级别缺 runtimeClassName",
			yaml: `
levels:
  kata-fc:
    runtimeClassName: ""
    overhead: {cpuMilli: 0, memMiB: 0}
`,
			wantInMsg: "必须指定 runtimeClassName",
		},
		{
			// 160Mi 不是 64Mi 的整数倍（2.5 倍），会在装箱时留下无法使用的残留。
			name: "开销未与档位粒度对齐",
			yaml: `
levels:
  kata-fc:
    runtimeClassName: kata-fc
    overhead: {cpuMilli: 250, memMiB: 160}
`,
			wantInMsg: "不是粒度",
		},
		{
			name: "降级候选未定义",
			yaml: `
levels:
  kata-fc:
    runtimeClassName: kata-fc
    overhead: {cpuMilli: 0, memMiB: 0}
    fallbacks: [nonexistent]
`,
			wantInMsg: "未定义",
		},
		{
			// 有环会导致 Resolve 的候选链不终止。
			name: "降级链成环",
			yaml: `
levels:
  a:
    runtimeClassName: a
    overhead: {cpuMilli: 0, memMiB: 0}
    fallbacks: [b]
  b:
    runtimeClassName: b
    overhead: {cpuMilli: 0, memMiB: 0}
    fallbacks: [a]
`,
			wantInMsg: "环",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.yaml))
			if err == nil {
				t.Fatal("期望报错，实际通过")
			}
			if !strings.Contains(err.Error(), tc.wantInMsg) {
				t.Fatalf("错误信息 %q 未包含 %q", err.Error(), tc.wantInMsg)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 级别解析与降级
// ---------------------------------------------------------------------------

func TestResolve(t *testing.T) {
	cfg := DefaultConfig()

	cases := []struct {
		name         string
		policy       sandboxv1alpha1.DegradationPolicy
		probe        map[string]bool
		want         sandboxv1alpha1.IsolationLevel
		wantLevel    Level
		wantAvail    bool
		wantDegraded bool
		wantReason   Reason
	}{
		{
			name:       "FailFast 且运行时存在 → 按请求落地",
			policy:     sandboxv1alpha1.DegradationFailFast,
			probe:      map[string]bool{"kata-fc": true},
			want:       sandboxv1alpha1.IsolationKataFC,
			wantLevel:  KataFC,
			wantAvail:  true,
			wantReason: ReasonOK,
		},
		{
			name:       "FailFast 且运行时缺失 → 拒绝，不降级",
			policy:     sandboxv1alpha1.DegradationFailFast,
			probe:      map[string]bool{},
			want:       sandboxv1alpha1.IsolationKataFC,
			wantAvail:  false,
			wantReason: ReasonRuntimeClassMissing,
		},
		{
			name:         "FallbackToRunc 且首选缺失、次选可用 → 降级到 kata-clh",
			policy:       sandboxv1alpha1.DegradationFallbackToRunc,
			probe:        map[string]bool{"kata-clh": true},
			want:         sandboxv1alpha1.IsolationKataFC,
			wantLevel:    KataCLH,
			wantAvail:    true,
			wantDegraded: true,
			wantReason:   ReasonFallbackApplied,
		},
		{
			// 最后兜底 runc 的 runtimeClassName 为空，无需探测即可用。
			name:         "FallbackToRunc 且 VM 运行时全缺 → 兜底到 runc",
			policy:       sandboxv1alpha1.DegradationFallbackToRunc,
			probe:        map[string]bool{},
			want:         sandboxv1alpha1.IsolationKataFC,
			wantLevel:    Runc,
			wantAvail:    true,
			wantDegraded: true,
			wantReason:   ReasonFallbackApplied,
		},
		{
			// PauseScaling 的语义是"保留存量、暂停增量"，因此不能降级 ——
			// 降级会让新库存落到错误的隔离级别，与意图冲突。
			name:       "PauseScaling 且运行时缺失 → 如实上报不可用",
			policy:     sandboxv1alpha1.DegradationPauseScaling,
			probe:      map[string]bool{"kata-clh": true},
			want:       sandboxv1alpha1.IsolationKataFC,
			wantAvail:  false,
			wantReason: ReasonRuntimeClassMissing,
		},
		{
			name:       "simulated → 用集群默认运行时，无隔离保证但可用",
			policy:     sandboxv1alpha1.DegradationFailFast,
			probe:      map[string]bool{},
			want:       sandboxv1alpha1.IsolationSimulated,
			wantLevel:  Simulated,
			wantAvail:  true,
			wantReason: ReasonDefaultRuntime,
		},
		{
			name:       "未知级别 → 明确拒绝而非静默回落",
			policy:     sandboxv1alpha1.DegradationFailFast,
			probe:      map[string]bool{"whatever": true},
			want:       sandboxv1alpha1.IsolationLevel("bogus"),
			wantAvail:  false,
			wantReason: ReasonUnknownLevel,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewResolver(cfg, StaticProbe(tc.probe), Options{Policy: tc.policy})
			av, err := r.Resolve(context.Background(), tc.want)
			if err != nil {
				t.Fatalf("Resolve 报错: %v", err)
			}
			if av.Available != tc.wantAvail {
				t.Fatalf("Available = %v, 期望 %v (reason=%s msg=%s)",
					av.Available, tc.wantAvail, av.Reason, av.Message)
			}
			if av.Degraded != tc.wantDegraded {
				t.Fatalf("Degraded = %v, 期望 %v", av.Degraded, tc.wantDegraded)
			}
			if tc.wantLevel != "" && av.Level != tc.wantLevel {
				t.Fatalf("Level = %v, 期望 %v", av.Level, tc.wantLevel)
			}
			if av.Reason != tc.wantReason {
				t.Fatalf("Reason = %v, 期望 %v", av.Reason, tc.wantReason)
			}
			if av.RequestedLevel != Level(tc.want) {
				t.Fatalf("RequestedLevel = %v, 期望 %v", av.RequestedLevel, tc.want)
			}
		})
	}
}

// TestResolve_DegradedMustBeObservable 守护一条安全不变式：
// 只要生效级别 ≠ 请求级别，Degraded 与 Message 就必须被填上。
// 静默降级是本项目最危险的一类故障 —— 沙箱照常跑，隔离已经没了。
func TestResolve_DegradedMustBeObservable(t *testing.T) {
	r := NewResolver(DefaultConfig(), StaticProbe{}, Options{
		Policy: sandboxv1alpha1.DegradationFallbackToRunc,
	})
	av, err := r.Resolve(context.Background(), sandboxv1alpha1.IsolationKataFC)
	if err != nil {
		t.Fatalf("Resolve 报错: %v", err)
	}
	if av.Level == av.RequestedLevel {
		t.Skip("本次未发生降级，用例前提不成立")
	}
	if !av.Degraded {
		t.Error("发生了降级但 Degraded=false，会导致上层的告警与条件全部失效")
	}
	if av.Message == "" {
		t.Error("发生了降级但 Message 为空，运维无法从事件里看出发生了什么")
	}
}

// TestResolve_ProbeErrorIsNotUnavailable 是错误语义的关键用例：
// 探测失败绝不能等同于"运行时不存在"。
// 否则一次 API Server 抖动就会触发大面积降级 —— 那是最糟的故障放大方式。
func TestResolve_ProbeErrorIsNotUnavailable(t *testing.T) {
	r := NewResolver(DefaultConfig(), errProbe{}, Options{Policy: sandboxv1alpha1.DegradationFallbackToRunc})
	av, err := r.Resolve(context.Background(), sandboxv1alpha1.IsolationKataFC)
	if err == nil {
		t.Fatalf("探测失败必须返回错误，而不是返回不可用；实际返回 %+v", av)
	}
	if av.Available || av.Degraded {
		t.Fatalf("探测失败时不应产生任何可用性结论: %+v", av)
	}
}

func TestResolver_CachesProbeResults(t *testing.T) {
	p := &countingProbe{answers: map[string]bool{"kata-fc": true}}
	r := NewResolver(DefaultConfig(), p, Options{
		Policy:   sandboxv1alpha1.DegradationFailFast,
		CacheTTL: time.Minute,
	})
	for i := 0; i < 20; i++ {
		if _, err := r.Resolve(context.Background(), sandboxv1alpha1.IsolationKataFC); err != nil {
			t.Fatalf("Resolve 报错: %v", err)
		}
	}
	if p.calls != 1 {
		t.Fatalf("探测次数 = %d, 期望 1（缓存应生效，否则每个 reconcile 都在打 API Server）", p.calls)
	}

	// 配置变更后必须重新探测，否则"kata 刚装好"会迟迟不生效。
	r.Invalidate()
	if _, err := r.Resolve(context.Background(), sandboxv1alpha1.IsolationKataFC); err != nil {
		t.Fatalf("Resolve 报错: %v", err)
	}
	if p.calls != 2 {
		t.Fatalf("Invalidate 后探测次数 = %d, 期望 2", p.calls)
	}
}

func TestResolver_CacheExpires(t *testing.T) {
	now := time.Unix(0, 0)
	p := &countingProbe{answers: map[string]bool{"kata-fc": true}}
	r := NewResolver(DefaultConfig(), p, Options{
		Policy:   sandboxv1alpha1.DegradationFailFast,
		CacheTTL: 30 * time.Second,
		Now:      func() time.Time { return now },
	})
	if _, err := r.Resolve(context.Background(), sandboxv1alpha1.IsolationKataFC); err != nil {
		t.Fatal(err)
	}
	now = now.Add(31 * time.Second)
	if _, err := r.Resolve(context.Background(), sandboxv1alpha1.IsolationKataFC); err != nil {
		t.Fatal(err)
	}
	if p.calls != 2 {
		t.Fatalf("缓存过期后探测次数 = %d, 期望 2", p.calls)
	}
}

func TestResolver_Snapshot(t *testing.T) {
	r := NewResolver(DefaultConfig(), StaticProbe{"kata-fc": true}, Options{
		Policy: sandboxv1alpha1.DegradationFailFast,
	})
	snap, err := r.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot 报错: %v", err)
	}
	// 关键：快照要能回答"当前集群实际提供哪些隔离"，
	// 而不是"我配置了哪些" —— 这是运维最需要一眼看到的信息。
	if av, ok := snap[KataFC]; !ok || !av.Available {
		t.Errorf("kata-fc 应可用: %+v", av)
	}
	if av, ok := snap[KataCLH]; !ok || av.Available {
		t.Errorf("kata-clh 未安装，应不可用: %+v", av)
	}
	if av := snap[Simulated]; !av.Available || av.Reason != ReasonDefaultRuntime {
		t.Errorf("simulated 应始终可用且标注为默认运行时: %+v", av)
	}
}

func TestNewResolver_NilConfigFallsBackToDefaults(t *testing.T) {
	// ConfigMap 没挂上时控制器必须还能起来并给出可诊断的状态，
	// 而不是 CrashLoop —— 那会让一次配置遗漏升级成线上事故。
	r := NewResolver(nil, nil, Options{})
	if len(r.Config().Levels) == 0 {
		t.Fatal("nil 配置应回落到内置默认配置")
	}
	av, err := r.Resolve(context.Background(), sandboxv1alpha1.IsolationRunc)
	if err != nil || !av.Available {
		t.Fatalf("默认配置下 runc 应可用: %+v, err=%v", av, err)
	}
}
