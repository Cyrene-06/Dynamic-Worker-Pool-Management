package gateway

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Metrics 是接入层的最小指标集。
//
// # 为什么手写而不是引入 prometheus/client_golang
//
// 需要暴露的只有"按结果分类的请求计数"与"按路径的延迟"，规模不到十条时序。
// 引入客户端库会带来一个问题：它鼓励"顺手多加几个指标"，而每加一个
// label 都会乘上租户数 —— 指标基数失控是比"少一个指标"严重得多的问题
// （docs/08 §2.1 专门写了这一条）。
//
// 明确的代价：这里没有直方图，只有简易的分位近似（见 latencySnapshot）。
// 若将来确实需要精确分位，应当换成客户端库并**同时**定义好 label 白名单，
// 而不是在这里逐步补功能。
type Metrics struct {
	mu sync.Mutex
	// results 按"结果标签"计数（created / failed / rate_limited / ...）。
	results map[string]int64
	// byRoute 按"路径 + 状态码"计数。
	byRoute map[string]int64
	// latencies 保存每个路径最近若干个耗时样本（毫秒），用于估算 P95。
	latencies map[string][]float64
	// genericLats 是所有请求的耗时样本，用于整体 P95。
	genericLats []float64

	maxSamples int
}

// NewMetrics 构造指标集。
func NewMetrics() *Metrics {
	return &Metrics{
		results:    map[string]int64{},
		byRoute:    map[string]int64{},
		latencies:  map[string][]float64{},
		maxSamples: 512,
	}
}

// Observe 记录一次业务结果。
func (m *Metrics) Observe(result string, status int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.results[result]++
	if status > 0 {
		m.byRoute[fmt.Sprintf("%d", status)]++
	}
}

// ObserveRequest 记录一次 HTTP 请求。
func (m *Metrics) ObserveRequest(path string, status int, d time.Duration) {
	if m == nil {
		return
	}
	// 路径做了**归一化**再作为 label：直接用原始路径会让每个沙箱 ID
	// 变成一个独立的时序，几千个沙箱就是几千个时序 —— 这正是
	// 基数爆炸最典型的形态，而且在开发环境完全看不出来。
	route := normalizePath(path)
	ms := float64(d.Milliseconds())

	m.mu.Lock()
	defer m.mu.Unlock()
	m.byRoute[fmt.Sprintf("%s|%d", route, status)]++
	m.latencies[route] = appendSample(m.latencies[route], ms, m.maxSamples)
	m.genericLats = appendSample(m.genericLats, ms, m.maxSamples)
}

// appendSample 追加样本并保持长度上限。
//
// 用"超限就保留后半段"的滑动窗口而不是全量保留：全量保留意味着
// 一个跑得足够久的进程会把内存吃光，而指标恰恰是那种"一直跑"的组件。
func appendSample(s []float64, v float64, max int) []float64 {
	s = append(s, v)
	if len(s) > max {
		s = s[len(s)-max:]
	}
	return s
}

// normalizePath 把路径中的 ID 替换为占位符。
func normalizePath(path string) string {
	path = strings.TrimSuffix(path, "/")
	switch {
	case path == apiPrefix, path == "/v1/pools", path == "/healthz", path == "/readyz", path == "/metrics":
		return path
	case strings.HasPrefix(path, apiPrefix+"/"):
		rest := strings.TrimPrefix(path, apiPrefix+"/")
		_, action := splitAction(rest)
		if action == "" {
			return apiPrefix + "/{id}"
		}
		return apiPrefix + "/{id}:" + action
	case strings.HasPrefix(path, "/v1/pools/"):
		rest := strings.TrimPrefix(path, "/v1/pools/")
		_, action := splitAction(rest)
		if action == "" {
			return "/v1/pools/{name}"
		}
		return "/v1/pools/{name}:" + action
	default:
		return "other"
	}
}

// ServeHTTP 以 Prometheus 文本格式输出指标。
//
// 格式是手写的，因此这里只输出了最简单的 counter 与 gauge 形态。
// 用 Prometheus 文本格式而不是自创 JSON：抓取端（Prometheus、
// VictoriaMetrics、OTel collector）全都原生支持它，自创格式会让
// 接入观测栈变成一次额外的适配工作。
func (m *Metrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	m.mu.Lock()
	results := make(map[string]int64, len(m.results))
	for k, v := range m.results {
		results[k] = v
	}
	routes := make(map[string]int64, len(m.byRoute))
	for k, v := range m.byRoute {
		routes[k] = v
	}
	lat := make(map[string][]float64, len(m.latencies))
	for k, v := range m.latencies {
		lat[k] = append([]float64(nil), v...)
	}
	all := append([]float64(nil), m.genericLats...)
	m.mu.Unlock()

	var b strings.Builder
	b.WriteString("# HELP sandbox_gateway_requests_total 按结果分类的请求数\n")
	b.WriteString("# TYPE sandbox_gateway_requests_total counter\n")
	for _, k := range sortedKeys(results) {
		fmt.Fprintf(&b, "sandbox_gateway_requests_total{result=%q} %d\n", k, results[k])
	}

	b.WriteString("# HELP sandbox_gateway_http_responses_total 按路由与状态码分类的响应数\n")
	b.WriteString("# TYPE sandbox_gateway_http_responses_total counter\n")
	for _, k := range sortedKeys(routes) {
		route, status, ok := strings.Cut(k, "|")
		if !ok {
			fmt.Fprintf(&b, "sandbox_gateway_http_responses_total{route=%q} %d\n", k, routes[k])
			continue
		}
		fmt.Fprintf(&b, "sandbox_gateway_http_responses_total{route=%q,status=%q} %d\n",
			route, status, routes[k])
	}

	b.WriteString("# HELP sandbox_gateway_request_duration_ms 请求耗时的滑动窗口分位估算\n")
	b.WriteString("# TYPE sandbox_gateway_request_duration_ms gauge\n")
	fmt.Fprintf(&b, "sandbox_gateway_request_duration_ms{route=\"_all\",quantile=\"0.95\"} %g\n",
		quantile(all, 0.95))
	for _, k := range sortedKeysFloat(lat) {
		fmt.Fprintf(&b, "sandbox_gateway_request_duration_ms{route=%q,quantile=\"0.95\"} %g\n",
			k, quantile(lat[k], 0.95))
	}

	_, _ = w.Write([]byte(b.String()))
}

// quantile 返回样本的近似分位（已排序后线性插值）。
//
// 它是**近似**的：样本来自固定长度的滑动窗口，且不含时间衰减。
// 这对"发现异常"够用（P95 从 300ms 涨到 2s 一定能看出来），
// 对"精确度量"不够用。这个边界必须写明，否则会有人拿它当 SLO 依据。
func quantile(samples []float64, q float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	s := append([]float64(nil), samples...)
	sort.Float64s(s)
	if q <= 0 {
		return s[0]
	}
	if q >= 1 {
		return s[len(s)-1]
	}
	pos := q * float64(len(s)-1)
	lo := int(pos)
	hi := lo + 1
	if hi >= len(s) {
		return s[lo]
	}
	frac := pos - float64(lo)
	return s[lo]*(1-frac) + s[hi]*frac
}

func sortedKeys(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeysFloat(m map[string][]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
