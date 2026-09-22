package sweeper

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Sweeper 是对账的执行体：采集快照 → Classify → 执行动作。
//
// 它**不包含任何判断逻辑**。所有"什么算泄漏"的判断都在 Classify 里，
// 因为那部分必须能被穷举测试。这个文件只负责把世界读进来、把结论写回去 ——
// 这类 IO 代码的正确性只能靠集成测试，所以它应该薄到没有可藏 bug 的地方。
type Sweeper struct {
	Client client.Client
	// Options 为零值时使用 DefaultOptions。
	Options Options
	// DryRun 为真时只报告不执行。首次上线必须先跑这个。
	DryRun bool
	// Clock 可注入，便于测试。
	Clock func() time.Time
	// Reporter 用于把发现计数导出为指标。可为空。
	Reporter Reporter
}

// Reporter 接收本轮发现与动作计数。
//
// 抽象成接口是为了让指标导出不绑定具体的 Prometheus 客户端，
// 从而让对账逻辑可以脱离监控栈被测试。
type Reporter interface {
	// Found 报告某类别发现的数量。
	Found(kind Kind, n int)
	// Applied 报告实际执行的动作数量。
	Applied(action Action, n int)
}

// Report 是一轮对账的结果。
//
// 它被刻意设计成 JSON 友好（字段全部导出）：CronJob 的日志就是唯一的现场记录，
// 报告必须能直接打进日志，而不是只能从一堆散落的日志行里事后拼凑。
type Report struct {
	// ScannedAt 是本轮快照的时间。
	ScannedAt time.Time `json:"scannedAt"`

	// 快照规模。它存在的意义是区分"0 发现是真的干净"与
	// "0 发现是因为什么都没扫到" —— 后者是权限配置错误，
	// 而它在报告上与"一切正常"一模一样。
	Sandboxes int `json:"sandboxes"`
	Pods      int `json:"pods"`
	Leases    int `json:"leases"`
	Netpols   int `json:"netpols"`
	Volumes   int `json:"volumes"`

	Findings []Finding      `json:"findings"`
	ByKind   map[Kind]int   `json:"byKind"`
	ByAction map[Action]int `json:"byAction"`

	// Applied 是实际执行的动作数（DryRun 时为 0）。
	Applied int  `json:"applied"`
	DryRun  bool `json:"dryRun"`

	// Truncated 表示本轮触达了 MaxFindings 上限。
	//
	// 这个信号必须单独暴露：达到上限意味着**还有问题没被报出来**。
	// 若只是把列表截断而不说明，报告看起来就是"发现了 200 个问题"，
	// 与"恰好有 200 个问题"完全无法区分。
	Truncated bool `json:"truncated"`

	// Errors 是执行阶段遇到的错误。分类阶段不会产生错误 —— 它是纯函数。
	Errors []string `json:"errors,omitempty"`
}

// Run 执行一轮对账。
func (s *Sweeper) Run(ctx context.Context) (*Report, error) {
	opt := s.Options.withDefaults()
	now := s.now()

	snap, index, err := s.snapshot(ctx, now)
	if err != nil {
		return nil, err
	}

	rep := &Report{
		ScannedAt: now,
		Sandboxes: len(snap.Sandboxes),
		Pods:      len(snap.Pods),
		Leases:    len(snap.Leases),
		Netpols:   len(snap.Netpols),
		Volumes:   len(snap.Volumes),
		DryRun:    s.DryRun,
		ByKind:    map[Kind]int{},
		ByAction:  map[Action]int{},
	}

	rep.Findings = Classify(snap, opt)
	rep.Truncated = len(rep.Findings) >= opt.MaxFindings
	for _, f := range rep.Findings {
		rep.ByKind[f.Kind]++
		rep.ByAction[f.Action]++
	}
	// 即使为 0 也要上报：指标缺失与指标为 0 在监控上长得一样，
	// 而这两者的含义完全相反（前者意味着采集坏了）。
	if s.Reporter != nil {
		for _, k := range AllKinds() {
			s.Reporter.Found(k, rep.ByKind[k])
		}
	}

	if s.DryRun {
		// DryRun 下不执行任何动作，但仍报告"本来会做什么"——
		// 这正是它作为上线前验证手段的全部价值。
		return rep, nil
	}

	for _, f := range rep.Findings {
		if err := s.apply(ctx, f, index, now); err != nil {
			// 单条失败不影响其余：一条失败就让整轮中止，会让对账永远停在
			// 同一个对象上，后面的泄漏再也得不到处理 —— 而那个对象
			// 往往正是导致失败的那一个。
			rep.Errors = append(rep.Errors, f.String()+": "+err.Error())
			continue
		}
		rep.Applied++
	}
	if s.Reporter != nil {
		for _, a := range []Action{ActionBlock, ActionDeleteObject, ActionForceFinalize, ActionMarkFailed} {
			s.Reporter.Applied(a, rep.ByAction[a])
		}
	}
	return rep, nil
}

func (s *Sweeper) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now()
}
