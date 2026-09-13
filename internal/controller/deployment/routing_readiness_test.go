package deployment

import (
	"context"
	"testing"

	"github.com/inferscale/inferscale/internal/routing"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGatewayParentReadyRequiresConfiguredGatewayAndFreshConditions(t *testing.T) {
	t.Parallel()
	object := routingStatusObject("InferencePool", "pool", 3, "other", "gateway-system", 3)
	if ready, _ := gatewayParentReady(object, "public", "gateway-system", "Accepted", "ResolvedRefs"); ready {
		t.Fatal("status from a different Gateway must not make the pool ready")
	}

	object = routingStatusObject("InferencePool", "pool", 3, "public", "gateway-system", 2)
	if ready, detail := gatewayParentReady(object, "public", "gateway-system", "Accepted", "ResolvedRefs"); ready || detail != "Accepted condition is stale" {
		t.Fatalf("ready=%v detail=%q", ready, detail)
	}

	object = routingStatusObject("InferencePool", "pool", 3, "public", "gateway-system", 3)
	if ready, detail := gatewayParentReady(object, "public", "gateway-system", "Accepted", "ResolvedRefs"); !ready {
		t.Fatalf("fresh configured parent should be ready: %s", detail)
	}
}

func TestRoutingReadyRequiresEPPPoolAndRoute(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	epp := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "rev-epp", Namespace: "tenant", Generation: 2},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 2, AvailableReplicas: 1},
	}
	pool := routingStatusObject("InferencePool", "rev-pool", 1, "public", "gateway-system", 1)
	route := routingStatusObject("HTTPRoute", "chat-inference", 1, "public", "gateway-system", 1)
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(epp).WithObjects(epp, pool, route).Build()
	reconciler := Reconciler{Client: client, Config: Config{GatewayName: "public", GatewayNamespace: "gateway-system"}}

	ready, reason, _, err := reconciler.routingReady(context.Background(), "tenant", "chat-inference", routing.RevisionNames{
		EndpointPicker: "rev-epp", Pool: "rev-pool",
	})
	if err != nil || !ready || reason != "RouteAccepted" {
		t.Fatalf("ready=%v reason=%q err=%v", ready, reason, err)
	}

	epp.Status.AvailableReplicas = 0
	if err := client.Status().Update(context.Background(), epp); err != nil {
		t.Fatal(err)
	}
	ready, reason, _, err = reconciler.routingReady(context.Background(), "tenant", "chat-inference", routing.RevisionNames{
		EndpointPicker: "rev-epp", Pool: "rev-pool",
	})
	if err != nil || ready || reason != "EndpointPickerNotReady" {
		t.Fatalf("ready=%v reason=%q err=%v", ready, reason, err)
	}
}

func routingStatusObject(kind, name string, generation int64, gateway, gatewayNamespace string, observed int64) *unstructured.Unstructured {
	apiVersion := "inference.networking.k8s.io/v1"
	if kind == "HTTPRoute" {
		apiVersion = "gateway.networking.k8s.io/v1"
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata": map[string]any{
			"name": name, "namespace": "tenant", "generation": generation,
		},
		"status": map[string]any{"parents": []any{map[string]any{
			"parentRef": map[string]any{
				"group": "gateway.networking.k8s.io", "kind": "Gateway",
				"name": gateway, "namespace": gatewayNamespace,
			},
			"conditions": []any{
				map[string]any{"type": "Accepted", "status": "True", "observedGeneration": observed},
				map[string]any{"type": "ResolvedRefs", "status": "True", "observedGeneration": observed},
			},
		}}},
	}}
}
