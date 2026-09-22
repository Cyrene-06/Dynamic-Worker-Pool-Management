package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
	"github.com/Cyrene-06/Dynamic-Worker-Pool-Management/internal/isolation"
)

// TestBuiltinTiers_AlignedToGranularity 守护 docs/07 §3.2 的规则 R1。
//
// 这条不变式很脆弱：只要有人把内存写成 160Mi（而不是 192Mi），
// 或把 CPU 写成 200m（而不是 250m），碎片率就会系统性上升 ——
// 而症状只是"节点总是装不满"，极难定位到具体是哪个数值写错了。
func TestBuiltinTiers_AlignedToGranularity(t *testing.T) {
	for name, spec := range builtinTiers {
		cpuReq := mustMilli(t, spec.CPURequest)
		cpuLim := mustMilli(t, spec.CPULimit)
		memReq := mustMiB(t, spec.MemRequest)
		memLim := mustMiB(t, spec.MemLimit)

		if cpuReq%isolation.GranularityCPUMilli != 0 {
			t.Errorf("档位 %s 的 cpuRequest=%s 不是粒度 %dm 的整数倍",
				name, spec.CPURequest, isolation.GranularityCPUMilli)
		}
		if memReq%isolation.GranularityMemMiB != 0 {
			t.Errorf("档位 %s 的 memRequest=%s 不是粒度 %dMi 的整数倍",
				name, spec.MemRequest, isolation.GranularityMemMiB)
		}
		// limits 也参与装箱（Kata 下 limits.memory 就是 guest RAM 规格），
		// 因此同样必须对齐。
		if memLim%isolation.GranularityMemMiB != 0 {
			t.Errorf("档位 %s 的 memLimit=%s 不是粒度 %dMi 的整数倍",
				name, spec.MemLimit, isolation.GranularityMemMiB)
		}
		if cpuLim%isolation.GranularityCPUMilli != 0 {
			t.Errorf("档位 %s 的 cpuLimit=%s 不是粒度 %dm 的整数倍",
				name, spec.CPULimit, isolation.GranularityCPUMilli)
		}
	}
}

// TestBuiltinTiers_OvercommitWithinHardLimits 守护超卖上限。
//
// 内存超卖上限必须硬性守住：越过它的后果是 OOM Killer 可能杀掉
// 其他租户的沙箱 —— 那是安全事故，不是性能问题。
func TestBuiltinTiers_OvercommitWithinHardLimits(t *testing.T) {
	const (
		maxCPURatio = 4.0
		maxMemRatio = 1.6
	)
	for name, spec := range builtinTiers {
		cpuReq := mustMilli(t, spec.CPURequest)
		cpuLim := mustMilli(t, spec.CPULimit)
		memReq := mustMiB(t, spec.MemRequest)
		memLim := mustMiB(t, spec.MemLimit)

		if cpuReq <= 0 || memReq <= 0 {
			t.Errorf("档位 %s 的 request 必须为正数（零值会让 Pod 退化为 BestEffort）", name)
			continue
		}
		if ratio := float64(cpuLim) / float64(cpuReq); ratio > maxCPURatio {
			t.Errorf("档位 %s 的 CPU 超卖比 %.2f 超过上限 %.1f", name, ratio, maxCPURatio)
		}
		if ratio := float64(memLim) / float64(memReq); ratio > maxMemRatio {
			t.Errorf("档位 %s 的内存超卖比 %.2f 超过上限 %.1f（内存不可压缩，超限会导致跨租户 OOM）",
				name, ratio, maxMemRatio)
		}
	}
}

// TestTierResources_UnknownTierFallsBackToSmall 确认拼写错误不会让沙箱
// 退化成零资源（零 requests = BestEffort = 节点压力下最先被驱逐）。
func TestTierResources_UnknownTierFallsBackToSmall(t *testing.T) {
	got := TierResources("tinyy")
	want := TierResources(sandboxv1alpha1.TierSmall)

	if got.Requests.Cpu().MilliValue() != want.Requests.Cpu().MilliValue() {
		t.Errorf("未知档位应回落到 small，实际 cpu=%v", got.Requests.Cpu())
	}
	if got.Requests.Memory().Value() != want.Requests.Memory().Value() {
		t.Errorf("未知档位应回落到 small，实际 mem=%v", got.Requests.Memory())
	}
	if got.Requests.Cpu().MilliValue() == 0 {
		t.Error("未知档位不得回落到零资源（会退化成 BestEffort）")
	}
}

func TestKnownTiers_CoversBuiltinTable(t *testing.T) {
	known := map[string]bool{}
	for _, n := range KnownTiers() {
		known[n] = true
	}
	for name := range builtinTiers {
		if !known[name] {
			t.Errorf("档位 %s 在 builtinTiers 里但没有出现在 KnownTiers() 中，校验会漏掉它", name)
		}
	}
	for _, n := range KnownTiers() {
		if _, ok := builtinTiers[n]; !ok {
			t.Errorf("KnownTiers() 列出了 %s，但 builtinTiers 里没有", n)
		}
	}
}

// ---- 数量解析小工具 ----

// 用 MustParse 而非返回 error：这些字符串是包内常量，
// 解析失败属于编码错误，让测试直接失败比传递错误更有用。
//
// 注意先赋给变量再调 MilliValue/Value：这两个方法是指针接收者，
// 直接在不可寻址的返回值上调用会被 vet 拒绝（而 go build 是过的，
// 所以只有跑 vet 才能发现 —— 这也是把 vet 放进 verify 的原因）。
func mustMilli(t *testing.T, s string) int64 {
	t.Helper()
	q := resource.MustParse(s)
	return q.MilliValue()
}

func mustMiB(t *testing.T, s string) int64 {
	t.Helper()
	q := resource.MustParse(s)
	return q.Value() / (1024 * 1024)
}
