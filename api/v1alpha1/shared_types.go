package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---------------------------------------------------------------------------
// 枚举
// ---------------------------------------------------------------------------

// IsolationLevel 是沙箱要求的隔离级别。
//
// 注意：CRD 里**不写死** RuntimeClass 名称。具体映射由平台侧的
// isolation ConfigMap 解析（见 docs/06 §1），因此同一份 CR 可以在
// kind（simulated）与生产（kata-fc）之间平移，业务逻辑无需改动。
//
// +kubebuilder:validation:Enum=simulated;runc;kata-fc;kata-clh
type IsolationLevel string

const (
	// IsolationSimulated 仅用于本地开发：回落到集群默认运行时（runc），
	// 目的是让控制面逻辑可以在无 KVM 环境完整验证。
	IsolationSimulated IsolationLevel = "simulated"
	// IsolationRunc 用于可信负载。
	IsolationRunc IsolationLevel = "runc"
	// IsolationKataFC 是主力隔离方案：Kata + Firecracker。
	IsolationKataFC IsolationLevel = "kata-fc"
	// IsolationKataCLH 是需要更多设备能力时的折中：Kata + Cloud Hypervisor。
	IsolationKataCLH IsolationLevel = "kata-clh"
)

// Phase 是沙箱的生命周期阶段。
//
// 支点语义：Ready = 池中库存（未认领），Running = 已售出。
// 池水位就是 count(phase=Ready)。详见 docs/04 §5.1。
//
// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Running;Idle;Hibernating;Hibernated;Resuming;Terminating;Succeeded;Failed
type Phase string

const (
	PhasePending      Phase = "Pending"
	PhaseProvisioning Phase = "Provisioning"
	PhaseReady        Phase = "Ready"
	PhaseRunning      Phase = "Running"
	PhaseIdle         Phase = "Idle"
	PhaseHibernating  Phase = "Hibernating"
	PhaseHibernated   Phase = "Hibernated"
	PhaseResuming     Phase = "Resuming"
	PhaseTerminating  Phase = "Terminating"
	PhaseSucceeded    Phase = "Succeeded"
	PhaseFailed       Phase = "Failed"
)

// IsClaimedPhase 判断该阶段是否持有 claim（不变式 INV-2）。
func (p Phase) IsClaimedPhase() bool {
	switch p {
	case PhaseRunning, PhaseIdle, PhaseHibernating, PhaseHibernated, PhaseResuming:
		return true
	default:
		return false
	}
}

// IsTerminal 判断是否为终态。
func (p Phase) IsTerminal() bool {
	return p == PhaseSucceeded || p == PhaseFailed
}

// ReclaimPolicy 决定沙箱结束后的去向。
//
// ReturnToPool 仅允许**同租户**复用，且必须经过 SandboxTemplate 的 resetHook；
// 跨租户复用被硬编码禁止（安全边界，非配置项）。详见 docs/08 §9。
//
// +kubebuilder:validation:Enum=ReturnToPool;Destroy
type ReclaimPolicy string

const (
	ReclaimReturnToPool ReclaimPolicy = "ReturnToPool"
	ReclaimDestroy      ReclaimPolicy = "Destroy"
)

// OnHeartbeatLoss 决定心跳丢失时的处置策略。
//
// +kubebuilder:validation:Enum=ReclaimImmediately;Grace;Ignore
type OnHeartbeatLoss string

const (
	// OnHeartbeatLossReclaim 立即回收（适用于短生命周期、可重启的沙箱）。
	OnHeartbeatLossReclaim OnHeartbeatLoss = "ReclaimImmediately"
	// OnHeartbeatLossGrace 先转 Idle 观察，仍无活动再回收（默认，保守）。
	OnHeartbeatLossGrace OnHeartbeatLoss = "Grace"
	// OnHeartbeatLossIgnore 忽略心跳（仅靠 TTL 与空闲检测兜底）。
	OnHeartbeatLossIgnore OnHeartbeatLoss = "Ignore"
)

// RuntimeState 是休眠子状态。
//
// +kubebuilder:validation:Enum=Active;Freezing;Frozen;Restoring
type RuntimeState string

const (
	RuntimeActive    RuntimeState = "Active"
	RuntimeFreezing  RuntimeState = "Freezing"
	RuntimeFrozen    RuntimeState = "Frozen"
	RuntimeRestoring RuntimeState = "Restoring"
)

// HibernationMode 是休眠实现方式。
//
// 可行性结论见 docs/02 D7：Freeze 立即可用；Snapshot 需自研 shim（进阶 PoC）；
// DestroyAndRehydrate（状态外置 + 重建）是最可靠的内存回收路径。
//
// +kubebuilder:validation:Enum=Freeze;Snapshot;DestroyAndRehydrate
type HibernationMode string

const (
	HibernationFreeze              HibernationMode = "Freeze"
	HibernationSnapshot            HibernationMode = "Snapshot"
	HibernationDestroyAndRehydrate HibernationMode = "DestroyAndRehydrate"
)

// RecycleReason 是回收原因码。必须是枚举：recycleReason 会作为指标 label，
// 自由文本会导致基数爆炸（docs/08 §2.1）。
//
// +kubebuilder:validation:Enum=IdleTimeout;TTLExpired;ClientReleased;HardDeadline;HeartbeatLost;NodeLost;PoolDrain;StockRotation;QuotaRevoked;RuntimeError;ReuseLimitReached
type RecycleReason string

const (
	RecycleIdleTimeout       RecycleReason = "IdleTimeout"
	RecycleTTLExpired        RecycleReason = "TTLExpired"
	RecycleClientReleased    RecycleReason = "ClientReleased"
	RecycleHardDeadline      RecycleReason = "HardDeadline"
	RecycleHeartbeatLost     RecycleReason = "HeartbeatLost"
	RecycleNodeLost          RecycleReason = "NodeLost"
	RecyclePoolDrain         RecycleReason = "PoolDrain"
	RecycleStockRotation     RecycleReason = "StockRotation"
	RecycleQuotaRevoked      RecycleReason = "QuotaRevoked"
	RecycleRuntimeError      RecycleReason = "RuntimeError"
	RecycleReuseLimitReached RecycleReason = "ReuseLimitReached"
)

// Condition 类型常量（docs/04 §4）。
const (
	CondPodReady         = "PodReady"
	CondClaimed          = "Claimed"
	CondNetworkReady     = "NetworkReady"
	CondStorageReady     = "StorageReady"
	CondLeaseHealthy     = "LeaseHealthy"
	CondHibernated       = "Hibernated"
	CondResourceAdjusted = "ResourceAdjusted"
	CondScalingReady     = "ScalingReady"
	CondNodeCapacity     = "NodeCapacitySufficient"
	CondRuntimeAvailable = "RuntimeClassAvailable"
)

// ---------------------------------------------------------------------------
// 通用子结构
// ---------------------------------------------------------------------------

// ClaimRequestedBy 记录"谁申请了这个沙箱"，用于审计与配额回收。
type ClaimRequestedBy struct {
	// Tenant 是租户标识。该字段一旦写入不可变更（见 ClaimSpec 的 CEL 规则）。
	// +kubebuilder:validation:MinLength=1
	Tenant string `json:"tenant"`
	// SessionID 是业务侧会话标识，用于把平台指标与业务日志关联。
	SessionID string `json:"sessionId,omitempty"`
	// Principal 是发起者身份（如 user:alice）。
	Principal string `json:"principal,omitempty"`
	// RequestID 用于幂等与全链路追踪；会传播到 Pod label 与沙箱内环境变量。
	RequestID string `json:"requestId,omitempty"`
}

// ClaimSpec 表达"期望把该沙箱绑定给谁"。
//
// 设计要点（docs/04 §0 P4）：这是声明式而非命令式 —— 不是"请帮我认领"，
// 而是"这个沙箱属于谁"。因此控制器只需观察字段变化，不需要额外协调。
type ClaimSpec struct {
	RequestedBy ClaimRequestedBy `json:"requestedBy"`
	// PriorityClassName 决定抢占关系（sandbox-interactive / sandbox-batch）。
	PriorityClassName string `json:"priorityClassName,omitempty"`
	// HardDeadline 是必须释放的硬期限，优先级高于 TTL，用于业务侧 SLA 兜底。
	HardDeadline *metav1.Time `json:"hardDeadline,omitempty"`
}

// ClaimStatus 是认领结果的不可变快照。tenant 一旦写入不可变更（INV-4）。
type ClaimStatus struct {
	Tenant    string `json:"tenant,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	// RequestID 是本次认领的幂等键。
	//
	// 它存在的理由很具体：控制器靠它判定"这次 spec.claim 是否已经写入过 status"。
	// 少了它就只能用"claimRef 是否为空"来判断，而 release 后的重认领（同租户复用）
	// 会被当成同一次认领 —— claimedCount 漏计，maxClaimCount 形同虚设。
	RequestID string       `json:"requestId,omitempty"`
	ClaimedAt *metav1.Time `json:"claimedAt,omitempty"`
	// LeaseName 指向心跳 Lease 对象；命名约定为与沙箱同名（便于按名查找，避免全量 list）。
	LeaseName string `json:"leaseName,omitempty"`
	// HardDeadline 是 claim 时的硬期限副本，防止 spec 被改写后无法追溯到原始约定。
	HardDeadline *metav1.Time `json:"hardDeadline,omitempty"`
}

// MountSpec 声明沙箱内的一个挂载点。
//
// Firecracker 不支持设备热插拔，因此该列表在创建前必须完全确定，
// 列表本身是不可变字段（docs/06 §7.1 的硬约束）。
type MountSpec struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// MountPath 是 guest 内路径。
	// +kubebuilder:validation:MinLength=1
	MountPath string `json:"mountPath"`
	// EphemeralSize 非空时使用 CSI 临时卷；为空表示使用 emptyDir（virtio-fs）。
	// 选型理由见 docs/07 与 docs/06 §7.1：CSI 走 virtio-blk，性能优于 virtio-fs。
	EphemeralSize string `json:"ephemeralSize,omitempty"`
	// ObjectStorage 用于大工作区（S3 场景）：避免大卷挂载，且天然支持"状态外置"。
	ObjectStorage *ObjectStorageSpec `json:"objectStorage,omitempty"`
	// ReadOnlyHostCache 挂载节点本地依赖缓存（只读），用于压缩冷启动。
	ReadOnlyHostCache string `json:"readOnlyHostCache,omitempty"`
}

// ObjectStorageSpec 描述一个对象存储前缀。
type ObjectStorageSpec struct {
	Bucket string `json:"bucket"`
	Prefix string `json:"prefix,omitempty"`
	// +kubebuilder:validation:Enum=ro;rw
	// +kubebuilder:default=ro
	Mode string `json:"mode,omitempty"`
}

// LifecycleSpec 是生命周期与回收参数。默认值由 SandboxTemplate 提供。
type LifecycleSpec struct {
	// TTLSecondsAfterCreation 是兜底上限：无论如何不超过它，防泄漏的最终保险。
	// +kubebuilder:validation:Minimum=30
	// +kubebuilder:validation:Maximum=86400
	// +kubebuilder:default=7200
	TTLSecondsAfterCreation int32 `json:"ttlSecondsAfterCreation,omitempty"`

	// IdleTimeoutSeconds 是无活动多久后判定 Idle。0 表示禁用空闲回收（长任务场景）。
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=300
	IdleTimeoutSeconds int32 `json:"idleTimeoutSeconds,omitempty"`

	// HeartbeatGraceSeconds 是心跳过期后仍容忍多久（Lease TTL 之外的缓冲）。
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=600
	// +kubebuilder:default=60
	HeartbeatGraceSeconds int32 `json:"heartbeatGraceSeconds,omitempty"`

	// HibernateAfterIdleSeconds 是空闲多久后进入冻结。0 表示不冻结。
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=120
	HibernateAfterIdleSeconds int32 `json:"hibernateAfterIdleSeconds,omitempty"`

	// ProvisionTimeoutSeconds 是 Provisioning 阶段的超时，超时转 Failed。
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:default=120
	ProvisionTimeoutSeconds int32 `json:"provisionTimeoutSeconds,omitempty"`

	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=30
	TerminationGracePeriodSeconds int32 `json:"terminationGracePeriodSeconds,omitempty"`

	// +kubebuilder:validation:Enum=ReturnToPool;Destroy
	// +kubebuilder:default=ReturnToPool
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`

	// +kubebuilder:validation:Enum=ReclaimImmediately;Grace;Ignore
	// +kubebuilder:default=Grace
	OnHeartbeatLoss OnHeartbeatLoss `json:"onHeartbeatLoss,omitempty"`
}

// HibernationSpec 是休眠配置。
type HibernationSpec struct {
	// +kubebuilder:default=true
	Enabled bool `json:"enabled,omitempty"`
	// +kubebuilder:validation:Enum=Freeze;Snapshot;DestroyAndRehydrate
	// +kubebuilder:default=Freeze
	Mode HibernationMode `json:"mode,omitempty"`
}

// StateSpec 描述状态外置（L3）配置。
//
// 这是"内存可归还"的关键手段：把分布式难题（VM 快照/迁移）转化为幂等重建问题。
type StateSpec struct {
	// +kubebuilder:default=false
	Externalize bool `json:"externalize,omitempty"`
	// StateURI 在 Externalize=true 时必填（CEL 校验）。
	StateURI string `json:"stateURI,omitempty"`
}

// NetworkSpec 描述出口治理。注意这里引用的是"档位"而非域名列表 ——
// 域名白名单由平台侧 egress profile 渲染成 CiliumNetworkPolicy，
// 避免业务方绕过审批直接放通任意域名（docs/08 §7 T4/T12）。
type NetworkSpec struct {
	// EgressProfile 是平台策展的出口档位名（如 default-llm）。
	EgressProfile string `json:"egressProfile,omitempty"`
	// AdditionalEgress 是需要单独审批的补充出口，条数受限。
	// +kubebuilder:validation:MaxItems=5
	AdditionalEgress []string `json:"additionalEgress,omitempty"`
}

// ActivityStatus 记录活动观测，是空闲判定的辅助信号。
type ActivityStatus struct {
	// LastActiveAt 是最近一次确认有活动的时间，是空闲判定的基准之一。
	LastActiveAt *metav1.Time `json:"lastActiveAt,omitempty"`
	// ActiveConnections 是当前活跃连接数。
	ActiveConnections int32 `json:"activeConnections,omitempty"`
	// CPUIncrementMilli 是**上一个采样区间内**的 CPU 用量增量（毫核·秒）。
	//
	// 它与下面的 P95CPUmilli 是两个不同的量，因此必须是两个字段：
	// "刚刚用了多少 CPU" 适合判定"现在是否有人在干活"，
	// 而 P95 分位数是历史统计量，一旦某个沙箱曾经跑过高负载，
	// 它的 P95 会在整个统计窗口内居高不下 —— 用它做空闲判定会让沙箱
	// **永远显示为活跃、永远不被回收**，且日志上看不出任何异常。
	CPUIncrementMilli int64 `json:"cpuIncrementMilli,omitempty"`
	// P95CPUmilli / P95MemMiB 是画像用的历史分位数，由 ResourceOptimizer 消费，
	// **不参与**空闲判定。
	P95CPUmilli int64 `json:"p95CpuMilli,omitempty"`
	P95MemMiB   int64 `json:"p95MemMiB,omitempty"`
}

// HibernationStatus 是休眠子状态。
type HibernationStatus struct {
	State RuntimeState `json:"state,omitempty"`
	Since *metav1.Time `json:"since,omitempty"`
}

// SandboxMetrics 是用于成本核算与回收分析的计数。
type SandboxMetrics struct {
	ProvisionedAt *metav1.Time `json:"provisionedAt,omitempty"`
	// ClaimedCount 是复用次数，超过模板的 maxClaimCount 后强制销毁重建。
	ClaimedCount int32 `json:"claimedCount,omitempty"`
	// ColdPath 标记该沙箱是否走了冷路径（池未命中），用于命中率统计。
	ColdPath bool `json:"coldPath,omitempty"`
	// RecycleReason 是回收原因码。
	RecycleReason RecycleReason `json:"recycleReason,omitempty"`
}
