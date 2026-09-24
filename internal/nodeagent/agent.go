package nodeagent

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var podUID = regexp.MustCompile(`pod([0-9a-f]{8}[-_][0-9a-f_-]{20,})`)

// Agent performs only node-local work. It never kills a process that appears orphaned.
type Agent struct {
	Client                                    client.Client
	NodeName, Namespace, ProcRoot, CgroupRoot string
	PullImage                                 func(context.Context, string) error
	Log                                       *slog.Logger
	Interval                                  time.Duration
	seenOrphans                               map[int]bool
	prewarmed                                 map[string]time.Time
	metricsMu                                 sync.RWMutex
	vmmRSS                                    map[string]uint64
	orphanCount                               int
}

func (a *Agent) Run(ctx context.Context) error {
	if a.NodeName == "" || a.Namespace == "" || a.PullImage == nil {
		return fmt.Errorf("node name, namespace and image puller are required")
	}
	if a.ProcRoot == "" {
		a.ProcRoot = "/host/proc"
	}
	if a.CgroupRoot == "" {
		a.CgroupRoot = "/host/sys/fs/cgroup"
	}
	if a.Interval <= 0 {
		a.Interval = 30 * time.Second
	}
	if a.seenOrphans == nil {
		a.seenOrphans = make(map[int]bool)
	}
	if a.prewarmed == nil {
		a.prewarmed = make(map[string]time.Time)
	}
	if a.Log == nil {
		a.Log = slog.Default()
	}
	for {
		if err := a.Reconcile(ctx); err != nil {
			a.Log.Error("node reconciliation failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(a.Interval):
		}
	}
}

func (a *Agent) Reconcile(ctx context.Context) error {
	var pods corev1.PodList
	if err := a.Client.List(ctx, &pods, client.MatchingFields{"spec.nodeName": a.NodeName}); err != nil {
		return fmt.Errorf("list node pods: %w", err)
	}
	active := make(map[string]bool, len(pods.Items))
	for i := range pods.Items {
		active[string(pods.Items[i].UID)] = true
	}
	if err := a.observeVMM(active); err != nil {
		a.Log.Error("VMM inspection failed", "error", err)
	}

	var sandboxes sandboxv1alpha1.AgentSandboxList
	if err := a.Client.List(ctx, &sandboxes, client.InNamespace(a.Namespace)); err != nil {
		return fmt.Errorf("list sandboxes: %w", err)
	}
	for i := range sandboxes.Items {
		s := &sandboxes.Items[i]
		if s.Status.NodeName != a.NodeName || s.Status.PodName == "" || !activePod(pods.Items, s.Namespace, s.Status.PodName) {
			continue
		}
		freeze := s.Status.Phase == sandboxv1alpha1.PhaseHibernating
		resume := s.Status.Phase == sandboxv1alpha1.PhaseResuming
		if !freeze && !resume {
			continue
		}
		if freeze && s.Status.Hibernation.State == sandboxv1alpha1.RuntimeFrozen {
			continue
		}
		if resume && s.Status.Hibernation.State == sandboxv1alpha1.RuntimeActive {
			continue
		}
		var uid string
		for j := range pods.Items {
			if pods.Items[j].Namespace == s.Namespace && pods.Items[j].Name == s.Status.PodName {
				uid = string(pods.Items[j].UID)
				break
			}
		}
		if uid == "" {
			continue
		}
		if err := a.setFrozen(uid, freeze); err != nil {
			a.Log.Error("cgroup freeze failed", "sandbox", s.Name, "error", err)
			continue
		}
		state := sandboxv1alpha1.RuntimeActive
		if freeze {
			state = sandboxv1alpha1.RuntimeFrozen
		}
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var current sandboxv1alpha1.AgentSandbox
			if err := a.Client.Get(ctx, client.ObjectKeyFromObject(s), &current); err != nil {
				return err
			}
			if current.Status.NodeName != a.NodeName || current.Status.PodName != s.Status.PodName {
				return nil
			}
			current.Status.Hibernation.State = state
			now := metav1.Now()
			current.Status.Hibernation.Since = &now
			return a.Client.Status().Update(ctx, &current)
		}); err != nil {
			a.Log.Error("hibernation status update failed", "sandbox", s.Name, "error", err)
		}
	}
	return a.prewarmOne(ctx)
}

// Pull at most one new image per pass so a slow registry cannot delay freeze/wake.
func (a *Agent) prewarmOne(ctx context.Context) error {
	var templates sandboxv1alpha1.SandboxTemplateList
	if err := a.Client.List(ctx, &templates); err != nil {
		return fmt.Errorf("list templates: %w", err)
	}
	for i := range templates.Items {
		image := templates.Items[i].Spec.Image
		if image == "" || time.Since(a.prewarmed[image]) < 10*time.Minute {
			continue
		}
		pullCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err := a.PullImage(pullCtx, image)
		cancel()
		if err != nil {
			a.Log.Error("image prewarm failed", "image", image, "error", err)
			return nil
		}
		a.prewarmed[image] = time.Now()
		return nil
	}
	return nil
}

func activePod(pods []corev1.Pod, namespace, name string) bool {
	for i := range pods {
		if pods[i].Namespace == namespace && pods[i].Name == name && pods[i].DeletionTimestamp == nil {
			return true
		}
	}
	return false
}

func (a *Agent) observeVMM(active map[string]bool) error {
	entries, err := os.ReadDir(a.ProcRoot)
	if err != nil {
		return err
	}
	current := make(map[int]bool)
	rssByProcess := make(map[string]uint64)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		comm, err := os.ReadFile(filepath.Join(a.ProcRoot, entry.Name(), "comm"))
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(comm))
		if name != "firecracker" && name != "virtiofsd" && name != "cloud-hypervisor" {
			continue
		}
		cg, err := os.ReadFile(filepath.Join(a.ProcRoot, entry.Name(), "cgroup"))
		if err != nil {
			continue
		}
		matches := podUID.FindStringSubmatch(string(cg))
		if len(matches) != 2 {
			continue
		}
		uid := strings.ReplaceAll(matches[1], "_", "-")
		statm, err := os.ReadFile(filepath.Join(a.ProcRoot, entry.Name(), "statm"))
		if err != nil {
			continue
		}
		fields := strings.Fields(string(statm))
		if len(fields) < 2 {
			continue
		}
		pages, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		rssByProcess[name] += pages * uint64(os.Getpagesize())
		if !active[uid] {
			current[pid] = true
			if a.seenOrphans[pid] {
				a.Log.Warn("orphan VMM process detected; manual cleanup required", "pid", pid, "podUID", uid)
			}
		}
	}
	a.seenOrphans = current
	a.metricsMu.Lock()
	a.vmmRSS = rssByProcess
	a.orphanCount = len(current)
	a.metricsMu.Unlock()
	return nil
}

// Metrics emits node-level gauges with bounded label cardinality.
func (a *Agent) Metrics() string {
	a.metricsMu.RLock()
	defer a.metricsMu.RUnlock()
	var b strings.Builder
	b.WriteString("# TYPE sandbox_vmm_rss_bytes gauge\n")
	for _, name := range []string{"firecracker", "virtiofsd", "cloud-hypervisor"} {
		fmt.Fprintf(&b, "sandbox_vmm_rss_bytes{node=%q,process=%q} %d\n", a.NodeName, name, a.vmmRSS[name])
	}
	b.WriteString("# TYPE sandbox_orphan_vmm_processes gauge\n")
	fmt.Fprintf(&b, "sandbox_orphan_vmm_processes{node=%q} %d\n", a.NodeName, a.orphanCount)
	return b.String()
}

func (a *Agent) setFrozen(uid string, freeze bool) error {
	var targets []string
	err := filepath.WalkDir(a.CgroupRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if !strings.Contains(path, uid) && !strings.Contains(path, strings.ReplaceAll(uid, "-", "_")) {
			return nil
		}
		if _, err := os.Stat(filepath.Join(path, "cgroup.freeze")); err == nil {
			targets = append(targets, path)
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(targets) != 1 {
		return fmt.Errorf("expected one pod cgroup for %s, found %d", uid, len(targets))
	}
	value := []byte("0")
	if freeze {
		value = []byte("1")
	}
	if err := os.WriteFile(filepath.Join(targets[0], "cgroup.freeze"), value, 0644); err != nil {
		return err
	}
	return wait.PollUntilContextTimeout(context.Background(), 100*time.Millisecond, 3*time.Second, true, func(context.Context) (bool, error) {
		b, err := os.ReadFile(filepath.Join(targets[0], "cgroup.events"))
		if err != nil {
			return false, err
		}
		want := "frozen 0"
		if freeze {
			want = "frozen 1"
		}
		return strings.Contains(string(b), want), nil
	})
}
