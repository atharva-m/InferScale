package routing

import (
	"encoding/json"
	"fmt"
	"strconv"

	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Renderer struct {
	Config Config
}

func (r Renderer) RenderRevision(spec platformruntime.Spec, revision platformruntime.Revision, runtimeService string) (RevisionResources, error) {
	if r.Config.EndpointPickerImage == "" {
		return RevisionResources{}, fmt.Errorf("endpoint picker image is required")
	}
	if runtimeService == "" {
		return RevisionResources{}, fmt.Errorf("runtime service name is required")
	}
	if spec.Tracing && r.Config.OTLPEndpoint == "" {
		return RevisionResources{}, fmt.Errorf("endpoint picker tracing requires an OTLP endpoint")
	}
	switch spec.RoutingPolicy {
	case "", "round-robin", "load-aware":
	case "prefix-aware":
		if spec.ResolvedBackend != platformruntime.BackendVLLM || !spec.PrefixCaching {
			return RevisionResources{}, fmt.Errorf("prefix-aware routing requires a prefix-caching vLLM revision")
		}
	default:
		return RevisionResources{}, fmt.Errorf("unsupported routing policy %q", spec.RoutingPolicy)
	}

	names := Names(revision)
	eppPort := r.Config.EndpointPickerPort
	if eppPort == 0 {
		eppPort = 9002
	}
	metricsPort := r.Config.MetricsPort
	if metricsPort == 0 {
		metricsPort = 9090
	}
	deploymentLabel := kubeutil.ResourceName(spec.DeploymentName)
	workerLabels := kubeutil.Labels(
		deploymentLabel, revision.Name, string(spec.ResolvedBackend),
		kubeutil.ResourceName(spec.ModelName), kubeutil.ResourceName(spec.Tenant),
	)
	workerLabels[kubeutil.LabelComponent] = "model-server"
	eppLabels := map[string]string{
		kubeutil.LabelManagedBy:  kubeutil.ManagedByValue,
		kubeutil.LabelName:       "llm-d-endpoint-picker",
		kubeutil.LabelComponent:  "endpoint-picker",
		kubeutil.LabelDeployment: deploymentLabel,
		kubeutil.LabelRevision:   revision.Name,
		kubeutil.LabelBackend:    string(spec.ResolvedBackend),
		kubeutil.LabelTenant:     kubeutil.ResourceName(spec.Tenant),
	}

	configData, err := endpointPickerConfig(spec, runtimeService)
	if err != nil {
		return RevisionResources{}, err
	}
	configMap := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: names.ConfigMap, Namespace: spec.Namespace, Labels: eppLabels},
		Data:       map[string]string{"endpoint-picker-config.yaml": configData},
	}
	serviceAccount := &corev1.ServiceAccount{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{Name: names.ServiceAccount, Namespace: spec.Namespace, Labels: eppLabels},
	}
	role := &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{Name: names.ServiceAccount, Namespace: spec.Namespace, Labels: eppLabels},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"pods", "services", "endpoints"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"discovery.k8s.io"}, Resources: []string{"endpointslices"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"inference.networking.k8s.io"}, Resources: []string{"inferencepools"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"llm-d.ai"}, Resources: []string{"inferenceobjectives", "inferencemodelrewrites"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"get", "create", "update", "patch"}},
		},
	}
	roleBinding := &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: names.ServiceAccount, Namespace: spec.Namespace, Labels: eppLabels},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: names.ServiceAccount, Namespace: spec.Namespace}},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: names.ServiceAccount},
	}
	eppDeployment := endpointPickerDeployment(spec, names, eppLabels, r.Config, eppPort, metricsPort)
	eppService := &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: names.EndpointPickerSvc, Namespace: spec.Namespace, Labels: eppLabels},
		Spec: corev1.ServiceSpec{
			Selector: eppLabels,
			Ports: []corev1.ServicePort{
				{Name: "grpc", Port: eppPort, TargetPort: intstr.FromString("grpc")},
				{Name: "metrics", Port: metricsPort, TargetPort: intstr.FromString("metrics")},
			},
		},
	}
	inferencePool := newUnstructured(
		"inference.networking.k8s.io/v1", "InferencePool", spec.Namespace, names.Pool, eppLabels,
		map[string]any{
			"selector":    map[string]any{"matchLabels": stringMap(workerLabels)},
			"targetPorts": []any{map[string]any{"number": int64(platformruntime.ServingPort)}},
			"appProtocol": "http",
			"endpointPickerRef": map[string]any{
				"group": "", "kind": "Service", "name": names.EndpointPickerSvc,
				"port": map[string]any{"number": int64(eppPort)}, "failureMode": "FailClose",
			},
		},
	)
	objectives := []client.Object{
		inferenceObjective(spec.Namespace, names.Interactive, names.Pool, 100, eppLabels),
		inferenceObjective(spec.Namespace, names.Standard, names.Pool, 0, eppLabels),
		inferenceObjective(spec.Namespace, names.Batch, names.Pool, -10, eppLabels),
	}
	runtimeMetricsPort := "http"
	if spec.ResolvedBackend == platformruntime.BackendTRTLLM {
		runtimeMetricsPort = "metrics"
	}
	runtimeMonitor := serviceMonitor(spec.Namespace, kubeutil.ResourceName(revision.Name, "runtime"), workerLabels, runtimeMetricsPort, "/metrics")
	eppMonitor := serviceMonitor(spec.Namespace, kubeutil.ResourceName(revision.Name, "epp"), eppLabels, "metrics", "/metrics")
	networkPolicy := runtimeNetworkPolicy(spec.Namespace, revision.Name, workerLabels, r.Config.GatewayNamespace, r.Config.MonitoringNamespace, spec.ResolvedBackend)

	objects := []client.Object{configMap, serviceAccount, role, roleBinding, eppDeployment, eppService, inferencePool}
	objects = append(objects, objectives...)
	objects = append(objects, runtimeMonitor, eppMonitor, networkPolicy)
	return RevisionResources{Names: names, Objects: objects}, nil
}

func endpointPickerConfig(spec platformruntime.Spec, runtimeService string) (string, error) {
	maxConcurrency := spec.MaxConcurrentRequests
	if maxConcurrency < 1 {
		maxConcurrency = 1
	}
	maxQueued := spec.MaxQueuedRequests
	if maxQueued < 1 {
		maxQueued = 1
	}
	requestTTL := "120s"
	if spec.MinReplicas == 0 {
		// At zero, an accepted request is the activator. Preserve it for the
		// documented cold model/engine activation bound; client cancellation
		// still propagates through the full-duplex EPP stream.
		requestTTL = "900s"
	}
	plugins := []any{
		map[string]any{"type": "round-robin-fairness-policy", "name": "tenant-fairness"},
		map[string]any{"type": "fcfs-ordering-policy", "name": "fcfs"},
		map[string]any{
			"type": "concurrency-detector", "name": "saturation",
			"parameters": map[string]any{"maxConcurrency": maxConcurrency, "concurrencyMode": "requests", "headroom": 0.0},
		},
		map[string]any{"type": "metrics-data-source"},
	}
	extractor := map[string]any{"type": "core-metrics-extractor"}
	if spec.ResolvedBackend == platformruntime.BackendTRTLLM {
		// The pinned TRT server exports JSON. Its sidecar exposes these
		// Prometheus gauges on port 9000 for both EPP and Prometheus.
		extractor["parameters"] = map[string]any{
			"defaultEngine": "inferscale-trtllm",
			"engineConfigs": []any{map[string]any{
				"name":                "inferscale-trtllm",
				"queuedRequestsSpec":  "inferscale_trtllm_queued_requests",
				"runningRequestsSpec": "inferscale_trtllm_active_requests",
				"kvUsageSpec":         "inferscale_trtllm_kv_cache_utilization_ratio",
			}},
		}
	}
	plugins = append(plugins, extractor)
	sources := []any{map[string]any{
		"pluginRef": "metrics-data-source", "extractors": []any{map[string]any{"pluginRef": "core-metrics-extractor"}},
	}}
	var profile []any
	switch spec.RoutingPolicy {
	case "", "round-robin":
		// The native picker plugin cycles through the eligible endpoint
		// identities within this EPP process, independent of load scores.
		plugins = append(plugins, map[string]any{"type": "round-robin-picker", "name": "picker"})
	case "load-aware", "prefix-aware":
		plugins = append(plugins, map[string]any{"type": "queue-scorer", "name": "routing-scorer"})
		profile = append(profile, map[string]any{"pluginRef": "routing-scorer"})
		if spec.RoutingPolicy == "prefix-aware" {
			plugins = append(plugins,
				map[string]any{"type": "token-producer", "parameters": map[string]any{
					"modelName": spec.DeploymentName,
					"vllm":      map[string]any{"url": fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", runtimeService, spec.Namespace, platformruntime.ServingPort)},
				}},
				map[string]any{"type": "endpoint-notification-source"},
				map[string]any{"type": "precise-prefix-cache-producer", "parameters": map[string]any{
					"tokenProcessorConfig": map[string]any{"blockSize": 64},
					"kvEventsConfig":       map[string]any{"discoverPods": true, "podDiscoveryConfig": map[string]any{"socketPort": 5557}},
					"indexerConfig":        map[string]any{"kvBlockIndexConfig": map[string]any{"enableMetrics": true}},
				}},
				map[string]any{"type": "prefix-cache-scorer", "parameters": map[string]any{"prefixMatchInfoProducerName": "precise-prefix-cache-producer"}},
				map[string]any{"type": "kv-cache-utilization-scorer"},
			)
			profile = append(profile,
				map[string]any{"pluginRef": "prefix-cache-scorer", "weight": 2},
				map[string]any{"pluginRef": "kv-cache-utilization-scorer"},
			)
			sources = append(sources, map[string]any{
				"pluginRef": "endpoint-notification-source", "extractors": []any{map[string]any{"pluginRef": "precise-prefix-cache-producer"}},
			})
		}
		plugins = append(plugins, map[string]any{"type": "max-score-picker", "name": "picker"})
	}
	profile = append(profile, map[string]any{"pluginRef": "picker"})
	config := map[string]any{
		"apiVersion":         "llm-d.ai/v1alpha1",
		"kind":               "EndpointPickerConfig",
		"featureGates":       []string{"flowControl"},
		"plugins":            plugins,
		"dataLayer":          map[string]any{"injectDefaults": false, "sources": sources},
		"saturationDetector": map[string]any{"pluginRef": "saturation"},
		"flowControl": map[string]any{
			"maxRequests":       strconv.FormatInt(int64(maxQueued), 10),
			"defaultRequestTTL": requestTTL,
			"priorityBands": []any{
				priorityBand(100, maxQueued, "tenant-fairness", "fcfs"),
				priorityBand(0, maxQueued, "tenant-fairness", "fcfs"),
				priorityBand(-10, maxQueued, "tenant-fairness", "fcfs"),
			},
		},
		"schedulingProfiles": []any{map[string]any{
			"name": "default", "plugins": profile,
		}},
	}
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal endpoint picker config: %w", err)
	}
	return string(encoded), nil
}

func priorityBand(priority, maxRequests int32, fairness, ordering string) map[string]any {
	return map[string]any{
		"priority": priority, "maxRequests": strconv.FormatInt(int64(maxRequests), 10),
		"fairnessPolicyRef": fairness, "orderingPolicyRef": ordering,
	}
}

func endpointPickerDeployment(spec platformruntime.Spec, names RevisionNames, labels map[string]string, config Config, eppPort, metricsPort int32) *appsv1.Deployment {
	replicas := int32(1)
	allowPrivilegeEscalation := false
	const healthPort = int32(9003)
	readinessService := "readiness"
	modelMetricsPort := platformruntime.ServingPort
	if spec.ResolvedBackend == platformruntime.BackendTRTLLM {
		modelMetricsPort = 9000
	}
	var environment []corev1.EnvVar
	if spec.Tracing {
		environment = []corev1.EnvVar{
			{Name: "OTEL_TRACES_EXPORTER", Value: "otlp"},
			{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: config.OTLPEndpoint},
			{Name: "OTEL_SERVICE_NAME", Value: "inferscale-epp"},
		}
	}
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: names.EndpointPicker, Namespace: spec.Namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: names.ServiceAccount,
					ImagePullSecrets:   platformruntime.PullSecrets(config.ImagePullSecret),
					Containers: []corev1.Container{{
						Name: "endpoint-picker", Image: config.EndpointPickerImage,
						Args: []string{
							"--config-file=/config/endpoint-picker-config.yaml",
							"--pool-name=" + names.Pool,
							"--pool-namespace=" + spec.Namespace,
							"--grpc-port=" + strconv.FormatInt(int64(eppPort), 10),
							"--metrics-port=" + strconv.FormatInt(int64(metricsPort), 10),
							"--model-server-metrics-port=" + strconv.FormatInt(int64(modelMetricsPort), 10),
							"--grpc-health-port=" + strconv.FormatInt(int64(healthPort), 10),
							"--secure-serving=true",
							// Metrics follow the runtime scrape contract: HTTP on
							// a tenant-isolated port reachable only by monitoring.
							"--metrics-endpoint-auth=false",
							"--enable-pprof=false",
							"--tracing=" + strconv.FormatBool(spec.Tracing),
						},
						Env: environment,
						Ports: []corev1.ContainerPort{
							{Name: "grpc", ContainerPort: eppPort, Protocol: corev1.ProtocolTCP},
							{Name: "metrics", ContainerPort: metricsPort, Protocol: corev1.ProtocolTCP},
							{Name: "health", ContainerPort: healthPort, Protocol: corev1.ProtocolTCP},
						},
						VolumeMounts:    []corev1.VolumeMount{{Name: "config", MountPath: "/config", ReadOnly: true}},
						SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &allowPrivilegeEscalation},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{GRPC: &corev1.GRPCAction{
								Port: healthPort, Service: &readinessService,
							}},
							PeriodSeconds: 5, FailureThreshold: 6,
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: names.ConfigMap},
						}},
					}},
				},
			},
		},
	}
}

func inferenceObjective(namespace, name, pool string, priority int64, labels map[string]string) *unstructured.Unstructured {
	return newUnstructured(
		"llm-d.ai/v1alpha2", "InferenceObjective", namespace, name, labels,
		map[string]any{
			"priority": priority,
			"poolRef": map[string]any{
				"group": "inference.networking.k8s.io", "kind": "InferencePool", "name": pool,
			},
		},
	)
}

func serviceMonitor(namespace, name string, labels map[string]string, port, path string) *unstructured.Unstructured {
	return newUnstructured(
		"monitoring.coreos.com/v1", "ServiceMonitor", namespace, name, labels,
		map[string]any{
			"selector":  map[string]any{"matchLabels": stringMap(labels)},
			"endpoints": []any{map[string]any{"port": port, "path": path, "interval": "15s"}},
		},
	)
}

func runtimeNetworkPolicy(namespace, revision string, labels map[string]string, gatewayNamespace, monitoringNamespace string, backend platformruntime.Backend) *networkingv1.NetworkPolicy {
	from := []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}
	for _, allowedNamespace := range []string{gatewayNamespace, monitoringNamespace} {
		if allowedNamespace == "" || allowedNamespace == namespace {
			continue
		}
		from = append(from, networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
			"kubernetes.io/metadata.name": allowedNamespace,
		}}})
	}
	protocol := corev1.ProtocolTCP
	ingress := []networkingv1.NetworkPolicyIngressRule{{
		From:  from,
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &protocol, Port: &intstr.IntOrString{Type: intstr.Int, IntVal: platformruntime.ServingPort}}},
	}}
	endpointPickerPort := int32(5557)
	if backend == platformruntime.BackendTRTLLM {
		endpointPickerPort = 9000
	}
	// KV event subscriptions and TRT metrics must reach the worker from its
	// own revision EPP. Namespace default-deny would otherwise block them.
	ingress = append(ingress, networkingv1.NetworkPolicyIngressRule{
		From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
			kubeutil.LabelComponent: "endpoint-picker", kubeutil.LabelRevision: revision,
		}}}},
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &protocol, Port: &intstr.IntOrString{Type: intstr.Int, IntVal: endpointPickerPort}}},
	})
	if backend == platformruntime.BackendTRTLLM && monitoringNamespace != "" {
		metricsFrom := []networkingv1.NetworkPolicyPeer{}
		if monitoringNamespace == namespace {
			metricsFrom = append(metricsFrom, networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{}})
		} else {
			metricsFrom = append(metricsFrom, networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
				"kubernetes.io/metadata.name": monitoringNamespace,
			}}})
		}
		ingress = append(ingress, networkingv1.NetworkPolicyIngressRule{
			From:  metricsFrom,
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &protocol, Port: &intstr.IntOrString{Type: intstr.Int, IntVal: 9000}}},
		})
	}
	return &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: kubeutil.ResourceName(revision, "runtime"), Namespace: namespace, Labels: labels},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: labels},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress:     ingress,
		},
	}
}

func newUnstructured(apiVersion, kind, namespace, name string, labels map[string]string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata": map[string]any{
			"name": name, "namespace": namespace, "labels": stringMap(labels),
		},
		"spec": spec,
	}}
}

func stringMap(values map[string]string) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
