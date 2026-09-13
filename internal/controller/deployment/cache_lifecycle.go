package deployment

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	"github.com/inferscale/inferscale/internal/controller/modelcache"
	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	"github.com/inferscale/inferscale/internal/rollout"
	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Cache repair takes priority over all revision lifecycle shortcuts. In
// particular, a desired-model update must not abandon an old repair whose
// owner is this deployment, or reopen admission to a held stable workload.
func (r *Reconciler) reconcilePriorityCacheRepair(ctx context.Context, resource *platformv1alpha1.InferenceDeployment, renderer modelcache.Renderer, spec platformruntime.Spec, revision platformruntime.Revision, now time.Time) (bool, ctrl.Result, error) {
	key := modelcache.CacheKey(spec.ModelURI, spec.ModelRevision)
	repairStatus := resource.Status.Cache.Weights == cacheStateRepairing || resource.Status.Cache.Weights == cacheStateSharedRepair
	workloads, err := r.listManagedCacheConsumers(ctx)
	if err != nil {
		return true, ctrl.Result{}, err
	}
	held := false
	prior := map[string]struct{}{}
	for _, workload := range workloads.Items {
		heldKey := workload.Annotations[annotationCacheHold]
		if heldKey == key {
			held = true
		} else if heldKey != "" && workload.Namespace == resource.Namespace && workload.Labels[kubeutil.LabelDeployment] == kubeutil.ResourceName(resource.Name) {
			prior[heldKey] = struct{}{}
		}
	}
	if repairStatus {
		// A status projection can precede the first worker hold. Observe our
		// active ownership as well, so a concurrent API update cannot lose it.
		leases := &coordinationv1.LeaseList{}
		if err := r.List(ctx, leases, client.InNamespace(r.Config.ControlNamespace), client.MatchingLabels{kubeutil.LabelManagedBy: kubeutil.ManagedByValue, kubeutil.LabelComponent: "model-cache-repair"}); err != nil {
			return true, ctrl.Result{}, err
		}
		for _, lease := range leases.Items {
			if dereferenceString(lease.Spec.HolderIdentity) == cacheRepairHolder(resource) && lease.Annotations[annotationCacheRepairState] == cacheRepairStateActive {
				if heldKey := lease.Annotations[annotationCacheRepairCacheKey]; heldKey != key {
					prior[heldKey] = struct{}{}
				}
			}
		}
	}
	if len(prior) > 0 {
		keys := make([]string, 0, len(prior))
		for priorKey := range prior {
			keys = append(keys, priorKey)
		}
		sort.Strings(keys)
		priorKey := keys[0]
		priorSpec, priorRevision, err := r.priorCacheRepairInput(ctx, resource, spec, priorKey)
		if err != nil {
			markSharedCacheRepair(resource, now)
			return true, ctrl.Result{}, errors.Join(err, r.persistStatus(ctx, resource, now))
		}
		priorRenderer := renderer
		for _, workload := range workloads.Items {
			if workload.Annotations[annotationCacheHold] == priorKey && len(workload.Spec.Template.Spec.NodeSelector) > 0 {
				// A desired accelerator change must not move repair away from
				// the node whose local entry these workers actually consume.
				priorRenderer.Config.NodeSelector = workload.Spec.Template.Spec.NodeSelector
				break
			}
		}
		ready, result, err := r.reconcileModelCache(ctx, resource, priorRenderer, priorSpec, priorRevision, now)
		if err != nil || !ready {
			return true, result, err
		}
		// This proof was for the previous model, never the newer desired one.
		resource.Status.Cache.Weights = "Cold"
		setCondition(resource, conditionModelCached, metav1.ConditionFalse, "DesiredModelChanged", "previous shared-cache repair completed; the desired model still needs verification", now)
		resetRolloutAfterCacheRepair(resource, now)
		return true, ctrl.Result{RequeueAfter: 2 * time.Second}, r.persistStatus(ctx, resource, now)
	}
	state, _, err := r.cacheRepairLeaseState(ctx, key)
	if err != nil {
		return true, ctrl.Result{}, err
	}
	if !repairStatus && !held && state != cacheRepairFollower {
		return false, ctrl.Result{}, nil
	}
	ready, result, err := r.reconcileModelCache(ctx, resource, renderer, spec, revision, now)
	if err != nil || !ready {
		return true, result, err
	}
	resetRolloutAfterCacheRepair(resource, now)
	return true, ctrl.Result{RequeueAfter: 2 * time.Second}, r.persistStatus(ctx, resource, now)
}

func resetRolloutAfterCacheRepair(resource *platformv1alpha1.InferenceDeployment, now time.Time) {
	if resource.Status.Revision.Candidate == "" || resource.Status.Rollout.Stage == string(rollout.StageFailed) {
		return
	}
	// Reestablish readiness and collect fresh stage evidence after a cache
	// outage, instead of interpreting normal process restart as a regression.
	resource.Status.Rollout.Stage = string(rollout.StagePending)
	resource.Status.Rollout.StageStartedAt = &metav1.Time{Time: now}
	resource.Status.Rollout.CandidateWeight = 0
	resource.Status.Rollout.RegressionWindows = 0
	resource.Status.Rollout.LastRegressionWindow = nil
}

func (r *Reconciler) priorCacheRepairInput(ctx context.Context, resource *platformv1alpha1.InferenceDeployment, desired platformruntime.Spec, key string) (platformruntime.Spec, platformruntime.Revision, error) {
	lease := &coordinationv1.Lease{}
	if err := r.Get(ctx, r.cacheRepairLeaseKey(key), lease); err != nil {
		return platformruntime.Spec{}, platformruntime.Revision{}, fmt.Errorf("read prior model-cache repair: %w", err)
	}
	if err := validateCacheRepairLease(lease, key); err != nil {
		return platformruntime.Spec{}, platformruntime.Revision{}, err
	}
	uri, modelRevision := lease.Annotations[annotationCacheRepairModelURI], lease.Annotations[annotationCacheRepairRevision]
	if uri == "" || modelRevision == "" {
		// Migrate older Leases using this deployment's immutable prerequisite.
		// Never select another tenant's Job or trust an unverified path string.
		jobs := &batchv1.JobList{}
		if err := r.List(ctx, jobs, client.InNamespace(resource.Namespace), client.MatchingLabels{kubeutil.LabelManagedBy: kubeutil.ManagedByValue, kubeutil.LabelDeployment: kubeutil.ResourceName(resource.Name), kubeutil.LabelComponent: "model-prefetch"}); err != nil {
			return platformruntime.Spec{}, platformruntime.Revision{}, err
		}
		for _, job := range jobs.Items {
			for _, container := range job.Spec.Template.Spec.Containers {
				args := container.Args
				candidateURI, candidateRevision := "", ""
				for index := 0; index+1 < len(args); index++ {
					switch args[index] {
					case "--uri":
						candidateURI = args[index+1]
					case "--revision":
						candidateRevision = args[index+1]
					}
				}
				if candidateURI != "" && candidateRevision != "" && modelcache.CacheKey(candidateURI, candidateRevision) == key {
					uri, modelRevision = candidateURI, candidateRevision
				}
			}
		}
		if uri != "" && modelRevision != "" {
			base := lease.DeepCopy()
			lease.Annotations[annotationCacheRepairModelURI] = uri
			lease.Annotations[annotationCacheRepairRevision] = modelRevision
			if err := r.Patch(ctx, lease, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return platformruntime.Spec{}, platformruntime.Revision{}, err
			}
		}
	}
	if uri == "" || modelRevision == "" || modelcache.CacheKey(uri, modelRevision) != key {
		return platformruntime.Spec{}, platformruntime.Revision{}, fmt.Errorf("prior model-cache repair %q lacks immutable model identity", key)
	}
	desired.ModelURI, desired.ModelRevision = uri, modelRevision
	revision, err := platformruntime.RevisionFor(desired)
	return desired, revision, err
}
