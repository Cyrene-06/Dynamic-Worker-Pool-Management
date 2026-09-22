package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
)

// TestEnsureSandboxID_DeterministicAndStable 守 INV-5。
//
// sandboxID 必须从 UID 派生而不是随机生成：随机 ID 只在"status 一定写得成功"
// 的假设下才不变。一旦写入失败（冲突、etcd 从备份恢复），下一轮就会生成
// 第二个 ID，而业务手里握着的是第一个 —— 症状是"用 ID 查沙箱返回 404，
// 但它明明还在跑"，几乎无法从现象反推原因。
func TestEnsureSandboxID_DeterministicAndStable(t *testing.T) {
	sbx := newSbx(sandboxv1alpha1.PhaseReady, time.Minute)
	sbx.UID = types.UID("11111111-2222-3333-4444-555555555555")

	if !ensureSandboxID(sbx) {
		t.Fatalf("首次调用应生成 sandboxID")
	}
	first := sbx.Status.SandboxID
	if first == "" {
		t.Fatalf("sandboxID 为空")
	}

	// 重复调用（模拟控制器重启后重新 reconcile）不得改变它。
	if ensureSandboxID(sbx) {
		t.Fatalf("重复调用不应报告变更")
	}
	if sbx.Status.SandboxID != first {
		t.Fatalf("sandboxID 被改写：%q -> %q（违反 INV-5）", first, sbx.Status.SandboxID)
	}

	// 从同一 UID 派生的值必须相同 —— 这是"status 丢失也能恢复出同一个 ID"
	// 的保证，也正是它优于随机数的原因。
	other := newSbx(sandboxv1alpha1.PhaseReady, time.Minute)
	other.UID = sbx.UID
	ensureSandboxID(other)
	if other.Status.SandboxID != first {
		t.Fatalf("同一 UID 派生出不同 ID：%q vs %q", other.Status.SandboxID, first)
	}
}

func TestBindClaimStatus_WarmPathRecordsContract(t *testing.T) {
	sbx := newSbx(sandboxv1alpha1.PhaseReady, time.Hour)
	sbx.Spec.Claim = nil
	sbx.Status.Metrics.ClaimedCount = 0

	dl := metav1.NewTime(t0.Add(30 * time.Minute))
	sbx.Spec.Claim = &sandboxv1alpha1.ClaimSpec{
		RequestedBy: sandboxv1alpha1.ClaimRequestedBy{
			Tenant: "t-1", SessionID: "sess-1", RequestID: "req-1",
		},
		HardDeadline: &dl,
	}

	r := &SandboxReconciler{}
	b := r.bindClaimStatus(sbx, t0)
	if !b.NewlyBound {
		t.Fatalf("应识别为一次新的认领")
	}

	ref := sbx.Status.ClaimRef
	if ref == nil {
		t.Fatalf("claimRef 未被写入")
	}
	if ref.Tenant != "t-1" || ref.SessionID != "sess-1" || ref.RequestID != "req-1" {
		t.Fatalf("claimRef 内容不正确: %+v", ref)
	}
	if ref.ClaimedAt == nil || !ref.ClaimedAt.Time.Equal(t0) {
		t.Fatalf("claimedAt = %v，期望 %v", ref.ClaimedAt, t0)
	}
	if ref.LeaseName != sbx.Name {
		t.Fatalf("leaseName = %q，期望与沙箱同名 %q", ref.LeaseName, sbx.Name)
	}

	// 硬期限必须是**副本**：spec 之后被改写不应影响这份契约快照。
	dl.Time = t0.Add(999 * time.Hour)
	if ref.HardDeadline.Time.Equal(dl.Time) {
		t.Fatalf("hardDeadline 是共享指针而非副本：契约会被追溯修改")
	}

	if sbx.Status.Metrics.ClaimedCount != 1 {
		t.Fatalf("claimedCount = %d，期望 1", sbx.Status.Metrics.ClaimedCount)
	}
	// 认领发生在 Ready 阶段 → 命中库存 → 不是冷路径。
	if sbx.Status.Metrics.ColdPath {
		t.Fatalf("从 Ready 认领应记为热路径")
	}

	// 重复观察同一次认领：不得重复计数、不得刷新 claimedAt。
	// 刷新 claimedAt 会让空闲计时被无限重置 —— 那等于彻底关闭空闲回收。
	later := t0.Add(10 * time.Minute)
	b2 := r.bindClaimStatus(sbx, later)
	if b2.NewlyBound || b2.Changed() {
		t.Fatalf("同一次认领的重复观察不应报告变更")
	}
	if sbx.Status.Metrics.ClaimedCount != 1 {
		t.Fatalf("claimedCount 被重复计数：%d", sbx.Status.Metrics.ClaimedCount)
	}
	if !sbx.Status.ClaimRef.ClaimedAt.Time.Equal(t0) {
		t.Fatalf("claimedAt 被刷新为 %v，空闲计时会被无限重置", sbx.Status.ClaimRef.ClaimedAt)
	}
}

// TestBindClaimStatus_ReclaimAfterReleaseCounts 守住"release 后重认领要重新计数"。
//
// 若用"claimRef 是否为空"来判断新认领，release 后重认领同一实例会漏计，
// maxClaimCount 因此形同虚设 —— 而它正是防止一个实例被无限复用的机制。
func TestBindClaimStatus_ReclaimAfterReleaseCounts(t *testing.T) {
	sbx := newSbx(sandboxv1alpha1.PhaseReady, time.Minute)
	sbx.Spec.Claim = &sandboxv1alpha1.ClaimSpec{
		RequestedBy: sandboxv1alpha1.ClaimRequestedBy{Tenant: "t-1", RequestID: "req-1"},
	}

	r := &SandboxReconciler{}
	r.bindClaimStatus(sbx, t0)
	if sbx.Status.Metrics.ClaimedCount != 1 {
		t.Fatalf("首次认领后 claimedCount = %d", sbx.Status.Metrics.ClaimedCount)
	}

	// 业务释放：claimRef **保留**（审计记录），而不是清空。
	sbx.Spec.Claim = nil
	if b := r.bindClaimStatus(sbx, t0.Add(time.Minute)); b.NewlyBound {
		t.Fatalf("释放不应被当作新认领")
	}
	if sbx.Status.ClaimRef == nil {
		t.Fatalf("释放后应保留 claimRef 作为审计记录")
	}

	// 同租户以新 RequestID 重新认领同一实例 → 必须重新计数。
	sbx.Spec.Claim = &sandboxv1alpha1.ClaimSpec{
		RequestedBy: sandboxv1alpha1.ClaimRequestedBy{Tenant: "t-1", RequestID: "req-2"},
	}
	if b := r.bindClaimStatus(sbx, t0.Add(2*time.Minute)); !b.NewlyBound {
		t.Fatalf("以新 RequestID 重认领应被识别为新认领")
	}
	if sbx.Status.Metrics.ClaimedCount != 2 {
		t.Fatalf("claimedCount = %d，期望 2（漏计会让 maxClaimCount 失效）",
			sbx.Status.Metrics.ClaimedCount)
	}
}

// TestBindClaimStatus_ColdPathFlagged 守命中率指标的输入。
//
// 冷路径的判定只能发生在"认领时沙箱还不是 Ready"这一瞬间。
// 若判错，命中率会偏离真实值，而它正是池水位调参的输入 ——
// 一个被污染的信号会让调参朝错误方向走。
func TestBindClaimStatus_ColdPathFlagged(t *testing.T) {
	sbx := newSbx(sandboxv1alpha1.PhaseProvisioning, time.Minute)
	sbx.Spec.Claim = &sandboxv1alpha1.ClaimSpec{
		RequestedBy: sandboxv1alpha1.ClaimRequestedBy{Tenant: "t-1", RequestID: "req-cold"},
	}

	r := &SandboxReconciler{}
	b := r.bindClaimStatus(sbx, t0)
	if !b.NewlyBound {
		t.Fatalf("应识别为新认领")
	}
	if !sbx.Status.Metrics.ColdPath {
		t.Fatalf("在 Provisioning 阶段认领应记为冷路径")
	}
}

func TestMarkProvisioned_OnlyOnce(t *testing.T) {
	sbx := newSbx(sandboxv1alpha1.PhaseProvisioning, time.Minute)
	if !markProvisioned(sbx, t0) {
		t.Fatalf("首次应写入 provisionedAt")
	}
	if !sbx.Status.Metrics.ProvisionedAt.Time.Equal(t0) {
		t.Fatalf("provisionedAt = %v，期望 %v", sbx.Status.Metrics.ProvisionedAt, t0)
	}
	// 重复调用必须保留首次时间：冷启动 SLO 衡量的是"从创建到就绪"，
	// 若被后续 reconcile 反复刷新，它就退化成"最后一次 reconcile 的时间"，
	// 从而永远显示为"刚刚就绪"。
	if markProvisioned(sbx, t0.Add(time.Hour)) {
		t.Fatalf("重复调用不应报告变更")
	}
	if !sbx.Status.Metrics.ProvisionedAt.Time.Equal(t0) {
		t.Fatalf("provisionedAt 被刷新")
	}
}

// TestApplyTemplateDefaults_Precedence 守"沙箱显式值 > 模板默认值 > 代码默认值"。
//
// 三者关系必须确定，否则会出现"改了模板却不生效"这种无从下手的现象。
func TestApplyTemplateDefaults_Precedence(t *testing.T) {
	tmpl := &sandboxv1alpha1.SandboxTemplate{}
	tmpl.Spec.Defaults.TTLSecondsAfterCreation = 1800
	tmpl.Spec.Defaults.IdleTimeoutSeconds = 600
	tmpl.Spec.Defaults.HeartbeatGraceSeconds = 120
	tmpl.Spec.Defaults.ReclaimPolicy = string(sandboxv1alpha1.ReclaimDestroy)
	tmpl.Spec.Defaults.MaxClaimCount = 7

	// 沙箱未指定 lifecycle → 全部取模板值。
	bare := newSbx(sandboxv1alpha1.PhaseReady, time.Minute)
	lc := ApplyTemplateDefaults(bare, tmpl)
	if lc.TTLSecondsAfterCreation != 1800 || lc.IdleTimeoutSeconds != 600 ||
		lc.HeartbeatGraceSeconds != 120 || lc.ReclaimPolicy != sandboxv1alpha1.ReclaimDestroy {
		t.Fatalf("模板默认值未生效: %+v", lc)
	}
	if lc.MaxClaimCount != 7 {
		t.Fatalf("MaxClaimCount = %d，期望 7", lc.MaxClaimCount)
	}

	// 沙箱显式指定 → 覆盖模板。
	explicit := newSbx(sandboxv1alpha1.PhaseReady, time.Minute)
	explicit.Spec.Lifecycle = &sandboxv1alpha1.LifecycleSpec{
		TTLSecondsAfterCreation: 300,
		ReclaimPolicy:           sandboxv1alpha1.ReclaimReturnToPool,
	}
	lc = ApplyTemplateDefaults(explicit, tmpl)
	if lc.TTLSecondsAfterCreation != 300 {
		t.Fatalf("沙箱显式 TTL 未覆盖模板：%d", lc.TTLSecondsAfterCreation)
	}
	if lc.ReclaimPolicy != sandboxv1alpha1.ReclaimReturnToPool {
		t.Fatalf("沙箱显式 reclaimPolicy 未覆盖模板：%q", lc.ReclaimPolicy)
	}
	// 未被沙箱指定的项仍取模板值。
	if lc.HeartbeatGraceSeconds != 120 {
		t.Fatalf("未指定项应取模板值：%d", lc.HeartbeatGraceSeconds)
	}

	// idleTimeoutSeconds = 0 是**语义有效值**（禁用空闲回收）。
	// 当 spec.lifecycle 存在且显式写了 0，就不能被模板值改回 600 ——
	// 那会把一次有意的"长任务禁用回收"悄悄变成"5 分钟后回收"。
	disabled := newSbx(sandboxv1alpha1.PhaseReady, time.Minute)
	disabled.Spec.Lifecycle = &sandboxv1alpha1.LifecycleSpec{IdleTimeoutSeconds: 0}
	lc = ApplyTemplateDefaults(disabled, tmpl)
	if lc.IdleTimeoutSeconds != 0 {
		t.Fatalf("显式的 idleTimeoutSeconds=0 被覆盖为 %d；这会让长任务被误回收",
			lc.IdleTimeoutSeconds)
	}

	// 模板为空时退回代码默认值，不能返回零值。
	lc = ApplyTemplateDefaults(bare, nil)
	if lc.TTLSecondsAfterCreation != DefaultTTLSeconds || lc.IdleTimeoutSeconds != DefaultIdleTimeoutSeconds {
		t.Fatalf("无模板时应退回代码默认值: %+v", lc)
	}
	if lc.ReclaimPolicy != sandboxv1alpha1.ReclaimReturnToPool {
		t.Fatalf("无模板时 reclaimPolicy = %q，期望 ReturnToPool", lc.ReclaimPolicy)
	}
}
