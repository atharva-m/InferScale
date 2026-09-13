package deployment

import (
	"fmt"
	"strings"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	platformruntime "github.com/inferscale/inferscale/internal/runtime"
)

func normalizeSpec(resource *platformv1alpha1.InferenceDeployment, resolved platformruntime.Backend, images platformruntime.Images) (platformruntime.Spec, error) {
	requested, err := normalizeBackend(resource.Spec.Runtime.Backend)
	if err != nil {
		return platformruntime.Spec{}, err
	}
	if resolved == "" {
		resolved = requested
	}
	tenant := resource.Labels[kubeutil.LabelTenant]
	if tenant == "" {
		tenant = strings.TrimPrefix(resource.Namespace, "tenant-")
	}
	spec := platformruntime.Spec{
		DeploymentName: resource.Name,
		Namespace:      resource.Namespace,
		Tenant:         tenant,
		ModelURI:       resource.Spec.Model.URI,
		ModelRevision:  resource.Spec.Model.Revision,
		ModelName:      modelName(resource.Spec.Model.URI),

		RequestedBackend: requested,
		ResolvedBackend:  resolved,
		Precision:        resource.Spec.Runtime.Precision,
		Quantization:     resource.Spec.Runtime.Quantization,
		TensorParallel:   resource.Spec.Runtime.TensorParallelism,
		MaxModelLen:      resource.Spec.Runtime.MaxModelLen,
		PrefixCaching:    resource.Spec.Runtime.PrefixCaching.Enabled,

		AcceleratorVendor: resource.Spec.Accelerator.Vendor,
		AcceleratorType:   resource.Spec.Accelerator.Type,
		AcceleratorCount:  resource.Spec.Accelerator.Count,
		MinReplicas:       resource.Spec.Scaling.MinReplicas,
		MaxReplicas:       resource.Spec.Scaling.MaxReplicas,

		MaxConcurrentRequests: resource.Spec.Admission.MaxConcurrentRequests,
		MaxQueuedRequests:     resource.Spec.Admission.MaxQueuedRequests,
		PriorityClass:         resource.Spec.Admission.PriorityClass,
		RoutingPolicy:         string(resource.Spec.Routing.Policy),
		Tracing:               resource.Spec.Observability.Tracing,
	}
	if strings.TrimSpace(spec.Quantization) == "" {
		spec.Quantization = "none"
	}
	if resource.Spec.SLO != nil {
		spec.TTFT = &platformruntime.SLOTarget{
			Percentile: resource.Spec.SLO.TTFT.Percentile,
			TargetMS:   resource.Spec.SLO.TTFT.TargetMS,
		}
		spec.TPOT = &platformruntime.SLOTarget{
			Percentile: resource.Spec.SLO.TPOT.Percentile,
			TargetMS:   resource.Spec.SLO.TPOT.TargetMS,
		}
	}
	switch resolved {
	case platformruntime.BackendVLLM:
		spec.RuntimeVersion = images.VLLM
	case platformruntime.BackendTRTLLM:
		spec.RuntimeVersion = images.TensorRTLLM
	}
	return spec, nil
}

func normalizeBackend(value platformv1alpha1.RuntimeBackend) (platformruntime.Backend, error) {
	switch value {
	case platformv1alpha1.RuntimeBackendAuto:
		return platformruntime.BackendAuto, nil
	case platformv1alpha1.RuntimeBackendVLLM:
		return platformruntime.BackendVLLM, nil
	case platformv1alpha1.RuntimeBackendTensorRT:
		return platformruntime.BackendTRTLLM, nil
	default:
		return "", fmt.Errorf("unsupported runtime backend %q", value)
	}
}

func publicBackend(value platformruntime.Backend) platformv1alpha1.RuntimeBackend {
	switch value {
	case platformruntime.BackendTRTLLM:
		return platformv1alpha1.RuntimeBackendTensorRT
	case platformruntime.BackendVLLM:
		return platformv1alpha1.RuntimeBackendVLLM
	default:
		return platformv1alpha1.RuntimeBackendAuto
	}
}

func modelName(uri string) string {
	value := strings.TrimSuffix(uri, "/")
	if slash := strings.LastIndex(value, "/"); slash >= 0 {
		value = value[slash+1:]
	}
	return kubeutil.ResourceName(value)
}
