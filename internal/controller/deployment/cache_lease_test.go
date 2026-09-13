package deployment

import (
	"context"
	"sync"
	"testing"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCacheRepairLeaseElectsOneOwnerAndRejectsLateStaleVerifier(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 8, 22, 15, 0, 0, 0, time.UTC)
	first := cacheLeaseDeployment("tenant-a", "chat-a", "deployment-a")
	second := cacheLeaseDeployment("tenant-b", "chat-b", "deployment-b")
	replicas := int32(2)
	cachePath := "/var/lib/inferscale/models/" + cacheKeyForTest("a")
	workload := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "chat-b-runtime", Namespace: second.Namespace,
			Labels: map[string]string{kubeutil.LabelManagedBy: kubeutil.ManagedByValue, kubeutil.LabelComponent: "model-server"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
				Name: "model", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: cachePath}},
			}}}},
		},
	}
	kubeClient := clientfake.NewClientBuilder().WithScheme(cacheReconcileScheme(t)).
		WithStatusSubresource(&platformv1alpha1.InferenceDeployment{}, &appsv1.Deployment{}).
		WithObjects(first, second, workload).Build()
	reconciler := &Reconciler{Client: kubeClient, Config: Config{ControlNamespace: "inferscale-system"}}
	cacheKey := cacheKeyForTest("a")

	type result struct {
		resource *platformv1alpha1.InferenceDeployment
		role     cacheRepairRole
		epoch    string
		err      error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var workers sync.WaitGroup
	for _, resource := range []*platformv1alpha1.InferenceDeployment{first, second} {
		workers.Add(1)
		go func(resource *platformv1alpha1.InferenceDeployment) {
			defer workers.Done()
			<-start
			role, epoch, err := reconciler.beginCacheRepair(ctx, resource, cacheKey, "", now)
			results <- result{resource: resource, role: role, epoch: epoch, err: err}
		}(resource)
	}
	close(start)
	workers.Wait()
	close(results)

	var owner, follower *platformv1alpha1.InferenceDeployment
	for observed := range results {
		if observed.err != nil {
			t.Fatal(observed.err)
		}
		if observed.epoch != "1" {
			t.Fatalf("initial repair epoch=%q, want 1", observed.epoch)
		}
		switch observed.role {
		case cacheRepairOwner:
			if owner != nil {
				t.Fatal("simultaneous claims elected more than one repair owner")
			}
			owner = observed.resource
		case cacheRepairFollower:
			if follower != nil {
				t.Fatal("simultaneous claims returned more than one follower")
			}
			follower = observed.resource
		default:
			t.Fatalf("simultaneous claim role=%d, want owner or follower", observed.role)
		}
	}
	if owner == nil || follower == nil {
		t.Fatalf("simultaneous claims owner=%v follower=%v", owner, follower)
	}

	if epoch, err := reconciler.completeCacheRepair(ctx, owner, cacheKey, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	} else if epoch != "1" {
		t.Fatalf("completed epoch=%q, want 1", epoch)
	}
	lease := &coordinationv1.Lease{}
	if err := kubeClient.Get(ctx, reconciler.cacheRepairLeaseKey(cacheKey), lease); err != nil {
		t.Fatal(err)
	}
	if lease.Annotations[annotationCacheRepairState] != cacheRepairStateComplete {
		t.Fatalf("completed Lease state=%q", lease.Annotations[annotationCacheRepairState])
	}

	// Reproduce the critical interleaving: the follower read the active Lease,
	// then the owner completed and released before the follower acted. The
	// delayed follower may close admission, but must never reapply the hold.
	freshFollower := &platformv1alpha1.InferenceDeployment{}
	if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: follower.Namespace, Name: follower.Name}, freshFollower); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.invalidateModelCache(ctx, freshFollower, cachePath, cacheKey, "old verifier", cacheRepairFollower, now.Add(1500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	observedWorkload := &appsv1.Deployment{}
	if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: workload.Namespace, Name: workload.Name}, observedWorkload); err != nil {
		t.Fatal(err)
	}
	if observedWorkload.Spec.Replicas == nil || *observedWorkload.Spec.Replicas != replicas || observedWorkload.Annotations[annotationCacheHold] != "" {
		t.Fatalf("delayed follower re-held a completed repair: %#v", observedWorkload)
	}

	// The follower's failed Job came from before epoch 1 was published. Even
	// if its reconcile was delayed until after completion, it cannot reopen the
	// lock and cause a release/re-hold outage flap.
	role, epoch, err := reconciler.beginCacheRepair(ctx, follower, cacheKey, "", now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if role != cacheRepairStaleVerification || epoch != "1" {
		t.Fatalf("late old verifier role=%d epoch=%q, want stale epoch 1", role, epoch)
	}

	// A verifier explicitly created against the completed epoch is new
	// evidence and may atomically advance to the next repair epoch.
	role, epoch, err = reconciler.beginCacheRepair(ctx, follower, cacheKey, "1", now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if role != cacheRepairOwner || epoch != "2" {
		t.Fatalf("new verifier role=%d epoch=%q, want owner epoch 2", role, epoch)
	}
	if err := kubeClient.Get(ctx, reconciler.cacheRepairLeaseKey(cacheKey), lease); err != nil {
		t.Fatal(err)
	}
	if lease.Annotations[annotationCacheRepairState] != cacheRepairStateActive ||
		lease.Annotations[annotationCacheRepairEpoch] != "2" ||
		lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != cacheRepairHolder(follower) {
		t.Fatalf("next repair Lease=%#v", lease)
	}
}

func TestCacheRepairLeaseOwnerDeletionAllowsImmediateTakeover(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 8, 22, 16, 0, 0, 0, time.UTC)
	owner := cacheLeaseDeployment("tenant-a", "chat-a", "deployment-a")
	follower := cacheLeaseDeployment("tenant-b", "chat-b", "deployment-b")
	kubeClient := clientfake.NewClientBuilder().WithScheme(cacheReconcileScheme(t)).WithObjects(owner, follower).Build()
	reconciler := &Reconciler{Client: kubeClient, Config: Config{ControlNamespace: "inferscale-system"}}
	cacheKey := cacheKeyForTest("b")

	if role, epoch, err := reconciler.beginCacheRepair(ctx, owner, cacheKey, "", now); err != nil {
		t.Fatal(err)
	} else if role != cacheRepairOwner || epoch != "1" {
		t.Fatalf("initial role=%d epoch=%q", role, epoch)
	}
	if err := kubeClient.Delete(ctx, owner); err != nil {
		t.Fatal(err)
	}
	role, epoch, err := reconciler.joinCacheRepair(ctx, follower, cacheKey, now.Add(time.Second), false)
	if err != nil {
		t.Fatal(err)
	}
	if role != cacheRepairOwner || epoch != "1" {
		t.Fatalf("takeover role=%d epoch=%q, want owner of same epoch", role, epoch)
	}

	lease := &coordinationv1.Lease{}
	if err := kubeClient.Get(ctx, reconciler.cacheRepairLeaseKey(cacheKey), lease); err != nil {
		t.Fatal(err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != cacheRepairHolder(follower) {
		t.Fatalf("takeover holder=%v, want %q", lease.Spec.HolderIdentity, cacheRepairHolder(follower))
	}
	if lease.Spec.LeaseTransitions == nil || *lease.Spec.LeaseTransitions != 1 {
		t.Fatalf("takeover transitions=%v, want 1", lease.Spec.LeaseTransitions)
	}
}

func TestCacheRepairLeaseExpiredOwnerCanBeReplaced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 8, 22, 17, 0, 0, 0, time.UTC)
	owner := cacheLeaseDeployment("tenant-a", "chat-a", "deployment-a")
	follower := cacheLeaseDeployment("tenant-b", "chat-b", "deployment-b")
	kubeClient := clientfake.NewClientBuilder().WithScheme(cacheReconcileScheme(t)).WithObjects(owner, follower).Build()
	reconciler := &Reconciler{Client: kubeClient, Config: Config{ControlNamespace: "inferscale-system"}}
	cacheKey := cacheKeyForTest("c")

	if _, _, err := reconciler.beginCacheRepair(ctx, owner, cacheKey, "", now); err != nil {
		t.Fatal(err)
	}
	role, epoch, err := reconciler.joinCacheRepair(ctx, follower, cacheKey, now.Add(cacheRepairLeaseDuration+time.Second), false)
	if err != nil {
		t.Fatal(err)
	}
	if role != cacheRepairOwner || epoch != "1" {
		t.Fatalf("expired takeover role=%d epoch=%q", role, epoch)
	}
}

func cacheLeaseDeployment(namespace, name, uid string) *platformv1alpha1.InferenceDeployment {
	return &platformv1alpha1.InferenceDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(uid)},
	}
}

func cacheKeyForTest(hexDigit string) string {
	return "sha256-" + repeatHex(hexDigit, 64)
}
