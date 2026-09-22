package claim

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
	"github.com/Cyrene-06/Dynamic-Worker-Pool-Management/test/envtest"
)

const (
	testNS   = sandboxv1alpha1.NamespacePool
	testPool = "fc-small"

	// maxContendRetries 模拟 gateway 在遇到 ErrContended 时的重试上限。
	// 有上限而不是无限重试：重试本身也在消耗同一个临界区的容量，
	// 无上限重试会把"竞争"放大成"活锁"。
	maxContendRetries = 8
)

// newStock 创建一个"池中库存"：Ready、未被认领。
//
// 注意要分两步：Create 时 status 会被忽略（CRD 有 status 子资源），
// 必须再用 Status().Update 写一次。这是 CRD 带 status 子资源后的标准姿势，
// 忘了第二步会得到一堆 phase 为空的库存，而它们的 label 看起来完全正常。
func newStock(t *testing.T, ctx context.Context, c client.Client, name, tenant string) *sandboxv1alpha1.AgentSandbox {
	t.Helper()
	sbx := &sandboxv1alpha1.AgentSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNS,
			Labels: map[string]string{
				sandboxv1alpha1.LabelPool:    testPool,
				sandboxv1alpha1.LabelClaimed: "false",
				sandboxv1alpha1.LabelRole:    sandboxv1alpha1.RoleSandbox,
			},
		},
		Spec: sandboxv1alpha1.AgentSandboxSpec{
			PoolRef:     sandboxv1alpha1.NameRef{Name: testPool},
			TemplateRef: sandboxv1alpha1.NameRef{Name: "python-3.12"},
			Tier:        sandboxv1alpha1.TierSmall,
			Isolation:   sandboxv1alpha1.IsolationSimulated,
		},
	}
	if tenant != "" {
		sbx.Labels[sandboxv1alpha1.LabelTenant] = tenant
	}
	if err := c.Create(ctx, sbx); err != nil {
		t.Fatalf("创建库存 %s 失败: %v", name, err)
	}
	sbx.Status.Phase = sandboxv1alpha1.PhaseReady
	if err := c.Status().Update(ctx, sbx); err != nil {
		t.Fatalf("写库存 %s 的 status 失败: %v", name, err)
	}
	return sbx
}

func listSandboxes(t *testing.T, ctx context.Context, c client.Client) []sandboxv1alpha1.AgentSandbox {
	t.Helper()
	var list sandboxv1alpha1.AgentSandboxList
	if err := c.List(ctx, &list, client.InNamespace(testNS)); err != nil {
		t.Fatalf("列出沙箱失败: %v", err)
	}
	return list.Items
}

// ---------------------------------------------------------------------------
// 核心用例：并发认领绝不重复
// ---------------------------------------------------------------------------

// TestClaim_ConcurrentClaimsNeverDoubleClaim 是本包存在的全部理由。
//
// 池化唯一的强一致临界区就在这里：并发申请抢同一批库存。
// 一旦有两个申请拿到同一个沙箱，那就是跨租户数据泄漏级别的故障。
//
// 这个测试**必须**跑在真实 API Server 上：乐观并发（resourceVersion 冲突）
// 是 API Server 的行为，fake client 没有这个概念 —— 用 fake client 测这个
// 等于没测，而且会给出虚假的安全感，比不测更糟。
func TestClaim_ConcurrentClaimsNeverDoubleClaim(t *testing.T) {
	env := envtest.Start(t)
	ctx := context.Background()

	const (
		stock    = 20
		claimers = 60
	)

	for i := 0; i < stock; i++ {
		newStock(t, ctx, env.Client, fmt.Sprintf("sbx-%03d", i), "")
	}

	cl := &Claimer{Client: env.Client, Namespace: testNS}

	var (
		mu        sync.Mutex
		succeeded []*Result
		exhausted int
		contended int
		conflicts int
		otherErrs []error
	)

	// 用 barrier 让所有 goroutine 尽量同时发起，把竞争窗口放到最大。
	// 若不加 barrier，goroutine 会被调度器串行化，竞争几乎不发生，
	// 测试会"通过"但什么都没验证到。
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < claimers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start

			req := Request{
				Pool:      testPool,
				Tenant:    "t-1",
				RequestID: fmt.Sprintf("req-%03d", i),
				SessionID: fmt.Sprintf("sess-%03d", i),
			}

			// 模拟 gateway 的真实行为：遇到 ErrContended 就退避重试（不要走冷路径）。
			// 不重试的话，这个测试只能断言"没有重复认领"，
			// 无法断言"库存最终都被认领完"—— 而后者才是库存真正被用起来的关键。
			var (
				res      *Result
				err      error
				contends int
			)
			for attempt := 0; attempt < maxContendRetries; attempt++ {
				res, err = cl.Claim(ctx, req)
				if !errors.Is(err, ErrContended) {
					break
				}
				contends++
			}

			mu.Lock()
			defer mu.Unlock()
			if res != nil {
				conflicts += res.Conflicts
			}
			contended += contends
			switch {
			case err == nil:
				succeeded = append(succeeded, res)
			case errors.Is(err, ErrPoolExhausted):
				exhausted++
			default:
				otherErrs = append(otherErrs, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if len(otherErrs) > 0 {
		t.Fatalf("出现非预期错误（池空与竞争都应该是可预期的）: %v", otherErrs)
	}

	// ---- 断言 1：成功数恰好等于库存数 ----
	//
	// "恰好"两个方向都要守：多于库存说明有沙箱被重复认领，
	// 少于库存说明有库存在高并发下被白白浪费 —— 那是可用性问题，
	// 因为业务侧收到了失败，而资源明明还在。
	if len(succeeded) != stock {
		t.Fatalf("成功认领 %d 个，期望恰好 %d 个（多于此说明重复认领，少于此说明有库存被漏掉）",
			len(succeeded), stock)
	}
	if exhausted != claimers-stock {
		t.Errorf("池空次数 %d，期望 %d", exhausted, claimers-stock)
	}

	// ---- 断言 2：没有两个成功结果指向同一个沙箱 ----
	seen := make(map[string]string, stock) // 沙箱名 -> requestID
	for _, r := range succeeded {
		if r.Sandbox == nil || r.Sandbox.Spec.Claim == nil {
			t.Fatal("成功结果必须带已写入 claim 的沙箱")
		}
		name := r.Sandbox.Name
		rid := r.Sandbox.Spec.Claim.RequestedBy.RequestID
		if prev, dup := seen[name]; dup {
			t.Fatalf("沙箱 %s 被重复认领：requestID %s 与 %s 都认为自己拿到了它", name, prev, rid)
		}
		seen[name] = rid
	}

	// ---- 断言 3：从 API Server 侧复核，而不是只信内存里的结果 ----
	// 内存里的成功结果可能因为缓存/时序而与实际落库状态不一致。
	items := listSandboxes(t, ctx, env.Client)
	claimed := 0
	byRequestID := make(map[string]string)
	for i := range items {
		s := &items[i]
		if s.Spec.Claim == nil {
			continue
		}
		claimed++
		rid := s.Spec.Claim.RequestedBy.RequestID
		if prev, dup := byRequestID[rid]; dup {
			t.Fatalf("同一个 requestID %s 出现在两个沙箱上：%s 与 %s", rid, prev, s.Name)
		}
		byRequestID[rid] = s.Name

		// label 与 spec 必须一致：认领是一次 patch 同时写两者的，
		// 若出现不一致说明有人只写了其中一部分。
		if got := s.Labels[sandboxv1alpha1.LabelClaimed]; got != "true" {
			t.Errorf("沙箱 %s 已写 claim 但 claimed label = %q，应为 \"true\"", s.Name, got)
		}
		if got := s.Labels[sandboxv1alpha1.LabelTenant]; got != "t-1" {
			t.Errorf("沙箱 %s 的 tenant label = %q，应为 t-1", s.Name, got)
		}
	}
	if claimed != stock {
		t.Fatalf("API Server 侧已认领数 %d，期望 %d", claimed, stock)
	}

	// 冲突次数不是硬断言（取决于调度），但记录下来用于判断测试是否真的
	// 制造出了竞争 —— 如果始终为 0，说明这个测试其实没有验证到并发。
	t.Logf("库存=%d 申请=%d 冲突次数=%d 竞争重试次数=%d"+
		"（冲突为 0 意味着没制造出竞争，需检查 barrier 是否有效）",
		stock, claimers, conflicts, contended)
}

// TestClaim_IdempotentByRequestID 验证客户端重试不会白吃库存。
//
// 客户端超时重试是常态而不是例外。没有幂等，一次重试就会吃掉第二个库存，
// 并且业务侧会拿到两个不同沙箱（第一个被泄漏，直到 TTL 兜底才回收）。
func TestClaim_IdempotentByRequestID(t *testing.T) {
	env := envtest.Start(t)
	ctx := context.Background()

	newStock(t, ctx, env.Client, "sbx-a", "")
	newStock(t, ctx, env.Client, "sbx-b", "")

	cl := &Claimer{Client: env.Client, Namespace: testNS}
	req := Request{Pool: testPool, Tenant: "t-1", RequestID: "req-idem", SessionID: "s-1"}

	first, err := cl.Claim(ctx, req)
	if err != nil {
		t.Fatalf("首次认领失败: %v", err)
	}
	if first.Idempotent {
		t.Error("首次认领不应标记为 Idempotent")
	}

	second, err := cl.Claim(ctx, req)
	if err != nil {
		t.Fatalf("重试认领失败: %v", err)
	}
	if !second.Idempotent {
		t.Error("同一 RequestID 的第二次认领必须走幂等路径")
	}
	if second.Sandbox.Name != first.Sandbox.Name {
		t.Fatalf("幂等路径必须返回同一个沙箱：%s vs %s", second.Sandbox.Name, first.Sandbox.Name)
	}

	// 关键：只应该消耗一个库存。
	claimed := 0
	for _, s := range listSandboxes(t, ctx, env.Client) {
		if s.Spec.Claim != nil {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("重试后已认领数 %d，期望 1（重试不应吃掉第二个库存）", claimed)
	}
}

func TestClaim_PoolExhausted(t *testing.T) {
	env := envtest.Start(t)
	ctx := context.Background()

	cl := &Claimer{Client: env.Client, Namespace: testNS}
	res, err := cl.Claim(ctx, Request{Pool: testPool, Tenant: "t-1", RequestID: "req-1"})

	if !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("空池必须返回 ErrPoolExhausted（它是期望内结果，gateway 据此返回 503 而不是 500），实际: %v", err)
	}
	// res 即使出错也应可用，让调用方能读取竞争强度。
	if res == nil {
		t.Fatal("即使池空也应返回非 nil 的 Result，否则调用方拿不到冲突计数")
	}
	if res.Sandbox != nil {
		t.Fatal("池空时不应返回沙箱")
	}
}

// TestClaim_SkipsNotReadyAndDeleting 验证候选过滤。
//
// 这两类对象在 label 上看起来都是"未被认领的库存"，
// 只有看 status.phase 与 deletionTimestamp 才能识别。
func TestClaim_SkipsNotReadyAndDeleting(t *testing.T) {
	env := envtest.Start(t)
	ctx := context.Background()

	// 1) Pending（还没就绪）：认领它等于让业务拿到一个连不上的沙箱。
	pending := newStock(t, ctx, env.Client, "sbx-pending", "")
	pending.Status.Phase = sandboxv1alpha1.PhasePending
	if err := env.Client.Status().Update(ctx, pending); err != nil {
		t.Fatalf("改 status 失败: %v", err)
	}

	// 2) Ready 但正在删除：加 finalizer 再删，才能让对象停在被删除状态。
	dying := newStock(t, ctx, env.Client, "sbx-dying", "")
	dying.Finalizers = []string{"test.hold"}
	if err := env.Client.Update(ctx, dying); err != nil {
		t.Fatalf("加 finalizer 失败: %v", err)
	}
	if err := env.Client.Delete(ctx, dying); err != nil {
		t.Fatalf("删除失败: %v", err)
	}

	// 3) 正常可用。
	newStock(t, ctx, env.Client, "sbx-good", "")

	cl := &Claimer{Client: env.Client, Namespace: testNS}
	res, err := cl.Claim(ctx, Request{Pool: testPool, Tenant: "t-1", RequestID: "req-x"})
	if err != nil {
		t.Fatalf("认领失败: %v", err)
	}
	if res.Sandbox == nil || res.Sandbox.Name != "sbx-good" {
		t.Fatalf("应认领到 sbx-good，实际 %+v", res.Sandbox)
	}
}

// TestClaim_PrefersSameTenantStock 固定"同租户优先"的语义。
//
// 注意：这条路径要等 M2 启用同租户原地复用（并落实 resetHook 强制清理）后
// 才会在生产里真正出现。现在保留它是因为排序逻辑已经在代码里，
// 把预期行为用测试钉住，比留一段"没人知道该怎么表现"的代码要好。
func TestClaim_PrefersSameTenantStock(t *testing.T) {
	env := envtest.Start(t)
	ctx := context.Background()

	// 先创建 3 个"无租户"的干净库存（创建时间更早）。
	for i := 0; i < 3; i++ {
		newStock(t, ctx, env.Client, fmt.Sprintf("sbx-fresh-%d", i), "")
	}
	// 再创建一个归属 t-1 的（创建时间更晚）。
	newStock(t, ctx, env.Client, "sbx-reuse-t1", "t-1")

	cl := &Claimer{Client: env.Client, Namespace: testNS}
	res, err := cl.Claim(ctx, Request{Pool: testPool, Tenant: "t-1", RequestID: "req-aff"})
	if err != nil {
		t.Fatalf("认领失败: %v", err)
	}
	if res.Sandbox.Name != "sbx-reuse-t1" {
		t.Fatalf("同租户库存应优先（可省一次冷启动），实际拿到 %s", res.Sandbox.Name)
	}
}

// ---------------------------------------------------------------------------
// 纯逻辑用例（不需要集群）
// ---------------------------------------------------------------------------

func TestRequest_Validate(t *testing.T) {
	cases := []struct {
		name      string
		req       Request
		wantInMsg string
	}{
		{
			name:      "缺 Pool",
			req:       Request{Tenant: "t-1"},
			wantInMsg: "Pool",
		},
		{
			name:      "缺 Tenant",
			req:       Request{Pool: "p"},
			wantInMsg: "Tenant",
		},
		{
			// RequestID 会被写进 label，超长会让 Set 失败。
			// 显式拒绝而不是静默截断：截断会让幂等查找失效，
			// 表现为"重试时又吃掉了第二个库存"。
			name:      "RequestID 超长",
			req:       Request{Pool: "p", Tenant: "t", RequestID: strings.Repeat("x", 64)},
			wantInMsg: "label",
		},
		{
			name:      "RequestID 含非法字符",
			req:       Request{Pool: "p", Tenant: "t", RequestID: "req/with/slash"},
			wantInMsg: "label",
		},
		{
			name: "合法请求",
			req:  Request{Pool: "p", Tenant: "t", RequestID: "req-abc.123_x"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.validate()
			if tc.wantInMsg == "" {
				if err != nil {
					t.Fatalf("期望通过校验，实际: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("期望校验失败，实际通过")
			}
			if !strings.Contains(err.Error(), tc.wantInMsg) {
				t.Fatalf("错误信息 %q 未包含 %q", err.Error(), tc.wantInMsg)
			}
		})
	}
}

func TestClaimable(t *testing.T) {
	ready := func() *sandboxv1alpha1.AgentSandbox {
		return &sandboxv1alpha1.AgentSandbox{Status: sandboxv1alpha1.AgentSandboxStatus{
			Phase: sandboxv1alpha1.PhaseReady,
		}}
	}

	cases := []struct {
		name string
		mut  func(*sandboxv1alpha1.AgentSandbox)
		want bool
	}{
		{"Ready 且未认领", func(*sandboxv1alpha1.AgentSandbox) {}, true},
		{"nil", nil, false},
		{"已被认领", func(s *sandboxv1alpha1.AgentSandbox) {
			s.Spec.Claim = &sandboxv1alpha1.ClaimSpec{}
		}, false},
		{"正在删除", func(s *sandboxv1alpha1.AgentSandbox) {
			now := metav1.Now()
			s.DeletionTimestamp = &now
		}, false},
		{"尚未就绪", func(s *sandboxv1alpha1.AgentSandbox) {
			s.Status.Phase = sandboxv1alpha1.PhaseProvisioning
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.mut == nil {
				if claimable(nil) {
					t.Fatal("nil 必须不可认领")
				}
				return
			}
			s := ready()
			tc.mut(s)
			if got := claimable(s); got != tc.want {
				t.Fatalf("claimable = %v, 期望 %v", got, tc.want)
			}
		})
	}
}
