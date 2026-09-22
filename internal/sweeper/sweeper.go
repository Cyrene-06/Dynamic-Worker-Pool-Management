// Package sweeper 实现泄漏对账（docs/04 §6.3）。
//
// # 它在整个架构里的位置
//
// 状态机处理**正常路径**，对账处理**一切异常路径**。这不是重复劳动：
// 控制器可能崩溃、可能被重启、可能读到过期缓存；Finalizer 可能在超时后
// 被强制推进而留下残余；etcd 恢复可能让对象引用已不存在的 Pod。
// 这些情况在代码里穷举不完，只能靠一个"定期独立核对"的兜底。
//
// # 为什么核心必须是纯函数
//
// 对账的动作是**删除别人的资源**。写错一个条件，它就会在深夜把正在服务的
// 沙箱连同数据一起删掉 —— 而且因为它"按设计定时运行"，这种错误会稳定地
// 重复发生。因此 Classify 被设计成纯函数：输入一份集群快照，输出一份发现列表，
// 不读时钟、不碰集群，从而可以被穷举测试。
package sweeper

import (
	"time"
)

// Kind 是被对账的对象类别。它进指标 label，因此必须是枚举。
type Kind string

const (
	// KindOrphanPod 表示存在 Pod 但没有对应的 AgentSandbox。
	KindOrphanPod Kind = "orphan_pod"
	// KindOrphanLease 表示存在 Lease 但没有对应的 AgentSandbox。
	KindOrphanLease Kind = "orphan_lease"
	// KindOrphanNetworkPolicy 表示存在网络策略但没有对应的 AgentSandbox。
	KindOrphanNetworkPolicy Kind = "orphan_netpol"
	// KindOrphanVolume 表示存在数据卷但没有对应的 AgentSandbox。
	KindOrphanVolume Kind = "orphan_volume"
	// KindStuckTerminating 表示删除流程卡死超过容忍期。
	KindStuckTerminating Kind = "stuck_terminating"
	// KindStuckPending 表示长期停在 Pending / Provisioning。
	KindStuckPending Kind = "stuck_pending"
	// KindStockWithoutPod 表示 phase=Ready 但 Pod 已不存在。
	KindStockWithoutPod Kind = "stock_without_pod"
	// KindNodeLost 表示承载沙箱的节点已不存在或长期不就绪。
	KindNodeLost Kind = "node_lost"
	// KindClaimWithoutLease 表示已认领但没有心跳 Lease。
	KindClaimWithoutLease Kind = "claim_without_lease"
)

// AllKinds 列出全部类别，供指标预热与测试穷举使用。
//
// 显式列出而不是靠遍历常量：漏掉一个类别会让它的指标永远为空，
// 而"指标为空"与"问题不存在"在监控上长得一模一样。
func AllKinds() []Kind {
	return []Kind{
		KindOrphanPod,
		KindOrphanLease,
		KindOrphanNetworkPolicy,
		KindOrphanVolume,
		KindStuckTerminating,
		KindStuckPending,
		KindStockWithoutPod,
		KindNodeLost,
		KindClaimWithoutLease,
	}
}

// Action 是对账可以采取的动作。
type Action string

const (
	// ActionBlock 表示"先标记、下一轮再处理"。
	//
	// 这不是犹豫，而是唯一安全的做法：对账看到的是某一瞬间的快照，
	// 而控制器可能恰好正在创建这个对象。立刻删除会误删正常资源，
	// 而误删一个正在服务的沙箱是不可逆的。标记后等一个宽限期再看一次，
	// 就能把"孤儿"与"正在诞生"区分开。
	ActionBlock Action = "block"
	// ActionDeleteObject 表示删除该对象（已过宽限期，确认是孤儿）。
	ActionDeleteObject Action = "delete_object"
	// ActionForceFinalize 表示移除 Finalizer 并删除（删除流程已卡死）。
	ActionForceFinalize Action = "force_finalize"
	// ActionMarkFailed 表示把沙箱标记为 Failed。
	//
	// 对账不直接删沙箱，而是标记后交给控制器删除：这样 Finalizer 链
	// 仍会正常执行（先断网、再落盘、后删卷）。对账绕过 Finalizer 去删，
	// 就等于把"清理顺序"这个安全设计丢掉了。
	ActionMarkFailed Action = "mark_failed"
)

// Finding 是一条对账发现。
type Finding struct {
	Kind   Kind
	Action Action
	// Namespace / Name 定位对象。Name 对集群级对象同样适用。
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	// Reason 是给运维看的一句话原因，会同时进日志与事件。
	Reason string `json:"reason"`
}

// String 便于日志与测试输出。
func (f Finding) String() string {
	return string(f.Kind) + "/" + string(f.Action) + " " + f.Namespace + "/" + f.Name + ": " + f.Reason
}

// sandboxView 是 Classify 需要的沙箱事实。
//
// 刻意不从 AgentSandbox 类型直接读，而是显式列出用到的字段：
// 对账的每个判断都必须能一眼看出依赖了什么，否则"为什么它删了我的沙箱"
// 会变成一次考古。
type sandboxView struct {
	Namespace string
	Name      string
	UID       string
	Phase     string
	// DeletionTimestamp 非零表示已在删除中。
	Deleting bool
	// DeletingSince 是删除开始时间（仅在 Deleting 为真时有意义）。
	DeletingSince time.Time
	// CreatedAt 是对象创建时间。
	CreatedAt time.Time
	// HasClaim 表示 spec.claim 非空。
	HasClaim bool
	// ClaimedTenant 用于日志定位"删了谁的沙箱"。
	ClaimedTenant string
	// NodeName 是承载节点（来自 status）。
	NodeName string
	// PodName 是 status 记录的 Pod 名。
	PodName string
	// RecycleReason 用于报告回收归因。
	RecycleReason string
}

// podView 是 Classify 需要的 Pod 事实。
type podView struct {
	Namespace string
	Name      string
	UID       string
	// OwnerUID 是 controller ownerReference 指向的沙箱 UID（空表示没有）。
	OwnerUID string
	// Role 是 LabelRole 的值。只有 role=sandbox 的 Pod 才纳入对账，
	//
	//	因为同一个命名空间里还会有节点容量预铺的占位 Pod（role=placeholder），
	//	它们**本来就没有**对应的 AgentSandbox。不区分角色就会把它们
	//	全部判成孤儿并删除 —— 而那会直接破坏池扩容能力。
	Role      string
	CreatedAt time.Time
	// Phase 是 Pod 阶段字符串。
	Phase string
	// SandboxLabel 是 LabelSandbox 的值（空表示没有）。
	SandboxLabel string
	// BlockedSince 是上次被判定为孤儿的时间（零值表示尚未标记过）。
	BlockedSince time.Time
}

// namedView 是 Classify 需要的"命名对象"事实（Lease / NetworkPolicy / PVC）。
type namedView struct {
	Namespace string
	Name      string
	UID       string
	OwnerUID  string
	// Role 是 LabelRole 的值，用于把对账范围限定在本平台创建的对象上。
	Role      string
	CreatedAt time.Time
	// SandboxLabel 是 LabelSandbox 的值（空表示没有）。
	SandboxLabel string
	// BlockedSince 是上次被判定为孤儿的时间（零值表示尚未标记过）。
	BlockedSince time.Time
}

// Snapshot 是对账所需的全部集群事实。
//
// 一次性快照而不是边查边删：对账的判断是"在某个时刻，这个东西是孤儿"。
// 若边查边删，前半轮的删除会改变后半轮看到的世界，
// 于是"是不是孤儿"这个问题的答案取决于遍历顺序 —— 那就无法推理了。
type Snapshot struct {
	Now time.Time

	Sandboxes []sandboxView
	Pods      []podView
	Leases    []namedView
	Netpols   []namedView
	Volumes   []namedView

	// NodeReady 是节点名到"是否就绪"的映射。**不存在的节点**不出现在 map 里。
	NodeReady map[string]bool
}

// Options 是对账阈值。
type Options struct {
	// OrphanGrace 是"已标记为孤儿"到"可以删除"之间必须等待的时间。
	//
	// 注意它衡量的是**从标记开始**的时间，而不是对象的年龄。
	// 这个区别至关重要：一个创建于 2 小时前的 Pod 可能刚刚才变成孤儿
	// （它的 AgentSandbox 在 1 秒前被删除）。若用年龄做宽限，
	// 它会在确认孤儿后立刻被删 —— 而那正是控制器自己的 Finalizer
	// 正在正常清理的时刻，对账就变成了在正常流程里乱插手。
	OrphanGrace time.Duration

	// StuckTerminatingGrace 是删除流程卡死多久后强制清理。
	StuckTerminatingGrace time.Duration

	// PendingGrace 是沙箱在 Pending / Provisioning 停留多久后判定卡死。
	//
	// 它必须显著大于 ProvisionTimeoutSeconds（默认 120s）：对账是**兜底**，
	// 正常超时由状态机处理。若两个阈值接近，对账就会在状态机即将自行处理时
	// 抢先介入，把"控制器有自己的超时"这件事变成不可观察。
	PendingGrace time.Duration

	// MaxFindings 限制单轮发现数，是**爆炸半径的上限**。
	//
	// 一个 bug 可能让对账把所有对象都判成孤儿。有这个上限时，
	// 最坏情况是删掉一批然后被上限截断、并被指标与告警暴露；
	// 没有上限时，它能在一次运行里清空整个池。
	MaxFindings int
}

// DefaultOptions 返回生产默认值，与 docs/09 §2 的多环境配置一致。
func DefaultOptions() Options {
	return Options{
		OrphanGrace:           5 * time.Minute,
		StuckTerminatingGrace: 10 * time.Minute,
		PendingGrace:          30 * time.Minute,
		MaxFindings:           200,
	}
}

// bounded 根据宽限期决定孤儿对象的处置：先标记，再删除。
//
// 这是整个对账里唯一需要谨慎的地方，因为它的输出会真的删掉东西。
// 关键在于宽限期衡量的是"**从标记开始**过了多久"，而不是对象的年龄：
// 一个创建于 2 小时前的 Pod 完全可能刚刚才变成孤儿（它的 AgentSandbox
// 在 1 秒前被删除）。若用年龄做宽限，它会在确认孤儿后立刻被删 ——
// 而那正是控制器自己的 Finalizer 正常清理的时刻，对账就变成了
// 在正常流程里乱插手，而且插得恰到好处地破坏性。
func bounded(v namedView, now time.Time, grace time.Duration) Action {
	if v.BlockedSince.IsZero() {
		// 第一次发现：只标记，不动手。
		return ActionBlock
	}
	if now.Sub(v.BlockedSince) < grace {
		// 还在宽限期内：保持标记，继续等。
		return ActionBlock
	}
	return ActionDeleteObject
}
