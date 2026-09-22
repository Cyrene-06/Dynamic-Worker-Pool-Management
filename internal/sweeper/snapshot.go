package sweeper

import (
	"context"
	"fmt"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
)

// objectIndex 把 Finding 定位回具体对象。
//
// 存在的理由是"分类"与"执行"必须分离：分类要能脱离集群被测试，
// 因此它只处理值类型的 view；而执行要对真实对象动手。
// 用 key（namespace/name）把它们对上，而不是让 Classify 返回对象引用 ——
// 后者会让纯函数测试不得不构造完整的 client.Object。
type objectIndex struct {
	pods      map[string]client.Object
	leases    map[string]client.Object
	netpols   map[string]client.Object
	volumes   map[string]client.Object
	sandboxes map[string]*sandboxv1alpha1.AgentSandbox
}

func key(namespace, name string) string { return namespace + "/" + name }

// snapshot 采集一轮对账所需的全部事实。
//
// # 每类对象都用**正向标签**筛选，这是刻意的
//
// 对账运行在一个共享集群里。用"名字看起来不像沙箱"来推断孤儿，
// 迟早会在别人命名空间里删掉不属于它的东西。所以每类查询都要求
// 对象带有本平台写入的角色/归属标签 —— 这是正向证据。
//
// 代价是：一个被人工剥掉标签的孤儿对象对账看不见它。
// 这个代价是被接受的 —— 看不见只是继续泄漏（可被后续轮次或被人的
// 资源配额告警发现），而误删是不可逆的。
func (s *Sweeper) snapshot(ctx context.Context, now time.Time) (Snapshot, *objectIndex, error) {
	idx := &objectIndex{
		pods:      map[string]client.Object{},
		leases:    map[string]client.Object{},
		netpols:   map[string]client.Object{},
		volumes:   map[string]client.Object{},
		sandboxes: map[string]*sandboxv1alpha1.AgentSandbox{},
	}
	snap := Snapshot{Now: now, NodeReady: map[string]bool{}}

	// ---- 沙箱 ----
	var sbxList sandboxv1alpha1.AgentSandboxList
	if err := s.Client.List(ctx, &sbxList); err != nil {
		return snap, idx, fmt.Errorf("列出沙箱失败: %w", err)
	}
	for i := range sbxList.Items {
		sbx := &sbxList.Items[i]
		idx.sandboxes[key(sbx.Namespace, sbx.Name)] = sbx
		v := sandboxView{
			Namespace: sbx.Namespace,
			Name:      sbx.Name,
			UID:       string(sbx.UID),
			Phase:     string(sbx.Status.Phase),
			CreatedAt: sbx.CreationTimestamp.Time,
			PodName:   sbx.Status.PodName,
			NodeName:  sbx.Status.NodeName,
			Deleting:  !sbx.DeletionTimestamp.IsZero(),
			// status.nodeName 为空时回退到 Pod 的调度结果由控制器写入，
			// 对账不额外去读 Pod —— 它已经拿到 Pod 列表了，见下面 d。
		}
		if !sbx.DeletionTimestamp.IsZero() {
			v.DeletingSince = sbx.DeletionTimestamp.Time
		}
		if sbx.Spec.Claim != nil {
			v.HasClaim = true
			v.ClaimedTenant = sbx.Spec.Claim.RequestedBy.Tenant
		}
		v.RecycleReason = string(sbx.Status.Metrics.RecycleReason)
		snap.Sandboxes = append(snap.Sandboxes, v)
	}

	// ---- Pod ----
	var podList corev1.PodList
	if err := s.Client.List(ctx, &podList,
		client.MatchingLabels{sandboxv1alpha1.LabelRole: sandboxv1alpha1.RoleSandbox},
	); err != nil {
		return snap, idx, fmt.Errorf("列出沙箱 Pod 失败: %w", err)
	}
	for i := range podList.Items {
		pod := &podList.Items[i]
		idx.pods[key(pod.Namespace, pod.Name)] = pod
		snap.Pods = append(snap.Pods, podView{
			Namespace:    pod.Namespace,
			Name:         pod.Name,
			UID:          string(pod.UID),
			OwnerUID:     controllerOwnerUID(pod.OwnerReferences),
			Role:         pod.Labels[sandboxv1alpha1.LabelRole],
			CreatedAt:    pod.CreationTimestamp.Time,
			Phase:        string(pod.Status.Phase),
			SandboxLabel: pod.Labels[sandboxv1alpha1.LabelSandbox],
			BlockedSince: blockedSince(pod.Annotations),
		})
	}

	// ---- Lease ----
	// 用"带 LabelSandbox"筛选：Lease 是最容易被误伤的一类对象，
	// 因为集群里到处都是 Lease（leader election、kubelet 心跳、其他 Operator）。
	var leaseList coordinationv1.LeaseList
	if err := s.Client.List(ctx, &leaseList,
		client.HasLabels{sandboxv1alpha1.LabelSandbox},
	); err != nil {
		return snap, idx, fmt.Errorf("列出 Lease 失败: %w", err)
	}
	for i := range leaseList.Items {
		l := &leaseList.Items[i]
		idx.leases[key(l.Namespace, l.Name)] = l
		snap.Leases = append(snap.Leases, namedView{
			Namespace:    l.Namespace,
			Name:         l.Name,
			UID:          string(l.UID),
			OwnerUID:     controllerOwnerUID(l.OwnerReferences),
			Role:         "lease",
			CreatedAt:    l.CreationTimestamp.Time,
			SandboxLabel: l.Labels[sandboxv1alpha1.LabelSandbox],
			BlockedSince: blockedSince(l.Annotations),
		})
	}

	// ---- NetworkPolicy ----
	var npList networkingv1.NetworkPolicyList
	if err := s.Client.List(ctx, &npList,
		client.MatchingLabels{sandboxv1alpha1.LabelRole: sandboxv1alpha1.RoleNetpol},
	); err != nil {
		return snap, idx, fmt.Errorf("列出网络策略失败: %w", err)
	}
	for i := range npList.Items {
		np := &npList.Items[i]
		idx.netpols[key(np.Namespace, np.Name)] = np
		snap.Netpols = append(snap.Netpols, namedView{
			Namespace:    np.Namespace,
			Name:         np.Name,
			UID:          string(np.UID),
			OwnerUID:     controllerOwnerUID(np.OwnerReferences),
			Role:         sandboxv1alpha1.RoleNetpol,
			CreatedAt:    np.CreationTimestamp.Time,
			SandboxLabel: np.Labels[sandboxv1alpha1.LabelSandbox],
			BlockedSince: blockedSince(np.Annotations),
		})
	}

	// ---- PVC ----
	var pvcList corev1.PersistentVolumeClaimList
	if err := s.Client.List(ctx, &pvcList,
		client.MatchingLabels{sandboxv1alpha1.LabelRole: sandboxv1alpha1.RoleDataVolume},
	); err != nil {
		return snap, idx, fmt.Errorf("列出数据卷失败: %w", err)
	}
	for i := range pvcList.Items {
		pvc := &pvcList.Items[i]
		idx.volumes[key(pvc.Namespace, pvc.Name)] = pvc
		snap.Volumes = append(snap.Volumes, namedView{
			Namespace:    pvc.Namespace,
			Name:         pvc.Name,
			UID:          string(pvc.UID),
			OwnerUID:     controllerOwnerUID(pvc.OwnerReferences),
			Role:         sandboxv1alpha1.RoleDataVolume,
			CreatedAt:    pvc.CreationTimestamp.Time,
			SandboxLabel: pvc.Labels[sandboxv1alpha1.LabelSandbox],
			BlockedSince: blockedSince(pvc.Annotations),
		})
	}

	// ---- 节点 ----
	var nodeList corev1.NodeList
	if err := s.Client.List(ctx, &nodeList); err != nil {
		return snap, idx, fmt.Errorf("列出节点失败: %w", err)
	}
	for i := range nodeList.Items {
		snap.NodeReady[nodeList.Items[i].Name] = nodeReady(&nodeList.Items[i])
	}

	return snap, idx, nil
}

// controllerOwnerUID 取 controller ownerReference 的 UID。
func controllerOwnerUID(refs []metav1.OwnerReference) string {
	for _, r := range refs {
		if r.Controller != nil && *r.Controller {
			return string(r.UID)
		}
	}
	return ""
}

// blockedSince 读取"首次被判为异常"的时间标记。
func blockedSince(annos map[string]string) time.Time {
	raw, ok := annos[sandboxv1alpha1.AnnoBlockedSince]
	if !ok {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		// 解析失败当作"未标记"：这一轮会重新打上正确的标记，
		// 因此宽限期从头开始算。比解析失败就当作"早就标记过"安全得多 ——
		// 后者会立刻删除，而删除的依据是一个我们读不懂的值。
		return time.Time{}
	}
	return t
}

// nodeReady 判断节点是否 Ready。
func nodeReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
