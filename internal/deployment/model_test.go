package deployment

import (
	"testing"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
)

func testSpec() platformv1alpha1.InferenceDeploymentSpec {
	return platformv1alpha1.InferenceDeploymentSpec{
		Model:         platformv1alpha1.ModelSpec{URI: "hf://Qwen/Qwen3-8B", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		Runtime:       platformv1alpha1.RuntimeSpec{Backend: platformv1alpha1.RuntimeBackendAuto, Precision: "bf16", TensorParallelism: 1, MaxModelLen: 8192, PrefixCaching: platformv1alpha1.PrefixCachingSpec{Enabled: true}},
		Accelerator:   platformv1alpha1.AcceleratorSpec{Vendor: "nvidia", Type: "RTX_5090", Count: 1},
		Scaling:       platformv1alpha1.ScalingSpec{MinReplicas: 0, MaxReplicas: 8, Policy: "saturation"},
		Admission:     platformv1alpha1.AdmissionSpec{MaxConcurrentRequests: 32, MaxQueuedRequests: 128, PriorityClass: "standard"},
		Routing:       platformv1alpha1.RoutingSpec{Policy: platformv1alpha1.RoutingPolicyPrefixAware},
		Rollout:       platformv1alpha1.RolloutSpec{Strategy: "progressive", ShadowPercent: 10},
		Observability: platformv1alpha1.ObservabilitySpec{Tracing: true},
	}
}

func TestServingDigestOnlyChangesForServingContract(t *testing.T) {
	base := testSpec()
	operational := base
	operational.Scaling.MaxReplicas = 20
	if ServingDigest(base) != ServingDigest(operational) {
		t.Fatal("scaling-only change created a serving revision")
	}
	serving := base
	serving.Model.Revision = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if ServingDigest(base) == ServingDigest(serving) {
		t.Fatal("model revision change did not create a serving revision")
	}
}

func TestServingDigestIncludesSLOOnlyForBackendAutoSelection(t *testing.T) {
	base := testSpec()
	base.SLO = &platformv1alpha1.SLOSpec{
		TTFT: platformv1alpha1.MetricSLO{Percentile: 95, TargetMS: 500},
		TPOT: platformv1alpha1.MetricSLO{Percentile: 95, TargetMS: 50},
	}
	changed := base
	changedSLO := *base.SLO
	changedSLO.TTFT.TargetMS = 250
	changed.SLO = &changedSLO
	if ServingDigest(base) == ServingDigest(changed) {
		t.Fatal("backend:auto SLO change did not create a selection revision")
	}

	base.Runtime.Backend = platformv1alpha1.RuntimeBackendVLLM
	changed.Runtime.Backend = platformv1alpha1.RuntimeBackendVLLM
	if ServingDigest(base) != ServingDigest(changed) {
		t.Fatal("explicit-backend SLO change created a serving revision")
	}
}

func TestNewCreatesInitialCandidateRevision(t *testing.T) {
	now := time.Unix(10, 0)
	d, revision, err := New(CreateInput{TenantID: "ten_1", Namespace: "tenant-acme", Name: "qwen-chat", Spec: testSpec()}, now)
	if err != nil {
		t.Fatal(err)
	}
	if d.Generation != 1 || revision.Number != 1 || d.CandidateRevisionID != revision.ID {
		t.Fatalf("unexpected deployment/revision: %#v %#v", d, revision)
	}
}
