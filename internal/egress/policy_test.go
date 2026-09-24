package egress

import (
	"testing"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestRenderEnforcesSandboxBoundary(t *testing.T) {
	c, err := Parse([]byte("profiles:\n  default:\n    matchNames: [pypi.org]\n    matchPatterns: ['*.pythonhosted.org']\n    ports: [443]\n"))
	if err != nil {
		t.Fatal(err)
	}
	sbx := &sandboxv1alpha1.AgentSandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx-1", Namespace: "sandbox-pool"}}
	p := Render(sbx, c.Profiles["default"])
	spec := p.Object["spec"].(map[string]any)
	selector, _, _ := unstructured.NestedStringMap(spec, "endpointSelector", "matchLabels")
	if selector[sandboxv1alpha1.LabelSandbox] != "sbx-1" {
		t.Fatalf("policy is not sandbox scoped: %v", selector)
	}
	ingress, _, _ := unstructured.NestedSlice(spec, "ingress")
	if len(ingress) != 1 {
		t.Fatalf("expected one gateway ingress rule, got %v", ingress)
	}
	from := ingress[0].(map[string]any)["fromEndpoints"].([]any)[0].(map[string]any)["matchLabels"].(map[string]any)
	if from["app"] != "sandbox-gateway" || from["k8s:io.kubernetes.pod.namespace"] != "sandbox-system" {
		t.Fatalf("ingress is not gateway-only: %v", from)
	}
	egress, _, _ := unstructured.NestedSlice(spec, "egress")
	if len(egress) != 3 {
		t.Fatalf("expected DNS, FQDN and gateway egress, got %v", egress)
	}
	dns := egress[0].(map[string]any)["toPorts"].([]any)[0].(map[string]any)["rules"].(map[string]any)["dns"].([]any)
	if len(dns) != 2 {
		t.Fatalf("DNS allowlist missing: %v", dns)
	}
	fqdns := egress[1].(map[string]any)["toFQDNs"].([]any)
	if len(fqdns) != 2 {
		t.Fatalf("FQDN allowlist missing: %v", fqdns)
	}
	deny, _, _ := unstructured.NestedSlice(spec, "egressDeny")
	if len(deny) != 2 {
		t.Fatalf("API server and metadata IP denies missing: %v", deny)
	}
}

func TestParseRejectsBroadWildcard(t *testing.T) {
	if _, err := Parse([]byte("profiles:\n  unsafe:\n    matchPatterns: ['*']\n")); err == nil {
		t.Fatal("broad wildcard should be rejected")
	}
}
