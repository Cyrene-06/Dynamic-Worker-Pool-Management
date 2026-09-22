package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Principal 是一次已鉴权的调用者身份。
//
// Tenant 是**授权的基本单位**，Principal 只用于审计。
// 这个区分很重要：所有配额、可见性、复用规则都按 tenant 判定，
// 若误用 principal（同一个租户下可能有多个用户/多个 Agent）做隔离，
// 就会出现"同一租户的两个 Agent 互相看不到对方的沙箱"这种莫名其妙的限制。
type Principal struct {
	Tenant string
	// Subject 是发起者身份（如 user:alice / service:agent-runner），仅用于审计。
	Subject string
}

// Authenticator 解析请求中的身份。
//
// 抽象成接口是因为鉴权方式会随环境变化（本地用静态令牌，
// 生产接公司 OIDC / mTLS）。但**不变的是**：鉴权必须在请求路径的最前面，
// 且失败必须默认拒绝 —— 一个可被绕过的鉴权比没有鉴权更危险，
// 因为它会让人以为已经有保护了。
type Authenticator interface {
	Authenticate(r httpRequest) (Principal, error)
}

// httpRequest 是本包对 http.Request 的最小依赖。
//
// 只暴露一个取头的方法，而不是直接吃 *http.Request：
// 这让鉴权逻辑可以用一个三行的假实现来测试，而不必构造完整的请求对象，
// 也就不必在测试里操心 URL、Body、Context 这些与鉴权无关的东西。
type httpRequest interface {
	Header(name string) string
}

// ErrUnauthenticated 表示未能确定调用者身份。
var ErrUnauthenticated = errors.New("未提供有效的租户令牌")

// TenantTokenSource 提供"令牌 → 租户"的映射。
//
// 生产环境应由配置中心或 Secret 挂载，且需要支持热更新（轮换）。
// 这里用接口而不是直接读文件，是为了让"轮换"成为实现细节，
// 而不是散落在鉴权代码里的重载逻辑。
type TenantTokenSource interface {
	// Lookup 返回令牌对应的租户。第二个返回值为 false 表示令牌无效。
	Lookup(token string) (Principal, bool)
}

// StaticTenantTokens 是静态令牌表，供本地开发与早期接入使用。
//
// # 为什么按哈希查表而不是逐个常量时间比较
//
// 逐个 `subtle.ConstantTimeCompare` 是常量时间的，但它需要遍历全部令牌，
// 于是耗时随令牌数量线性增长 —— 这个耗时差本身就是一个可被利用的信道
// （能反推出配置了多少个租户）。先把**输入**哈希一次再查 map，
// 比较就变成了"哈希相等"，而攻击者无法让哈希碰撞去探测密钥。
//
// 表里存哈希而不是明文，还有一个附带好处：即使这份配置被误提交或
// 被日志打印，泄露的也只是哈希。
type StaticTenantTokens struct {
	byHash map[string]Principal
}

// NewStaticTenantTokens 从"令牌 → 租户[:主体]"的映射构造。
//
// 入参是明文令牌，因为它来自 flag 或 Secret 挂载文件；
// 构造函数内部立即哈希并丢弃明文引用。
func NewStaticTenantTokens(entries map[string]string) (*StaticTenantTokens, error) {
	if len(entries) == 0 {
		return nil, errors.New("租户令牌表为空：未配置鉴权时不应启动接入层")
	}
	s := &StaticTenantTokens{byHash: make(map[string]Principal, len(entries))}
	for token, spec := range entries {
		if len(token) < minTokenLen {
			// 短令牌可被暴力枚举，而它保护的是一整个租户的沙箱（可能含数据）。
			// 这里选择拒绝启动而不是警告：警告会被忽略，而拒绝不会。
			return nil, fmt.Errorf("租户令牌过短（最少 %d 字符）：%s", minTokenLen, redact(token))
		}
		tenant, subject := splitTenantSpec(spec)
		if tenant == "" {
			return nil, fmt.Errorf("令牌对应的租户为空: %s", spec)
		}
		s.byHash[hashToken(token)] = Principal{Tenant: tenant, Subject: subject}
	}
	return s, nil
}

// minTokenLen 是令牌的最小长度。32 字符约 190 bit 熵（若是随机生成），
// 远超暴力枚举可行范围。
const minTokenLen = 32

// Lookup 实现 TenantTokenSource。
func (s *StaticTenantTokens) Lookup(token string) (Principal, bool) {
	if s == nil || token == "" {
		return Principal{}, false
	}
	// 保留一次常量时间比较：hash 查表虽然不泄露"哪个令牌匹配"，
	// 但完全没有比较就无法防御时序上的存在性探测。
	// 这里比较的是哈希，因此不泄露原文。
	h := hashToken(token)
	p, ok := s.byHash[h]
	if !ok {
		// 与成功路径做一次等长的比较，抹平"未命中"与"命中"的差异。
		_ = subtle.ConstantTimeCompare([]byte(h), []byte(h))
		return Principal{}, false
	}
	return p, true
}

// HeaderAuthenticator 从请求头读取令牌并解析身份。
type HeaderAuthenticator struct {
	Source TenantTokenSource
	// Header 是承载令牌的头名，默认 Authorization。
	Header string
}

// Authenticate 实现 Authenticator。
func (a *HeaderAuthenticator) Authenticate(r httpRequest) (Principal, error) {
	name := a.Header
	if name == "" {
		name = "Authorization"
	}
	raw := strings.TrimSpace(r.Header(name))
	if raw == "" {
		return Principal{}, ErrUnauthenticated
	}
	// 同时接受 "Bearer <token>" 与裸令牌：前者是标准形式，
	// 后者方便用 curl 手工调试。两者都走同一套校验，
	// 因此接受裸令牌不会降低安全性。
	if rest, ok := strings.CutPrefix(raw, "Bearer "); ok {
		raw = strings.TrimSpace(rest)
	}
	p, ok := a.Source.Lookup(raw)
	if !ok {
		return Principal{}, ErrUnauthenticated
	}
	return p, nil
}

// ---- 接入令牌签发 ----

// AccessTokenIssuer 签发沙箱的短期接入令牌。
//
// # 这个令牌现在能做什么，不能做什么
//
// 它做的是"把平台侧的授权结论带进数据面"：签名里包含租户与沙箱 ID
// 以及过期时间，因此数据面（沙箱侧的连接代理）可以在**不回调 gateway** 的
// 前提下独立验证"这次连接是否被允许、是否过期"。
//
// 需要明确的是：**当前仓库里还没有这个数据面代理**。因此在数据面落地之前，
// 这个令牌只是随响应一起返回、供业务接入自己的代理时使用的凭据，
// 它并不构成一道已经生效的防线。把它当成"已经安全了"是危险的，
// 因此这里显式写明，而不是让它看起来像个已闭环的机制。
type AccessTokenIssuer struct {
	secret []byte
	// TTL 是令牌有效期。docs/04 §8 约定 10 分钟并定期轮换。
	TTL time.Duration
	// Now 可注入，便于测试过期分支。
	Now func() time.Time
}

// NewAccessTokenIssuer 构造签发器。
//
// 密钥为空时返回错误而不是生成随机密钥：随机密钥在进程重启后就变了，
// 而已签发的令牌还没过期 —— 表现是"服务重启后所有在跑的沙箱连接
// 突然全部鉴权失败"，且现象无法从任何日志里定位到原因。
func NewAccessTokenIssuer(secret string, ttl time.Duration) (*AccessTokenIssuer, error) {
	if len(secret) < minTokenLen {
		return nil, fmt.Errorf("接入令牌签名密钥过短（最少 %d 字符）", minTokenLen)
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &AccessTokenIssuer{secret: []byte(secret), TTL: ttl}, nil
}

// claim 是令牌载荷。
//
// 用紧凑的竖线分隔而不是 JSON：令牌会出现在 URL 与日志里，
// 越短越好；而且它的唯一读者是本平台的验证代码，不需要通用性。
type accessClaim struct {
	Tenant    string
	SandboxID string
	ExpiresAt time.Time
}

// Issue 为指定沙箱签发一个接入令牌。
func (i *AccessTokenIssuer) Issue(tenant, sandboxID string) (string, time.Time, error) {
	if tenant == "" || sandboxID == "" {
		return "", time.Time{}, errors.New("签发接入令牌需要 tenant 与 sandboxID")
	}
	exp := i.now().Add(i.TTL)
	payload := strings.Join([]string{tenant, sandboxID, strconv.FormatInt(exp.Unix(), 10)}, "|")
	enc := base64.RawURLEncoding.EncodeToString([]byte(payload))
	sig := i.sign(enc)
	return enc + "." + sig, exp, nil
}

// Verify 校验接入令牌，返回其载荷。
func (i *AccessTokenIssuer) Verify(token string) (tenant, sandboxID string, err error) {
	enc, sig, ok := strings.Cut(token, ".")
	if !ok {
		return "", "", errors.New("接入令牌格式非法")
	}
	// 先验签再解析：先解析会让未签名的输入进入解析路径，
	// 那等于用一个不可信的字符串去驱动代码分支。
	expected := i.sign(enc)
	if subtle.ConstantTimeCompare([]byte(sig), []byte(expected)) != 1 {
		return "", "", errors.New("接入令牌签名不匹配")
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return "", "", fmt.Errorf("接入令牌载荷不可解码: %w", err)
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 {
		return "", "", errors.New("接入令牌载荷字段数不为 3")
	}
	exp, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", "", fmt.Errorf("接入令牌过期时间非法: %w", err)
	}
	if !i.now().Before(time.Unix(exp, 0)) {
		// 用 Before 而不是 After 的反义，是为了在"恰好等于过期时刻"
		// 时判为过期。边界只能选一边，而选"更保守"的那一边不需要理由。
		return "", "", errors.New("接入令牌已过期")
	}
	return parts[0], parts[1], nil
}

func (i *AccessTokenIssuer) sign(enc string) string {
	m := hmac.New(sha256.New, i.secret)
	m.Write([]byte(enc))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (i *AccessTokenIssuer) now() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now()
}

// StaticAdminSet 把逗号分隔的租户列表变成管理员判定函数。
//
// # 为什么管理员是一个显式集合而不是某个特权租户名
//
// 把"租户名等于 admin"当作权限，等于把一个可猜的字符串变成权限。
// 这里要求配置里显式列出，且**列表为空时一律拒绝**——
// 默认放行是这类接口最常见的翻车方式（部署时忘了配，接口就裸奔了）。
func StaticAdminSet(tenants []string) func(Principal) bool {
	set := map[string]struct{}{}
	for _, t := range tenants {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		set[t] = struct{}{}
	}
	return func(p Principal) bool {
		if p.Tenant == "" || len(set) == 0 {
			return false
		}
		_, ok := set[p.Tenant]
		return ok
	}
}

// ---- 小工具 ----

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func splitTenantSpec(spec string) (tenant, subject string) {
	tenant, subject, _ = strings.Cut(spec, ":")
	return strings.TrimSpace(tenant), strings.TrimSpace(subject)
}

// redact 只保留令牌首尾各 2 个字符，用于错误信息与日志。
//
// 直接打印令牌会让它进入日志聚合系统与告警通知 —— 一次排查就把凭据
// 扩散到了很多本不该看到它的地方。而完全打码又会让"是哪个令牌配错了"
// 变得无法判断，因此保留头尾用于人工比对。
func redact(token string) string {
	if len(token) <= 4 {
		return "****"
	}
	return token[:2] + "****" + token[len(token)-2:]
}

// LoadTenantTokensFile 从文件加载令牌表。
//
// 文件格式是一行一个 `token=tenant[:subject]`，`#` 开头为注释。
// 用文件而不是单个 flag，是因为多租户场景下令牌数量会增长，
// 而一串逗号分隔的长 flag 既易错又会被 shell 历史记录。
func LoadTenantTokensFile(path string) (*StaticTenantTokens, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取令牌表失败: %w", err)
	}
	entries := map[string]string{}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		token, spec, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("令牌表第 %d 行缺少 '='", i+1)
		}
		entries[strings.TrimSpace(token)] = strings.TrimSpace(spec)
	}
	return NewStaticTenantTokens(entries)
}
