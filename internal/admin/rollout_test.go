package admin

import (
	"context"
	"testing"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	"github.com/inferscale/inferscale/internal/deployment"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type rolloutDeploymentLookup struct{ value *deployment.Deployment }

func (r rolloutDeploymentLookup) Get(context.Context, string, string) (*deployment.Deployment, error) {
	return r.value, nil
}

func TestRolloutResumeIsObservableByController(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	resource := &platformv1alpha1.InferenceDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "qwen", Namespace: "tenant-a",
			Annotations: map[string]string{AnnotationRolloutControl: string(RolloutPause)},
		},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(resource).Build()
	admin := RolloutAdmin{
		Deployments: rolloutDeploymentLookup{value: &deployment.Deployment{
			ID: "019c1234-1234-7123-8123-123456789abc", TenantID: "tenant", Name: "qwen", Namespace: "tenant-a",
		}},
		Client: kube,
	}
	if err := admin.Set(context.Background(), "tenant", "deployment", RolloutResume); err != nil {
		t.Fatal(err)
	}
	var observed platformv1alpha1.InferenceDeployment
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(resource), &observed); err != nil {
		t.Fatal(err)
	}
	if observed.Annotations[AnnotationRolloutControl] != string(RolloutResume) {
		t.Fatalf("rollout control = %q, want resume", observed.Annotations[AnnotationRolloutControl])
	}
}
