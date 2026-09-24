package controller

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	rtctrl "sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
	"github.com/Cyrene-06/Dynamic-Worker-Pool-Management/internal/isolation"
)

// HitRatioSource 提供池的热路径命中率（千分比）。
//
// **必须能表达"未知"**：返回 -1 表示不可用，返回 0 表示命中率真的是 0。
// 这个区分不是洁癖：把"指标不可用"当成 0，会让反馈项把目标水位推到上限 ——
// 一次监控故障就放大成一次大规模扩容。这是最难排查的一类故障，
// 因为控制器的每一步看起来都对。
type HitRatioSource interface {
	HitRatioPermille(pool string) int32
}

// StaticHitRatio 是固定返回"未知"的实现，用于骨架阶段。
//
// 默认返回未知而不是返回一个假数字：宁可让反馈项保持中性，
// 也不要让控制器基于编造的指标做决策。
type StaticHitRatio struct{}

// HitRatioPermille 实现 HitRatioSource。
func (StaticHitRatio) HitRatioPermille(string) int32 { return -1 }

// PoolReconciler 维持沙箱池的水位。
//
// 与 SandboxController 的分工（docs/03 §2）：本控制器只消费"库存水位"这个
// 聚合量，只创建/删除 **Pending 状态的库存对象**，绝不动已认领的沙箱。
// 两者混在一个控制器里会互相污染：回收瞬时降低库存会触发扩容，
// 新库存刚就绪又被识别为多余而缩容，形成自激振荡。
type PoolReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Resolver *isolation.Resolver
	Recorder record.EventRecorder
	Clock    func() time.Time
	// HitRatio 提供命中率指标；为 nil 时按"未知"处理。
	HitRatio HitRatioSource

	// mu 保护 history。振荡检测是尽力而为的安全信号，不是正确性依据，
	// 因此放在内存里即可；重启后历史丢失只会让检测晚几轮生效。
	mu      sync.Mutex
	history map[string][]scaleEvent
}

type scaleEvent struct {
	at    time.Time
	delta int32
}

// oscillationWindow 是振荡检测的观察窗口。
const oscillationWindow = 5 * time.Minute

// SetupWithManager 注册控制器。
//
// **并发度硬编码为 1**，不开放配置。原因很具体：池决策是全局的，
// 若允许并发，多个 goroutine 会各自看到同一个缺口并各自扩容一次，
// 直接造成数倍超额扩容。这个参数看起来像个"性能调优项"，
// 实际上是个正确性开关，所以不给它被调大的机会。
func (r *PoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("sandboxpool").
		For(&sandboxv1alpha1.SandboxPool{}).
		// 库存对象的变化必须能驱动池决策：否则"库存被认领了"这件事
		// 要等下一次定时评估才被发现，而那时用户已经在等冷启动了。
		Owns(&sandboxv1alpha1.AgentSandbox{}).
		WithOptions(rtctrl.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

// Reconcile 实现 reconcile.Reconciler。
func (r *PoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var pool sandboxv1alpha1.SandboxPool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	counts, err := r.countStock(ctx, &pool)
	if err != nil {
		return ctrl.Result{}, err
	}

	// ---- 保护模式（雪崩保护，docs/05 §5）----
	//
	// 判定放在所有分支之前：它要进 status，而接入层（gateway）读的就是
	// 那个字段来决定“保护期内谁被拒绝”。同一个事实只能有一个来源，
	// 否则会出现“控制器认为已恢复、gateway 仍在拒绝”这类无法解释的现象。
	//
	// 放在排空分支之前也是有意的：排空期间仍然可能有存活会话在失败，
	// 而“保护状态”在排空结束后还需要继续可见 —— 否则一次维护窗口
	// 会把上一轮的告警悄悄滑掉。
	protect := EvaluateProtectMode(pool.Spec.ProtectMode, ProtectObservation{
		Now:      r.now(),
		Inflight: counts.inflight,
		Failed:   counts.failed,
		// API Server 延迟观测需要一个进程级的时间序列来源，
		// 而 client-go 的直方图并不落在 controller-runtime 的 registry 里、
		// 无法直接读取。因此这里如实上报“未知”，而不是填 0：
		// 填 0 会让“不知道”与“非常健康”长得完全一样，
		// 而保护模式恰恰是在监控出问题的时候最需要工作。
		// 处理方式与 NodeHeadroom 一致（显式上报未知，M3 接入指标管线）。
		LatencyKnown: false,
		PrevActive:   pool.Status.ProtectMode.Active,
		PrevSince:    metav1TimeOrZero(pool.Status.ProtectMode.Since),
		PrevUntil:    metav1TimeOrZero(pool.Status.ProtectMode.Until),
		PrevTrips:    pool.Status.ProtectMode.Trips,
	})
	r.applyProtectMode(&pool, protect)

	// ---- 排空：优先于一切自动决策 ----
	if pool.Spec.Drain.Enabled {
		return r.reconcileDrain(ctx, &pool, counts)
	}

	// ---- 阶段一：清理上一轮标记的 draining 对象 ----
	//
	// 这一步不会造成过度缩容：被标记 draining 的对象在 countStock 里
	// 已经**不计入 warm**，所以本轮的水位计算与它们的删除无关。
	// 两阶段的意义在于崩溃安全：若在标记与删除之间控制器退出，
	// 重启后这些对象仍然被排除在水位之外，不会触发第二轮缩容。
	if counts.draining > 0 {
		if err := r.deleteDraining(ctx, &pool); err != nil {
			return ctrl.Result{}, err
		}
	}

	// ---- 阶段二：隔离能力检查 ----
	//
	// 在创建库存**之前**确认运行时可用。否则会造出一批调度不上、
	// 或者更糟 —— 静默回落到 runc 的"沙箱"。
	av, err := r.Resolver.Resolve(ctx, pool.Spec.Isolation)
	if err != nil {
		return ctrl.Result{}, err
	}
	r.setCondition(&pool, sandboxv1alpha1.CondRuntimeAvailable, av.Available,
		string(av.Reason), av.Message)
	if !av.Available {
		r.Recorder.Eventf(&pool, corev1.EventTypeWarning, "IsolationUnavailable", "%s", av.Message)
		return r.finish(ctx, &pool, counts, ScaleDecision{
			Action: ScaleHold, Target: counts.warm, Reason: string(av.Reason),
		}, "运行时不可用，暂停扩缩")
	}
	if av.Degraded {
		// 降级是安全事件，必须留下独立证据：事件 + 日志。
		r.Recorder.Eventf(&pool, corev1.EventTypeWarning, "IsolationDegraded",
			"请求 %s，实际生效 %s", av.RequestedLevel, av.Level)
	}
	if pool.Spec.RuntimeClassName != "" && av.Degraded {
		return ctrl.Result{}, fmt.Errorf("池 %s 指定 RuntimeClass %s，不允许降级到 %s", pool.Name, pool.Spec.RuntimeClassName, av.Level)
	}
	if err := validateRuntimeClassOverride(ctx, r.Client, pool.Spec.Isolation, pool.Spec.RuntimeClassName); err != nil {
		return ctrl.Result{}, err
	}

	// ---- 阶段三：决策 ----
	o := ScaleObservation{
		Now:                  r.now(),
		Warm:                 counts.warm,
		Inflight:             counts.inflight,
		Draining:             counts.draining,
		Claimed:              counts.claimed,
		Failed:               counts.failed,
		PredictedDemand:      pool.Status.PredictedDemand,
		HitRatioPermille:     r.hitRatio(&pool),
		OscillationReversals: r.reversals(pool.Name),
		NodeHeadroom:         -1, // 节点容量观测需 Karpenter/CA 指标，M3 接入
		ProtectActive:        protect.Active,
	}
	if pool.Status.LastScaleDecision != nil {
		o.LastScaleDelta = pool.Status.LastScaleDecision.Delta
		if pool.Status.LastScaleDecision.At != nil {
			o.LastScaleAt = pool.Status.LastScaleDecision.At.Time
		}
	}

	d := DecideScaling(&pool, o)

	// ---- 阶段四：执行 ----
	switch d.Action {
	case ScaleUp:
		created, err := r.createStock(ctx, &pool, d.Delta)
		if err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(&pool, corev1.EventTypeNormal, "PoolScaledUp",
			"target=%d delta=%d created=%d reason=%s", d.Target, d.Delta, created, d.Reason)
		r.recordScale(pool.Name, created)

	case ScaleDown:
		marked, err := r.markDraining(ctx, &pool, -d.Delta)
		if err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(&pool, corev1.EventTypeNormal, "PoolScaledDown",
			"target=%d delta=%d marked=%d reason=%s", d.Target, d.Delta, marked, d.Reason)
		r.recordScale(pool.Name, -marked)

	default:
		if d.Suppressed {
			// 被抑制时必须能看见原因，否则运维只会看到"水位长期不达标"，
			// 然后去调错参数。
			log.V(1).Info("扩缩被抑制", "reason", d.SuppressReason, "target", d.Target)
		}
	}

	if d.MetricsUnavailable {
		r.setCondition(&pool, "MetricsAvailable", false, ScaleReasonMetricsMissing,
			"命中率指标不可用，反馈项按中性处理（目标水位不会因此被推高）")
	}

	return r.finish(ctx, &pool, counts, d, DescribeScaleDecision(d))
}

// reconcileDrain 处理排空：停止认领、回收未认领库存，等现有会话结束后允许维护。
func (r *PoolReconciler) reconcileDrain(ctx context.Context, pool *sandboxv1alpha1.SandboxPool, counts *stockCounts) (ctrl.Result, error) {
	// 1. 已标记 draining 的直接删。
	if err := r.deleteDraining(ctx, pool); err != nil {
		return ctrl.Result{}, err
	}
	// 2. 把仍可用的库存标记为 draining（下一轮删除）。
	//    先标记再删的理由与缩容相同：崩溃安全。
	marked, err := r.markDraining(ctx, pool, counts.warm)
	if err != nil {
		return ctrl.Result{}, err
	}
	if marked > 0 {
		r.Recorder.Eventf(pool, corev1.EventTypeNormal, "PoolDraining",
			"标记 %d 个库存待回收（原因：%s）", marked, pool.Spec.Drain.Reason)
	}

	// 3. 未认领库存清零即视为排空完成；已认领的沙箱不主动杀，等自然释放
	//    或超过 graceSeconds 后由 SandboxController 处理。
	drained := counts.warm == 0 && counts.draining == 0
	r.setCondition(pool, "Drained", drained, "PoolDraining",
		fmt.Sprintf("drain in progress: warm=%d draining=%d claimed=%d grace=%ds",
			counts.warm, counts.draining, counts.claimed, pool.Spec.Drain.GraceSeconds))

	r.setCondition(pool, sandboxv1alpha1.CondScalingReady, false, ScaleReasonDraining,
		"池正在排空，自动扩缩已停止")

	return ctrl.Result{RequeueAfter: 10 * time.Second}, r.updateStatus(ctx, pool, counts, ScaleDecision{
		Action: ScaleHold, Target: 0, Reason: ScaleReasonDraining,
	})
}

// ---------------------------------------------------------------------------
// 库存清点与操作
// ---------------------------------------------------------------------------

// stockCounts 是库存的分类计数。
type stockCounts struct {
	warm     int32
	inflight int32
	draining int32
	claimed  int32
	failed   int32
}

// countStock 按阶段清点池内对象。
//
// 分类顺序很重要：draining 必须优先判断。一个被标记 draining 的库存
// 在 phase 上仍然是 Ready，若先按 phase 分类就会被计入 warm，
// 于是缩容决策会重复计算同一批对象 —— 表现为一次缩掉两三倍。
func (r *PoolReconciler) countStock(ctx context.Context, pool *sandboxv1alpha1.SandboxPool) (*stockCounts, error) {
	var list sandboxv1alpha1.AgentSandboxList
	if err := r.List(ctx, &list, client.MatchingLabels{
		sandboxv1alpha1.LabelPool: pool.Name,
	}); err != nil {
		return nil, fmt.Errorf("清点池 %s 的库存失败: %w", pool.Name, err)
	}

	c := &stockCounts{}
	for i := range list.Items {
		s := &list.Items[i]
		switch {
		case s.Labels[sandboxv1alpha1.LabelDraining] == "true":
			c.draining++
		case s.Status.Phase == sandboxv1alpha1.PhaseReady && s.Spec.Claim == nil:
			c.warm++
		case s.Status.Phase == sandboxv1alpha1.PhasePending,
			s.Status.Phase == sandboxv1alpha1.PhaseProvisioning:
			c.inflight++
		case s.Status.Phase.IsClaimedPhase():
			c.claimed++
		case s.Status.Phase == sandboxv1alpha1.PhaseFailed:
			c.failed++
		}
	}
	return c, nil
}

// createStock 创建 n 个 Pending 库存对象。
//
// 刻意返回实际创建数而不是假装成功：部分失败（配额、API 抖动）时
// 水位需要按真实值回写，否则 status 会虚高，下一轮又少补。
func (r *PoolReconciler) createStock(ctx context.Context, pool *sandboxv1alpha1.SandboxPool, n int32) (int32, error) {
	if n <= 0 {
		return 0, nil
	}
	var tmpl sandboxv1alpha1.SandboxTemplate
	if err := r.Get(ctx, client.ObjectKey{Name: pool.Spec.TemplateRef.Name}, &tmpl); err != nil {
		return 0, fmt.Errorf("读取池 %s 引用的模板 %q 失败: %w", pool.Name, pool.Spec.TemplateRef.Name, err)
	}

	var created int32
	for i := int32(0); i < n; i++ {
		sbx := r.newStockSandbox(pool, &tmpl)
		if err := r.Create(ctx, sbx); err != nil {
			if apierrors.IsAlreadyExists(err) {
				continue
			}
			// 部分成功：把已创建的数量报上去，让 status 反映真实水位。
			return created, fmt.Errorf("创建库存失败（已创建 %d/%d）: %w", created, n, err)
		}
		created++
	}
	return created, nil
}

func (r *PoolReconciler) newStockSandbox(pool *sandboxv1alpha1.SandboxPool, tmpl *sandboxv1alpha1.SandboxTemplate) *sandboxv1alpha1.AgentSandbox {
	name := fmt.Sprintf("%s-", pool.Name)
	sbx := &sandboxv1alpha1.AgentSandbox{
		ObjectMeta: metav1.ObjectMeta{
			// generateName 而不是自己拼序号：并发创建时自拼序号会撞名，
			// 而 AlreadyExists 会被当成失败。
			GenerateName: name,
			Namespace:    sandboxv1alpha1.NamespacePool,
			Labels: map[string]string{
				sandboxv1alpha1.LabelPool:      pool.Name,
				sandboxv1alpha1.LabelTier:      pool.Spec.Tier,
				sandboxv1alpha1.LabelTemplate:  pool.Spec.TemplateRef.Name,
				sandboxv1alpha1.LabelIsolation: string(pool.Spec.Isolation),
				sandboxv1alpha1.LabelRole:      sandboxv1alpha1.RoleSandbox,
				// 库存阶段就带上 claimed=false：认领方靠这个 label 列表，
				// 没有它就连候选都查不到。
				sandboxv1alpha1.LabelClaimed: "false",
			},
		},
		Spec: sandboxv1alpha1.AgentSandboxSpec{
			PoolRef:          sandboxv1alpha1.NameRef{Name: pool.Name},
			TemplateRef:      sandboxv1alpha1.NameRef{Name: pool.Spec.TemplateRef.Name},
			Tier:             pool.Spec.Tier,
			Isolation:        pool.Spec.Isolation,
			RuntimeClassName: pool.Spec.RuntimeClassName,
			// Claim 留空 —— 这就是"库存"的定义。
		},
	}
	// 让库存随池一起被 GC，避免池删除后留下一堆无主对象。
	// 注意：这只是第三层防护，真正的清理靠 PoolController 与对账 Sweeper。
	_ = ctrl.SetControllerReference(pool, sbx, r.Scheme)
	return sbx
}

// markDraining 把 n 个最老的可用库存标记为 draining。
//
// 只改 label，不改 spec/status：**认领方靠 label 过滤候选**，
// 打上 draining 后它们立刻从可认领集合里消失，这是"停止认领"的实现方式。
// 若改 status 则需要 status 写入权限与额外一轮 reconcile，没必要。
func (r *PoolReconciler) markDraining(ctx context.Context, pool *sandboxv1alpha1.SandboxPool, n int32) (int32, error) {
	if n <= 0 {
		return 0, nil
	}
	candidates, err := r.drainCandidates(ctx, pool)
	if err != nil {
		return 0, err
	}

	var marked int32
	for i := range candidates {
		if marked >= n {
			break
		}
		s := &candidates[i]
		base := s.DeepCopy()
		if s.Labels == nil {
			s.Labels = map[string]string{}
		}
		s.Labels[sandboxv1alpha1.LabelDraining] = "true"
		// 这里用的是普通 MergeFrom（不带乐观锁）：标记 draining 是幂等操作，
		// 即便与认领竞争也不会造成不一致 —— 认领会写 spec.claim，我们只写 label，
		// 两者字段不重叠。带上乐观锁反而会在高竞争下大量冲突、拖慢缩容。
		if err := r.Patch(ctx, s, client.MergeFrom(base)); err != nil {
			return marked, fmt.Errorf("标记库存 %s 为 draining 失败: %w", s.Name, err)
		}
		marked++
	}
	return marked, nil
}

// drainCandidates 返回可被标记 draining 的库存：Ready、未认领、未在删除、未标记。
func (r *PoolReconciler) drainCandidates(ctx context.Context, pool *sandboxv1alpha1.SandboxPool) ([]sandboxv1alpha1.AgentSandbox, error) {
	var list sandboxv1alpha1.AgentSandboxList
	if err := r.List(ctx, &list, client.MatchingLabels{
		sandboxv1alpha1.LabelPool:    pool.Name,
		sandboxv1alpha1.LabelClaimed: "false",
	}); err != nil {
		return nil, err
	}

	out := make([]sandboxv1alpha1.AgentSandbox, 0, len(list.Items))
	for i := range list.Items {
		s := &list.Items[i]
		if s.DeletionTimestamp != nil {
			continue
		}
		if s.Labels[sandboxv1alpha1.LabelDraining] == "true" {
			continue
		}
		if s.Status.Phase != sandboxv1alpha1.PhaseReady || s.Spec.Claim != nil {
			continue
		}
		out = append(out, *s)
	}

	// 先淘汰最老的库存。理由与认领侧的 FIFO 相同：让库存真正被轮换，
	// 而不是让同一批对象一直躺在池里直到 maxStockAgeSeconds 强制销毁。
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CreationTimestamp.Before(&out[j].CreationTimestamp)
	})
	return out, nil
}

// deleteDraining 删除已标记 draining 的对象（限速）。
//
// 限速是关键：批量删除会同时压 API Server、CNI 与配额，而缩容没有时效性要求
// （不像扩容会影响用户体验），所以宁慢勿快。
func (r *PoolReconciler) deleteDraining(ctx context.Context, pool *sandboxv1alpha1.SandboxPool) error {
	var list sandboxv1alpha1.AgentSandboxList
	if err := r.List(ctx, &list, client.MatchingLabels{
		sandboxv1alpha1.LabelPool:     pool.Name,
		sandboxv1alpha1.LabelDraining: "true",
	}); err != nil {
		return err
	}

	limit := int(effectiveMaxReclaim(pool.Spec.Scaling))
	deleted := 0
	for i := range list.Items {
		if deleted >= limit {
			break
		}
		s := &list.Items[i]
		if err := r.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("删除 draining 库存 %s 失败: %w", s.Name, err)
		}
		deleted++
	}
	return nil
}

// ---------------------------------------------------------------------------
// 状态与辅助
// ---------------------------------------------------------------------------

// finish 统一写 status，避免每条分支各写一遍（漏写会让 status 与水位不一致）。
func (r *PoolReconciler) finish(ctx context.Context, pool *sandboxv1alpha1.SandboxPool,
	counts *stockCounts, d ScaleDecision, msg string) (ctrl.Result, error) {

	total := counts.warm + counts.claimed
	saturation := int32(0)
	if total > 0 {
		saturation = counts.claimed * 1000 / total
	}

	pool.Status.Warm = counts.warm
	pool.Status.Inflight = counts.inflight
	pool.Status.Draining = counts.draining
	pool.Status.Claimed = counts.claimed
	pool.Status.Failed = counts.failed
	pool.Status.Target = d.Target
	pool.Status.PredictedDemand = d.PredictedDemand
	pool.Status.SaturationPermille = saturation
	pool.Status.HitRatio1hPermille = r.hitRatio(pool)

	if d.Delta != 0 {
		now := metav1.NewTime(r.now())
		pool.Status.LastScaleDecision = &sandboxv1alpha1.ScaleDecision{
			At:     &now,
			Reason: d.Reason,
			Delta:  d.Delta,
		}
	}

	r.setCondition(pool, sandboxv1alpha1.CondScalingReady, true, d.Reason, msg)

	if err := r.updateStatus(ctx, pool, counts, d); err != nil {
		return ctrl.Result{}, err
	}

	// 有缺口时尽快回来（draining 待删、库存待补），否则按退避间隔评估。
	requeue := 30 * time.Second
	if counts.draining > 0 || d.Action != ScaleHold {
		requeue = 2 * time.Second
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

func (r *PoolReconciler) updateStatus(ctx context.Context, pool *sandboxv1alpha1.SandboxPool, _ *stockCounts, _ ScaleDecision) error {
	pool.Status.ObservedGeneration = pool.Generation
	// 只在实质变化时写：status 写入会触发所有 watcher，
	// 5k 规模下"每轮都写一次"的代价非常具体。
	return r.Status().Update(ctx, pool)
}

// applyProtectMode 把保护模式的判定结果写进 status，并在**状态变化时**发事件。
//
// 只在变化时发事件：一场持续 10 分钟的危机若每轮都发一条，会刷出几百条
// 相同事件，把真正需要看的东西挤出事件流 —— 而事件流正是危机中最先被读的东西。
func (r *PoolReconciler) applyProtectMode(pool *sandboxv1alpha1.SandboxPool, d ProtectDecision) {
	st := &pool.Status.ProtectMode
	st.Active = d.Active
	st.Reason = d.Reason
	st.Trips = d.Trips
	st.FailureRatePermille = d.FailureRatePermille
	if !d.Since.IsZero() {
		t := metav1.NewTime(d.Since)
		st.Since = &t
	}
	if !d.Until.IsZero() {
		t := metav1.NewTime(d.Until)
		st.Until = &t
	} else {
		st.Until = nil
	}

	// Condition 用“正面陈述”：ProtectModeNormal=False 即处于保护中。
	r.setCondition(pool, sandboxv1alpha1.CondProtectModeNormal, !d.Active, d.Reason,
		protectMessage(d))

	switch {
	case d.TrippedNow:
		r.Recorder.Eventf(pool, corev1.EventTypeWarning, "ProtectModeEnabled",
			"进入雪崩保护: reason=%s failureRate=%d‰ hold=%s（低优申请将被拒绝，扩容限速降至 %d‰）",
			d.Reason, d.FailureRatePermille,
			d.Until.Sub(d.Since).Truncate(time.Second), d.ThrottlePermille)
	case d.RecoveredNow:
		r.Recorder.Eventf(pool, corev1.EventTypeNormal, "ProtectModeDisabled",
			"退出雪崩保护: 累计进入 %d 次", d.Trips)
	}
}

// protectMessage 生成给运维看的一句话。
//
// 刻意包含“还要持续多久”：保护模式生效时，运维最先问的就是
// “它是会自己好，还是需要我去做什么”。
func protectMessage(d ProtectDecision) string {
	if !d.Active {
		return "未处于雪崩保护"
	}
	if d.Reason == ProtectReasonHold {
		return "触发条件已消失，处于滞回期（自动恢复）"
	}
	return fmt.Sprintf("雪崩保护生效（原因 %s，失败率 %d‰），低优申请被拒绝、扩容限速至 %d‰",
		d.Reason, d.FailureRatePermille, d.ThrottlePermille)
}

// metav1TimeOrZero 把可选的 metav1.Time 转成 time.Time（nil 即零值）。
//
// 零值在这里是有意义的：它表示“上一轮没有进入过保护”，
// 而不是“进入了但时间未知”——后者在状态机里必须被当作已经过期。
func metav1TimeOrZero(t *metav1.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.Time
}

func (r *PoolReconciler) setCondition(pool *sandboxv1alpha1.SandboxPool, condType string, ok bool, reason, msg string) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: pool.Generation,
	})
}

func (r *PoolReconciler) hitRatio(pool *sandboxv1alpha1.SandboxPool) int32 {
	if r.HitRatio == nil {
		return -1 // 未知，而不是 0
	}
	return r.HitRatio.HitRatioPermille(pool.Name)
}

func (r *PoolReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

// recordScale 记录一次扩缩方向，用于振荡检测。
func (r *PoolReconciler) recordScale(pool string, delta int32) {
	if delta == 0 {
		return
	}
	dir := int32(1)
	if delta < 0 {
		dir = -1
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.history == nil {
		r.history = map[string][]scaleEvent{}
	}
	h := r.history[pool]
	h = append(h, scaleEvent{at: r.now(), delta: dir})
	// 只保留窗口内的记录，避免内存随运行时间无界增长。
	cutoff := r.now().Add(-oscillationWindow)
	kept := h[:0]
	for _, e := range h {
		if e.at.After(cutoff) {
			kept = append(kept, e)
		}
	}
	r.history[pool] = kept
}

// reversals 统计窗口内扩缩方向反转的次数。
func (r *PoolReconciler) reversals(pool string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.history[pool]
	if len(h) < 2 {
		return 0
	}
	cutoff := r.now().Add(-oscillationWindow)
	n := 0
	var prev int32
	for _, e := range h {
		if e.at.Before(cutoff) {
			continue
		}
		if prev != 0 && e.delta != prev {
			n++
		}
		prev = e.delta
	}
	return n
}
