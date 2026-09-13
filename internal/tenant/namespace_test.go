package tenant

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
)

func TestNamespaceBaselineIsDefaultDenyAndGPUQuotaBounded(t *testing.T) {
	value := &Tenant{Slug: "acme", Namespace: "tenant-acme", Quota: Quota{MaxGPUs: 4}}
	objects := NamespaceBaseline(value)
	var quota *corev1.ResourceQuota
	var serviceAccount *corev1.ServiceAccount
	var defaultDeny *networkingv1.NetworkPolicy
	var gateway *networkingv1.NetworkPolicy
	var monitoring *networkingv1.NetworkPolicy
	var benchmarkPrometheus *networkingv1.NetworkPolicy
	policies := map[string]bool{}
	for _, object := range objects {
		switch typed := object.(type) {
		case *corev1.ServiceAccount:
			serviceAccount = typed
		case *corev1.ResourceQuota:
			quota = typed
		case *networkingv1.NetworkPolicy:
			policies[typed.Name] = true
			if typed.Name == "default-deny" {
				defaultDeny = typed
			}
			if typed.Name == "allow-gateway-ingress" {
				gateway = typed
			}
			if typed.Name == "allow-monitoring-ingress" {
				monitoring = typed
			}
			if typed.Name == "allow-benchmark-prometheus-egress" {
				benchmarkPrometheus = typed
			}
		}
	}
	if quota == nil {
		t.Fatal("GPU ResourceQuota missing")
	}
	if serviceAccount == nil || serviceAccount.AutomountServiceAccountToken == nil || *serviceAccount.AutomountServiceAccountToken {
		t.Fatal("runtime ServiceAccount must not automount a Kubernetes API token")
	}
	quantity := quota.Spec.Hard[corev1.ResourceName("limits.nvidia.com/gpu")]
	if quantity.Value() != 8 || quota.Annotations["inferscale.io/steady-state-gpu-quota"] != "4" {
		t.Fatal("GPU ResourceQuota must include one platform-controlled rollout surge")
	}
	if defaultDeny == nil || len(defaultDeny.Spec.PolicyTypes) != 2 {
		t.Fatal("default-deny ingress/egress policy missing")
	}
	if gateway == nil || len(gateway.Spec.PodSelector.MatchLabels) != 0 || len(gateway.Spec.Ingress) != 1 || len(gateway.Spec.Ingress[0].Ports) != 2 {
		t.Fatal("Gateway ingress must reach both runtime and EPP data-plane ports")
	}
	for _, port := range gateway.Spec.Ingress[0].Ports {
		if port.Port == nil || (port.Port.IntVal != 8000 && port.Port.IntVal != 9002) {
			t.Fatal("Gateway ingress must not expose unauthenticated EPP metrics or health")
		}
	}
	if monitoring == nil || len(monitoring.Spec.Ingress) != 1 || len(monitoring.Spec.Ingress[0].From) != 1 {
		t.Fatal("isolated runtime/EPP metrics require a dedicated monitoring ingress peer")
	}
	monitoringPeer := monitoring.Spec.Ingress[0].From[0]
	if monitoringPeer.NamespaceSelector == nil || monitoringPeer.PodSelector == nil ||
		monitoringPeer.NamespaceSelector.MatchLabels["inferscale.io/access-role"] != "monitoring" ||
		monitoringPeer.PodSelector.MatchLabels["app.kubernetes.io/name"] != "prometheus" {
		t.Fatal("unauthenticated metrics must be reachable only from monitoring Prometheus Pods")
	}
	if benchmarkPrometheus == nil || len(benchmarkPrometheus.Spec.Egress) != 1 ||
		len(benchmarkPrometheus.Spec.Egress[0].Ports) != 1 ||
		benchmarkPrometheus.Spec.Egress[0].Ports[0].Port == nil ||
		benchmarkPrometheus.Spec.Egress[0].Ports[0].Port.IntVal != 9090 {
		t.Fatal("benchmark Pods must reach only the monitoring Prometheus port through their dedicated policy")
	}
	for _, name := range []string{"allow-dns-egress", "allow-same-namespace-egress", "allow-https-egress", "allow-kubernetes-api-egress", "allow-gateway-ingress", "allow-monitoring-ingress", "allow-benchmark-callback-egress", "allow-benchmark-gateway-egress", "allow-benchmark-prometheus-egress"} {
		if !policies[name] {
			t.Errorf("required network policy %s missing", name)
		}
	}
}
