package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TemplateDefaults 是模板提供的生命周期默认值。
type TemplateDefaults struct {
	TTLSecondsAfterCreation int32  `json:"ttlSecondsAfterCreation,omitempty"`
	IdleTimeoutSeconds      int32  `json:"idleTimeoutSeconds,omitempty"`
	HeartbeatGraceSeconds   int32  `json:"heartbeatGraceSeconds,omitempty"`
	ReclaimPolicy           string `json:"reclaimPolicy,omitempty"`
	AllowSameTenantReuse    bool   `json:"allowSameTenantReuse,omitempty"`
	MaxClaimCount           int32  `json:"maxClaimCount,omitempty"`
}

// HookSpec 是一个在沙箱内执行的钩子。
type HookSpec struct {
	Command []string `json:"command"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=20
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

// TemplateIsolation 声明模板允许的隔离级别。
//
// PreferredRuntimeClass 刻意用「级别」而非 RuntimeClass 名称表达，
// 这样模板不绑定具体集群拓扑（docs/06 §1 的抽象层）。
type TemplateIsolation struct {
	// +kubebuilder:validation:Enum=simulated;runc;kata-fc;kata-clh
	Preferred IsolationLevel `json:"preferred,omitempty"`
	// +kubebuilder:validation:MaxItems=4
	Allowed []IsolationLevel `json:"allowed,omitempty"`
}

// TemplateScheduling 是调度相关默认值。
type TemplateScheduling struct {
	NodeSelector      map[string]string `json:"nodeSelector,omitempty"`
	PriorityClassName string            `json:"priorityClassName,omitempty"`
}

// IdleSignals 描述空闲判定使用的辅助信号。
//
// 主判据始终是心跳（Lease）；这些信号用于处理"心跳丢失但沙箱确实在被使用"
// 的矛盾情况 —— 此时保守选择"不回收"，并上报冲突指标用于调参（docs/03 §4.3）。
type IdleSignals struct {
	// +kubebuilder:default=true
	RequireHeartbeat bool `json:"requireHeartbeat,omitempty"`
	// +kubebuilder:default=true
	TreatNetworkActivityAsActive bool `json:"treatNetworkActivityAsActive,omitempty"`
	// TreatCPUAboveMilli 是"仍有活动"的 CPU 增量阈值。
	// +kubebuilder:default=50
	TreatCPUAboveMilli int64 `json:"treatCpuAboveMilli,omitempty"`
}

// SandboxTemplateSpec 是可复用的规格模板。
//
// 平台策展、租户不可写：模板决定了镜像、出口档位与允许的隔离级别，
// 是安全边界的一部分（docs/08 §8.4 镜像供应链）。
type SandboxTemplateSpec struct {
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`
	// +kubebuilder:validation:Enum=Always;IfNotPresent;Never
	// +kubebuilder:default=IfNotPresent
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	Command        []string        `json:"command,omitempty"`
	WorkingDir     string          `json:"workingDir,omitempty"`
	Env            []corev1.EnvVar `json:"env,omitempty"`
	StartupProbe   *corev1.Probe   `json:"startupProbe,omitempty"`
	ReadinessProbe *corev1.Probe   `json:"readinessProbe,omitempty"`

	// AllowedTiers 是该模板允许的档位白名单。
	// +kubebuilder:validation:MaxItems=4
	AllowedTiers []string `json:"allowedTiers,omitempty"`
	// +kubebuilder:default=small
	DefaultTier string `json:"defaultTier,omitempty"`

	Defaults TemplateDefaults `json:"defaults,omitempty"`

	// ResetHook 是复用前必跑的清理脚本（清工作区、杀残留进程、轮换凭据）。
	// 同租户复用必须经过它，否则视为配置错误。
	ResetHook *HookSpec `json:"resetHook,omitempty"`

	// +kubebuilder:validation:MaxItems=8
	Mounts []MountSpec `json:"mounts,omitempty"`

	// EgressProfile 引用平台策展的出口档位。
	EgressProfile string `json:"egressProfile,omitempty"`

	Isolation   TemplateIsolation  `json:"isolation,omitempty"`
	Scheduling  TemplateScheduling `json:"scheduling,omitempty"`
	IdleSignals IdleSignals        `json:"idleSignals,omitempty"`

	// SandboxEntrypointProcess 是识别"沙箱主进程"的进程名，
	// 节点侧 agent 用它定位需要冻结/统计的目标（避免误伤探针与辅助进程）。
	SandboxEntrypointProcess string `json:"sandboxEntrypointProcess,omitempty"`
}

// ObservedTier 是 ResourceOptimizer 回写的画像结果。
//
// 注意：控制器只写"建议"，实际档位数值的变更必须经过人工评审 + PR 修改
// 平台侧 tiers ConfigMap —— 资源规格变更必须有人负责（docs/07 §3.4）。
type ObservedTier struct {
	Tier          string       `json:"tier"`
	P95CPUMilli   int64        `json:"p95CpuMilli,omitempty"`
	P95MemMiB     int64        `json:"p95MemMiB,omitempty"`
	SampleCount   int64        `json:"sampleCount,omitempty"`
	LastEvaluated *metav1.Time `json:"lastEvaluated,omitempty"`
}

// TemplatePhase 是模板的生命周期状态。
//
// +kubebuilder:validation:Enum=Active;Deprecated;Retired
type TemplatePhase string

const (
	// TemplateActive 表示可被新沙箱引用。
	TemplateActive TemplatePhase = "Active"
	// TemplateDeprecated 表示仍可运行存量沙箱，但不允许新建。
	TemplateDeprecated TemplatePhase = "Deprecated"
	// TemplateRetired 表示不再允许任何引用。
	TemplateRetired TemplatePhase = "Retired"
)

// SandboxTemplateStatus 是模板观测状态。
type SandboxTemplateStatus struct {
	// +optional
	ObservedGeneration int64         `json:"observedGeneration,omitempty"`
	Phase              TemplatePhase `json:"phase,omitempty"`
	// +kubebuilder:validation:MaxItems=4
	// +listType=map
	// +listMapKey=tier
	ObservedTiers []ObservedTier     `json:"observedTiers,omitempty"`
	Conditions    []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sbt,scope=Cluster
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.image`
// +kubebuilder:printcolumn:name="DefaultTier",type=string,JSONPath=`.spec.defaultTier`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`

// SandboxTemplate 是平台策展的沙箱规格模板。
type SandboxTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SandboxTemplateSpec `json:"spec,omitempty"`
	// +optional
	Status SandboxTemplateStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SandboxTemplateList 包含 SandboxTemplate 列表。
type SandboxTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandboxTemplate{}, &SandboxTemplateList{})
}
