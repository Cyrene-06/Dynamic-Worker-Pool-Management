package controller

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
)

// t0 固定时间基准，让所有时间推导可精确复现（不用 time.Now()，
// 否则偶发的边界抖动会变成"本地偶发失败、CI 上不复现"的经典难题）。
var t0 = time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

// newSbx 构造一个生命周期参数为默认值的沙箱对象。
func newSbx(phase sandboxv1alpha1.Phase, age time.Duration) *sandboxv1alpha1.AgentSandbox {
	s := &sandboxv1alpha1.AgentSandbox{}
	s.Name = "sbx-test"
	s.Namespace = "sandbox-pool"
	s.CreationTimestamp = metav1.NewTime(t0.Add(-age))
	s.Status.Phase = phase
	return s
}

// healthy 返回一个"一切正常"的观测。
func healthy() Observation {
	return Observation{
		Now:          t0,
		PodExists:    true,
		PodPhase:     corev1.PodRunning,
		PodReady:     true,
		PodCreatedAt: t0.Add(-50 * time.Second),
		NodeReady:    true,
		LeaseHealthy: true,
	}
}

// quiet 关闭全部活动信号，用于构造"确认空闲"的场景。
func quiet(o *Observation) {
	o.NetworkActivity = false
	o.ActiveConnections = 0
	o.CPUIncrementMilli = 0
}

func TestNextPhase(t *testing.T) {
	tests := []struct {
		name    string
		sbx     *sandboxv1alpha1.AgentSandbox
		mutate  func(*sandboxv1alpha1.AgentSandbox, *Observation)
		want    sandboxv1alpha1.Phase // "" 表示原地不动
		reason  ConditionReason
		reclaim bool
	}{
		{
			name:   "Pending 推进到 Provisioning",
			sbx:    newSbx(sandboxv1alpha1.PhasePending, 5*time.Second),
			want:   sandboxv1alpha1.PhaseProvisioning,
			reason: ReasonPodCreating,
		},
		{
			name:   "Provisioning 且 Pod 就绪、未认领 → Ready（成为库存）",
			sbx:    newSbx(sandboxv1alpha1.PhaseProvisioning, 30*time.Second),
			want:   sandboxv1alpha1.PhaseReady,
			reason: ReasonStockReady,
		},
		{
			name: "Provisioning 且 Pod 就绪、已认领 → Running（冷路径直接持有）",
			sbx:  newSbx(sandboxv1alpha1.PhaseProvisioning, 30*time.Second),
			mutate: func(s *sandboxv1alpha1.AgentSandbox, _ *Observation) {
				s.Spec.Claim = &sandboxv1alpha1.ClaimSpec{}
			},
			want:   sandboxv1alpha1.PhaseRunning,
			reason: ReasonColdPathClaimed,
		},
		{
			name: "Provisioning 且就绪探针未过 → 原地等待",
			sbx:  newSbx(sandboxv1alpha1.PhaseProvisioning, 30*time.Second),
			mutate: func(_ *sandboxv1alpha1.AgentSandbox, o *Observation) {
				o.PodReady = false
			},
			reason: ReasonWaitingPodReady,
		},
		{
			name: "Provisioning 且 Pod 不存在 → 原地等待（不猜测）",
			sbx:  newSbx(sandboxv1alpha1.PhaseProvisioning, 30*time.Second),
			mutate: func(_ *sandboxv1alpha1.AgentSandbox, o *Observation) {
				o.PodExists = false
			},
			reason: ReasonPodMissing,
		},
		{
			name: "Provisioning 且节点失联 → Failed",
			sbx:  newSbx(sandboxv1alpha1.PhaseProvisioning, 30*time.Second),
			mutate: func(_ *sandboxv1alpha1.AgentSandbox, o *Observation) {
				o.NodeReady = false
			},
			want:    sandboxv1alpha1.PhaseFailed,
			reason:  ReasonNodeLost,
			reclaim: true,
		},
		{
			name: "Provisioning 且 Pod 已失败 → Failed",
			sbx:  newSbx(sandboxv1alpha1.PhaseProvisioning, 30*time.Second),
			mutate: func(_ *sandboxv1alpha1.AgentSandbox, o *Observation) {
				o.PodPhase = corev1.PodFailed
			},
			want:    sandboxv1alpha1.PhaseFailed,
			reason:  ReasonPodTerminated,
			reclaim: true,
		},
		{
			name: "Provisioning 超时 → Failed（防止永远卡在中间态）",
			sbx:  newSbx(sandboxv1alpha1.PhaseProvisioning, 200*time.Second),
			mutate: func(_ *sandboxv1alpha1.AgentSandbox, o *Observation) {
				o.PodPhase = corev1.PodPending
				o.PodReady = false
				o.PodCreatedAt = t0.Add(-200 * time.Second)
			},
			want:    sandboxv1alpha1.PhaseFailed,
			reason:  ReasonProvisionTimeout,
			reclaim: true,
		},
		{
			name: "Ready 且被认领 → Running（原地转正）",
			sbx:  newSbx(sandboxv1alpha1.PhaseReady, 30*time.Second),
			mutate: func(s *sandboxv1alpha1.AgentSandbox, _ *Observation) {
				s.Spec.Claim = &sandboxv1alpha1.ClaimSpec{}
			},
			want:   sandboxv1alpha1.PhaseRunning,
			reason: ReasonClaimed,
		},
		{
			name:    "Ready 且库存过老 → Terminating（轮换）",
			sbx:     newSbx(sandboxv1alpha1.PhaseReady, 4000*time.Second),
			want:    sandboxv1alpha1.PhaseTerminating,
			reason:  ReasonStockRotation,
			reclaim: true,
		},
		{
			name: "Ready 且 Pod 消失 → Failed（让池补货）",
			sbx:  newSbx(sandboxv1alpha1.PhaseReady, 30*time.Second),
			mutate: func(_ *sandboxv1alpha1.AgentSandbox, o *Observation) {
				o.PodExists = false
			},
			want:    sandboxv1alpha1.PhaseFailed,
			reason:  ReasonNodeLost,
			reclaim: true,
		},
		{
			name: "Running 且硬期限已到 → Terminating(HardDeadline)",
			sbx:  newSbx(sandboxv1alpha1.PhaseRunning, 60*time.Second),
			mutate: func(s *sandboxv1alpha1.AgentSandbox, _ *Observation) {
				dl := metav1.NewTime(t0.Add(-time.Second))
				s.Status.ClaimRef = &sandboxv1alpha1.ClaimStatus{Tenant: "t-1", HardDeadline: &dl}
			},
			want:    sandboxv1alpha1.PhaseTerminating,
			reason:  ReasonHardDeadline,
			reclaim: true,
		},
		{
			name:    "Running 且 TTL 到期 → Terminating(TTLExpired)",
			sbx:     newSbx(sandboxv1alpha1.PhaseRunning, 8000*time.Second),
			want:    sandboxv1alpha1.PhaseTerminating,
			reason:  ReasonTTLExpired,
			reclaim: true,
		},
		{
			// 硬期限来自业务契约，优先级必须高于平台推导出的 TTL。
			name: "硬期限优先于 TTL",
			sbx:  newSbx(sandboxv1alpha1.PhaseRunning, 8000*time.Second),
			mutate: func(s *sandboxv1alpha1.AgentSandbox, _ *Observation) {
				dl := metav1.NewTime(t0.Add(-time.Second))
				s.Status.ClaimRef = &sandboxv1alpha1.ClaimStatus{Tenant: "t-1", HardDeadline: &dl}
			},
			want:    sandboxv1alpha1.PhaseTerminating,
			reason:  ReasonHardDeadline,
			reclaim: true,
		},
		{
			name: "Running 且节点失联 → Terminating（不等 TTL）",
			sbx:  newSbx(sandboxv1alpha1.PhaseRunning, 60*time.Second),
			mutate: func(_ *sandboxv1alpha1.AgentSandbox, o *Observation) {
				o.NodeReady = false
			},
			want:    sandboxv1alpha1.PhaseTerminating,
			reason:  ReasonNodeLost,
			reclaim: true,
		},
		{
			// 整个状态机最需要克制的一处：信号矛盾时宁可多留一会儿，
			// 也不能误杀一个正在跑长任务的会话。
			name: "心跳丢失但仍观测到网络活动 → 不回收，上报冲突",
			sbx:  newSbx(sandboxv1alpha1.PhaseRunning, 60*time.Second),
			mutate: func(_ *sandboxv1alpha1.AgentSandbox, o *Observation) {
				o.LeaseHealthy = false
				o.NetworkActivity = true
			},
			reason:  ReasonIdleDetectionConflict,
			reclaim: false,
		},
		{
			name: "心跳丢失且确认无活动（Grace）→ 转 Idle 观察",
			sbx:  newSbx(sandboxv1alpha1.PhaseRunning, 60*time.Second),
			mutate: func(_ *sandboxv1alpha1.AgentSandbox, o *Observation) {
				o.LeaseHealthy = false
				quiet(o)
			},
			want:   sandboxv1alpha1.PhaseIdle,
			reason: ReasonHeartbeatLost,
		},
		{
			name: "已在 Idle 且心跳仍未恢复 → Terminating",
			sbx:  newSbx(sandboxv1alpha1.PhaseIdle, 60*time.Second),
			mutate: func(_ *sandboxv1alpha1.AgentSandbox, o *Observation) {
				o.LeaseHealthy = false
				quiet(o)
			},
			want:    sandboxv1alpha1.PhaseTerminating,
			reason:  ReasonHeartbeatLost,
			reclaim: true,
		},
		{
			name: "心跳丢失 + ReclaimImmediately → 立即回收",
			sbx:  newSbx(sandboxv1alpha1.PhaseRunning, 60*time.Second),
			mutate: func(s *sandboxv1alpha1.AgentSandbox, o *Observation) {
				s.Spec.Lifecycle = &sandboxv1alpha1.LifecycleSpec{
					OnHeartbeatLoss: sandboxv1alpha1.OnHeartbeatLossReclaim,
				}
				o.LeaseHealthy = false
				quiet(o)
			},
			want:    sandboxv1alpha1.PhaseTerminating,
			reason:  ReasonHeartbeatLost,
			reclaim: true,
		},
		{
			name: "心跳丢失 + Ignore → 不因心跳回收",
			sbx:  newSbx(sandboxv1alpha1.PhaseRunning, 60*time.Second),
			mutate: func(s *sandboxv1alpha1.AgentSandbox, o *Observation) {
				s.Spec.Lifecycle = &sandboxv1alpha1.LifecycleSpec{
					OnHeartbeatLoss: sandboxv1alpha1.OnHeartbeatLossIgnore,
				}
				o.LeaseHealthy = false
				quiet(o)
			},
			reason: ReasonClaimed,
		},
		{
			name:    "空闲达回收阈值 → Terminating(IdleTimeout)",
			sbx:     newSbx(sandboxv1alpha1.PhaseRunning, 400*time.Second),
			want:    sandboxv1alpha1.PhaseTerminating,
			reason:  ReasonIdleTimeout,
			reclaim: true,
		},
		{
			// 冻结阈值（120s）小于回收阈值（300s），因此先冻，不回收。
			name: "空闲达冻结阈值 → Hibernating（先释放 CPU）",
			sbx:  newSbx(sandboxv1alpha1.PhaseRunning, 200*time.Second),
			mutate: func(s *sandboxv1alpha1.AgentSandbox, _ *Observation) {
				s.Spec.Hibernation = &sandboxv1alpha1.HibernationSpec{Enabled: true}
			},
			want:   sandboxv1alpha1.PhaseHibernating,
			reason: ReasonIdleHibernate,
		},
		{
			// 配置笔误防护：冻结阈值 ≥ 回收阈值时冻结毫无意义，必须直接回收，
			// 否则沙箱会被冻住却永不销毁 —— 表现为"内存占用迟迟不下降"。
			name: "冻结阈值不小于回收阈值 → 不冻结，直接回收",
			sbx:  newSbx(sandboxv1alpha1.PhaseRunning, 400*time.Second),
			mutate: func(s *sandboxv1alpha1.AgentSandbox, _ *Observation) {
				s.Spec.Hibernation = &sandboxv1alpha1.HibernationSpec{Enabled: true}
				s.Spec.Lifecycle = &sandboxv1alpha1.LifecycleSpec{
					IdleTimeoutSeconds:        300,
					HibernateAfterIdleSeconds: 300, // 与回收阈值持平 = 配置错误
				}
			},
			want:    sandboxv1alpha1.PhaseTerminating,
			reason:  ReasonIdleTimeout,
			reclaim: true,
		},
		{
			name: "Hibernating 且冻结完成 → Hibernated",
			sbx:  newSbx(sandboxv1alpha1.PhaseHibernating, 200*time.Second),
			mutate: func(_ *sandboxv1alpha1.AgentSandbox, o *Observation) {
				o.Frozen = true
			},
			want:   sandboxv1alpha1.PhaseHibernated,
			reason: ReasonHibernated,
		},
		{
			name: "Hibernated 且观测到活动 → Resuming",
			sbx:  newSbx(sandboxv1alpha1.PhaseHibernated, 150*time.Second),
			mutate: func(_ *sandboxv1alpha1.AgentSandbox, o *Observation) {
				o.NetworkActivity = true
			},
			want:   sandboxv1alpha1.PhaseResuming,
			reason: ReasonResuming,
		},
		{
			// 冻结只释放 CPU，内存仍在占用；达阈值必须真正销毁。
			name: "Hibernated 且达回收阈值 → Terminating（真正归还内存）",
			sbx:  newSbx(sandboxv1alpha1.PhaseHibernated, 400*time.Second),
			mutate: func(_ *sandboxv1alpha1.AgentSandbox, o *Observation) {
				quiet(o)
			},
			want:    sandboxv1alpha1.PhaseTerminating,
			reason:  ReasonIdleTimeout,
			reclaim: true,
		},
		{
			name: "Resuming 且唤醒完成 → Running",
			sbx:  newSbx(sandboxv1alpha1.PhaseResuming, 200*time.Second),
			mutate: func(_ *sandboxv1alpha1.AgentSandbox, o *Observation) {
				o.Resumed = true
			},
			want:   sandboxv1alpha1.PhaseRunning,
			reason: ReasonResumed,
		},
		{
			name:   "终态不再迁移",
			sbx:    newSbx(sandboxv1alpha1.PhaseSucceeded, 60*time.Second),
			reason: ReasonTerminal,
		},
		{
			name: "删除中不推进状态机（交给 Finalizer 链）",
			sbx:  newSbx(sandboxv1alpha1.PhaseRunning, 60*time.Second),
			mutate: func(s *sandboxv1alpha1.AgentSandbox, _ *Observation) {
				ts := metav1.NewTime(t0)
				s.DeletionTimestamp = &ts
			},
			reason: ReasonDeleting,
		},
		{
			name:   "一切正常 → 原地不动，只安排下次评估",
			sbx:    newSbx(sandboxv1alpha1.PhaseRunning, 60*time.Second),
			reason: ReasonClaimed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := healthy()
			if tc.mutate != nil {
				tc.mutate(tc.sbx, &o)
			}

			got := NextPhase(tc.sbx, o)

			if got.Next != tc.want {
				t.Errorf("Next = %q, 期望 %q (reason=%s)", got.Next, tc.want, got.Reason)
			}
			if got.Reason != tc.reason {
				t.Errorf("Reason = %q, 期望 %q", got.Reason, tc.reason)
			}
			if got.Reclaim != tc.reclaim {
				t.Errorf("Reclaim = %v, 期望 %v (reason=%s)", got.Reclaim, tc.reclaim, got.Reason)
			}
			if got.Next == "" && got.RequeueAfter <= 0 && tc.reason != ReasonDeleting && tc.reason != ReasonTerminal {
				t.Error("原地不动的决策必须给出 RequeueAfter，否则沙箱会永久停在此状态")
			}
			if got.Reclaim && got.RecycleReason == "" {
				t.Error("回收决策必须给出 RecycleReason（它会作为指标 label，不能为空）")
			}
			if got.Reclaim && got.Next != sandboxv1alpha1.PhaseTerminating && got.Next != sandboxv1alpha1.PhaseFailed {
				t.Errorf("回收决策只能指向 Terminating 或 Failed，实际 %q", got.Next)
			}
		})
	}
}

// TestNextPhase_EveryPhaseProducesADecision 守护一条不变式：
// 任何 Phase 都必须能给出**有原因码**的处置。
// 没有出口的 Phase 会让沙箱永久卡住，而这类 bug 只在特定时序下才暴露。
func TestNextPhase_EveryPhaseProducesADecision(t *testing.T) {
	phases := []sandboxv1alpha1.Phase{
		"", // 新建对象可能还没写 status
		sandboxv1alpha1.PhasePending,
		sandboxv1alpha1.PhaseProvisioning,
		sandboxv1alpha1.PhaseReady,
		sandboxv1alpha1.PhaseRunning,
		sandboxv1alpha1.PhaseIdle,
		sandboxv1alpha1.PhaseHibernating,
		sandboxv1alpha1.PhaseHibernated,
		sandboxv1alpha1.PhaseResuming,
		sandboxv1alpha1.PhaseTerminating,
		sandboxv1alpha1.PhaseSucceeded,
		sandboxv1alpha1.PhaseFailed,
	}
	for _, p := range phases {
		for _, age := range []time.Duration{5 * time.Second, 400 * time.Second, 8000 * time.Second} {
			sbx := newSbx(p, age)
			got := NextPhase(sbx, healthy())
			if got.Reason == "" {
				t.Errorf("phase=%q age=%s 的决策缺少原因码: %+v", p, age, got)
			}
		}
	}
}

// TestNextPhase_Idempotent 确认同一输入重复决策结果一致。
// 控制器的 reconcile 会被以任意顺序重复调用，幂等是它的基本前提。
func TestNextPhase_Idempotent(t *testing.T) {
	sbx := newSbx(sandboxv1alpha1.PhaseRunning, 400*time.Second)
	o := healthy()
	first := NextPhase(sbx, o)
	for i := 0; i < 50; i++ {
		if got := NextPhase(sbx, o); got != first {
			t.Fatalf("第 %d 次决策结果不同: %+v vs %+v", i, got, first)
		}
	}
}

func TestNextPhase_NilSandbox(t *testing.T) {
	got := NextPhase(nil, healthy())
	if got.Reason != ReasonUnexpectedPhase {
		t.Fatalf("nil 输入应返回 UnexpectedPhase，实际 %q", got.Reason)
	}
}

// ---------------------------------------------------------------------------
// 生效参数
// ---------------------------------------------------------------------------

// TestEffectiveLifecycle_DefaultsMatchConstants 是防止"两处默认值漂移"的守卫。
//
// CRD 的 +kubebuilder:default 与代码里的常量是两份独立来源，一旦不一致，
// 会表现为"单测通过但线上行为不同"—— 这类问题几乎无法从日志里看出来。
func TestEffectiveLifecycle_DefaultsMatchConstants(t *testing.T) {
	lc := EffectiveLifecycle(newSbx(sandboxv1alpha1.PhasePending, time.Second))

	checks := []struct {
		field string
		got   int32
		want  int32
	}{
		{"TTLSecondsAfterCreation", lc.TTLSecondsAfterCreation, DefaultTTLSeconds},
		{"IdleTimeoutSeconds", lc.IdleTimeoutSeconds, DefaultIdleTimeoutSeconds},
		{"HeartbeatGraceSeconds", lc.HeartbeatGraceSeconds, DefaultHeartbeatGraceSecs},
		{"HibernateAfterIdleSeconds", lc.HibernateAfterIdleSeconds, DefaultHibernateAfterIdle},
		{"ProvisionTimeoutSeconds", lc.ProvisionTimeoutSeconds, DefaultProvisionTimeout},
		{"MaxStockAgeSeconds", lc.MaxStockAgeSeconds, DefaultMaxStockAgeSeconds},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, 期望常量 %d", c.field, c.got, c.want)
		}
	}
	if lc.OnHeartbeatLoss != sandboxv1alpha1.OnHeartbeatLossGrace {
		t.Errorf("心跳丢失默认策略应为 Grace（保守），实际 %q", lc.OnHeartbeatLoss)
	}
	if lc.ReclaimPolicy != sandboxv1alpha1.ReclaimReturnToPool {
		t.Errorf("默认回收策略应为 ReturnToPool，实际 %q", lc.ReclaimPolicy)
	}
}

func TestEffectiveLifecycle_SpecOverridesDefaults(t *testing.T) {
	sbx := newSbx(sandboxv1alpha1.PhaseRunning, time.Second)
	sbx.Spec.Lifecycle = &sandboxv1alpha1.LifecycleSpec{
		TTLSecondsAfterCreation:   600,
		IdleTimeoutSeconds:        0, // 0 是"禁用空闲回收"的有效取值，必须被尊重
		HeartbeatGraceSeconds:     30,
		HibernateAfterIdleSeconds: 60,
		ProvisionTimeoutSeconds:   45,
		OnHeartbeatLoss:           sandboxv1alpha1.OnHeartbeatLossReclaim,
		ReclaimPolicy:             sandboxv1alpha1.ReclaimDestroy,
	}
	lc := EffectiveLifecycle(sbx)

	if lc.TTLSecondsAfterCreation != 600 {
		t.Errorf("TTL 未被覆盖: %d", lc.TTLSecondsAfterCreation)
	}
	if lc.IdleTimeoutSeconds != 0 {
		t.Errorf("IdleTimeoutSeconds=0 表示禁用空闲回收，不应被默认值顶掉: %d", lc.IdleTimeoutSeconds)
	}
	if lc.OnHeartbeatLoss != sandboxv1alpha1.OnHeartbeatLossReclaim {
		t.Errorf("OnHeartbeatLoss 未被覆盖: %q", lc.OnHeartbeatLoss)
	}
	if lc.ReclaimPolicy != sandboxv1alpha1.ReclaimDestroy {
		t.Errorf("ReclaimPolicy 未被覆盖: %q", lc.ReclaimPolicy)
	}
}

// TestActivityObserved_IsConservativeOnAnySignal 确认活动判定对"任意一个信号为真"
// 都返回活动。代价不对称：漏判活动会误杀会话，误判活动只是多占用一会儿资源。
func TestActivityObserved_ConservativeOnAnySignal(t *testing.T) {
	lc := EffectiveLifecycle(newSbx(sandboxv1alpha1.PhaseRunning, time.Second))

	cases := []struct {
		name string
		o    Observation
		want bool
	}{
		{"全静默", Observation{}, false},
		{"仅网络活动", Observation{NetworkActivity: true}, true},
		{"仅活跃连接", Observation{ActiveConnections: 1}, true},
		{"仅 CPU 超阈", Observation{CPUIncrementMilli: 200}, true},
		{"CPU 未超阈", Observation{CPUIncrementMilli: 1}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := activityObserved(lc, c.o); got != c.want {
				t.Errorf("activityObserved = %v, 期望 %v", got, c.want)
			}
		})
	}
}

// TestNextIdleCheck_DoesNotPollEverySecond 是性能守卫。
// 5k 沙箱如果每秒各评估一次，就是 5k QPS 的无效 reconcile —— 纯属自伤。
func TestNextIdleCheck_DoesNotPollEverySecond(t *testing.T) {
	sbx := newSbx(sandboxv1alpha1.PhaseRunning, 10*time.Second)
	lc := EffectiveLifecycle(sbx)
	o := healthy()
	o.Now = t0

	got := nextIdleCheck(lc, sbx, o)
	if got < 5*time.Second {
		t.Fatalf("下一次空闲评估间隔 = %s，过短会打爆 workqueue", got)
	}
}
