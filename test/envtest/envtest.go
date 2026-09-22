// Package envtest 提供"真实 kube-apiserver + etcd"的测试环境。
//
// # 它为什么重要
//
// envtest 不需要 Docker：setup-envtest 会下载 etcd 与 kube-apiserver 两个二进制，
// 在本地起一个真实的 API Server。这让我们能验证一些**用 fake client 根本测不到**
// 的行为：
//
//   - 乐观并发：resourceVersion 冲突是 API Server 的真实语义，
//     fake client 没有这个概念，永远"通过"，给出虚假的安全感
//   - CRD 注册：CEL 规则在 CRD 创建时由 API Server 编译校验，
//     规则写错会直接失败 —— 这是唯一能在本地验证 CEL 的方式
//   - SSA 合并、listType=listMapKey 的语义
//
// # 它做不到什么
//
// envtest **没有 kubelet 与 scheduler**，因此 Pod 永远不会变成 Running。
// 完整端到端（申请 → 就绪 → 回收）仍然需要 kind 或真实集群。
package envtest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlenvtest "sigs.k8s.io/controller-runtime/pkg/envtest"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
)

// Env 是一个已启动的测试环境。
type Env struct {
	Environment *ctrlenvtest.Environment
	Client      client.Client
	Scheme      *runtime.Scheme
}

// Available 报告 envtest 资产是否就绪。
//
// 用 KUBEBUILDER_ASSETS 作为开关而不是自己找二进制：这是 setup-envtest 的
// 标准约定，CI 与本地用同一套机制，不需要第二份配置。
func Available() bool { return os.Getenv("KUBEBUILDER_ASSETS") != "" }

// SkipUnlessAvailable 在资产缺失时跳过测试，并给出**可执行的**提示。
//
// 刻意在提示里写清怎么跑起来：否则"跳过"会变成"永远没跑过还不知道"——
// 一个测试长期被跳过，比没有这个测试更危险，因为它会让人以为覆盖到了。
func SkipUnlessAvailable(t *testing.T) {
	t.Helper()
	if !Available() {
		t.Skip("KUBEBUILDER_ASSETS 未设置，跳过需要真实 API Server 的测试。\n" +
			"  本地运行：make test-envtest\n" +
			"  或先执行：setup-envtest use 1.32.0 --bin-dir <dir> -p path 并设置 KUBEBUILDER_ASSETS")
	}
}

// Start 启动 API Server、安装本项目 CRD，并返回一个直连 client。
//
// 注意返回的是**非缓存** client：测试里要的是权威读，而不是 informer 快照。
// 缓存的存在（以及它的滞后）本身需要被单独测试，不能在所有测试里默认引入。
func Start(t *testing.T) *Env {
	t.Helper()
	SkipUnlessAvailable(t)

	env := &ctrlenvtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join(RepoRoot(t), "config", "crd", "bases")},
		// CRD 缺失必须报错而不是静默跳过：否则所有 CR 操作都会以
		// "no matches for kind" 失败，排查起来绕远路。
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("启动 envtest 失败（若失败信息提到 CEL，说明 config/crd 里的校验规则不合法）: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			// Windows 上会报 "unable to signal for process ... not supported by windows"：
			// controller-runtime 的进程终止在 Windows 上只实现了"不支持"。
			// 实测进程**不会残留**（已用 Get-Process 复核过），所以这里只记录不失败；
			// 但保留日志 —— 万一将来真的开始漏进程，痕迹还在。
			t.Logf("停止 envtest 返回错误（Windows 上属已知限制，进程已确认不残留）: %v", err)
		}
	})

	sch := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(sch))
	utilruntime.Must(sandboxv1alpha1.AddToScheme(sch))

	c, err := client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		t.Fatalf("构造 client 失败: %v", err)
	}

	// envtest 起的是**裸 API Server**：没有 default、kube-system，也没有我们自己的
	// 命名空间。不建它们，所有 Create 都会以 `namespaces "sandbox-pool" not found`
	// 失败 —— 而这个错误信息指向命名空间，容易让人误以为是 CRD 或 RBAC 问题。
	ctx := context.Background()
	for _, ns := range []string{sandboxv1alpha1.NamespacePool, sandboxv1alpha1.NamespaceSystem} {
		if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatalf("创建命名空间 %s 失败: %v", ns, err)
		}
	}

	return &Env{Environment: env, Client: c, Scheme: sch}
}

// RepoRoot 从当前工作目录向上找到含 go.mod 的目录。
//
// 为什么不用相对路径 "../../.."：测试二进制的工作目录是包目录，
// 相对层数会随包位置变化。写死层数的结果是"换个目录跑就找不到 CRD"，
// 而失败信息只会说"路径不存在"，不会告诉你是层数错了。
func RepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("获取工作目录失败: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("向上查找未能找到 go.mod，无法定位仓库根目录")
		}
		dir = parent
	}
}
