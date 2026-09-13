package deployment

import (
	"context"
	"sync"
	"testing"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	"github.com/inferscale/inferscale/internal/controller/modelcache"
	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSharedCacheVerificationAcrossTenants(t *testing.T) {
	r, renderer, consumers, specs, revisions := sharedVerificationFixture(t, "node-a", "node-a")
	ctx := context.Background()
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	// Concurrent reconciles must elect exactly one Job without SSA ownership
	// conflicts over different tenants' immutable Job templates.
	var workers sync.WaitGroup
	errors := make(chan error, len(consumers))
	for index := range consumers {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			_, _, err := r.reconcileModelCache(ctx, consumers[index], renderer, specs[index], revisions[index], now)
			errors <- err
		}(index)
	}
	workers.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("created %d full-hash Jobs for the same node/cache, want one", len(jobs.Items))
	}
	job := &jobs.Items[0]
	if job.Namespace != modelcache.SharedVerificationNamespace || job.Namespace == r.Config.ControlNamespace || job.Spec.Template.Spec.NodeName != "node-a" || len(job.OwnerReferences) != 0 || job.Labels[kubeutil.LabelRevision] != "" {
		t.Fatalf("shared Job is not independent of its tenant consumers: %#v", job)
	}
	job.Status.CompletionTime = &metav1.Time{Time: now}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(now)}}
	if err := r.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	for index := range consumers {
		ready, _, err := r.reconcileModelCache(ctx, consumers[index], renderer, specs[index], revisions[index], now.Add(time.Minute))
		if err != nil || !ready {
			t.Fatalf("consumer %d could not reuse fresh full proof: ready=%v err=%v", index, ready, err)
		}
		condition := kubeutil.FindCondition(consumers[index].Status.Conditions, conditionModelCached)
		if condition == nil || condition.Reason != "CacheVerified" {
			t.Fatalf("consumer %d did not observe verification: %#v", index, condition)
		}
	}
	// Expiry still causes real full verification; sharing must not suppress
	// periodic checks or extend the proof's validity on every observation.
	for index := range consumers {
		if _, _, err := r.reconcileModelCache(ctx, consumers[index], renderer, specs[index], revisions[index], now.Add(cacheVerificationInterval)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.List(ctx, jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 || len(jobs.Items[0].Status.Conditions) != 0 {
		t.Fatalf("expired proof was not replaced by one fresh verification Job: %#v", jobs.Items)
	}
}

func TestSharedCacheVerificationDoesNotReuseOtherNode(t *testing.T) {
	r, renderer, consumers, specs, revisions := sharedVerificationFixture(t, "node-a", "node-b")
	for index := range consumers {
		if _, _, err := r.reconcileModelCache(context.Background(), consumers[index], renderer, specs[index], revisions[index], time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	jobs := &batchv1.JobList{}
	if err := r.List(context.Background(), jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 2 || jobs.Items[0].Spec.Template.Spec.NodeName == jobs.Items[1].Spec.Template.Spec.NodeName {
		t.Fatalf("distinct node-local cache contents reused one proof: %#v", jobs.Items)
	}
}

func TestSharedCacheVerificationFailureStillStartsRepair(t *testing.T) {
	r, renderer, consumers, specs, revisions := sharedVerificationFixture(t, "node-a", "node-a")
	ctx := context.Background()
	now := time.Now().UTC()
	job, err := r.modelCacheVerifier(ctx, renderer, specs[0], revisions[0])
	if err != nil {
		t.Fatal(err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "checksum mismatch"}}
	if err := r.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	for index := range consumers {
		ready, _, err := r.reconcileModelCache(ctx, consumers[index], renderer, specs[index], revisions[index], now)
		if err != nil || ready {
			t.Fatalf("consumer %d accepted failed shared proof: ready=%v err=%v", index, ready, err)
		}
		if consumers[index].Status.Cache.Weights != cacheStateRepairing && consumers[index].Status.Cache.Weights != cacheStateSharedRepair {
			t.Fatalf("consumer %d did not enter repair: %#v", index, consumers[index].Status)
		}
	}
}

func TestExistingFailedVerifierIsConsumedBeforeSharing(t *testing.T) {
	r, renderer, _, specs, revisions := sharedVerificationFixture(t, "node-a", "node-a")
	legacy, err := renderer.VerificationJob(specs[0], revisions[0])
	if err != nil {
		t.Fatal(err)
	}
	legacy.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	if err := r.Create(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	verifier, err := r.modelCacheVerifier(context.Background(), renderer, specs[0], revisions[0])
	if err != nil || client.ObjectKeyFromObject(verifier) != client.ObjectKeyFromObject(legacy) {
		t.Fatalf("migration hid a previous verifier result: job=%#v err=%v", verifier, err)
	}
}

func TestSharedVerifierSurvivesAnotherConsumerRepairRecovery(t *testing.T) {
	r, renderer, consumers, specs, revisions := sharedVerificationFixture(t, "node-a", "node-a")
	ctx := context.Background()
	now := time.Now().UTC()
	key := modelcache.CacheKey(specs[0].ModelURI, specs[0].ModelRevision)
	if _, _, err := r.beginCacheRepair(ctx, consumers[0], key, "", now); err != nil {
		t.Fatal(err)
	}
	epoch, err := r.completeCacheRepair(ctx, consumers[0], key, now)
	if err != nil {
		t.Fatal(err)
	}
	job, err := r.modelCacheVerifier(ctx, renderer, specs[0], revisions[0])
	if err != nil {
		t.Fatal(err)
	}
	job.Annotations = map[string]string{annotationCacheVerificationEpoch: epoch}
	if err := r.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	ready, _, err := r.recoverCompletedCacheRepair(ctx, consumers[1], job, renderer.Path(specs[1]), key, now)
	if err != nil || !ready {
		t.Fatalf("repair follower did not reuse new proof: ready=%v err=%v", ready, err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(job), &batchv1.Job{}); err != nil {
		t.Fatalf("repair follower deleted proof for the completed epoch: %v", err)
	}
}

func TestRepairRecoveryConsumesNewEpochFailureBeforeReleasingWorkers(t *testing.T) {
	r, renderer, consumers, specs, revisions := sharedVerificationFixture(t, "node-a", "node-a")
	ctx := context.Background()
	now := time.Now().UTC()
	key := modelcache.CacheKey(specs[0].ModelURI, specs[0].ModelRevision)
	if _, _, err := r.beginCacheRepair(ctx, consumers[0], key, "", now); err != nil {
		t.Fatal(err)
	}
	epoch, err := r.completeCacheRepair(ctx, consumers[0], key, now)
	if err != nil {
		t.Fatal(err)
	}
	job, err := r.modelCacheVerifier(ctx, renderer, specs[0], revisions[0])
	if err != nil {
		t.Fatal(err)
	}
	job.Annotations = map[string]string{annotationCacheVerificationEpoch: epoch}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "checksum mismatch after repair"}}
	if err := r.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	zero := int32(0)
	worker := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "held-worker", Namespace: consumers[1].Namespace,
			Labels:      map[string]string{kubeutil.LabelManagedBy: kubeutil.ManagedByValue, kubeutil.LabelComponent: "model-server"},
			Annotations: map[string]string{annotationCacheHold: key, annotationCacheHeldReplicas: "2"},
		},
		Spec: appsv1.DeploymentSpec{Replicas: &zero},
	}
	if err := r.Create(ctx, worker); err != nil {
		t.Fatal(err)
	}
	tracking := &cacheReleaseTrackingClient{Client: r.Client}
	r.Client = tracking
	consumers[1].Status.Cache.Weights = cacheStateSharedRepair
	ready, _, err := r.reconcileModelCache(ctx, consumers[1], renderer, specs[1], revisions[1], now)
	if err != nil || ready {
		t.Fatalf("repair recovery accepted new corrupt bytes: ready=%v err=%v", ready, err)
	}
	if consumers[1].Status.Cache.Weights != cacheStateRepairing {
		t.Fatalf("fresh failure did not start another repair: %#v", consumers[1].Status)
	}
	if tracking.releases != 0 {
		t.Fatalf("workers were briefly released %d times despite new checksum failure", tracking.releases)
	}
	_, nextEpoch, err := r.cacheRepairLeaseState(ctx, key)
	if err != nil || nextEpoch == epoch {
		t.Fatalf("fresh failure did not advance repair epoch: epoch=%q err=%v", nextEpoch, err)
	}
}

type cacheReleaseTrackingClient struct {
	client.Client
	releases int
}

func (c *cacheReleaseTrackingClient) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	if workload, ok := object.(*appsv1.Deployment); ok && workload.Spec.Replicas != nil && *workload.Spec.Replicas > 0 {
		c.releases++
	}
	return c.Client.Patch(ctx, object, patch, options...)
}

func TestDelayedVerifierExpiryCannotDeleteNewProof(t *testing.T) {
	r, renderer, _, specs, revisions := sharedVerificationFixture(t, "node-a")
	ctx := context.Background()
	job, err := r.modelCacheVerifier(ctx, renderer, specs[0], revisions[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	stale := job.DeepCopy()
	// A replacement has a newer resource version. The stale consumer's
	// delete must be rejected instead of deleting that new proof by name.
	job.Annotations = map[string]string{annotationCacheVerificationEpoch: "2"}
	if err := r.Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := r.deleteModelCacheVerifier(ctx, stale); err != nil {
		t.Fatal(err)
	}
	retained := &batchv1.Job{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(job), retained); err != nil {
		t.Fatalf("stale consumer deleted new proof: %v", err)
	}
	if retained.Annotations[annotationCacheVerificationEpoch] != "2" {
		t.Fatalf("new proof was modified: %#v", retained)
	}
}

func TestSharedCacheVerificationDoesNotReuseControlNamespaceJob(t *testing.T) {
	r, renderer, consumers, specs, revisions := sharedVerificationFixture(t, "node-a")
	ctx := context.Background()
	legacy, err := renderer.SharedVerificationJob(specs[0], "node-a", r.Config.ControlNamespace)
	if err != nil {
		t.Fatal(err)
	}
	legacy.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "DeadlineExceeded",
		Message: "Job could not create a Pod under control-plane Pod Security enforcement",
	}}
	if err := r.Create(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	_, _, err = r.reconcileModelCache(ctx, consumers[0], renderer, specs[0], revisions[0], time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	created := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: modelcache.SharedVerificationNamespace, Name: legacy.Name}, created); err != nil {
		t.Fatalf("no fresh proof created in dedicated verification namespace: %v", err)
	}
	if len(created.Status.Conditions) != 0 || consumers[0].Status.Cache.Weights != "Warm" {
		t.Fatalf("control-namespace admission failure contaminated new proof: job=%#v cache=%#v", created.Status, consumers[0].Status.Cache)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(legacy), &batchv1.Job{}); err != nil {
		t.Fatalf("old evidence must remain available for explicit migration cleanup: %v", err)
	}
}

func sharedVerificationFixture(t *testing.T, nodeNames ...string) (*Reconciler, modelcache.Renderer, []*platformv1alpha1.InferenceDeployment, []platformruntime.Spec, []platformruntime.Revision) {
	t.Helper()
	renderer := modelcache.Renderer{Config: modelcache.Config{Image: "cache@sha256:abcd", CacheRoot: "/var/lib/inferscale/models"}}
	var consumers []*platformv1alpha1.InferenceDeployment
	var specs []platformruntime.Spec
	var revisions []platformruntime.Revision
	var objects []client.Object
	for index, node := range nodeNames {
		resource := validLocalFakeDeployment()
		resource.Namespace = "tenant-" + string(rune('a'+index))
		resource.Name = "chat-" + string(rune('a'+index))
		resource.UID = types.UID(resource.Name)
		resource.Status.Conditions = []metav1.Condition{{Type: conditionModelCached, Status: metav1.ConditionTrue, ObservedGeneration: resource.Generation, Reason: "CacheComplete"}}
		spec, err := normalizeSpec(resource, platformruntime.BackendVLLM, platformruntime.Images{VLLM: "vllm-image"})
		if err != nil {
			t.Fatal(err)
		}
		revision, err := platformruntime.RevisionFor(spec)
		if err != nil {
			t.Fatal(err)
		}
		pod := cacheTestPod("worker", resource.Namespace, map[string]string{
			kubeutil.LabelManagedBy: kubeutil.ManagedByValue,
			kubeutil.LabelRevision:  revision.Name,
			kubeutil.LabelComponent: "model-server",
		}, renderer.Path(spec))
		pod.Spec.NodeName = node
		consumers = append(consumers, resource)
		specs = append(specs, spec)
		revisions = append(revisions, revision)
		objects = append(objects, resource, pod)
	}
	scheme := cacheReconcileScheme(t)
	kubeClient := clientfake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&platformv1alpha1.InferenceDeployment{}, &batchv1.Job{}).WithObjects(objects...).Build()
	r := &Reconciler{Client: kubeClient, Scheme: scheme, Config: Config{ControlNamespace: "inferscale-system"}}
	r.defaults()
	return r, renderer, consumers, specs, revisions
}
