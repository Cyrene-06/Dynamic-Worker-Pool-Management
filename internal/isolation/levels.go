// Package isolation 是"隔离级别 → 具体运行时"的唯一映射层。
//
// # 为什么需要这一层
//
// 本项目必须同时支持本地 kind 与生产 metal 集群（docs/02 D11），而两者能提供的
// 运行时截然不同：kind 上无法运行 Kata，生产上又必须用 Kata 才有隔离保证。
// 如果把 RuntimeClass 名称写死在控制器逻辑里，就会出现两难：
// 要么本地完全无法验证状态机，要么为本地加一堆 "if env == dev" 的分支。
//
// 把映射收敛到这一层之后：
//
//   - CRD 与控制器只认 IsolationLevel（simulated / runc / kata-fc / kata-clh）
//   - RuntimeClass 名称、每 Pod 开销、是否允许超卖、节点选择器都由配置决定
//   - "kata-fc 不可用时怎么办"这类降级决策也只在这里发生
//
// 结果：隔离相关的验证集中在运行时一致性套件里，而状态机、池化、回收等
// 全部控制面逻辑可以在无 KVM 的机器上 100% 被测试覆盖。
package isolation

import (
	"errors"
	"fmt"
	"sort"

	"sigs.k8s.io/yaml"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
)

// 配置来源的约定。与 docs/06 §1 的 ConfigMap 一致。
const (
	ConfigMapName      = "sandbox-isolation"
	ConfigMapNamespace = "sandbox-system"
	ConfigMapKey       = "levels.yaml"
)

// Level 是隔离级别标识。
//
// 这里用**类型别名**而不是独立类型：独立类型会引入两套常量与大量来回转换，
// 而转换点正是“CRD 里写 kata-fc、配置里写 kataFC”这类静默不匹配 bug 的温床。
// 别名让 CRD 与本层共享同一个类型，从根上消除这类不一致。
type Level = sandboxv1alpha1.IsolationLevel

const (
	// Simulated 仅用于本地开发：回落到集群默认运行时，目的是让控制面逻辑可被验证。
	Simulated = sandboxv1alpha1.IsolationSimulated
	// Runc 用于可信负载与本地开发回退。
	Runc = sandboxv1alpha1.IsolationRunc
	// KataFC 是主力隔离方案：Kata + Firecracker（VM 级隔离 + 亚秒启动）。
	KataFC = sandboxv1alpha1.IsolationKataFC
	// KataCLH 是需要更多设备能力时的折中：Kata + Cloud Hypervisor。
	KataCLH = sandboxv1alpha1.IsolationKataCLH
)

// 资源档位的最小粒度。
//
// docs/07 §3.2 的规则 R1：所有档位 requests、RuntimeClass 每 Pod 开销、
// 节点可分配资源都取同一粒度的整数倍，则 CPU/内存维度上碎片率为 0。
//
// 为什么 CPU 粒度取 50m 而不是 100m：250m 是一个很自然的档位，
// 但它不是 100m 的整数倍。若取 100m，规则 R1 会在真实档位表上直接失效 ——
// 这正是“粒度拍脑袋定”的典型后果。50m 能同时整除 100m / 250m / 1 / 4 与 300m 开销。
const (
	GranularityCPUMilli int64 = 50
	GranularityMemMiB   int64 = 64
)

// Overhead 是每 Pod 的固定开销（对应 RuntimeClass.spec.overhead）。
//
// Kata 路径下这个值不可忽略：VMM 进程 + guest 内核 + virtiofsd 合计约 50–120 MiB，
// 若不显式计入，调度器会过度装箱并导致节点 OOM（docs/06 §8.1）。
type Overhead struct {
	CPUMilli int64 `json:"cpuMilli"`
	MemMiB   int64 `json:"memMiB"`
}

// IsAlignedToGranularity 判断开销是否与档位粒度对齐。
func (o Overhead) IsAlignedToGranularity() bool {
	return o.CPUMilli%GranularityCPUMilli == 0 && o.MemMiB%GranularityMemMiB == 0
}

// Spec 是一个隔离级别的全部平台侧属性。
type Spec struct {
	// RuntimeClassName 为空表示使用集群默认运行时（runc）。
	// simulated 与 runc 都为空；kata-fc / kata-clh 必须非空。
	RuntimeClassName string `json:"runtimeClassName"`

	// RequiresKVM 标记该级别是否依赖节点 /dev/kvm。
	// 用于启动自检与状态上报：在无 KVM 的节点上把 kata-fc 报成可用是危险的静默故障。
	RequiresKVM bool `json:"requiresKvm"`

	// AllowOvercommit 决定该级别是否参与 CPU/内存超卖（docs/07 §4.3）。
	// 默认关闭：本地与调试环境不需要超卖，打开只会让排查变难。
	AllowOvercommit bool `json:"allowOvercommit"`

	// Overhead 是每 Pod 固定开销，必须与粒度对齐。
	Overhead Overhead `json:"overhead"`

	// NodeSelector 把 Pod 约束到对应节点池。
	// 即使有人绕过 SandboxPool 直接建 Pod，也不会落到错误的节点池上。
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Fallbacks 是降级候选，按顺序尝试。
	//
	// 典型配置：kata-fc → [kata-clh, runc]。注意降级会削弱隔离强度，
	// 因此只在策略允许时生效，且必须把结果暴露到 status（绝不能静默降级）。
	Fallbacks []Level `json:"fallbacks,omitempty"`
}

// Config 是全部隔离级别的配置。
type Config struct {
	Levels map[Level]Spec `json:"levels"`
}

// ParseConfig 解析 ConfigMap 中的 levels.yaml。
//
// 解析后必定执行 Validate，因此调用方拿到的 Config 一定是自洽的
// （引用存在、无循环降级、开销对齐粒度）。
func ParseConfig(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", ConfigMapKey, err)
	}
	if len(cfg.Levels) == 0 {
		return nil, errors.New("配置中没有任何隔离级别")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate 校验配置自洽性。这些检查在启动时做一次，比运行时踩坑便宜得多。
func (c *Config) Validate() error {
	for name, spec := range c.Levels {
		// 1. 默认运行时（runc / simulated）之外，必须有明确的 RuntimeClass。
		//    少了这条，kata-fc 会被静默地当成 runc 用 —— 隔离强度无声消失。
		if spec.RuntimeClassName == "" && name != Simulated && name != Runc {
			return fmt.Errorf("级别 %q 必须指定 runtimeClassName（否则会静默退回默认运行时）", name)
		}

		// 2. 开销必须与粒度对齐，否则装箱会系统性留下无法使用的残留。
		if !spec.Overhead.IsAlignedToGranularity() {
			return fmt.Errorf(
				"级别 %q 的 overhead (cpu=%dm, mem=%dMi) 不是粒度 (cpu=%dm, mem=%dMi) 的整数倍，会推高调度碎片率",
				name, spec.Overhead.CPUMilli, spec.Overhead.MemMiB,
				GranularityCPUMilli, GranularityMemMiB)
		}

		// 3. 降级候选必须已定义。
		for _, fb := range spec.Fallbacks {
			if _, ok := c.Levels[fb]; !ok {
				return fmt.Errorf("级别 %q 的降级候选 %q 未定义", name, fb)
			}
		}
	}

	// 4. 降级链不能有环，否则 Resolve 可能不终止。
	for name := range c.Levels {
		if err := c.checkNoCycle(name, name, map[Level]bool{}); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) checkNoCycle(origin, cur Level, seen map[Level]bool) error {
	if seen[cur] {
		return fmt.Errorf("隔离级别降级链存在环: %s", origin)
	}
	seen[cur] = true
	for _, fb := range c.Levels[cur].Fallbacks {
		if fb == origin {
			return fmt.Errorf("隔离级别 %q 的降级链存在环（回到自身）", origin)
		}
		if err := c.checkNoCycle(origin, fb, seen); err != nil {
			return err
		}
	}
	delete(seen, cur)
	return nil
}

// Level 返回某个级别的配置与是否存在。
func (c *Config) Level(name Level) (Spec, bool) {
	spec, ok := c.Levels[name]
	return spec, ok
}

// LevelNames 返回排序后的级别名，用于日志与状态上报（保证输出稳定可比对）。
func (c *Config) LevelNames() []string {
	out := make([]string, 0, len(c.Levels))
	for name := range c.Levels {
		out = append(out, string(name))
	}
	sort.Strings(out)
	return out
}

// DefaultConfig 是内置的兜底配置，对应 docs/06 §1 的示例。
//
// 存在的理由：配置缺失时控制器仍应能启动并给出可诊断的状态，
// 而不是直接 CrashLoop —— 那会让"ConfigMap 没挂上"变成一次线上事故。
func DefaultConfig() *Config {
	return &Config{
		Levels: map[Level]Spec{
			Simulated: {
				RuntimeClassName: "",
				RequiresKVM:      false,
				AllowOvercommit:  false,
				Overhead:         Overhead{CPUMilli: 0, MemMiB: 0},
			},
			Runc: {
				RuntimeClassName: "",
				RequiresKVM:      false,
				AllowOvercommit:  true,
				Overhead:         Overhead{CPUMilli: 0, MemMiB: 0},
			},
			KataFC: {
				RuntimeClassName: "kata-fc",
				RequiresKVM:      true,
				AllowOvercommit:  true,
				// 300m / 192Mi 都是粒度整数倍；注意 160Mi 会破坏对齐（160/64=2.5）。
				Overhead: Overhead{CPUMilli: 300, MemMiB: 192},
				NodeSelector: map[string]string{
					"sandbox.example.com/isolation": "kata-fc",
				},
				Fallbacks: []Level{KataCLH, Runc},
			},
			KataCLH: {
				RuntimeClassName: "kata-clh",
				RequiresKVM:      true,
				AllowOvercommit:  true,
				Overhead:         Overhead{CPUMilli: 300, MemMiB: 256},
				NodeSelector: map[string]string{
					"sandbox.example.com/isolation": "kata-clh",
				},
				Fallbacks: []Level{Runc},
			},
		},
	}
}
