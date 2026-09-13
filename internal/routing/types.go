package routing

import (
	"fmt"

	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	HeaderPriorityClass = "x-inferscale-priority-class"
	HeaderObjective     = "x-llm-d-inference-objective"
	HeaderFairnessID    = "x-llm-d-inference-fairness-id"
)

type Config struct {
	EndpointPickerImage string
	ImagePullSecret     string
	GatewayName         string
	GatewayNamespace    string
	MonitoringNamespace string
	EndpointPickerPort  int32
	MetricsPort         int32
}

type RevisionNames struct {
	Pool              string
	EndpointPicker    string
	EndpointPickerSvc string
	ConfigMap         string
	ServiceAccount    string
	Interactive       string
	Standard          string
	Batch             string
}

func (n RevisionNames) Objective(priorityClass string) (string, error) {
	switch priorityClass {
	case "interactive":
		return n.Interactive, nil
	case "standard", "":
		return n.Standard, nil
	case "batch":
		return n.Batch, nil
	default:
		return "", fmt.Errorf("unsupported priority class %q", priorityClass)
	}
}

type RevisionResources struct {
	Names   RevisionNames
	Objects []client.Object
}

type RouteRevision struct {
	Revision    platformruntime.Revision
	Names       RevisionNames
	ServiceName string
	Weight      int32
}

type RouteConfig struct {
	DeploymentName   string
	Namespace        string
	GatewayName      string
	GatewayNamespace string
	Path             string
	Stable           *RouteRevision
	Candidate        *RouteRevision
	ShadowPercent    int32
	Shadow           bool
}
