package controller

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
)

// TierSpec 是一个资源档位的数值定义。
type TierSpec struct {
	CPURequest string
	CPULimit   string
	MemRequest string
	MemLimit   string
	// MaxPerNode 是该档位下的单节点密度上限。
	// 实际生效值取它与 IP 池容量、PID 上限、inotify 上限的最小值（docs/06 §8.2）。
	MaxPerNode int32
}

// builtinTiers 是内置档位表，数值与 docs/04 §2 的 tiers ConfigMap 一致。
//
// 两条由测试守着的不变式（tiers_test.go）：
//
//  1. **所有数值都是粒度（CPU 50m / 内存 64Mi）的整数倍。**
//     192Mi 满足（3×64），160Mi 不满足（2.5×64）—— 后者会在装箱时留下
//     无法被任何档位使用的残留，直接推高碎片率。
//
//  2. **超卖比在硬上限内**：CPU ≤ 4.0，内存 ≤ 1.6。
//     理由是不对称的：CPU 是可压缩资源，用超了只是变慢；
//     内存不可压缩，用超了是 OOM Killer 动手，可能杀掉**其他租户**的无辜沙箱 ——
//     那是安全事故，不只是性能问题（docs/07 §4.3）。
//
// 关于 requests < limits：在 Kata 路径下 limits.memory 决定 guest RAM 大小，
// requests.memory 只参与调度。所以把 requests 设小**就是在做内存超卖**，
// 而是否真的节省宿主内存，取决于 Firecracker 是否开启预分配/大页
// —— 预分配换低延迟但零超卖收益（docs/07 §4.2）。
var builtinTiers = map[string]TierSpec{
	sandboxv1alpha1.TierTiny: {
		CPURequest: "100m", CPULimit: "400m", MemRequest: "128Mi", MemLimit: "192Mi", MaxPerNode: 60,
	},
	sandboxv1alpha1.TierSmall: {
		CPURequest: "250m", CPULimit: "1", MemRequest: "512Mi", MemLimit: "768Mi", MaxPerNode: 40,
	},
	sandboxv1alpha1.TierMedium: {
		CPURequest: "1", CPULimit: "2", MemRequest: "2Gi", MemLimit: "3Gi", MaxPerNode: 16,
	},
	sandboxv1alpha1.TierLarge: {
		CPURequest: "4", CPULimit: "6", MemRequest: "8Gi", MemLimit: "10Gi", MaxPerNode: 4,
	},
}

// TierResources 返回某档位的资源声明。
//
// 未知档位回落到 small 而不是"零资源"：零资源会被调度器当成 BestEffort，
// 在节点压力下最先被驱逐 —— 一个拼写错误不应让沙箱变成最脆弱的那个。
func TierResources(tier string) corev1.ResourceRequirements {
	spec, ok := builtinTiers[tier]
	if !ok {
		spec = builtinTiers[sandboxv1alpha1.TierSmall]
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(spec.CPURequest),
			corev1.ResourceMemory: resource.MustParse(spec.MemRequest),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(spec.CPULimit),
			corev1.ResourceMemory: resource.MustParse(spec.MemLimit),
		},
	}
}

// TierMaxPerNode 返回某档位的单节点密度上限。
func TierMaxPerNode(tier string) int32 {
	if spec, ok := builtinTiers[tier]; ok {
		return spec.MaxPerNode
	}
	return builtinTiers[sandboxv1alpha1.TierSmall].MaxPerNode
}

// KnownTiers 返回全部已知档位名，供校验与错误信息使用。
func KnownTiers() []string {
	return []string{
		sandboxv1alpha1.TierTiny,
		sandboxv1alpha1.TierSmall,
		sandboxv1alpha1.TierMedium,
		sandboxv1alpha1.TierLarge,
	}
}
