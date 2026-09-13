package autoscaling

import (
	"fmt"
	"strconv"

	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	QueueMetric   = "llm_d_epp_flow_control_queue_size"
	RunningMetric = "llm_d_epp_request_running"
)

type Policy struct {
	QueueTarget                   int32
	RunningTarget                 int32
	PollingIntervalSeconds        int32
	CooldownPeriodSeconds         int32
	ScaleDownStabilizationSeconds int32
	ScaleUpStabilizationSeconds   int32
}

func DefaultPolicy(maxConcurrent, maxQueued, maxReplicas int32) Policy {
	if maxReplicas < 1 {
		maxReplicas = 1
	}
	queueTarget := maxQueued / maxReplicas
	if queueTarget < 1 {
		queueTarget = 1
	}
	runningTarget := maxConcurrent / maxReplicas
	if maxConcurrent%maxReplicas != 0 {
		runningTarget++
	}
	if runningTarget < 1 {
		runningTarget = 1
	}
	return Policy{
		QueueTarget: queueTarget, RunningTarget: runningTarget,
		PollingIntervalSeconds: 5, CooldownPeriodSeconds: 300,
		ScaleDownStabilizationSeconds: 600, ScaleUpStabilizationSeconds: 0,
	}
}

type RenderConfig struct {
	Namespace         string
	DeploymentName    string
	Revision          string
	TargetDeployment  string
	InferencePool     string
	EndpointPickerSvc string
	PrometheusURL     string
	MinReplicas       int32
	MaxReplicas       int32
	Policy            Policy
}

type Renderer struct{}

func (Renderer) Render(config RenderConfig) (*unstructured.Unstructured, error) {
	if config.Namespace == "" || config.TargetDeployment == "" || config.InferencePool == "" || config.EndpointPickerSvc == "" {
		return nil, fmt.Errorf("namespace, target deployment, inference pool, and endpoint-picker service are required")
	}
	if config.PrometheusURL == "" {
		return nil, fmt.Errorf("Prometheus URL is required")
	}
	if config.MinReplicas < 0 || config.MaxReplicas < config.MinReplicas || config.MaxReplicas < 1 {
		return nil, fmt.Errorf("replica bounds must satisfy 0 <= min <= max and max >= 1")
	}
	if config.Policy.QueueTarget < 1 || config.Policy.RunningTarget < 1 {
		return nil, fmt.Errorf("queue and running targets must be positive")
	}
	if config.Policy.PollingIntervalSeconds < 1 || config.Policy.CooldownPeriodSeconds < 0 {
		return nil, fmt.Errorf("polling interval must be positive and cooldown non-negative")
	}
	labels := map[string]any{
		kubeutil.LabelManagedBy:  kubeutil.ManagedByValue,
		kubeutil.LabelName:       "inferscale-autoscaler",
		kubeutil.LabelComponent:  "autoscaling",
		kubeutil.LabelDeployment: kubeutil.ResourceName(config.DeploymentName),
		kubeutil.LabelRevision:   config.Revision,
	}
	queueQuery := fmt.Sprintf(
		"sum(%s{namespace=%s,service=%s})",
		QueueMetric, promString(config.Namespace), promString(config.EndpointPickerSvc),
	)
	runningQuery := fmt.Sprintf(
		"sum(%s{namespace=%s,service=%s})",
		RunningMetric, promString(config.Namespace), promString(config.EndpointPickerSvc),
	)
	trigger := func(name, query string, target, activationThreshold int32) map[string]any {
		return map[string]any{
			"type":       "prometheus",
			"metricType": "AverageValue",
			"metadata": map[string]any{
				"serverAddress":       config.PrometheusURL,
				"metricName":          name,
				"query":               query,
				"threshold":           strconv.FormatInt(int64(target), 10),
				"activationThreshold": strconv.FormatInt(int64(activationThreshold), 10),
			},
		}
	}
	fallbackReplicas := config.MinReplicas
	if fallbackReplicas < 1 {
		fallbackReplicas = 1
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "keda.sh/v1alpha1",
		"kind":       "ScaledObject",
		"metadata": map[string]any{
			"name":      kubeutil.ResourceName(config.Revision, "autoscaler"),
			"namespace": config.Namespace,
			"labels":    labels,
		},
		"spec": map[string]any{
			"scaleTargetRef":  map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "name": config.TargetDeployment},
			"pollingInterval": int64(config.Policy.PollingIntervalSeconds),
			"cooldownPeriod":  int64(config.Policy.CooldownPeriodSeconds),
			"minReplicaCount": int64(config.MinReplicas),
			"maxReplicaCount": int64(config.MaxReplicas),
			"fallback": map[string]any{
				"failureThreshold": int64(3),
				"replicas":         int64(fallbackReplicas),
				"behavior":         "static",
			},
			"advanced": map[string]any{
				"restoreToOriginalReplicaCount": false,
				"horizontalPodAutoscalerConfig": map[string]any{
					"behavior": map[string]any{
						"scaleUp": map[string]any{
							"stabilizationWindowSeconds": int64(config.Policy.ScaleUpStabilizationSeconds),
							"selectPolicy":               "Max",
							"policies":                   []any{map[string]any{"type": "Pods", "value": int64(1), "periodSeconds": int64(15)}},
						},
						"scaleDown": map[string]any{
							"stabilizationWindowSeconds": int64(config.Policy.ScaleDownStabilizationSeconds),
							"selectPolicy":               "Min",
							"policies":                   []any{map[string]any{"type": "Pods", "value": int64(1), "periodSeconds": int64(60)}},
						},
					},
				},
			},
			"triggers": []any{
				trigger("inferscale_queue_depth", queueQuery, config.Policy.QueueTarget, 0),
				trigger("inferscale_running_requests", runningQuery, config.Policy.RunningTarget, 0),
			},
		},
	}}, nil
}

func promString(value string) string {
	return strconv.Quote(value)
}
