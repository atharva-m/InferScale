package deployment

import (
	"testing"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNormalizeSpecUsesExplicitNoQuantization(t *testing.T) {
	t.Parallel()
	resource := &platformv1alpha1.InferenceDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "chat", Namespace: "tenant-a"},
		Spec: platformv1alpha1.InferenceDeploymentSpec{
			Model: platformv1alpha1.ModelSpec{URI: "hf://org/model", Revision: "deadbeef"},
			Runtime: platformv1alpha1.RuntimeSpec{
				Backend: platformv1alpha1.RuntimeBackendVLLM, Precision: "bf16",
				TensorParallelism: 1, MaxModelLen: 4096,
			},
			Accelerator: platformv1alpha1.AcceleratorSpec{Vendor: "nvidia", Type: "l40s", Count: 1},
			Scaling:     platformv1alpha1.ScalingSpec{MinReplicas: 0, MaxReplicas: 4},
		},
	}
	spec, err := normalizeSpec(resource, platformruntime.BackendVLLM, platformruntime.Images{VLLM: "runtime@sha256:deadbeef"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Quantization != "none" {
		t.Fatalf("quantization=%q, want none", spec.Quantization)
	}
}

func TestPublicDeploymentIDPrefersSyncAnnotation(t *testing.T) {
	t.Parallel()
	resource := &platformv1alpha1.InferenceDeployment{ObjectMeta: metav1.ObjectMeta{
		Name:        "break-glass-name",
		Annotations: map[string]string{kubeutil.AnnotationDeploymentID: "d83a8c31-4e19-45ac-995b-4df68216dc5c"},
	}}
	if got := publicDeploymentID(resource); got != "d83a8c31-4e19-45ac-995b-4df68216dc5c" {
		t.Fatalf("publicDeploymentID=%q", got)
	}
	resource.Annotations = nil
	if got := publicDeploymentID(resource); got != resource.Name {
		t.Fatalf("break-glass publicDeploymentID=%q, want resource name", got)
	}
}
