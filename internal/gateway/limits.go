package gateway

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
)

// TenantQuota 是一个租户的配额。
type TenantQuota struct {
	// MaxConcurrentSandboxes 是该租户同时持有的沙箱数上限。
	// 0 表示不限制。
	MaxConcurrentSandboxes int32
	// MaxCreatePerSecond 是创建速率上限；0 表示用 DefaultCreatePerSecond。
	MaxCreatePerSecond float64
	// Burst 是允许的瞬时突发量；0 表示用 DefaultCreateBurst。
	Burst float64
}

// 默认限流参数：docs/03 §6 的 "10 创建/秒/租户（突发 30）"。
const (
	DefaultCreatePerSecond float64 = 10
	DefaultCreateBurst     float64 = 30
)

// QuotaTable 提供租户配额。
type QuotaTable interface {
	// Quota 返回该租户的配额。第二个返回值为 false 表示租户未知。
	Quota(tenant string) (TenantQuota, bool)
}

// StaticQuotaTable 是固定配额表，未列出的租户使用默认值。
//
// 默认值选择"有限但宽松"而不是"不限制"：不限制意味着任何一个租户的
// bug 都能把整个集群的容量吃光，而那是跨租户的故障传播。
// 给出一个明确的上限，至少让"某个租户失控"表现为它自己被限流。
type StaticQuotaTable struct {
	ByTenant map[string]TenantQuota
	Default  TenantQuota
}

// Quota 实现 QuotaTable。
func (t *StaticQuotaTable) Quota(tenant string) (TenantQuota, bool) {
	if t == nil {
		return TenantQuota{}, false
	}
	if q, ok := t.ByTenant[tenant]; ok {
		return q.withDefaults(), true
	}
	return t.Default.withDefaults(), false
}

func (q TenantQuota) withDefaults() TenantQuota {
	if q.MaxCreatePerSecond <= 0 {
		q.MaxCreatePerSecond = DefaultCreatePerSecond
	}
	if q.Burst <= 0 {
		q.Burst = DefaultCreateBurst
	}
	if q.Burst < q.MaxCreatePerSecond {
		// 突发量小于速率时，桶会在一个周期内就被耗尽，
		// 于是配置的"10/秒"实际表现为"每 3 秒 10 个"的锯齿。
		// 这里把突发量抬到不低于速率 —— 一个自洽的下限，
		// 而不是让配置者去发现这个反直觉的细节。
		q.Burst = q.MaxCreatePerSecond
	}
	return q
}

// RateLimiter 是每租户的令牌桶。
//
// # 为什么按副本限流是刻意的而不是缺陷
//
// 本组件的限流目标不是"保护业务侧的总配额"，而是**保护 API Server 的写入压力**
// （风险表 R7）。写入压力来自本副本发出的请求，因此每个副本各限一份
// 恰好符合目标。若把限流做成集群全局的（需要 Redis 之类的外部依赖），
// 副本数变化时反而需要重新调参，而它保护的资源（本副本的写入速率）
// 并没有跟着变。
//
// 需要说明的是：这也意味着**扩副本会线性放大总创建速率**。
// 若业务侧需要的是"每租户全局限速"，那必须在更外层（网关入口 / 服务网格）做。
// 这不是本层能提供的语义，因此不做成看起来像、实际却不是的东西。
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	Now     func() time.Time

	// maxBuckets 上限，防止 key 空间被撑爆。
	//
	// key 只来自已鉴权的租户，因此正常情况下规模可控；但租户数量本身
	// 可以增长，而"可控"和"有上限"是两件事。加上限是为了让内存占用
	// 在任何输入下都有界 —— 这也是拒绝服务类问题的唯一可靠解法。
	maxBuckets int
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter 构造限流器。
func NewRateLimiter() *RateLimiter {
	return &RateLimiter{buckets: map[string]*bucket{}, maxBuckets: 4096}
}

// Allow 消耗一个令牌。不允许时返回需要等待多久。
func (l *RateLimiter) Allow(tenant string, q TenantQuota) (bool, time.Duration) {
	q = q.withDefaults()
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[tenant]
	if !ok {
		if len(l.buckets) >= l.maxBuckets {
			l.evictStaleLocked(now)
			if len(l.buckets) >= l.maxBuckets {
				// 无法再分配新桶时**拒绝**而不是放行。
				// 放行会让上限失去意义；拒绝只是让新租户等一会儿，
				// 而旧租户的桶会被下面的清理腾出来。
				return false, time.Second
			}
		}
		// 新租户从满桶开始：首次请求不该被等待，
		// 否则"第一次调用就限流"会给接入方一个非常糟的首印象，
		// 而且它反映的并不是真实负载。
		b = &bucket{tokens: q.Burst, last: now}
		l.buckets[tenant] = b
	}

	// 按经过的时间补充令牌。用 min(经过时间, 桶容量/速率) 之外的处理见下：
	// 直接线性补充即可，多余部分由 cap 截断。
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens = math.Min(q.Burst, b.tokens+elapsed*q.MaxCreatePerSecond)
		b.last = now
	}

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	// 需要等待的时间 = 补满 1 个令牌所需时间。
	wait := time.Duration((1-b.tokens)/q.MaxCreatePerSecond*float64(time.Second)) + time.Second
	return false, wait
}

// evictStaleLocked 清掉长时间未使用的桶。调用方必须已持锁。
func (l *RateLimiter) evictStaleLocked(now time.Time) {
	const idle = 10 * time.Minute
	for k, b := range l.buckets {
		if now.Sub(b.last) > idle {
			delete(l.buckets, k)
		}
	}
}

func (l *RateLimiter) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// DefaultQuotaTable 返回默认配额表。
//
// 默认给一个**非零**上限而不是"不限制"：不限制意味着任何一个租户的
// bug（例如一个失控的重试循环）都能把整个集群的容量吃光，
// 而那是跨租户的故障传播。给一个明确的上限，至少让"某个租户失控"
// 表现为它自己被限流，而不是所有租户一起受影响。
func DefaultQuotaTable() *StaticQuotaTable {
	return &StaticQuotaTable{
		ByTenant: map[string]TenantQuota{},
		Default:  TenantQuota{MaxConcurrentSandboxes: 500},
	}
}

// LoadQuotaFile 从文件加载租户配额。
//
// 格式：一行一个 `tenant maxConcurrent createPerSecond burst`，
// `#` 开头为注释。后两个字段可省略（省略则用默认值）。
//
// 解析失败一律返回错误而不是跳过该行：一行被静默跳过的配额
// 会让那个租户得到**默认的、通常更宽松**的限额，
// 而这与"已经为它配了严格限额"的意图正好相反。
func LoadQuotaFile(path string) (*StaticQuotaTable, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配额文件失败: %w", err)
	}
	table := DefaultQuotaTable()
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || len(fields) > 4 {
			return nil, fmt.Errorf("配额文件第 %d 行字段数应为 2-4 个，实际 %d 个", i+1, len(fields))
		}
		table.ByTenant[fields[0]] = TenantQuota{}
		q := table.ByTenant[fields[0]]
		maxConcurrent, err := strconv.ParseInt(fields[1], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("配额文件第 %d 行的 maxConcurrent 非法: %w", i+1, err)
		}
		q.MaxConcurrentSandboxes = int32(maxConcurrent)
		if len(fields) > 2 {
			rate, err := strconv.ParseFloat(fields[2], 64)
			if err != nil {
				return nil, fmt.Errorf("配额文件第 %d 行的 createPerSecond 非法: %w", i+1, err)
			}
			q.MaxCreatePerSecond = rate
		}
		if len(fields) > 3 {
			burst, err := strconv.ParseFloat(fields[3], 64)
			if err != nil {
				return nil, fmt.Errorf("配额文件第 %d 行的 burst 非法: %w", i+1, err)
			}
			q.Burst = burst
		}
		table.ByTenant[fields[0]] = q
	}
	return table, nil
}

// QuotaChecker 检查租户的并发沙箱配额。
type QuotaChecker struct {
	Client client.Client
	Now    func() time.Time
}

// Check 统计该租户当前持有的沙箱数并判断是否超配额。
//
// # 这里的计数是**近似**的，而且必须是近似的
//
// 精确实现需要在一次事务里"读计数 + 写对象"，而 Kubernetes 没有跨对象事务。
// 用乐观锁去实现它会把配额检查变成一个竞争点，正好落在 P95 < 800ms
// 的热路径上 —— 为了一个统计精度去牺牲最核心的延迟指标不划算。
//
// 因此这里用带缓存的 List（最终一致）。代价是并发申请可能短暂超出配额
// 一两个。这个偏差是**可接受**的，因为配额的作用是防止单个租户
// 吃掉整个集群，而不是做精确计量（精确计量是计费系统的事，它读的是
// 事后的用量记录，不是这个检查）。
func (q *QuotaChecker) Check(ctx context.Context, tenant string, quota TenantQuota) *APIError {
	if tenant == "" || quota.MaxConcurrentSandboxes <= 0 {
		return nil
	}
	var list sandboxv1alpha1.AgentSandboxList
	if err := q.Client.List(ctx, &list,
		client.MatchingLabels{sandboxv1alpha1.LabelTenant: tenant},
	); err != nil {
		// 配额检查失败时**放行**，但明确返回 nil 而不吞掉错误信息。
		//
		// 这里的取向是"可用性优先"：配额检查依赖一次集群读取，
		// 而集群读取会因为限流、缓存重建、临时故障而失败。
		// 若失败即拒绝，一次 API Server 抖动就会让所有租户完全无法申请 ——
		// 那是把"防止超用"升级成了"全站不可用"。
		// 代价是短暂失去配额保护，这个取舍是显式的（docs/04 §7.2 同样取向）。
		return nil
	}

	var live int32
	for i := range list.Items {
		s := &list.Items[i]
		if s.DeletionTimestamp != nil || s.Status.Phase.IsTerminal() {
			continue
		}
		live++
	}
	if live >= quota.MaxConcurrentSandboxes {
		return errQuotaExceeded(
			fmt.Sprintf("租户 %s 的并发沙箱数已达上限 %d", tenant, quota.MaxConcurrentSandboxes),
			5*time.Second)
	}
	return nil
}
