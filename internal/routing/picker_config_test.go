package routing

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	appsv1 "k8s.io/api/apps/v1"
)

func TestEndpointPickerTracingRequiresAndUsesCollector(t *testing.T) {
	renderer := Renderer{Config: Config{EndpointPickerImage: "epp:test"}}
	spec := platformruntime.Spec{DeploymentName: "chat", Namespace: "tenant-a", Tracing: true}
	revision := platformruntime.Revision{Name: "chat-rev"}
	if _, err := renderer.RenderRevision(spec, revision, "chat-rev-runtime"); err == nil || !strings.Contains(err.Error(), "OTLP endpoint") {
		t.Fatalf("tracing without a collector accepted: %v", err)
	}
	renderer.Config.OTLPEndpoint = "http://otel.monitoring.svc:4317"
	for _, enabled := range []bool{true, false} {
		spec.Tracing = enabled
		resources, err := renderer.RenderRevision(spec, revision, "chat-rev-runtime")
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, object := range resources.Objects {
			workload, ok := object.(*appsv1.Deployment)
			if !ok {
				continue
			}
			found = true
			container := workload.Spec.Template.Spec.Containers[0]
			flag := "--tracing=false"
			if enabled {
				flag = "--tracing=true"
			}
			if !slices.Contains(container.Args, flag) {
				t.Fatalf("tracing is not explicitly configured: %v", container.Args)
			}
			env := map[string]string{}
			for _, entry := range container.Env {
				env[entry.Name] = entry.Value
			}
			if enabled {
				if env["OTEL_TRACES_EXPORTER"] != "otlp" || env["OTEL_EXPORTER_OTLP_ENDPOINT"] != renderer.Config.OTLPEndpoint || env["OTEL_SERVICE_NAME"] != "inferscale-epp" {
					t.Fatalf("traces would miss the shared collector: %v", env)
				}
			} else if len(env) != 0 {
				t.Fatal("disabled tracing retained exporter configuration")
			}
		}
		if !found {
			t.Fatal("missing endpoint picker Deployment")
		}
	}
}

func TestPinnedPickerPluginsAndDataSources(t *testing.T) {
	for _, policy := range []string{"", "round-robin", "load-aware", "prefix-aware"} {
		t.Run(policy, func(t *testing.T) {
			raw, err := endpointPickerConfig(platformruntime.Spec{
				DeploymentName: "chat", Namespace: "tenant-a", RoutingPolicy: policy,
				ResolvedBackend: platformruntime.BackendVLLM, PrefixCaching: true,
			}, "chat-rev-runtime")
			if err != nil {
				t.Fatal(err)
			}
			var config map[string]any
			if err := json.Unmarshal([]byte(raw), &config); err != nil {
				t.Fatal(err)
			}
			plugins := map[string]map[string]any{}
			for _, entry := range config["plugins"].([]any) {
				plugin := entry.(map[string]any)
				name, _ := plugin["name"].(string)
				if name == "" {
					name = plugin["type"].(string)
				}
				plugins[name] = plugin
				if plugin["type"] == "random-picker" || plugin["type"] == "round-robin-scorer" || plugin["type"] == "precise-prefix-cache-scorer" {
					t.Fatalf("unsupported or incomplete legacy plugin: %v", plugin)
				}
			}
			profile := config["schedulingProfiles"].([]any)[0].(map[string]any)["plugins"].([]any)
			for _, entry := range profile {
				if plugins[entry.(map[string]any)["pluginRef"].(string)] == nil {
					t.Fatalf("unresolved profile reference: %v", entry)
				}
			}
			wantPicker := "max-score-picker"
			if policy == "" || policy == "round-robin" {
				wantPicker = "round-robin-picker"
				if len(profile) != 1 || plugins["routing-scorer"] != nil {
					t.Fatalf("round-robin must cycle directly over eligible endpoints without a load scorer: %v", profile)
				}
			}
			if plugins["picker"]["type"] != wantPicker {
				t.Fatalf("picker = %v", plugins["picker"])
			}
			sources := config["dataLayer"].(map[string]any)["sources"].([]any)
			if policy != "prefix-aware" {
				if len(sources) != 1 || plugins["token-producer"] != nil {
					t.Fatal("non-prefix policy configured tokenization/KV indexing")
				}
				return
			}
			if len(sources) != 2 {
				t.Fatal("KV events are not connected to the data layer")
			}
			if sources[1].(map[string]any)["pluginRef"] != "endpoint-notification-source" ||
				sources[1].(map[string]any)["extractors"].([]any)[0].(map[string]any)["pluginRef"] != "precise-prefix-cache-producer" {
				t.Fatalf("invalid subscriber lifecycle wiring: %v", sources)
			}
			tokens := plugins["token-producer"]["parameters"].(map[string]any)
			if tokens["modelName"] != "chat" || tokens["vllm"].(map[string]any)["url"] != "http://chat-rev-runtime.tenant-a.svc.cluster.local:8000" {
				t.Fatalf("tokenization must use this revision's serving contract: %v", tokens)
			}
			if plugins["prefix-cache-scorer"]["parameters"].(map[string]any)["prefixMatchInfoProducerName"] != "precise-prefix-cache-producer" {
				t.Fatal("prefix scorer falls back to approximate tokens")
			}
		})
	}
}

func TestPrefixEventIngressIsRevisionScoped(t *testing.T) {
	policy := runtimeNetworkPolicy("tenant-a", "chat-rev", nil, "gateway", "monitoring", platformruntime.BackendVLLM)
	for _, rule := range policy.Spec.Ingress {
		for _, port := range rule.Ports {
			if port.Port.IntVal != 5557 {
				continue
			}
			if len(rule.From) != 1 || rule.From[0].NamespaceSelector != nil || rule.From[0].PodSelector == nil ||
				rule.From[0].PodSelector.MatchLabels["inferscale.io/revision"] != "chat-rev" ||
				rule.From[0].PodSelector.MatchLabels["app.kubernetes.io/component"] != "endpoint-picker" {
				t.Fatalf("KV event publisher is not limited to revision EPP: %v", rule)
			}
			return
		}
	}
	t.Fatal("KV event publisher is unreachable from EPP")
}
