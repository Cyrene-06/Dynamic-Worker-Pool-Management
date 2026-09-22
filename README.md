<div align="center">
  <h1>基于 Kubernetes 的 Agent 沙箱生命周期管理与资源优化</h1>
  <p><b>Dynamic Worker Pool Management</b> · <a href="https://github.com/Cyrene-06/Dynamic-Worker-Pool-Management">Cyrene-06/Dynamic-Worker-Pool-Management</a></p>
  <p>🚀Kubernetes+Go 的 Agent 沙箱生命周期管理与资源优化平台，为海量短生命周期、需强隔离的 Agent 负载提供企业级沙箱控制面。它集成了声明式 CRD（AgentSandbox / SandboxPool / SandboxTemplate）、预热池与 CAS 认领协议、TTL 与空闲双触发回收、Finalizer 泄漏防护、Kata/Firecracker 分级隔离、水位闭环弹性扩缩、超卖分级与画像分档、指标告警与安全审计等生产化能力。</p>
</div>

<div align="center">
  <img src="https://img.shields.io/badge/kubernetes-1.32%2B-blue" alt="kubernetes" />
  <img src="https://img.shields.io/badge/go-1.27%2B-00ADD8" alt="go" />
  <img src="https://img.shields.io/badge/controller--runtime-v0.24.1-lightBlue" alt="controller-runtime" />
  <img src="https://img.shields.io/badge/kata--containers-3.4-orange" alt="kata-containers" />
  <img src="https://img.shields.io/badge/firecracker-1.7-purple" alt="firecracker" />
  <img src="https://img.shields.io/badge/cilium-1.16-green" alt="cilium" />
  <img src="https://img.shields.io/badge/karpenter-v1-yellow" alt="karpenter" />
  <img src="https://img.shields.io/badge/license-Apache--2.0-blue" alt="license" />
  <img src="https://img.shields.io/badge/status-M0%20完成%20%7C%20M1%20进行中-yellow" alt="status" />
  <img src="https://img.shields.io/badge/PRs-welcome-brightgreen" alt="PRs welcome" />
</div>

<br>

<p align="center">
  <b>简体中文</b> | English（待补充）
</p>

---

## 项目文档

- **源码仓库**: [Cyrene-06/Dynamic-Worker-Pool-Management](https://github.com/Cyrene-06/Dynamic-Worker-Pool-Management)
- **设计文档总目录**: [docs/](docs/)（共 10 篇，索引见 [6.1 设计文档索引](#61-设计文档索引)）
- **需求与 SLO 量化**: [docs/01-requirements.md](docs/01-requirements.md)
- **技术选型对比分析**: [docs/02-tech-selection.md](docs/02-tech-selection.md)（11 项选型 + 14 项被否决方案）
- **总体架构**: [docs/03-architecture.md](docs/03-architecture.md)
- **CRD 与生命周期状态机**: [docs/04-api-and-state-machine.md](docs/04-api-and-state-machine.md)
- **预热池与弹性伸缩**: [docs/05-warm-pool-and-scaling.md](docs/05-warm-pool-and-scaling.md)
- **隔离运行时与节点池**: [docs/06-isolation-runtime.md](docs/06-isolation-runtime.md)
- **资源优化策略**: [docs/07-resource-optimization.md](docs/07-resource-optimization.md)
- **可观测性与安全**: [docs/08-observability-security.md](docs/08-observability-security.md)
- **多环境部署方案**: [docs/09-deployment-environments.md](docs/09-deployment-environments.md)
- **里程碑与风险登记**: [docs/10-roadmap-risks.md](docs/10-roadmap-risks.md)

## 架构速览

```mermaid
flowchart LR
    U[Agent 调度器/业务服务] -->|1. 申请| API[AgentSandbox CR]
    API --> SC[Sandbox Controller]
    SC -->|2a. 池命中| WP[(Warm Pool<br/>Ready Pod 库存)]
    SC -->|2b. 冷路径| CRE[创建 Pod]
    WP -->|3. 认领 claim| POD[Sandbox Pod]
    CRE --> POD
    POD -->|4. 使用| AG[Agent 运行时]
    AG -->|5. 空闲/超时| SC
    SC -->|6. 回收/休眠| POD
    PC[Pool Controller] -.->|维持水位| WP
    RO[Resource Optimizer] -.->|回写规格| POD
    RO -.->|画像数据| PROM[(Prometheus)]
    PROM --> RO
```

<table>
  <tr>
    <td width="50%" valign="top">
      <h4>🔥 热路径（池命中）</h4>
      <p>申请 → 认领库存沙箱 → 注入会话上下文</p>
      <p><b>目标 P95 &lt; 800 ms</b></p>
      <p>认领 = 一次带 <code>resourceVersion</code> 前置条件的 CAS Patch，天然分布式安全，无需额外协调服务。</p>
    </td>
    <td width="50%" valign="top">
      <h4>🧊 冷路径（池空兜底）</h4>
      <p>申请 → 创建 Pod → 调度 / 镜像 / microVM 启动</p>
      <p><b>目标 P95 &lt; 2.5 s</b></p>
      <p>不是常态路径。分阶段延迟预算分解见 <a href="docs/03-architecture.md">docs/03</a>。</p>
    </td>
  </tr>
</table>

## 重要提示

1. **当前处于 M0 骨架阶段**：已落地 CRD 定义、隔离抽象层、状态机与控制器骨架（`internal/controller` 的 `NextPhase` 是纯函数，21 个单测全绿）。**尚未实现**：`PoolController`、`sandbox-gateway`、Pod 之外的资源清理（M1）。隔离验证仍需带 `/dev/kvm` 的环境。
2. 阅读或实践本项目需要一定的 **Kubernetes Operator 开发（Go）** 与 **容器运行时** 基础。
3. 主隔离方案 Kata Containers + Firecracker 要求节点具备 `/dev/kvm`（裸金属或支持嵌套虚拟化的实例）；**本地 kind 环境无法真实验证隔离**，只能用 `simulated` 模式验证控制逻辑。
4. 文档中所有性能数字均为**量级参考**，必须以本项目的压测基线校正后才能写入 SLO。
5. 本项目**不含** Agent 业务逻辑（推理编排、工具调用实现）与 GPU 直通场景，边界见 [docs/01-requirements.md](docs/01-requirements.md)。
6. 仓库尚未包含 `LICENSE` 文件，将于首个代码里程碑补齐（计划 Apache License 2.0）。在此之前请勿将本仓库内容用于商业分发。

<br>

## 1. 基本介绍

### 1.1 项目介绍

> 本项目是一个建立在 Kubernetes 之上的**面向 Agent 工作负载的沙箱控制面**：用自定义 Operator 管理 `AgentSandbox` 的完整生命周期（含泄漏防护），用**预热池 + 认领协议**把冷启动从秒级压到百毫秒级，用**分级隔离（runc / Kata-Firecracker）+ 四层资源优化**在满足隔离与 SLO 的前提下把节点利用率拉高。

**范围说明**

- **包含**：控制面架构、CRD 与状态机设计、池化与扩缩策略、隔离运行时选型与落地、资源优化模型、可观测性与安全设计、多环境部署方案、里程碑与风险。
- **不包含**（明确排除）：Agent 业务逻辑本身、Kubernetes 自身改造、GPU 训练类负载调度、跨集群联邦调度。

### 1.2 贡献指南

Hi! 首先感谢你关注本项目。

本项目是一套面向生产环境的平台设计，任何设计层面的修正、反例与实战数据都比"多写几页文档"更有价值。如果你愿意贡献代码或提出建议，请先阅读以下内容。

#### 1.2.1 Issue 规范

- issue 仅用于提交 Bug、Feature 或设计相关的内容，其它内容可能会被直接关闭。
- 提交 issue 之前，请先搜索相关内容是否已被提出。
- 若为**设计争议**，请在 issue 中给出：现状描述 → 你的结论 → **依据（数据/上游文档/实测）** → 反转条件。

#### 1.2.2 Pull Request 规范

- 请先 fork 一份到自己的项目下，不要直接在仓库下建分支。
- commit 信息以 `[文件名]: 描述信息` 的形式填写，例如 `docs/05-warm-pool-and-scaling.md: 修正安全库存公式`。
- 修正 Bug 或数据错误时，请在 PR 描述中给出复现方式或数据来源。
- **性能数字**的改动必须附压测方法与原始数据，不接受"经验上应该是"。
- 合并代码需要两名维护人员参与：一人 review 后 approve，另一人再次 review，通过后即可合并。

## 2. 使用说明

**环境要求**

- Kubernetes >= **v1.30**
- golang >= **v1.27.1**（与 `go.mod` 的 `go` 指令一致；低版本会直接拒绝构建该模块）
- containerd >= **1.7**（如需真实隔离，节点需具备 `/dev/kvm` 与 `vhost_vsock`）
- IDE 推荐：**VS Code**（`Go` + `Kubernetes` 扩展）或 **Goland**

> **依赖版本钉死说明**：`sigs.k8s.io/controller-runtime` 固定在 **v0.24.1**，不要升级到 v0.25.x。
> v0.25.x 在 **Windows 上无法编译**：`pkg/internal/testing/process/signal_windows.go` 定义了 `signalProcess`，
> 与同包内平台无关文件中的同名符号重复声明（`signalProcess redeclared in this block`），
> 而 `signal_other.go` / `signal_unix.go` 用的是 `signalProcessImpl`。v0.25.1 是该分支最新版，上游尚无修复。
> 该包仅在进程内 envtest 测试路径上被编译，因此**只影响本地/CI 的 Windows 开发者**。

### 2.1 控制面 Operator

```bash
# 克隆项目
git clone https://github.com/Cyrene-06/Dynamic-Worker-Pool-Management.git
cd Dynamic-Worker-Pool-Management

# 生成 CRD / RBAC 清单与 DeepCopy（改了 api/ 下的类型后必跑）
make manifests generate

# 单元测试：纯函数状态机 + 隔离抽象层 + 弹性算法，不需要集群、不需要 KVM
make test

# 需要真实 API Server 的测试：CAS 认领并发冲突、CRD 校验（依赖 envtest，不需要 Docker）
make test-envtest

# 完整验证（格式 + vet + 构建 + 测试）—— 提交前跑这一条就够
make verify

# 安装 CRD 并把 Operator 跑起来（本地 simulated 隔离）
make install
make deploy-local
```

> Windows 上没有 make 时，用等价入口：
> `powershell -ExecutionPolicy Bypass -File hack/verify.ps1`
> （支持 `-Task verify|fmt|vet|build|test|manifests|generate`）

> **envtest 说明**：`make test-envtest` 会调用 `setup-envtest` 下载并缓存 etcd / kube-apiserver 二进制，
> **完全不需要 Docker**（与 kind 不同）。未设置 `KUBEBUILDER_ASSETS` 时，`internal/claim` 的集成测试会
> 主动 **skip** 而不是失败——目的是让"没装 envtest"的开发者也能跑 `make test` 拿到真实绿灯，
> 而不是把环境缺失伪装成跳过。CI 中请务必设置该变量。

### 2.2 本地开发集群（kind）

```bash
# 创建集群
kind create cluster --config hack/kind-config.yaml

# 应用示例命名空间 / 模板 / 池 / 沙箱（simulated 隔离，回落 runc）
kubectl apply -k config/samples

# 观察状态机推进（Ready = 池中库存，Running = 已售出）
kubectl get agentsandbox -n sandbox-pool -w
```

> E1 默认保留 kind 自带的 kindnet，**不需要先装 Cilium** —— 它的目的是验证控制面逻辑。
> 出口白名单（`CiliumNetworkPolicy`）的验证放在 E2/E3，见 [docs/06 §7.2](docs/06-isolation-runtime.md)。
> 本地集群**无法**验证隔离强度与启动延迟，这两项必须去带 `/dev/kvm` 的环境。

### 2.3 CRD 与 API 参考

```bash
# 查看已注册的 CRD
kubectl get crd | grep sandbox.example.com

# 查看某个沙箱的实时状态与条件（Phase / Conditions）
kubectl get agentsandbox -n sandbox-pool -o yaml
kubectl describe sandboxpool fc-small
```

> 3 个 CRD 的 OpenAPI schema 由 `make manifests` 从 Go 类型生成到 `config/crd/bases/`，**已随仓库提交**；
> 字段级设计说明与状态机语义见 [docs/04-api-and-state-machine.md](docs/04-api-and-state-machine.md)。
> 需要可读的 API 文档时直接看 `kubectl explain agentsandbox.spec`（schema 已带完整描述与校验）。

### 2.4 真实隔离环境（KVM 单节点）

> ⚠️ 本地 kind / minikube **无法**真实验证隔离与启动延迟，必须使用带 `/dev/kvm` 的环境。

```bash
# 前置校验（CPU 虚拟化扩展 / /dev/kvm / vhost-vsock / cgroup v2）
sudo apt-get install -y cpu-checker && sudo kvm-ok
ls -l /dev/kvm /dev/vhost-vsock

# 单节点集群 + 节点标签 + Cilium + Kata
sudo kubeadm init --pod-network-cidr=10.244.0.0/16
kubectl label node $(hostname) sandbox.example.com/isolation=kata-fc
helm install kata-deploy oci://ghcr.io/kata-containers/kata-deploy-charts/kata-deploy -n kube-system

# 冒烟：Pod 内内核版本必须与宿主不同，才证明是独立内核
kubectl apply -f hack/smoke/kata-fc-pod.yaml
kubectl exec -it kata-smoke -- uname -r
```

### 2.5 VSCode 工作区

- **开发**：使用 VS Code 打开仓库根目录，`Go` 扩展会自动加载 `go.mod`；`config/` 下是 Kustomize 清单（`crd` / `isolation` / `rbac` / `samples`）。
- **运行/调试**：`.vscode/launch.json` 提供 `Operator` 调试配置（`simulated` 隔离，对接本地 kubeconfig）；`Gateway` / `Both` 配置随 M1 的接入层一起补齐。
- **settings**：`.vscode/settings.json` 中的 `go.toolsEnvVars` 用于 VS Code 自身 Go 工具的环境变量。
  **本项目不提交任何绝对路径**（如 `KUBEBUILDER_ASSETS`）—— 它随机器而异，写进仓库必然在别人机器上失效；
  请改为在自己的 shell 或用户级配置中设置。

## 3. 技术选型

- **控制面**：使用 [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime) 构建 Operator，代码生成只用 `controller-gen`（`controller-tools`）；不引入 operator-sdk 的重框架。
  ⚠️ **不使用 kubebuilder 脚手架**：v4 没有 Windows 发行版，且 `sigs.k8s.io/kubebuilder/v4` 模块不含 `cmd` 包，
  `go install` 必然失败。骨架按 [docs/09](docs/09-deployment-environments.md) 的 Kustomize 布局手写，
  这样 macOS / Linux / Windows 三端行为一致，且避免脚手架生成一堆没人维护的文件。
- **抽象层**：自定义 CRD（`AgentSandbox` / `SandboxPool` / `SandboxTemplate`），承载状态机、TTL、认领、休眠等原生对象无法表达的语义。
- **隔离运行时**：主用 [Kata Containers](https://katacontainers.io/) + [Firecracker](https://firecracker-microvm.github.io/)，通过 `RuntimeClass` 暴露；[Cloud Hypervisor](https://www.cloudhypervisor.org/) 作为需更多设备能力时的折中；runc 用于可信负载与本地开发。
- **调度**：默认 `kube-scheduler` 的第二个 profile（`NodeResourcesFit` = `MostAllocated`）实现装箱，不引入第三方调度器。
- **网络**：使用 [Cilium](https://cilium.io/) 提供默认拒绝、`toFQDNs` 出口白名单与 Hubble 观测（`CiliumNetworkPolicy`）。
- **节点扩缩**：使用 [Karpenter](https://karpenter.sh/) 管理节点池，配合负优先级占位 Pod 预铺节点容量。
- **资源优化**：画像分档（ConfigMap 维护档位）+ 超卖规则引擎；VPA 仅以 `Off` 模式作为画像数据源，不做自动决策。
- **观测**：Prometheus + OpenTelemetry + Grafana / Loki / Tempo；cAdvisor 与节点侧 VMM 进程 RSS 交叉校验内存画像。
- **配置与部署**：Kustomize base + per-env overlay，禁用"仅本地可用"的代码分支。

## 4. 项目架构

### 4.1 系统架构图

```mermaid
flowchart TB
    subgraph L0["接入层"]
        GW[sandbox-gateway<br/>REST/gRPC]
    end
    subgraph L1["声明层"]
        TPL[SandboxTemplate]
        POOL[SandboxPool]
        SBX[AgentSandbox]
    end
    subgraph L2["控制层 sandbox-system"]
        SC[SandboxController]
        PC[PoolController]
        TC[TemplateController]
        ROC[ResourceOptimizer]
        ADM[Admission 配额/校验]
    end
    subgraph L3["执行层"]
        SCHED[kube-scheduler<br/>sandbox-binpack]
        RT[RuntimeClass<br/>kata-fc / runc]
        CNI[Cilium]
        CSI[CSI / ephemeral volumes]
    end
    subgraph L4["节点层"]
        NA[sandbox-node-agent<br/>镜像预热 / VMM 观测]
    end
    subgraph L5["观测与状态层"]
        PROM[Prometheus]
        STATE[(状态存储<br/>对象存储 + KV)]
    end
    L0 --> L1 --> L2 --> L3 --> L4
    L4 --> L5
    L2 -.-> L5
```

### 4.2 生命周期状态机

```mermaid
stateDiagram-v2
    [*] --> Pending
    Pending --> Provisioning
    Provisioning --> Ready: 库存就绪
    Provisioning --> Running: 冷路径直接持有
    Ready --> Running: 认领成功
    Ready --> Terminating: 库龄轮换 / 池 drain
    Running --> Idle: 无活动 / 心跳丢失
    Idle --> Running: 续租 / 观测到活动
    Idle --> Hibernating: 达空闲阈值
    Hibernating --> Hibernated
    Hibernated --> Resuming: 业务唤醒
    Resuming --> Running
    Idle --> Terminating: 达回收阈值
    Running --> Terminating: 释放 / TTL / 硬期限
    Terminating --> Succeeded
    Provisioning --> Failed
    Ready --> Failed: 节点失联
    Running --> Failed: 节点失联 / OOM
    Terminating --> Failed: 清理部分失败
    Succeeded --> [*]
    Failed --> [*]
```

> `Ready` = **池中库存（未认领）**，`Running` = **已售出**。池水位就是 `count(phase=Ready)`。完整迁移表与不变式见 [docs/04-api-and-state-machine.md](docs/04-api-and-state-machine.md)。

### 4.3 目录结构

```
.
├── api/v1alpha1/                 # CRD 类型 + labels（跨组件契约）+ zz_generated.deepcopy.go
├── cmd/operator/main.go          # 装配层：scheme / 缓存收窄 / 隔离层 / 控制器
├── internal/
│   ├── controller/               # NextPhase 纯函数 + SandboxReconciler + tiers
│   └── isolation/                # 隔离级别抽象层（配置校验 + 降级策略 + 探测）
├── config/
│   ├── crd/                      # 生成的 CRD 清单 + kustomization
│   ├── isolation/                # levels.yaml（隔离级别的唯一配置入口）+ ConfigMap
│   ├── rbac/                     # 最小权限 RBAC，兼当可执行安全基线
│   └── samples/                  # 命名空间 / 模板 / 池 / 沙箱示例
├── hack/
│   ├── kind-config.yaml          # E1 集群配置
│   ├── smoke/                    # Kata / 网络策略冒烟用例
│   ├── bootstrap/                # 节点初始化与 KVM/vsock 自检
│   └── verify.ps1                # Windows 验证入口
├── docs/                         # 设计文档 01–10
├── Makefile                      # Linux / CI 验证入口
└── go.mod  go.sum
```

**规划中（M1）**：`cmd/gateway`、`cmd/node-agent`、`internal/claim`（CAS 认领协议）、
`internal/sweeper`（泄漏对账）、`test/e2e`、`test/conformance`。

## 5. 主要功能

**生命周期管理**

- **完整状态机**：11 个 Phase、7 类 Condition，所有迁移都有原因码（`recycleReason`）与事件，不允许"黑盒回收"。
- **双触发回收**：心跳（`coordination.k8s.io/v1 Lease`）为主判据 + 空闲检测（eBPF 网络活动 / cgroup CPU 增量）为辅 + TTL 兜底上限，避免长任务被误杀与僵尸占用。
- **泄漏防护**：5 个 Finalizer 按 `lease → network → state-flush → volume → metrics` 顺序回收，每步 30s 超时强制推进；独立对账 Sweeper 兜底异常路径。
- **沙箱复用**：同租户原地转正复用，`resetHook` 强制清理 + `maxClaimCount` 轮换；**跨租户复用硬编码禁止**。

**池化与弹性伸缩**

- **预热池分级**：L0 节点容量 → L1 镜像预热 → L2 Ready 库存 → L3 冻结库存 → L4 快照库存，按成本收益比依序实施。
- **CAS 认领协议**：`resourceVersion` 乐观锁实现排他认领，无中心化分配器，天然可伸缩。
- **水位闭环控制**：EWMA + 趋势预测（前馈）+ 命中率反馈 + 平方根安全库存公式，配非对称冷却、滞回、限速与振荡检测。
- **雪崩保护**：容量危机下进入 ProtectMode，拒绝低优申请、限制并发，避免"扩容→压垮 API Server→更多重试"的自激放大。
- **节点容量预铺**：负优先级占位 Pod + 高优抢占，解决"池是秒级、节点是分钟级"的时间常数不匹配。

**隔离与安全**

- **分级隔离**：`simulated` / `runc` / `kata-fc` / `kata-clh` 通过配置切换，控制逻辑在 kind 上可完整验证。
- **RuntimeClass 原生约束**：`overhead` 精确计入 VMM 开销，`scheduling.nodeSelector` 保证 Pod 不落到错误节点池。
- **出口治理**：Cilium `toFQDNs` 白名单 + 默认拒绝，显式阻断 API Server、云元数据地址与内网网段。
- **威胁模型**：16 类威胁与控制矩阵，含逃逸响应剧本、镜像签名准入、短期凭据与多租户模型。

**资源优化**

- **画像分档**：以 P95 使用量映射档位，替代逐沙箱精调 requests，避免调度碎片；提供"零碎片"设计规则（档位/开销/节点容量同粒度整数倍）。
- **超卖分级**：CPU 可压缩、内存不可压缩，超卖仅对可重启沙箱开放并设硬上限；Kata 路径下 `limits.memory` 即 guest RAM，是否真省内存取决于 Firecracker 预分配策略。
- **休眠回收**：L1 `cgroup.freeze` 释放 CPU（低成本、可逆、典型节省 70%）→ L3 状态外置 + 销毁重建 → L5 Firecracker 快照（进阶 PoC）。
- **装箱与整理**：`MostAllocated` 装箱 + `PodTopologySpread` 限制故障域；碎片整理仅在低峰执行。

**可观测与运维**

- **指标契约**：严格基数控制（禁止把 `sandbox_id`/`session_id` 放进 label），`reason` 必须是枚举。
- **告警与 Runbook**：16 条告警全部绑定 runbook 与"用户可感知影响"，不因"指标异常"而告警。
- **追踪与审计**：`requestId` 全链路传播，失败与冷路径 100% 采样；审计覆盖申请/生命周期/出口访问/控制面操作。
- **可运维开关**：`pauseScaling`（冻结自动扩缩）、`disableColdPath`（保护节点）、`maxWarm: 0`（一键止血）、池 drain（升级前排空）。

## 6. 知识库

### 6.1 设计文档索引

| 文档 | 内容 | 建议阅读顺序 |
|---|---|---|
| [01 需求与目标](docs/01-requirements.md) | 场景、功能/非功能需求、SLO 量化、边界与非目标 | ① |
| [02 技术选型分析](docs/02-tech-selection.md) | 11 项关键选型的对比矩阵、结论、**被否决方案及原因** | ② |
| [03 总体架构](docs/03-architecture.md) | 分层架构、组件职责、部署拓扑、关键时序、HA 与并发控制 | ③ |
| [04 API 与状态机](docs/04-api-and-state-machine.md) | CRD 字段设计、Phase/Condition、Finalizer、认领协议 | ④ |
| [05 预热池与弹性伸缩](docs/05-warm-pool-and-scaling.md) | 池分级、水位算法、防抖动、与 Karpenter/CA 协同、容量公式 | ⑤ |
| [06 隔离与运行时](docs/06-isolation-runtime.md) | RuntimeClass、Kata/Firecracker 落地、节点规格、存储与网络 | ⑥ |
| [07 资源优化](docs/07-resource-optimization.md) | 四层优化模型、超卖建模、休眠回收、装箱与碎片整理、收益验证 | ⑦ |
| [08 可观测性与安全](docs/08-observability-security.md) | 指标/日志/追踪规范、告警、威胁模型与控制措施 | ⑧ |
| [09 多环境部署](docs/09-deployment-environments.md) | 本地/生产环境矩阵、安装顺序、配置矩阵、升级回滚 | ⑨ |
| [10 里程碑与风险](docs/10-roadmap-risks.md) | 4 个里程碑、验收标准、风险登记表、开放问题 | ⑩ |

### 6.2 参考资料

- **Kubernetes**：[RuntimeClass](https://kubernetes.io/docs/concepts/containers/runtime-class/)、[Pod Topology Spread](https://kubernetes.io/docs/concepts/scheduling-eviction/topology-spread-constraints/)、[KubeSchedulerConfiguration](https://kubernetes.io/docs/reference/scheduling/config/)、[CRD Validation (CEL)](https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definitions/#validation-rules)
- **运行时**：[Kata Containers 文档](https://katacontainers.io/docs/)、[Firecracker 设计](https://github.com/firecracker-microvm/firecracker/blob/main/docs/design.md)、[Cloud Hypervisor](https://www.cloudhypervisor.org/)
- **Operator 开发**：[Kubebuilder Book](https://book.kubebuilder.io/)、[controller-runtime](https://github.com/kubernetes-sigs/controller-runtime)
- **网络与节点**：[Cilium Network Policy](https://docs.cilium.io/en/stable/security/policy/)、[Karpenter](https://karpenter.sh/docs/)
- **资源效率**：[Vertical Pod Autoscaler](https://github.com/kubernetes/autoscaler/tree/master/vertical-pod-autoscaler)、[Kubernetes 调度器插件](https://github.com/kubernetes-sigs/scheduler-plugins)

## 7. 联系方式

- **问题反馈**: 提交 GitHub Issue（模板见 [1.2.1](#121-issue-规范)）
- **设计讨论**: GitHub Discussions
- **安全漏洞**: 请勿公开提交 Issue，通过私下渠道向维护者披露（后续补充 `SECURITY.md`）
- **交流群**: 待建立

## 8. 贡献者

感谢每一位为本项目提交设计修正、反例与实测数据的人。

贡献者名单与贡献排行将随首个代码里程碑公布（可参考 GitHub 仓库的 Contributors 页面）。

## 9. 支持我们

如果这套设计对你有帮助，欢迎：

- ⭐ **Star** 本仓库，让更多做 Agent 平台的人看到；
- 🐛 提交你在落地过程中遇到的**反例**与**踩坑记录**（这比通用文档更有价值）；
- 📝 提交 PR 修正公式、参数或性能数字（需附依据）。

## 10. 注意事项

1. 使用、修改和分发本仓库内容时，请遵循仓库中的 `LICENSE`，并保留许可证要求的适用声明。仓库尚未包含 `LICENSE` 文件，将于首个代码里程碑补齐。
2. 文档中的所有性能与容量数字均为**设计目标或量级参考**，不是实测承诺。生产决策前请以自建压测基线为准。
3. 本项目不提供 SLA 承诺；若需商业支持请联系维护者。
4. 文中提及的第三方项目与商标（Kubernetes® 归 CNCF 所有、Kata Containers 归 OpenInfra Foundation、Firecracker 归 Amazon、Cilium 归 Isovalent 等）归各自所有者所有，本项目与它们无从属关系。

<br>

<div align="center">
  <img src="https://img.shields.io/badge/kubernetes-1.32%2B-blue" alt="kubernetes" />
  <img src="https://img.shields.io/badge/kata--containers-3.4-orange" alt="kata" />
  <img src="https://img.shields.io/badge/firecracker-1.7-purple" alt="firecracker" />
  <img src="https://img.shields.io/badge/go-1.27%2B-00ADD8" alt="go" />
</div>

<div align="center">
  <sub>Agent Sandbox Control Plane · M0 骨架 · 欢迎 Issue / PR</sub>
</div>
