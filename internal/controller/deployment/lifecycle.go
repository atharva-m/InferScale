package deployment

import (
	"context"
	"fmt"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	"github.com/inferscale/inferscale/internal/rollout"
	"github.com/inferscale/inferscale/internal/routing"
	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func startupPhase(resource *platformv1alpha1.InferenceDeployment, phase platformv1alpha1.DeploymentPhase) platformv1alpha1.DeploymentPhase {
	if resource.Status.Cache.Weights == cacheStateRepairing || resource.Status.Cache.Weights == cacheStateSharedRepair {
		return platformv1alpha1.DeploymentPhasePrefetching
	}
	if resource.Status.Revision.Stable == "" {
		return phase
	}
	switch phase {
	case platformv1alpha1.DeploymentPhasePending, platformv1alpha1.DeploymentPhasePrefetching, platformv1alpha1.DeploymentPhaseDeploying:
		return platformv1alpha1.DeploymentPhaseUpdating
	case platformv1alpha1.DeploymentPhaseFailed:
		return platformv1alpha1.DeploymentPhaseDegraded
	default:
		return phase
	}
}

// Record the decision before changing routing or replicas. A restart at any
// subsequent step resumes through reconcileRetainedFailedRevision.
func (r *Reconciler) rollbackCandidate(ctx context.Context, resource *platformv1alpha1.InferenceDeployment, revision string, backend platformruntime.Backend, reason string, now time.Time) (ctrl.Result, error) {
	if resource.Status.Rollout.LastRollback == nil || resource.Status.Rollout.LastRollback.Reason != boundedEvidenceText(reason) {
		stage := rollout.Stage(resource.Status.Rollout.Stage)
		resource.Status.Rollout.LastRollback = rollbackEvidence(stage, rolloutStateSince(resource, stage, now), now, reason, rollout.Metrics{}, r.Rollout.Policy)
	}
	resource.Status.Revision.Candidate = revision
	resource.Status.Revision.CandidateID = databaseRevisionID(resource)
	resource.Status.Rollout.Stage = string(rollout.StageFailed)
	resource.Status.Rollout.CandidateWeight = 0
	resource.Status.Phase = platformv1alpha1.DeploymentPhaseDegraded
	resource.Status.ObservedGeneration = resource.Generation
	setCondition(resource, conditionRollout, metav1.ConditionFalse, "Failed", reason, now)
	if err := r.persistStatus(ctx, resource, now); err != nil {
		return ctrl.Result{}, err
	}
	return r.reconcileRetainedFailedRevision(ctx, resource, revision, backend, now)
}

func (r *Reconciler) ensureStableRoute(ctx context.Context, resource *platformv1alpha1.InferenceDeployment, now time.Time) (bool, error) {
	stable := platformruntime.Revision{Name: resource.Status.Revision.Stable}
	names := routing.Names(stable)
	route, err := r.Router.RenderRoute(routing.RouteConfig{
		DeploymentName: resource.Name, Namespace: resource.Namespace,
		GatewayName: r.Config.GatewayName, GatewayNamespace: r.Config.GatewayNamespace,
		Path:   "/v1/deployments/" + publicDeploymentID(resource) + "/chat/completions",
		Stable: &routing.RouteRevision{Revision: stable, Names: names, ServiceName: kubeutil.ResourceName(stable.Name, "runtime"), Weight: 100},
	})
	if err != nil {
		return false, err
	}
	if err := r.Applier.Apply(ctx, resource, route); err != nil {
		return false, err
	}
	ready, reason, detail, err := r.routingReady(ctx, resource.Namespace, route.GetName(), names)
	if err != nil {
		return false, err
	}
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	setCondition(resource, conditionRouteReady, status, reason, detail, now)
	return ready, nil
}

// Discover superseded resources by labels rather than the latest candidate
// pointer, which may have advanced through several API updates between passes.
func (r *Reconciler) retireSupersededRevisions(ctx context.Context, resource *platformv1alpha1.InferenceDeployment, desired string, now time.Time) (bool, error) {
	labels := client.MatchingLabels{kubeutil.LabelManagedBy: kubeutil.ManagedByValue, kubeutil.LabelDeployment: kubeutil.ResourceName(resource.Name)}
	workloads := &appsv1.DeploymentList{}
	if err := r.List(ctx, workloads, client.InNamespace(resource.Namespace), labels); err != nil {
		return false, err
	}
	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs, client.InNamespace(resource.Namespace), labels); err != nil {
		return false, err
	}
	obsolete := func(revision string) bool {
		return revision != "" && revision != desired && revision != resource.Status.Revision.Stable
	}
	revisions := map[string][]string{}
	for _, workload := range workloads.Items {
		revision := workload.Labels[kubeutil.LabelRevision]
		if obsolete(revision) && workload.Annotations[annotationRetiredAt] == "" {
			revisions[revision] = append(revisions[revision], workload.Name)
		}
	}
	for _, job := range jobs.Items {
		revision := job.Labels[kubeutil.LabelRevision]
		if obsolete(revision) {
			if _, ok := revisions[revision]; !ok {
				revisions[revision] = nil
			}
		}
	}
	if len(revisions) == 0 {
		return false, nil
	}
	if resource.Status.Revision.Stable != "" {
		ready, err := r.ensureStableRoute(ctx, resource, now)
		if err != nil {
			return false, err
		}
		if !ready {
			resource.Status.Phase = platformv1alpha1.DeploymentPhaseUpdating
			return true, r.persistStatus(ctx, resource, now)
		}
	}
	for revision, names := range revisions {
		for _, name := range names {
			if err := r.retireWorkload(ctx, resource.Namespace, revision, name, "superseded by a newer desired revision", now); err != nil {
				return false, err
			}
		}
		if err := r.DeleteAllOf(ctx, &batchv1.Job{}, client.InNamespace(resource.Namespace), client.MatchingLabels{
			kubeutil.LabelManagedBy: kubeutil.ManagedByValue, kubeutil.LabelDeployment: kubeutil.ResourceName(resource.Name), kubeutil.LabelRevision: revision,
		}); err != nil {
			return false, fmt.Errorf("delete superseded revision jobs: %w", err)
		}
	}
	return false, nil
}

func (r *Reconciler) reactivateWorkload(ctx context.Context, workload *appsv1.Deployment) error {
	if workload.Annotations[annotationRetiredAt] == "" {
		return nil
	}
	base := workload.DeepCopy()
	delete(workload.Annotations, annotationRetiredAt)
	delete(workload.Annotations, annotationRetirementCause)
	if workload.Spec.Replicas == nil || *workload.Spec.Replicas == 0 {
		one := int32(1)
		workload.Spec.Replicas = &one
	}
	if err := r.Patch(ctx, workload, client.MergeFrom(base)); err != nil {
		return err
	}
	epp := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: workload.Namespace, Name: kubeutil.ResourceName(workload.Labels[kubeutil.LabelRevision], "epp")}, epp); err != nil {
		return client.IgnoreNotFound(err)
	}
	base = epp.DeepCopy()
	delete(epp.Annotations, annotationRetiredAt)
	delete(epp.Annotations, annotationRetirementCause)
	return r.Patch(ctx, epp, client.MergeFrom(base))
}
