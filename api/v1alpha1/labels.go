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
)

// 角色取值。
const (
	RoleSandbox     = "sandbox"
	RolePlaceholder = "placeholder" // 节点容量预铺用的占位 Pod，负优先级、可被抢占
)

// 资源档位名称。与平台侧 tiers ConfigMap 的键一一对应。
const (
	TierTiny   = "tiny"
	TierSmall  = "small"
	TierMedium = "medium"
	TierLarge  = "large"
)
