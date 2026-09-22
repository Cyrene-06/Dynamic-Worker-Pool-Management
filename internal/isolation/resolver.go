package isolation

import (
	"context"
	"fmt"
	"sync"
	"time"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
)

// Reason 是可用性判定的原因码。
//
// 必须枚举化：它会进入 Conditions 与指标 label，
// 自由文本会让基数失控（docs/08 §2.1 的红线之一）。
type Reason string

const (
	// ReasonOK 表示请求的级别本身可用。
	ReasonOK Reason = "OK"
	// ReasonDefaultRuntime 表示该级别使用集群默认运行时（runc），无需探测。
	ReasonDefaultRuntime Reason = "DefaultRuntime"
	// ReasonFallbackApplied 表示发生了降级 —— 隔离强度下降，必须告警而非静默接受。
	ReasonFallbackApplied Reason = "FallbackApplied"
	// ReasonRuntimeClassMissing 表示请求的级别不可用。
	ReasonRuntimeClassMissing Reason = "RuntimeClassMissing"
	// ReasonNoFallback 表示请求的级别与全部降级候选都不可用。
	ReasonNoFallback Reason = "NoFallbackAvailable"
	// ReasonUnknownLevel 表示配置里没有这个级别。
	ReasonUnknownLevel Reason = "UnknownLevel"
)

// Availability 是一次隔离级别解析的结果。
type Availability struct {
	// RequestedLevel 是业务请求的级别，用于对比是否发生了降级。
	RequestedLevel Level
	// Level 是最终生效的级别。
	Level Level
	// Spec 是生效级别的配置。
	Spec Spec
	// RuntimeClassName 是要写进 Pod 的运行时类名；空表示使用集群默认运行时。
	RuntimeClassName string
	// Available 为 false 时控制器必须拒绝新建，而不是用错运行时继续跑。
	Available bool
	// Degraded 表示生效级别 ≠ 请求级别。
	//
	// 这个标志必须暴露到 status 与 Conditions 上。静默降级是本项目最危险的
	// 一类故障：沙箱照常运行、指标一切正常，但隔离强度已经消失。
	Degraded bool
	Reason   Reason
	// Message 是人类可读的补充信息，用于事件与条件。
	Message string
}

// RuntimeClassProbe 抽象"集群里有没有这个 RuntimeClass"。
//
// 抽成接口的目的很实际：降级逻辑是本层最容易写错的部分，
// 而用接口替身可以让它在没有集群的机器上被穷举测试。
type RuntimeClassProbe interface {
	Exists(ctx context.Context, name string) (bool, error)
}

// StaticProbe 是固定答案的探针，用于单元测试与 dry-run。
type StaticProbe map[string]bool

// Exists 实现 RuntimeClassProbe。
func (p StaticProbe) Exists(_ context.Context, name string) (bool, error) { return p[name], nil }

// Options 是 Resolver 的可调参数。
type Options struct {
	// Policy 决定运行时不可用时是否允许降级，取值来自 SandboxPool.spec.degradation。
	Policy sandboxv1alpha1.DegradationPolicy
	// CacheTTL 是探测结果缓存时长。
	// 太短会把 API Server 打爆；太长会让"kata 刚装好"迟迟不生效。5 分钟是折中。
	CacheTTL time.Duration
	// Now 可注入，便于测试缓存过期与冷却期。
	Now func() time.Time
}

func (o Options) withDefaults() Options {
	if o.CacheTTL <= 0 {
		o.CacheTTL = 5 * time.Minute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Policy == "" {
		o.Policy = sandboxv1alpha1.DegradationFailFast
	}
	return o
}

// Resolver 把"期望的隔离级别"解析成"当前集群可用的落地规格"。
//
// 它是无状态服务的形态：可以被并发调用，内部用读写锁保护探测缓存。
type Resolver struct {
	mu       sync.RWMutex
	cfg      *Config
	probe    RuntimeClassProbe
	opts     Options
	cache    map[Level]Availability
	cachedAt time.Time
}

// NewResolver 构造 Resolver。cfg 为 nil 时使用内置兜底配置，
// 保证"ConfigMap 没挂上"不会变成一次 CrashLoop。
func NewResolver(cfg *Config, probe RuntimeClassProbe, opts Options) *Resolver {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	if probe == nil {
		probe = StaticProbe{}
	}
	return &Resolver{
		cfg:   cfg,
		probe: probe,
		opts:  opts.withDefaults(),
		cache: map[Level]Availability{},
	}
}

// SetConfig 热替换配置（ConfigMap 变更时调用），并清空缓存。
func (r *Resolver) SetConfig(cfg *Config) {
	if cfg == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg = cfg
	r.cache = map[Level]Availability{}
	r.cachedAt = time.Time{}
}

// SetProbe 替换探针（可用于测试或切换探测实现）。
func (r *Resolver) SetProbe(p RuntimeClassProbe) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.probe = p
	r.cache = map[Level]Availability{}
	r.cachedAt = time.Time{}
}

// Invalidate 清空缓存，强制下次重新探测。
func (r *Resolver) Invalidate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache = map[Level]Availability{}
	r.cachedAt = time.Time{}
}

// Config 返回当前配置（只读快照）。
func (r *Resolver) Config() *Config {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg
}

// Resolve 解析请求的隔离级别。
//
// 返回值语义：
//   - Available=true, Degraded=false → 按请求级别落地
//   - Available=true, Degraded=true  → 降级落地，调用方**必须**把 Degraded 暴露出去
//   - Available=false               → 调用方必须拒绝新建，并按 Pool 的策略处理
//
// 探测本身出错时不返回"不可用"，而是把错误抛给调用方：
// 把"探测失败"当成"运行时不存在"会导致一次 API Server 抖动就触发大面积降级。
func (r *Resolver) Resolve(ctx context.Context, level Level) (Availability, error) {
	r.mu.RLock()
	if av, ok := r.cached(level); ok {
		r.mu.RUnlock()
		return av, nil
	}
	cfg, probe := r.cfg, r.probe
	policy := r.opts.Policy
	r.mu.RUnlock()

	av, err := resolve(ctx, cfg, probe, policy, level)
	if err != nil {
		return Availability{}, err
	}

	r.mu.Lock()
	if r.cachedAt.IsZero() || r.opts.Now().Sub(r.cachedAt) > r.opts.CacheTTL {
		r.cache = map[Level]Availability{}
		r.cachedAt = r.opts.Now()
	}
	r.cache[level] = av
	r.mu.Unlock()

	return av, nil
}

// cached 返回未过期的缓存项。
func (r *Resolver) cached(level Level) (Availability, bool) {
	if r.cachedAt.IsZero() || r.opts.Now().Sub(r.cachedAt) > r.opts.CacheTTL {
		return Availability{}, false
	}
	av, ok := r.cache[level]
	return av, ok
}

// Snapshot 探测并返回全部级别的可用性。
//
// 用途是状态上报：运维最需要一眼看到的不是"我配了什么"，而是
// "当前集群实际能提供什么隔离"。
func (r *Resolver) Snapshot(ctx context.Context) (map[Level]Availability, error) {
	r.mu.RLock()
	cfg, probe := r.cfg, r.probe
	policy := r.opts.Policy
	r.mu.RUnlock()

	out := make(map[Level]Availability, len(cfg.Levels))
	for _, name := range cfg.LevelNames() {
		av, err := resolve(ctx, cfg, probe, policy, Level(name))
		if err != nil {
			return nil, err
		}
		out[Level(name)] = av
	}
	return out, nil
}

// DegradationFor 返回该池在"请求级别不可用"时应采取的动作。
//
// 注意它与 candidates() 的分工：candidates 决定"能不能换级别跑"，
// 这里决定"换不了的时候平台怎么办" —— 两者都取决于策略，但后果不同。
func DegradationFor(policy sandboxv1alpha1.DegradationPolicy) sandboxv1alpha1.DegradationPolicy {
	switch policy {
	case sandboxv1alpha1.DegradationFallbackToRunc, sandboxv1alpha1.DegradationPauseScaling:
		return policy
	default:
		return sandboxv1alpha1.DegradationFailFast
	}
}

// ---------------------------------------------------------------------------
// 纯函数实现：不带锁、不带缓存，便于穷举测试
// ---------------------------------------------------------------------------

func resolve(
	ctx context.Context,
	cfg *Config,
	probe RuntimeClassProbe,
	policy sandboxv1alpha1.DegradationPolicy,
	want Level,
) (Availability, error) {
	wantSpec, ok := cfg.Level(want)
	if !ok {
		return Availability{
			RequestedLevel: want,
			Level:          want,
			Available:      false,
			Reason:         ReasonUnknownLevel,
			Message:        fmt.Sprintf("配置中不存在隔离级别 %q，可用级别: %v", want, cfg.LevelNames()),
		}, nil
	}

	var missing []RuntimeClassRef
	for _, cand := range candidates(cfg, want, wantSpec, policy) {
		spec := cfg.Levels[cand]
		exists, err := probeLevel(ctx, probe, spec)
		if err != nil {
			return Availability{}, fmt.Errorf("探测隔离级别 %q 的运行时失败: %w", cand, err)
		}
		if !exists {
			missing = append(missing, RuntimeClassRef{Level: cand, RuntimeClassName: spec.RuntimeClassName})
			continue
		}

		out := Availability{
			RequestedLevel:   want,
			Level:            cand,
			Spec:             spec,
			RuntimeClassName: spec.RuntimeClassName,
			Available:        true,
			Reason:           ReasonOK,
		}

		// 注意这里不能写成互斥的 switch。最初的实现用 switch 分派，结果
		// "从 kata-fc 降级到 runc" 会先命中 RuntimeClassName == "" 分支，
		// 永不评估 cand != want，于是 Degraded 停在 false —— 降级被上报成 DefaultRuntime。
		// 那正是本项目最危险的一类故障：沙箱照常运行、指标一切正常，
		// 但隔离强度已经消失且无人知晓。因此优先级是：
		//   “是否发生了降级” > “是否在用默认运行时”
		// 前者是安全事件，后者只是一条信息。
		switch {
		case cand != want:
			out.Degraded = true
			out.Reason = ReasonFallbackApplied
			if spec.RuntimeClassName == "" {
				out.Message = fmt.Sprintf(
					"隔离级别 %q 不可用，已降级到集群默认运行时 %q（无 VM 级隔离）", want, cand)
			} else {
				out.Message = fmt.Sprintf(
					"隔离级别 %q 不可用，已降级到 %q（隔离强度下降）", want, cand)
			}
		case spec.RuntimeClassName == "":
			out.Reason = ReasonDefaultRuntime
			out.Message = fmt.Sprintf("隔离级别 %q 使用集群默认运行时（runc），无隔离保证", cand)
		}
		return out, nil
	}

	reason := ReasonNoFallback
	if len(candidates(cfg, want, wantSpec, policy)) == 1 {
		reason = ReasonRuntimeClassMissing
	}
	return Availability{
		RequestedLevel: want,
		Level:          want,
		Spec:           wantSpec,
		Available:      false,
		Reason:         reason,
		Message:        fmt.Sprintf("隔离级别 %q 不可用，缺失: %v（策略 %s）", want, missing, policy),
	}, nil
}

// RuntimeClassRef 描述一个缺失的运行时，用于诊断信息。
type RuntimeClassRef struct {
	Level            Level
	RuntimeClassName string
}

// candidates 返回"可接受的级别链"，首项永远是请求级别。
//
// 降级必须由策略显式开启：
//   - FailFast：只接受请求级别本身。
//   - PauseScaling：同样只接受本身。这里不能降级 —— 降级会让**新库存**落到
//     错误的隔离级别，而 PauseScaling 的意图是"保留存量、暂停增量"。
//     把不可用如实上报，由控制器执行暂停扩缩。
//   - FallbackToRunc：按配置的 Fallbacks 顺序降级。
func candidates(cfg *Config, want Level, spec Spec, policy sandboxv1alpha1.DegradationPolicy) []Level {
	chain := []Level{want}
	if policy != sandboxv1alpha1.DegradationFallbackToRunc {
		return chain
	}

	seen := map[Level]bool{want: true}
	for _, fb := range spec.Fallbacks {
		if seen[fb] {
			continue
		}
		if _, ok := cfg.Level(fb); !ok {
			continue // 配置已校验过，这里只是双保险
		}
		seen[fb] = true
		chain = append(chain, fb)
	}
	return chain
}

// probeLevel 判断某个级别在当前集群是否可用。
func probeLevel(ctx context.Context, probe RuntimeClassProbe, spec Spec) (bool, error) {
	if spec.RuntimeClassName == "" {
		// 空=集群默认运行时（runc），一定可用，无需往返 API Server。
		return true, nil
	}
	return probe.Exists(ctx, spec.RuntimeClassName)
}
