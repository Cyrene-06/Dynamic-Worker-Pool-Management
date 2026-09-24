package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
	"github.com/Cyrene-06/Dynamic-Worker-Pool-Management/internal/nodeagent"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

func main() {
	var node, namespace, criSocket, procRoot, cgroupRoot, healthAddr string
	var interval time.Duration
	flag.StringVar(&node, "node", os.Getenv("NODE_NAME"), "node name (default: NODE_NAME)")
	flag.StringVar(&namespace, "namespace", sandboxv1alpha1.NamespacePool, "sandbox namespace")
	flag.StringVar(&criSocket, "cri-socket", "/host/run/containerd/containerd.sock", "host CRI socket")
	flag.StringVar(&procRoot, "proc-root", "/host/proc", "host procfs")
	flag.StringVar(&cgroupRoot, "cgroup-root", "/host/sys/fs/cgroup", "host cgroup v2 root")
	flag.StringVar(&healthAddr, "health-address", ":8081", "health server address")
	flag.DurationVar(&interval, "interval", 30*time.Second, "reconciliation interval")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if node == "" {
		log.Error("NODE_NAME is empty")
		os.Exit(1)
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		log.Error("core scheme", "error", err)
		os.Exit(1)
	}
	if err := sandboxv1alpha1.AddToScheme(scheme); err != nil {
		log.Error("sandbox scheme", "error", err)
		os.Exit(1)
	}
	k8s, err := client.New(config.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		log.Error("Kubernetes client", "error", err)
		os.Exit(1)
	}
	pull, closeCRI, err := nodeagent.NewImagePuller(criSocket)
	if err != nil {
		log.Error("CRI client", "error", err)
		os.Exit(1)
	}
	defer closeCRI()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	agent := &nodeagent.Agent{Client: k8s, NodeName: node, Namespace: namespace, ProcRoot: procRoot, CgroupRoot: cgroupRoot, PullImage: pull, Log: log, Interval: interval}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(agent.Metrics()))
	})
	server := &http.Server{Addr: healthAddr, Handler: mux}
	go func() { _ = server.ListenAndServe() }()
	defer server.Shutdown(context.Background())
	if err := agent.Run(ctx); err != nil {
		log.Error("node agent stopped", "error", err)
		os.Exit(1)
	}
}
