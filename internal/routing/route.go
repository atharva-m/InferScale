package routing

import (
	"fmt"

	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func (r Renderer) RenderRoute(config RouteConfig) (*unstructured.Unstructured, error) {
	if config.DeploymentName == "" || config.Namespace == "" || config.GatewayName == "" {
		return nil, fmt.Errorf("deployment, namespace, and gateway names are required")
	}
	if config.Stable == nil && config.Candidate == nil {
		return nil, fmt.Errorf("at least one route revision is required")
	}
	if config.ShadowPercent < 0 || config.ShadowPercent > 100 {
		return nil, fmt.Errorf("shadow percent must be between 0 and 100")
	}
	if config.Path == "" {
		config.Path = "/v1/deployments/" + config.DeploymentName + "/chat/completions"
	}
	if config.GatewayNamespace == "" {
		config.GatewayNamespace = config.Namespace
	}
	parentRef := map[string]any{
		"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": config.GatewayName,
	}
	if config.GatewayNamespace != config.Namespace {
		parentRef["namespace"] = config.GatewayNamespace
	}
	labels := map[string]string{
		kubeutil.LabelManagedBy:  kubeutil.ManagedByValue,
		kubeutil.LabelName:       "inferscale-inference-route",
		kubeutil.LabelComponent:  "routing",
		kubeutil.LabelDeployment: kubeutil.ResourceName(config.DeploymentName),
	}
	classes := []string{"interactive", "standard", "batch"}
	rules := make([]any, 0, len(classes)+1)
	for _, class := range classes {
		rule, err := routeRule(config, class, true)
		if err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}
	defaultRule, err := routeRule(config, "standard", false)
	if err != nil {
		return nil, err
	}
	rules = append(rules, defaultRule)

	return newUnstructured(
		"gateway.networking.k8s.io/v1", "HTTPRoute", config.Namespace,
		kubeutil.ResourceName(config.DeploymentName, "inference"), labels,
		map[string]any{
			"parentRefs": []any{parentRef},
			"rules":      rules,
		},
	), nil
}

func routeRule(config RouteConfig, priorityClass string, matchClass bool) (map[string]any, error) {
	match := map[string]any{
		"path":   map[string]any{"type": "Exact", "value": config.Path},
		"method": "POST",
	}
	if matchClass {
		match["headers"] = []any{map[string]any{
			"type": "Exact", "name": HeaderPriorityClass, "value": priorityClass,
		}}
	}
	backendRefs := make([]any, 0, 2)
	if config.Stable != nil {
		ref, err := routeBackend(*config.Stable, priorityClass)
		if err != nil {
			return nil, err
		}
		backendRefs = append(backendRefs, ref)
	}
	if config.Candidate != nil {
		ref, err := routeBackend(*config.Candidate, priorityClass)
		if err != nil {
			return nil, err
		}
		backendRefs = append(backendRefs, ref)
	}
	filters := []any{
		map[string]any{
			"type": "URLRewrite",
			"urlRewrite": map[string]any{"path": map[string]any{
				"type": "ReplaceFullPath", "replaceFullPath": "/v1/chat/completions",
			}},
		},
		map[string]any{
			"type":                  "RequestHeaderModifier",
			"requestHeaderModifier": map[string]any{"remove": []any{HeaderPriorityClass}},
		},
	}
	if config.Shadow && config.Candidate != nil && config.ShadowPercent > 0 {
		if config.Candidate.ServiceName == "" {
			return nil, fmt.Errorf("candidate runtime service is required for shadow traffic")
		}
		filters = append(filters, map[string]any{
			"type": "RequestMirror",
			"requestMirror": map[string]any{
				"backendRef": map[string]any{
					"group": "", "kind": "Service", "name": config.Candidate.ServiceName, "port": int64(8000),
				},
				"percent": int64(config.ShadowPercent),
			},
		})
	}
	return map[string]any{
		"matches":     []any{match},
		"filters":     filters,
		"backendRefs": backendRefs,
	}, nil
}

func routeBackend(revision RouteRevision, priorityClass string) (map[string]any, error) {
	objective, err := revision.Names.Objective(priorityClass)
	if err != nil {
		return nil, err
	}
	weight := revision.Weight
	if weight < 0 {
		return nil, fmt.Errorf("route weight cannot be negative")
	}
	return map[string]any{
		"group":  "inference.networking.k8s.io",
		"kind":   "InferencePool",
		"name":   revision.Names.Pool,
		"weight": int64(weight),
		"filters": []any{map[string]any{
			"type": "RequestHeaderModifier",
			"requestHeaderModifier": map[string]any{
				"set": []any{map[string]any{"name": HeaderObjective, "value": objective}},
			},
		}},
	}, nil
}
