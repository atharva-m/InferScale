package runtime

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Backend string

const (
	BackendAuto   Backend = "auto"
	BackendVLLM   Backend = "vllm"
	BackendTRTLLM Backend = "tensorrt-llm"
	ServingPort           = int32(8000)
	MetricsPort           = int32(8000)
)

type SLOTarget struct {
	Percentile int32
	TargetMS   int64
}

// Spec is the controller's normalized deployment contract. Runtime adapters
// deliberately do not import the public CRD package.
type Spec struct {
	DeploymentName string
	Namespace      string
	Tenant         string

	ModelURI      string
	ModelRevision string
	ModelName     string

	RequestedBackend Backend
	ResolvedBackend  Backend
	RuntimeVersion   string
	Precision        string
	Quantization     string
	TensorParallel   int32
	MaxModelLen      int32
	PrefixCaching    bool

	AcceleratorVendor string
	AcceleratorType   string
	AcceleratorCount  int32

	MinReplicas int32
	MaxReplicas int32

	TTFT *SLOTarget
	TPOT *SLOTarget

	MaxConcurrentRequests int32
	MaxQueuedRequests     int32
	PriorityClass         string
	RoutingPolicy         string
	Tracing               bool
}

type Revision struct {
	Name   string
	Digest string
}

// BackendResolution is the immutable backend decision recorded for a serving
// revision. Once persisted, a controller restart must reuse it even if newer
// benchmark profiles have since been approved.
type BackendResolution struct {
	Backend            Backend
	ProfileID          string
	SelectionStatus    string
	RuntimeImageDigest string
}

type Images struct {
	VLLM           string
	TensorRTLLM    string
	TensorRTBuild  string
	ModelPrefetch  string
	EndpointPicker string
}

type RenderContext struct {
	Spec     Spec
	Revision Revision
	Images   Images

	ModelCacheHostPath  string
	ModelPath           string
	EngineCacheHostPath string
	EnginePath          string
	ServiceAccountName  string
	ImagePullSecret     string
	GPUNodeSelector     map[string]string
	KVEventsPort        int32
	OTLPEndpoint        string
}

type Resources struct {
	Prerequisites []client.Object
	Serving       []client.Object
	WorkloadName  string
	ServiceName   string
}

type RuntimeCapabilities struct {
	RequiresModelCache   bool
	PrefixCache          bool
	KVEventReporting     bool
	CacheIsolation       bool
	ContinuousBatching   bool
	PriorityPropagation  bool
	BF16                 bool
	FP8                  bool
	TensorParallelism    bool
	SpeculativeDecoding  bool
	DisaggregatedServing bool
}

type Adapter interface {
	Name() Backend
	Capabilities() RuntimeCapabilities
	Validate(Spec) error
	Render(context.Context, RenderContext) (Resources, error)
}

func ValidateCommon(spec Spec) error {
	if spec.DeploymentName == "" || spec.Namespace == "" {
		return fmt.Errorf("deployment name and namespace are required")
	}
	if spec.ModelURI == "" || spec.ModelRevision == "" {
		return fmt.Errorf("model URI and immutable revision are required")
	}
	if spec.TensorParallel < 1 {
		return fmt.Errorf("tensor parallelism must be positive")
	}
	if spec.AcceleratorCount != spec.TensorParallel {
		return fmt.Errorf("accelerator count %d must equal tensor parallelism %d in v1", spec.AcceleratorCount, spec.TensorParallel)
	}
	if spec.TensorParallel != 1 && spec.TensorParallel != 2 && spec.TensorParallel != 4 {
		return fmt.Errorf("tensor parallelism must be one of 1, 2, or 4")
	}
	if spec.MaxModelLen < 1 {
		return fmt.Errorf("max model length must be positive")
	}
	if spec.MinReplicas < 0 || spec.MaxReplicas < spec.MinReplicas {
		return fmt.Errorf("replica bounds must satisfy 0 <= min <= max")
	}
	return nil
}
