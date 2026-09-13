package deployment

import (
	"context"
	"fmt"

	"github.com/inferscale/inferscale/internal/routing"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

func (r *Reconciler) routingReady(
	ctx context.Context,
	namespace, routeName string,
	names routing.RevisionNames,
) (bool, string, string, error) {
	epp := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: names.EndpointPicker}, epp); err != nil {
		if apierrors.IsNotFound(err) {
			return false, "EndpointPickerCreating", "waiting for endpoint-picker deployment", nil
		}
		return false, "", "", fmt.Errorf("observe endpoint-picker deployment: %w", err)
	}
	if epp.Status.ObservedGeneration < epp.Generation || epp.Status.AvailableReplicas < 1 {
		return false, "EndpointPickerNotReady", "waiting for a ready endpoint-picker replica", nil
	}

	pool := &unstructured.Unstructured{}
	pool.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "inference.networking.k8s.io", Version: "v1", Kind: "InferencePool",
	})
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: names.Pool}, pool); err != nil {
		if apierrors.IsNotFound(err) {
			return false, "InferencePoolCreating", "waiting for the inference pool", nil
		}
		return false, "", "", fmt.Errorf("observe inference pool: %w", err)
	}
	if ready, detail := gatewayParentReady(pool, r.Config.GatewayName, r.Config.GatewayNamespace, "Accepted", "ResolvedRefs"); !ready {
		return false, "InferencePoolNotAccepted", detail, nil
	}

	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute",
	})
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: routeName}, route); err != nil {
		if apierrors.IsNotFound(err) {
			return false, "HTTPRouteCreating", "waiting for the public inference route", nil
		}
		return false, "", "", fmt.Errorf("observe public HTTPRoute: %w", err)
	}
	if ready, detail := gatewayParentReady(route, r.Config.GatewayName, r.Config.GatewayNamespace, "Accepted", "ResolvedRefs"); !ready {
		return false, "HTTPRouteNotAccepted", detail, nil
	}
	return true, "RouteAccepted", "endpoint picker, inference pool, and public route are ready", nil
}

// gatewayParentReady validates the status entry owned by the configured
// Gateway. Looking at an arbitrary True condition can otherwise report a route
// ready because a different Gateway accepted it. Conditions from an older
// object generation are also rejected.
func gatewayParentReady(object *unstructured.Unstructured, gatewayName, gatewayNamespace string, required ...string) (bool, string) {
	if gatewayName == "" {
		return false, "configured Gateway name is empty"
	}
	if gatewayNamespace == "" {
		gatewayNamespace = object.GetNamespace()
	}
	parents, found, err := unstructured.NestedSlice(object.Object, "status", "parents")
	if err != nil || !found {
		return false, "waiting for Gateway parent status"
	}
	for _, rawParent := range parents {
		parent, ok := rawParent.(map[string]any)
		if !ok || !matchingGatewayParent(parent, object.GetNamespace(), gatewayName, gatewayNamespace) {
			continue
		}
		conditions, found, err := unstructured.NestedSlice(parent, "conditions")
		if err != nil || !found {
			return false, "waiting for Gateway parent conditions"
		}
		for _, requiredType := range required {
			condition, found := findUnstructuredCondition(conditions, requiredType)
			if !found {
				return false, fmt.Sprintf("waiting for %s condition", requiredType)
			}
			status, _, _ := unstructured.NestedString(condition, "status")
			if status != "True" {
				reason, _, _ := unstructured.NestedString(condition, "reason")
				if reason == "" {
					reason = "Pending"
				}
				return false, fmt.Sprintf("%s is %s (%s)", requiredType, status, reason)
			}
			observed, hasObserved, _ := unstructured.NestedInt64(condition, "observedGeneration")
			if object.GetGeneration() > 0 && (!hasObserved || observed < object.GetGeneration()) {
				return false, fmt.Sprintf("%s condition is stale", requiredType)
			}
		}
		return true, ""
	}
	return false, "waiting for configured Gateway parent status"
}

func matchingGatewayParent(parent map[string]any, objectNamespace, gatewayName, gatewayNamespace string) bool {
	ref, found, _ := unstructured.NestedMap(parent, "parentRef")
	if !found {
		return false
	}
	name, _, _ := unstructured.NestedString(ref, "name")
	if name != gatewayName {
		return false
	}
	group, _, _ := unstructured.NestedString(ref, "group")
	if group != "" && group != "gateway.networking.k8s.io" {
		return false
	}
	kind, _, _ := unstructured.NestedString(ref, "kind")
	if kind != "" && kind != "Gateway" {
		return false
	}
	namespace, found, _ := unstructured.NestedString(ref, "namespace")
	if !found || namespace == "" {
		namespace = objectNamespace
	}
	return namespace == gatewayNamespace
}

func findUnstructuredCondition(conditions []any, conditionType string) (map[string]any, bool) {
	for _, rawCondition := range conditions {
		condition, ok := rawCondition.(map[string]any)
		if !ok {
			continue
		}
		value, _, _ := unstructured.NestedString(condition, "type")
		if value == conditionType {
			return condition, true
		}
	}
	return nil, false
}
