package deployment

import (
	"context"
	"testing"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	"github.com/inferscale/inferscale/internal/controller/modelcache"
	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	"github.com/inferscale/inferscale/internal/rollout"
	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	"github.com/inferscale/inferscale/internal/runtime/vllm"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCanaryCacheRepairPrecedesReadinessRollbackAndAbort(t *testing.T) {
	for _, abort := range []bool{false, true} {
		name := "readiness loss"
		if abort {
			name = "operator abort"
		}
		t.Run(name, func(t *testing.T) {
			r, resource, prefetch, cacheKey := repairingCanaryFixture(t)
			if abort {
				resource.Annotations[kubeutil.AnnotationRolloutControl] = "abort"
				if err := r.Update(context.Background(), resource); err != nil {
					t.Fatal(err)
				}
			}
			finishCanaryCacheRepair(t, r, resource, prefetch, cacheKey)
			if resource.Status.Rollout.Stage != string(rollout.StagePending) {
				t.Fatalf("repaired canary did not restart readiness/evidence collection: %#v", resource.Status)
			}
			reconcileLocal(t, r, resource)
			if abort && resource.Status.Rollout.Stage != string(rollout.StageFailed) {
				t.Fatalf("abort was lost after shared repair completed: %#v", resource.Status)
			}
			if !abort && resource.Status.Rollout.Stage == string(rollout.StageFailed) {
				t.Fatalf("normal worker restart after repair triggered rollback: %#v", resource.Status)
			}
		})
	}
}

func TestChangedDesiredModelFinishesPriorOwnedCacheRepair(t *testing.T) {
	for _, legacyLease := range []bool{false, true} {
		name := "durable identity"
		if legacyLease {
			name = "migrate prefetch identity"
		}
		t.Run(name, func(t *testing.T) {
			r, resource, prefetch, cacheKey := repairingCanaryFixture(t)
			if legacyLease {
				lease := &coordinationv1.Lease{}
				if err := r.Get(context.Background(), r.cacheRepairLeaseKey(cacheKey), lease); err != nil {
					t.Fatal(err)
				}
				delete(lease.Annotations, annotationCacheRepairModelURI)
				delete(lease.Annotations, annotationCacheRepairRevision)
				if err := r.Update(context.Background(), lease); err != nil {
					t.Fatal(err)
				}
			}
			resource.Spec.Model.Revision = repeatHex("f", 40)
			resource.Generation++
			if err := r.Update(context.Background(), resource); err != nil {
				t.Fatal(err)
			}
			finishCanaryCacheRepair(t, r, resource, prefetch, cacheKey)
			if resource.Status.Cache.Weights != "Cold" || currentConditionTrue(resource.Status.Conditions, conditionModelCached, resource.Generation) {
				t.Fatalf("old repair proof incorrectly verified the newer desired model: %#v", resource.Status)
			}
			newKey := modelcache.CacheKey(resource.Spec.Model.URI, resource.Spec.Model.Revision)
			state, _, err := r.cacheRepairLeaseState(context.Background(), newKey)
			if err != nil || state != cacheRepairMissing {
				t.Fatalf("old repair was confused with the new desired key: state=%d err=%v", state, err)
			}
		})
	}
}

func finishCanaryCacheRepair(t *testing.T, r *Reconciler, resource *platformv1alpha1.InferenceDeployment, prefetch *batchv1.Job, cacheKey string) {
	t.Helper()
	// The first pass must enter repair even though the active candidate has
	// zero available replicas. Another pass recreates its drained prefetch.
	for range 2 {
		reconcileLocal(t, r, resource)
		if resource.Status.Phase != platformv1alpha1.DeploymentPhasePrefetching || resource.Status.Rollout.Stage == string(rollout.StageFailed) {
			t.Fatalf("lifecycle shortcut abandoned an active shared repair: %#v", resource.Status)
		}
	}
	repaired := &batchv1.Job{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(prefetch), repaired); err != nil {
		t.Fatal(err)
	}
	if repaired.Annotations[annotationCacheRepairJob] != cacheKey {
		t.Fatalf("prior cache repair did not recreate its immutable prerequisite: %#v", repaired)
	}
	repaired.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	repaired.Status.CompletionTime = &metav1.Time{Time: r.Now()}
	if err := r.Status().Update(context.Background(), repaired); err != nil {
		t.Fatal(err)
	}
	reconcileLocal(t, r, resource)
	state, _, err := r.cacheRepairLeaseState(context.Background(), cacheKey)
	if err != nil || state != cacheRepairComplete {
		t.Fatalf("owned repair never completed: state=%d err=%v", state, err)
	}
	workloads, err := r.listManagedCacheConsumers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, workload := range workloads.Items {
		if workload.Annotations[annotationCacheHold] == cacheKey || desiredReplicas(&workload) == 0 {
			t.Fatalf("worker remained held after cache repair: %#v", workload)
		}
	}
}

func repairingCanaryFixture(t *testing.T) (*Reconciler, *platformv1alpha1.InferenceDeployment, *batchv1.Job, string) {
	t.Helper()
	resource := validLocalFakeDeployment()
	images := platformruntime.Images{VLLM: "vllm@sha256:" + repeatHex("c", 64), ModelPrefetch: "prefetch@sha256:" + repeatHex("d", 64), EndpointPicker: "epp@sha256:" + repeatHex("e", 64)}
	spec, err := normalizeSpec(resource, platformruntime.BackendVLLM, images)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := platformruntime.RevisionFor(spec)
	if err != nil {
		t.Fatal(err)
	}
	resource.Status.Revision = platformv1alpha1.RevisionStatus{Stable: "prior-stable", Candidate: revision.Name, CandidateID: databaseRevisionID(resource)}
	resource.Status.Rollout = platformv1alpha1.RolloutStatus{Stage: string(rollout.StageCanary5), CandidateWeight: 5}
	resource.Status.Phase = platformv1alpha1.DeploymentPhasePrefetching
	resource.Status.Cache.Weights = cacheStateRepairing
	resource.Status.Runtime.Backend = platformv1alpha1.RuntimeBackendVLLM
	renderer := modelcache.Renderer{Config: modelcache.Config{Image: images.ModelPrefetch, CacheRoot: "/var/lib/inferscale/models"}}
	prefetch, err := renderer.Job(spec, revision)
	if err != nil {
		t.Fatal(err)
	}
	prefetch.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	key := modelcache.CacheKey(spec.ModelURI, spec.ModelRevision)
	objects := []client.Object{resource, prefetch}
	zero := int32(0)
	for _, name := range []string{resource.Status.Revision.Stable, revision.Name} {
		labels := kubeutil.Labels(kubeutil.ResourceName(resource.Name), name, string(platformruntime.BackendVLLM), spec.ModelName, spec.Tenant)
		labels[kubeutil.LabelComponent] = "model-server"
		objects = append(objects, &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: workloadName(name, platformruntime.BackendVLLM), Namespace: resource.Namespace, Labels: labels, Annotations: map[string]string{annotationCacheHold: key, annotationCacheHeldReplicas: "1"}},
			Spec:       appsv1.DeploymentSpec{Replicas: &zero, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "model", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: renderer.Path(spec)}}}}}}},
		})
	}
	scheme := cacheReconcileScheme(t)
	c := cacheApplyCompatibleClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&platformv1alpha1.InferenceDeployment{}, &appsv1.Deployment{}, &batchv1.Job{}).WithObjects(objects...).Build()}
	registry, err := platformruntime.NewRegistry(vllm.New())
	if err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Client: c, Scheme: scheme, Registry: registry, CacheRenderer: renderer, Config: Config{Images: images, ModelCacheRoot: renderer.Config.CacheRoot, GatewayName: "gateway", GatewayNamespace: "gateway-system", ProgressiveRollout: true, MonitoringNamespace: "monitoring", PrometheusURL: "http://prometheus:9090"}, Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
	r.defaults()
	if _, _, err := r.beginCacheRepair(context.Background(), resource, key, "", r.Now()); err != nil {
		t.Fatal(err)
	}
	return r, resource, prefetch, key
}
