package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SandboxPoolScaling 是池水位策略。
//
// 算法与推导见 docs/05 §3：前馈（EWMA + 趋势）+ 反馈（命中率缺口）
// + 平方根安全库存，外面再套冷却期、滞回与限速三层阻尼。
type SandboxPoolScaling struct {
	// MinWarm 是低峰保底库存：保证低峰也走热路径。
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=2
	MinWarm int32 `json:"minWarm,omitempty"`

	// MaxWarm 是上限，防止池失控烧钱。
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=100
	MaxWarm int32 `json:"maxWarm,omitempty"`

	// TargetWarmBuffer 是在预测需求之外额外维持的库存缓冲。
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=10
	TargetWarmBuffer int32 `json:"targetWarmBuffer,omitempty"`

	// MaxProvisionPerSecond / MaxReclaimPerSecond 是扩缩限速，
	// 保护 API Server 与 CNI 不被批量操作打爆。
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=50
	MaxProvisionPerSecond int32 `json:"maxProvisionPerSecond,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=30
	MaxReclaimPerSecond int32 `json:"maxReclaimPerSecond,omitempty"`

	// ReplenishSeconds 是"决定扩容 → 库存 Ready"的实测补货时间。
	// 这是安全库存公式里最敏感的参数：平方根关系意味着补货时间减半、所需缓冲减少近半。
	// 主导项是"节点是否已就绪 + L1 镜像是否已预热"，因此该值必须来自实测而非估计。
	// +kubebuilder:validation:Minimum=5
	// +kubebuilder:default=90
	ReplenishSeconds int32 `json:"replenishSeconds,omitempty"`

	// TargetHitRatioPermille 是命中率目标，以千分比表示（900 = 90%）。
	//
	// 用整数千分比而不是 float：Kubernetes API 约定要求避免浮点数
	// （不同语言与中间层对 JSON number 的表示不一致），而且控制回路参数
	// 用整数才能保证单测与 A/B 对比可精确复现。
	// +kubebuilder:validation:Minimum=500
	// +kubebuilder:validation:Maximum=999
	// +kubebuilder:default=900
	TargetHitRatioPermille int32 `json:"targetHitRatioPermille,omitempty"`

	// Dampening 是防抖参数。这是池化最容易翻车的地方（docs/05 §3.4）。
	Dampening DampeningSpec `json:"dampening,omitempty"`

	// PauseScaling 是运维熔断开关：冻结所有自动扩缩，便于排障。
	// +kubebuilder:default=false
	PauseScaling bool `json:"pauseScaling,omitempty"`
}

// DampeningSpec 是防抖参数。
type DampeningSpec struct {
	// ScaleUpCooldownSeconds 应显著小于缩容冷却：扩容要快，缩容要慢。
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=15
	ScaleUpCooldownSeconds int32 `json:"scaleUpCooldownSeconds,omitempty"`

	// ScaleDownCooldownSeconds 故意远大于扩容冷却（保守优先）。
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=180
	ScaleDownCooldownSeconds int32 `json:"scaleDownCooldownSeconds,omitempty"`

	// HysteresisRatioPermille 是滞回带宽度（千分比）：只有偏离目标超过该比例才动作，
	// 避免水位在目标附近反复进出。
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=500
	// +kubebuilder:default=100
	HysteresisRatioPermille int32 `json:"hysteresisRatioPermille,omitempty"`

	// EWMAAlphaPermille 是需求预测的平滑系数（千分比），越小越平滑。
	// +kubebuilder:validation:Minimum=50
	// +kubebuilder:validation:Maximum=1000
	// +kubebuilder:default=300
	EWMAAlphaPermille int32 `json:"ewmaAlphaPermille,omitempty"`
}

// StockPolicy 是库存健康策略。
type StockPolicy struct {
	// MaxStockAgeSeconds 是库存最长未认领时间，超过则轮换销毁，
	// 避免镜像过期与长驻实例的内存缓慢增长累积。
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:default=3600
	MaxStockAgeSeconds int32 `json:"maxStockAgeSeconds,omitempty"`

	// StockReadyTimeoutSeconds 是库存就绪超时（对应 Provisioning 阶段超时）。
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:default=120
	StockReadyTimeoutSeconds int32 `json:"stockReadyTimeoutSeconds,omitempty"`
}

// DegradationPolicy 是运行时不可用时的降级策略。
//
// +kubebuilder:validation:Enum=FailFast;FallbackToRunc;PauseScaling
type DegradationPolicy string

const (
	// DegradationFailFast 拒绝新建并告警（安全优先）。
	DegradationFailFast DegradationPolicy = "FailFast"
	// DegradationFallbackToRunc 降级到 runc 继续服务（可用性优先，但隔离强度下降）。
	DegradationFallbackToRunc DegradationPolicy = "FallbackToRunc"
	// DegradationPauseScaling 停止扩缩但保留现有库存（等待人工介入）。
	DegradationPauseScaling DegradationPolicy = "PauseScaling"
)

// PoolDegradation 是池的降级配置。
type PoolDegradation struct {
	// +kubebuilder:validation:Enum=FailFast;FallbackToRunc;PauseScaling
	// +kubebuilder:default=FailFast
	OnRuntimeUnavailable DegradationPolicy `json:"onRuntimeUnavailable,omitempty"`

	// DisableColdPath 为 true 时只服务池内申请，保护节点不被冷启动压垮。
	// +kubebuilder:default=false
	DisableColdPath bool `json:"disableColdPath,omitempty"`

	// OnNodeFailuresAbovePercent 是节点故障率阈值，超过则暂停扩容，避免雪崩。
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=30
	OnNodeFailuresAbovePercent int32 `json:"onNodeFailuresAbovePercent,omitempty"`
}

// PriorityBand 把一个优先级类别限定到一定的库存份额。
//
// 注意：跨 band 的"抢占"实际是"回收 + 重建"（Pod 的 PriorityClass 不可变），
// 代价约 1.5–2.5s。因此 band 划分必须靠预测，而不是靠抢占来临时调剂。
type PriorityBand struct {
	// +kubebuilder:validation:MinLength=1
	ClassName string `json:"className"`
	// MinWarmSharePermille 是该 band 的最小库存份额（千分比，700 = 70%）。
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000
	MinWarmSharePermille int32 `json:"minWarmSharePermille,omitempty"`
}

// PoolDrain 是排空配置，用于升级前停止认领并回收库存。
type PoolDrain struct {
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`
	// Reason 记录排空原因（如 kata 升级），会写入 status 与事件便于追溯。
	Reason string `json:"reason,omitempty"`
	// GraceSeconds 是停止认领后等待现有会话自然结束的最长时间。
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=600
	GraceSeconds int32 `json:"graceSeconds,omitempty"`
}

// ProtectModeSpec 是雪崩保护（docs/05 §5）的配置。
//
// 为什么它不是一个可选优化：容量危机下系统会**自我放大** ——
// 扩容/重试请求压垮 API Server，更多超时，更多重试，直到彻底不可用。
// 保护模式是打断这个正反馈的唯一机制，而它必须在阈值上保守、
// 在退路上明确，否则只会在真正的危机里把问题搞得更糟。
type ProtectModeSpec struct {
	// Enabled 关闭后完全不做保护模式判定（留一个显式的开关，
	// 而不是让运维去把阈值改成不可能达到的值）。
	//
	// 用 *bool 而不是 bool：这个字段的默认值是 true，而 bool 的零值
	// 与显式 false 无法区分 —— 任何不经过 API Server 默认化的写入
	// （单测构造对象、部分 SSA 场景、直接改对象）都会静默关掉雪崩保护。
	// "安全机制被静默关闭"是最不该发生的一类问题，因此多一个指针是值得的。
	// +kubebuilder:default=true
	Enabled *bool `json:"enabled,omitempty"`

	// FailureRatePercent 是 provision 失败率阈值（百分比）。
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=30
	FailureRatePercent int32 `json:"failureRatePercent,omitempty"`

	// MinSamples 是判定失败率所需的最小样本数。
	//
	// 没有它的保护模式会在“1 次尝试、1 次失败”上立刻触发，而保护模式
	// 本身是有代价的（拒绝低优申请）—— 一个刚扩容、库存尚在创建中的池
	// 恰好就是这种形态，于是正常扩容会被自己的保护机制打断。
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=10
	MinSamples int32 `json:"minSamples,omitempty"`

	// ApiserverLatencyThresholdMs 是 API Server P99 阈值（毫秒）。
	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:default=2000
	ApiserverLatencyThresholdMs int32 `json:"apiserverLatencyThresholdMs,omitempty"`

	// HoldSeconds 是进入保护后的最短持续时间（滞回，防抖动）。
	//
	// 没有滞回的保护模式会变成振荡器：危机时进、一缓解就出，
	// 每次进出都伴随一批请求被拒绝/放行。而它的目的是争取恢复时间，
	// 不是逐秒反映健康度。
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:default=120
	HoldSeconds int32 `json:"holdSeconds,omitempty"`

	// ScaleUpThrottlePermille 是保护期内扩容限速的比例（500 = 降至 50%）。
	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:validation:Maximum=1000
	// +kubebuilder:default=500
	ScaleUpThrottlePermille int32 `json:"scaleUpThrottlePermille,omitempty"`

	// MaxQueueDepth 是保护期内高优申请的排队上限（docs/05 §5 的 200）。
	//
	// 当前接入层是**快速失败**而非排队（docs/10 Q8 尚未结案），
	// 因此这个值暂时只作为对外承诺的上限记录在 API 里：
	// 将来若实现排队，它必须被真正执行；若继续快速失败，它应被删除。
	// 保留一个不会被读取的字段是有代价的 —— 它会被误认为已经生效。
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=200
	MaxQueueDepth int32 `json:"maxQueueDepth,omitempty"`
}

// SandboxPoolSpec 是池策略。
//
// 池按「隔离级别 + 规格档位」划分，而不是按租户划分：池的物理意义是资源规格，
// 按租户预热会导致小租户池命中率极差（docs/03 §3.1 的取舍记录）。
type SandboxPoolSpec struct {
	// Isolation 是该池统一的隔离级别。
	// +kubebuilder:validation:Enum=simulated;runc;kata-fc;kata-clh
	Isolation IsolationLevel `json:"isolation"`
	// RuntimeClassName 可在 Kata 升级期间覆盖该池的具体版本化 RuntimeClass。
	// 留空时使用隔离级别映射；控制器会校验 handler 与隔离级别一致。
	RuntimeClassName string `json:"runtimeClassName,omitempty"`

	// NodePoolSelector 把池绑定到节点池（由 Karpenter/CA 维护）。
	NodePoolSelector map[string]string `json:"nodePoolSelector,omitempty"`

	// TemplateRef 是该池的默认规格模板（集群级对象）。
	TemplateRef NameRef `json:"templateRef"`

	// Tier 是该池统一的资源档位。
	// +kubebuilder:validation:Enum=tiny;small;medium;large
	Tier string `json:"tier"`

	Scaling     SandboxPoolScaling `json:"scaling,omitempty"`
	StockPolicy StockPolicy        `json:"stockPolicy,omitempty"`
	Degradation PoolDegradation    `json:"degradation,omitempty"`

	// +kubebuilder:validation:MaxItems=4
	PriorityBands []PriorityBand `json:"priorityBands,omitempty"`

	Drain PoolDrain `json:"drain,omitempty"`

	// ProtectMode 是雪崩保护配置（docs/05 §5）。
	ProtectMode ProtectModeSpec `json:"protectMode,omitempty"`
}

// SandboxPoolStatus 是池水位与决策的观测状态。
type SandboxPoolStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Warm 是未认领且 Ready 的库存数 —— 池水位的唯一定义来源。
	Warm int32 `json:"warm,omitempty"`
	// Claimed 是已认领数。
	Claimed int32 `json:"claimed,omitempty"`
	// Inflight 是正在创建（Pending/Provisioning）的库存数，用于避免重复扩容决策。
	Inflight int32 `json:"inflight,omitempty"`
	Failed   int32 `json:"failed,omitempty"`
	// Draining 是被标记排空、已从水位统计中排除的库存数。
	Draining int32 `json:"draining,omitempty"`

	Target int32 `json:"target,omitempty"`
	// SaturationPermille 是饱和度 claimed/(warm+claimed)，千分比。
	SaturationPermille int32 `json:"saturationPermille,omitempty"`
	// HitRatio1hPermille 是 1 小时滑动窗口的热路径命中率，千分比。
	HitRatio1hPermille int32 `json:"hitRatio1hPermille,omitempty"`
	ClaimLatencyP95Ms  int64 `json:"claimLatencyP95Ms,omitempty"`
	PredictedDemand    int32 `json:"predictedDemand,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastScaleDecision 记录最近一次扩缩决策，用于排查振荡。
	LastScaleDecision *ScaleDecision `json:"lastScaleDecision,omitempty"`

	// ProtectMode 是保护模式的当前状态（docs/05 §5）。
	//
	// 放在池的 status 里而不是让各组件各自算：控制面与接入层必须看到
	// **同一个事实**。否则会出现“控制器认为已恢复、gateway 仍在拒绝”这类
	// 从任一组件都解释不了的现象，而排查它要花掉危机里最宝贵的时间。
	ProtectMode ProtectModeStatus `json:"protectMode,omitempty"`
}

// ProtectModeStatus 是保护模式的当前状态。
type ProtectModeStatus struct {
	// Active 表示保护模式正在生效。
	Active bool `json:"active,omitempty"`
	// Reason 是进入保护的原因码（枚举，避免成为高基数 label）。
	Reason string `json:"reason,omitempty"`
	// Since 是本次（或最近一次）进入保护的时刻。
	Since *metav1.Time `json:"since,omitempty"`
	// Until 是滞回期结束时刻；在此之前即使触发条件消失也保持保护。
	Until *metav1.Time `json:"until,omitempty"`
	// Trips 是累计进入次数。它比 active 更有信息量：短时间内持续增长
	// 说明阈值偏低或故障未真正恢复，而 active=true 这两个原因长得一样。
	Trips int64 `json:"trips,omitempty"`
	// FailureRatePermille 是本次判定用的失败率（千分比），便于事后复盘
	// “当时到底多糟”。
	FailureRatePermille int32 `json:"failureRatePermille,omitempty"`
}

// ScaleDecision 是一次扩缩决策的快照。
type ScaleDecision struct {
	At *metav1.Time `json:"at,omitempty"`
	// Reason 必须是枚举化的字符串，避免指标基数爆炸。
	Reason string `json:"reason,omitempty"`
	Delta  int32  `json:"delta,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sbp,scope=Cluster
// +kubebuilder:printcolumn:name="Isolation",type=string,JSONPath=`.spec.isolation`
// +kubebuilder:printcolumn:name="Tier",type=string,JSONPath=`.spec.tier`
// +kubebuilder:printcolumn:name="Warm",type=integer,JSONPath=`.status.warm`
// +kubebuilder:printcolumn:name="Target",type=integer,JSONPath=`.status.target`
// +kubebuilder:printcolumn:name="HitRatio1hPermille",type=integer,JSONPath=`.status.hitRatio1hPermille`

// SandboxPool 是一组同规格沙箱的容量池。
type SandboxPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SandboxPoolSpec `json:"spec,omitempty"`
	// +optional
	Status SandboxPoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SandboxPoolList 包含 SandboxPool 列表。
type SandboxPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandboxPool{}, &SandboxPoolList{})
}
