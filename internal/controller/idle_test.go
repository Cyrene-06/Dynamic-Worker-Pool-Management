package controller

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
)

// TestIdleFor_BaselineIsLastActivityNotCreation 是这一批测试里最重要的一个。
//
// 它守住的是一整类"池化彻底失效"的 bug：
//
//	池中库存可能躺了很久才被认领。若空闲判定以 CR 的创建时间为基准，
//	一个在池里待了 1 小时的库存会在被认领后的**第一次** reconcile
//	就被判定"已空闲 1 小时"并立即回收。
//
// 症状是"刚认领就被销毁"—— 而日志上只有一次看起来完全正常的 IdleTimeout 回收，
// 没有任何报错。这正是本项目最核心的机制（池化）被整体废掉的形态。
func TestIdleFor_BaselineIsLastActivityNotCreation(t *testing.T) {
	// 库存躺了 1 小时
	const poolDwell = time.Hour
	sbx := newSbx(sandboxv1alpha1.PhaseReady, poolDwell)
	sbx.Spec.Claim = nil

	// 刚刚被认领 5 秒前
	claimedAt := metav1.NewTime(t0.Add(-5 * time.Second))
	sbx.Status.ClaimRef = &sandboxv1alpha1.ClaimStatus{
		Tenant:    "t-1",
		RequestID: "req-1",
		ClaimedAt: &claimedAt,
	}
	sbx.Status.Phase = sandboxv1alpha1.PhaseRunning
	sbx.Spec.Claim = &sandboxv1alpha1.ClaimSpec{
		RequestedBy: sandboxv1alpha1.ClaimRequestedBy{Tenant: "t-1", RequestID: "req-1"},
	}

	o := healthy()
	o.Now = t0

	got := idleFor(sbx, o)
	if got != 5*time.Second {
		t.Fatalf("idleFor = %v，期望 5s（应以认领时间为基准，而不是创建时间 %v）", got, poolDwell)
	}

	// 而一个刚刚认领的沙箱绝不能被判成空闲 —— 这是端到端的表述。
	d := NextPhase(sbx, o)
	if d.Reclaim {
		t.Fatalf("刚认领 5s 的沙箱被判定回收（reason=%s），池化会因此在认领当轮失效", d.Reason)
	}
}

// TestIdleFor_LeaseRenewalCountsAsActivity 守住"续租必须真的起作用"。
//
// 不把 Lease 续租算作活动的话，客户端即使每 20 秒续租一次，
// 沙箱仍会在 idleTimeout（默认 300s）后被当作空闲回收 ——
// 心跳机制看起来在工作（Lease 一直在续），却完全不影响回收决策。
func TestIdleFor_LeaseRenewalCountsAsActivity(t *testing.T) {
	// 沙箱创建于 1 小时前，认领于 1 小时前，但 10 秒前刚续租过。
	sbx := newSbx(sandboxv1alpha1.PhaseRunning, time.Hour)
	claimedAt := metav1.NewTime(t0.Add(-time.Hour))
	sbx.Status.ClaimRef = &sandboxv1alpha1.ClaimStatus{
		Tenant:    "t-1",
		RequestID: "req-1",
		ClaimedAt: &claimedAt,
	}

	o := healthy()
	o.Now = t0
	o.LastLeaseRenewAt = t0.Add(-10 * time.Second)
	quiet(&o)

	got := idleFor(sbx, o)
	if got != 10*time.Second {
		t.Fatalf("idleFor = %v，期望 10s（应以最近一次续租为基准）", got)
	}

	// 续租中的沙箱不该被回收，也不该被冻结。
	d := NextPhase(sbx, o)
	if d.Reclaim {
		t.Fatalf("持续续租的沙箱被判回收（reason=%s）", d.Reason)
	}
	if d.Next == sandboxv1alpha1.PhaseHibernating {
		t.Fatalf("持续续租的沙箱被判冻结；冻结一个活跃会话会直接让业务不可用")
	}
}

// TestIdleFor_ActivitySignalsAreMonotonic 守住"取最大值"这个语义。
//
// 四个来源中任何一个更晚，都应当成为基准。若实现写成 if/else 链，
// 就会出现"有 LastActiveAt 时忽略更晚的续租时间"这类顺序依赖的错误 ——
// 而那种错误只在特定信号组合下出现，几乎不可能靠手工测试发现。
func TestIdleFor_ActivitySignalsAreMonotonic(t *testing.T) {
	sbx := newSbx(sandboxv1alpha1.PhaseRunning, time.Hour)

	older := metav1.NewTime(t0.Add(-40 * time.Minute))
	sbx.Status.ClaimRef = &sandboxv1alpha1.ClaimStatus{ClaimedAt: &older}
	mid := metav1.NewTime(t0.Add(-20 * time.Minute))
	sbx.Status.Activity.LastActiveAt = &mid

	o := healthy()
	o.Now = t0

	// 续租时间比 LastActiveAt 更晚 → 以续租为准。
	o.LastLeaseRenewAt = t0.Add(-30 * time.Second)
	if got := idleFor(sbx, o); got != 30*time.Second {
		t.Fatalf("续租更晚时 idleFor = %v，期望 30s", got)
	}

	// 续租时间比 LastActiveAt 更早 → 仍以 LastActiveAt 为准。
	o.LastLeaseRenewAt = t0.Add(-50 * time.Minute)
	if got := idleFor(sbx, o); got != 20*time.Minute {
		t.Fatalf("续租更早时 idleFor = %v，期望 20m（应取最晚的那个信号）", got)
	}

	// 时钟回退（NTP 校正、节点漂移）不能产生负数。
	o.LastLeaseRenewAt = t0.Add(5 * time.Minute)
	if got := idleFor(sbx, o); got != 0 {
		t.Fatalf("时钟超前时 idleFor = %v，期望 0（不能返回负值）", got)
	}
}

// TestReuseLimitReached_RotatesStock 守重复用上限。
//
// 上限的判定必须发生在**库存空闲时**：在认领之后判定会变成
// "把沙箱交给业务、再抢回来"，那是一次业务可感知的中断。
func TestReuseLimitReached_RotatesStock(t *testing.T) {
	tmpl := &sandboxv1alpha1.SandboxTemplate{}
	tmpl.Spec.Defaults.MaxClaimCount = 2

	sbx := newSbx(sandboxv1alpha1.PhaseReady, time.Minute)
	sbx.Status.Metrics.ClaimedCount = 2

	lc := ApplyTemplateDefaults(sbx, tmpl)
	if !reuseLimitReached(lc, sbx) {
		t.Fatalf("claimedCount=2 达到 maxClaimCount=2，应判定需要轮换")
	}

	o := healthy()
	o.Now = t0
	// 必须走 NextPhaseWithLifecycle：模板取值不在沙箱对象上，
	// 用 NextPhase 会内部重算成零值 —— 那正是这条测试要防的失效。
	d := NextPhaseWithLifecycle(sbx, lc, o)
	if d.Next != sandboxv1alpha1.PhaseTerminating {
		t.Fatalf("Next = %q，期望 Terminating（超限库存应被销毁重建）", d.Next)
	}
	if d.RecycleReason != sandboxv1alpha1.RecycleReuseLimitReached {
		t.Fatalf("RecycleReason = %q，期望 %q", d.RecycleReason, sandboxv1alpha1.RecycleReuseLimitReached)
	}

	// 未达上限时不应轮换。
	sbx.Status.Metrics.ClaimedCount = 1
	lc = ApplyTemplateDefaults(sbx, tmpl)
	if reuseLimitReached(lc, sbx) {
		t.Fatalf("claimedCount=1 未达上限，不应轮换")
	}

	// 上限为 0（未配置）表示不限制。默认必须是不限制：
	// 把默认设成 1 会让池化收益归零（每次复用都要重建），
	// 而这是一个性能开关，不是安全开关（安全边界是 resetHook）。
	lc = ApplyTemplateDefaults(sbx, &sandboxv1alpha1.SandboxTemplate{})
	if lc.MaxClaimCount != 0 || reuseLimitReached(lc, sbx) {
		t.Fatalf("未配置 maxClaimCount 时应不限制，实际 MaxClaimCount=%d", lc.MaxClaimCount)
	}
}

// TestDecideProvisioning_EveryWaitBranchTimesOut 守住 Provisioning 的超时覆盖。
//
// 原实现把超时判定写在 PodPending 的分支里，于是两条最需要兜底的路径
// 反而没有超时：Pod 始终没被创建，以及 Pod 起来了但就绪探针永不通过。
// 这两种情况下状态机无限返回 Stay，表现出来就是"沙箱永远卡在 Provisioning"，
// 而且没有任何报错 —— 它看起来只是在等。
func TestDecideProvisioning_EveryWaitBranchTimesOut(t *testing.T) {
	lc := EffectiveLifecycle(nil)
	timeout := time.Duration(lc.ProvisionTimeoutSeconds) * time.Second

	cases := []struct {
		name   string
		mutate func(*Observation)
	}{
		{
			name:   "Pod 从未被创建",
			mutate: func(o *Observation) { o.PodExists = false },
		},
		{
			name: "Pod 运行中但永不就绪",
			mutate: func(o *Observation) {
				o.PodExists = true
				o.PodReady = false
				o.PodPhase = corev1.PodRunning
			},
		},
		{
			name: "Pod 一直 Pending（调度不上）",
			mutate: func(o *Observation) {
				o.PodExists = true
				o.PodPhase = corev1.PodPending
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sbx := newSbx(sandboxv1alpha1.PhaseProvisioning, timeout+time.Minute)
			sbx.Spec.Claim = nil

			o := healthy()
			o.Now = t0
			o.PodCreatedAt = t0.Add(-timeout - time.Minute)
			tc.mutate(&o)

			d := NextPhase(sbx, o)
			if !d.Reclaim || d.Next != sandboxv1alpha1.PhaseFailed {
				t.Fatalf("Next = %q Reclaim = %v，期望 Failed+Reclaim（该分支没有超时兜底）",
					d.Next, d.Reclaim)
			}
			if d.Reason != ReasonProvisionTimeout {
				t.Fatalf("Reason = %q，期望 %q", d.Reason, ReasonProvisionTimeout)
			}

			// 反向：未超时时必须继续等待，不能过早判失败 ——
			// 冷路径正常要花 0.8–2.3s，过早判失败会让每次申请都重建。
			fresh := healthy()
			fresh.Now = t0
			fresh.PodCreatedAt = t0.Add(-time.Second)
			tc.mutate(&fresh)
			if d := NextPhase(sbx, fresh); d.Reclaim {
				t.Fatalf("未超时就判失败（reason=%s）", d.Reason)
			}
		})
	}
}

// TestDecideClaimed_ManualHibernateAndWake 守人工干预。
//
// 不把人工请求接进状态机的话，gateway 的 :hibernate / :wake
// 就是两个返回 200 却什么都不做的接口 —— 那种"看起来成功了"的
// 空操作比明确的 501 危险得多。
func TestDecideClaimed_ManualHibernateAndWake(t *testing.T) {
	// 人工休眠：即使没有任何空闲迹象，也应当进入冻结。
	sbx := newSbx(sandboxv1alpha1.PhaseRunning, time.Minute)
	o := healthy()
	o.Now = t0
	o.ManualHibernate = true

	if d := NextPhase(sbx, o); d.Next != sandboxv1alpha1.PhaseHibernating {
		t.Fatalf("人工休眠请求未生效：Next = %q", d.Next)
	}

	// 人工唤醒：休眠态下应当开始唤醒。
	hib := newSbx(sandboxv1alpha1.PhaseHibernated, time.Minute)
	hib.Status.Hibernation.State = sandboxv1alpha1.RuntimeFrozen
	o2 := healthy()
	o2.Now = t0
	o2.Frozen = true
	o2.ManualWake = true
	quiet(&o2)

	if d := NextPhase(hib, o2); d.Next != sandboxv1alpha1.PhaseResuming {
		t.Fatalf("人工唤醒请求未生效：Next = %q", d.Next)
	}

	// 冻结状态下空闲超时仍然必须真的回收：冻结只释放了 CPU，内存还在占用。
	// 若冻结会"吃掉"回收时机，内存就永远不归还，而状态看起来一切正常。
	hib2 := newSbx(sandboxv1alpha1.PhaseHibernated, time.Hour)
	hib2.Status.Hibernation.State = sandboxv1alpha1.RuntimeFrozen
	o3 := healthy()
	o3.Now = t0
	o3.Frozen = true
	quiet(&o3)
	d := NextPhase(hib2, o3)
	if !d.Reclaim || d.Next != sandboxv1alpha1.PhaseTerminating {
		t.Fatalf("休眠态超过空闲上限却未回收：Next = %q Reclaim = %v", d.Next, d.Reclaim)
	}
	if d.RecycleReason != sandboxv1alpha1.RecycleIdleTimeout {
		t.Fatalf("RecycleReason = %q，期望 %q", d.RecycleReason, sandboxv1alpha1.RecycleIdleTimeout)
	}
}
