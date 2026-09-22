package v1alpha1

// Label 与 Annotation 键名。
//
// 这些字符串是**跨组件契约**：gateway 靠 LabelClaimed=false 列出池中库存、
// 靠 LabelPool 做分片筛选，operator 靠 LabelRole 过滤自己的 Pod（避免把
// 集群里所有 Pod 都拉进 informer 缓存）。因此集中定义在这里，任何一处
// 硬编码字符串都可能造成两端静默不匹配。
const (
	// LabelPool 标记沙箱归属的池（池按"隔离级别 + 档位"划分，不按租户）。
	LabelPool = "sandbox.example.com/pool"
	// LabelTier 标记资源档位。
	LabelTier = "sandbox.example.com/tier"
	// LabelTemplate 标记规格模板。
	LabelTemplate = "sandbox.example.com/template"
	// LabelIsolation 标记隔离级别，用于按节点池选择。
	LabelIsolation = "sandbox.example.com/isolation"
	// LabelTenant 标记租户。库存阶段为空 —— 这正是"未被认领"的标识之一。
	LabelTenant = "sandbox.example.com/tenant"
	// LabelClaimed 是认领状态的字符串表示（"true"/"false"）。
	// 用字符串而不是布尔：Kubernetes label 值只能是字符串，
	// 且 gateway 需要用它做 List 的 selector 匹配。
	LabelClaimed = "sandbox.example.com/claimed"
	// LabelRole 标记对象的角色，operator 用它把 informer 限定在自己的对象上。
	// 5k 沙箱规模下，这条过滤能显著降低控制器内存占用。
	LabelRole = "sandbox.example.com/role"
	// LabelDraining 标记正在排空、已从水位统计中排除的库存。
	LabelDraining = "sandbox.example.com/draining"

	// LabelRequestID 用于把平台对象与业务日志关联（全链路追踪）。
	LabelRequestID = "sandbox.example.com/request-id"

	// LabelSandbox 是**子对象**（Pod / NetworkPolicy / PVC）指向其所属沙箱的标签。
	//
	// 有了它，Finalizer 与 Sweeper 就能用 label selector 精确找出"属于这个沙箱"的
	// 附属资源，而不必反推命名规则。命名规则（如 "<沙箱>-data"）在代码里
	// 每改一次就会静默地漏掉一批对象，而 selector 不会。
	LabelSandbox = "sandbox.example.com/sandbox"
)

// 注解键名。同样是跨组件契约。
const (
	// AnnoBlockedSince 记录一个对象被判定为异常（孤儿/卡死）的起始时间。
	//
	// Sweeper 用它实现"先标记、下一轮再删"：单轮直接删会在
	// "控制器恰好正在创建这个对象"时误删正常资源。关键在于宽限期衡量的是
	// **从标记开始**过了多久，而不是对象有多老 —— 一个 2 小时前创建的 Pod
	// 完全可能刚刚才变成孤儿，用年龄做宽限会让它在正常删除流程中被立刻删掉。
	AnnoBlockedSince = "sandbox.example.com/blocked-since"
	// AnnoWakeRequested 是手工唤醒请求（gateway 的 :wake），值为 "true"。
	//
	// 用注解而不是新增 spec 字段：唤醒是一次**动作**而非持续期望状态，
	// 写成 spec 字段会变成"一直要求唤醒"，与休眠态的判断直接打架。
	//
	// 契约是单向的：gateway 写，控制器只读。解除请求同样由 gateway 完成
	// （:wake 清掉 hibernate 标记）—— 若让控制器在动作完成后回写注解，
	// 就变成两个写入者，而两个写入者互相覆盖是这类 bug 的常见来源。
	AnnoWakeRequested = "sandbox.example.com/wake-requested"
	// AnnoHibernateRequested 是手工休眠请求（gateway 的 :hibernate），值为 "true"。
	AnnoHibernateRequested = "sandbox.example.com/hibernate-requested"
)

// 平台命名空间。库存与已认领沙箱都在 sandbox-pool 里，租户不直接操作
// Kubernetes API（只走 gateway）—— 因为 Kubernetes RBAC 无法表达
// "只能看到属于自己租户的对象"（docs/04 §0 P1）。
const (
	NamespacePool   = "sandbox-pool"
	NamespaceSystem = "sandbox-system"
)

// 角色取值。
const (
	RoleSandbox     = "sandbox"
	RolePlaceholder = "placeholder" // 节点容量预铺用的占位 Pod，负优先级、可被抢占
	// RoleNetpol 标记由平台为沙箱创建的网络策略。
	// 与 RoleSandbox 分开是必要的：operator 的 Pod 缓存按 role=sandbox 过滤，
	// 把网络策略也标成同一个值会让"按角色过滤"失去意义。
	RoleNetpol = "sandbox-netpol"
	// RoleDataVolume 标记沙箱的数据卷（ephemeral PVC）。
	RoleDataVolume = "sandbox-volume"
)

// 资源档位名称。与平台侧 tiers ConfigMap 的键一一对应。
const (
	TierTiny   = "tiny"
	TierSmall  = "small"
	TierMedium = "medium"
	TierLarge  = "large"
)
