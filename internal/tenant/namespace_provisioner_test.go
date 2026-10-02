package tenant

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The fake client does not exercise the wire encoding of server-side apply.
// Use the real REST client so missing TypeMeta cannot silently pass this test.
func TestNamespaceProvisionerAppliesCompleteTypedBaseline(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{corev1.SchemeGroupVersion, networkingv1.SchemeGroupVersion})
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Namespace"), meta.RESTScopeRoot)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("ServiceAccount"), meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("ResourceQuota"), meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("LimitRange"), meta.RESTScopeNamespace)
	mapper.Add(networkingv1.SchemeGroupVersion.WithKind("NetworkPolicy"), meta.RESTScopeNamespace)

	expected := map[string]metav1.TypeMeta{
		"/api/v1/namespaces/tenant-acme":                                         {APIVersion: "v1", Kind: "Namespace"},
		"/api/v1/namespaces/tenant-acme/serviceaccounts/inferscale-runtime":      {APIVersion: "v1", Kind: "ServiceAccount"},
		"/api/v1/namespaces/tenant-acme/resourcequotas/inferscale-tenant":        {APIVersion: "v1", Kind: "ResourceQuota"},
		"/api/v1/namespaces/tenant-acme/limitranges/inferscale-container-bounds": {APIVersion: "v1", Kind: "LimitRange"},
	}
	for _, name := range []string{
		"default-deny", "allow-dns-egress", "allow-gateway-ingress", "allow-monitoring-ingress",
		"allow-otel-egress", "allow-same-namespace-egress", "allow-https-egress", "allow-kubernetes-api-egress",
		"allow-benchmark-callback-egress", "allow-benchmark-gateway-egress", "allow-benchmark-prometheus-egress",
	} {
		expected["/apis/networking.k8s.io/v1/namespaces/tenant-acme/networkpolicies/"+name] = metav1.TypeMeta{
			APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy",
		}
	}
	requests := make(chan string, len(expected))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want, known := expected[r.URL.Path]
		if !known || r.Method != http.MethodPatch || r.Header.Get("Content-Type") != string(types.ApplyPatchType) {
			t.Errorf("unexpected apply request: %s %s (%s)", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
			http.Error(w, "unexpected apply request", http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("fieldManager") != "inferscale-controller" || r.URL.Query().Get("force") != "true" {
			t.Errorf("apply ownership missing: %s", r.URL.RawQuery)
		}
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode apply body: %v", err)
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		var got metav1.TypeMeta
		if err := json.Unmarshal(body, &got); err != nil || got != want {
			t.Errorf("%s apply metadata = %#v, want %#v (decode error: %v)", r.URL.Path, got, want, err)
			http.Error(w, "missing or incorrect type metadata", http.StatusBadRequest)
			return
		}
		select {
		case requests <- r.URL.Path:
		default:
			t.Error("more apply requests than baseline objects")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	kubeClient, err := client.New(&rest.Config{Host: server.URL}, client.Options{Scheme: scheme, Mapper: mapper})
	if err != nil {
		t.Fatal(err)
	}
	provisioner := NamespaceProvisioner{Client: kubeClient}
	if err := provisioner.Provision(context.Background(), &Tenant{Slug: "acme", Namespace: "tenant-acme", Quota: Quota{MaxGPUs: 1}}); err != nil {
		t.Fatalf("provision tenant namespace through REST client: %v", err)
	}
	if len(requests) != len(expected) {
		t.Fatalf("applied %d baseline objects, want %d", len(requests), len(expected))
	}
	seen := make(map[string]bool, len(expected))
	for range expected {
		path := <-requests
		if seen[path] {
			t.Errorf("applied %s more than once", path)
		}
		seen[path] = true
	}
}
