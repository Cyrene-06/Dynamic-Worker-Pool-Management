# M2 E2 节点部署与 node-agent

本节记录 M2 前三项交付物的仓库入口。清单和代码已提交；当前仓库没有 E2 KVM
宿主的执行记录，因此不能把“可部署”写成“已在真实 Kata 节点通过”。

## 1. E2 宿主

使用带 `/dev/kvm`、cgroup v2、`vhost_vsock` 的 Ubuntu Linux 节点。节点上先安装
`containerd`、`kubeadm`、`kubectl`、`helm`，并从本仓库执行：

```bash
export CILIUM_VERSION=<审定的 Helm chart 版本>
export KATA_VERSION=<审定的 kata-deploy chart 版本>
sudo -E bash hack/e2/bootstrap.sh
```

脚本运行 `hack/bootstrap/node-init.sh`，初始化单节点 kubeadm 集群，安装 Cilium 与
kata-deploy，再安装 CRD、命名空间、RuntimeClass、出口档位配置和 node-agent。镜像 tag 默认仍为
`dev`；在真实 E2 节点发布前，先通过 `make docker-buildx` 推送 node-agent 镜像，并将
`config/node-agent/daemonset.yaml` 中的镜像替换成不可变 digest。

`node-init.sh` 默认不预留大页，不格式化磁盘。需要大页时显式设
`HUGEPAGES_2M_COUNT`；需要挂载已格式化的数据盘时显式设 `SANDBOX_DATA_DEVICE`。
这些值必须来自节点规格与基准测试，不能沿用演示设备名。

## 2. RuntimeClass

`config/runtimeclass/` 提供 `kata-fc`、`kata-clh` 和版本化的
`kata-fc-3-4-0`。`overhead` 与 `config/isolation/levels.yaml` 一致：FC 为
`300m/192Mi`，CLH 为 `300m/256Mi`。调度约束要求节点隔离标签和 `sandbox=true`
污点容忍。升级时先在新节点上设置 `sandbox.example.com/runtime-version`，然后将
目标 `SandboxPool.spec.runtimeClassName` 切到版本化名称；旧库存排空后再撤旧类。

## 3. node-agent

`cmd/node-agent` 作为 DaemonSet 在 Kata 节点运行，每轮：

1. 从 `SandboxTemplate` 读取镜像，每轮最多通过节点 CRI `ImageStatus` /
   `PullImage` 预热一个镜像，避免拉取阻塞休眠控制；
2. 从宿主 `/proc` 采集 Firecracker、virtiofsd、Cloud Hypervisor RSS；连续两轮
   发现没有对应 Pod 的 VMM 进程时记录告警，不自动杀进程；
3. 观察本节点沙箱的 `Hibernating` / `Resuming` 状态，只对其 Pod UID 对应的
   cgroup v2 写入 `cgroup.freeze`，确认 `cgroup.events` 后更新休眠子状态。

节点级指标在 `/metrics`：`sandbox_vmm_rss_bytes` 和
`sandbox_orphan_vmm_processes`。`/healthz` 供 DaemonSet 探针使用。
Agent 为访问 CRI socket 和写宿主 cgroup 以 root 身份运行，RBAC 将对象写入权限
限制在 `sandbox-pool` 的 `agentsandboxes/status`，不授予 Pod exec、Secret 或节点修改权限。

镜像预热目前针对模板公开镜像；私有仓库认证需要后续接入节点 CRI 的认证配置。
所有节点会预热所有模板镜像，模板规模增大时需要加节点池筛选与拉取预算。

## 4. 出口策略与一致性验证

平台在 `config/egress/profiles.yaml` 策展允许的域名和端口。Kata 沙箱创建 Pod
之前，控制器按模板的 `egressProfile` 建立单沙箱 CiliumNetworkPolicy：DNS 只解析
档位域名，HTTPS 只允许 FQDN 白名单，入站只允许 `sandbox-gateway`，出站显式拒绝
API Server 与元数据 IP。策略缺失或档位无效时拒绝创建 Kata Pod。

`hack/conformance/run.py` 在 E2 节点执行 T1–T9；
`hack/conformance/report.py` 从原始 JSON 生成分阶段启动延迟、密度曲线和候选
`maxSandboxesPerNode`；`hack/e2/kata_upgrade.py` 记录版本化 RuntimeClass 切换和
强制回滚。采集命令与尚未采集的状态见[基线记录](../reports/m2-e2-baseline.md)。
