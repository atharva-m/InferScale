package deployment

import (
	"context"
	"testing"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	"github.com/inferscale/inferscale/internal/rollout"
	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	runtimefake "github.com/inferscale/inferscale/internal/runtime/fake"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func acceptRoute(t *testing.T, c client.Client, namespace, name, gateway, gatewayNamespace string) {
	t.Helper()
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"})
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, object); err != nil {
		t.Fatal(err)
	}
	object.Object["status"] = routingStatusObject("HTTPRoute", name, object.GetGeneration(), gateway, gatewayNamespace, object.GetGeneration()).Object["status"]
	if err := c.Update(context.Background(), object); err != nil {
		t.Fatal(err)
	}
}

func readyLocalDeployment(t *testing.T) (*Reconciler, *platformv1alpha1.InferenceDeployment) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	resource := validLocalFakeDeployment()
	c := applyCompatibleClient{Client: fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&platformv1alpha1.InferenceDeployment{}, &appsv1.Deployment{}, &batchv1.Job{}).WithObjects(resource).Build()}
	registry, err := platformruntime.NewRegistry(runtimefake.New())
	if err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Client: c, Scheme: scheme, Registry: registry, Config: Config{
		Images:      platformruntime.Images{VLLM: "ghcr.io/inferscale/fake-runtime:0.1.0-dev", EndpointPicker: "ghcr.io/llm-d/endpoint-picker@sha256:" + repeatHex("a", 64)},
		GatewayName: "gateway", GatewayNamespace: "gateway-system", MonitoringNamespace: "monitoring", PrometheusURL: "http://prometheus:9090", ProgressiveRollout: true, ScaleToZero: true,
	}, Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
	for range 4 {
		reconcileLocal(t, r, resource)
		if resource.Status.Phase == platformv1alpha1.DeploymentPhaseReady {
			return r, resource
		}
		workloads := &appsv1.DeploymentList{}
		if err := c.List(context.Background(), workloads); err != nil {
			t.Fatal(err)
		}
		for index := range workloads.Items {
			w := &workloads.Items[index]
			w.Status = appsv1.DeploymentStatus{ObservedGeneration: w.Generation, AvailableReplicas: 1, ReadyReplicas: 1}
			if err := c.Status().Update(context.Background(), w); err != nil {
				t.Fatal(err)
			}
		}
		for _, kind := range []string{"InferencePool", "HTTPRoute"} {
			group := "inference.networking.k8s.io"
			if kind == "HTTPRoute" {
				group = "gateway.networking.k8s.io"
			}
			objects := &unstructured.UnstructuredList{}
			objects.SetGroupVersionKind(schema.GroupVersionKind{Group: group, Version: "v1", Kind: kind + "List"})
			if err := c.List(context.Background(), objects); err != nil {
				t.Fatal(err)
			}
			for index := range objects.Items {
				object := &objects.Items[index]
				object.Object["status"] = routingStatusObject(kind, object.GetName(), object.GetGeneration(), "gateway", "gateway-system", object.GetGeneration()).Object["status"]
				if err := c.Update(context.Background(), object); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	t.Fatalf("initial deployment did not become ready: %#v", resource.Status)
	return nil, nil
}

func reconcileLocal(t *testing.T, r *Reconciler, resource *platformv1alpha1.InferenceDeployment) {
	t.Helper()
	key := client.ObjectKeyFromObject(resource)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(context.Background(), key, resource); err != nil {
		t.Fatal(err)
	}
}

func TestWarmingCandidatePreservesStableAndCanAbort(t *testing.T) {
	t.Parallel()
	r, resource := readyLocalDeployment(t)
	stable := resource.Status.Revision.Stable
	resource.Spec.Runtime.MaxModelLen++
	resource.Generation++
	resource.Annotations[kubeutil.AnnotationRevisionID] = "new-candidate-id"
	if err := r.Update(context.Background(), resource); err != nil {
		t.Fatal(err)
	}
	reconcileLocal(t, r, resource)
	if resource.Status.Phase != platformv1alpha1.DeploymentPhaseUpdating || resource.Status.Revision.Stable != stable || resource.Status.Revision.Candidate == "" {
		t.Fatalf("warming blocked stable admission: %#v", resource.Status)
	}
	candidate := resource.Status.Revision.Candidate
	resource.Annotations[kubeutil.AnnotationRolloutControl] = "abort"
	if err := r.Update(context.Background(), resource); err != nil {
		t.Fatal(err)
	}
	reconcileLocal(t, r, resource)
	if !retainedFailedCandidate(resource, candidate) || resource.Status.Phase != platformv1alpha1.DeploymentPhaseDegraded {
		t.Fatalf("abort while warming not persisted: %#v", resource.Status)
	}
	for range 2 {
		reconcileLocal(t, r, resource)
	}
	workload := &appsv1.Deployment{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: resource.Namespace, Name: workloadName(candidate, platformruntime.BackendVLLM)}, workload); err != nil {
		t.Fatal(err)
	}
	if desiredReplicas(workload) != 0 || workload.Annotations[annotationRetiredAt] == "" {
		t.Fatalf("failed candidate resurrected: %#v", workload.Spec)
	}
}

func TestStableAtZeroKeepsAdmissionAndAutoscalerActive(t *testing.T) {
	t.Parallel()
	r, resource := readyLocalDeployment(t)
	stable := resource.Status.Revision.Stable
	resource.Spec.Scaling.MinReplicas = 0
	resource.Generation++
	if err := r.Update(context.Background(), resource); err != nil {
		t.Fatal(err)
	}
	workload := &appsv1.Deployment{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: resource.Namespace, Name: workloadName(stable, platformruntime.BackendVLLM)}, workload); err != nil {
		t.Fatal(err)
	}
	zero := int32(0)
	workload.Spec.Replicas = &zero
	if err := r.Update(context.Background(), workload); err != nil {
		t.Fatal(err)
	}
	workload.Status = appsv1.DeploymentStatus{ObservedGeneration: workload.Generation}
	if err := r.Status().Update(context.Background(), workload); err != nil {
		t.Fatal(err)
	}
	reconcileLocal(t, r, resource)
	if resource.Status.Phase != platformv1alpha1.DeploymentPhaseReady || resource.Status.Revision.Stable != stable {
		t.Fatalf("idle stable blocked admission: %#v", resource.Status)
	}
	autoscaler := &unstructured.Unstructured{}
	autoscaler.SetGroupVersionKind(schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"})
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: resource.Namespace, Name: kubeutil.ResourceName(stable, "autoscaler")}, autoscaler); err != nil {
		t.Fatal(err)
	}
	minimum, _, _ := unstructured.NestedInt64(autoscaler.Object, "spec", "minReplicaCount")
	if minimum != 0 {
		t.Fatalf("autoscaler minimum=%d", minimum)
	}
}

func TestStartupFailuresPreserveAdmissionToStable(t *testing.T) {
	t.Parallel()
	resource := &platformv1alpha1.InferenceDeployment{Status: platformv1alpha1.InferenceDeploymentStatus{Revision: platformv1alpha1.RevisionStatus{Stable: "stable"}}}
	for _, phase := range []platformv1alpha1.DeploymentPhase{platformv1alpha1.DeploymentPhasePending, platformv1alpha1.DeploymentPhasePrefetching, platformv1alpha1.DeploymentPhaseDeploying} {
		if got := startupPhase(resource, phase); got != platformv1alpha1.DeploymentPhaseUpdating {
			t.Fatalf("startup %s blocked stable with %s", phase, got)
		}
	}
	if startupPhase(resource, platformv1alpha1.DeploymentPhaseFailed) != platformv1alpha1.DeploymentPhaseDegraded {
		t.Fatal("candidate failure blocked stable")
	}
	for _, repairState := range []string{cacheStateRepairing, cacheStateSharedRepair} {
		resource.Status.Cache.Weights = repairState
		if startupPhase(resource, platformv1alpha1.DeploymentPhaseFailed) != platformv1alpha1.DeploymentPhasePrefetching {
			t.Fatal("spec resolution failure reopened admission during cache repair")
		}
	}
}

func TestCandidateReadinessLossRollsBackBeforeCacheWait(t *testing.T) {
	t.Parallel()
	r, resource := readyLocalDeployment(t)
	resource.Spec.Runtime.MaxModelLen++
	resource.Generation++
	resource.Annotations[kubeutil.AnnotationRevisionID] = "candidate-id"
	if err := r.Update(context.Background(), resource); err != nil {
		t.Fatal(err)
	}
	reconcileLocal(t, r, resource)
	resource.Status.Rollout.Stage = string(rollout.StageCanary5)
	resource.Status.Rollout.CandidateWeight = 5
	if err := r.Status().Update(context.Background(), resource); err != nil {
		t.Fatal(err)
	}
	reconcileLocal(t, r, resource)
	if resource.Status.Rollout.Stage != string(rollout.StageFailed) || resource.Status.Rollout.LastRollback == nil {
		t.Fatalf("readiness loss did not persist rollback: %#v", resource.Status)
	}
}

func TestSupersededCandidateCleanupSurvivesPointerReplacement(t *testing.T) {
	t.Parallel()
	r, resource := readyLocalDeployment(t)
	stable := resource.Status.Revision.Stable
	resource.Spec.Runtime.MaxModelLen++
	resource.Generation++
	resource.Annotations[kubeutil.AnnotationRevisionID] = "candidate-one"
	if err := r.Update(context.Background(), resource); err != nil {
		t.Fatal(err)
	}
	reconcileLocal(t, r, resource)
	oldCandidate := resource.Status.Revision.Candidate
	// The API advances again before this candidate ever becomes ready.
	resource.Spec.Runtime.MaxModelLen++
	resource.Generation++
	resource.Annotations[kubeutil.AnnotationRevisionID] = "candidate-two"
	if err := r.Update(context.Background(), resource); err != nil {
		t.Fatal(err)
	}
	reconcileLocal(t, r, resource)
	if resource.Status.Revision.Stable != stable || resource.Status.Revision.Candidate == oldCandidate {
		t.Fatalf("new candidate not reconciled: %#v", resource.Status)
	}
	for _, suffix := range []string{"vllm", "epp"} {
		w := &appsv1.Deployment{}
		if err := r.Get(context.Background(), types.NamespacedName{Namespace: resource.Namespace, Name: kubeutil.ResourceName(oldCandidate, suffix)}, w); err != nil {
			t.Fatal(err)
		}
		if desiredReplicas(w) != 0 || w.Annotations[annotationRetiredAt] == "" {
			t.Fatalf("superseded %s still active", suffix)
		}
	}
	so := &unstructured.Unstructured{}
	so.SetGroupVersionKind(schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"})
	err := r.Get(context.Background(), types.NamespacedName{Namespace: resource.Namespace, Name: kubeutil.ResourceName(oldCandidate, "autoscaler")}, so)
	if client.IgnoreNotFound(err) != nil || err == nil {
		t.Fatalf("superseded autoscaler not deleted: %v", err)
	}
}

func TestReselectedRetiredWorkloadCanStartAgain(t *testing.T) {
	t.Parallel()
	r, resource := readyLocalDeployment(t)
	revision := resource.Status.Revision.Stable
	name := workloadName(revision, platformruntime.BackendVLLM)
	if err := r.retireWorkload(context.Background(), resource.Namespace, revision, name, "previous revision", time.Now()); err != nil {
		t.Fatal(err)
	}
	w := &appsv1.Deployment{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: resource.Namespace, Name: name}, w); err != nil {
		t.Fatal(err)
	}
	if err := r.reactivateWorkload(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if desiredReplicas(w) != 1 || w.Annotations[annotationRetiredAt] != "" {
		t.Fatalf("retired workload cannot reactivate: %#v", w)
	}
	epp := &appsv1.Deployment{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: resource.Namespace, Name: kubeutil.ResourceName(revision, "epp")}, epp); err != nil {
		t.Fatal(err)
	}
	if epp.Annotations[annotationRetiredAt] != "" {
		t.Fatal("reactivated EPP retains cleanup marker")
	}
}

func TestRollbackDecisionIsDurableBeforeRouteMutation(t *testing.T) {
	t.Parallel()
	r, resource := readyLocalDeployment(t)
	resource.Status.Rollout.Stage = string(rollout.StageCanary5)
	resource.Status.Rollout.StageStartedAt = &metav1.Time{Time: time.Unix(1_600_000_000, 0)}
	// Force route rendering to fail after the rollback status write.
	r.Config.GatewayName = ""
	if _, err := r.rollbackCandidate(context.Background(), resource, "failed-candidate", platformruntime.BackendVLLM, "candidate lost readiness", time.Now()); err == nil {
		t.Fatal("expected route error")
	}
	observed := &platformv1alpha1.InferenceDeployment{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(resource), observed); err != nil {
		t.Fatal(err)
	}
	if !retainedFailedCandidate(observed, "failed-candidate") || observed.Status.Rollout.LastRollback == nil {
		t.Fatalf("rollback not durable across interrupted routing: %#v", observed.Status)
	}
}
