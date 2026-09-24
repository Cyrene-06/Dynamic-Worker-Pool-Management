#!/usr/bin/env bash
#
# hack/bootstrap/node-init.sh —— Kata 节点的初始化与自检。
#
# 放进节点镜像或由 cloud-init 执行。设计原则：**先在启动时失败，别在运行时失败**。
#
# 这个脚本里最重要的部分不是 sysctl，而是 KVM/vsock 的硬校验。
# 因为缺 /dev/kvm 或 vhost_vsock 时，kata-deploy 依然会"安装成功"，
# Pod 却会卡在 ContainerCreating（vsock 缺失时更隐蔽：Pod 能起来，
# 但 kubectl exec / logs 全部失败）—— 这类故障的排查成本远高于启动即失败。
set -euo pipefail

log() { echo "[node-init] $*"; }
fail() { echo "[node-init][FATAL] $*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# 1. 内核参数
# ---------------------------------------------------------------------------
log "配置内核参数"
cat >/etc/sysctl.d/99-sandbox.conf <<'EOF'
# Kata 与内存超卖的前提是禁用 swap（docs/06 §4）
vm.swappiness=0
# 允许内存超卖，配合 cgroup v2 的内存控制
vm.overcommit_memory=1
# 高密度容器场景的常见耗尽项
fs.inotify.max_user_instances=8192
fs.inotify.max_user_watches=524288
fs.file-max=2097152
vm.max_map_count=262144
net.core.somaxconn=32768
EOF
sysctl --system >/dev/null

# ---------------------------------------------------------------------------
# 2. 虚拟化能力硬校验（失败即拒绝成为沙箱节点）
# ---------------------------------------------------------------------------
log "校验虚拟化能力"

if ! grep -qE 'vmx|svm' /proc/cpuinfo; then
    fail "CPU 无虚拟化扩展（vmx/svm）。该节点不能运行 Kata：换机器，或把节点标签改为 runc 池。"
fi

[ -e /dev/kvm ] || fail "/dev/kvm 不存在。裸金属请检查 BIOS 虚拟化；云 VM 请确认实例支持嵌套虚拟化。"
[ -r /dev/kvm ] && [ -w /dev/kvm ] || fail "/dev/kvm 权限不足，containerd 无法使用。"

# 嵌套虚拟化：裸金属上该参数无意义，云 VM 上必须开启
for f in /sys/module/kvm_intel/parameters/nested /sys/module/kvm_amd/parameters/nested; do
    if [ -e "$f" ] && systemd-detect-virt --vm --quiet; then
        case "$(cat "$f")" in
            1|Y|y) log "嵌套虚拟化已开启 ($f)" ;;
            *) fail "嵌套虚拟化未开启 ($f)。云 VM 需显式启用，否则 Firecracker 无法启动。" ;;
        esac
    fi
done

# vhost_vsock 缺失时的表现很隐蔽：Pod 正常 Running，但 kubectl exec/logs 全失败。
# 因此必须在节点启动阶段就拦住。
modprobe vhost_vsock || fail "加载 vhost_vsock 失败"
modprobe vhost_net   || fail "加载 vhost_net 失败"
[ -e /dev/vhost-vsock ] || fail "/dev/vhost-vsock 不存在，kubectl exec/logs 会失败。"

# cgroup v2：Firecracker 与资源核算都依赖它
if [ "$(stat -fc %T /sys/fs/cgroup)" != "cgroup2fs" ]; then
    fail "cgroup 不是 v2。请在内核启动参数加 systemd.unified_cgroup_hierarchy=1 并重启。"
fi

KVER="$(uname -r)"
log "内核 $KVER（Kata 3.3+ 建议 5.15+，推荐 6.1+；低于建议值时启动延迟与稳定性需实测确认）"

# ---------------------------------------------------------------------------
# 3. CPU 频率稳定性
#
# 实测中这一步能把 microVM 启动延迟缩短 20–40%，并显著降低 P99 抖动 ——
# 因为节能状态切换会放大启动阶段的时序抖动（docs/06 §4）。
# ---------------------------------------------------------------------------
log "固定 CPU 频率策略"
for gov in /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor; do
    [ -e "$gov" ] && echo performance > "$gov" 2>/dev/null || true
done

# ---------------------------------------------------------------------------
# 4. 大页预留
#
# 注意这是一个**显式取舍**（docs/07 §4.2）：
#   大页 → 更低启动延迟、更少 TLB miss，但内存被预分配常驻，失去按需分配带来的
#          "隐性超卖"收益。
# 因此短生命周期池与长驻池应该用两套节点规格/配置，而不是全局统一。
# ---------------------------------------------------------------------------
HUGEPAGES_2M_COUNT="${HUGEPAGES_2M_COUNT:-0}"
if [ "$HUGEPAGES_2M_COUNT" -gt 0 ]; then
    log "预留 ${HUGEPAGES_2M_COUNT} × 2MiB 大页"
    echo "$HUGEPAGES_2M_COUNT" > /proc/sys/vm/nr_hugepages
    printf 'vm.nr_hugepages=%s\n' "$HUGEPAGES_2M_COUNT" >/etc/sysctl.d/98-sandbox-hugepages.conf
else
    log "未预留大页；待 M2 prealloc/hugepages 基准测试后设定"
fi

# ---------------------------------------------------------------------------
# 5. 本地盘（镜像层与沙箱临时盘）
# ---------------------------------------------------------------------------
log "准备本地盘挂载点"
mkdir -p /var/lib/sandbox /var/lib/sandbox/scratch
# 数据盘需要显式指定并事先格式化。绝不按猜测的设备名自动 mkfs。
if [ -n "${SANDBOX_DATA_DEVICE:-}" ]; then
    [ -b "$SANDBOX_DATA_DEVICE" ] || fail "SANDBOX_DATA_DEVICE 不是块设备"
    blkid "$SANDBOX_DATA_DEVICE" >/dev/null 2>&1 || fail "数据盘未格式化；请人工确认并格式化"
    grep -q ' /var/lib/sandbox ' /etc/fstab || \
        echo "$SANDBOX_DATA_DEVICE /var/lib/sandbox ext4 defaults,noatime,nodiratime 0 2" >>/etc/fstab
    mount -a
fi

# ---------------------------------------------------------------------------
# 6. containerd 配置（内容见 docs/06 §5.1）
# ---------------------------------------------------------------------------
if [ -f /etc/containerd/sandbox.toml ]; then
    log "应用 containerd 沙箱运行时配置"
    mkdir -p /etc/containerd/conf.d
    install -m 0644 /etc/containerd/sandbox.toml /etc/containerd/conf.d/99-sandbox.toml
    systemctl restart containerd
else
    log "未提供 /etc/containerd/sandbox.toml，跳过 containerd 配置（由节点镜像负责）"
fi

# ---------------------------------------------------------------------------
# 7. 自检汇总
# ---------------------------------------------------------------------------
log "节点自检通过："
log "  /dev/kvm         : ok"
log "  /dev/vhost-vsock : ok"
log "  cgroup           : v2"
log "  hugepages        : $(grep HugePages_Total /proc/meminfo | awk '{print $2}') × 2MiB"
log "下一步：确保节点带有标签 sandbox.example.com/isolation=kata-fc，"
log "        并由 kata-deploy DaemonSet（nodeSelector 限定到该标签）安装运行时。"
