package controller

import (
	"context"
	"fmt"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
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

	o, err := r.observe(ctx, &sbx)
	if err != nil {
		// 观察失败时**不做决策**：把"读不到状态"当成"沙箱异常"会导致误回收。
		return ctrl.Result{}, err
	}

	d := NextPhase(&sbx, *o)

	// 先落 status，再动实际资源。顺序反过来的话，一旦中间失败，
	// 实际资源已经变了但 status 没变，下一轮会做出不一致的决策。
	if d.Next != "" && d.Next != sbx.Status.Phase {
		if err := r.applyTransition(ctx, &sbx, d); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(&sbx, corev1.EventTypeNormal, "PhaseTransition",
			"%s -> %s (%s)", sbx.Status.Phase, d.Next, d.Reason)
		log.Info("phase transition",
			"from", sbx.Status.Phase, "to", d.Next, "reason", string(d.Reason))
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
		o.LeaseHealthy = leaseHealthy(&lease, EffectiveLifecycle(sbx), now)
	case apierrors.IsNotFound(err):
		// 尚未认领的库存没有 Lease，这不算"心跳丢失"。
		o.LeaseHealthy = sbx.Spec.Claim == nil
	default:
		return nil, fmt.Errorf("读取 Lease 失败: %w", err)
	}

	// ---- 活动（辅助判据）----
	// 这里只填已经持久化在 status 里的观测值。实时的 eBPF / cgroup 信号
	// 由节点侧 agent 采集后回写 status，控制器不直接读宿主指标 ——
	// 否则控制器就依赖了具体节点，无法水平扩展。
	o.NetworkActivity = sbx.Status.Activity.ActiveConnections > 0
	o.ActiveConnections = sbx.Status.Activity.ActiveConnections
	o.CPUIncrementMilli = sbx.Status.Activity.P95CPUmilli

	// ---- 休眠 ----
	o.Frozen = sbx.Status.Hibernation.State == sandboxv1alpha1.RuntimeFrozen
	o.Resumed = sbx.Status.Hibernation.State == sandboxv1alpha1.RuntimeActive

	return o, nil
}

// applyTransition 把决策落到 status，并在需要时创建/删除实际资源。
func (r *SandboxReconciler) applyTransition(
	ctx context.Context,
	sbx *sandboxv1alpha1.AgentSandbox,
	d Decision,
) error {
	// 1. 回收类决策：进入 Terminating，后续由 Finalizer 链完成实际清理。
	if d.Reclaim && d.Next == sandboxv1alpha1.PhaseTerminating {
		sbx.Status.Metrics.RecycleReason = d.RecycleReason
	}

	// 2. 需要 Pod 的阶段但 Pod 不存在 → 创建。
	//    Provisioning 的创建动作放在这里，而不是由决策函数做 ——
	//    决策函数必须是纯函数，不能有副作用。
	if d.Next == sandboxv1alpha1.PhaseProvisioning && sbx.Status.PodName == "" {
		if err := r.ensurePod(ctx, sbx); err != nil {
			return err
		}
	}

	// 3. 写 status。
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
// 骨架阶段只实现了 Lease 清理（它最关键：Lease 是业务判断"沙箱是否存活"的依据，
// 不先释放会让客户端一直以为沙箱还在）。其余步骤在 M1 补齐。
func (r *SandboxReconciler) runFinalizerStep(ctx context.Context, name string, sbx *sandboxv1alpha1.AgentSandbox) error {
	switch name {
	case FinalizerLeaseCleanup:
		var lease coordinationv1.Lease
		err := r.Get(ctx, types.NamespacedName{Namespace: sbx.Namespace, Name: sbxLeaseName(sbx)}, &lease)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return client.IgnoreNotFound(r.Delete(ctx, &lease))

	case FinalizerNetworkCleanup:
		// TODO(M1): 删除该沙箱的 CiliumNetworkPolicy / NetworkPolicy。
		return nil

	case FinalizerStateFlush:
		// TODO(M1): 触发状态外置落盘（L3），带超时。
		// 注意顺序：必须在断网之后、删卷之前 —— 此时卷仍可读。
		return nil

	case FinalizerVolumeCleanup:
		// TODO(M1): 清理残余的 ephemeral PVC 与本地卷。
		return nil

	case FinalizerMetricsFinalize:
		// TODO(M1): 上报最终用量与回收原因到外部系统，供成本核算。
		return nil

	default:
		return nil
	}
}

// ensurePod 创建沙箱 Pod。
//
// 骨架实现：只落最小可运行的 Pod（镜像 + 档位资源 + RuntimeClass + 节点约束）。
// M0/M1 需要补齐的部分已在下方标注。
func (r *SandboxReconciler) ensurePod(ctx context.Context, sbx *sandboxv1alpha1.AgentSandbox) error {
	var tmpl sandboxv1alpha1.SandboxTemplate
	if err := r.Get(ctx, types.NamespacedName{Name: sbx.Spec.TemplateRef.Name}, &tmpl); err != nil {
		return fmt.Errorf("读取模板 %q 失败: %w", sbx.Spec.TemplateRef.Name, err)
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

	labels := map[string]string{
		sandboxv1alpha1.LabelPool:      sbx.Spec.PoolRef.Name,
		sandboxv1alpha1.LabelTier:      sbx.Spec.Tier,
		sandboxv1alpha1.LabelTemplate:  sbx.Spec.TemplateRef.Name,
		sandboxv1alpha1.LabelIsolation: string(av.Level),
		sandboxv1alpha1.LabelRole:      sandboxv1alpha1.RoleSandbox,
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
		Owns(&corev1.Pod{}).
		WithOptions(rtctrl.Options{MaxConcurrentReconciles: concurrency}).
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
