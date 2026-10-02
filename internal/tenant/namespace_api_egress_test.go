package tenant

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
)

func TestNamespaceKubernetesAPIEgressFallbackAndCIDR(t *testing.T) {
	for _, tc := range []struct {
		name string
		cidr string
	}{
		{name: "portable fallback"},
		{name: "configured IPv4", cidr: "10.43.0.1/32"},
		{name: "configured IPv6", cidr: "fd00::1/128"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects := NamespaceBaselineWithOptions(&Tenant{Slug: "acme", Namespace: "tenant-acme"}, NamespaceOptions{
				KubernetesAPIServerCIDR: tc.cidr,
			})
			var policy *networkingv1.NetworkPolicy
			for _, object := range objects {
				if candidate, ok := object.(*networkingv1.NetworkPolicy); ok && candidate.Name == "allow-kubernetes-api-egress" {
					policy = candidate
					break
				}
			}
			if policy == nil || len(policy.Spec.Egress) != 1 {
				t.Fatal("expected one Kubernetes API egress rule")
			}
			rule := policy.Spec.Egress[0]
			if len(rule.Ports) != 1 || rule.Ports[0].Protocol == nil || *rule.Ports[0].Protocol != corev1.ProtocolTCP ||
				rule.Ports[0].Port == nil || rule.Ports[0].Port.IntVal != 443 {
				t.Fatalf("Kubernetes API egress must allow only TCP/443: %#v", rule.Ports)
			}
			body, err := json.Marshal(rule)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(body, &wire); err != nil {
				t.Fatal(err)
			}
			if tc.cidr == "" {
				if _, hasTo := wire["to"]; hasTo {
					t.Fatalf("portable fallback must omit the peer list, not send an invalid empty peer: %s", body)
				}
				return
			}
			var peers []networkingv1.NetworkPolicyPeer
			if err := json.Unmarshal(wire["to"], &peers); err != nil {
				t.Fatal(err)
			}
			if len(peers) != 1 || peers[0].IPBlock == nil || peers[0].IPBlock.CIDR != tc.cidr ||
				len(peers[0].IPBlock.Except) != 0 || peers[0].PodSelector != nil || peers[0].NamespaceSelector != nil {
				t.Fatalf("configured API destination must be exactly %s: %s", tc.cidr, body)
			}
		})
	}
}

func TestNamespaceKubernetesAPIEgressAfterK3sDNAT(t *testing.T) {
	objects := NamespaceBaselineWithOptions(&Tenant{Slug: "acme", Namespace: "tenant-acme"}, NamespaceOptions{
		KubernetesAPIServerCIDRs: []string{"10.0.0.12/32", "fd00::12/128"},
		KubernetesAPIServerPort:  6443,
	})
	for _, object := range objects {
		policy, ok := object.(*networkingv1.NetworkPolicy)
		if !ok || policy.Name != "allow-kubernetes-api-egress" {
			continue
		}
		rules := policy.Spec.Egress
		if len(rules) != 1 || len(rules[0].To) != 2 || len(rules[0].Ports) != 1 || rules[0].Ports[0].Port.IntVal != 6443 {
			t.Fatalf("expected one bounded dual-stack TCP6443 egress rule: %#v", rules)
		}
		for i, cidr := range []string{"10.0.0.12/32", "fd00::12/128"} {
			peer := rules[0].To[i]
			if peer.IPBlock == nil || peer.IPBlock.CIDR != cidr || peer.NamespaceSelector != nil || peer.PodSelector != nil {
				t.Fatalf("API egress escaped its explicit endpoint: %#v", peer)
			}
		}
		return
	}
	t.Fatal("missing API egress policy")
}
