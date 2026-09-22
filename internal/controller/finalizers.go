package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
)

// ErrStateFlusherNotConfigured 表示启用了状态外置却没有配置落盘实现。
//
// 返回错误而不是静默成功是刻意的：状态外置是"内存可归还"的前提，
// 也是业务在 410 Gone 之后恢复会话的唯一依据。静默跳过它会让沙箱
// 看似正常回收，而业务永远拿不回状态 —— 那是一种数据丢失，
// 而且是在最不容易被注意到的地方（删除流程里）发生的。
var ErrStateFlusherNotConfigured = errors.New(
	"sandbox.state.externalize 为 true，但没有配置状态落盘实现（--state-flusher）")

// StateFlusher 把沙箱内的工作区状态落盘到外部存储（L3）。
//
// 这是一个接口而不是具体实现，因为落盘方式取决于环境：
// 生产走对象存储（业务侧 SDK 配合），本地开发没有对象存储。
// 抽象出来后，"启用状态外置但环境不支持"会变成一个**明确的错误**，
// 而不是一个悄悄被跳过的步骤。
type StateFlusher interface {
	// Flush 落盘该沙箱的状态。
	// 返回 nil 表示确认已落盘（或确认无需落盘）。
	Flush(ctx context.Context, sbx *sandboxv1alpha1.AgentSandbox) error
}

// DisabledFlusher 是默认实现：拒绝为启用了状态外置的沙箱服务。
//
// 它不谎报成功。默认实现选择"明确拒绝"而不是"假装落盘"，
// 是因为这两者在生产中的后果差异极大：前者会留下一条 Warning 事件
// 与一次失败的 finalizer 步骤（可被告警捕获），后者会安静地丢掉业务状态。
type DisabledFlusher struct{}

// Flush 实现 StateFlusher。
func (DisabledFlusher) Flush(_ context.Context, sbx *sandboxv1alpha1.AgentSandbox) error {
	if sbx.Spec.State.Externalize {
		return fmt.Errorf("%w (stateURI=%q)", ErrStateFlusherNotConfigured, sbx.Spec.State.StateURI)
	}
	return nil
}

// finalizeLeaseCleanup 删除心跳 Lease。
//
// 顺序上排第一：Lease 是业务侧判断"沙箱是否还活着"的信号，
// 先释放它能让客户端尽快失败转移，而不是等 TTL 超时。
func (r *SandboxReconciler) finalizeLeaseCleanup(ctx context.Context, sbx *sandboxv1alpha1.AgentSandbox) error {
	var lease coordinationv1.Lease
	err := r.Get(ctx, types.NamespacedName{Namespace: sbx.Namespace, Name: sbxLeaseName(sbx)}, &lease)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return client.IgnoreNotFound(r.Delete(ctx, &lease))
}

// finalizeNetworkCleanup 删除该沙箱的网络策略。
//
// 先断网再落盘（docs/04 §6.2）：否则落盘过程中沙箱仍可对外发送数据。
// 这里只清理核心 NetworkPolicy；Cilium 的 CNP 在 M2 引入 Cilium 后一并纳管
// （见 docs/06 §7），在那之前不假装清理了一个并不存在的对象。
func (r *SandboxReconciler) finalizeNetworkCleanup(ctx context.Context, sbx *sandboxv1alpha1.AgentSandbox) error {
	var list networkingv1.NetworkPolicyList
	if err := r.List(ctx, &list,
		client.InNamespace(sbx.Namespace),
		client.MatchingLabels{
			sandboxv1alpha1.LabelSandbox: sbx.Name,
			sandboxv1alpha1.LabelRole:    sandboxv1alpha1.RoleNetpol,
		},
	); err != nil {
		return fmt.Errorf("列出网络策略失败: %w", err)
	}
	return r.deleteAll(ctx, &list)
}

// finalizeStateFlush 触发状态外置落盘。
//
// 位置在断网之后、删卷之前 —— 此时工作区仍可读，但已经无法外泄（docs/04 §6.2）。
func (r *SandboxReconciler) finalizeStateFlush(ctx context.Context, sbx *sandboxv1alpha1.AgentSandbox) error {
	flusher := r.StateFlusher
	if flusher == nil {
		flusher = DisabledFlusher{}
	}
	return flusher.Flush(ctx, sbx)
}

// finalizeVolumeCleanup 删除沙箱的数据卷。
//
// 用 label selector 找而不是反推命名规则：清理逻辑一旦依赖命名规则，
// 任何一次命名调整都会静默地漏掉一批卷 —— 而漏掉的正是最花钱的那部分资源。
func (r *SandboxReconciler) finalizeVolumeCleanup(ctx context.Context, sbx *sandboxv1alpha1.AgentSandbox) error {
	var list corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &list,
		client.InNamespace(sbx.Namespace),
		client.MatchingLabels{
			sandboxv1alpha1.LabelSandbox: sbx.Name,
			sandboxv1alpha1.LabelRole:    sandboxv1alpha1.RoleDataVolume,
		},
	); err != nil {
		return fmt.Errorf("列出数据卷失败: %w", err)
	}
	return r.deleteAll(ctx, &list)
}

// finalizeMetricsFinalize 确认 Pod 已消失，并记录最终用量。
//
// Pod 本身由 ownerReference 自动 GC，理论上无需 finalizer。这里再确认一次
// 是为了**可观测性**：如果 Pod 在 CR 删除后仍然存在，那是一个必须被看见的
// 异常（GC 失效、或有人手工移除了 ownerReference），而不是可以忽略的细节。
// 它也正是对账 Sweeper 的输入之一。
func (r *SandboxReconciler) finalizeMetricsFinalize(ctx context.Context, sbx *sandboxv1alpha1.AgentSandbox) error {
	if sbx.Status.PodName != "" {
		gone, err := r.podGone(ctx, sbx)
		if err != nil {
			return err
		}
		if !gone {
			// ctx 已被 finalizerStepTimeout 限定，因此这里的等待是**有界**的。
			// 有界很重要：无限等待会把删除流程卡死，而对账 Sweeper 能兜住泄漏。
			return fmt.Errorf("Pod %q 在最终确认时仍存在（GC 可能失效，泄漏由 Sweeper 兜底）",
				sbx.Status.PodName)
		}
	}

	// 最终用量以事件形式落地。选择事件而不是写回 status：status 会随对象一起
	// 消失，事件会进入集群事件流，可被采集端抓走做成本核算。
	r.Recorder.Eventf(sbx, corev1.EventTypeNormal, "Reclaimed",
		"回收完成: reason=%s claimedCount=%d coldPath=%v phase=%s",
		sbx.Status.Metrics.RecycleReason, sbx.Status.Metrics.ClaimedCount,
		sbx.Status.Metrics.ColdPath, sbx.Status.Phase)
	return nil
}

// podGone 判断沙箱 Pod 是否已经不存在（带重试，直到 ctx 超时）。
//
// 删除是异步的，删完立刻 Get 多半还能查到。这里做一个小步长轮询：
// 它比"立刻判定仍存在"更准确，又比"永久等待"更安全。
func (r *SandboxReconciler) podGone(ctx context.Context, sbx *sandboxv1alpha1.AgentSandbox) (bool, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	key := types.NamespacedName{Namespace: sbx.Namespace, Name: sbx.Status.PodName}
	for {
		var pod corev1.Pod
		err := r.Get(ctx, key, &pod)
		switch {
		case apierrors.IsNotFound(err):
			return true, nil
		case err != nil:
			return false, err
		}
		select {
		case <-ctx.Done():
			return false, nil
		case <-ticker.C:
		}
	}
}

// deleteAll 删除列表中的全部对象，逐个容忍 NotFound。
func (r *SandboxReconciler) deleteAll(ctx context.Context, list client.ObjectList) error {
	items, err := extractItems(list)
	if err != nil {
		return err
	}
	for _, obj := range items {
		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("删除 %s/%s 失败: %w", obj.GetNamespace(), obj.GetName(), err)
		}
	}
	return nil
}

// extractItems 把受支持的 List 拆成对象切片。
//
// 用类型 switch 而不是反射：需要清理的对象类型会随里程碑增加
// （NetworkPolicy、PVC，未来还有 CNP / Secret）。用一个显式的 switch，
// 新类型忘记加进来时会**编译并运行到默认分支报错**，
// 而反射写法会安静地返回空列表 —— 也就是悄悄漏掉一批资源。
func extractItems(list client.ObjectList) ([]client.Object, error) {
	switch l := list.(type) {
	case *networkingv1.NetworkPolicyList:
		out := make([]client.Object, 0, len(l.Items))
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
		return out, nil
	case *corev1.PersistentVolumeClaimList:
		out := make([]client.Object, 0, len(l.Items))
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
		return out, nil
	default:
		return nil, fmt.Errorf("extractItems: 未支持的列表类型 %T", list)
	}
}

// ensureNetworkPolicy 为沙箱建立默认拒绝的入站策略。
//
// 默认拒绝 + 显式放通 gateway，而不是"先开着以后再收紧"：
// 后者在实践中的结果几乎总是"以后再也没收紧过"。这里哪怕只放通一条，
// 也已经把"任何同集群 Pod 都能连进沙箱"这个默认行为关掉了。
//
// 出站治理（LLM 白名单等）依赖 Cilium 的 FQDN 能力，属于 M2（docs/06 §7）。
// 在那之前这里不写一条"看起来治理了出口其实没生效"的规则 ——
// 那比没有规则更危险，因为它会让人以为已经防住了。
func (r *SandboxReconciler) ensureNetworkPolicy(ctx context.Context, sbx *sandboxv1alpha1.AgentSandbox) error {
	name := sbx.Name
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: sbx.Namespace,
			Labels: map[string]string{
				sandboxv1alpha1.LabelSandbox: name,
				sandboxv1alpha1.LabelRole:    sandboxv1alpha1.RoleNetpol,
				sandboxv1alpha1.LabelPool:    sbx.Spec.PoolRef.Name,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{
				sandboxv1alpha1.LabelSandbox: name,
				sandboxv1alpha1.LabelRole:    sandboxv1alpha1.RoleSandbox,
			}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						"kubernetes.io/metadata.name": sandboxv1alpha1.NamespaceSystem,
					}},
				}},
			}},
		},
	}
	if err := controllerutil.SetControllerReference(sbx, np, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, np); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("创建网络策略失败: %w", err)
	}
	return nil
}
