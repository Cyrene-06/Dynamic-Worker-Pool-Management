package controller

import (
	"context"
	"fmt"
	"reflect"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
	"github.com/Cyrene-06/Dynamic-Worker-Pool-Management/internal/egress"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func isVMIsolation(level sandboxv1alpha1.IsolationLevel) bool {
	return level == sandboxv1alpha1.IsolationKataFC || level == sandboxv1alpha1.IsolationKataCLH
}

func (r *SandboxReconciler) ensureCiliumPolicy(ctx context.Context, sbx *sandboxv1alpha1.AgentSandbox, tmpl *sandboxv1alpha1.SandboxTemplate) error {
	if r.CiliumClient == nil {
		return fmt.Errorf("Kata sandbox requires a Cilium client")
	}
	name := tmpl.Spec.EgressProfile
	if name == "" {
		return fmt.Errorf("template %q has no egress profile", tmpl.Name)
	}
	if sbx.Spec.Network.EgressProfile != "" && sbx.Spec.Network.EgressProfile != name {
		return fmt.Errorf("sandbox egress profile %q differs from curated template profile %q", sbx.Spec.Network.EgressProfile, name)
	}
	var cm corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Namespace: sandboxv1alpha1.NamespaceSystem, Name: "sandbox-egress-profiles"}, &cm); err != nil {
		return fmt.Errorf("read egress profiles: %w", err)
	}
	catalog, err := egress.Parse([]byte(cm.Data["profiles.yaml"]))
	if err != nil {
		return fmt.Errorf("parse egress profiles: %w", err)
	}
	profile, ok := catalog.Profiles[name]
	if !ok {
		return fmt.Errorf("unknown egress profile %q", name)
	}
	for _, extra := range sbx.Spec.Network.AdditionalEgress {
		approved := false
		for _, d := range profile.MatchNames {
			if extra == d {
				approved = true
				break
			}
		}
		if !approved {
			return fmt.Errorf("additional egress %q is not approved in profile %q", extra, name)
		}
	}
	desired := egress.Render(sbx, profile)
	var existing unstructured.Unstructured
	existing.SetAPIVersion("cilium.io/v2")
	existing.SetKind("CiliumNetworkPolicy")
	err = r.CiliumClient.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if apierrors.IsNotFound(err) {
		return r.CiliumClient.Create(ctx, desired)
	}
	if err != nil {
		return fmt.Errorf("read Cilium policy: %w", err)
	}
	if reflect.DeepEqual(existing.Object["spec"], desired.Object["spec"]) {
		return nil
	}
	existing.Object["spec"] = desired.Object["spec"]
	return r.CiliumClient.Update(ctx, &existing)
}

func (r *SandboxReconciler) quarantineCiliumPolicy(ctx context.Context, sbx *sandboxv1alpha1.AgentSandbox) error {
	if r.CiliumClient == nil {
		return nil
	}
	cnp := &unstructured.Unstructured{}
	cnp.SetAPIVersion("cilium.io/v2")
	cnp.SetKind("CiliumNetworkPolicy")
	if err := r.CiliumClient.Get(ctx, types.NamespacedName{Namespace: sbx.Namespace, Name: sbx.Name}, cnp); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil
		}
		return err
	}
	spec, ok := cnp.Object["spec"].(map[string]any)
	if !ok {
		return fmt.Errorf("Cilium policy %s has no spec", cnp.GetName())
	}
	cnp.Object["spec"] = map[string]any{
		"endpointSelector": spec["endpointSelector"],
		"ingressDeny":      []any{map[string]any{"fromEntities": []any{"all"}}},
		"egressDeny":       []any{map[string]any{"toEntities": []any{"all"}}},
	}
	return r.CiliumClient.Update(ctx, cnp)
}
