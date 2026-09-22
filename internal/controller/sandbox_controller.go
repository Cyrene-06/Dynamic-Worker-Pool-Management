package controller

import (
	"context"
	"fmt"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	rtctrl "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
	"github.com/Cyrene-06/Dynamic-Worker-Pool-Management/internal/isolation"
)

// Finalizer 名称。顺序即语义，见 docs/04 §6.1：
// 先断网再落盘、先释放 Lease 让业务尽快感知"沙箱已死"。
const (
	FinalizerLeaseCleanup    = "sandbox.example.com/lease-cleanup"
	FinalizerNetworkCleanup  = "sandbox.example.com/network-cleanup"
	FinalizerStateFlush      = "sandbox.example.com/state-flush"
	FinalizerVolumeCleanup   = "sandbox.example.com/volume-cleanup"
	FinalizerMetricsFinalize = "sandbox.example.com/metrics-finalize"
)

// finalizerStepTimeout 是 Finalizer 链每一步的硬超时。
//
// 超时后**强制推进**（移除 finalizer 并继续），而不是返回 error。
// 理由：Finalizer 卡死会让对象永远无法删除，只能人工 patch 救场 ——
// 那比"泄漏一个资源、由对账 Sweeper 兜底"严重得多。宁可泄漏，不可锁死。
const finalizerStepTimeout = 30 * time.Second

// SandboxReconciler 把 AgentSandbox 的期望状态推进到实际状态。
//
// 职责刻意很窄，只有三件事：
//  1. 观察：把集群事实收敛成一个 Observation
//  2. 决策：调用纯函数 NextPhase
//  3. 应用：写 status、建/删 Pod
//
// 所有判断逻辑都在 NextPhase / isolation 里，因此这个文件几乎没有 if-else。
// 这是让状态机可测试的关键 —— controller 越薄，未被测试覆盖的分支就越少。
type SandboxReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Resolver *isolation.Resolver
	Recorder record.EventRecorder

	// StateFlusher 负责状态外置落盘（L3）。为空时使用 DisabledFlusher，
	// 它会明确拒绝而不是假装落盘成功。
	StateFlusher StateFlusher

	// Clock 可注入，便于测试时间相关分支。
	Clock func() time.Time
}

// Reconcile 实现 reconcile.Reconciler。
func (r *SandboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var sbx sandboxv1alpha1.AgentSandbox
	if err := r.Get(ctx, req.NamespacedName, &sbx); err != nil {
		// 对象已删除是正常路径，不是错误。
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 删除路径优先：一旦进入删除，状态机不再推进。
	if !sbx.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &sbx)
	}

	if !controllerutil.ContainsFinalizer(&sbx, FinalizerLeaseCleanup) {
		return r.ensureFinalizers(ctx, &sbx)
	}

	// 崩溃恢复：已经决定回收、但对象还没删掉。
	//
	// 决策与删除是两次写，中间可能崩溃或被重启。没有这一步，对象会永远停在
	// Terminating —— 而状态机对 Terminating 不做任何推进，于是它既不回收资源、
	// 也不再计入池水位，成为一个纯粹靠人工发现的泄漏。
	if sbx.Status.Phase == sandboxv1alpha1.PhaseTerminating {
		return ctrl.Result{}, r.deleteSandbox(ctx, &sbx)
	}

	// 终态对象必须被删除。
	//
	// Failed 是终态，而状态机对终态不做任何推进（NextPhase 返回 ReasonTerminal）。
	// 若不在这里删掉，一个被判失败的沙箱会永远留在集群里：它不回收资源、
	// 不计入池水位、也不会被下一轮 reconcile 处理 —— 相当于把泄漏
	// 换成了另一个名字。删除后 Finalizer 链会完成实际清理。
	//
	// 同样的原因，Sweeper 的对账动作选择"标记 Failed"而不是自己删：
	// 标记后由这里接手，就能保住 Finalizer 链的清理顺序（先断网、再落盘、后删卷）。
	if sbx.Status.Phase.IsTerminal() {
		return ctrl.Result{}, r.deleteSandbox(ctx, &sbx)
	}

	o, err := r.observe(ctx, &sbx)
	if err != nil {
		// 观察失败时**不做决策**：把"读不到状态"当成"沙箱异常"会导致误回收。
		return ctrl.Result{}, err
	}

	// 认领契约必须在**决策之前**落进 status。
	// 顺序反了的话，本轮决策读到的 claimedAt 是空的，空闲判定会退化成
	// "从创建时间算起"，于是一个在池里待了很久的库存会在认领当轮被判空闲并回收。
	bound := r.bindClaimStatus(&sbx, o.Now)

	// 承载节点必须写回 status：
	//   - Sweeper 的"节点已不存在"对账靠 status.nodeName 定位；
	//   - 排障时需要知道沙箱当时跑在哪台机器上，而 Pod 删掉后就查不到了。
	nodeChanged := false
	if o.NodeName != "" && sbx.Status.NodeName != o.NodeName {
		sbx.Status.NodeName = o.NodeName
		nodeChanged = true
	}

	// 心跳 Lease 是**收敛目标**而不是一次性动作：每轮都确保它存在，
	// 这样一次手工误删能被自动修复，而不会演变成一次误回收。
	if sbx.Spec.Claim != nil {
		if err := r.ensureLease(ctx, &sbx, o.Now); err != nil {
			return ctrl.Result{}, err
		}
	}

	// 模板提供生命周期默认值（maxClaimCount、模板级空闲阈值）。
	//
	// 必须在决策**之前**拿到：状态机自己只能看到沙箱，若不给它这些取值，
	// 所有模板级默认值都会被重算成零值 —— 表现是"模板里配了 maxClaimCount
	// 但库存永远不轮换"，而配置看起来完全正确，没有任何线索。
	tmpl := r.templateFor(ctx, &sbx)
	lc := ApplyTemplateDefaults(&sbx, tmpl)

	d := NextPhaseWithLifecycle(&sbx, lc, *o)

	// 先落 status，再动实际资源。顺序反过来的话，一旦中间失败，
	// 实际资源已经变了但 status 没变，下一轮会做出不一致的决策。
	if d.Next != "" && d.Next != sbx.Status.Phase {
		if err := r.applyTransition(ctx, &sbx, d, tmpl); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(&sbx, corev1.EventTypeNormal, "PhaseTransition",
			"%s -> %s (%s)", sbx.Status.Phase, d.Next, d.Reason)
		log.Info("phase transition",
			"from", sbx.Status.Phase, "to", d.Next, "reason", string(d.Reason))
	} else if bound.Changed() || nodeChanged {
		// 没有阶段迁移但事实变了（典型场景：冷路径直接在 Provisioning 上认领，
		// 不会有 Ready → Running 这次迁移）。不持久化的话 claimedAt 永远写不进去，
		// 空闲判定会一直是错的 —— 而且从任何日志里都看不出哪里错。
		if err := r.Status().Update(ctx, &sbx); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{RequeueAfter: d.RequeueAfter}, nil
}

// observe 把集群事实收敛成 Observation。
func (r *SandboxReconciler) observe(ctx context.Context, sbx *sandboxv1alpha1.AgentSandbox) (*Observation, error) {
	now := r.now()
	o := &Observation{Now: now}

	// ---- Pod ----
	var pod corev1.Pod
	err := r.Get(ctx, types.NamespacedName{Namespace: sbx.Namespace, Name: sbx.Status.PodName}, &pod)
	switch {
	case err == nil:
		o.PodExists = true
		o.PodPhase = pod.Status.Phase
		o.PodCreatedAt = pod.CreationTimestamp.Time
		o.PodReady = podReady(&pod)
		o.NodeName = pod.Spec.NodeName
	case apierrors.IsNotFound(err):
		o.PodExists = false
	default:
		return nil, fmt.Errorf("读取 Pod 失败: %w", err)
	}

	// ---- 节点 ----
	// 只有拿到 Pod 的 nodeName 才能判断节点健康；Pod 不存在时不臆测节点状态。
	o.NodeReady = true
	if o.NodeName != "" {
		var node corev1.Node
		if err := r.Get(ctx, types.NamespacedName{Name: o.NodeName}, &node); err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("读取 Node 失败: %w", err)
			}
			o.NodeReady = false
		} else {
			o.NodeReady = nodeReady(&node)
		}
	}

	// ---- 心跳（主判据）----
	// Lease 与沙箱同名同命名空间，这样可以按名 Get 走缓存，不必 List。
	var lease coordinationv1.Lease
	err = r.Get(ctx, types.NamespacedName{Namespace: sbx.Namespace, Name: sbxLeaseName(sbx)}, &lease)
	switch {
	case err == nil:
		o.LastLeaseRenewAt = leaseRenewTime(&lease)
		o.LeaseHealthy = leaseHealthy(&lease, EffectiveLifecycle(sbx), now)
	case apierrors.IsNotFound(err):
		// 尚未认领的库存没有 Lease，这不算"心跳丢失"。
		o.LeaseHealthy = sbx.Spec.Claim == nil
		o.LastLeaseRenewAt = time.Time{}
	default:
		return nil, fmt.Errorf("读取 Lease 失败: %w", err)
	}

	// ---- 活动（辅助判据）----
	// 这里只填已经持久化在 status 里的观测值。实时的 eBPF / cgroup 信号
	// 由节点侧 agent 采集后回写 status，控制器不直接读宿主指标 ——
	// 否则控制器就依赖了具体节点，无法水平扩展。
	//
	// 注意取的是 CPUIncrementMilli（区间增量）而不是 P95CPUmilli：
	// P95 是历史分位数，一个沙箱只要曾经跑过高负载，它的 P95 就会在整个
	// 统计窗口内居高不下。把 P95 当增量用会让该沙箱**永远显示为活跃、
	// 永远不被回收**，而且从 status 上看不出任何异常。
	o.ActiveConnections = sbx.Status.Activity.ActiveConnections
	o.NetworkActivity = o.ActiveConnections > 0
	o.CPUIncrementMilli = sbx.Status.Activity.CPUIncrementMilli

	// ---- 休眠 ----
	o.Frozen = sbx.Status.Hibernation.State == sandboxv1alpha1.RuntimeFrozen
	o.Resumed = sbx.Status.Hibernation.State == sandboxv1alpha1.RuntimeActive

	// ---- 人工干预 ----
	// 只有"要求唤醒"而没有"要求休眠"时才算人工休眠请求：
	// 两个标记可能同时存在（客户先后调了 :hibernate 和 :wake），
	// 而 gateway 未必来得及清理。此时以唤醒为准 —— 让一个本应被唤醒的
	// 会话保持冻结，比让一个本应冻结的会话多跑一会儿代价大得多：
	// 前者是业务实际不可用，后者只是资源多占一点。
	o.ManualWake = sbx.Annotations[sandboxv1alpha1.AnnoWakeRequested] == "true"
	o.ManualHibernate = sbx.Annotations[sandboxv1alpha1.AnnoHibernateRequested] == "true" && !o.ManualWake

	return o, nil
}

// applyTransition 把决策落到 status，并在需要时创建/删除实际资源。
func (r *SandboxReconciler) applyTransition(
	ctx context.Context,
	sbx *sandboxv1alpha1.AgentSandbox,
	d Decision,
	tmpl *sandboxv1alpha1.SandboxTemplate,
) error {
	// 1. 回收/失败类决策：记下原因、落 status，然后**删除对象**，
	//    由 Finalizer 链接手实际清理。
	//
	//    为什么必须真删、而不是停在 Terminating：状态机对 Terminating 不做任何推进，
	//    停在那个阶段的对象既不回收资源、也不再计入池水位 —— 是纯泄漏。
	//    删除后清理顺序由 Finalizer 链的顺序保证（先断网、后落盘、再删卷）。
	//
	//    先写 status 再删除，是为了让业务在对象消失前还能通过 GET 拿到
	//    recycleReason（对应 API 契约里的 410 Gone）。
	if d.Reclaim {
		sbx.Status.Phase = d.Next
		sbx.Status.ObservedGeneration = sbx.Generation
		if d.RecycleReason != "" {
			sbx.Status.Metrics.RecycleReason = d.RecycleReason
		}
		meta.SetStatusCondition(&sbx.Status.Conditions, metav1.Condition{
			Type:               conditionTypeFor(d),
			Status:             metav1.ConditionTrue,
			Reason:             string(d.Reason),
			Message:            d.Message,
			ObservedGeneration: sbx.Generation,
		})
		if err := r.Status().Update(ctx, sbx); err != nil {
			// 冲突时交给下一轮重试：此时决策仍未生效，重放是安全的。
			if apierrors.IsConflict(err) {
				return nil
			}
			return err
		}
		return r.deleteSandbox(ctx, sbx)
	}

	// 2. 需要 Pod 的阶段但 Pod 不存在 → 创建。
	//    创建动作放在这里，而不是由决策函数做 ——
	//    决策函数必须是纯函数，不能有副作用。
	if d.Next == sandboxv1alpha1.PhaseProvisioning && sbx.Status.PodName == "" {
		if err := r.ensurePod(ctx, sbx, tmpl); err != nil {
			return err
		}
	}

	// 3. 首次就绪时间用于冷启动延迟 SLO（P95 ≤ 2.5s）。
	if d.Next == sandboxv1alpha1.PhaseReady || d.Next == sandboxv1alpha1.PhaseRunning {
		markProvisioned(sbx, r.now())
	}

	// 4. 写 status。
	sbx.Status.Phase = d.Next
	sbx.Status.ObservedGeneration = sbx.Generation
	meta.SetStatusCondition(&sbx.Status.Conditions, metav1.Condition{
		Type:               conditionTypeFor(d),
		Status:             metav1.ConditionTrue,
		Reason:             string(d.Reason),
		Message:            d.Message,
		ObservedGeneration: sbx.Generation,
	})
	return r.Status().Update(ctx, sbx)
}

// deleteSandbox 删除 AgentSandbox，交由 Finalizer 链完成实际清理。
//
// 幂等是硬要求：崩溃恢复路径会重复调用它。
func (r *SandboxReconciler) deleteSandbox(ctx context.Context, sbx *sandboxv1alpha1.AgentSandbox) error {
	// 显式指定 Background 传播：默认传播策略会随对象带不带 finalizer 变化，
	// 而删除时机在这里必须是确定的（background 不会阻塞本对象的删除）。
	err := r.Delete(ctx, sbx, client.PropagationPolicy(metav1.DeletePropagationBackground))
	switch {
	case err == nil, apierrors.IsNotFound(err):
		return nil
	case apierrors.IsConflict(err):
		// 对象已被别人改动（如 gateway 刚刚写入 claim）。
		// 交给下一轮：重读后重新决策比强行覆盖安全。
		return nil
	default:
		return fmt.Errorf("删除沙箱失败: %w", err)
	}
}

// conditionTypeFor 把决策映射到 Condition 类型。
func conditionTypeFor(d Decision) string {
	switch d.Next {
	case sandboxv1alpha1.PhaseProvisioning, sandboxv1alpha1.PhaseReady, sandboxv1alpha1.PhaseRunning:
		return sandboxv1alpha1.CondPodReady
	case sandboxv1alpha1.PhaseHibernating, sandboxv1alpha1.PhaseHibernated:
		return sandboxv1alpha1.CondHibernated
	default:
		return sandboxv1alpha1.CondClaimed
	}
}

// ensureFinalizers 一次性写入完整的 Finalizer 链。
//
// 一次写全而不是逐个添加：Finalizer 链的**顺序**是设计的一部分
// （先 Lease 后网络、最后才落盘），分次添加会让顺序取决于时序，
// 从而出现"先落盘后断网"这种会泄露数据的顺序。
func (r *SandboxReconciler) ensureFinalizers(ctx context.Context, sbx *sandboxv1alpha1.AgentSandbox) (ctrl.Result, error) {
	for _, f := range finalizerOrder {
		controllerutil.AddFinalizer(sbx, f)
	}
	if err := r.Update(ctx, sbx); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// finalizerOrder 是 Finalizer 链的顺序。
var finalizerOrder = []string{
	FinalizerLeaseCleanup,
	FinalizerNetworkCleanup,
	FinalizerStateFlush,
	FinalizerVolumeCleanup,
	FinalizerMetricsFinalize,
}

// reconcileDelete 执行 Finalizer 链。
func (r *SandboxReconciler) reconcileDelete(ctx context.Context, sbx *sandboxv1alpha1.AgentSandbox) (ctrl.Result, error) {
	for _, name := range finalizerOrder {
		if !controllerutil.ContainsFinalizer(sbx, name) {
			continue
		}

		stepCtx, cancel := context.WithTimeout(ctx, finalizerStepTimeout)
		err := r.runFinalizerStep(stepCtx, name, sbx)
		cancel()

		if err != nil {
			// 关键：**不返回 error**。返回 error 会让删除流程被永久锁死，
			// 只能人工介入；而泄漏的资源有对账 Sweeper 兜底（docs/04 §6.3）。
			r.Recorder.Eventf(sbx, corev1.EventTypeWarning, "FinalizerFailed",
				"%s: %v（继续推进，泄漏由 Sweeper 兜底）", name, err)
		}

		controllerutil.RemoveFinalizer(sbx, name)
		// 逐个持久化，保证删除过程可断点续行：即使本轮崩溃，
		// 已完成的步骤不会重做。
		if err := r.Update(ctx, sbx); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// runFinalizerStep 执行单个清理步骤。
//
// 每一步的实现都假设自己可能被**重复调用**（删除流程可断点续行），
// 因此全部写成"确保状态"而非"执行动作"：删除不存在的对象返回 nil。
func (r *SandboxReconciler) runFinalizerStep(ctx context.Context, name string, sbx *sandboxv1alpha1.AgentSandbox) error {
	switch name {
	case FinalizerLeaseCleanup:
		return r.finalizeLeaseCleanup(ctx, sbx)
	case FinalizerNetworkCleanup:
		return r.finalizeNetworkCleanup(ctx, sbx)
	case FinalizerStateFlush:
		return r.finalizeStateFlush(ctx, sbx)
	case FinalizerVolumeCleanup:
		return r.finalizeVolumeCleanup(ctx, sbx)
	case FinalizerMetricsFinalize:
		return r.finalizeMetricsFinalize(ctx, sbx)
	default:
		// 未知 finalizer 不得让删除流程卡住：它不是本版本注入的，
		// 本版本也无从知道怎么清理它。持绘不解比持绘错更危险。
		return nil
	}
}

// ensurePod 创建沙箱 Pod。
//
// 骨架实现：只落最小可运行的 Pod（镜像 + 档位资源 + RuntimeClass + 节点约束）。
// M0/M1 需要补齐的部分已在下方标注。
func (r *SandboxReconciler) ensurePod(
	ctx context.Context,
	sbx *sandboxv1alpha1.AgentSandbox,
	tmpl *sandboxv1alpha1.SandboxTemplate,
) error {
	if tmpl == nil {
		// 模板缺失是**硬错误**，不是"用默认值继续"。
		// 模板决定了镜像与出口档位，缺它就没法创建一个正确的沙箱；
		// 用占位值建出来的 Pod 会是一个既连不上也不受出口管制的东西 ——
		// 那比不创建危险得多。
		return fmt.Errorf("模板 %q 不存在", sbx.Spec.TemplateRef.Name)
	}

	// 解析隔离级别 —— 这是本骨架与"写死 RuntimeClass"的关键差别：
	// 同一份 CR 在 kind 上解析为 simulated（回落 runc），在生产解析为 kata-fc，
	// 控制器代码完全不变。降级结果必须写进 status，绝不能静默。
	av, err := r.Resolver.Resolve(ctx, sbx.Spec.Isolation)
	if err != nil {
		return fmt.Errorf("解析隔离级别失败: %w", err)
	}
	if !av.Available {
		r.Recorder.Eventf(sbx, corev1.EventTypeWarning, "IsolationUnavailable",
			"%s: %s", av.Reason, av.Message)
		return nil // 不创建 Pod，等待运行时可用或被人工处理
	}
	if av.Degraded {
		// 降级是安全事件，必须留下两条独立证据：事件 + 条件。
		r.Recorder.Eventf(sbx, corev1.EventTypeWarning, "IsolationDegraded",
			"请求 %s，实际生效 %s", av.RequestedLevel, av.Level)
	}

	// 网络策略在 Pod **之前**创建。
	//
	// NetworkPolicy 是按 label 选 Pod 的声明式对象，后创建也仍然生效；
	// 区别只在于"Pod 已经能收包但策略还没下发"这段窗口的长度。
	// 默认拒绝是要关门，门就应当在房间里有人之前装好。
	if err := r.ensureNetworkPolicy(ctx, sbx); err != nil {
		return err
	}

	labels := map[string]string{
		sandboxv1alpha1.LabelPool:      sbx.Spec.PoolRef.Name,
		sandboxv1alpha1.LabelTier:      sbx.Spec.Tier,
		sandboxv1alpha1.LabelTemplate:  sbx.Spec.TemplateRef.Name,
		sandboxv1alpha1.LabelIsolation: string(av.Level),
		sandboxv1alpha1.LabelRole:      sandboxv1alpha1.RoleSandbox,
		// LabelSandbox 让 Finalizer 与 Sweeper 能用 selector 精确找到
		// "属于这个沙箱"的附属资源。没有它就只能反推命名规则，
		// 而命名规则每改一次都会静默地漏掉一批对象。
		sandboxv1alpha1.LabelSandbox: sbx.Name,
	}
	claimed := "false"
	if sbx.Spec.Claim != nil {
		claimed = "true"
		labels[sandboxv1alpha1.LabelTenant] = sbx.Spec.Claim.RequestedBy.Tenant
	}
	labels[sandboxv1alpha1.LabelClaimed] = claimed

	res := TierResources(sbx.Spec.Tier)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sbx.Name,
			Namespace: sbx.Namespace,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:      "sandbox",
				Image:     tmpl.Spec.Image,
				Command:   tmpl.Spec.Command,
				Resources: res,
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptrFalse(),
					ReadOnlyRootFilesystem:   ptrTrue(),
					RunAsNonRoot:             ptrTrue(),
				},
			}},
			// 沙箱不应持有集群凭据（docs/08 §8.1 的强制基线）。
			AutomountServiceAccountToken: ptrFalse(),
			RestartPolicy:                corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: ptrInt64(int64(
				EffectiveLifecycle(sbx).TerminationGracePeriodSeconds)),
		},
	}
	if av.RuntimeClassName != "" {
		// 空表示使用集群默认运行时（simulated / runc）。
		pod.Spec.RuntimeClassName = &av.RuntimeClassName
	}
	if len(av.Spec.NodeSelector) > 0 {
		pod.Spec.NodeSelector = av.Spec.NodeSelector
	}
	if tmpl.Spec.Scheduling.PriorityClassName != "" {
		pod.Spec.PriorityClassName = tmpl.Spec.Scheduling.PriorityClassName
	}

	// OwnerReference 让 Pod 随 CR 自动 GC —— 这是泄漏防护的第二层
	// （第一层是 Finalizer 链，第三层是对账 Sweeper）。
	if err := controllerutil.SetControllerReference(sbx, pod, r.Scheme); err != nil {
		return err
	}

	if err := r.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("创建沙箱 Pod 失败: %w", err)
	}

	sbx.Status.PodName = pod.Name
	sbx.Status.RuntimeClassName = av.RuntimeClassName
	sbx.Status.IsolationLevel = av.Level
	return nil
}

// templateFor 读取沙箱引用的模板。
//
// 取不到时返回 nil 而不是错误：模板可能正在被（重新）创建，
// 而沙箱此时仍应能被正确回收 —— 用"模板读不到"当作"不能决策"，
// 会让一个模板问题升级成所有引用它的沙箱都无法回收。
func (r *SandboxReconciler) templateFor(
	ctx context.Context,
	sbx *sandboxv1alpha1.AgentSandbox,
) *sandboxv1alpha1.SandboxTemplate {
	var tmpl sandboxv1alpha1.SandboxTemplate
	if err := r.Get(ctx, types.NamespacedName{Name: sbx.Spec.TemplateRef.Name}, &tmpl); err != nil {
		logf.FromContext(ctx).V(1).Info("读取模板失败，本次决策将只使用代码默认值",
			"template", sbx.Spec.TemplateRef.Name, "err", err.Error())
		return nil
	}
	return &tmpl
}

// SetupWithManager 注册控制器。
//
// concurrency 可以设得较大（默认 32）：单沙箱的决策彼此独立，
// 并发度只受 API Server 写入能力限制。但要注意 **PoolController 不能照抄这个值**
// —— 池决策是全局的，并发执行会各自看到同一个缺口并各自扩容，
// 直接导致超额扩容（docs/09 §4）。
func (r *SandboxReconciler) SetupWithManager(mgr ctrl.Manager, concurrency int) error {
	if concurrency <= 0 {
		concurrency = 32
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("agentsandbox").
		For(&sandboxv1alpha1.AgentSandbox{}).
		// Owns 让 Pod 的创建/删除/就绪变化直接驱动 reconcile。
		// 这是把“等待 Pod 就绪”从 2s 轮询变成事件驱动的关键：
		// 冷路径 P95 目标 2.5s，如果靠轮询，光发现就绪就要多花 2s。
		Owns(&corev1.Pod{}). // Lease 的变化同样直接驱动：心跳丢失需要**尽快**被发现。
		// 若只靠 RequeueAfter，识别延迟至少叠加一个轮询周期（30s），
		// 而这个延迟会原封不动地加到"客户端已经死了"到"资源被回收"之间。
		Owns(&coordinationv1.Lease{}).
		// 网络策略被手工删除时要能重新收敛，否则会静默地失去隔离边界。
		Owns(&networkingv1.NetworkPolicy{}).WithOptions(rtctrl.Options{MaxConcurrentReconciles: concurrency}).
		Complete(r)
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func (r *SandboxReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

// sbxLeaseName 是心跳 Lease 的命名约定：与沙箱同名。
// 这样按名 Get 就能命中缓存，无需 List 全部 Lease。
func sbxLeaseName(sbx *sandboxv1alpha1.AgentSandbox) string { return sbx.Name }

func podReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func nodeReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// leaseHealthy 判断心跳是否仍在有效期内。
//
// 容忍期 = Lease 自身的有效期 + HeartbeatGraceSeconds。
// 加这一层缓冲是必要的：客户端与 API Server 之间的瞬时抖动不应被解读为
// "客户端已死"，否则会误杀正在执行的会话。
func leaseHealthy(lease *coordinationv1.Lease, lc Lifecycle, now time.Time) bool {
	renewed := lease.Spec.RenewTime
	if renewed == nil {
		return false
	}
	ttl := time.Duration(15) * time.Second // Lease 默认租期，实际应由 gateway 写入
	if lease.Spec.LeaseDurationSeconds != nil {
		ttl = time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second
	}
	deadline := renewed.Add(ttl + time.Duration(lc.HeartbeatGraceSeconds)*time.Second)
	return now.Before(deadline)
}

func ptrTrue() *bool          { b := true; return &b }
func ptrFalse() *bool         { b := false; return &b }
func ptrInt64(v int64) *int64 { return &v }
