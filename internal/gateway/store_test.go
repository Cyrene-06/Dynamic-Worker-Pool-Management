package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
	"github.com/Cyrene-06/Dynamic-Worker-Pool-Management/internal/claim"
	"github.com/Cyrene-06/Dynamic-Worker-Pool-Management/test/envtest"
)

const (
	tenantA = "t-1"
	tenantB = "t-2"
	tokenA  = devToken
	tokenB  = "fedcba9876543210fedcba9876543210"
	poolA   = "fc-small"
)

// newTestServer 起一个接通真实 API Server 的 gateway。
//
// 用 envtest 而不是 mock client：这条路径的价值恰恰在于它经过真实的
// 乐观锁（resourceVersion 前置条件）、真实的状态子资源与真实的
// label selector 语义。用 mock 会把这些全部替换成"我认为它应该怎样"，
// 而那正是最需要被验证的部分。
func newTestServer(t *testing.T) (*Server, client.Client) {
	t.Helper()
	env := envtest.Start(t)

	sch := runtime.NewScheme()
	utilruntime.Must(scheme.AddToScheme(sch))
	utilruntime.Must(sandboxv1alpha1.AddToScheme(sch))

	toks, err := NewStaticTenantTokens(map[string]string{
		tokenA: tenantA,
		tokenB: tenantB,
	})
	if err != nil {
		t.Fatalf("构造令牌表失败: %v", err)
	}
	issuer, err := NewAccessTokenIssuer(devToken, 10*time.Minute)
	if err != nil {
		t.Fatalf("构造签发器失败: %v", err)
	}

	store := NewStore(env.Client, issuer)
	store.HeartbeatIntervalSeconds = 60
	store.Claimer = &claim.Claimer{
		Client:    env.Client,
		Namespace: sandboxv1alpha1.NamespacePool,
		// 并发冲突测试里候选很少，但生产默认值不应因为测试而变。
		MaxAttempts: 4,
	}

	srv := NewServer(store)
	srv.Auth = &HeaderAuthenticator{Source: toks}
	srv.Quotas = DefaultQuotaTable()
	srv.IsAdmin = StaticAdminSet([]string{"ops"})
	srv.QuotaChecker = &QuotaChecker{Client: env.Client}
	srv.CreateTimeout = 10 * time.Second

	// 建池与模板，供冷路径使用（热路径用不到它们）。
	createPool(t, env.Client)
	return srv, env.Client
}

func createPool(t *testing.T, c client.Client) {
	t.Helper()
	ctx := context.Background()

	tmpl := &sandboxv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "fc-base"},
		Spec: sandboxv1alpha1.SandboxTemplateSpec{
			Image: "busybox:1.36",
			Defaults: sandboxv1alpha1.TemplateDefaults{
				TTLSecondsAfterCreation: 1800,
				MaxClaimCount:           3,
			},
		},
	}
	if err := c.Create(ctx, tmpl); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("创建模板失败: %v", err)
	}

	pool := &sandboxv1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: poolA},
		Spec: sandboxv1alpha1.SandboxPoolSpec{
			Isolation:   sandboxv1alpha1.IsolationSimulated,
			TemplateRef: sandboxv1alpha1.NameRef{Name: "fc-base"},
			Tier:        "small",
		},
	}
	if err := c.Create(ctx, pool); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("创建池失败: %v", err)
	}
}

// stock 造一个"池中库存"沙箱：phase=Ready、无 claim、带池标签。
func stock(t *testing.T, c client.Client, name string, tenant string) *sandboxv1alpha1.AgentSandbox {
	t.Helper()
	ctx := context.Background()

	labels := map[string]string{
		sandboxv1alpha1.LabelPool:      poolA,
		sandboxv1alpha1.LabelTier:      "small",
		sandboxv1alpha1.LabelTemplate:  "fc-base",
		sandboxv1alpha1.LabelIsolation: string(sandboxv1alpha1.IsolationSimulated),
		sandboxv1alpha1.LabelRole:      sandboxv1alpha1.RoleSandbox,
		sandboxv1alpha1.LabelClaimed:   "false",
	}
	if tenant != "" {
		labels[sandboxv1alpha1.LabelTenant] = tenant
	}

	sbx := &sandboxv1alpha1.AgentSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: sandboxv1alpha1.NamespacePool,
			Labels:    labels,
		},
		Spec: sandboxv1alpha1.AgentSandboxSpec{
			PoolRef:     sandboxv1alpha1.NameRef{Name: poolA},
			TemplateRef: sandboxv1alpha1.NameRef{Name: "fc-base"},
			Tier:        "small",
			Isolation:   sandboxv1alpha1.IsolationSimulated,
			Lifecycle:   &sandboxv1alpha1.LifecycleSpec{TTLSecondsAfterCreation: 1800},
		},
	}
	if err := c.Create(ctx, sbx); err != nil {
		t.Fatalf("创建库存失败: %v", err)
	}
	// phase=Ready 必须走状态子资源 —— 这也是"库存"这个语义的唯一来源。
	sbx.Status.Phase = sandboxv1alpha1.PhaseReady
	if err := c.Status().Update(ctx, sbx); err != nil {
		t.Fatalf("写库存 status 失败: %v", err)
	}
	return sbx
}

// do 发一个请求。
func do(t *testing.T, h http.Handler, method, path, token, idemKey, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if idemKey != "" {
		r.Header.Set("Idempotency-Key", idemKey)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// TestFlow_ClaimGetRenewRelease 走一遍完整的业务生命周期。
//
// 这条测试固定的是**对外契约**：状态码、字段名、以及"谁能看到谁的沙箱"。
// 任何一个变了都是业务可感知的破坏性变更，而它们最容易在重构中被无声改掉。
func TestFlow_ClaimGetRenewRelease(t *testing.T) {
	srv, c := newTestServer(t)
	h := srv.Handler()
	ctx := context.Background()

	stock(t, c, "warm-1", tenantA)

	// ---- 申请（热路径） ----
	rec := do(t, h, http.MethodPost, "/v1/sandboxes", tokenA, "idem-1",
		`{"pool":"fc-small","ttlSeconds":600}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("申请状态码 = %d，期望 201，body=%s", rec.Code, rec.Body.String())
	}
	var created SandboxResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if created.ID == "" {
		t.Fatalf("响应缺少 id")
	}
	if created.Path != claim.PathWarm {
		t.Fatalf("path = %q，期望 %q", created.Path, claim.PathWarm)
	}
	if created.Access.Token == "" {
		t.Fatalf("响应缺少接入令牌")
	}
	if created.RenewIntervalSeconds != 60 {
		t.Fatalf("renewIntervalSeconds = %d，期望 60（与控制器的心跳契约一致）",
			created.RenewIntervalSeconds)
	}

	// 契约快照必须落进 status（claimedAt 是空闲判定的基准）。
	var got sandboxv1alpha1.AgentSandbox
	if err := c.Get(ctx, types.NamespacedName{
		Namespace: sandboxv1alpha1.NamespacePool, Name: created.ID}, &got); err != nil {
		t.Fatalf("读取沙箱失败: %v", err)
	}
	if got.Spec.Claim == nil {
		t.Fatalf("spec.claim 未被写入")
	}
	if got.Spec.Claim.RequestedBy.Tenant != tenantA {
		t.Fatalf("claim 租户 = %q，期望 %q", got.Spec.Claim.RequestedBy.Tenant, tenantA)
	}
	if got.Labels[sandboxv1alpha1.LabelClaimed] != "true" {
		t.Fatalf("claimed label 未更新为 true（gateway 的候选筛选依赖它）")
	}
	if got.Labels[sandboxv1alpha1.LabelRequestID] != "idem-1" {
		t.Fatalf("requestId label 未写入（幂等查找依赖它）")
	}

	// ---- 幂等：同一个 Idempotency-Key 必须返回同一个沙箱 ----
	rec = do(t, h, http.MethodPost, "/v1/sandboxes", tokenA, "idem-1",
		`{"pool":"fc-small","ttlSeconds":600}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("幂等重试状态码 = %d，期望 201，body=%s", rec.Code, rec.Body.String())
	}
	var again SandboxResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &again); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if again.ID != created.ID {
		t.Fatalf("幂等重试拿到不同的沙箱：%q vs %q —— 一次重试会白吃掉一个库存",
			again.ID, created.ID)
	}

	// ---- 查询 ----
	rec = do(t, h, http.MethodGet, "/v1/sandboxes/"+created.ID, tokenA, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("查询状态码 = %d，body=%s", rec.Code, rec.Body.String())
	}

	// ---- 跨租户不可见：必须返回 404 而不是 403 ----
	// 403 会确认"这个 ID 存在，只是不归你"，那等于提供了一个枚举他人沙箱 ID 的接口。
	rec = do(t, h, http.MethodGet, "/v1/sandboxes/"+created.ID, tokenB, "", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("跨租户查询状态码 = %d，期望 404（403 会泄露对象存在性）", rec.Code)
	}

	// ---- 续租：控制器尚未创建 Lease，必须返回 409 让业务稍后重试 ----
	// 返回 404/500 会让业务以为沙箱已失效并去重建，而它其实完全正常。
	rec = do(t, h, http.MethodPost, "/v1/sandboxes/"+created.ID+":renew", tokenA, "", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("Lease 尚未就绪时续租状态码 = %d，期望 409，body=%s", rec.Code, rec.Body.String())
	}

	// 补上控制器本该创建的 Lease 后，续租必须成功。
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      created.ID,
			Namespace: sandboxv1alpha1.NamespacePool,
			Labels: map[string]string{
				sandboxv1alpha1.LabelTenant:  tenantA,
				sandboxv1alpha1.LabelSandbox: created.ID,
			},
		},
		Spec: coordinationv1.LeaseSpec{RenewTime: &metav1.MicroTime{Time: time.Now().Add(-time.Minute)}},
	}
	if err := c.Create(ctx, lease); err != nil {
		t.Fatalf("创建 Lease 失败: %v", err)
	}
	rec = do(t, h, http.MethodPost, "/v1/sandboxes/"+created.ID+":renew", tokenA, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("续租状态码 = %d，期望 200，body=%s", rec.Code, rec.Body.String())
	}
	if err := c.Get(ctx, types.NamespacedName{
		Namespace: sandboxv1alpha1.NamespacePool, Name: created.ID}, lease); err != nil {
		t.Fatalf("重读 Lease 失败: %v", err)
	}
	if lease.Spec.RenewTime.Time.Before(time.Now().Add(-10 * time.Second)) {
		t.Fatalf("Lease.renewTime 未被刷新: %v", lease.Spec.RenewTime)
	}

	// 另一个租户不能续租别人的心跳 —— 否则它能把别人的沙箱"续死"。
	rec = do(t, h, http.MethodPost, "/v1/sandboxes/"+created.ID+":renew", tokenB, "", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("跨租户续租状态码 = %d，期望 404", rec.Code)
	}

	// ---- 释放：清空 spec.claim，而不是删除对象 ----
	rec = do(t, h, http.MethodPost, "/v1/sandboxes/"+created.ID+":release", tokenA, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("释放状态码 = %d，body=%s", rec.Code, rec.Body.String())
	}
	if err := c.Get(ctx, types.NamespacedName{
		Namespace: sandboxv1alpha1.NamespacePool, Name: created.ID}, &got); err != nil {
		t.Fatalf("重读沙箱失败: %v", err)
	}
	if got.Spec.Claim != nil {
		t.Fatalf("释放后 spec.claim 应被清空")
	}
	// 释放是"我不要了"，不是"请你立刻销毁"：必须由控制器决定何时真正回收
	// （它还要处理 Finalizer 链、状态落盘、指标上报）。
	if !got.DeletionTimestamp.IsZero() {
		t.Fatalf("释放不应直接删除对象：那会绕过整套清理流程")
	}

	// 释放后再次释放必须幂等成功：业务重试释放是常态（没收到第一次响应），
	// 报错会制造出无意义的告警。
	rec = do(t, h, http.MethodPost, "/v1/sandboxes/"+created.ID+":release", tokenA, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("重复释放状态码 = %d，期望 200（幂等），body=%s", rec.Code, rec.Body.String())
	}
}

// TestCreate_PoolExhaustedAndColdPath 守"池空"与"冷路径"的分界。
func TestCreate_PoolExhaustedAndColdPath(t *testing.T) {
	srv, c := newTestServer(t)
	h := srv.Handler()
	ctx := context.Background()

	// 池里没有任何库存。
	rec := do(t, h, http.MethodPost, "/v1/sandboxes", tokenA, "idem-x",
		`{"pool":"fc-small","allowColdPath":false}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("池空且禁用冷路径时状态码 = %d，期望 503，body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatalf("池空必须带 Retry-After：业务据此退避或降级")
	}

	// 允许冷路径时应当创建一个**直接归属**业务的新沙箱。
	rec = do(t, h, http.MethodPost, "/v1/sandboxes", tokenA, "idem-cold",
		`{"pool":"fc-small","ttlSeconds":600,"allowColdPath":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("冷路径状态码 = %d，期望 201，body=%s", rec.Code, rec.Body.String())
	}
	var cold SandboxResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &cold); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cold.Path != claim.PathCold {
		t.Fatalf("path = %q，期望 %q", cold.Path, claim.PathCold)
	}

	var got sandboxv1alpha1.AgentSandbox
	if err := c.Get(ctx, types.NamespacedName{
		Namespace: sandboxv1alpha1.NamespacePool, Name: cold.ID}, &got); err != nil {
		t.Fatalf("读取冷路径沙箱失败: %v", err)
	}
	// 冷路径必须在创建时就带上 claim 与租户标签：不存在"先库存后认领"的过程，
	// 若标签缺失，配额计数会在一个窗口内看不到这个沙箱。
	if got.Spec.Claim == nil || got.Labels[sandboxv1alpha1.LabelTenant] != tenantA {
		t.Fatalf("冷路径沙箱未在创建时就归属业务: claim=%v labels=%v",
			got.Spec.Claim, got.Labels)
	}
	if got.Labels[sandboxv1alpha1.LabelClaimed] != "true" {
		t.Fatalf("冷路径沙箱的 claimed label 应为 true")
	}
}

// TestCreate_ContendedIsNotPoolExhausted 守一条会连锁出错的分类。
//
// 竞争（409）与池空（503）都表示"现在拿不到"，但正确反应完全相反：
// 前者应立即重试（库存可能还在），后者应退避或降级。
// 混为一谈会让业务在池里还有库存时去走冷路径 —— 白花一次冷启动，
// 并让命中率指标偏低，进而误导池水位调参去解决一个不存在的问题。
func TestCreate_ContendedIsNotPoolExhausted(t *testing.T) {
	srv, c := newTestServer(t)
	h := srv.Handler()

	// 两个库存，但把尝试预算压到 1：第一个候选被占用后预算即耗尽。
	stock(t, c, "warm-a", "")
	stock(t, c, "warm-b", "")

	// 先把两个都占掉一半：让 warm-a 变成"已认领但不是本租户"的状态，
	// 于是候选筛选（LabelClaimed=false）会排除它，模拟"缓存里看起来还有、
	// 实际已被拿走"的场景。
	rec := do(t, h, http.MethodPost, "/v1/sandboxes", tokenA, "idem-a",
		`{"pool":"fc-small","allowColdPath":false}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("第一次申请 = %d，body=%s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, http.MethodPost, "/v1/sandboxes", tokenB, "idem-b",
		`{"pool":"fc-small","allowColdPath":false}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("第二次申请 = %d，body=%s", rec.Code, rec.Body.String())
	}

	// 现在池确实空了 —— 这才是 503 的正确场合。
	rec = do(t, h, http.MethodPost, "/v1/sandboxes", tokenA, "idem-c",
		`{"pool":"fc-small","allowColdPath":false}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("两个库存都被占后 = %d，期望 503，body=%s", rec.Code, rec.Body.String())
	}
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if body.Error.Code != CodePoolExhausted {
		t.Fatalf("错误码 = %q，期望 %q（竞争与池空必须可区分）",
			body.Error.Code, CodePoolExhausted)
	}
}

// TestRouter_AuthAndAdminGates 守鉴权与越权防护。
func TestRouter_AuthAndAdminGates(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()

	// 未提供令牌 → 401（而不是 403）：换一个令牌就行，
	// 而 403 是"你是谁我清楚了但你不能做"，换令牌没用。
	rec := do(t, h, http.MethodPost, "/v1/sandboxes", "", "", `{"pool":"fc-small"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌时 = %d，期望 401", rec.Code)
	}

	// 池水位是管理员接口。租户能排空整个池是一个能造成全平台故障的越权。
	rec = do(t, h, http.MethodGet, "/v1/pools", tokenA, "", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("普通租户访问池水位 = %d，期望 403", rec.Code)
	}
	rec = do(t, h, http.MethodPost, "/v1/pools/fc-small:drain", tokenA, "", `{"reason":"test"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("普通租户排空池 = %d，期望 403", rec.Code)
	}

	// 未知路径 → 404。
	rec = do(t, h, http.MethodGet, "/v1/nope", tokenA, "", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知路径 = %d，期望 404", rec.Code)
	}

	// 方法不匹配 → 422（请求语法对、语义不允许）。
	rec = do(t, h, http.MethodGet, "/v1/sandboxes", tokenA, "", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("方法不匹配 = %d，期望 422", rec.Code)
	}

	// 探针与指标不需要鉴权：它们不暴露租户数据，而要求鉴权会让
	// kubelet 探针与 Prometheus 抓取都要配凭据，那些凭据最终会被写进各种配置。
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		rec = do(t, h, http.MethodGet, path, "", "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d，期望 200（不应要求鉴权）", path, rec.Code)
		}
	}
}

// TestDrainPool_Idempotent 守排空的幂等与去向。
func TestDrainPool_Idempotent(t *testing.T) {
	srv, c := newTestServer(t)
	h := srv.Handler()
	ctx := context.Background()

	// 用一个管理员身份的服务器实例。
	toks, err := NewStaticTenantTokens(map[string]string{
		devToken: "ops",
		tokenB:   tenantB,
	})
	if err != nil {
		t.Fatalf("构造令牌表失败: %v", err)
	}
	srv.Auth = &HeaderAuthenticator{Source: toks}
	h = srv.Handler()

	rec := do(t, h, http.MethodPost, "/v1/pools/"+poolA+":drain", devToken, "", `{"reason":"kata-upgrade"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("排空状态码 = %d，body=%s", rec.Code, rec.Body.String())
	}

	var pool sandboxv1alpha1.SandboxPool
	if err := c.Get(ctx, types.NamespacedName{Name: poolA}, &pool); err != nil {
		t.Fatalf("读取池失败: %v", err)
	}
	// 写 spec.drain 而不是注解：控制器已经在处理这个字段，
	// 再用一个注解表达同一件事会产生"两个真相"。
	if !pool.Spec.Drain.Enabled || pool.Spec.Drain.Reason != "kata-upgrade" {
		t.Fatalf("排空未写入 spec.drain: %+v", pool.Spec.Drain)
	}

	// 重复排空必须幂等。
	rec = do(t, h, http.MethodPost, "/v1/pools/"+poolA+":drain", devToken, "", `{"reason":"again"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("重复排空 = %d，期望 200", rec.Code)
	}

	// 排空后的池不接受新申请：否则一边回收一边新建，水位永远降不下去。
	rec = do(t, h, http.MethodPost, "/v1/sandboxes", devToken, "idem-drain",
		`{"pool":"fc-small","allowColdPath":true}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("排空中的池创建沙箱 = %d，期望 503", rec.Code)
	}
}

// TestHibernation_RequestsAreMutuallyExclusive 守人工休眠/唤醒的标记互斥。
//
// 两个标记同时存在时控制器以唤醒为准，但留着互相矛盾的标记会让
// status 与注解读起来互相打架 —— 排障时第一件事就是困惑"到底哪个算数"。
func TestHibernation_RequestsAreMutuallyExclusive(t *testing.T) {
	srv, c := newTestServer(t)
	h := srv.Handler()
	ctx := context.Background()

	stock(t, c, "warm-hib", tenantA)
	rec := do(t, h, http.MethodPost, "/v1/sandboxes", tokenA, "idem-hib",
		`{"pool":"fc-small","allowColdPath":false}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("申请 = %d，body=%s", rec.Code, rec.Body.String())
	}
	var created SandboxResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	rec = do(t, h, http.MethodPost, "/v1/sandboxes/"+created.ID+":hibernate", tokenA, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("休眠 = %d，body=%s", rec.Code, rec.Body.String())
	}
	var got sandboxv1alpha1.AgentSandbox
	if err := c.Get(ctx, types.NamespacedName{
		Namespace: sandboxv1alpha1.NamespacePool, Name: created.ID}, &got); err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got.Annotations[sandboxv1alpha1.AnnoHibernateRequested] != "true" {
		t.Fatalf("休眠标记未写入: %v", got.Annotations)
	}

	rec = do(t, h, http.MethodPost, "/v1/sandboxes/"+created.ID+":wake", tokenA, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("唤醒 = %d，body=%s", rec.Code, rec.Body.String())
	}
	if err := c.Get(ctx, types.NamespacedName{
		Namespace: sandboxv1alpha1.NamespacePool, Name: created.ID}, &got); err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got.Annotations[sandboxv1alpha1.AnnoWakeRequested] != "true" {
		t.Fatalf("唤醒标记未写入: %v", got.Annotations)
	}
	if _, ok := got.Annotations[sandboxv1alpha1.AnnoHibernateRequested]; ok {
		t.Fatalf("唤醒后必须清掉休眠标记，否则两个矛盾的标记会让人无从判断")
	}
}
