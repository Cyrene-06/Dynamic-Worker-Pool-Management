// Command operator 是 Agent 沙箱控制面的入口。
//
// 它只负责**装配**：把 scheme、缓存策略、隔离抽象层与各个控制器接起来。
// 所有业务判断都在 internal/controller 与 internal/isolation 里，
// 这样控制器逻辑才能在 envtest 里被测试，而不需要真的跑起一个进程。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/go-logr/logr"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
	"github.com/Cyrene-06/Dynamic-Worker-Pool-Management/internal/controller"
	"github.com/Cyrene-06/Dynamic-Worker-Pool-Management/internal/isolation"
)

func main() {
	var (
		metricsAddr          string
		probeAddr            string
		enableLeaderElection bool
		sandboxConcurrency   int
		isolationConfigPath  string
		namespace            string
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "指标服务监听地址")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "健康检查监听地址")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"启用选主。多副本时必须开启，否则两个副本会同时做池决策并互相打架")
	flag.IntVar(&sandboxConcurrency, "sandbox-concurrency", 32,
		"SandboxController 的并发 reconcile 数。这个值可以大，因为单沙箱决策彼此独立")
	flag.StringVar(&isolationConfigPath, "isolation-config", "",
		"从本地文件加载隔离级别配置（用于本地开发；留空则从 ConfigMap 读取）")
	flag.StringVar(&namespace, "namespace", "sandbox-pool", "沙箱对象所在命名空间")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	logger := ctrl.Log.WithName("setup")
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// ---- scheme ----
	appScheme := runtime.NewScheme()
	utilruntime.Must(scheme.AddToScheme(appScheme))
	utilruntime.Must(coordinationv1.AddToScheme(appScheme))
	utilruntime.Must(nodev1.AddToScheme(appScheme))
	utilruntime.Must(sandboxv1alpha1.AddToScheme(appScheme))

	// ---- manager ----
	//
	// 缓存策略是这里唯一需要解释的地方：默认情况下 controller-runtime 会把
	// **全部** Pod 拉进 informer 缓存。在 5k 沙箱规模下，这会让控制器内存
	// 涨到几百 MB 甚至 GB 级，而控制器从不关心别人的 Pod。
	// 用 label selector 把缓存限定在自己的对象上，是成本最低的一次优化。
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 appScheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		WebhookServer:          webhook.NewServer(webhook.Options{Port: 9443}),
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "sandbox-operator.sandbox.example.com",
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Pod{}: {
					Label: labels.SelectorFromSet(labels.Set{
						sandboxv1alpha1.LabelRole: sandboxv1alpha1.RoleSandbox,
					}),
				},
				// Lease 数量与沙箱同阶，同样需要按命名空间限定，
				// 否则会把 kube-system 里所有组件的选主 Lease 都缓存下来。
				&coordinationv1.Lease{}: {
					Namespaces: map[string]cache.Config{namespace: {}},
				},
			},
		},
	})
	if err != nil {
		logger.Error(err, "创建 manager 失败")
		os.Exit(1)
	}

	// ---- 隔离抽象层 ----
	//
	// 这一层是整个"同一份 CRD 在 kind 与生产之间平移"能力的技术前提。
	// 构造时先用内置默认配置，保证 ConfigMap 缺失也能启动并给出可诊断状态
	// （而不是 CrashLoop —— 那会把一次配置遗漏升级成线上事故）。
	resolver := isolation.NewResolver(
		loadInitialIsolationConfig(isolationConfigPath, logger),
		&isolation.ClientProbe{Reader: mgr.GetClient()},
		isolation.Options{
			// 默认 FailFast：宁可拒绝新建，也不能偷偷把 VM 级隔离降成 runc。
			// 需要可用性优先的池可以按池覆盖这个策略。
			Policy:   sandboxv1alpha1.DegradationFailFast,
			CacheTTL: 5 * time.Minute,
		},
	)

	if err := mgr.Add(&isolationConfigSync{
		Client:   mgr.GetClient(),
		Resolver: resolver,
		Interval: 60 * time.Second,
		Logger:   logger.WithName("isolation-config"),
	}); err != nil {
		logger.Error(err, "注册隔离配置同步失败")
		os.Exit(1)
	}

	// ---- 控制器 ----
	if err := (&controller.SandboxReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Resolver: resolver,
		Recorder: mgr.GetEventRecorderFor("sandbox-controller"),
	}).SetupWithManager(mgr, sandboxConcurrency); err != nil {
		logger.Error(err, "注册 SandboxController 失败")
		os.Exit(1)
	}

	// ---- 健康检查 ----
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		logger.Error(err, "注册 healthz 失败")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		logger.Error(err, "注册 readyz 失败")
		os.Exit(1)
	}

	logger.Info("启动控制面",
		"namespace", namespace,
		"concurrency", sandboxConcurrency,
		"levels", resolver.Config().LevelNames())

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		logger.Error(err, "manager 退出")
		os.Exit(1)
	}
}

// loadInitialIsolationConfig 读取初始的隔离级别配置。
//
// 支持本地文件是为了让 E1 环境（kind）不需要先装 ConfigMap 就能起来，
// 降低"第一次跑起来"的摩擦 —— 这一点对新人上手影响很大。
func loadInitialIsolationConfig(path string, logger logr.Logger) *isolation.Config {
	if path == "" {
		logger.Info("未指定 --isolation-config，先使用内置默认配置，随后从 ConfigMap 同步")
		return isolation.DefaultConfig()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		logger.Info("读取隔离配置失败，回落到内置默认配置", "path", path, "err", err.Error())
		return isolation.DefaultConfig()
	}
	cfg, err := isolation.ParseConfig(data)
	if err != nil {
		// 配置写错时必须显式回落到默认值并让上层看见，而不是静默用错的配置跑。
		logger.Info("隔离配置非法，回落到内置默认配置", "path", path, "err", err.Error())
		return isolation.DefaultConfig()
	}
	return cfg
}

// isolationConfigSync 把 ConfigMap 中的隔离级别配置同步进 Resolver。
//
// 骨架实现用轮询（60s）。M1 会改成事件驱动（Watch ConfigMap）：
// 轮询的代价是"kata 装好之后最多 60s 才被识别"，事件驱动的代价是多一条 watch。
// 这个取舍在 M1 之前不影响正确性，因此先选简单的那个。
type isolationConfigSync struct {
	Client   client.Client
	Resolver *isolation.Resolver
	Interval time.Duration
	Logger   logr.Logger
	lastRV   string
}

// Start 实现 manager.Runnable。
func (s *isolationConfigSync) Start(ctx context.Context) error {
	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()

	// 先立即同步一次，避免"启动后 60s 内用的是默认配置"这个窗口。
	if err := s.sync(ctx); err != nil {
		s.Logger.Error(err, "首次同步隔离配置失败（继续使用当前配置）")
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.sync(ctx); err != nil {
				// 同步失败不能中断循环：ConfigMap 短暂不可读不应让
				// 隔离能力被意外收回（那会导致大面积拒绝新建）。
				s.Logger.Error(err, "同步隔离配置失败（继续使用当前配置）")
			}
		}
	}
}

// NeedLeaderElection 返回 false：配置同步是幂等的只读动作，
// 每个副本各自同步即可，没必要只在 leader 上跑。
func (s *isolationConfigSync) NeedLeaderElection() bool { return false }

func (s *isolationConfigSync) sync(ctx context.Context) error {
	var cm corev1.ConfigMap
	key := client.ObjectKey{
		Namespace: isolation.ConfigMapNamespace,
		Name:      isolation.ConfigMapName,
	}
	if err := s.Client.Get(ctx, key, &cm); err != nil {
		return fmt.Errorf("读取 ConfigMap %s/%s 失败: %w", key.Namespace, key.Name, err)
	}
	// 用 resourceVersion 做变更检测，避免每次解析 + 清空 Resolver 缓存。
	if cm.ResourceVersion == s.lastRV {
		return nil
	}
	raw, ok := cm.Data[isolation.ConfigMapKey]
	if !ok {
		return fmt.Errorf("ConfigMap %s 缺少键 %q", key.Name, isolation.ConfigMapKey)
	}
	cfg, err := isolation.ParseConfig([]byte(raw))
	if err != nil {
		// 新配置非法时保留旧配置：宁可继续用旧的可用配置，
		// 也不要因为一次手抖的 ConfigMap 编辑让整个控制面失去隔离能力。
		return fmt.Errorf("隔离配置非法，保留原配置: %w", err)
	}
	s.Resolver.SetConfig(cfg)
	s.lastRV = cm.ResourceVersion
	s.Logger.Info("隔离配置已更新", "levels", cfg.LevelNames(), "resourceVersion", cm.ResourceVersion)
	return nil
}
