package v1alpha1

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var dnsLabelRE = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$`)
var commitSHA1RE = regexp.MustCompile(`^[0-9a-f]{40}$`)
var modelURI_RE = regexp.MustCompile(`^hf://[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

type validationErrors []error

func (e validationErrors) Error() string {
	parts := make([]string, 0, len(e))
	for _, err := range e {
		parts = append(parts, err.Error())
	}
	return strings.Join(parts, "; ")
}

func (e validationErrors) Unwrap() []error { return []error(e) }

func (d *InferenceDeployment) Validate() error {
	var errs validationErrors
	if d.Name == "" || len(d.Name) > 63 || !dnsLabelRE.MatchString(d.Name) {
		errs = append(errs, errors.New("metadata.name must be a DNS label of at most 63 characters"))
	}
	errs = append(errs, d.Spec.validate()...)
	if len(errs) > 0 {
		return errs
	}
	return nil
}

func (d *InferenceDeployment) ValidateUpdate(old *InferenceDeployment) error {
	if old == nil {
		return errors.New("old deployment is required")
	}
	if d.Name != old.Name || d.Namespace != old.Namespace {
		return errors.New("metadata.name and metadata.namespace are immutable")
	}
	return d.Validate()
}

func (s InferenceDeploymentSpec) validate() validationErrors {
	var errs validationErrors
	if err := validateModelURI(s.Model.URI); err != nil {
		errs = append(errs, err)
	}
	if !commitSHA1RE.MatchString(s.Model.Revision) {
		errs = append(errs, errors.New("spec.model.revision must be an immutable lowercase 40-hex commit SHA"))
	}
	switch s.Runtime.Backend {
	case RuntimeBackendAuto, RuntimeBackendVLLM, RuntimeBackendTensorRT:
	default:
		errs = append(errs, fmt.Errorf("spec.runtime.backend %q is unsupported", s.Runtime.Backend))
	}
	switch s.Runtime.Precision {
	case "bf16", "fp8":
	default:
		errs = append(errs, fmt.Errorf("spec.runtime.precision %q is unsupported", s.Runtime.Precision))
	}
	switch s.Runtime.Quantization {
	case "", "none", "awq":
	default:
		errs = append(errs, fmt.Errorf("spec.runtime.quantization %q is unsupported", s.Runtime.Quantization))
	}
	if s.Runtime.Precision == "fp8" && s.Runtime.Quantization == "awq" {
		errs = append(errs, errors.New("spec.runtime.precision fp8 cannot be combined with awq quantization"))
	}
	if s.Routing.Policy == RoutingPolicyPrefixAware && !s.Runtime.PrefixCaching.Enabled {
		errs = append(errs, errors.New("spec.routing.policy prefix-aware requires prefix caching"))
	}
	if s.Runtime.Backend == RuntimeBackendTensorRT {
		if s.Routing.Policy == RoutingPolicyPrefixAware {
			errs = append(errs, errors.New("spec.runtime.backend tensorrt-llm does not support prefix-aware routing in v1"))
		}
		if s.Runtime.Quantization != "" && s.Runtime.Quantization != "none" {
			errs = append(errs, errors.New("spec.runtime.backend tensorrt-llm supports only quantization=none in v1"))
		}
		if s.Runtime.Precision != "bf16" {
			errs = append(errs, errors.New("spec.runtime.backend tensorrt-llm supports only bf16 precision in v1"))
		}
	}
	if s.Runtime.TensorParallelism != 1 && s.Runtime.TensorParallelism != 2 && s.Runtime.TensorParallelism != 4 {
		errs = append(errs, errors.New("spec.runtime.tensorParallelism must be 1, 2, or 4"))
	}
	if s.Runtime.MaxModelLen <= 0 {
		errs = append(errs, errors.New("spec.runtime.maxModelLen must be positive"))
	}
	if s.Accelerator.Vendor != "nvidia" {
		errs = append(errs, errors.New("spec.accelerator.vendor must be nvidia in v1"))
	}
	if strings.TrimSpace(s.Accelerator.Type) == "" {
		errs = append(errs, errors.New("spec.accelerator.type is required"))
	}
	if s.Accelerator.Count != 1 && s.Accelerator.Count != 2 && s.Accelerator.Count != 4 {
		errs = append(errs, errors.New("spec.accelerator.count must be 1, 2, or 4"))
	}
	if s.Runtime.TensorParallelism != s.Accelerator.Count {
		errs = append(errs, errors.New("spec.accelerator.count must equal spec.runtime.tensorParallelism in v1"))
	}
	if s.Scaling.MinReplicas < 0 || s.Scaling.MaxReplicas < 1 || s.Scaling.MinReplicas > s.Scaling.MaxReplicas {
		errs = append(errs, errors.New("spec.scaling requires 0 <= minReplicas <= maxReplicas and maxReplicas >= 1"))
	}
	if s.Scaling.Policy != "saturation" {
		errs = append(errs, errors.New("spec.scaling.policy must be saturation in v1"))
	}
	if s.SLO != nil {
		errs = append(errs, validateMetricSLO("ttft", s.SLO.TTFT)...)
		errs = append(errs, validateMetricSLO("tpot", s.SLO.TPOT)...)
	}
	if s.Admission.MaxConcurrentRequests < 1 {
		errs = append(errs, errors.New("spec.admission.maxConcurrentRequests must be positive"))
	}
	if s.Admission.MaxQueuedRequests < 0 {
		errs = append(errs, errors.New("spec.admission.maxQueuedRequests cannot be negative"))
	}
	switch s.Admission.PriorityClass {
	case "interactive", "standard", "batch":
	default:
		errs = append(errs, errors.New("spec.admission.priorityClass must be interactive, standard, or batch"))
	}
	switch s.Routing.Policy {
	case RoutingPolicyRoundRobin, RoutingPolicyLoadAware, RoutingPolicyPrefixAware:
	default:
		errs = append(errs, fmt.Errorf("spec.routing.policy %q is unsupported", s.Routing.Policy))
	}
	if s.Rollout.Strategy != "progressive" {
		errs = append(errs, errors.New("spec.rollout.strategy must be progressive in v1"))
	}
	if s.Rollout.ShadowPercent < 0 || s.Rollout.ShadowPercent > 100 {
		errs = append(errs, errors.New("spec.rollout.shadowPercent must be between 0 and 100"))
	}
	return errs
}

func validateMetricSLO(name string, slo MetricSLO) validationErrors {
	var errs validationErrors
	if slo.Percentile < 1 || slo.Percentile > 100 {
		errs = append(errs, fmt.Errorf("spec.slo.%s.percentile must be between 1 and 100", name))
	}
	if slo.TargetMS <= 0 {
		errs = append(errs, fmt.Errorf("spec.slo.%s.targetMs must be positive", name))
	}
	return errs
}

func validateModelURI(raw string) error {
	if !modelURI_RE.MatchString(raw) {
		return errors.New("spec.model.uri must be hf://<organization>/<model> in v1")
	}
	return nil
}
