#!/usr/bin/env bash
# Run on an Ubuntu KVM host with kubeadm, kubectl, helm and containerd installed.
# Chart versions are explicit so an E2 environment can be reproduced.
set -euo pipefail

: "${KATA_VERSION:?set the reviewed kata-deploy chart version}"
: "${CILIUM_VERSION:?set the reviewed Cilium chart version}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
for bin in kubeadm kubectl helm containerd; do command -v "$bin" >/dev/null || { echo "missing $bin" >&2; exit 1; }; done
if [ "$(id -u)" -ne 0 ]; then echo "run as root on the E2 node" >&2; exit 1; fi

bash "$REPO_ROOT/hack/bootstrap/node-init.sh"
if [ ! -f /etc/kubernetes/admin.conf ]; then
    kubeadm init --pod-network-cidr=10.244.0.0/16
fi
export KUBECONFIG=/etc/kubernetes/admin.conf
NODE_NAME="$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')"
kubectl taint node "$NODE_NAME" node-role.kubernetes.io/control-plane- 2>/dev/null || true
kubectl label node "$NODE_NAME" sandbox.example.com/isolation=kata-fc --overwrite
kubectl taint node "$NODE_NAME" sandbox=true:NoSchedule --overwrite

helm repo add cilium https://helm.cilium.io/ --force-update
helm repo update cilium
helm upgrade --install cilium cilium/cilium --namespace kube-system \
  --version "$CILIUM_VERSION" --wait --timeout 10m

helm upgrade --install kata-deploy \
  oci://ghcr.io/kata-containers/kata-deploy-charts/kata-deploy \
  --namespace kube-system --version "$KATA_VERSION" \
  --set-string 'shims.enabled=firecracker\,clh' \
  --set createRuntimeClasses=false \
  --set-string 'nodeSelector.sandbox\.example\.com/isolation=kata-fc' \
  --set 'tolerations[0].key=sandbox' \
  --set 'tolerations[0].operator=Equal' \
  --set-string 'tolerations[0].value=true' \
  --set 'tolerations[0].effect=NoSchedule' \
  --wait --timeout 10m

kubectl apply -k "$REPO_ROOT/config/crd"
kubectl apply -f "$REPO_ROOT/config/samples/namespaces.yaml"
kubectl apply -k "$REPO_ROOT/config/runtimeclass"
kubectl apply -k "$REPO_ROOT/config/node-agent"
echo "E2 resources installed on $NODE_NAME. Replace the node-agent dev image with a built immutable tag before rollout."
