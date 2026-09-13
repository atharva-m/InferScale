package deployment

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	"github.com/inferscale/inferscale/internal/rollout"
	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRetireWorkloadScalesToZeroAndRemovesAutoscaler(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	replicas := int32(3)
	workload := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "rev-vllm", Namespace: "tenant"},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}
	epp := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "rev-epp", Namespace: "tenant"}}
	autoscaler := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "keda.sh/v1alpha1", "kind": "ScaledObject",
		"metadata": map[string]any{"name": kubeutil.ResourceName("rev", "autoscaler"), "namespace": "tenant"},
		"spec":     map[string]any{"minReplicaCount": int64(2), "maxReplicaCount": int64(4)},
	}}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workload, epp, autoscaler).Build()
	reconciler := Reconciler{Client: client}
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

	if err := reconciler.retireWorkload(context.Background(), "tenant", "rev", "rev-vllm", "automatic rollback", now); err != nil {
		t.Fatal(err)
	}
	observed := &appsv1.Deployment{}
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "tenant", Name: "rev-vllm"}, observed); err != nil {
		t.Fatal(err)
	}
	if observed.Spec.Replicas == nil || *observed.Spec.Replicas != 0 {
		t.Fatalf("retired replicas=%v, want zero", observed.Spec.Replicas)
	}
	if observed.Annotations[annotationRetiredAt] != now.Format(time.RFC3339Nano) || observed.Annotations[annotationRetirementCause] != "automatic rollback" {
		t.Fatalf("retirement evidence=%#v", observed.Annotations)
	}
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "tenant", Name: "rev-epp"}, epp); err != nil {
		t.Fatal(err)
	}
	if epp.Annotations[annotationRetiredAt] != now.Format(time.RFC3339Nano) {
		t.Fatalf("endpoint-picker retention evidence=%#v", epp.Annotations)
	}
	if epp.Spec.Replicas == nil || *epp.Spec.Replicas != 0 {
		t.Fatalf("retired endpoint-picker replicas=%v, want zero", epp.Spec.Replicas)
	}
	deleted := &unstructured.Unstructured{}
	deleted.SetGroupVersionKind(schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"})
	err := client.Get(context.Background(), types.NamespacedName{Namespace: "tenant", Name: kubeutil.ResourceName("rev", "autoscaler")}, deleted)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("retired autoscaler still exists: %v", err)
	}
}

func TestRetainedFailedCandidateIsNotDesiredServingState(t *testing.T) {
	t.Parallel()
	resource := &platformv1alpha1.InferenceDeployment{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{kubeutil.AnnotationRevisionID: "candidate-id"}},
		Status: platformv1alpha1.InferenceDeploymentStatus{
			Revision: platformv1alpha1.RevisionStatus{Stable: "chat-stable", Candidate: "chat-candidate", CandidateID: "candidate-id"},
			Rollout:  platformv1alpha1.RolloutStatus{Stage: string(rollout.StageFailed)},
		},
	}
	if !retainedFailedCandidate(resource, "chat-candidate") {
		t.Fatal("failed candidate should be treated as a retained tombstone")
	}
	if retainedFailedCandidate(resource, "chat-next") {
		t.Fatal("a new revision must not inherit the previous candidate's failed state")
	}
	resource.Status.Rollout.Stage = string(rollout.StageCanary5)
	if retainedFailedCandidate(resource, "chat-candidate") {
		t.Fatal("an active canary must remain desired serving state")
	}
}

func TestExpiredFailedRevisionClearsActiveCandidateAndRetainsTombstoneEvidence(t *testing.T) {
	t.Parallel()
	evidence := &platformv1alpha1.RollbackEvidenceStatus{
		Source: "endpoint-picker", Stage: "Canary25", Reason: "candidate regression",
	}
	resource := &platformv1alpha1.InferenceDeployment{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{kubeutil.AnnotationRevisionID: "candidate-id"}},
		Status: platformv1alpha1.InferenceDeploymentStatus{
			Revision: platformv1alpha1.RevisionStatus{
				Stable: "chat-stable", StableID: "stable-id",
				Candidate: "chat-failed", CandidateID: "candidate-id",
			},
			Rollout: platformv1alpha1.RolloutStatus{
				Stage: string(rollout.StageFailed), CandidateWeight: 25, LastRollback: evidence,
			},
		},
	}

	markExpiredFailedRevision(resource, "chat-failed")
	if resource.Status.Revision.Candidate != "" || resource.Status.Revision.CandidateID != "" ||
		resource.Status.Revision.LastFailed != "chat-failed" || resource.Status.Revision.LastFailedID != "candidate-id" {
		t.Fatalf("expired revision identity=%#v", resource.Status.Revision)
	}
	if resource.Status.Rollout.Stage != string(rollout.StageFailed) ||
		resource.Status.Rollout.CandidateWeight != 0 || resource.Status.Rollout.LastRollback != evidence {
		t.Fatalf("rollback evidence was not retained: %#v", resource.Status.Rollout)
	}
	if !retainedFailedCandidate(resource, "chat-failed") {
		t.Fatal("expired failed revision tombstone no longer blocks workload recreation")
	}
}

func TestRollbackEvidenceIsBoundedAndLabelFree(t *testing.T) {
	t.Parallel()
	start := time.Unix(1_000, 0).UTC()
	now := start.Add(time.Minute)
	reason := strings.Repeat("é", 200)
	evidence := rollbackEvidence(rollout.StageShadow, start, now, reason, rollout.Metrics{
		Source: rollout.MetricsSourceShadowRuntime, Available: true, CandidateRequests: 211,
		CandidateOOMs: 1, CandidateErrorRate: 0.01, CandidateTTFTP95MS: 45,
	}, rollout.DefaultPolicy())
	if evidence.Source != "shadow-runtime" || evidence.Stage != "Shadow" || evidence.CandidateRequests != 211 || evidence.CandidateOOMs != 1 || evidence.CandidateErrorRatePPM != 10_000 || evidence.MaxTTFTRatioPPM != 1_150_000 {
		t.Fatalf("evidence=%#v", evidence)
	}
	if len(evidence.Reason) > 256 || !utf8.ValidString(evidence.Reason) {
		t.Fatalf("bounded reason is invalid: bytes=%d valid=%v", len(evidence.Reason), utf8.ValidString(evidence.Reason))
	}
}

func TestRetainedFailedRevisionKeepsStableServingWithoutRestoringCandidate(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	routeGVK := schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"}
	scheme.AddKnownTypeWithName(routeGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: routeGVK.Group, Version: routeGVK.Version, Kind: routeGVK.Kind + "List"}, &unstructured.UnstructuredList{})

	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	stableReplicas, candidateReplicas, eppReplicas := int32(2), int32(3), int32(1)
	evidence := &platformv1alpha1.RollbackEvidenceStatus{
		Source: "endpoint-picker", Stage: "Canary5", ObservedAt: metav1.NewTime(now), WindowStartedAt: metav1.NewTime(now.Add(-time.Minute)),
		Available: true, CandidateRequests: 210, CandidateOOMs: 1, CandidateErrorRatePPM: 10_000, Reason: "candidate reported an OOM",
	}
	resource := &platformv1alpha1.InferenceDeployment{
		TypeMeta: metav1.TypeMeta{APIVersion: platformv1alpha1.GroupVersion.String(), Kind: "InferenceDeployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "chat", Namespace: "tenant", UID: types.UID("deployment-uid"), Generation: 2,
			Annotations: map[string]string{kubeutil.AnnotationDeploymentID: "deployment-id", kubeutil.AnnotationRevisionID: "candidate-id"},
		},
		Status: platformv1alpha1.InferenceDeploymentStatus{
			Phase:      platformv1alpha1.DeploymentPhaseDegraded,
			Runtime:    platformv1alpha1.RuntimeStatus{Backend: platformv1alpha1.RuntimeBackendVLLM},
			Revision:   platformv1alpha1.RevisionStatus{Stable: "chat-stable", Candidate: "chat-candidate", StableID: "stable-id", CandidateID: "candidate-id"},
			Rollout:    platformv1alpha1.RolloutStatus{Stage: string(rollout.StageFailed), LastRollback: evidence},
			Conditions: []metav1.Condition{{Type: conditionRollout, Status: metav1.ConditionFalse, Reason: "Failed", Message: evidence.Reason, LastTransitionTime: metav1.NewTime(now)}},
		},
	}
	stable := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "chat-stable-vllm", Namespace: "tenant"},
		Spec:       appsv1.DeploymentSpec{Replicas: &stableReplicas},
		Status:     appsv1.DeploymentStatus{Replicas: 2, ReadyReplicas: 2, AvailableReplicas: 2},
	}
	candidate := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "chat-candidate-vllm", Namespace: "tenant"}, Spec: appsv1.DeploymentSpec{Replicas: &candidateReplicas}}
	candidateEPP := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "chat-candidate-epp", Namespace: "tenant"}, Spec: appsv1.DeploymentSpec{Replicas: &eppReplicas}}
	baseClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&platformv1alpha1.InferenceDeployment{}, &appsv1.Deployment{}).
		WithObjects(resource, stable, candidate, candidateEPP,
			&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "candidate-build", Namespace: "tenant", Labels: map[string]string{kubeutil.LabelManagedBy: kubeutil.ManagedByValue, kubeutil.LabelRevision: "chat-candidate"}}},
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "chat-stable-epp", Namespace: "tenant"}, Status: appsv1.DeploymentStatus{AvailableReplicas: 1}},
			routingStatusObject("InferencePool", "chat-stable-pool", 1, "gateway", "gateway-system", 1)).Build()
	kubeClient := applyCompatibleClient{Client: baseClient}
	reconciler := &Reconciler{
		Client: kubeClient, Scheme: scheme,
		Config:  Config{GatewayName: "gateway", GatewayNamespace: "gateway-system"},
		Applier: kubeutil.Applier{Client: kubeClient, Scheme: scheme, FieldOwner: kubeutil.ManagedByValue},
	}
	observedResource := &platformv1alpha1.InferenceDeployment{}
	key := types.NamespacedName{Namespace: "tenant", Name: "chat"}
	if err := kubeClient.Get(context.Background(), key, observedResource); err != nil {
		t.Fatal(err)
	}
	result, err := reconciler.reconcileRetainedFailedRevision(
		context.Background(), observedResource, "chat-candidate", platformruntime.BackendVLLM, now.Add(time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 10*time.Second {
		t.Fatalf("requeue=%s", result.RequeueAfter)
	}
	// Retirement waits for the new route generation; a restart must resume
	// from the durable failed state without recreating the candidate.
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: "tenant", Name: candidate.Name}, candidate); err != nil {
		t.Fatal(err)
	}
	if *candidate.Spec.Replicas != 3 {
		t.Fatal("candidate retired before Gateway accepted stable route")
	}
	acceptRoute(t, kubeClient, "tenant", "chat-inference", "gateway", "gateway-system")
	reconciler = &Reconciler{Client: kubeClient, Scheme: scheme, Config: reconciler.Config, Applier: reconciler.Applier}
	if err := kubeClient.Get(context.Background(), key, observedResource); err != nil {
		t.Fatal(err)
	}
	result, err = reconciler.reconcileRetainedFailedRevision(context.Background(), observedResource, "chat-candidate", platformruntime.BackendVLLM, now.Add(2*time.Minute))
	if err != nil || result.RequeueAfter != 30*time.Second {
		t.Fatalf("resume=%v err=%v", result, err)
	}
	if err := kubeClient.Get(context.Background(), key, observedResource); err != nil {
		t.Fatal(err)
	}
	if observedResource.Status.Phase != platformv1alpha1.DeploymentPhaseDegraded || observedResource.Status.Replicas.Ready != 2 {
		t.Fatalf("stable availability status=%#v", observedResource.Status)
	}
	if observedResource.Status.Revision.Candidate != "chat-candidate" || observedResource.Status.Rollout.LastRollback == nil || observedResource.Status.Rollout.LastRollback.CandidateOOMs != 1 {
		t.Fatalf("rollback evidence was not preserved: %#v", observedResource.Status)
	}
	build := &batchv1.Job{}
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: "tenant", Name: "candidate-build"}, build); err != nil {
		t.Fatal(err)
	}
	if build.Spec.Suspend == nil || !*build.Spec.Suspend {
		t.Fatal("retired candidate build still consumes resources")
	}
	for name, want := range map[string]int32{"chat-stable-vllm": 2, "chat-candidate-vllm": 0, "chat-candidate-epp": 0} {
		deployment := &appsv1.Deployment{}
		if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: "tenant", Name: name}, deployment); err != nil {
			t.Fatal(err)
		}
		if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != want {
			t.Fatalf("%s replicas=%v, want %d", name, deployment.Spec.Replicas, want)
		}
	}
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(routeGVK)
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: "tenant", Name: "chat-inference"}, route); err != nil {
		t.Fatal(err)
	}
	routeText := fmt.Sprint(route.Object)
	if !strings.Contains(routeText, "chat-stable-pool") || strings.Contains(routeText, "chat-candidate-pool") {
		t.Fatalf("rollback route does not exclusively target stable: %s", routeText)
	}
}

func TestPruneExpiredFailedRevisionAfterInspectionWindow(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, gvk := range []schema.GroupVersionKind{
		{Group: "inference.networking.k8s.io", Version: "v1", Kind: "InferencePool"},
		{Group: "llm-d.ai", Version: "v1alpha2", Kind: "InferenceObjective"},
		{Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor"},
		{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"},
	} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		listGVK := gvk
		listGVK.Kind += "List"
		scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	}
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	labels := map[string]string{
		kubeutil.LabelManagedBy: kubeutil.ManagedByValue, kubeutil.LabelDeployment: "chat", kubeutil.LabelRevision: "chat-failed",
	}
	workload := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "chat-failed-vllm", Namespace: "tenant", Labels: labels,
		Annotations: map[string]string{annotationRetiredAt: now.Add(-25 * time.Hour).Format(time.RFC3339Nano)},
	}}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "chat-failed-runtime", Namespace: "tenant", Labels: labels}}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "older-build", Namespace: "tenant",
		Labels:      map[string]string{kubeutil.LabelManagedBy: kubeutil.ManagedByValue, kubeutil.LabelDeployment: "chat", kubeutil.LabelRevision: "older-job-only-revision"},
		Annotations: map[string]string{annotationRetiredAt: now.Add(-25 * time.Hour).Format(time.RFC3339Nano)},
	}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workload, service, job).Build()
	reconciler := Reconciler{Client: kubeClient}
	resource := &platformv1alpha1.InferenceDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "chat", Namespace: "tenant"},
		Status: platformv1alpha1.InferenceDeploymentStatus{
			Revision: platformv1alpha1.RevisionStatus{Candidate: "chat-failed"},
			Rollout:  platformv1alpha1.RolloutStatus{Stage: string(rollout.StageFailed)},
			Conditions: []metav1.Condition{{
				Type: conditionRollout, Status: metav1.ConditionFalse,
				LastTransitionTime: metav1.NewTime(now.Add(-25 * time.Hour)),
			}},
		},
	}

	currentPruned, err := reconciler.pruneExpiredRetiredRevisions(context.Background(), resource, "chat-failed", now)
	if err != nil {
		t.Fatal(err)
	}
	if !currentPruned {
		t.Fatal("expired failed candidate was not identified as pruned")
	}
	for _, object := range []runtime.Object{&appsv1.Deployment{}, &corev1.Service{}, &batchv1.Job{}} {
		key := types.NamespacedName{Namespace: "tenant"}
		switch value := object.(type) {
		case *appsv1.Deployment:
			key.Name = workload.Name
			err = kubeClient.Get(context.Background(), key, value)
		case *corev1.Service:
			key.Name = service.Name
			err = kubeClient.Get(context.Background(), key, value)
		case *batchv1.Job:
			key.Name = job.Name
			err = kubeClient.Get(context.Background(), key, value)
		}
		if !apierrors.IsNotFound(err) {
			t.Fatalf("retired %T still exists: %v", object, err)
		}
	}
}
