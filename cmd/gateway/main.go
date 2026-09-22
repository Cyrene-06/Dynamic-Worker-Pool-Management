// Command gateway 是业务接入层（docs/04 §8）。
//
// 它只负责**装配**：把鉴权、配额、限流、Store 与 HTTP 服务接起来。
// 所有业务判断都在 internal/gateway 里，因此可以在不启动进程的情况下
// 用 httptest 完整测试错误语义 —— 而错误语义恰恰是接入层最需要守住的东西。
package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
	"github.com/Cyrene-06/Dynamic-Worker-Pool-Management/internal/claim"
	"github.com/Cyrene-06/Dynamic-Worker-Pool-Management/internal/controller"
	"github.com/Cyrene-06/Dynamic-Worker-Pool-Management/internal/gateway"
)

func main() {
	var (
		bindAddress   string
		probeAddress  string
		tokensFile    string
		issuerSecret  string
		quotaFile     string
		adminTenants  string
		namespace     string
		sandboxPort   int
		protocol      string
		createTimeout time.Duration
	)
	flag.StringVar(&bindAddress, "bind-address", ":8090", "业务 API 监听地址")
	flag.StringVar(&probeAddress, "health-probe-bind-address", ":8091", "健康检查监听地址")
	flag.StringVar(&tokensFile, "tenant-tokens-file", "",
		"租户令牌表文件（格式：一行一个 `token=tenant[:subject]`）。**必填**")
	flag.StringVar(&issuerSecret, "token-issuer-secret", "",
		"接入令牌的签名密钥（最少 32 字符）。**必填**，也可由环境变量 "+
			"SANDBOX_TOKEN_ISSUER_SECRET 提供——推荐后者，因为命令行参数会出现在 "+
			"ps 输出与 kubectl describe 里，而那些输出经常被贴进工单与聊天记录")
	flag.StringVar(&quotaFile, "quota-file", "",
		"租户配额文件（格式：一行一个 `tenant maxConcurrent createPerSecond burst`）。留空则全部用默认值")
	flag.StringVar(&adminTenants, "admin-tenants", "",
		"拥有管理员权限的租户，逗号分隔。留空则管理员接口一律拒绝")
	flag.StringVar(&namespace, "sandbox-namespace", sandboxv1alpha1.NamespacePool,
		"沙箱对象所在命名空间")
	flag.IntVar(&sandboxPort, "sandbox-port", 7788, "返回给业务的沙箱数据面端口")
	flag.StringVar(&protocol, "sandbox-protocol", "ws", "返回给业务的数据面协议标识")
	flag.DurationVar(&createTimeout, "create-timeout", 5*time.Second,
		"单次申请的整体超时。必须有：集群写入会因 API Server 限流而长时间挂起")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	logger := ctrl.Log.WithName("setup")
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// 鉴权与签名密钥没有"默认放行"的降级路径。
	//
	// 刻意不提供 --insecure-no-auth：那种开关几乎必然会出现在生产配置里，
	// 而它打开的是一整个租户边界的缺口。本地开发用一份示例令牌文件即可
	// （见 config/gateway/tenant-tokens.example）。
	//
	// 密钥允许从环境变量取：命令行参数会出现在 ps 输出与 kubectl describe 里，
	// 而那些输出经常被贴进工单、CI 日志与聊天记录。
	if issuerSecret == "" {
		issuerSecret = os.Getenv("SANDBOX_TOKEN_ISSUER_SECRET")
	}
	if tokensFile == "" || issuerSecret == "" {
		logger.Error(nil, "必须提供 --tenant-tokens-file 与 --token-issuer-secret"+
			"（或环境变量 SANDBOX_TOKEN_ISSUER_SECRET）")
		os.Exit(1)
	}

	tokens, err := gateway.LoadTenantTokensFile(tokensFile)
	if err != nil {
		logger.Error(err, "加载租户令牌表失败")
		os.Exit(1)
	}
	issuer, err := gateway.NewAccessTokenIssuer(issuerSecret, 10*time.Minute)
	if err != nil {
		logger.Error(err, "构造接入令牌签发器失败")
		os.Exit(1)
	}

	quotas := gateway.DefaultQuotaTable()
	if quotaFile != "" {
		loaded, err := gateway.LoadQuotaFile(quotaFile)
		if err != nil {
			logger.Error(err, "加载配额文件失败")
			os.Exit(1)
		}
		quotas = loaded
	}

	admin := gateway.StaticAdminSet(strings.Split(adminTenants, ","))

	// ---- scheme & manager ----
	//
	// 用 manager 而不是裸的 client.New：我们需要的是**带缓存的读**。
	// 认领时会 List 候选库存，而那是热路径上唯一的集群读；
	// 走 API Server 的话，200 申请/秒就意味着 200 次 LIST/秒，
	// 这个压力足以让 API Server 成为瓶颈（风险表 R7）。
	//
	// 写仍然走直连（delegating client 的默认行为），因此 CAS 的
	// 幂等性与一致性不受缓存影响。
	appScheme := runtime.NewScheme()
	utilruntime.Must(scheme.AddToScheme(appScheme))
	utilruntime.Must(sandboxv1alpha1.AddToScheme(appScheme))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: appScheme,
		// 接入层不暴露 controller-runtime 自己的指标端点（用 0 表示关闭），
		// 它有自己的 /metrics（见 internal/gateway/metrics.go）。
		// 两个端点都叫 /metrics 而内容不同，会让抓取配置变成一个陷阱。
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: probeAddress,
		// 接入层是**无状态**的，每个副本都能独立服务任何请求，
		// 因此不需要选主 —— 选主反而会让副本数失去意义。
		LeaderElection: false,
	})
	if err != nil {
		logger.Error(err, "创建 manager 失败")
		os.Exit(1)
	}

	store := gateway.NewStore(mgr.GetClient(), issuer)
	store.Namespace = namespace
	store.SandboxPort = sandboxPort
	store.Protocol = protocol
	store.Claimer = &claim.Claimer{Client: mgr.GetClient(), Namespace: namespace}
	// 心跳间隔从控制器侧的常量注入，而不是在 gateway 里再定义一份：
	// 那是同一个契约的两端，分两处定义必然会在某次改动后不一致，
	// 而症状是"客户端按 60s 续租、服务端 30s 就判离线"这种极难定位的误回收。
	store.HeartbeatIntervalSeconds = controller.HeartbeatLeaseDurationSeconds

	srv := gateway.NewServer(store)
	srv.Auth = &gateway.HeaderAuthenticator{Source: tokens}
	srv.Quotas = quotas
	srv.IsAdmin = admin
	srv.QuotaChecker = &gateway.QuotaChecker{Client: mgr.GetClient()}
	srv.CreateTimeout = createTimeout

	httpSrv := &http.Server{
		Addr:              bindAddress,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		// 有意的短超时：接入层是无状态的，慢请求在这里排队只会
		// 把过载放大成雪崩。让它快速失败，由业务按 Retry-After 重试。
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	if err := mgr.Add(&serverRunnable{srv: httpSrv, logger: logger.WithName("http")}); err != nil {
		logger.Error(err, "注册 HTTP 服务失败")
		os.Exit(1)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		logger.Error(err, "注册 healthz 失败")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		logger.Error(err, "注册 readyz 失败")
		os.Exit(1)
	}

	logger.Info("启动接入层",
		"bind", bindAddress,
		"namespace", namespace,
		"renewIntervalSeconds", store.HeartbeatIntervalSeconds)

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		logger.Error(err, "manager 退出")
		os.Exit(1)
	}
}

// serverRunnable 把 HTTP 服务接入 manager 的生命周期。
//
// 这样做而不是自己 go func() + 自己处理信号：manager 已经
// 提供了"先停收流量、再取消 context、最后等 goroutine 退出"这套顺序。
// 自己写一遍的结果通常是少了最后一步，于是进程在请求处理到一半时退出，
// 业务侧看到一批被截断的响应 —— 而这类错误只在滚动更新时出现。
type serverRunnable struct {
	srv    *http.Server
	logger interface {
		Info(msg string, keysAndValues ...any)
		Error(err error, msg string, keysAndValues ...any)
	}
}

// Start 实现 manager.Runnable。
func (r *serverRunnable) Start(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		r.logger.Info("HTTP 服务已监听", "addr", r.srv.Addr)
		if err := r.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		// 用独立的 context 做优雅关闭：ctx 已经取消，拿它去做关闭
		// 会让 Shutdown 立刻失败，于是所有在途请求被直接切断。
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := r.srv.Shutdown(shutdownCtx); err != nil {
			r.logger.Error(err, "HTTP 优雅关闭失败")
		}
		return nil
	}
}

// NeedLeaderElection 明确声明不需要选主。
//
// 不声明的话会沿用 manager 的默认值 —— 而默认值是由构建时的选项决定的，
// 一旦有人给 manager 打开选主，接入层就会变成"只有一个副本在服务"，
// 而这件事在监控上几乎看不出来（副本数正常、错误率正常，只是吞吐不涨）。
func (r *serverRunnable) NeedLeaderElection() bool { return false }
