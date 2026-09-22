# 02 · 技术选型分析

[← 上一篇：需求与目标](01-requirements.md) | [返回导航](../README.md) | [下一篇：总体架构 →](03-architecture.md)

> 本文给出 11 项关键选型的对比与结论，并单列**被否决方案及原因**。所有性能数字为**量级参考**，实际必须用本项目的压测基线校正。

---

## 选型总览

| # | 决策点 | 结论 | 核心理由 |
|---|---|---|---|
| D1 | 控制器框架 | **Go + controller-runtime**（kubebuilder 脚手架） | 生态标准、性能、informer 缓存、Server-Side Apply |
| D2 | 抽象层 | **自定义 CRD**（不用裸 Pod/Job） | 状态机、TTL、认领、快照语义无处安放 |
| D3 | 隔离运行时 | **Kata Containers，主用 Firecracker，备选 Cloud Hypervisor** | VM 级隔离 + 亚秒启动，K8s 原生 RuntimeClass |
| D4 | 池化机制 | **自研 Warm Pool + Claim 协议** | 需要 1:1 排他认领，K8s 无此语义 |
| D5 | 扩缩驱动 | **自研 Pool Controller 为主，暴露 `/scale` 供 KEDA 选用** | HPA 稳定窗口与"维持库存"语义不匹配 |
| D6 | 资源推荐 | **画像分档 + VPA(Off/recommendation only)** | 逐沙箱精调会制造调度碎片 |
| D7 | 休眠方案 | **默认"状态外置 + 重建"；快照为进阶 PoC** | Kata 路径下来宾内存回收不可控 |
| D8 | 调度策略 | **默认调度器 + 第二个 profile（MostAllocated 装箱）** | 无需引入第三方调度器 |
| D9 | 网络策略 | **Cilium** | FQDN 出口白名单、per-pod 策略、无 iptables、Hubble |
| D10 | 存储 | **generic ephemeral volume + 只读镜像层 + 对象存储** | 兼顾隔离、性能与可恢复性 |
| D11 | 多环境 | **RuntimeClass 抽象 + 特性开关；本地 simulated/runc，隔离验证在 KVM 环境** | kind 无法真跑 Kata |

---

## D1 · 控制器框架

| 方案 | 优势 | 劣势 | 适用 |
|---|---|---|---|
| **kubebuilder / controller-runtime** | 事实标准；informer 本地缓存；`Owns()`/`Watches()` 声明式依赖；SSA 支持；`envtest` 可离线测；社区资料多 | 需手写样本量大；生成器有一定学习曲线 | ✅ **本项目** |
| operator-sdk | 基于 kubebuilder 插件体系；带 OLM、Helm/Ansible operator 支持 | 本项目不用 OLM，等价于 kubebuilder + 冗余层 | 需要 OLM 分发时 |
| client-go 手写 | 无生成器，完全可控 | 需手写 informer/workqueue/leader election/重试，易踩坑 | 极简场景 |
| kopf (Python) | 开发快，装饰器风格；适合事件驱动脚本 | 单进程吞吐受限（5k 对象 + 200/s 创建难达标）；GIL；错误处理与并发控制弱 | 原型/低频场景 |
| Fabric8 (Java) | Java 团队友好，Spring 集成 | 启动慢、内存高；本项目无 Java 依赖 | Java 组织 |
| Crossplane | 声明式组合外部资源 | 面向云资源编排，非运行时对象生命周期 | 非适用 |

**结论**：`Go 1.23+` + `controller-runtime`，`kubebuilder` 仅用于 `make manifests` / `generate`。

工程约束（直接影响设计）：

- **并发度**：`MaxConcurrentReconciles` 按对象类型分别设置——`AgentSandbox` 高（如 32），`SandboxPool` 低（如 2，因为它是全局决策者，并发会导致扩容抖动）。
- **缓存**：使用 informer 缓存而非直读 API Server；对 `Pod` 使用 `Watches` + predicate 过滤（只看带特定 label 的 Pod），否则 5k 沙箱会给缓存造成巨大内存压力。
- **写入**：优先 **Server-Side Apply（SSA）** 管理字段所有权，避免多控制器互相覆盖；认领操作用 **`resourceVersion` 乐观锁**（409 冲突即重试下一个候选）。
- **重试**：用 `RequeueAfter` 实现状态机的"定时推进"（TTL/空闲判定），不滥用 `Requeue: true`（会打爆 workqueue）。
- **校验**：能用 **CEL（`x-kubernetes-validations`，1.25 GA）** 表达的约束一律不放 Webhook，减少可用性依赖；只有跨对象校验（如租户配额）才用 ValidatingAdmissionPolicy 或 Webhook。
- **可测试**：逻辑与副作用分离，`envtest` 覆盖状态机；`fake client` 覆盖异常分支。

## D2 · 抽象层：CRD vs 原生对象

| 方案 | 能否表达 TTL | 能否表达认领 | 能否表达状态机 | 结论 |
|---|---|---|---|---|
| 裸 `Pod` | ❌（靠 Job/自研） | ❌ | ❌ | 否 |
| `Job`/`CronJob` | 部分（`activeDeadlineSeconds`） | ❌ | 弱（仅 Completed/Failed） | 否 |
| `Deployment` + `Service` | ❌ | ❌（共享语义） | ❌ | 否（见否决方案 V2） |
| **`AgentSandbox` CRD** | ✅ 原生字段 | ✅ `spec.claimRef` | ✅ Phase/Condition | ✅ |
| `PodTemplate` + 注解约定 | 弱 | 弱 | ❌ | 否 |

**结论**：三层 CRD

- `SandboxTemplate`：可复用的规格模板（镜像、资源档位、挂载、网络策略、RuntimeClass）。
- `SandboxPool`：池策略（水位、隔离级别、模板引用、租户范围）。
- `AgentSandbox`：单个沙箱实例（含 `spec.claimRef`、`status.phase`、TTL 参数）。

> 详细字段见 [04-api-and-state-machine.md](04-api-and-state-machine.md)。

## D3 · 隔离运行时（核心选型）

### 对比矩阵

| 维度 | runc | gVisor (runsc) | Kata + QEMU | **Kata + Firecracker** | Kata + Cloud Hypervisor |
|---|---|---|---|---|---|
| 隔离边界 | 内核命名空间+cgroup | 用户态内核（syscall 拦截） | KVM 硬件虚拟化 | KVM 硬件虚拟化 | KVM 硬件虚拟化 |
| 启动开销（量级） | 50–150 ms | +100–250 ms | +500–1200 ms | **+120–350 ms** | +200–500 ms |
| 每实例内存开销 | ~0 | ~10–30 MB | ~40–80 MB（QEMU 进程） | **~10–25 MB（VMM + guest 内核）** | ~25–50 MB |
| 内核共享 | 是（逃逸=节点失守） | 否（但 syscall 覆盖有缺口） | 否 | 否 | 否 |
| 设备能力 | 全部 | 全部 | 全 PCI/GPU/hotplug | **仅 virtio-net/block/fs/vsock，无 PCI** | PCI/hotplug/vhost-user/GPU |
| 嵌套虚拟化要求 | 无 | 无 | 需 `/dev/kvm` | 需 `/dev/kvm` | 需 `/dev/kvm` |
| 兼容性风险 | 低 | **中高**（部分 syscall/`io_uring`/`eBPF` 不支持） | 低 | 中（内核/rootfs 需适配） | 中低 |
| K8s 集成成熟度 | 原生 | RuntimeClass 成熟 | RuntimeClass 成熟 | RuntimeClass 成熟（`kata-deploy` 提供 `kata-fc`） | 成熟（`kata-clh`） |
| 适合场景 | 可信代码 | 中等隔离 + 高密度 | 需要 GPU/hotplug | **不可信 Agent 代码 + 高频短生命周期** | 需要更多设备能力时的折中 |

### 结论与分层策略

1. **主隔离：Kata + Firecracker（`kata-fc`）** —— 唯一同时满足「VM 级隔离」+「亚秒启动」+「低每实例开销」的组合，与 S1 高频繁场景高度契合。
2. **备选：Kata + Cloud Hypervisor（`kata-clh`）** —— 当需要 PCI/hotplug/更好的内核兼容性时切换到该 RuntimeClass（配置化，非代码改动）。
3. **低敏感 + 本地开发：runc** —— 用于内部可信负载、以及 kind/minikube 上的逻辑验证。
4. **明确不用 gVisor 作为主方案** —— 理由见否决方案 V4。

### Firecracker 的能力边界（必须写进产品契约）

| 限制 | 影响 | 缓解 |
|---|---|---|
| 无 PCI 直通 | 无法 GPU/加速器直通、无 SR-IOV | 另立 GPU 节点池 + runc/CH；超出本项目范围 |
| 无设备热插拔 | 运行中不能挂新卷 | 申请时一次性声明所有卷 |
| 仅 virtio 设备 | 需 virtio-fs / virtio-blk 驱动支持 | 节点镜像定制（含 `virtiofsd`） |
| guest 内核需专用 | 通用发行版内核可能不支持 | 使用 Kata 官方 `vmlinux` + 定制 rootfs |
| 无内存热插拔 | 运行时不能调大内存 | 规格档位申请时确定 |
| 快照需自研集成 | 无 K8s 原生接口 | 见 D7 |

### 版本与前置条件

| 组件 | 建议 | 说明 |
|---|---|---|
| Kubernetes | ≥ 1.30 | RuntimeClass 稳定；`InPlacePodVerticalScaling` beta 自 1.33 |
| containerd | ≥ 1.7 | 需启用 `enable_annotations`（Kata 注解透传） |
| Kata Containers | ≥ 3.3 | `kata-deploy` 生成 `kata-qemu`/`kata-fc`/`kata-clh` RuntimeClass |
| 节点内核 | ≥ 5.15，开启 KVM/vhost/overlayfs/cgroup v2 | 建议 6.1+ |
| CPU | 需 `vmx`/`svm`；云上选 metal 实例或开嵌套虚拟化 | 见 [06](06-isolation-runtime.md) |

> 版本组合须固化为「支持矩阵」并在 CI 中验证，见 [06-isolation-runtime.md](06-isolation-runtime.md)。

## D4 · 池化机制

| 方案 | 认领语义 | 冷启动优化 | 状态管理 | 结论 |
|---|---|---|---|---|
| `Deployment` 常驻 + `Service` | ❌ 共享，无法 1:1 绑定 | ⚠️ 部分 | ❌ | 否 |
| 预创建裸 Pod + Label 选举 | ⚠️ 可实现但易错 | ✅ | ❌ | 否 |
| **自研 Pool + Claim（CAS 认领）** | ✅ 原子排他 | ✅ | ✅ | ✅ |
| Knative Serving | ⚠️ 面向 HTTP | ✅（Activator 思路可借鉴） | ❌ | 否 |

**结论**：自研 `SandboxPool` + **CAS 认领协议**（详见 [05](05-warm-pool-and-scaling.md)）。核心是：

- 池中库存以**未认领的 `AgentSandbox` 对象**表示（而非裸 Pod），这样库存本身也有状态机与 TTL，避免"僵尸库存"。
- 认领 = 带 `resourceVersion` 前置条件的 Patch，天然分布式安全，无需额外协调服务。
- 借鉴 Knative 的三点：队列深度驱动扩容、`Activator` 式兜底（池空时的快速失败/排队）、就绪探针与端点解耦。

## D5 · 扩缩驱动

| 方案 | 表达"维持 N 个未认领库存" | 响应时延 | 抖动控制 | 结论 |
|---|---|---|---|---|
| HPA（CPU/内存） | ❌ | 慢（指标管道 15–60s） | 默认 scaleDown 稳定窗口 5min | 否 |
| HPA（自定义/外部指标） | 勉强（需自定义指标）= 库存数 | 慢 | 同上 | 否 |
| **自研 Pool Controller** | ✅ 原生 | 事件驱动 + `RequeueAfter`，亚秒 | 自研阻尼/冷却 | ✅ |
| KEDA | ⚠️ 需暴露 `/scale` 子资源；面向事件源 | 中 | 中 | 可选叠加 |
| 集群自动扩缩（CA / Karpenter） | ❌（管节点，不管池） | 分钟级 | — | **协作者**，非替代 |

**结论**：

- **池水位由自研控制器控制**（目标水位、库存缺口、需求预测）。
- 给 `SandboxPool` 加 **`/scale` 子资源**（`spec.warmReplicas`），使 KEDA/HPA 可在特定场景（如基于外部队列长度）**叠加**驱动；默认关闭。
- **节点侧**交给 Karpenter/CA：池应在创建库存前先确保节点容量（`NodePool` 预留 + `do-not-disrupt` 与 drain 协同）。注意 **Pool 的 scale-to-zero 与节点 scale-to-zero 会互相打架**——必须显式配置节点最小容量，否则"预热池永远预热不了"。

## D6 · 资源推荐

| 方案 | 粒度 | 与池化冲突 | 结论 |
|---|---|---|---|
| 静态人工规格 | 粗 | 无 | 起步可用 |
| VPA (`Auto`/`Recreate`) | 逐 Pod | ⚠️ 会重启 Pod，破坏沙箱与预热池 | ❌ |
| VPA (`Off` + recommendation) | 逐控制器 | 无（只出建议） | ✅ 采集器 |
| **画像分档（tiny/small/medium/large）** | 按模板 | 无 | ✅ 落地方式 |
| 逐沙箱精调 requests | 极细 | ✅ 会制造调度碎片、降低装箱率 | ❌ |
| 原地调整（`InPlacePodVerticalScaling`，1.33 beta+） | 逐 Pod | 低（不重启） | ✅ 可选增强 |

**结论**：用 VPA 的 **recommendation-only** 数据作为输入，但**输出是"档位"而非逐沙箱值**：控制器定期计算每个 `SandboxTemplate` 的 P95 用量，映射到最近的档位，写入模板。既避免碎片，又保留自适应能力。原地调整仅用于长驻沙箱（S3/S4），且需验证 Kata 对 in-place resize 的支持（**QEMU/CH 支持较好，Firecracker 需实测**——列为开放问题）。

## D7 · 休眠与恢复

| 方案 | 释放 CPU | 释放内存 | 恢复时延 | 可用性 | 结论 |
|---|---|---|---|---|---|
| **L1 cgroup freeze（冻结）** | ✅ | ❌ | 毫秒 | 立即可用 | ✅ 默认 |
| L2 内存气球回收 | — | ⚠️ | — | Firecracker 有 balloon 设备但 Kata 未经 K8s API 暴露 | ❌ 短期不可行 |
| L3 应用状态外置 + 销毁重建 | ✅ | ✅ | 0.5–2.5 s | 立即可用、最可靠 | ✅ **默认主策略** |
| L4 CRIU 检查点（kubelet `ContainerCheckpoint`） | ✅ | ✅ | 秒级 | 仅 runc；Kata 不适用；特性长期未 GA | ⚠️ 仅 runc 场景 |
| L5 Firecracker 快照/恢复 | ✅ | ✅ | 100–300 ms | 需**自研 shim** 绕过 Kata 的 VM 管理层 | ⚠️ M4 PoC |

**关键洞察（反直觉但重要）**：Kata/Firecracker 路径下**来宾内存由 VMM 独占，宿主无法回收**，因此"休眠省内存"最可靠的实现不是快照，而是**把沙箱状态外置**（工作区增量同步到对象存储/PVC、会话状态写入外部 KV），直接销毁沙箱、唤醒时重建 + 拉取状态。这把一个**分布式系统难题**（VM 快照与迁移）转化为一个**幂等重建问题**，工程成本低一个数量级。

**结论**：
- 默认：`L1 冻结`（短空闲，秒～分钟）+ `L3 状态外置重建`（长空闲，分钟～小时）。
- PoC 可选：`L5 Firecracker 快照`，仅当业务能接受自研 runtime shim 的复杂度且恢复时延指标要求 < 300ms 时启动。

## D8 · 调度策略

| 方案 | 装箱效果 | 复杂度 | 结论 |
|---|---|---|---|
| 默认调度器（LeastAllocated） | 差（打散） | 0 | ❌ 作为沙箱池默认 |
| **默认调度器 + 第二 profile（`NodeResourcesFit` = `MostAllocated`）** | 好 | 低 | ✅ |
| scheduler-plugins（binpack） | 好 | 中（额外部署） | 备选 |
| Volcano / YuniKorn | 好（含 gang） | 高 | ❌ 无 gang 需求 |
| 自研调度器 | 可控 | 极高 | ❌ |

**结论**：`KubeSchedulerConfiguration` 配置**多 profile**，为沙箱引入独立 `schedulerName`：

- `default-scheduler`：系统组件、Kata 节点守护进程。
- `sandbox-binpack`：`scoringStrategy.type: MostAllocated`，配合 `nodeAffinity`（按 RuntimeClass 分池）、`PodTopologySpread`（限制单节点单租户集中度）、`PriorityClass` 抢占。

配套：`Descheduler`（或自研 defrag 控制器）在低峰做碎片整理；`PodDisruptionBudget` 保护预热池不被并发驱逐清空。

## D9 · 网络

| 方案 | FQDN 出口策略 | per-pod 策略 | 性能/可观测 | 结论 |
|---|---|---|---|---|
| Calico | ✅（需 DNS 组件） | ✅ | iptables/eBPF | 备选 |
| **Cilium** | ✅ `toFQDNs`（内置 DNS proxy） | ✅ `CiliumNetworkPolicy` | eBPF，Hubble 可观测 | ✅ |
| 无 CNI 策略（默认） | ❌ | ❌ | — | ❌ 不可接受 |

**结论**：Cilium，理由：Agent 沙箱的**主要风险是数据外泄**，`toFQDNs` 出口白名单是刚需（允许 LLM 网关域名、包仓库、对象存储，其余全部拒绝）；Hubble 提供的连接级可观测性直接支撑 FR-14 审计。同时必须显式**拒绝访问 API Server（`kubernetes.default.svc`）**。

> Kata 的网络栈：每个 Pod 有独立 netns，CNI 策略照常生效，无需特殊处理。

## D10 · 存储

| 用途 | 方案 | 理由 |
|---|---|---|
| 根文件系统 | **只读镜像层**（overlay）+ 可写 `emptyDir` | 沙箱不可持久化污染镜像 |
| 每沙箱临时盘 | **generic ephemeral volume（CSI）** + `sizeLimit` | 走 PVC 生命周期，避免 `emptyDir` 撑爆节点 ephemeral storage 触发驱逐 |
| 大工作区（S3 场景） | 对象存储 + 本地缓存卷 | 大卷挂载慢，对象存储更快；且天然支持"状态外置" |
| 依赖/模型缓存 | 节点本地 hostPath 缓存（只读挂载） | 避免每次拉取，降低冷启动 |
| 状态持久化 | PVC 或对象存储 | 支撑 L3 重建策略 |

**加速手段**：
- **懒加载镜像**（Nydus / Stargz snapshotter）——冷路径镜像拉取是主要延迟来源之一，可显著压缩；但需验证与 Kata 的兼容性（见 [06](06-isolation-runtime.md)）。
- 节点级预热 DaemonSet：提前把常用镜像 `crictl pull` 到节点。

> 注意：K8s 1.31 起有 alpha 的 `ImageVolume`（镜像直接作为只读卷，零拷贝），可作为未来优化项。

## D11 · 多环境策略（本地 + 生产）

| 环境 | 隔离模式 | 能验证什么 | 不能验证什么 |
|---|---|---|---|
| **kind**（Docker 节点） | `simulated`（走 runc，仅逻辑） | 状态机、认领、TTL、回收、指标、Webhook | Kata 真实隔离、KVM 性能 |
| **minikube** | `simulated` / `runc` | 同上 + 多节点调度拓扑 | 同上 |
| **单节点 kubeadm on KVM 主机** | `kata-fc` | 隔离、启动延迟、`/dev/kvm` 链路 | 大规模调度、多节点故障 |
| **生产集群（metal/嵌套虚拟化）** | `kata-fc` 主力 + `runc` 低敏感 | 全部 | — |

**结论——引入「隔离抽象层」**：

```yaml
# 配置项（ConfigMap）
isolation:
  level: simulated | runc | kata-fc | kata-clh
  runtimeClassName:
    runc: ""
    kata-fc: kata-fc
  # 业务代码与 CRD 完全不变，只有这一处映射随环境切换
```

设计收益：`AgentSandbox` 的 `status.phase`、认领协议、资源优化逻辑**在 kind 上 100% 可测**；Kata 相关验证集中在「运行时一致性测试套件」中，在 KVM 环境按里程碑跑。

---

## 被否决的方案及原因

| # | 方案 | 否决原因 |
|---|---|---|
| V1 | 直接用 `Job` + `activeDeadlineSeconds` 做沙箱 | 只能表达"超时终止"，无法表达空闲检测、认领、休眠、池化；且 Job 完成后的清理是异步的，难以做严格泄漏防护 |
| V2 | `Deployment` + `Service` 承载沙箱 | 共享语义与"会话 1:1 独占沙箱"根本冲突；无法保证请求落到空闲实例；状态机无处安放 |
| V3 | HPA 驱动池大小 | HPA 面向"按负载伸缩已有服务"，本项目需要"维持未认领库存"；且默认 5min scaleDown 稳定窗口与秒级需求不匹配；冷启动本身会污染 HPA 的指标闭环（扩容→启动→负载下降→缩容→抖动） |
| V4 | gVisor 作为主隔离 | ① 隔离强度弱于 VM（用户态内核仍有共享的攻击面）；② syscall 兼容性缺口对"任意代码执行"场景风险不可控；③ 无强隔离的合规背书，多租户场景难以通过安全评审。*注：不作为主方案，但可作为"中等隔离"档位在低敏感租户使用。* |
| V5 | 自研 microVM 编排（不用 K8s） | 放弃全部生态（CNI/CSI/可观测/调度），运维成本超过收益 |
| V6 | Nomad / Swarm | 组织已有 K8s 能力；双层编排复杂度高 |
| V7 | Docker-in-Docker / 特权容器做沙箱 | 安全性不可接受（共享内核 + 特权 = 节点失守） |
| V8 | nsjail / bubblewrap / seccomp 进程级沙箱 | 面向单一进程的轻量隔离，无法承载完整 Agent 运行时（浏览器/多进程/包管理） |
| V9 | Knative Serving 直接作为沙箱平台 | 面向 HTTP 请求-响应无状态函数；有状态会话、长驻、强隔离均不匹配。**但其 Activator/queue-proxy/冷启动思路被借鉴** |
| V10 | Kata + QEMU 作为主运行时 | 每实例 QEMU 进程开销与启动延迟显著高于 Firecracker，与 S1 高频场景冲突。保留为 `kata-clh` 之外的兼容回退 |
| V11 | 逐沙箱独立 `requests` 精调 | 制造调度碎片（碎片率上升），降低装箱率；正确做法是画像分档 |
| V12 | 用 `emptyDir` 承载沙箱工作区 | 占用节点 ephemeral storage，易触发 `DiskPressure` 驱逐，且无配额上限控制语义 |
| V13 | 引入服务网格（Istio）做出口治理 | 为沙箱注入 sidecar 会破坏 Firecracker 轻量优势并增加启动延迟；出口治理用 CNI 层（Cilium）更合适 |
| V14 | 自研调度器 | 需重实现抢占/亲和/拓扑等全套语义，收益不匹配成本 |

---

## 选型风险与待验证清单

| 选型 | 待验证项 | 验证方法 | 阻塞里程碑 |
|---|---|---|---|
| Kata + Firecracker | 启动延迟是否满足 P95 < 2.5s；`virtiofsd` 共享卷性能 | KVM 环境基准测试（详见 [10](10-roadmap-risks.md)） | M2 |
| Kata + FC | 是否支持 in-place pod resize | 特性矩阵测试 | M4 |
| Kata + 懒加载镜像 | Nydus/Stargz 与 Kata snapshotter 兼容性 | 集成测试 | M2 |
| Cilium + Kata | FQDN 策略在 FC netns 下生效 | 连通性 + 泄漏测试 | M2 |
| Cilium | eBPF 与 cgroup v2 下的策略规模（5k Pod） | 压测 | M3 |
| 池化 | 认领冲突率与 API Server 写入放大 | 压测（200/s 创建） | M1 |
| 快照（L5） | Firecracker 快照与 Kata 结合的可行性 | PoC spike（限时 2 周） | M4 |

---

[← 上一篇：需求与目标](01-requirements.md) | [返回导航](../README.md) | [下一篇：总体架构 →](03-architecture.md)
