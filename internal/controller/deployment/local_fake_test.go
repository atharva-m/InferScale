package deployment

import (
	"context"
	"reflect"
	"testing"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	runtimefake "github.com/inferscale/inferscale/internal/runtime/fake"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestLocalFakeReconcileSkipsPrefetchAndRendersCPUWorker(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	resource := validLocalFakeDeployment()
	baseClient := clientfake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&platformv1alpha1.InferenceDeployment{}, &appsv1.Deployment{}, &batchv1.Job{}).
		WithObjects(resource).
		Build()
	kubeClient := applyCompatibleClient{Client: baseClient}
	registry, err := platformruntime.NewRegistry(runtimefake.New())
	if err != nil {
		t.Fatal(err)
	}
	reconciler := &Reconciler{
		Client: kubeClient, Scheme: scheme, Registry: registry,
		Config: Config{
			Images: platformruntime.Images{
				VLLM:           "ghcr.io/inferscale/fake-runtime:0.1.0-dev",
				EndpointPicker: "ghcr.io/llm-d/endpoint-picker@sha256:" + repeatHex("a", 64),
			},
			GatewayName: "inferscale", GatewayNamespace: "inferscale-gateway",
			MonitoringNamespace: "inferscale-monitoring", PrometheusURL: "http://prometheus:9090",
			OTLPEndpoint: "http://otel:4317",
		},
		Now: func() time.Time { return time.Unix(1_700_000_000, 0) },
	}

	key := types.NamespacedName{Namespace: resource.Namespace, Name: resource.Name}
	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 10*time.Second {
		t.Fatalf("requeue=%s, want runtime readiness wait", result.RequeueAfter)
	}

	jobs := &batchv1.JobList{}
	if err := kubeClient.List(context.Background(), jobs, client.InNamespace(resource.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("local fake reconciliation created model/engine jobs: %#v", jobs.Items)
	}

	requested, err := normalizeSpec(resource, platformruntime.BackendVLLM, reconciler.Config.Images)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := platformruntime.RevisionFor(requested)
	if err != nil {
		t.Fatal(err)
	}
	workload := &appsv1.Deployment{}
	if err := kubeClient.Get(context.Background(), types.NamespacedName{
		Namespace: resource.Namespace, Name: kubeutil.ResourceName(revision.Name, "vllm"),
	}, workload); err != nil {
		t.Fatalf("get fake worker: %v", err)
	}
	container := workload.Spec.Template.Spec.Containers[0]
	if container.Image != reconciler.Config.Images.VLLM || len(container.Resources.Requests) != 0 || len(container.Resources.Limits) != 0 {
		t.Fatalf("fake worker image/resources=%q %#v", container.Image, container.Resources)
	}

	observed := &platformv1alpha1.InferenceDeployment{}
	if err := kubeClient.Get(context.Background(), key, observed); err != nil {
		t.Fatal(err)
	}
	if observed.Status.Cache.Weights != "NotRequired" {
		t.Fatalf("cache status=%q, want NotRequired", observed.Status.Cache.Weights)
	}
	condition := kubeutil.FindCondition(observed.Status.Conditions, conditionModelCached)
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != "NotRequired" {
		t.Fatalf("model-cache condition=%#v", condition)
	}
}

// controller-runtime's in-memory client cannot strategic-merge every typed
// object through server-side apply. This adapter preserves the create/update
// semantics needed by this focused reconcile test; production still uses SSA.
type applyCompatibleClient struct{ client.Client }

func (c applyCompatibleClient) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	if patch.Type() != types.ApplyPatchType {
		return c.Client.Patch(ctx, object, patch, options...)
	}
	current := object.DeepCopyObject().(client.Object)
	err := c.Client.Get(ctx, client.ObjectKeyFromObject(object), current)
	if apierrors.IsNotFound(err) {
		object.SetGeneration(1)
		return c.Client.Create(ctx, object)
	}
	if err != nil {
		return err
	}
	object.SetResourceVersion(current.GetResourceVersion())
	object.SetGeneration(current.GetGeneration())
	annotations := current.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	for key, value := range object.GetAnnotations() {
		annotations[key] = value
	}
	object.SetAnnotations(annotations)
	switch desired := object.(type) {
	case *appsv1.Deployment:
		existing := current.(*appsv1.Deployment)
		if desired.Spec.Replicas == nil {
			desired.Spec.Replicas = existing.Spec.Replicas
		}
		if !reflect.DeepEqual(desired.Spec, existing.Spec) {
			desired.Generation++
		}
	case *unstructured.Unstructured:
		existing := current.(*unstructured.Unstructured)
		if !reflect.DeepEqual(desired.Object["spec"], existing.Object["spec"]) {
			desired.SetGeneration(desired.GetGeneration() + 1)
		}
		if status, found := existing.Object["status"]; found {
			desired.Object["status"] = status
		}
	}
	return c.Client.Update(ctx, object)
}

func validLocalFakeDeployment() *platformv1alpha1.InferenceDeployment {
	return &platformv1alpha1.InferenceDeployment{
		TypeMeta: metav1.TypeMeta{APIVersion: platformv1alpha1.GroupVersion.String(), Kind: "InferenceDeployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "local-chat", Namespace: "tenant-local", UID: types.UID("local-deployment"), Generation: 1,
			Finalizers: []string{finalizerName},
			Labels:     map[string]string{kubeutil.LabelTenant: "local"},
			Annotations: map[string]string{
				kubeutil.AnnotationDeploymentID: "0198b9b7-22f0-7c6d-8f2e-7d1f9b9f1420",
				kubeutil.AnnotationRevisionID:   "0198b9b7-22f0-7c6d-8f2e-7d1f9b9f1421",
			},
		},
		Spec: platformv1alpha1.InferenceDeploymentSpec{
			Model: platformv1alpha1.ModelSpec{
				URI: "hf://Qwen/Qwen3-0.6B", Revision: repeatHex("b", 40),
			},
			Runtime: platformv1alpha1.RuntimeSpec{
				Backend: platformv1alpha1.RuntimeBackendVLLM, Precision: "bf16", Quantization: "none",
				TensorParallelism: 1, MaxModelLen: 2048,
			},
			Accelerator: platformv1alpha1.AcceleratorSpec{Vendor: "nvidia", Type: "local-fake", Count: 1},
			Scaling:     platformv1alpha1.ScalingSpec{MinReplicas: 1, MaxReplicas: 2, Policy: "saturation"},
			Admission: platformv1alpha1.AdmissionSpec{
				MaxConcurrentRequests: 4, MaxQueuedRequests: 8, PriorityClass: "standard",
			},
			Routing:       platformv1alpha1.RoutingSpec{Policy: platformv1alpha1.RoutingPolicyLoadAware},
			Rollout:       platformv1alpha1.RolloutSpec{Strategy: "progressive", ShadowPercent: 10},
			Observability: platformv1alpha1.ObservabilitySpec{Tracing: true},
		},
	}
}

func repeatHex(value string, count int) string {
	result := ""
	for range count {
		result += value
	}
	return result
}
