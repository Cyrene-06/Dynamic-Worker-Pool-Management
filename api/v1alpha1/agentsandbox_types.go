package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentSandboxSpec 是单个沙箱实例的期望状态。
//
// 设计原则（docs/04 §0）：
//   - P2 规格不逐沙箱精调：只写档位（Tier），不直接写 requests，避免调度碎片。
//   - P4 声明式：Claim 表达"属于谁"，而不是"请帮我认领"。
//
// +kubebuilder:validation:XValidation:rule="!self.state.externalize || size(self.state.stateURI) > 0",message="启用状态外置（state.externalize=true）时必须提供 stateURI"
type AgentSandboxSpec struct {
	// PoolRef 指向所属沙箱池（集群级对象）。
	PoolRef NameRef `json:"poolRef"`
	// TemplateRef 指向规格模板（集群级对象）。
	TemplateRef NameRef `json:"templateRef"`

	// Tier 是资源档位（tiny/small/medium/large）。档位定义在平台侧 tiers ConfigMap，
	// 由 ResourceOptimizer 依据 P95 画像回写建议，不需要重建沙箱。
	// +kubebuilder:validation:Enum=tiny;small;medium;large
	// +kubebuilder:default=small
	Tier string `json:"tier,omitempty"`

	// Isolation 是期望的隔离级别。解析为 RuntimeClass 的过程在 internal/isolation，
	// 因此同一份 spec 可跨环境平移。
	// +kubebuilder:validation:Enum=simulated;runc;kata-fc;kata-clh
	Isolation IsolationLevel `json:"isolation,omitempty"`

	// Claim 为空表示这是池中库存；非空表示已被认领。
	//
	// 不可变性规则：tenant 绑定后不可变更，必须"先释放（清空）再重新认领"。
	// 这样可保证审计链完整，并避免跨租户复用（docs/08 §9）。
	//
	// +kubebuilder:validation:XValidation:rule="!has(oldSelf.requestedBy) || !has(self.requestedBy) || oldSelf.requestedBy.tenant == self.requestedBy.tenant",message="claim 的 tenant 绑定后不可变更；如需更换租户请先释放沙箱（清空 spec.claim）"
	Claim *ClaimSpec `json:"claim,omitempty"`

	// Lifecycle 为零值时由 TemplateController / Webhook 填入模板默认值。
	Lifecycle *LifecycleSpec `json:"lifecycle,omitempty"`

	// Hibernation 是休眠配置。
	Hibernation *HibernationSpec `json:"hibernation,omitempty"`

	// State 是状态外置配置（L3）。
	State StateSpec `json:"state,omitempty"`

	// Network 是出口治理配置。
	Network NetworkSpec `json:"network,omitempty"`

	// Mounts 是挂载点列表。
	//
	// 不可变性：Firecracker 不支持设备热插拔，因此创建后无法追加。
	// 需要新数据集时必须走对象存储/网络接口，而不是挂卷。
	//
	// +kubebuilder:validation:MaxItems=8
	// +listType=map
	// +listMapKey=name
	Mounts []MountSpec `json:"mounts,omitempty"`

	// CredentialsSecretRef 指向租户侧的凭据 Secret（短期有效，回收时吊销）。
	CredentialsSecretRef *NameRef `json:"credentialsSecretRef,omitempty"`
}

// NameRef 是按名字引用一个对象。
//
// 被引用的 SandboxPool 与 SandboxTemplate 都是集群级对象，因此这里不带 namespace；
// 集群级对象引用命名空间对象（如租户 Secret）需要由控制器显式限定，
// 避免成为跨命名空间读写跳板。
type NameRef struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// AgentSandboxStatus 是沙箱的观测状态。
//
// 写入约定：所有 status 变更必须走 status 子资源，且控制器按 observedGeneration
// 判断是否需要重新处理，避免同一次变更被反复 reconcile。
type AgentSandboxStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// SandboxID 是业务侧持有的句柄，一经生成不可变（INV-5）。
	// 池中库存转正时该值保持不变，因此业务持有的句柄不会失效。
	SandboxID string `json:"sandboxID,omitempty"`

	Phase Phase `json:"phase,omitempty"`

	// Standard Kubernetes condition list.
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	PodName          string `json:"podName,omitempty"`
	NodeName         string `json:"nodeName,omitempty"`
	RuntimeClassName string `json:"runtimeClassName,omitempty"`
	// IsolationLevel 记录实际生效的隔离级别（可能是降级后的结果，用于暴露静默降级）。
	IsolationLevel IsolationLevel `json:"isolationLevel,omitempty"`

	ClaimRef    *ClaimStatus      `json:"claimRef,omitempty"`
	Activity    ActivityStatus    `json:"activity,omitempty"`
	Hibernation HibernationStatus `json:"hibernation,omitempty"`
	Metrics     SandboxMetrics    `json:"metrics,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sbx
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolRef.name`
// +kubebuilder:printcolumn:name="Tier",type=string,JSONPath=`.spec.tier`
// +kubebuilder:printcolumn:name="Tenant",type=string,JSONPath=`.status.claimRef.tenant`
// +kubebuilder:printcolumn:name="Isolation",type=string,JSONPath=`.status.isolationLevel`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentSandbox 是一个 Agent 沙箱实例。
//
// 池中库存与已认领实例**共用同一类对象**：认领只是给 spec.claim 赋值，
// 而不是"删除库存 + 新建实例"。这样 sandboxID 稳定不变，业务持有的句柄不会失效，
// 也避免了删除/重建之间的一致性窗口。
type AgentSandbox struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec AgentSandboxSpec `json:"spec,omitempty"`
	// +optional
	Status AgentSandboxStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentSandboxList 包含 AgentSandbox 列表。
type AgentSandboxList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentSandbox `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentSandbox{}, &AgentSandboxList{})
}
