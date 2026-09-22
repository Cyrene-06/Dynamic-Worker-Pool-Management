package sweeper

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
)

// apply 执行一条发现。
//
// 每条动作都必须**幂等**：对账是定期运行且可能重入的，一个不幂等的动作
// （比如"删掉再报告失败"）会在下一轮产生新的副作用，最终变成不可预期的行为。
func (s *Sweeper) apply(ctx context.Context, f Finding, idx *objectIndex, now time.Time) error {
	switch f.Action {
	case ActionBlock:
		return s.markBlocked(ctx, f, idx, now)
	case ActionDeleteObject:
		return s.deleteObject(ctx, f, idx)
	case ActionForceFinalize:
		return s.forceFinalize(ctx, f, idx)
	case ActionMarkFailed:
		return s.markFailed(ctx, f, idx)
	default:
		// 未知动作不静默忽略：它说明 Classify 与 apply 的枚举不同步，
		// 而这种不同步的后果是"报告说会处理、实际什么都没发生"。
		return fmt.Errorf("未知动作 %q", f.Action)
	}
}

// markBlocked 打上"首次被判为异常"的时间标记，为下一轮的删除建立宽限期。
//
// 已经标记过的对象不做任何写入：反复刷新标记会让宽限期永远走不完，
// 于是对账永远不会真正清理任何东西 —— 那是一种"看起来很忙的失效"。
func (s *Sweeper) markBlocked(ctx context.Context, f Finding, idx *objectIndex, now time.Time) error {
	obj := idx.lookup(f)
	if obj == nil {
		// 对象已经消失（例如控制器自己把它清理掉了）。这是最好的结果，
		// 不是错误。
		return nil
	}
	if _, ok := obj.GetAnnotations()[sandboxv1alpha1.AnnoBlockedSince]; ok {
		return nil
	}
	base := obj.DeepCopyObject().(client.Object)
	annos := obj.GetAnnotations()
	if annos == nil {
		annos = map[string]string{}
	}
	annos[sandboxv1alpha1.AnnoBlockedSince] = now.UTC().Format(time.RFC3339)
	obj.SetAnnotations(annos)
	if err := s.Client.Patch(ctx, obj, client.MergeFrom(base)); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("标记异常起始时间失败: %w", err)
	}
	return nil
}

// deleteObject 删除孤儿对象。
func (s *Sweeper) deleteObject(ctx context.Context, f Finding, idx *objectIndex) error {
	obj := idx.lookup(f)
	if obj == nil {
		return nil
	}
	err := s.Client.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground))
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("删除失败: %w", err)
	}
	return nil
}

// forceFinalize 移除全部 Finalizer 后删除对象。
//
// 这是对账里唯一会"绕过清理顺序"的动作，因此只在删除流程已卡死超过容忍期时使用。
// 代价说明白：绕过 Finalizer 意味着可能漏掉状态落盘或断网步骤 ——
// 但一个卡死的删除流程会让对象永远存在，那连"下一轮重试"的机会都没有。
// 宁可漏掉一次落盘（业务侧可从硬期限得知），也不能留下一个永久对象。
func (s *Sweeper) forceFinalize(ctx context.Context, f Finding, idx *objectIndex) error {
	obj := idx.lookup(f)
	if obj == nil {
		return nil
	}
	if len(obj.GetFinalizers()) > 0 {
		base := obj.DeepCopyObject().(client.Object)
		obj.SetFinalizers(nil)
		if err := s.Client.Patch(ctx, obj, client.MergeFrom(base)); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("移除 Finalizer 失败: %w", err)
		}
	}
	err := s.Client.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground))
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("删除失败: %w", err)
	}
	return nil
}

// markFailed 把沙箱标记为 Failed。
//
// 刻意**不**直接删除沙箱：标记后由控制器走删除流程，Finalizer 链仍会
// 正常执行（先断网、再落盘、后删卷）。对账绕过 Finalizer 去删沙箱，
// 等于把"清理顺序"这个安全设计整个丢掉 —— 而顺序正是它存在的理由。
func (s *Sweeper) markFailed(ctx context.Context, f Finding, idx *objectIndex) error {
	sbx := idx.sandboxes[key(f.Namespace, f.Name)]
	if sbx == nil {
		return nil
	}
	if sbx.Status.Phase.IsTerminal() {
		return nil
	}

	// 原因码用枚举而不是自由文本：它进指标 label，也进业务可见的 410 响应。
	reason := sandboxv1alpha1.RecycleRuntimeError
	if f.Kind == KindNodeLost {
		reason = sandboxv1alpha1.RecycleNodeLost
	}

	base := sbx.DeepCopy()
	sbx.Status.Phase = sandboxv1alpha1.PhaseFailed
	sbx.Status.Metrics.RecycleReason = reason
	meta.SetStatusCondition(&sbx.Status.Conditions, metav1.Condition{
		Type:               sandboxv1alpha1.CondClaimed,
		Status:             metav1.ConditionFalse,
		Reason:             string(f.Kind),
		Message:            f.Reason,
		ObservedGeneration: sbx.Generation,
	})
	if err := s.Client.Status().Patch(ctx, sbx, client.MergeFrom(base)); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("标记 Failed 失败: %w", err)
	}
	return nil
}

// lookup 把发现定位回具体对象。
func (i *objectIndex) lookup(f Finding) client.Object {
	k := key(f.Namespace, f.Name)
	switch f.Kind {
	case KindOrphanPod:
		return i.pods[k]
	case KindOrphanLease, KindClaimWithoutLease:
		return i.leases[k]
	case KindOrphanNetworkPolicy:
		return i.netpols[k]
	case KindOrphanVolume:
		return i.volumes[k]
	case KindStuckTerminating:
		if sbx := i.sandboxes[k]; sbx != nil {
			return sbx
		}
		return nil
	default:
		return nil
	}
}
