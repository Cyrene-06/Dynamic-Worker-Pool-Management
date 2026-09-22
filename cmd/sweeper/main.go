// Command sweeper 是泄漏对账的一次性执行入口（CronJob，docs/04 §6.3）。
//
// # 它为什么是一次性进程而不是常驻服务
//
// 对账的本质是"定期独立核对"，而不是"持续持有状态"。做成 CronJob 有三个
// 实际好处：一是它天然与控制器隔离（控制器坏掉时它仍然看到真实集群）；
// 二是它不需要选主、不需要缓存，因此可以在控制器宕机时照常运行；
// 三是它的日志本身就是一轮完整报告的现场记录，不需要额外拼凑。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
	"github.com/Cyrene-06/Dynamic-Worker-Pool-Management/internal/sweeper"
)

func main() {
	var (
		dryRun          bool
		orphanGrace     time.Duration
		stuckGrace      time.Duration
		pendingGrace    time.Duration
		maxFindings     int
		kubeconfig      string
		outputPath      string
		timeoutDuration time.Duration
	)
	flag.BoolVar(&dryRun, "dry-run", false,
		"只报告不执行。**首次上线、以及任何一次修改了对账规则之后，都必须先跑这个**")
	flag.DurationVar(&orphanGrace, "orphan-grace", 5*time.Minute,
		"孤儿对象从\"首次被发现\"到可删除之间的等待时间")
	flag.DurationVar(&stuckGrace, "stuck-terminating-grace", 10*time.Minute,
		"删除流程卡死多久后强制清理")
	flag.DurationVar(&pendingGrace, "pending-grace", 30*time.Minute,
		"沙箱停留在 Pending/Provisioning 多久后判定卡死")
	flag.IntVar(&maxFindings, "max-findings", 200,
		"单轮最多处理多少条发现（爆炸半径上限）")
	flag.StringVar(&kubeconfig, "kubeconfig", "",
		"kubeconfig 路径（本地运行用；在集群内运行时留空）")
	flag.StringVar(&outputPath, "output", "",
		"把报告写到该文件（供采集端读取；留空则只打日志）")
	flag.DurationVar(&timeoutDuration, "timeout", 2*time.Minute,
		"整轮对账的超时。必须有：对账要遍历全部对象，而它绝不能比它的执行周期还久")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	logger := ctrl.Log.WithName("sweeper")
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	cfg, err := restConfig(kubeconfig)
	if err != nil {
		logger.Error(err, "加载集群配置失败")
		os.Exit(1)
	}

	appScheme := runtime.NewScheme()
	utilruntime.Must(scheme.AddToScheme(appScheme))
	utilruntime.Must(sandboxv1alpha1.AddToScheme(appScheme))

	// 对账用**非缓存**的直连 client。
	//
	// 这是刻意的：对账存在的全部意义就是发现"缓存与真实集群不一致"这类问题。
	// 若它也读缓存，就会与控制器看到同一份可能已经过期的世界，
	// 于是两者会一致地漏掉同一个泄漏 —— 一个只会在有人手工核对时
	// 才被发现的失效模式。
	c, err := client.New(cfg, client.Options{Scheme: appScheme})
	if err != nil {
		logger.Error(err, "构造 client 失败")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeoutDuration)
	defer cancel()

	s := &sweeper.Sweeper{
		Client: c,
		DryRun: dryRun,
		Options: sweeper.Options{
			OrphanGrace:           orphanGrace,
			StuckTerminatingGrace: stuckGrace,
			PendingGrace:          pendingGrace,
			MaxFindings:           maxFindings,
		},
		Reporter: &logReporter{logger: logger},
	}

	rep, err := s.Run(ctx)
	if err != nil {
		// 扫描本身失败（RBAC 缺失、API Server 不可达）才算失败。
		// 见下面关于退出码的说明。
		logger.Error(err, "对账扫描失败")
		os.Exit(1)
	}

	if err := printReport(rep, outputPath, logger); err != nil {
		logger.Error(err, "写出报告失败")
		os.Exit(1)
	}

	// # 退出码的语义：它回答"对账有没有正常工作"，而不是"集群干不干净"
	//
	// 发现泄漏时仍然返回 0。理由：CronJob 的失败状态是给"任务本身坏了"
	// 用的。若把"有发现"也映射成失败，那么这个 Job 在存在任何长期泄漏时
	// 会永远处于 Failed —— 而"永远失败"最终等价于"没人看"，
	// 它同时还会淹没真正需要告警的情况（比如 RBAC 被收走导致的扫描失败）。
	// 泄漏的告警应当来自指标与日志，那里才有分类与趋势。
	if len(rep.Errors) > 0 {
		logger.Info("本轮有部分动作执行失败", "errors", len(rep.Errors))
	}
}

// printReport 输出报告。
func printReport(rep *sweeper.Report, outputPath string, logger logr.Logger) error {
	body, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	// 报告整体打进日志：CronJob 的日志就是这轮对账唯一的现场记录，
	// 而现场记录的价值在于"事后能完整复现当时看到了什么"。
	fmt.Println(string(body))

	if outputPath != "" {
		if dir := filepath.Dir(outputPath); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
		if err := os.WriteFile(outputPath, body, 0o600); err != nil {
			return err
		}
	}

	logger.Info("对账完成",
		"sandboxes", rep.Sandboxes,
		"pods", rep.Pods,
		"leases", rep.Leases,
		"netpols", rep.Netpols,
		"volumes", rep.Volumes,
		"findings", len(rep.Findings),
		"applied", rep.Applied,
		"dryRun", rep.DryRun,
		"truncated", rep.Truncated)
	return nil
}

// logReporter 把发现计数写进日志。
//
// 生产的指标导出应当接 Prometheus（docs/08 §2.1 的
// `sandbox_leak_sweeper_found_total`）。这里用日志是为了让这个一次性
// 进程不引入 push gateway 依赖 —— 而日志采集端（Loki/ELK）完全可以
// 从这行结构化日志里派生出同一个指标。
type logReporter struct {
	logger logr.Logger
}

// Found 实现 sweeper.Reporter。
func (r *logReporter) Found(kind sweeper.Kind, n int) {
	if n == 0 {
		// 只为非零项打日志：九个类别里有七个恒为 0，全打出来会把
		// 真正有信息量的那两行淹没 —— 而"指标为 0"这件事由报告里的
		// byKind 映射承载，不需要九行日志。
		return
	}
	r.logger.Info("对账发现", "kind", string(kind), "count", n)
}

// Applied 实现 sweeper.Reporter。
func (r *logReporter) Applied(action sweeper.Action, n int) {
	if n == 0 {
		return
	}
	r.logger.Info("已执行动作", "action", string(action), "count", n)
}

// restConfig 加载集群配置。
func restConfig(kubeconfig string) (*rest.Config, error) {
	// 在集群内运行时走 in-cluster 配置。
	if cfg, err := ctrl.GetConfig(); err == nil {
		return cfg, nil
	}
	if kubeconfig == "" {
		if home := homedir.HomeDir(); home != "" {
			kubeconfig = filepath.Join(home, ".kube", "config")
		}
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}
