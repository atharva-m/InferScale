package tenant

import (
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
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
	metricsPorts := map[int32]bool{8000: true, 9000: true, 9090: true}
	for _, port := range monitoring.Spec.Ingress[0].Ports {
		if port.Protocol == nil || *port.Protocol != corev1.ProtocolTCP || port.Port == nil || !metricsPorts[port.Port.IntVal] {
			t.Fatalf("monitoring must expose only runtime/exporter/EPP metrics: %#v", port)
		}
		delete(metricsPorts, port.Port.IntVal)
	}
	if len(metricsPorts) != 0 {
		t.Fatalf("missing monitoring metrics ports: %v", metricsPorts)
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

func TestNamespaceContainerBoundsPreserveModelDependentMemoryLimits(t *testing.T) {
	for _, object := range NamespaceBaseline(&Tenant{Slug: "acme", Namespace: "tenant-acme", Quota: Quota{MaxGPUs: 4}}) {
		bounds, ok := object.(*corev1.LimitRange)
		if !ok {
			continue
		}
		if len(bounds.Spec.Limits) != 1 || bounds.Spec.Limits[0].Type != corev1.LimitTypeContainer {
			t.Fatalf("expected one container LimitRange: %#v", bounds.Spec)
		}
		item := bounds.Spec.Limits[0]
		if len(item.Default) != 0 || len(item.Max) != 0 {
			t.Fatal("namespace maxima/default limits can over-reserve EPP or starve GPU workers")
		}
		if item.Min.Cpu().MilliValue() != 10 || item.Min.Memory().Value() != 16*1024*1024 ||
			item.DefaultRequest.Cpu().MilliValue() != 100 || item.DefaultRequest.Memory().Value() != 128*1024*1024 {
			t.Fatalf("expected bounded minimum/default scheduling requests: %#v", item)
		}
		return
	}
	t.Fatal("tenant container LimitRange missing")
}

// The bootstrap path must preserve the same isolation and admission behavior
// as operator provisioning; a looser policy is additive, not overridden later.
func TestBootstrapTemplateMatchesProvisionedTenantPolicies(t *testing.T) {
	data, err := os.ReadFile("../../deploy/templates/tenant-baseline.yaml")
	if err != nil {
		t.Fatal(err)
	}
	rendered := strings.NewReplacer("TENANT_NAMESPACE", "tenant-acme", "TENANT_SLUG", "acme", "GPU_RESOURCE_QUOTA", "8", "GPU_QUOTA", "4").Replace(string(data))
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(rendered), 4096)
	actual := make(map[string]map[string]interface{})
	for {
		var object unstructured.Unstructured
		if err := decoder.Decode(&object); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		key := object.GetKind() + "/" + object.GetName()
		if _, duplicate := actual[key]; duplicate {
			t.Fatalf("duplicate bootstrap object %s", key)
		}
		actual[key] = object.Object
	}
	for _, expected := range NamespaceBaseline(&Tenant{Slug: "acme", Namespace: "tenant-acme", Quota: Quota{MaxGPUs: 4}}) {
		key := expected.GetObjectKind().GroupVersionKind().Kind + "/" + expected.GetName()
		got, ok := actual[key]
		if !ok {
			t.Errorf("bootstrap object %s missing", key)
			continue
		}
		delete(actual, key)
		decoded := reflect.New(reflect.TypeOf(expected).Elem()).Interface()
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(got, decoded); err != nil {
			t.Fatalf("decode %s: %v", key, err)
		}
		// Labels/annotations identify operator ownership, while the spec,
		// namespace isolation labels, and token automount affect admission.
		var wantPolicy, gotPolicy interface{}
		switch want := expected.(type) {
		case *corev1.Namespace:
			wantPolicy, gotPolicy = want.Labels, decoded.(*corev1.Namespace).Labels
		case *corev1.ServiceAccount:
			wantPolicy, gotPolicy = want.AutomountServiceAccountToken, decoded.(*corev1.ServiceAccount).AutomountServiceAccountToken
		case *corev1.ResourceQuota:
			wantPolicy, gotPolicy = want.Spec, decoded.(*corev1.ResourceQuota).Spec
		case *corev1.LimitRange:
			wantPolicy, gotPolicy = want.Spec, decoded.(*corev1.LimitRange).Spec
		case *networkingv1.NetworkPolicy:
			wantPolicy, gotPolicy = want.Spec, decoded.(*networkingv1.NetworkPolicy).Spec
		default:
			t.Fatalf("unhandled tenant baseline object %T", expected)
		}
		if !equality.Semantic.DeepEqual(wantPolicy, gotPolicy) {
			t.Errorf("%s bootstrap policy differs from provisioned policy:\nwant %#v\ngot %#v", key, wantPolicy, gotPolicy)
		}
	}
	if len(actual) != 0 {
		t.Fatalf("unexpected bootstrap objects: %v", actual)
	}
}
