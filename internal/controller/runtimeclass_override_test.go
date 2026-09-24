package controller

import (
	"context"
	"testing"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
	nodev1 "k8s.io/api/node/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRuntimeClassOverrideHandlerMustMatchIsolation(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := nodev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "kata-fc-v2"}, Handler: "kata-fc"},
		&nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "wrong"}, Handler: "runc"},
	).Build()
	ctx := context.Background()
	if err := validateRuntimeClassOverride(ctx, c, sandboxv1alpha1.IsolationKataFC, "kata-fc-v2"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		level sandboxv1alpha1.IsolationLevel
		name  string
	}{
		{sandboxv1alpha1.IsolationKataFC, "wrong"},
		{sandboxv1alpha1.IsolationKataCLH, "kata-fc-v2"},
		{sandboxv1alpha1.IsolationRunc, "kata-fc-v2"},
		{sandboxv1alpha1.IsolationKataFC, "missing"},
	} {
		if err := validateRuntimeClassOverride(ctx, c, tc.level, tc.name); err == nil {
			t.Fatalf("expected rejection for %s / %s", tc.level, tc.name)
		}
	}
}
