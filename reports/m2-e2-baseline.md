# M2 E2 基线记录

状态：**尚未采集**（2026-09-24）。当前工作区没有可访问的 KVM Linux 节点，
因此冷路径 P95、密度曲线、`maxSandboxesPerNode` 和 Kata 升级结果都不能填写实测值。

仓库已提供 `hack/conformance/run.py` 与 `hack/conformance/report.py`。在 E2 节点执行后，
将原始 `results.json`、`density.csv`、`density-latency.svg`、生成的 `baseline.md`
和 `kata-upgrade.json` 一并保存；只有这些证据齐全后才更新 M2 退出标准。

## 采集命令

```bash
python3 hack/conformance/run.py \
  --node <KVM节点名> --output reports/e2-<日期> \
  --runtime kata-fc --densities 1,5,10,20,40 \
  --lazy-image <已配置懒加载snapshotter的镜像> \
  --lazy-snapshotter <nydus或stargz> \
  --csi-claim <测试PVC> \
  --policy-pod <由Operator创建且装有curl的Kata沙箱Pod> \
  --allowed-url https://<白名单域名> \
  --blocked-url https://<非白名单域名> \
  --gateway-pod <sandbox-system内带app=sandbox-gateway且有curl的探针Pod> \
  --outside-pod <sandbox-system内无gateway标签且有curl的探针Pod> \
  --policy-port <沙箱Pod监听端口> \
  --oom-image <含python3的镜像> \
  --exercise-eviction \
  --upgrade-runtime kata-fc-3-4-0

python3 hack/conformance/report.py reports/e2-<日期>/results.json \
  --output reports/e2-<日期> --ipam-available <实测Cilium可用Pod地址数>

python3 hack/e2/kata_upgrade.py \
  --pool <kata-fc池名> --new-runtime kata-fc-3-4-0 \
  --observe-seconds 86400 --output reports/e2-<日期>
```

T8 首次执行保存重启前快照；节点重启后以相同 `--output` 再执行 T8。
T6 的 OOM 子项自动执行；`--exercise-eviction` 在专用可丢弃节点额外创建
16Mi 临时盘限额的压力 Pod，验证 kubelet 驱逐。未启用时 T6 标为 `PARTIAL`。
T9 在一致性套件里验证新旧 RuntimeClass
都可启动和回退；完整池流量切换与回滚由 `kata_upgrade.py` 记录。
运行升级脚本前，新旧 Kata 节点及对应 RuntimeClass 必须同时存在，新节点须有
`sandbox.example.com/runtime-version` 标签。脚本只切换指定池，先排空库存，
再恢复旧类；正在服务的旧沙箱按原运行时自然结束。
