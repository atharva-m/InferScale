package v1alpha1

import (
	"strings"
	"testing"
)

func validDeployment() *InferenceDeployment {
	return &InferenceDeployment{
		Spec: InferenceDeploymentSpec{
			Model:         ModelSpec{URI: "hf://Qwen/Qwen3-8B", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			Runtime:       RuntimeSpec{Backend: RuntimeBackendAuto, Precision: "bf16", TensorParallelism: 1, MaxModelLen: 8192, PrefixCaching: PrefixCachingSpec{Enabled: true}},
			Accelerator:   AcceleratorSpec{Vendor: "nvidia", Type: "RTX_5090", Count: 1},
			Scaling:       ScalingSpec{MinReplicas: 0, MaxReplicas: 8, Policy: "saturation"},
			SLO:           &SLOSpec{TTFT: MetricSLO{Percentile: 95, TargetMS: 750}, TPOT: MetricSLO{Percentile: 95, TargetMS: 50}},
			Admission:     AdmissionSpec{MaxConcurrentRequests: 32, MaxQueuedRequests: 128, PriorityClass: "standard"},
			Routing:       RoutingSpec{Policy: RoutingPolicyPrefixAware},
			Rollout:       RolloutSpec{Strategy: "progressive", ShadowPercent: 10},
			Observability: ObservabilitySpec{Tracing: true},
		},
	}
}

func TestInferenceDeploymentValidate(t *testing.T) {
	d := validDeployment()
	d.Name = "qwen-chat"
	if err := d.Validate(); err != nil {
		t.Fatalf("valid deployment rejected: %v", err)
	}
}

func TestInferenceDeploymentValidateReportsAllErrors(t *testing.T) {
	d := validDeployment()
	d.Name = "Invalid_Name"
	d.Spec.Model.Revision = ""
	d.Spec.Runtime.TensorParallelism = 3
	d.Spec.Scaling.MinReplicas = 9
	d.Spec.Scaling.MaxReplicas = 2
	err := d.Validate()
	if err == nil {
		t.Fatal("invalid deployment accepted")
	}
	for _, want := range []string{"metadata.name", "model.revision", "tensorParallelism", "minReplicas"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("validation error %q does not contain %q", err, want)
		}
	}
}

func TestInferenceDeploymentValidateUpdateRejectsRename(t *testing.T) {
	old := validDeployment()
	old.Name, old.Namespace = "qwen-chat", "tenant-acme"
	next := validDeployment()
	next.Name, next.Namespace = "other", old.Namespace
	if err := next.ValidateUpdate(old); err == nil {
		t.Fatal("rename accepted")
	}
}

func TestInferenceDeploymentValidateMatchesCRDModelURIPattern(t *testing.T) {
	for _, uri := range []string{
		"hf://Qwen/Qwen3-8B/extra",
		"hf://Qwen/Qwen3-8B?revision=main",
		"https://huggingface.co/Qwen/Qwen3-8B",
	} {
		d := validDeployment()
		d.Name = "qwen-chat"
		d.Spec.Model.URI = uri
		if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "model.uri") {
			t.Errorf("model URI %q was not rejected consistently with the CRD: %v", uri, err)
		}
	}
}

func TestInferenceDeploymentRejectsNonCanonicalRevisionCase(t *testing.T) {
	d := validDeployment()
	d.Name = "qwen-chat"
	d.Spec.Model.Revision = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "lowercase") {
		t.Fatalf("uppercase revision was not rejected: %v", err)
	}
}

func TestInferenceDeploymentRejectsUnsupportedRuntimeCapabilityCombinations(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(*InferenceDeployment)
		want   string
	}{
		{
			name: "prefix routing without cache",
			mutate: func(d *InferenceDeployment) {
				d.Spec.Runtime.PrefixCaching.Enabled = false
			},
			want: "requires prefix caching",
		},
		{
			name: "TensorRT prefix routing",
			mutate: func(d *InferenceDeployment) {
				d.Spec.Runtime.Backend = RuntimeBackendTensorRT
			},
			want: "does not support prefix-aware",
		},
		{
			name: "TensorRT AWQ",
			mutate: func(d *InferenceDeployment) {
				d.Spec.Runtime.Backend = RuntimeBackendTensorRT
				d.Spec.Runtime.Quantization = "awq"
				d.Spec.Routing.Policy = RoutingPolicyLoadAware
			},
			want: "only quantization=none",
		},
		{
			name: "TensorRT fp8",
			mutate: func(d *InferenceDeployment) {
				d.Spec.Runtime.Backend = RuntimeBackendTensorRT
				d.Spec.Runtime.Precision = "fp8"
				d.Spec.Routing.Policy = RoutingPolicyLoadAware
			},
			want: "only bf16",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := validDeployment()
			d.Name = "qwen-chat"
			test.mutate(d)
			if err := d.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unsupported combination was not rejected with %q: %v", test.want, err)
			}
		})
	}
}
