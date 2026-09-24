package egress

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"

	sandboxv1alpha1 "github.com/Cyrene-06/Dynamic-Worker-Pool-Management/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

type Catalog struct {
	Profiles map[string]Profile `json:"profiles"`
}

type Profile struct {
	MatchNames    []string `json:"matchNames"`
	MatchPatterns []string `json:"matchPatterns"`
	Ports         []int    `json:"ports"`
}

var domain = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)+$`)

func Parse(data []byte) (*Catalog, error) {
	var c Catalog
	if err := yaml.UnmarshalStrict(data, &c); err != nil {
		return nil, err
	}
	if len(c.Profiles) == 0 {
		return nil, fmt.Errorf("egress catalog has no profiles")
	}
	for name, p := range c.Profiles {
		if name == "" || len(p.MatchNames)+len(p.MatchPatterns) == 0 {
			return nil, fmt.Errorf("profile %q has no domains", name)
		}
		for _, d := range p.MatchNames {
			if !validDomain(d) {
				return nil, fmt.Errorf("profile %q has invalid domain %q", name, d)
			}
		}
		for _, d := range p.MatchPatterns {
			if !strings.HasPrefix(d, "*.") || !validDomain(strings.TrimPrefix(d, "*.")) {
				return nil, fmt.Errorf("profile %q has invalid pattern %q", name, d)
			}
		}
		if len(p.Ports) == 0 {
			p.Ports = []int{443}
		}
		for _, port := range p.Ports {
			if port < 1 || port > 65535 {
				return nil, fmt.Errorf("profile %q has invalid port %d", name, port)
			}
		}
		c.Profiles[name] = p
	}
	return &c, nil
}

func validDomain(d string) bool { return domain.MatchString(d) && net.ParseIP(d) == nil }

// Render creates a deny-by-default CiliumNetworkPolicy for one sandbox.
// A sandbox-specific selector prevents one template's allowlist from widening another's.
func Render(sbx *sandboxv1alpha1.AgentSandbox, profile Profile) *unstructured.Unstructured {
	cnp := &unstructured.Unstructured{}
	cnp.SetAPIVersion("cilium.io/v2")
	cnp.SetKind("CiliumNetworkPolicy")
	cnp.SetName(sbx.Name)
	cnp.SetNamespace(sbx.Namespace)
	cnp.SetLabels(map[string]string{
		sandboxv1alpha1.LabelSandbox: sbx.Name,
		sandboxv1alpha1.LabelRole:    sandboxv1alpha1.RoleNetpol,
		sandboxv1alpha1.LabelPool:    sbx.Spec.PoolRef.Name,
	})
	cnp.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: sandboxv1alpha1.GroupVersion.String(), Kind: "AgentSandbox", Name: sbx.Name, UID: sbx.UID}})
	selector := map[string]any{"matchLabels": map[string]any{sandboxv1alpha1.LabelSandbox: sbx.Name, sandboxv1alpha1.LabelRole: sandboxv1alpha1.RoleSandbox}}
	fqdns, dns := []any{}, []any{}
	for _, d := range profile.MatchNames {
		fqdns = append(fqdns, map[string]any{"matchName": d})
		dns = append(dns, map[string]any{"matchName": d})
	}
	for _, d := range profile.MatchPatterns {
		fqdns = append(fqdns, map[string]any{"matchPattern": d})
		dns = append(dns, map[string]any{"matchPattern": d})
	}
	ports := []any{}
	for _, p := range profile.Ports {
		ports = append(ports, map[string]any{"port": fmt.Sprint(p), "protocol": "TCP"})
	}
	// Stable order keeps patches and policy reviews readable.
	sort.Slice(ports, func(i, j int) bool {
		return ports[i].(map[string]any)["port"].(string) < ports[j].(map[string]any)["port"].(string)
	})
	dnsEndpoint := map[string]any{"matchLabels": map[string]any{"k8s:io.kubernetes.pod.namespace": "kube-system", "k8s:k8s-app": "kube-dns"}}
	gatewayEndpoint := map[string]any{"matchLabels": map[string]any{"k8s:io.kubernetes.pod.namespace": sandboxv1alpha1.NamespaceSystem, "app": "sandbox-gateway"}}
	cnp.Object["spec"] = map[string]any{
		"endpointSelector": selector,
		"ingress":          []any{map[string]any{"fromEndpoints": []any{gatewayEndpoint}}},
		"egress": []any{
			map[string]any{"toEndpoints": []any{dnsEndpoint}, "toPorts": []any{map[string]any{"ports": []any{map[string]any{"port": "53", "protocol": "ANY"}}, "rules": map[string]any{"dns": dns}}}},
			map[string]any{"toFQDNs": fqdns, "toPorts": []any{map[string]any{"ports": ports}}},
			map[string]any{"toEndpoints": []any{gatewayEndpoint}},
		},
		"egressDeny": []any{
			map[string]any{"toEntities": []any{"kube-apiserver"}},
			map[string]any{"toCIDRSet": []any{map[string]any{"cidr": "169.254.169.254/32"}}},
		},
	}
	return cnp
}
