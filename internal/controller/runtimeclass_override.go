package controller

import (
	"context"
	"fmt"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func validateRuntimeClassOverride(ctx context.Context, c client.Client, level sandboxv1alpha1.IsolationLevel, name string) error {
	if name == "" {
		return nil
	}
	var handler string
	switch level {
	case sandboxv1alpha1.IsolationKataFC:
		handler = "kata-fc"
	case sandboxv1alpha1.IsolationKataCLH:
		handler = "kata-clh"
	default:
		return fmt.Errorf("runtimeClassName override requires a Kata isolation level")
	}
	var rc nodev1.RuntimeClass
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &rc); err != nil {
		return fmt.Errorf("RuntimeClass %q unavailable: %w", name, err)
	}
	if rc.Handler != handler {
		return fmt.Errorf("RuntimeClass %q handler %q does not match isolation %q", name, rc.Handler, level)
	}
	return nil
}
