package sweeper

import (
	"testing"
	"time"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
)

var now = time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

// sbxView 造一个沙箱视图。默认是"健康、无异常"的状态。
func sbxView(name string) sandboxView {
	return sandboxView{
		Namespace: "sandbox-pool",
		Name:      name,
		UID:       "uid-" + name,
		Phase:     "Running",
		CreatedAt: now.Add(-time.Hour),
		HasClaim:  true,
		NodeName:  "node-a",
		PodName:   name,
	}
}

func readyNodes() map[string]bool { return map[string]bool{"node-a": true} }

// find 在结果里找指定 Kind 的发现。
func find(fs []Finding, kind Kind, name string) (Finding, bool) {
	for _, f := range fs {
		if f.Kind == kind && f.Name == name {
			return f, true
		}
	}
	return Finding{}, false
}

// TestClassify_CleanClusterReportsNothing 是"不误报"的基准。
//
// 它对账的意义和"不误删"同等重要：一个会无端报警的对账很快就会被忽略，
// 而一个被忽略的对账在真正需要它的时候不会起作用。
func TestClassify_CleanClusterReportsNothing(t *testing.T) {
	s := Snapshot{
		Now:       now,
		Sandboxes: []sandboxView{sbxView("sbx-1")},
		Pods: []podView{{
			Namespace: "sandbox-pool", Name: "sbx-1", UID: "pod-uid-1",
			OwnerUID: "uid-sbx-1", Role: roleSandbox, SandboxLabel: "sbx-1",
			CreatedAt: now.Add(-time.Hour), Phase: "Running",
		}},
		Leases: []namedView{{
			Namespace: "sandbox-pool", Name: "sbx-1", UID: "lease-uid-1",
			OwnerUID: "uid-sbx-1", Role: "lease", SandboxLabel: "sbx-1",
			CreatedAt: now.Add(-time.Hour),
		}},
		NodeReady: readyNodes(),
	}
	if fs := Classify(s, DefaultOptions()); len(fs) != 0 {
		t.Fatalf("干净集群不应有任何发现，实际 %d 条: %v", len(fs), fs)
	}
}

// TestClassify_OrphanIsBlockedFirstThenDeleted 守"先标记、下一轮再删"。
//
// 单轮直接删会在"控制器恰好正在创建这个对象"时误删正常资源。
// 关键在于宽限期衡量的是**从标记开始**过了多久，而不是对象有多老：
// 一个 2 小时前创建的 Pod 完全可能刚刚才变成孤儿，用年龄做宽限
// 会让它在正常删除流程中被立刻删掉 —— 而对账本应什么都没看到。
func TestClassify_OrphanIsBlockedFirstThenDeleted(t *testing.T) {
	base := Snapshot{
		Now: now,
		Pods: []podView{{
			Namespace: "sandbox-pool", Name: "orphan", UID: "pod-uid-x",
			Role: roleSandbox,
			// 2 小时前创建 —— 若用"年龄"做宽限，它会被立刻删除。
			CreatedAt: now.Add(-2 * time.Hour), Phase: "Running",
		}},
		NodeReady: readyNodes(),
	}

	// 第一次发现：只标记。
	first := Classify(base, DefaultOptions())
	f, ok := find(first, KindOrphanPod, "orphan")
	if !ok {
		t.Fatalf("应发现孤儿 Pod，实际: %v", first)
	}
	if f.Action != ActionBlock {
		t.Fatalf("首次发现应为 %s（标记），实际 %s —— 立刻删除会在控制器正常删除流程中误删",
			ActionBlock, f.Action)
	}

	// 已标记但在宽限期内：仍不删。
	blocked := base
	blocked.Pods[0].BlockedSince = now.Add(-time.Minute)
	if got, _ := find(Classify(blocked, DefaultOptions()), KindOrphanPod, "orphan"); got.Action != ActionBlock {
		t.Fatalf("宽限期内应保持标记，实际 %s", got.Action)
	}

	// 标记已超过宽限期：可以删。
	expired := base
	expired.Pods[0].BlockedSince = now.Add(-10 * time.Minute)
	got, _ := find(Classify(expired, DefaultOptions()), KindOrphanPod, "orphan")
	if got.Action != ActionDeleteObject {
		t.Fatalf("超过宽限期后应删除，实际 %s", got.Action)
	}
}

// TestClassify_OwnershipByEitherSignal 守归属判定的宽松取向。
//
// 少判一个孤儿 → 资源多留一轮（下一轮还会被发现）；
// 多判一个孤儿 → 删掉一个可能正在服务的沙箱。
// 两者代价不对称，因此"两条线索任一命中"即视为有主。
func TestClassify_OwnershipByEitherSignal(t *testing.T) {
	sbx := sbxView("sbx-1")
	s := Snapshot{
		Now:       now,
		Sandboxes: []sandboxView{sbx},
		Pods: []podView{
			{
				// 只有 ownerReference，label 丢失（例如由旧版本创建或被人工改过）。
				Namespace: "sandbox-pool", Name: "sbx-1", UID: "pod-1",
				OwnerUID: "uid-sbx-1", Role: roleSandbox,
				CreatedAt: now.Add(-time.Hour),
			},
			{
				// 只有 label，ownerReference 丢失（例如人工迁移过对象）。
				Namespace: "sandbox-pool", Name: "sbx-1-labelonly", UID: "pod-2",
				Role: roleSandbox, SandboxLabel: "sbx-1",
				CreatedAt: now.Add(-time.Hour),
			},
		},
		// 心跳 Lease 必须存在，否则会额外触发 claim_without_lease ——
		// 那是另一个独立规则，不该混进这条测试的断言里。
		Leases: []namedView{{
			Namespace: "sandbox-pool", Name: "sbx-1", UID: "lease-1",
			Role: "lease", SandboxLabel: "sbx-1", CreatedAt: now.Add(-time.Hour),
		}},
		NodeReady: readyNodes(),
	}
	if fs := Classify(s, DefaultOptions()); len(fs) != 0 {
		t.Fatalf("两种归属线索任一命中都不应判为孤儿，实际: %v", fs)
	}
}

// TestClassify_IgnoresObjectsOutsideOurScope 守"只碰自己的对象"。
//
// 对账运行在共享集群里。同一个命名空间里还有节点容量预铺的占位 Pod、
// 其他 Operator 的 Lease。把它们判成孤儿并删除会直接破坏池扩容能力，
// 或者干脆删掉别人的东西。
func TestClassify_IgnoresObjectsOutsideOurScope(t *testing.T) {
	s := Snapshot{
		Now: now,
		Pods: []podView{
			{
				// 节点容量预铺的占位 Pod：本来就没有对应的 AgentSandbox。
				Namespace: "sandbox-pool", Name: "placeholder-a", UID: "p-ph",
				Role: sandboxv1alpha1.RolePlaceholder, CreatedAt: now.Add(-24 * time.Hour),
			},
			{
				// 别人的 Pod。
				Namespace: "sandbox-pool", Name: "some-other-pod", UID: "p-other",
				Role: "", CreatedAt: now.Add(-24 * time.Hour),
			},
		},
		Leases: []namedView{
			{
				// 别的组件的选主 Lease（Role 为空 → 不属于我们的对账范围）。
				Namespace: "sandbox-system", Name: "leader-election", UID: "l-1",
				CreatedAt: now.Add(-24 * time.Hour),
			},
		},
		NodeReady: readyNodes(),
	}
	if fs := Classify(s, DefaultOptions()); len(fs) != 0 {
		t.Fatalf("不属于本平台的对象必须被忽略，实际: %v", fs)
	}
}

// TestClassify_ObjectsOfDeletingSandboxAreNotOrphans 守一条容易写错的正向规则。
//
// 沙箱正在删除时，它的附属对象**应当**还存在（Finalizer 链还没走到那一步）。
// 把这段正常过程判成孤儿，会对账反复标记每一个正在被删除的沙箱的附属资源 ——
// 既制造噪声，也可能在宽限期结束后真的抢在 Finalizer 之前删掉它们，
// 从而破坏"先断网、再落盘、后删卷"这个顺序。
func TestClassify_ObjectsOfDeletingSandboxAreNotOrphans(t *testing.T) {
	sbx := sbxView("sbx-1")
	sbx.Deleting = true
	sbx.DeletingSince = now.Add(-time.Minute)

	s := Snapshot{
		Now:       now,
		Sandboxes: []sandboxView{sbx},
		Pods: []podView{{
			Namespace: "sandbox-pool", Name: "sbx-1", UID: "pod-1",
			OwnerUID: "uid-sbx-1", Role: roleSandbox, SandboxLabel: "sbx-1",
			BlockedSince: now.Add(-time.Hour), CreatedAt: now.Add(-time.Hour),
		}},
		Netpols: []namedView{{
			Namespace: "sandbox-pool", Name: "sbx-1", UID: "np-1",
			OwnerUID: "uid-sbx-1", Role: roleNetpol, SandboxLabel: "sbx-1",
			BlockedSince: now.Add(-time.Hour),
		}},
		NodeReady: readyNodes(),
	}
	for _, f := range Classify(s, DefaultOptions()) {
		if f.Kind == KindOrphanPod || f.Kind == KindOrphanNetworkPolicy {
			t.Fatalf("删除中的沙箱的附属对象不应判为孤儿: %v", f)
		}
	}
}

// TestClassify_SandboxAnomalies 覆盖沙箱自身的四类异常。
func TestClassify_SandboxAnomalies(t *testing.T) {
	opt := DefaultOptions()

	t.Run("删除流程卡死", func(t *testing.T) {
		sbx := sbxView("stuck")
		sbx.Deleting = true
		sbx.DeletingSince = now.Add(-opt.StuckTerminatingGrace - time.Minute)
		fs := Classify(Snapshot{Now: now, Sandboxes: []sandboxView{sbx}, NodeReady: readyNodes()}, opt)
		f, ok := find(fs, KindStuckTerminating, "stuck")
		if !ok {
			t.Fatalf("应发现卡死的删除流程，实际: %v", fs)
		}
		if f.Action != ActionForceFinalize {
			t.Fatalf("动作为 %s，期望 %s", f.Action, ActionForceFinalize)
		}
	})

	t.Run("删除未超容忍期不报", func(t *testing.T) {
		sbx := sbxView("deleting-ok")
		sbx.Deleting = true
		sbx.DeletingSince = now.Add(-time.Minute)
		fs := Classify(Snapshot{Now: now, Sandboxes: []sandboxView{sbx}, NodeReady: readyNodes()}, opt)
		if _, ok := find(fs, KindStuckTerminating, "deleting-ok"); ok {
			t.Fatalf("未超容忍期不应报卡死: %v", fs)
		}
	})

	t.Run("长期 Pending", func(t *testing.T) {
		sbx := sbxView("pending")
		sbx.Phase = "Pending"
		sbx.CreatedAt = now.Add(-opt.PendingGrace - time.Minute)
		fs := Classify(Snapshot{Now: now, Sandboxes: []sandboxView{sbx}, NodeReady: readyNodes()}, opt)
		f, ok := find(fs, KindStuckPending, "pending")
		if !ok {
			t.Fatalf("应发现长期 Pending，实际: %v", fs)
		}
		if f.Action != ActionMarkFailed {
			t.Fatalf("动作为 %s，期望 %s（标记后由控制器走 Finalizer 链删除）",
				f.Action, ActionMarkFailed)
		}
	})

	t.Run("Ready 但无 Pod", func(t *testing.T) {
		sbx := sbxView("ghost-stock")
		sbx.Phase = "Ready"
		sbx.HasClaim = false
		fs := Classify(Snapshot{Now: now, Sandboxes: []sandboxView{sbx}, NodeReady: readyNodes()}, opt)
		if _, ok := find(fs, KindStockWithoutPod, "ghost-stock"); !ok {
			t.Fatalf("应发现虚高的池水位（Ready 但 Pod 不存在），实际: %v", fs)
		}
	})

	t.Run("节点丢失", func(t *testing.T) {
		sbx := sbxView("nodelost")
		sbx.NodeName = "node-gone"
		fs := Classify(Snapshot{Now: now, Sandboxes: []sandboxView{sbx}, NodeReady: readyNodes()}, opt)
		f, ok := find(fs, KindNodeLost, "nodelost")
		if !ok {
			t.Fatalf("应发现节点丢失，实际: %v", fs)
		}
		if f.Action != ActionMarkFailed {
			t.Fatalf("动作为 %s，期望 %s", f.Action, ActionMarkFailed)
		}
	})
}

// TestClassify_ClaimWithoutLeaseIsReportedNotReclaimed 守一条刻意的克制。
//
// 控制器的 ensureLease 每轮都会重建缺失的 Lease，因此这通常是一个
// 自愈中的瞬时状态。把它当成需要销毁的异常，会在控制器重启期间
// 杀掉所有正在服务的会话。
func TestClassify_ClaimWithoutLeaseIsReportedNotReclaimed(t *testing.T) {
	sbx := sbxView("no-lease")
	s := Snapshot{
		Now:       now,
		Sandboxes: []sandboxView{sbx},
		Pods: []podView{{
			Namespace: "sandbox-pool", Name: "no-lease", UID: "p-1",
			OwnerUID: "uid-no-lease", Role: roleSandbox, SandboxLabel: "no-lease",
			CreatedAt: now.Add(-time.Hour),
		}},
		NodeReady: readyNodes(),
	}
	f, ok := find(Classify(s, DefaultOptions()), KindClaimWithoutLease, "no-lease")
	if !ok {
		t.Fatalf("应上报缺少心跳 Lease")
	}
	if f.Action == ActionMarkFailed {
		t.Fatalf("缺少 Lease 不应标记 Failed：控制器会自行重建，标记 Failed 会在重启期间误杀会话")
	}
}

// TestClassify_RespectsMaxFindings 守爆炸半径上限。
//
// 一个 bug 可能让对账把所有对象都判成孤儿。有上限时，最坏情况是
// 删掉一批然后被截断并被 Truncated 标记暴露；没有上限时，
// 它能在一次运行里清空整个池。
func TestClassify_RespectsMaxFindings(t *testing.T) {
	var pods []podView
	for i := 0; i < 50; i++ {
		pods = append(pods, podView{
			Namespace: "sandbox-pool", Name: string(rune('a'+i%26)) + "-orphan",
			UID: "p", Role: roleSandbox, BlockedSince: now.Add(-time.Hour),
			CreatedAt: now.Add(-time.Hour),
		})
	}
	opt := DefaultOptions()
	opt.MaxFindings = 5
	fs := Classify(Snapshot{Now: now, Pods: pods, NodeReady: readyNodes()}, opt)
	if len(fs) != 5 {
		t.Fatalf("发现数 = %d，期望被上限截断为 5", len(fs))
	}
}

// TestClassify_DeterministicAndTimeSafe 守两个可测性前提。
func TestClassify_DeterministicAndTimeSafe(t *testing.T) {
	pods := []podView{
		{Namespace: "sandbox-pool", Name: "b", UID: "p2", Role: roleSandbox, CreatedAt: now.Add(-time.Hour)},
		{Namespace: "sandbox-pool", Name: "a", UID: "p1", Role: roleSandbox, CreatedAt: now.Add(-time.Hour)},
	}
	s := Snapshot{Now: now, Pods: pods, NodeReady: readyNodes()}

	first := Classify(s, DefaultOptions())
	second := Classify(s, DefaultOptions())
	if len(first) != 2 {
		t.Fatalf("应发现 2 条，实际 %d", len(first))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("同一输入两次调用结果不同（顺序不稳定），%d: %+v vs %+v",
				i, first[i], second[i])
		}
	}
	// 输出顺序必须稳定，否则日志对比与测试断言都不可靠。
	if first[0].Name != "a" || first[1].Name != "b" {
		t.Fatalf("结果未按稳定顺序排序: %v", first)
	}

	// 时间未知时不做任何判定：用一个错误的时间基准去决定删东西不可接受。
	if fs := Classify(Snapshot{Pods: pods}, DefaultOptions()); fs != nil {
		t.Fatalf("Now 为零值时应返回 nil，实际: %v", fs)
	}
}

// TestOptions_ZeroValueIsConservative 守零值语义。
//
// 一个把字段忘掉的调用方不应该意外获得最激进的行为（宽限期为 0 = 立刻删除）。
func TestOptions_ZeroValueIsConservative(t *testing.T) {
	opt := Options{}.withDefaults()
	if opt.OrphanGrace <= 0 || opt.StuckTerminatingGrace <= 0 || opt.PendingGrace <= 0 {
		t.Fatalf("零值 Options 必须补齐为保守默认值，实际: %+v", opt)
	}
	if opt.MaxFindings <= 0 {
		t.Fatalf("零值 Options 必须有爆炸半径上限，实际: %+v", opt)
	}
}

// TestAllKindsCovered 保证指标类别与实现同步。
//
// 漏掉一个类别会让它的指标永远为空，而"指标为空"与"问题不存在"
// 在监控上长得一模一样。
func TestAllKindsCovered(t *testing.T) {
	if len(AllKinds()) == 0 {
		t.Fatalf("AllKinds 为空")
	}
	seen := map[Kind]bool{}
	for _, k := range AllKinds() {
		if seen[k] {
			t.Fatalf("类别重复: %s", k)
		}
		seen[k] = true
	}
}
