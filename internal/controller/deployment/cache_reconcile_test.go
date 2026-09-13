package deployment

import (
	"context"
	"strconv"
	"testing"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	"github.com/inferscale/inferscale/internal/controller/modelcache"
	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	"github.com/inferscale/inferscale/internal/runtime/vllm"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestReconcileInvalidatesReadyCacheAndRecoversWithoutGenerationChange(t *testing.T) {
	for name, verifierMessage := range map[string]string{
		"entry deleted after Ready":   "cache entry sha256-deadbeef is incomplete",
		"entry corrupted after Ready": "cache checksum verification failed",
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
			scheme := cacheReconcileScheme(t)
			resource := validLocalFakeDeployment()
			images := platformruntime.Images{
				VLLM:           "ghcr.io/vllm-project/vllm-openai@sha256:" + repeatHex("c", 64),
				ModelPrefetch:  "ghcr.io/inferscale/modelcache@sha256:" + repeatHex("d", 64),
				EndpointPicker: "ghcr.io/llm-d/endpoint-picker@sha256:" + repeatHex("e", 64),
			}
			spec, err := normalizeSpec(resource, platformruntime.BackendVLLM, images)
			if err != nil {
				t.Fatal(err)
			}
			revision, err := platformruntime.RevisionFor(spec)
			if err != nil {
				t.Fatal(err)
			}
			renderer := modelcache.Renderer{Config: modelcache.Config{
				Image: images.ModelPrefetch, CacheRoot: "/var/lib/inferscale/models",
			}}
			conditionTime := metav1.NewTime(now.Add(-time.Hour))
			resource.Status = platformv1alpha1.InferenceDeploymentStatus{
				Phase:              platformv1alpha1.DeploymentPhaseReady,
				ObservedGeneration: resource.Generation,
				Revision: platformv1alpha1.RevisionStatus{
					Stable: revision.Name, StableID: resource.Annotations[kubeutil.AnnotationRevisionID],
				},
				Runtime:  platformv1alpha1.RuntimeStatus{Backend: platformv1alpha1.RuntimeBackendVLLM},
				Replicas: platformv1alpha1.ReplicaStatus{Desired: 2, Ready: 2},
				Cache:    platformv1alpha1.CacheStatus{Weights: "Warm", WorkersWarm: 2},
				Conditions: []metav1.Condition{
					{Type: conditionModelCached, Status: metav1.ConditionTrue, Reason: "CacheComplete", ObservedGeneration: resource.Generation, LastTransitionTime: conditionTime},
					{Type: conditionRuntimeReady, Status: metav1.ConditionTrue, Reason: "WorkersReady", ObservedGeneration: resource.Generation, LastTransitionTime: conditionTime},
				},
			}

			prefetch, err := renderer.Job(spec, revision)
			if err != nil {
				t.Fatal(err)
			}
			prefetch.Status.Conditions = []batchv1.JobCondition{{
				Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: conditionTime,
			}}
			verifier, err := renderer.VerificationJob(spec, revision)
			if err != nil {
				t.Fatal(err)
			}
			verifier.Status.Conditions = []batchv1.JobCondition{{
				Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: verifierMessage, LastTransitionTime: metav1.NewTime(now),
			}}

			replicas := int32(2)
			labels := kubeutil.Labels(
				kubeutil.ResourceName(spec.DeploymentName), revision.Name, string(spec.ResolvedBackend),
				kubeutil.ResourceName(spec.ModelName), kubeutil.ResourceName(spec.Tenant),
			)
			labels[kubeutil.LabelComponent] = "model-server"
			workload := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: workloadName(revision.Name, platformruntime.BackendVLLM), Namespace: spec.Namespace, Labels: labels},
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
					Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
						Name: "model", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: renderer.Path(spec)}},
					}}}},
				},
				Status: appsv1.DeploymentStatus{Replicas: 2, ReadyReplicas: 2, AvailableReplicas: 2},
			}
			autoscaler := cacheTestAutoscaler(spec.Namespace, revision.Name)
			sharedResource := validLocalFakeDeployment()
			sharedResource.Name = "another-chat"
			sharedResource.Namespace = "another-tenant-namespace"
			sharedResource.UID = types.UID("another-deployment")
			sharedResource.Labels[kubeutil.LabelTenant] = "another-tenant"
			sharedResource.Annotations[kubeutil.AnnotationDeploymentID] = "0198b9b7-22f0-7c6d-8f2e-7d1f9b9f1430"
			sharedResource.Annotations[kubeutil.AnnotationRevisionID] = "0198b9b7-22f0-7c6d-8f2e-7d1f9b9f1431"
			sharedSpec, err := normalizeSpec(sharedResource, platformruntime.BackendVLLM, images)
			if err != nil {
				t.Fatal(err)
			}
			sharedRevision, err := platformruntime.RevisionFor(sharedSpec)
			if err != nil {
				t.Fatal(err)
			}
			sharedResource.Status = platformv1alpha1.InferenceDeploymentStatus{
				Phase:              platformv1alpha1.DeploymentPhaseReady,
				ObservedGeneration: sharedResource.Generation,
				Revision: platformv1alpha1.RevisionStatus{
					Stable: sharedRevision.Name, StableID: sharedResource.Annotations[kubeutil.AnnotationRevisionID],
				},
				Runtime:  platformv1alpha1.RuntimeStatus{Backend: platformv1alpha1.RuntimeBackendVLLM},
				Replicas: platformv1alpha1.ReplicaStatus{Desired: 3, Ready: 3},
				Cache:    platformv1alpha1.CacheStatus{Weights: "Warm", WorkersWarm: 3},
				Conditions: []metav1.Condition{
					{Type: conditionModelCached, Status: metav1.ConditionTrue, Reason: "CacheComplete", ObservedGeneration: sharedResource.Generation, LastTransitionTime: conditionTime},
					{Type: conditionRuntimeReady, Status: metav1.ConditionTrue, Reason: "WorkersReady", ObservedGeneration: sharedResource.Generation, LastTransitionTime: conditionTime},
				},
			}
			sharedReplicas := int32(3)
			sharedLabels := kubeutil.Labels(
				kubeutil.ResourceName(sharedSpec.DeploymentName), sharedRevision.Name, string(sharedSpec.ResolvedBackend),
				kubeutil.ResourceName(sharedSpec.ModelName), kubeutil.ResourceName(sharedSpec.Tenant),
			)
			sharedLabels[kubeutil.LabelComponent] = "model-server"
			sharedWorkload := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      workloadName(sharedRevision.Name, platformruntime.BackendVLLM),
					Namespace: sharedResource.Namespace, Labels: sharedLabels,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: &sharedReplicas,
					Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
						Name: "model", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: renderer.Path(spec)}},
					}}}},
				},
				Status: appsv1.DeploymentStatus{Replicas: 3, ReadyReplicas: 3, AvailableReplicas: 3},
			}
			sharedAutoscaler := cacheTestAutoscaler(sharedWorkload.Namespace, sharedRevision.Name)
			sharedAutoscaler.SetAnnotations(map[string]string{kedaPausedReplicas: "1"})
			pod := cacheTestPod("runtime-a", workload.Namespace, labels, renderer.Path(spec))
			sharedPod := cacheTestPod("runtime-b", sharedWorkload.Namespace, sharedLabels, renderer.Path(spec))
			baseClient := clientfake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&platformv1alpha1.InferenceDeployment{}, &appsv1.Deployment{}, &batchv1.Job{}).
				WithObjects(resource, sharedResource, prefetch, verifier, workload, autoscaler, sharedWorkload, sharedAutoscaler, pod, sharedPod).Build()
			kubeClient := cacheApplyCompatibleClient{Client: baseClient}
			registry, err := platformruntime.NewRegistry(vllm.New())
			if err != nil {
				t.Fatal(err)
			}
			reconciler := &Reconciler{
				Client: kubeClient, Scheme: scheme, Registry: registry, CacheRenderer: renderer,
				Config: Config{
					Images: images, ModelCacheRoot: renderer.Config.CacheRoot,
					GatewayName: "inferscale", GatewayNamespace: "inferscale-gateway",
					MonitoringNamespace: "inferscale-monitoring", PrometheusURL: "http://prometheus:9090",
				},
				Now: func() time.Time { return now },
			}
			key := types.NamespacedName{Namespace: resource.Namespace, Name: resource.Name}
			result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
			if err != nil {
				t.Fatal(err)
			}
			if result.RequeueAfter != 2*time.Second {
				t.Fatalf("cache invalidation requeue=%s, want 2s", result.RequeueAfter)
			}

			observedResource := &platformv1alpha1.InferenceDeployment{}
			if err := kubeClient.Get(context.Background(), key, observedResource); err != nil {
				t.Fatal(err)
			}
			if observedResource.Generation != resource.Generation || observedResource.Status.Phase != platformv1alpha1.DeploymentPhasePrefetching || observedResource.Status.Cache.Weights != "Repairing" {
				t.Fatalf("invalidated deployment status=%#v generation=%d", observedResource.Status, observedResource.Generation)
			}
			cacheCondition := kubeutil.FindCondition(observedResource.Status.Conditions, conditionModelCached)
			if cacheCondition == nil || cacheCondition.Status != metav1.ConditionFalse || cacheCondition.Reason != "CacheVerificationFailed" {
				t.Fatalf("cache condition=%#v", cacheCondition)
			}
			assertCacheConsumerHeld(t, kubeClient, workload, revision.Name, replicas, "")
			assertCacheConsumerHeld(t, kubeClient, sharedWorkload, sharedRevision.Name, sharedReplicas, "1")
			// Draining is a safety barrier: the failed verifier and old prefetch
			// remain while Deployment status or an active exact-path Pod proves a
			// consumer can still retain the quarantined inode.
			for _, job := range []*batchv1.Job{prefetch, verifier} {
				err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(job), &batchv1.Job{})
				if err != nil {
					t.Fatalf("cache prerequisite %s was removed before drain: %v", job.Name, err)
				}
			}

			// Interleave the second deployment's controller after the global hold.
			// It must project itself unavailable and return before workload/KEDA SSA.
			sharedKey := types.NamespacedName{Namespace: sharedResource.Namespace, Name: sharedResource.Name}
			now = now.Add(2 * time.Second)
			if result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: sharedKey}); err != nil {
				t.Fatal(err)
			} else if result.RequeueAfter != 2*time.Second {
				t.Fatalf("shared consumer requeue=%s, want 2s", result.RequeueAfter)
			}
			observedShared := &platformv1alpha1.InferenceDeployment{}
			if err := kubeClient.Get(context.Background(), sharedKey, observedShared); err != nil {
				t.Fatal(err)
			}
			if observedShared.Status.Phase != platformv1alpha1.DeploymentPhasePrefetching || observedShared.Status.Cache.Weights != cacheStateSharedRepair {
				t.Fatalf("shared consumer did not publish unavailable repair status: %#v", observedShared.Status)
			}
			assertCacheConsumerHeld(t, kubeClient, sharedWorkload, sharedRevision.Name, sharedReplicas, "1")

			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatal(err)
			}
			for _, job := range []*batchv1.Job{prefetch, verifier} {
				if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(job), &batchv1.Job{}); err != nil {
					t.Fatalf("cache prerequisite %s was removed with active consumers: %v", job.Name, err)
				}
			}

			for _, desired := range []*appsv1.Deployment{workload, sharedWorkload} {
				observed := &appsv1.Deployment{}
				if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(desired), observed); err != nil {
					t.Fatal(err)
				}
				observed.Status = appsv1.DeploymentStatus{}
				if err := kubeClient.Status().Update(context.Background(), observed); err != nil {
					t.Fatal(err)
				}
			}
			for _, activePod := range []*corev1.Pod{pod, sharedPod} {
				if err := kubeClient.Delete(context.Background(), activePod); err != nil {
					t.Fatal(err)
				}
			}

			// Drain completion first removes the failed verifier, then the stale
			// unmarked prefetch, and only then creates a repair-marked Job.
			for attempt := 0; attempt < 3; attempt++ {
				now = now.Add(2 * time.Second)
				if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
					t.Fatal(err)
				}
			}
			repairedJob := &batchv1.Job{}
			if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(prefetch), repairedJob); err != nil {
				t.Fatalf("get replacement prefetch Job: %v", err)
			}
			cacheKey := modelcache.CacheKey(spec.ModelURI, spec.ModelRevision)
			if repairedJob.Annotations[annotationCacheRepairJob] != cacheKey {
				t.Fatalf("replacement prefetch is not repair-marked: %#v", repairedJob.Annotations)
			}
			if repairedJob.Annotations[annotationCacheRepairEpoch] != "1" {
				t.Fatalf("replacement prefetch epoch=%q, want 1", repairedJob.Annotations[annotationCacheRepairEpoch])
			}
			repairedJob.Status.Conditions = []batchv1.JobCondition{{
				Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(now),
			}}
			completionTime := metav1.NewTime(now)
			repairedJob.Status.CompletionTime = &completionTime
			if err := kubeClient.Status().Update(context.Background(), repairedJob); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Second)
			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatal(err)
			}
			if err := kubeClient.Get(context.Background(), key, observedResource); err != nil {
				t.Fatal(err)
			}
			if observedResource.Generation != resource.Generation || observedResource.Status.Cache.Weights != "Warm" {
				t.Fatalf("cache did not recover without a generation change: generation=%d status=%#v", observedResource.Generation, observedResource.Status)
			}
			cacheCondition = kubeutil.FindCondition(observedResource.Status.Conditions, conditionModelCached)
			if cacheCondition == nil || cacheCondition.Status != metav1.ConditionTrue || cacheCondition.Reason != "CacheComplete" {
				t.Fatalf("recovered cache condition=%#v", cacheCondition)
			}
			assertCacheConsumerReleased(t, kubeClient, workload, revision.Name, replicas, "")
			assertCacheConsumerReleased(t, kubeClient, sharedWorkload, sharedRevision.Name, sharedReplicas, "1")

			// The non-owner can now observe release, discard any stale verifier,
			// and resume from the successfully verified shared entry.
			now = now.Add(time.Second)
			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: sharedKey}); err != nil {
				t.Fatal(err)
			}
			if err := kubeClient.Get(context.Background(), sharedKey, observedShared); err != nil {
				t.Fatal(err)
			}
			if observedShared.Status.Cache.Weights != "Warm" {
				t.Fatalf("shared consumer did not recover: %#v", observedShared.Status)
			}
		})
	}
}

func TestCacheRepairDoesNotReuseCompletedJobFromPriorEpoch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 8, 22, 18, 0, 0, 0, time.UTC)
	scheme := cacheReconcileScheme(t)
	resource := validLocalFakeDeployment()
	images := platformruntime.Images{
		VLLM:          "ghcr.io/vllm-project/vllm-openai@sha256:" + repeatHex("c", 64),
		ModelPrefetch: "ghcr.io/inferscale/modelcache@sha256:" + repeatHex("d", 64),
	}
	spec, err := normalizeSpec(resource, platformruntime.BackendVLLM, images)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := platformruntime.RevisionFor(spec)
	if err != nil {
		t.Fatal(err)
	}
	renderer := modelcache.Renderer{Config: modelcache.Config{
		Image: images.ModelPrefetch, CacheRoot: "/var/lib/inferscale/models",
	}}
	prefetch, err := renderer.Job(spec, revision)
	if err != nil {
		t.Fatal(err)
	}
	prefetch.Annotations = map[string]string{
		annotationCacheRepairJob:   modelcache.CacheKey(spec.ModelURI, spec.ModelRevision),
		annotationCacheRepairEpoch: "1",
	}
	prefetch.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(now.Add(-time.Minute)),
	}}
	baseClient := clientfake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&platformv1alpha1.InferenceDeployment{}, &batchv1.Job{}).
		WithObjects(resource, prefetch).Build()
	kubeClient := cacheApplyCompatibleClient{Client: baseClient}
	reconciler := &Reconciler{
		Client: kubeClient, Scheme: scheme, CacheRenderer: renderer,
		Config:  Config{ControlNamespace: "inferscale-system", Images: images},
		Applier: kubeutil.Applier{Client: kubeClient, Scheme: scheme, FieldOwner: kubeutil.ManagedByValue},
	}
	cacheKey := modelcache.CacheKey(spec.ModelURI, spec.ModelRevision)
	if role, epoch, err := reconciler.beginCacheRepair(ctx, resource, cacheKey, "1", now); err != nil {
		t.Fatal(err)
	} else if role != cacheRepairOwner || epoch != "2" {
		t.Fatalf("begin repair role=%d epoch=%q, want owner epoch 2", role, epoch)
	}
	current := &platformv1alpha1.InferenceDeployment{}
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(resource), current); err != nil {
		t.Fatal(err)
	}
	markCacheRepairOwner(current, now)
	verifier, err := renderer.VerificationJob(spec, revision)
	if err != nil {
		t.Fatal(err)
	}

	if _, result, err := reconciler.reconcileModelCacheRepair(ctx, current, prefetch, verifier, renderer.Path(spec), cacheKey, now); err != nil {
		t.Fatal(err)
	} else if result.RequeueAfter != 2*time.Second {
		t.Fatalf("old epoch cleanup requeue=%s, want 2s", result.RequeueAfter)
	}
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(prefetch), &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("completed epoch-1 Job was reused by epoch 2: %v", err)
	}

	now = now.Add(2 * time.Second)
	replacementDesired, err := renderer.Job(spec, revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, result, err := reconciler.reconcileModelCacheRepair(ctx, current, replacementDesired, verifier, renderer.Path(spec), cacheKey, now); err != nil {
		t.Fatal(err)
	} else if result.RequeueAfter != 10*time.Second {
		t.Fatalf("replacement repair requeue=%s, want 10s", result.RequeueAfter)
	}
	replacement := &batchv1.Job{}
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(replacementDesired), replacement); err != nil {
		t.Fatal(err)
	}
	if replacement.Annotations[annotationCacheRepairJob] != cacheKey || replacement.Annotations[annotationCacheRepairEpoch] != "2" {
		t.Fatalf("replacement repair annotations=%#v", replacement.Annotations)
	}
}

func cacheReconcileScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, gvk := range []schema.GroupVersionKind{
		{Group: "inference.networking.k8s.io", Version: "v1", Kind: "InferencePool"},
		{Group: "llm-d.ai", Version: "v1alpha2", Kind: "InferenceObjective"},
		{Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor"},
		{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"},
		{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"},
	} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		listGVK := gvk
		listGVK.Kind += "List"
		scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	}
	return scheme
}

func cacheTestAutoscaler(namespace, revision string) *unstructured.Unstructured {
	object := cacheAutoscalerObject()
	object.SetName(kubeutil.ResourceName(revision, "autoscaler"))
	object.SetNamespace(namespace)
	object.SetLabels(map[string]string{kubeutil.LabelRevision: revision})
	object.Object["spec"] = map[string]any{"scaleTargetRef": map[string]any{"name": workloadName(revision, platformruntime.BackendVLLM)}}
	return object
}

func cacheTestPod(name, namespace string, labels map[string]string, cachePath string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
			Name: "model", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: cachePath}},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func assertCacheConsumerHeld(
	t *testing.T,
	kubeClient client.Client,
	desired *appsv1.Deployment,
	revision string,
	replicas int32,
	previousKEDAPause string,
) {
	t.Helper()
	workload := &appsv1.Deployment{}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(desired), workload); err != nil {
		t.Fatal(err)
	}
	if workload.Spec.Replicas == nil || *workload.Spec.Replicas != 0 || workload.Annotations[annotationCacheHeldReplicas] != strconv.FormatInt(int64(replicas), 10) || workload.Annotations[annotationCacheHold] == "" {
		t.Fatalf("held workload=%#v", workload)
	}
	autoscaler := cacheAutoscalerObject()
	key := types.NamespacedName{Namespace: desired.Namespace, Name: kubeutil.ResourceName(revision, "autoscaler")}
	if err := kubeClient.Get(context.Background(), key, autoscaler); err != nil {
		t.Fatal(err)
	}
	wantPrevious := previousKEDAPause
	if wantPrevious == "" {
		wantPrevious = cacheNoPreviousKEDAPause
	}
	if autoscaler.GetAnnotations()[kedaPausedReplicas] != "0" ||
		autoscaler.GetAnnotations()[annotationCacheHold] == "" ||
		autoscaler.GetAnnotations()[annotationCachePreviousKEDAPause] != wantPrevious {
		t.Fatalf("held autoscaler annotations=%#v", autoscaler.GetAnnotations())
	}
}

func assertCacheConsumerReleased(
	t *testing.T,
	kubeClient client.Client,
	desired *appsv1.Deployment,
	revision string,
	replicas int32,
	previousKEDAPause string,
) {
	t.Helper()
	workload := &appsv1.Deployment{}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(desired), workload); err != nil {
		t.Fatal(err)
	}
	if workload.Spec.Replicas == nil || *workload.Spec.Replicas != replicas || workload.Annotations[annotationCacheHold] != "" || workload.Annotations[annotationCacheHeldReplicas] != "" {
		t.Fatalf("released workload=%#v", workload)
	}
	autoscaler := cacheAutoscalerObject()
	key := types.NamespacedName{Namespace: desired.Namespace, Name: kubeutil.ResourceName(revision, "autoscaler")}
	if err := kubeClient.Get(context.Background(), key, autoscaler); err != nil {
		t.Fatal(err)
	}
	if autoscaler.GetAnnotations()[kedaPausedReplicas] != previousKEDAPause ||
		autoscaler.GetAnnotations()[annotationCacheHold] != "" ||
		autoscaler.GetAnnotations()[annotationCachePreviousKEDAPause] != "" {
		t.Fatalf("released autoscaler annotations=%#v", autoscaler.GetAnnotations())
	}
}

// This focused fake preserves status and the scale subresource across an SSA
// update, matching apiserver behavior closely enough for the recovery path.
type cacheApplyCompatibleClient struct{ client.Client }

func (c cacheApplyCompatibleClient) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	if patch.Type() != types.ApplyPatchType {
		return c.Client.Patch(ctx, object, patch, options...)
	}
	current := object.DeepCopyObject().(client.Object)
	err := c.Client.Get(ctx, client.ObjectKeyFromObject(object), current)
	if apierrors.IsNotFound(err) {
		return c.Client.Create(ctx, object)
	}
	if err != nil {
		return err
	}
	switch desired := object.(type) {
	case *appsv1.Deployment:
		existing := current.(*appsv1.Deployment)
		if desired.Spec.Replicas == nil {
			desired.Spec.Replicas = existing.Spec.Replicas
		}
		desired.Status = existing.Status
	case *batchv1.Job:
		desired.Status = current.(*batchv1.Job).Status
	}
	object.SetResourceVersion(current.GetResourceVersion())
	return c.Client.Update(ctx, object)
}
