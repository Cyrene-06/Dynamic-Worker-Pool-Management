package gateway

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
)

// devToken 满足最小长度要求（32 字符）。
const devToken = "0123456789abcdef0123456789abcdef"

func mustTokens(t *testing.T) *StaticTenantTokens {
	t.Helper()
	toks, err := NewStaticTenantTokens(map[string]string{
		devToken:                           "t-1:user:alice",
		"fedcba9876543210fedcba9876543210": "t-2",
	})
	if err != nil {
		t.Fatalf("构造令牌表失败: %v", err)
	}
	return toks
}

// TestNewStaticTenantTokens_RejectsWeakConfig 守"弱令牌必须拒绝启动"。
//
// 短令牌可被暴力枚举，而它保护的是一整个租户的沙箱（可能含数据）。
// 这里选择拒绝启动而不是打印警告：警告会被忽略，而拒绝不会。
func TestNewStaticTenantTokens_RejectsWeakConfig(t *testing.T) {
	if _, err := NewStaticTenantTokens(map[string]string{"short": "t-1"}); err == nil {
		t.Fatalf("过短的令牌必须被拒绝")
	}
	if _, err := NewStaticTenantTokens(nil); err == nil {
		t.Fatalf("空令牌表必须被拒绝：没有鉴权的接入层不应启动")
	}
	// 租户为空同样必须拒绝 —— 空租户会让所有归属判断失效。
	if _, err := NewStaticTenantTokens(map[string]string{devToken: ""}); err == nil {
		t.Fatalf("租户为空的条目必须被拒绝")
	}
}

func TestStaticTenantTokens_Lookup(t *testing.T) {
	toks := mustTokens(t)

	p, ok := toks.Lookup(devToken)
	if !ok {
		t.Fatalf("有效令牌应命中")
	}
	if p.Tenant != "t-1" || p.Subject != "user:alice" {
		t.Fatalf("身份解析错误: %+v", p)
	}

	if _, ok := toks.Lookup("f" + strings.Repeat("0", 31)); ok {
		t.Fatalf("未知令牌不应命中")
	}
	if _, ok := toks.Lookup(""); ok {
		t.Fatalf("空令牌不应命中")
	}
}

func TestHeaderAuthenticator(t *testing.T) {
	a := &HeaderAuthenticator{Source: mustTokens(t)}

	cases := []struct {
		name   string
		header string
		wantOK bool
	}{
		{"裸令牌", devToken, true},
		{"Bearer 前缀", "Bearer " + devToken, true},
		{"多余空白", "  Bearer   " + devToken + "  ", true},
		{"缺失", "", false},
		{"错误令牌", "not-the-token-but-long-enough-xxxx", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1/pools", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			_, err := a.Authenticate(headerView{req: r})
			if tc.wantOK && err != nil {
				t.Fatalf("应鉴权成功，实际错误: %v", err)
			}
			if !tc.wantOK && err == nil {
				t.Fatalf("应鉴权失败")
			}
		})
	}
}

// TestAccessToken_IssueVerifyAndExpiry 守接入令牌的基本契约。
func TestAccessToken_IssueVerifyAndExpiry(t *testing.T) {
	iss, err := NewAccessTokenIssuer(devToken, time.Minute)
	if err != nil {
		t.Fatalf("构造签发器失败: %v", err)
	}
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	iss.Now = func() time.Time { return now }

	token, exp, err := iss.Issue("t-1", "sbx-abc")
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if !exp.Equal(now.Add(time.Minute)) {
		t.Fatalf("过期时间 = %v，期望 %v", exp, now.Add(time.Minute))
	}

	tenant, sandboxID, err := iss.Verify(token)
	if err != nil {
		t.Fatalf("验签失败: %v", err)
	}
	if tenant != "t-1" || sandboxID != "sbx-abc" {
		t.Fatalf("载荷解析错误: tenant=%q sandboxID=%q", tenant, sandboxID)
	}

	// 过期必须被拒。边界时刻判为过期（更保守的那一侧）。
	iss.Now = func() time.Time { return now.Add(time.Minute) }
	if _, _, err := iss.Verify(token); err == nil {
		t.Fatalf("过期令牌必须被拒绝")
	}
	iss.Now = func() time.Time { return now }

	// 篡改载荷必须被拒：把租户改成别人。
	if enc, sig, ok := strings.Cut(token, "."); ok {
		tampered := base64.RawURLEncoding.EncodeToString([]byte("t-9|sbx-abc|9999999999")) + "." + sig
		if _, _, err := iss.Verify(tampered); err == nil {
			t.Fatalf("篡改后的令牌必须被拒绝")
		}
		// 篡改签名同样必须被拒。
		bogus := base64.RawURLEncoding.EncodeToString([]byte("not-a-real-signature"))
		if _, _, err := iss.Verify(enc + "." + bogus); err == nil {
			t.Fatalf("伪造签名的令牌必须被拒绝")
		}
	} else {
		t.Fatalf("令牌格式应为 payload.signature")
	}

	// 换一个密钥后旧令牌必须失效。
	other, err := NewAccessTokenIssuer("another-secret-key-0123456789abcd", time.Minute)
	if err != nil {
		t.Fatalf("构造第二个签发器失败: %v", err)
	}
	other.Now = iss.Now
	if _, _, err := other.Verify(token); err == nil {
		t.Fatalf("换密钥后旧令牌必须失效")
	}
}

// TestNewAccessTokenIssuer_RejectsWeakSecret 守"不能自动生成随机密钥"。
//
// 随机密钥在进程重启后就变了，而已签发的令牌还没过期 ——
// 表现是"服务重启后所有在跑的沙箱连接突然全部鉴权失败"，
// 且现象无法从任何日志里定位到原因。
func TestNewAccessTokenIssuer_RejectsWeakSecret(t *testing.T) {
	if _, err := NewAccessTokenIssuer("too-short", time.Minute); err == nil {
		t.Fatalf("过短的签名密钥必须被拒绝")
	}
}

// TestRateLimiter_BurstThenRefill 守令牌桶。
func TestRateLimiter_BurstThenRefill(t *testing.T) {
	l := NewRateLimiter()
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	l.Now = func() time.Time { return now }

	q := TenantQuota{MaxCreatePerSecond: 10, Burst: 30}

	// 突发额度必须一次性可用：首次调用就被限流会给接入方一个极糟的首印象，
	// 而且它反映的并不是真实负载。
	for i := 0; i < 30; i++ {
		if ok, _ := l.Allow("t-1", q); !ok {
			t.Fatalf("第 %d 次突发额度内请求被限流", i+1)
		}
	}
	ok, wait := l.Allow("t-1", q)
	if ok {
		t.Fatalf("超出突发额度后应被限流")
	}
	if wait <= 0 {
		t.Fatalf("被限流时必须给出等待时间，否则业务只能盲目重试")
	}

	// 过一段时间后补充令牌。
	now = now.Add(time.Second)
	ok, _ = l.Allow("t-1", q)
	if !ok {
		t.Fatalf("1 秒后应补充到 10 个令牌（速率 10/秒），实际仍被限流")
	}

	// 租户之间互不影响：一个租户刷爆额度不应影响另一个。
	if ok, _ := l.Allow("t-2", q); !ok {
		t.Fatalf("另一个租户不应受影响")
	}
}

// TestRateLimiter_BucketCountBounded 守内存上限。
//
// 拒绝服务类问题的唯一可靠解法是有界：bucket 数必须有上限，
// 否则租户数量增长（或被伪造的头制造出大量 key）会撑爆内存。
func TestRateLimiter_BucketCountBounded(t *testing.T) {
	l := NewRateLimiter()
	l.maxBuckets = 4
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	l.Now = func() time.Time { return now }
	q := TenantQuota{MaxCreatePerSecond: 1, Burst: 1}

	for i := 0; i < 100; i++ {
		l.Allow(string(rune('a'+i%26))+"-tenant", q)
	}
	l.mu.Lock()
	n := len(l.buckets)
	l.mu.Unlock()
	if n > 4 {
		t.Fatalf("bucket 数 = %d，超过上限 4", n)
	}
}

func TestQuotaTable_DefaultsAndFloor(t *testing.T) {
	// 突发量小于速率时，桶会在一瞬间被耗尽，于是"10/秒"实际表现为
	// "每 3 秒 10 个"的锯齿。这里把突发量抬到不低于速率。
	q := TenantQuota{MaxCreatePerSecond: 10, Burst: 2}.withDefaults()
	if q.Burst < q.MaxCreatePerSecond {
		t.Fatalf("burst = %v 低于速率 %v，会产生锯齿行为", q.Burst, q.MaxCreatePerSecond)
	}

	table := DefaultQuotaTable()
	def, _ := table.Quota("unknown-tenant")
	if def.MaxConcurrentSandboxes <= 0 {
		t.Fatalf("默认配额必须是非零上限：不限制会让一个租户的 bug 吃光整个集群")
	}
	if def.MaxCreatePerSecond <= 0 || def.Burst <= 0 {
		t.Fatalf("默认限流参数未补齐: %+v", def)
	}
}

func TestLoadQuotaFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "quota.txt")
	content := strings.Join([]string{
		"# 租户 并发上限 速率 突发",
		"t-1 100 5 15",
		"t-2 50",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写配额文件失败: %v", err)
	}

	table, err := LoadQuotaFile(path)
	if err != nil {
		t.Fatalf("加载配额文件失败: %v", err)
	}
	q1, _ := table.Quota("t-1")
	if q1.MaxConcurrentSandboxes != 100 || q1.MaxCreatePerSecond != 5 || q1.Burst != 15 {
		t.Fatalf("t-1 配额错误: %+v", q1)
	}
	// 未给出速率的行应补齐默认值，而不是拿 0 当速率
	// （0 速率会让这个租户完全无法创建）。
	q2, _ := table.Quota("t-2")
	if q2.MaxConcurrentSandboxes != 50 {
		t.Fatalf("t-2 并发上限 = %d，期望 50", q2.MaxConcurrentSandboxes)
	}
	if q2.MaxCreatePerSecond <= 0 {
		t.Fatalf("t-2 速率未补齐为默认值: %+v", q2)
	}

	// 解析失败必须报错而不是跳过该行：被静默跳过的配额会让那个租户
	// 得到默认的、通常更宽松的限额 —— 与"已经为它配了严格限额"的意图正好相反。
	bad := filepath.Join(dir, "bad.txt")
	if err := os.WriteFile(bad, []byte("t-3 notanumber\n"), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	if _, err := LoadQuotaFile(bad); err == nil {
		t.Fatalf("非法配额必须报错")
	}
}

// TestStaticAdminSet_DeniesByDefault 守"默认拒绝"。
//
// 管理员接口默认放行是这类接口最常见的翻车方式（部署时忘了配，接口就裸奔了）。
func TestStaticAdminSet_DeniesByDefault(t *testing.T) {
	empty := StaticAdminSet(nil)
	if empty(Principal{Tenant: "t-1"}) {
		t.Fatalf("未配置管理员时任何人都必须被拒绝")
	}
	if empty(Principal{Tenant: "admin"}) {
		t.Fatalf("租户名恰好叫 admin 不应因此获得权限")
	}

	set := StaticAdminSet([]string{" ops ", "", "t-9"})
	if !set(Principal{Tenant: "ops"}) || !set(Principal{Tenant: "t-9"}) {
		t.Fatalf("配置内的租户应被允许（且空白应被裁剪）")
	}
	if set(Principal{Tenant: "t-1"}) || set(Principal{}) {
		t.Fatalf("配置外的租户与空身份必须被拒绝")
	}
}

// TestWriteError_StatusCodeAndRetryAfter 守对外错误语义。
//
// 业务需要的不是一个"失败了"的信号，而是**该做什么**。
// 这里逐条固定每个场景的状态码与 Retry-After，因为它们是契约。
func TestWriteError_StatusCodeAndRetryAfter(t *testing.T) {
	cases := []struct {
		name       string
		err        *APIError
		wantStatus int
		wantCode   string
		wantRetry  bool
	}{
		{"未鉴权", errUnauthorized("x"), http.StatusUnauthorized, CodeUnauthorized, false},
		{"无权限", errForbidden("x"), http.StatusForbidden, CodeForbidden, false},
		{"规格非法", errInvalidSpec("x"), http.StatusUnprocessableEntity, CodeInvalidSpec, false},
		{"不存在", errNotFound("x"), http.StatusNotFound, CodeNotFound, false},
		{"已回收", errGone("IdleTimeout"), http.StatusGone, CodeGone, false},
		{"竞争", errContended(time.Second), http.StatusConflict, CodeContended, true},
		{"配额", errQuotaExceeded("x", time.Second), http.StatusTooManyRequests, CodeQuotaExceeded, true},
		{"限流", errRateLimited("x", time.Second), http.StatusTooManyRequests, CodeRateLimited, true},
		{"池空", errPoolExhausted(time.Second), http.StatusServiceUnavailable, CodePoolExhausted, true},
		{"容量不足", errInsufficientCapacity("x"), http.StatusInsufficientStorage, CodeInsufficientCap, false},
		{"内部错误", errInternal("x"), http.StatusInternalServerError, CodeInternal, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeError(rec, "req-1", tc.err)

			if rec.Code != tc.wantStatus {
				t.Fatalf("状态码 = %d，期望 %d", rec.Code, tc.wantStatus)
			}
			var body errorBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("响应体不是合法 JSON: %v", err)
			}
			if body.Error.Code != tc.wantCode {
				t.Fatalf("错误码 = %q，期望 %q", body.Error.Code, tc.wantCode)
			}
			if body.RequestID != "req-1" {
				t.Fatalf("requestId 未回显: %q", body.RequestID)
			}

			ra := rec.Header().Get("Retry-After")
			if tc.wantRetry {
				if ra == "" {
					t.Fatalf("必须带 Retry-After：否则业务只能盲目重试")
				}
				// 向上取整：报小了会让客户端在服务端还没准备好时又打回来。
				if n, err := json.Number(ra).Int64(); err != nil || n < 1 {
					t.Fatalf("Retry-After = %q，应为不小于 1 的秒数", ra)
				}
			} else if ra != "" {
				t.Fatalf("不该带 Retry-After（会让业务去做无意义的重试）: %q", ra)
			}
		})
	}
}

// TestErrGone_CarriesRecycleReason 守 410 的可用性。
//
// 业务需要区分"这个 ID 从来不存在"（可能自己拼错了）与"它存在过但已被回收"
// （应当走重建并恢复状态的流程）。后者的正确反应是恢复，
// 前者的正确反应是不要重试。
func TestErrGone_CarriesRecycleReason(t *testing.T) {
	reason := string(sandboxv1alpha1.RecycleIdleTimeout)
	rec := httptest.NewRecorder()
	writeError(rec, "", errGone(reason))

	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if body.Error.Details["recycleReason"] != reason {
		t.Fatalf("410 必须携带 recycleReason，实际: %+v", body.Error.Details)
	}
}

// TestValidateIdempotencyKey 守"显式拒绝而不是静默截断"。
//
// 静默截断会让两个不同的键变成同一个（重试吃掉第二个沙箱），
// 静默忽略则会让"我提供了幂等键"变成一句谎话。
func TestValidateIdempotencyKey(t *testing.T) {
	cases := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{"空（表示不做幂等）", "", false},
		{"合法", "abc-123_XYZ.9", false},
		{"63 字符边界", strings.Repeat("a", 63), false},
		{"超长", strings.Repeat("a", 64), true},
		{"含斜杠", "abc/def", true},
		{"含空格", "abc def", true},
		{"含中文", "中文键", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateIdempotencyKey(tc.key)
			if tc.wantErr && err == nil {
				t.Fatalf("应被拒绝")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("不应被拒绝: %v", err)
			}
		})
	}
}

// TestNormalizePath_NoCardinalityExplosion 守指标基数。
//
// 直接用原始路径会让每个沙箱 ID 变成一个独立时序，几千个沙箱就是几千个时序 ——
// 这正是基数爆炸最典型的形态，而且在开发环境完全看不出来。
func TestNormalizePath_NoCardinalityExplosion(t *testing.T) {
	cases := map[string]string{
		"/v1/sandboxes":               "/v1/sandboxes",
		"/v1/sandboxes/sbx-abc":       "/v1/sandboxes/{id}",
		"/v1/sandboxes/sbx-abc:renew": "/v1/sandboxes/{id}:renew",
		"/v1/sandboxes/sbx-xyz:renew": "/v1/sandboxes/{id}:renew",
		"/v1/pools":                   "/v1/pools",
		"/v1/pools/fc-small:drain":    "/v1/pools/{name}:drain",
		"/completely/unknown":         "other",
	}
	for in, want := range cases {
		if got := normalizePath(in); got != want {
			t.Fatalf("normalizePath(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestSplitAction 守"在最后一个冒号处拆分"。
func TestSplitAction(t *testing.T) {
	cases := []struct{ in, id, action string }{
		{"sbx-abc", "sbx-abc", ""},
		{"sbx-abc:renew", "sbx-abc", "renew"},
		{"a:b:release", "a:b", "release"},
		{":renew", "", "renew"},
	}
	for _, tc := range cases {
		id, action := splitAction(tc.in)
		if id != tc.id || action != tc.action {
			t.Fatalf("splitAction(%q) = (%q,%q)，期望 (%q,%q)",
				tc.in, id, action, tc.id, tc.action)
		}
	}
}

// TestDecodeBody_RejectsUnknownFields 守"拼错的字段名必须报错"。
//
// 未知字段被静默忽略时，业务会以为自己设置了 TTL —— 直到沙箱按默认值
// 提前消失，那时才发现字段名拼错了。早报错胜过晚困惑。
func TestDecodeBody_RejectsUnknownFields(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/sandboxes",
		strings.NewReader(`{"pool":"fc-small","ttlsecond":600}`))
	var req CreateRequest
	if aerr := decodeBody(r, &req); aerr == nil {
		t.Fatalf("拼错的字段名必须报错，而不是被静默忽略")
	}
}
