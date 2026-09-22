package controller

import (
	"context"
	"fmt"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
)

// HeartbeatLeaseDurationSeconds 是心跳 Lease 自身的有效期。
//
// 语义：客户端必须在 renewTime + 本值 之前续租一次。
// 60s 与 docs/04 的 `renewIntervalSeconds: 60` 对应，客户端通常在
// 还剩一半时间时就续租，因此能容忍一次网络抖动而不触发误判。
//
// 刻意不做成配置项：它是**客户端与平台的契约**（业务 SDK 按它决定续租频率），
// 一旦可配就会出现"平台调短了、业务没跟着调"的静默失配，
// 表现为随机时刻的沙箱被误回收。需要不同节奏应通过模板另立契约。
const HeartbeatLeaseDurationSeconds int32 = 60

// ensureLease 为已认领的沙箱建立心跳 Lease。
//
// # 为什么由控制器创建，而不是像时序图那样由 gateway 创建
//
// gateway 的 CAS 认领与 Lease 创建是**两次独立的写**。中间任何一次失败
// （进程被调度掉、网络抖动、API Server 限流）都会留下"有 claim 但没有 Lease"
// 的中间状态，而控制器读不到 Lease 时只能判定"心跳丢失"——
// 于是它会亲手误杀一个刚刚成功认领、客户端正在建立连接的沙箱。
//
// 分工由此变得清晰，且每一步都幂等：
//
//   - 控制器只负责**创建**（这是收敛动作："应该存在" → "让它存在"）
//   - gateway 只负责**续租**（这是入站信号：只有客户端知道自己还活着）
//   - 控制器只**读**续租时间做决策，**绝不回写 renewTime**
//
// 最后一条是硬约束。若控制器也续租，Lease 就变成了"控制器还活着"的证据，
// 而不是"客户端还活着"的证据 —— 用错误信号做决策比没有信号更糟。
func (r *SandboxReconciler) ensureLease(
	ctx context.Context,
	sbx *sandboxv1alpha1.AgentSandbox,
	now time.Time,
) error {
	key := types.NamespacedName{Namespace: sbx.Namespace, Name: sbxLeaseName(sbx)}

	var existing coordinationv1.Lease
	err := r.Get(ctx, key, &existing)
	switch {
	case err == nil:
		// 已存在：此后 renewTime 归 gateway 与客户端所有，这里什么都不做。
		return nil
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("读取 Lease 失败: %w", err)
	}

	// 库存没有心跳：未认领的沙箱不应该有"存活证明"。
	if sbx.Spec.Claim == nil {
		return nil
	}

	renewed := metav1.NewMicroTime(now)
	duration := HeartbeatLeaseDurationSeconds
	holder := sbx.Spec.Claim.RequestedBy.Tenant
	if id := sbx.Spec.Claim.RequestedBy.SessionID; id != "" {
		holder = holder + "/" + id
	}

	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
			Labels: map[string]string{
				sandboxv1alpha1.LabelSandbox: key.Name,
				sandboxv1alpha1.LabelPool:    sbx.Spec.PoolRef.Name,
				sandboxv1alpha1.LabelTenant:  sbx.Spec.Claim.RequestedBy.Tenant,
			},
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &holder,
			LeaseDurationSeconds: &duration,
			// 初始续租时间取认领时刻：这样"从未续租过的客户端"
			// 也会在 leaseDuration + grace 之后被正确判为离线，
			// 而不是得到一个无限期的宽限。
			RenewTime: &renewed,
			// AcquireTime 只在首次获取时写，用于区分"从未续租"与"续租后失联"。
			AcquireTime: &renewed,
		},
	}
	if err := controllerutil.SetControllerReference(sbx, lease, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, lease); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("创建心跳 Lease 失败: %w", err)
	}
	return nil
}

// leaseRenewTime 提取 Lease 的续租时间。
//
// 缺 RenewTime 时返回零值而不是"现在"：只有一个字段为空的 Lease 说明
// 写入方中途失败了。把它当成"刚刚续租过"会让一个已经死掉的客户端
// 无限期地被认为活着 —— 那是最不该出现的失败方向。
func leaseRenewTime(lease *coordinationv1.Lease) time.Time {
	if lease == nil || lease.Spec.RenewTime == nil {
		return time.Time{}
	}
	return lease.Spec.RenewTime.Time
}
