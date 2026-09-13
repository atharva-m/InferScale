package contracts_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/inferscale/inferscale/internal/controller/modelcache"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	kyaml "sigs.k8s.io/yaml"
)

func TestSharedCacheVerifierNamespaceIsIsolatedFromControlPlane(t *testing.T) {
	read := func(path string, object any) {
		t.Helper()
		content, err := os.ReadFile(filepath.Join(repositoryRoot, path))
		if err != nil {
			t.Fatal(err)
		}
		if err := kyaml.Unmarshal(content, object); err != nil {
			t.Fatal(err)
		}
	}
	var cacheNamespace, controlNamespace corev1.Namespace
	read("deploy/base/model-cache/namespace.yaml", &cacheNamespace)
	read("deploy/base/inferscale/namespace.yaml", &controlNamespace)
	if cacheNamespace.Name != modelcache.SharedVerificationNamespace || cacheNamespace.Name == controlNamespace.Name {
		t.Fatalf("shared verifiers require their own installed namespace: cache=%s control=%s", cacheNamespace.Name, controlNamespace.Name)
	}
	for mode, level := range map[string]string{"enforce": "privileged", "audit": "restricted", "warn": "restricted"} {
		if cacheNamespace.Labels["pod-security.kubernetes.io/"+mode] != level {
			t.Errorf("cache namespace must set Pod Security %s=%s", mode, level)
		}
		if controlNamespace.Labels["pod-security.kubernetes.io/"+mode] != "restricted" {
			t.Errorf("control namespace must retain restricted Pod Security %s", mode)
		}
	}
	var policy networkingv1.NetworkPolicy
	read("deploy/base/model-cache/networkpolicy.yaml", &policy)
	if policy.Namespace != cacheNamespace.Name || len(policy.Spec.PodSelector.MatchLabels) != 0 || len(policy.Spec.PodSelector.MatchExpressions) != 0 ||
		len(policy.Spec.Ingress) != 0 || len(policy.Spec.Egress) != 0 {
		t.Fatalf("cache namespace must deny all Pod ingress and egress: %#v", policy.Spec)
	}
	policyTypes := map[networkingv1.PolicyType]bool{}
	for _, policyType := range policy.Spec.PolicyTypes {
		policyTypes[policyType] = true
	}
	if len(policyTypes) != 2 || !policyTypes[networkingv1.PolicyTypeIngress] || !policyTypes[networkingv1.PolicyTypeEgress] {
		t.Fatalf("cache namespace must isolate both traffic directions: %v", policy.Spec.PolicyTypes)
	}
}
