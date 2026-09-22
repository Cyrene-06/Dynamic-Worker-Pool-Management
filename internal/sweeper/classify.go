package sweeper

import (
	"sort"
	"time"
)

// withDefaults 补齐零值，保证 Options 的任何零值字段都有确定语义。
//
// 必须在这里补齐而不是靠调用方：对账的阈值全都有"零值会导致什么"的问题。
// 例如 OrphanGrace=0 意味着"标记后立刻删除"，也就是完全取消宽限期 ——
// 一个把字段忘掉的调用方会意外地获得最激进的行为。
func (o Options) withDefaults() Options {
	if o.OrphanGrace <= 0 {
		o.OrphanGrace = 5 * time.Minute
	}
	if o.StuckTerminatingGrace <= 0 {
		o.StuckTerminatingGrace = 10 * time.Minute
	}
	if o.PendingGrace <= 0 {
		o.PendingGrace = 30 * time.Minute
	}
	if o.MaxFindings <= 0 {
		o.MaxFindings = 200
	}
	return o
}

// classifyScope 描述"哪些对象归我们对账"。
//
// # 这是整个 Sweeper 里最关键的一个决定
//
// 只把**带有本平台角色标签**的对象纳入对账范围。理由：Sweeper 运行在
// 一个共享集群里，同一个命名空间（sandbox-pool）里完全可能有别人的
// Lease、别人的 PVC。一个按"名字看起来不像沙箱"来推断孤儿的实现，
// 迟早会在某个用户的命名空间里删掉不属于它的东西。
//
// 判据必须是**正向证据**（带我们的标签 / ownerReference 指向我们的对象），
// 而不是**反向推断**（没找到对应沙箱所以是孤儿）。反向推断的失败模式
// 是把"我不认识的东西"当成"没人要的东西"。
const (
	roleSandbox    = "sandbox"
	roleNetpol     = "sandbox-netpol"
	roleDataVolume = "sandbox-volume"
)

// IsManagedPod 判断一个 Pod 是否归本平台对账（README：只碰自己的对象）。
func IsManagedPod(role string) bool { return role == roleSandbox }

// ownerLookup 是沙箱归属索引。
type ownerLookup struct {
	byUID  map[string]sandboxView
	byName map[string]sandboxView // key: namespace + "/" + name
}

func newOwnerLookup(sandboxes []sandboxView) ownerLookup {
	l := ownerLookup{
		byUID:  make(map[string]sandboxView, len(sandboxes)),
		byName: make(map[string]sandboxView, len(sandboxes)),
	}
	for _, s := range sandboxes {
		if s.UID != "" {
			l.byUID[s.UID] = s
		}
		l.byName[s.Namespace+"/"+s.Name] = s
	}
	return l
}

// owns 判断一个附属对象是否仍然归属某个存活的沙箱。
//
// 用 **UID 或 label 任一命中**即视为有主，是刻意的宽松取向：
//
//	少判一个孤儿，代价是一个资源多留一轮（下一轮还会被发现）；
//	多判一个孤儿，代价是删掉一个正在服务的沙箱。
//
// 两者代价不对称，因此所有不确定的情况都往"不删"一侧倒。
// 两种判据都留着是因为它们各有失效场景：label 可能被人工改动或
// 由旧版本控制器创建时缺失，而 ownerReference 在跨命名空间或
// 人工迁移时会丢。
func (l ownerLookup) owns(ownerUID, namespace, sandboxLabel string) (sandboxView, bool) {
	if ownerUID != "" {
		if s, ok := l.byUID[ownerUID]; ok {
			return s, true
		}
	}
	if sandboxLabel != "" {
		if s, ok := l.byName[namespace+"/"+sandboxLabel]; ok {
			return s, true
		}
	}
	return sandboxView{}, false
}

// Classify 是泄漏对账的**唯一决策点**，且是纯函数。
//
// 它不读时钟（时间来自 Snapshot）、不碰集群、不做任何删除。
// 因此可以用"构造一份快照"的方式穷举测试全部判断，
// 包括那些在真实集群里极难复现的组合（节点消失 + 对象正好在删除中 + 缓存滞后）。
//
// 返回的发现列表已按 Kind、Namespace、Name 排序，保证同一份输入
// 永远产出同一顺序的输出 —— 否则测试与日志对比都会变得不可靠。
func Classify(s Snapshot, opt Options) []Finding {
	opt = opt.withDefaults()
	if s.Now.IsZero() {
		// 时间未知时不做任何时间相关判定。宁可这一轮什么都不报，
		// 也不能用一个错误的时间基准去决定删东西。
		return nil
	}

	owners := newOwnerLookup(s.Sandboxes)
	var out []Finding

	// emit 记录一条发现，并强制 MaxFindings 上限。
	//
	// 超过上限时**停止继续收集**，而不是丢弃后面的：丢弃会让"发现了多少"
	// 这个数字失去意义，而停止至少让报告里的条目都是真实且完整的。
	emit := func(f Finding) bool {
		if len(out) >= opt.MaxFindings {
			return false
		}
		out = append(out, f)
		return true
	}

	// ---- 1. 孤儿附属对象 ----
	//
	// 统一走"先标记、下一轮再删"。见 bounded() 与 ActionBlock 的说明。
	checkOrphans := func(kind Kind, views []namedView, roleLabel string) {
		for _, v := range views {
			// 只处理带我们角色标签的对象（正向证据，见 classifyScope）。
			if v.Role != roleLabel {
				continue
			}
			owner, owned := owners.owns(v.OwnerUID, v.Namespace, v.SandboxLabel)
			if owned {
				// 沙箱正在删除时，它的附属对象**应当**还存在（Finalizer 链
				// 还没走到那一步）。把这段正常过程判成孤儿，就会让对账
				// 反复标记每一个正在被删除的沙箱的附属资源。
				_ = owner
				continue
			}
			action := bounded(v, s.Now, opt.OrphanGrace)
			reason := "未找到归属的沙箱（ownerUID=" + v.OwnerUID + " label=" + v.SandboxLabel + "）"
			if action == ActionBlock && !v.BlockedSince.IsZero() {
				reason = "已标记为孤儿，仍在宽限期内（自 " +
					v.BlockedSince.UTC().Format(time.RFC3339) + "）"
			}
			if !emit(Finding{Kind: kind, Action: action, Namespace: v.Namespace, Name: v.Name, Reason: reason}) {
				return
			}
		}
	}

	checkOrphans(KindOrphanLease, s.Leases, "lease")
	checkOrphans(KindOrphanNetworkPolicy, s.Netpols, roleNetpol)
	checkOrphans(KindOrphanVolume, s.Volumes, roleDataVolume)

	// Pod 单独走一遍：它的归属判断还多一个"角色"维度。
	for _, p := range s.Pods {
		if !IsManagedPod(p.Role) {
			continue
		}
		if _, owned := owners.owns(p.OwnerUID, p.Namespace, p.SandboxLabel); owned {
			continue
		}
		action := bounded(namedView{
			Namespace: p.Namespace, Name: p.Name, UID: p.UID,
			OwnerUID: p.OwnerUID, CreatedAt: p.CreatedAt,
			SandboxLabel: p.SandboxLabel, BlockedSince: p.BlockedSince,
		}, s.Now, opt.OrphanGrace)
		reason := "未找到归属的沙箱（ownerUID=" + p.OwnerUID + " label=" + p.SandboxLabel + "）"
		if action == ActionBlock && !p.BlockedSince.IsZero() {
			reason = "已标记为孤儿 Pod，仍在宽限期内（自 " +
				p.BlockedSince.UTC().Format(time.RFC3339) + "）"
		}
		if !emit(Finding{Kind: KindOrphanPod, Action: action, Namespace: p.Namespace, Name: p.Name, Reason: reason}) {
			return out
		}
	}

	// ---- 2. 沙箱自身的异常 ----
	leaseOwners := map[string]bool{}
	for _, l := range s.Leases {
		if l.SandboxLabel != "" {
			leaseOwners[l.Namespace+"/"+l.SandboxLabel] = true
		}
	}
	podBySandbox := map[string]podView{}
	for _, p := range s.Pods {
		if p.SandboxLabel != "" {
			podBySandbox[p.Namespace+"/"+p.SandboxLabel] = p
		}
	}

	for _, sbx := range s.Sandboxes {
		key := sbx.Namespace + "/" + sbx.Name

		// 2a. 删除流程卡死。
		//
		// 以 deletionTimestamp 为基准而不是创建时间：卡不卡死只取决于
		// "开始删除之后过了多久"，与对象活了多久无关。
		if sbx.Deleting && !sbx.DeletingSince.IsZero() &&
			s.Now.Sub(sbx.DeletingSince) > opt.StuckTerminatingGrace {
			if !emit(Finding{
				Kind: KindStuckTerminating, Action: ActionForceFinalize,
				Namespace: sbx.Namespace, Name: sbx.Name,
				Reason: "删除已开始 " + s.Now.Sub(sbx.DeletingSince).Round(time.Second).String() +
					" 仍未完成，超过容忍期 " + opt.StuckTerminatingGrace.String(),
			}) {
				return sorted(out)
			}
			// 已在删除中的对象不再做其它判定。
			continue
		}

		// 2b. 长期停在 Pending / Provisioning。
		if (sbx.Phase == "Pending" || sbx.Phase == "Provisioning" || sbx.Phase == "") &&
			!sbx.Deleting && !sbx.CreatedAt.IsZero() &&
			s.Now.Sub(sbx.CreatedAt) > opt.PendingGrace {
			if !emit(Finding{
				Kind: KindStuckPending, Action: ActionMarkFailed,
				Namespace: sbx.Namespace, Name: sbx.Name,
				Reason: "已处于 " + sbx.Phase + " " + s.Now.Sub(sbx.CreatedAt).Round(time.Second).String() +
					"，疑似容量不足或调度失败",
			}) {
				return sorted(out)
			}
			continue
		}

		if sbx.Deleting {
			continue
		}

		// 2c. phase=Ready 但 Pod 已不存在。池水位会一直把它算作库存，
		// 于是池永远看到"水位够了"，而实际上可认领的库存是零。
		if sbx.Phase == "Ready" {
			if _, ok := podBySandbox[key]; !ok {
				if !emit(Finding{
					Kind: KindStockWithoutPod, Action: ActionMarkFailed,
					Namespace: sbx.Namespace, Name: sbx.Name,
					Reason: "库存标记为 Ready 但 Pod 不存在，池水位因此虚高",
				}) {
					return sorted(out)
				}
			}
			continue
		}

		// 2d. 承载节点已不存在。
		if sbx.NodeName != "" {
			ready, exists := s.NodeReady[sbx.NodeName]
			if !exists || !ready {
				if !emit(Finding{
					Kind: KindNodeLost, Action: ActionMarkFailed,
					Namespace: sbx.Namespace, Name: sbx.Name,
					Reason: "承载节点 " + sbx.NodeName + " 不存在或未就绪",
				}) {
					return sorted(out)
				}
				continue
			}
		}

		// 2e. 已认领但没有心跳 Lease。
		//
		// 这里只**上报**（ActionBlock），不标记 Failed：控制器的 ensureLease
		// 每轮都会重建缺失的 Lease，因此这通常是一个自愈中的瞬时状态。
		// 把它当成需要销毁的异常，会在控制器重启期间杀掉所有正在服务的会话。
		if sbx.HasClaim && !leaseOwners[key] {
			if !emit(Finding{
				Kind: KindClaimWithoutLease, Action: ActionBlock,
				Namespace: sbx.Namespace, Name: sbx.Name,
				Reason: "已认领但缺少心跳 Lease（tenant=" + sbx.ClaimedTenant + "），控制器应自行重建",
			}) {
				return sorted(out)
			}
		}
	}

	return sorted(out)
}

// sorted 按稳定顺序排列，保证输出可复现。
func sorted(in []Finding) []Finding {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].Kind != in[j].Kind {
			return in[i].Kind < in[j].Kind
		}
		if in[i].Namespace != in[j].Namespace {
			return in[i].Namespace < in[j].Namespace
		}
		return in[i].Name < in[j].Name
	})
	return in
}
