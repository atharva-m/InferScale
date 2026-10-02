package deployment

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	"github.com/inferscale/inferscale/internal/controller/modelcache"
	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	cacheVerificationInterval = 5 * time.Minute
	cacheRepairRetryInterval  = time.Minute

	annotationCacheHold              = "inferscale.io/cache-hold"
	annotationCacheHeldReplicas      = "inferscale.io/cache-held-replicas"
	annotationCachePreviousKEDAPause = "inferscale.io/cache-previous-keda-pause"
	annotationCacheRepairJob         = "inferscale.io/cache-repair-key"
	cacheNoPreviousKEDAPause         = "<unset>"
	cacheStateRepairing              = "Repairing"
	cacheStateSharedRepair           = "SharedRepair"
)

// reconcileModelCache treats the CR condition as an observation, not as proof
// that an immutable host entry still exists. A completed prefetch establishes
// the first proof; thereafter a short-lived, read-only Job periodically hashes
// every manifest entry. Failed verification withdraws admission before any
// worker is held and forces the normal quarantine/refetch path to run again.
func (r *Reconciler) reconcileModelCache(
	ctx context.Context,
	resource *platformv1alpha1.InferenceDeployment,
	renderer modelcache.Renderer,
	spec platformruntime.Spec,
	revision platformruntime.Revision,
	now time.Time,
) (bool, ctrl.Result, error) {
	prefetch, err := renderer.Job(spec, revision)
	if err != nil {
		return false, ctrl.Result{}, fmt.Errorf("render model prefetch Job: %w", err)
	}
	cachePath := renderer.Path(spec)
	cacheKey := modelcache.CacheKey(spec.ModelURI, spec.ModelRevision)
	verifier, err := r.modelCacheVerifier(ctx, renderer, spec, revision)
	if err != nil {
		return false, ctrl.Result{}, fmt.Errorf("render model-cache verification Job: %w", err)
	}
	held, err := r.cacheConsumersHeld(ctx, cacheKey)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	leaseState, _, err := r.cacheRepairLeaseState(ctx, cacheKey)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	repairStatus := resource.Status.Cache.Weights == cacheStateRepairing || resource.Status.Cache.Weights == cacheStateSharedRepair
	if repairStatus || held || leaseState == cacheRepairFollower {
		role, _, err := r.joinCacheRepair(ctx, resource, cacheKey, now, repairStatus || held)
		if err != nil {
			return false, ctrl.Result{}, err
		}
		switch role {
		case cacheRepairOwner:
			markCacheRepairOwner(resource, now)
			return r.reconcileModelCacheRepair(ctx, resource, prefetch, verifier, cachePath, cacheKey, now)
		case cacheRepairFollower:
			markSharedCacheRepair(resource, now)
			// The owner alone mutates the cluster-wide workload/KEDA hold. A
			// follower closes its own admission projection and returns before SSA;
			// this prevents a follower that read the active Lease just before the
			// owner publishes completion from re-holding already released workers.
			if err := r.persistStatus(ctx, resource, now); err != nil {
				return false, ctrl.Result{}, err
			}
			return false, ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		case cacheRepairComplete:
			if repairStatus {
				return r.recoverCompletedCacheRepair(ctx, resource, verifier, cachePath, cacheKey, now)
			}
			if err := r.releaseCacheConsumers(ctx, cacheKey); err != nil {
				return false, ctrl.Result{}, fmt.Errorf("release workers after completed model-cache repair: %w", err)
			}
		case cacheRepairMissing:
			// No repair exists and this deployment is not held. Continue through
			// the normal cache prerequisite and verifier path below.
		}
	}
	cacheReady := currentConditionTrue(resource.Status.Conditions, conditionModelCached, resource.Generation)
	if !cacheReady {
		observed := &batchv1.Job{}
		err := r.Get(ctx, client.ObjectKeyFromObject(prefetch), observed)
		if apierrors.IsNotFound(err) {
			if err := r.Applier.Apply(ctx, resource, prefetch); err != nil {
				return false, ctrl.Result{}, err
			}
			resource.Status.Cache.Weights = "Cold"
			result, err := r.wait(ctx, resource, platformv1alpha1.DeploymentPhasePrefetching, conditionModelCached, "Prefetching", "waiting for immutable model cache", now, 10*time.Second)
			return false, result, err
		}
		if err != nil {
			return false, ctrl.Result{}, fmt.Errorf("get model prefetch Job: %w", err)
		}
		if !observed.DeletionTimestamp.IsZero() {
			result, err := r.wait(ctx, resource, platformv1alpha1.DeploymentPhasePrefetching, conditionModelCached, "Repairing", "waiting to replace the invalid model-cache prerequisite", now, 2*time.Second)
			return false, result, err
		}
		complete, failed, message := modelcache.JobState(observed)
		if failed {
			resource.Status.Cache.Weights = "Failed"
			resource.Status.Phase = startupPhase(resource, platformv1alpha1.DeploymentPhaseFailed)
			resource.Status.ObservedGeneration = resource.Generation
			setCondition(resource, conditionModelCached, metav1.ConditionFalse, "ModelDownloadFailed", cacheFailureMessage("model download failed", message), now)
			return false, ctrl.Result{}, r.persistStatus(ctx, resource, now)
		}
		if !complete {
			resource.Status.Cache.Weights = "Cold"
			result, err := r.wait(ctx, resource, platformv1alpha1.DeploymentPhasePrefetching, conditionModelCached, "Prefetching", "waiting for immutable model cache", now, 10*time.Second)
			return false, result, err
		}

		setCondition(resource, conditionModelCached, metav1.ConditionTrue, "CacheComplete", "model weights verified and cached", now)
		resource.Status.Cache.Weights = "Warm"
		if err := r.releaseCacheConsumers(ctx, cacheKey); err != nil {
			return false, ctrl.Result{}, fmt.Errorf("release workers after model-cache repair: %w", err)
		}
		return true, ctrl.Result{}, nil
	}

	resource.Status.Cache.Weights = "Warm"
	observed := &batchv1.Job{}
	err = r.Get(ctx, client.ObjectKeyFromObject(verifier), observed)
	if apierrors.IsNotFound(err) {
		state, epoch, leaseErr := r.cacheRepairLeaseState(ctx, cacheKey)
		if leaseErr != nil {
			return false, ctrl.Result{}, leaseErr
		}
		if state == cacheRepairComplete {
			annotations := verifier.GetAnnotations()
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations[annotationCacheVerificationEpoch] = epoch
			verifier.SetAnnotations(annotations)
		}
		if err := r.createModelCacheVerifier(ctx, resource, verifier); err != nil {
			return false, ctrl.Result{}, err
		}
		return true, ctrl.Result{}, nil
	}
	if err != nil {
		return false, ctrl.Result{}, fmt.Errorf("get model-cache verification Job: %w", err)
	}
	if !observed.DeletionTimestamp.IsZero() {
		return true, ctrl.Result{}, nil
	}
	complete, failed, message := modelcache.JobState(observed)
	if failed {
		return r.handleFailedCacheVerification(ctx, resource, observed, cachePath, cacheKey, message, now)
	}
	if complete {
		setCondition(resource, conditionModelCached, metav1.ConditionTrue, "CacheVerified", "model cache passed full manifest verification", now)
		if !cacheVerificationTime(observed).Add(cacheVerificationInterval).After(now) {
			if err := r.deleteModelCacheJob(ctx, observed); err != nil {
				return false, ctrl.Result{}, fmt.Errorf("replace stale model-cache verification Job: %w", err)
			}
		}
	}
	return true, ctrl.Result{}, nil
}

func (r *Reconciler) handleFailedCacheVerification(
	ctx context.Context,
	resource *platformv1alpha1.InferenceDeployment,
	observed *batchv1.Job,
	cachePath, cacheKey, message string,
	now time.Time,
) (bool, ctrl.Result, error) {
	role, _, err := r.beginCacheRepair(ctx, resource, cacheKey, observed.Annotations[annotationCacheVerificationEpoch], now)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	if role == cacheRepairStaleVerification || role == cacheRepairComplete {
		if observed.DeletionTimestamp.IsZero() {
			if err := r.deleteModelCacheJob(ctx, observed); err != nil {
				return false, ctrl.Result{}, fmt.Errorf("delete stale model-cache verifier: %w", err)
			}
		}
		setCondition(resource, conditionModelCached, metav1.ConditionTrue, "StaleVerificationDiscarded", "discarded verifier evidence from before the completed shared-cache repair", now)
		resource.Status.Cache.Weights = "Warm"
		if err := r.persistStatus(ctx, resource, now); err != nil {
			return false, ctrl.Result{}, err
		}
		return false, ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	if err := r.invalidateModelCache(ctx, resource, cachePath, cacheKey, message, role, now); err != nil {
		return false, ctrl.Result{}, err
	}
	return false, ctrl.Result{RequeueAfter: 2 * time.Second}, nil
}

func (r *Reconciler) invalidateModelCache(
	ctx context.Context,
	resource *platformv1alpha1.InferenceDeployment,
	cachePath, cacheKey, verifierMessage string,
	role cacheRepairRole,
	now time.Time,
) error {
	if role == cacheRepairFollower {
		markSharedCacheRepair(resource, now)
		// Only the Lease owner applies the global hold. The follower's reconcile
		// stops here, so it cannot SSA its own workload back to a serving replica
		// count while the owner drains the exact shared path.
		return r.persistStatus(ctx, resource, now)
	}
	if role != cacheRepairOwner {
		return fmt.Errorf("cannot invalidate model cache without repair ownership")
	}
	message := cacheFailureMessage("completed model-cache entry failed full verification", verifierMessage)
	resource.Status.Phase = platformv1alpha1.DeploymentPhasePrefetching
	resource.Status.ObservedGeneration = resource.Generation
	resource.Status.Cache.Weights = cacheStateRepairing
	resource.Status.Cache.WorkersWarm = 0
	resource.Status.Replicas.Desired = 0
	resource.Status.Replicas.Ready = 0
	setCondition(resource, conditionModelCached, metav1.ConditionFalse, "CacheVerificationFailed", message, now)
	setCondition(resource, conditionRuntimeReady, metav1.ConditionFalse, "CacheUnavailable", "runtime workers are held until the immutable model cache is repaired", now)

	// Project the non-serving phase first. Even if PostgreSQL projection is
	// temporarily unavailable, continue holding the workers so corrupted bytes
	// cannot be consumed while the status update is retried.
	statusErr := r.persistStatus(ctx, resource, now)
	holdErr := r.holdCacheConsumers(ctx, cachePath, cacheKey)
	return errors.Join(statusErr, holdErr)
}

func (r *Reconciler) recoverCompletedCacheRepair(
	ctx context.Context,
	resource *platformv1alpha1.InferenceDeployment,
	verifier *batchv1.Job,
	cachePath, cacheKey string,
	now time.Time,
) (bool, ctrl.Result, error) {
	// A completed Lease is the durable publication barrier. Discard any
	// verifier from the prior epoch before scheduling a new epoch-tagged check.
	observedVerifier := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKeyFromObject(verifier), observedVerifier)
	if err == nil {
		_, epoch, err := r.cacheRepairLeaseState(ctx, cacheKey)
		if err != nil {
			return false, ctrl.Result{}, err
		}
		if epoch != "" && observedVerifier.Annotations[annotationCacheVerificationEpoch] == epoch {
			// Another consumer already scheduled a proof for the repaired
			// bytes. Reuse it instead of deleting a shared verifier repeatedly.
			// A new failure is fresh evidence, even for a consumer still
			// recovering from the previous epoch. Keep workers held while
			// entering the next repair instead of briefly restoring them.
			if _, failed, message := modelcache.JobState(observedVerifier); failed {
				return r.handleFailedCacheVerification(ctx, resource, observedVerifier, cachePath, cacheKey, message, now)
			}
			if err := r.releaseCacheConsumers(ctx, cacheKey); err != nil {
				return false, ctrl.Result{}, fmt.Errorf("release workers after shared model-cache repair: %w", err)
			}
			setCondition(resource, conditionModelCached, metav1.ConditionTrue, "SharedCacheRepaired", "shared model cache was repaired and verified", now)
			resource.Status.Cache.Weights = "Warm"
			return true, ctrl.Result{}, nil
		}
		if observedVerifier.DeletionTimestamp.IsZero() {
			if err := r.deleteModelCacheJob(ctx, observedVerifier); err != nil {
				return false, ctrl.Result{}, fmt.Errorf("delete stale verifier after shared model-cache repair: %w", err)
			}
		}
		return false, ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, ctrl.Result{}, fmt.Errorf("get verifier after shared model-cache repair: %w", err)
	}
	if err := r.releaseCacheConsumers(ctx, cacheKey); err != nil {
		return false, ctrl.Result{}, fmt.Errorf("release workers after shared model-cache repair: %w", err)
	}
	setCondition(resource, conditionModelCached, metav1.ConditionTrue, "SharedCacheRepaired", "shared model cache was repaired and verified", now)
	resource.Status.Cache.Weights = "Warm"
	return true, ctrl.Result{}, nil
}

func (r *Reconciler) reconcileModelCacheRepair(
	ctx context.Context,
	resource *platformv1alpha1.InferenceDeployment,
	prefetch, verifier *batchv1.Job,
	cachePath, cacheKey string,
	now time.Time,
) (bool, ctrl.Result, error) {
	leaseState, repairEpoch, err := r.cacheRepairLeaseState(ctx, cacheKey)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	if leaseState != cacheRepairFollower || repairEpoch == "" {
		return false, ctrl.Result{}, fmt.Errorf("model-cache repair owner has no active repair epoch")
	}
	resource.Status.Phase = platformv1alpha1.DeploymentPhasePrefetching
	resource.Status.ObservedGeneration = resource.Generation
	resource.Status.Cache.Weights = cacheStateRepairing
	resource.Status.Cache.WorkersWarm = 0
	resource.Status.Replicas.Desired = 0
	resource.Status.Replicas.Ready = 0
	setCondition(resource, conditionRuntimeReady, metav1.ConditionFalse, "CacheUnavailable", "runtime workers are held until the immutable model cache is repaired", now)

	// A prior attempt can have written the CR status but failed its PostgreSQL
	// projection. Replay that durable unavailable status before touching shared
	// consumers, then reassert every hold idempotently.
	statusErr := r.persistStatus(ctx, resource, now)
	holdErr := r.holdCacheConsumers(ctx, cachePath, cacheKey)
	if err := errors.Join(statusErr, holdErr); err != nil {
		return false, ctrl.Result{}, err
	}
	drained, err := r.cacheConsumersDrained(ctx, cachePath, cacheKey)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	if !drained {
		return false, ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// The failed verifier and the old, successful prefetch are evidence for the
	// pre-repair inode. Do not remove either until every active Pod has released
	// that inode. A replacement prefetch is explicitly marked so a restart can
	// distinguish it from the stale prerequisite with the same deterministic
	// name.
	observedVerifier := &batchv1.Job{}
	err = r.Get(ctx, client.ObjectKeyFromObject(verifier), observedVerifier)
	if err == nil {
		if observedVerifier.DeletionTimestamp.IsZero() {
			if err := r.deleteModelCacheJob(ctx, observedVerifier); err != nil {
				return false, ctrl.Result{}, fmt.Errorf("delete failed model-cache verifier after drain: %w", err)
			}
		}
		return false, ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, ctrl.Result{}, fmt.Errorf("get failed model-cache verifier after drain: %w", err)
	}

	observedPrefetch := &batchv1.Job{}
	err = r.Get(ctx, client.ObjectKeyFromObject(prefetch), observedPrefetch)
	if apierrors.IsNotFound(err) {
		annotations := prefetch.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[annotationCacheRepairJob] = cacheKey
		annotations[annotationCacheRepairEpoch] = repairEpoch
		prefetch.SetAnnotations(annotations)
		if err := r.Applier.Apply(ctx, resource, prefetch); err != nil {
			return false, ctrl.Result{}, fmt.Errorf("create model-cache repair Job: %w", err)
		}
		return false, ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if err != nil {
		return false, ctrl.Result{}, fmt.Errorf("get model-cache repair Job: %w", err)
	}
	if observedPrefetch.Annotations[annotationCacheRepairJob] != cacheKey ||
		observedPrefetch.Annotations[annotationCacheRepairEpoch] != repairEpoch {
		if observedPrefetch.DeletionTimestamp.IsZero() {
			if err := r.deleteModelCacheJob(ctx, observedPrefetch); err != nil {
				return false, ctrl.Result{}, fmt.Errorf("delete stale model prefetch after drain: %w", err)
			}
		}
		return false, ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	if !observedPrefetch.DeletionTimestamp.IsZero() {
		return false, ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	complete, failed, message := modelcache.JobState(observedPrefetch)
	if failed {
		setCondition(resource, conditionModelCached, metav1.ConditionFalse, "ModelDownloadFailed", cacheFailureMessage("model-cache repair failed", message), now)
		statusErr := r.persistStatus(ctx, resource, now)
		deleteErr := r.deleteModelCacheJob(ctx, observedPrefetch)
		return false, ctrl.Result{RequeueAfter: cacheRepairRetryInterval}, errors.Join(statusErr, deleteErr)
	}
	if !complete {
		return false, ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	setCondition(resource, conditionModelCached, metav1.ConditionTrue, "CacheComplete", "model weights repaired, verified, and cached", now)
	resource.Status.Cache.Weights = "Warm"
	// Publish completion in the durable epoch Lease before restoring any
	// worker. If restoration is interrupted, every controller can replay it;
	// once completion is visible, no contender will reassert the hold.
	if _, err := r.completeCacheRepair(ctx, resource, cacheKey, now); err != nil {
		return false, ctrl.Result{}, err
	}
	if err := r.releaseCacheConsumers(ctx, cacheKey); err != nil {
		return false, ctrl.Result{}, fmt.Errorf("release workers after model-cache repair: %w", err)
	}
	return true, ctrl.Result{}, nil
}

func markCacheRepairOwner(resource *platformv1alpha1.InferenceDeployment, now time.Time) {
	resource.Status.Phase = platformv1alpha1.DeploymentPhasePrefetching
	resource.Status.ObservedGeneration = resource.Generation
	resource.Status.Cache.Weights = cacheStateRepairing
	resource.Status.Cache.WorkersWarm = 0
	resource.Status.Replicas.Desired = 0
	resource.Status.Replicas.Ready = 0
	setCondition(resource, conditionModelCached, metav1.ConditionFalse, "CacheRepairOwner", "this deployment owns repair of the shared immutable model cache", now)
	setCondition(resource, conditionRuntimeReady, metav1.ConditionFalse, "CacheUnavailable", "runtime workers are held until the immutable model cache is repaired", now)
}

func markSharedCacheRepair(resource *platformv1alpha1.InferenceDeployment, now time.Time) {
	resource.Status.Phase = platformv1alpha1.DeploymentPhasePrefetching
	resource.Status.ObservedGeneration = resource.Generation
	resource.Status.Cache.Weights = cacheStateSharedRepair
	resource.Status.Cache.WorkersWarm = 0
	resource.Status.Replicas.Desired = 0
	resource.Status.Replicas.Ready = 0
	setCondition(resource, conditionModelCached, metav1.ConditionFalse, "SharedCacheRepair", "another deployment is repairing the shared immutable model cache", now)
	setCondition(resource, conditionRuntimeReady, metav1.ConditionFalse, "CacheUnavailable", "runtime workers are held until the shared immutable model cache is repaired", now)
}

func (r *Reconciler) cacheConsumersHeld(ctx context.Context, cacheKey string) (bool, error) {
	workloads, err := r.listManagedCacheConsumers(ctx)
	if err != nil {
		return false, err
	}
	for index := range workloads.Items {
		if workloads.Items[index].Annotations[annotationCacheHold] == cacheKey {
			return true, nil
		}
	}
	return false, nil
}

func (r *Reconciler) cacheConsumersDrained(ctx context.Context, cachePath, cacheKey string) (bool, error) {
	workloads, err := r.listManagedCacheConsumers(ctx)
	if err != nil {
		return false, err
	}
	for index := range workloads.Items {
		workload := &workloads.Items[index]
		if !workloadUsesCachePath(workload, cachePath) && workload.Annotations[annotationCacheHold] != cacheKey {
			continue
		}
		if workload.Annotations[annotationCacheHold] != cacheKey || desiredReplicas(workload) != 0 ||
			workload.Status.Replicas != 0 || workload.Status.ReadyReplicas != 0 || workload.Status.AvailableReplicas != 0 {
			return false, nil
		}
	}

	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.MatchingLabels{
		kubeutil.LabelManagedBy: kubeutil.ManagedByValue,
		kubeutil.LabelComponent: "model-server",
	}); err != nil {
		return false, fmt.Errorf("list active model-cache consumer Pods across namespaces: %w", err)
	}
	for index := range pods.Items {
		pod := &pods.Items[index]
		if !podUsesCachePath(pod, cachePath) {
			continue
		}
		if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			return false, nil
		}
	}
	return true, nil
}

func (r *Reconciler) listManagedCacheConsumers(ctx context.Context) (*appsv1.DeploymentList, error) {
	workloads := &appsv1.DeploymentList{}
	if err := r.List(ctx, workloads, client.MatchingLabels{
		kubeutil.LabelManagedBy: kubeutil.ManagedByValue,
		kubeutil.LabelComponent: "model-server",
	}); err != nil {
		return nil, fmt.Errorf("list model-cache consumers across namespaces: %w", err)
	}
	return workloads, nil
}

func (r *Reconciler) holdCacheConsumers(ctx context.Context, cachePath, cacheKey string) error {
	workloads, err := r.listManagedCacheConsumers(ctx)
	if err != nil {
		return err
	}
	var result error
	for index := range workloads.Items {
		workload := &workloads.Items[index]
		if !workloadUsesCachePath(workload, cachePath) && workload.Annotations[annotationCacheHold] != cacheKey {
			continue
		}
		if heldFor := workload.Annotations[annotationCacheHold]; heldFor != "" && heldFor != cacheKey {
			result = errors.Join(result, fmt.Errorf("workload %s/%s is already held for another cache entry", workload.Namespace, workload.Name))
			continue
		}
		if err := r.pauseCacheAutoscaler(ctx, workload, cacheKey); err != nil {
			result = errors.Join(result, err)
		}
		base := workload.DeepCopy()
		annotations := workload.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		if annotations[annotationCacheHold] == "" {
			annotations[annotationCacheHold] = cacheKey
			annotations[annotationCacheHeldReplicas] = strconv.FormatInt(int64(desiredReplicas(workload)), 10)
		}
		workload.SetAnnotations(annotations)
		zero := int32(0)
		workload.Spec.Replicas = &zero
		if err := r.Patch(ctx, workload, client.MergeFrom(base)); err != nil {
			result = errors.Join(result, fmt.Errorf("hold model-cache consumer %s/%s: %w", workload.Namespace, workload.Name, err))
		}
	}
	return result
}

func (r *Reconciler) releaseCacheConsumers(ctx context.Context, cacheKey string) error {
	workloads, err := r.listManagedCacheConsumers(ctx)
	if err != nil {
		return err
	}
	var result error
	for index := range workloads.Items {
		workload := &workloads.Items[index]
		if workload.Annotations[annotationCacheHold] != cacheKey {
			continue
		}
		replicas, err := strconv.ParseInt(workload.Annotations[annotationCacheHeldReplicas], 10, 32)
		if err != nil || replicas < 0 {
			result = errors.Join(result, fmt.Errorf("restore model-cache consumer %s/%s: invalid held replica count %q", workload.Namespace, workload.Name, workload.Annotations[annotationCacheHeldReplicas]))
			continue
		}
		base := workload.DeepCopy()
		restored := int32(replicas)
		workload.Spec.Replicas = &restored
		if err := r.Patch(ctx, workload, client.MergeFrom(base)); err != nil {
			result = errors.Join(result, fmt.Errorf("restore model-cache consumer %s/%s: %w", workload.Namespace, workload.Name, err))
			continue
		}
		if err := r.releaseCacheAutoscaler(ctx, workload, cacheKey); err != nil {
			result = errors.Join(result, err)
			continue
		}
		base = workload.DeepCopy()
		annotations := workload.GetAnnotations()
		delete(annotations, annotationCacheHold)
		delete(annotations, annotationCacheHeldReplicas)
		workload.SetAnnotations(annotations)
		if err := r.Patch(ctx, workload, client.MergeFrom(base)); err != nil {
			result = errors.Join(result, fmt.Errorf("clear model-cache hold on %s/%s: %w", workload.Namespace, workload.Name, err))
		}
	}
	return result
}

func (r *Reconciler) pauseCacheAutoscaler(ctx context.Context, workload *appsv1.Deployment, cacheKey string) error {
	revision := workload.Labels[kubeutil.LabelRevision]
	if revision == "" {
		return nil
	}
	autoscaler := cacheAutoscalerObject()
	key := types.NamespacedName{Namespace: workload.Namespace, Name: kubeutil.ResourceName(revision, "autoscaler")}
	if err := r.Get(ctx, key, autoscaler); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get model-cache consumer autoscaler %s/%s: %w", key.Namespace, key.Name, err)
	}
	annotations := autoscaler.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if heldFor := annotations[annotationCacheHold]; heldFor != "" && heldFor != cacheKey {
		return fmt.Errorf("autoscaler %s/%s is already held for another cache entry", autoscaler.GetNamespace(), autoscaler.GetName())
	}
	base := autoscaler.DeepCopy()
	if annotations[annotationCacheHold] == "" {
		previous, found := annotations[kedaPausedReplicas]
		if !found {
			previous = cacheNoPreviousKEDAPause
		}
		annotations[annotationCachePreviousKEDAPause] = previous
		annotations[annotationCacheHold] = cacheKey
	}
	annotations[kedaPausedReplicas] = "0"
	autoscaler.SetAnnotations(annotations)
	if err := r.Patch(ctx, autoscaler, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("pause model-cache consumer autoscaler %s/%s: %w", autoscaler.GetNamespace(), autoscaler.GetName(), err)
	}
	return nil
}

func (r *Reconciler) releaseCacheAutoscaler(ctx context.Context, workload *appsv1.Deployment, cacheKey string) error {
	revision := workload.Labels[kubeutil.LabelRevision]
	if revision == "" {
		return nil
	}
	autoscaler := cacheAutoscalerObject()
	key := types.NamespacedName{Namespace: workload.Namespace, Name: kubeutil.ResourceName(revision, "autoscaler")}
	if err := r.Get(ctx, key, autoscaler); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get held model-cache consumer autoscaler %s/%s: %w", key.Namespace, key.Name, err)
	}
	annotations := autoscaler.GetAnnotations()
	if annotations[annotationCacheHold] != cacheKey {
		return nil
	}
	base := autoscaler.DeepCopy()
	if previous := annotations[annotationCachePreviousKEDAPause]; previous == cacheNoPreviousKEDAPause || previous == "" {
		delete(annotations, kedaPausedReplicas)
	} else {
		annotations[kedaPausedReplicas] = previous
	}
	delete(annotations, annotationCachePreviousKEDAPause)
	delete(annotations, annotationCacheHold)
	autoscaler.SetAnnotations(annotations)
	if err := r.Patch(ctx, autoscaler, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("release model-cache consumer autoscaler %s/%s: %w", autoscaler.GetNamespace(), autoscaler.GetName(), err)
	}
	return nil
}

func cacheAutoscalerObject() *unstructured.Unstructured {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"})
	return object
}

func workloadUsesCachePath(workload *appsv1.Deployment, cachePath string) bool {
	for _, volume := range workload.Spec.Template.Spec.Volumes {
		if volume.HostPath != nil && volume.HostPath.Path == cachePath {
			return true
		}
	}
	return false
}

func podUsesCachePath(pod *corev1.Pod, cachePath string) bool {
	for _, volume := range pod.Spec.Volumes {
		if volume.HostPath != nil && volume.HostPath.Path == cachePath {
			return true
		}
	}
	return false
}

func cacheVerificationTime(job *batchv1.Job) time.Time {
	if job.Status.CompletionTime != nil {
		return job.Status.CompletionTime.Time
	}
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobComplete && condition.Status == "True" {
			return condition.LastTransitionTime.Time
		}
	}
	return time.Time{}
}

func cacheFailureMessage(prefix, detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return prefix
	}
	return prefix + ": " + boundedEvidenceText(detail)
}
