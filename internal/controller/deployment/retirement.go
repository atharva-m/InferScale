package deployment

import (
	"context"
	"fmt"
	"strings"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	"github.com/inferscale/inferscale/internal/rollout"
	"github.com/inferscale/inferscale/internal/routing"
	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	annotationRetiredAt       = "inferscale.io/retired-at"
	annotationRetirementCause = "inferscale.io/retirement-cause"
	kedaPausedReplicas        = "autoscaling.keda.sh/paused-replicas"
	failedRevisionRetention   = 24 * time.Hour
)

// retireWorkload makes retirement independent from the user-facing
// scale-to-zero feature gate. Keeping the zero-replica Deployment and its
// retirement metadata for inspection is intentional; PostgreSQL remains the
// durable audit record after Kubernetes event retention expires.
func (r *Reconciler) retireWorkload(
	ctx context.Context,
	namespace, revision, workloadName, cause string,
	now time.Time,
) error {
	if namespace == "" || revision == "" || workloadName == "" {
		return fmt.Errorf("namespace, revision, and workload name are required for retirement")
	}

	// Pause first so a delayed KEDA finalizer or a temporarily unavailable KEDA
	// operator cannot restore a failed candidate to its previous minimum.
	scaledObject := &unstructured.Unstructured{}
	scaledObject.SetGroupVersionKind(schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"})
	scaledObjectKey := types.NamespacedName{Namespace: namespace, Name: kubeutil.ResourceName(revision, "autoscaler")}
	if err := r.Get(ctx, scaledObjectKey, scaledObject); err == nil {
		base := scaledObject.DeepCopy()
		annotations := scaledObject.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[kedaPausedReplicas] = "0"
		scaledObject.SetAnnotations(annotations)
		if err := r.Patch(ctx, scaledObject, client.MergeFrom(base)); err != nil {
			return fmt.Errorf("pause retired revision autoscaler: %w", err)
		}
		if err := r.Delete(ctx, scaledObject); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete retired revision autoscaler: %w", err)
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get retired revision autoscaler: %w", err)
	}

	workload := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: workloadName}, workload); err == nil {
		base := workload.DeepCopy()
		zero := int32(0)
		workload.Spec.Replicas = &zero
		markRetirement(workload, cause, now)
		if err := r.Patch(ctx, workload, client.MergeFrom(base)); err != nil {
			return fmt.Errorf("scale retired runtime workload to zero: %w", err)
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get retired runtime workload: %w", err)
	}

	// Duplicate the retention marker on the CPU-only EPP Deployment. It acts as
	// a cleanup tombstone if an operator deletes the runtime Deployment while
	// investigating a failed rollout.
	epp := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: kubeutil.ResourceName(revision, "epp")}, epp); err == nil {
		base := epp.DeepCopy()
		zero := int32(0)
		epp.Spec.Replicas = &zero
		markRetirement(epp, cause, now)
		if err := r.Patch(ctx, epp, client.MergeFrom(base)); err != nil {
			return fmt.Errorf("mark retired endpoint-picker deployment: %w", err)
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get retired endpoint-picker deployment: %w", err)
	}
	return nil
}

func retainedFailedCandidate(resource *platformv1alpha1.InferenceDeployment, currentRevision string) bool {
	if resource.Status.Rollout.Stage != string(rollout.StageFailed) ||
		resource.Status.Revision.Stable == "" || resource.Status.Revision.Stable == currentRevision {
		return false
	}
	active := resource.Status.Revision.Candidate == currentRevision &&
		resource.Status.Revision.CandidateID == databaseRevisionID(resource)
	retired := resource.Status.Revision.Candidate == "" && resource.Status.Revision.CandidateID == "" &&
		resource.Status.Revision.LastFailed == currentRevision &&
		resource.Status.Revision.LastFailedID == databaseRevisionID(resource)
	return active || retired
}

func (r *Reconciler) reconcileRetainedFailedRevision(
	ctx context.Context,
	resource *platformv1alpha1.InferenceDeployment,
	candidateRevision string,
	candidateBackend platformruntime.Backend,
	now time.Time,
) (ctrl.Result, error) {
	reason := "candidate previously failed"
	if condition := kubeutil.FindCondition(resource.Status.Conditions, conditionRollout); condition != nil && strings.TrimSpace(condition.Message) != "" {
		reason = condition.Message
	}
	ready, err := r.ensureStableRoute(ctx, resource, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		resource.Status.Phase = platformv1alpha1.DeploymentPhaseDegraded
		if err := r.persistStatus(ctx, resource, now); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if err := r.retireWorkload(
		ctx, resource.Namespace, candidateRevision,
		workloadName(candidateRevision, candidateBackend), reason, now,
	); err != nil {
		return ctrl.Result{}, err
	}
	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs, client.InNamespace(resource.Namespace), client.MatchingLabels{
		kubeutil.LabelManagedBy: kubeutil.ManagedByValue, kubeutil.LabelRevision: candidateRevision,
	}); err != nil {
		return ctrl.Result{}, err
	}
	for index := range jobs.Items {
		job := &jobs.Items[index]
		if job.Annotations[annotationRetiredAt] != "" && (job.Status.CompletionTime != nil || (job.Spec.Suspend != nil && *job.Spec.Suspend)) {
			continue
		}
		base := job.DeepCopy()
		markRetirement(job, reason, now)
		if job.Status.CompletionTime == nil {
			suspend := true
			job.Spec.Suspend = &suspend
		}
		if err := r.Patch(ctx, job, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("suspend retired candidate prerequisite: %w", err)
		}
	}
	return r.reconcileFailedStable(ctx, resource, "FailedCandidateRetained", "failed candidate is retired at zero replicas for the 24-hour inspection window", now)
}

func markRetirement(object client.Object, cause string, now time.Time) {
	annotations := object.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if strings.TrimSpace(annotations[annotationRetiredAt]) == "" {
		annotations[annotationRetiredAt] = now.UTC().Format(time.RFC3339Nano)
	}
	if strings.TrimSpace(cause) != "" {
		annotations[annotationRetirementCause] = cause
	}
	object.SetAnnotations(annotations)
}

// pruneExpiredRetiredRevisions removes only revision-scoped Kubernetes
// resources. Shared model/engine cache directories and all PostgreSQL history
// are deliberately outside this lifecycle.
func (r *Reconciler) pruneExpiredRetiredRevisions(
	ctx context.Context,
	resource *platformv1alpha1.InferenceDeployment,
	currentRevision string,
	now time.Time,
) (bool, error) {
	expired := map[string]struct{}{}
	currentFailed := false
	currentIsFailed := resource.Status.Rollout.Stage == string(rollout.StageFailed) &&
		resource.Status.Revision.Candidate == currentRevision

	workloads := &appsv1.DeploymentList{}
	if err := r.List(ctx, workloads, client.InNamespace(resource.Namespace), client.MatchingLabels{
		kubeutil.LabelManagedBy:  kubeutil.ManagedByValue,
		kubeutil.LabelDeployment: kubeutil.ResourceName(resource.Name),
	}); err != nil {
		return false, fmt.Errorf("list retired runtime workloads: %w", err)
	}
	for index := range workloads.Items {
		workload := &workloads.Items[index]
		retiredAt := strings.TrimSpace(workload.Annotations[annotationRetiredAt])
		if retiredAt == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, retiredAt)
		if err != nil {
			return false, fmt.Errorf("parse retirement time for workload %s: %w", workload.Name, err)
		}
		if now.Before(at.Add(failedRevisionRetention)) {
			continue
		}
		revision := workload.Labels[kubeutil.LabelRevision]
		if revision == resource.Status.Revision.Stable {
			continue
		}
		if revision == currentRevision && !currentIsFailed {
			continue
		}
		if revision != "" {
			expired[revision] = struct{}{}
			currentFailed = currentFailed || (currentIsFailed && revision == currentRevision)
		}
	}
	// An abort may happen during artifact preparation, before a runtime/EPP
	// Deployment exists. Retired Jobs provide the same cleanup tombstone.
	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs, client.InNamespace(resource.Namespace), client.MatchingLabels{
		kubeutil.LabelManagedBy: kubeutil.ManagedByValue, kubeutil.LabelDeployment: kubeutil.ResourceName(resource.Name),
	}); err != nil {
		return false, fmt.Errorf("list retired prerequisite jobs: %w", err)
	}
	for index := range jobs.Items {
		job := &jobs.Items[index]
		revision := job.Labels[kubeutil.LabelRevision]
		retiredAt := job.Annotations[annotationRetiredAt]
		if retiredAt == "" || revision == "" || revision == resource.Status.Revision.Stable || (revision == currentRevision && !currentIsFailed) {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, retiredAt)
		if err != nil {
			return false, fmt.Errorf("parse retirement time for job %s: %w", job.Name, err)
		}
		if !now.Before(at.Add(failedRevisionRetention)) {
			expired[revision] = struct{}{}
			currentFailed = currentFailed || (currentIsFailed && revision == currentRevision)
		}
	}
	for revision := range expired {
		if err := r.deleteRevisionResources(ctx, resource.Namespace, resource.Name, revision); err != nil {
			return false, err
		}
	}
	return currentFailed, nil
}

func (r *Reconciler) deleteRevisionResources(ctx context.Context, namespace, deploymentName, revision string) error {
	labels := client.MatchingLabels{
		kubeutil.LabelManagedBy:  kubeutil.ManagedByValue,
		kubeutil.LabelDeployment: kubeutil.ResourceName(deploymentName),
		kubeutil.LabelRevision:   revision,
	}
	options := []client.DeleteAllOfOption{client.InNamespace(namespace), labels}
	objects := []client.Object{
		&appsv1.Deployment{}, &batchv1.Job{}, &corev1.Service{}, &corev1.ConfigMap{}, &corev1.ServiceAccount{},
		&networkingv1.NetworkPolicy{}, &rbacv1.Role{}, &rbacv1.RoleBinding{},
	}
	for _, object := range objects {
		if err := r.DeleteAllOf(ctx, object, options...); err != nil {
			return fmt.Errorf("delete %T for retired revision %s: %w", object, revision, err)
		}
	}
	for _, gvk := range []schema.GroupVersionKind{
		{Group: "inference.networking.k8s.io", Version: "v1", Kind: "InferencePool"},
		{Group: "llm-d.ai", Version: "v1alpha2", Kind: "InferenceObjective"},
		{Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor"},
		{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"},
	} {
		object := &unstructured.Unstructured{}
		object.SetGroupVersionKind(gvk)
		if err := r.DeleteAllOf(ctx, object, options...); err != nil {
			return fmt.Errorf("delete %s for retired revision %s: %w", gvk.Kind, revision, err)
		}
	}
	return nil
}

func (r *Reconciler) reconcileExpiredFailedRevision(
	ctx context.Context,
	resource *platformv1alpha1.InferenceDeployment,
	failedRevision string,
	now time.Time,
) (ctrl.Result, error) {
	markExpiredFailedRevision(resource, failedRevision)
	message := "failed candidate resources were retained for 24 hours and then pruned; database rollout evidence remains available"
	return r.reconcileFailedStable(ctx, resource, "FailedRevisionPruned", message, now)
}

func markExpiredFailedRevision(resource *platformv1alpha1.InferenceDeployment, failedRevision string) {
	resource.Status.Revision.LastFailed = failedRevision
	resource.Status.Revision.LastFailedID = resource.Status.Revision.CandidateID
	if resource.Status.Revision.LastFailedID == "" {
		resource.Status.Revision.LastFailedID = databaseRevisionID(resource)
	}
	resource.Status.Revision.Candidate = ""
	resource.Status.Revision.CandidateID = ""
	resource.Status.Rollout.CandidateWeight = 0
}

// reconcileFailedStable projects the availability of the still-active stable
// revision without observing or restoring the failed candidate. Degraded is
// an admission-active phase, so traffic continues whenever the stable route
// remains accepted.
func (r *Reconciler) reconcileFailedStable(
	ctx context.Context,
	resource *platformv1alpha1.InferenceDeployment,
	failureReason, message string,
	now time.Time,
) (ctrl.Result, error) {
	resource.Status.ObservedGeneration = resource.Generation
	resource.Status.Phase = platformv1alpha1.DeploymentPhaseFailed

	stable := resource.Status.Revision.Stable
	if stable != "" {
		resource.Status.Phase = platformv1alpha1.DeploymentPhaseDegraded
		if resource.Status.Revision.Candidate == "" && resource.Status.Revision.CandidateID == "" &&
			resource.Status.Revision.LastFailedID != "" {
			resource.Status.Phase = platformv1alpha1.DeploymentPhaseReady
		}
		stableBackend := platformruntime.Backend(resource.Status.Runtime.Backend)
		if stableBackend != platformruntime.BackendTRTLLM {
			stableBackend = platformruntime.BackendVLLM
		}
		stableWorkload := &appsv1.Deployment{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: resource.Namespace, Name: workloadName(stable, stableBackend)}, stableWorkload); err == nil {
			resource.Status.Replicas.Desired = desiredReplicas(stableWorkload)
			resource.Status.Replicas.Ready = stableWorkload.Status.ReadyReplicas
			resource.Status.Cache.WorkersWarm = stableWorkload.Status.AvailableReplicas
			if stableWorkload.Status.AvailableReplicas > 0 {
				setCondition(resource, conditionRuntimeReady, metav1.ConditionTrue, "StableWorkersReady", "stable runtime workers remain ready after candidate rollback", now)
			} else {
				setCondition(resource, conditionRuntimeReady, metav1.ConditionFalse, "StableWorkersNotReady", "stable runtime workers are not ready after candidate rollback", now)
			}
		} else if apierrors.IsNotFound(err) {
			resource.Status.Replicas.Desired = 0
			resource.Status.Replicas.Ready = 0
			resource.Status.Cache.WorkersWarm = 0
			setCondition(resource, conditionRuntimeReady, metav1.ConditionFalse, "StableWorkloadMissing", "stable runtime workload is missing after candidate rollback", now)
		} else {
			return ctrl.Result{}, fmt.Errorf("observe stable workload after candidate rollback: %w", err)
		}
		stableRevision := platformruntime.Revision{Name: stable}
		stableNames := routing.Names(stableRevision)
		routeConfig := routing.RouteConfig{
			DeploymentName: resource.Name, Namespace: resource.Namespace,
			GatewayName: r.Config.GatewayName, GatewayNamespace: r.Config.GatewayNamespace,
			Path: "/v1/deployments/" + publicDeploymentID(resource) + "/chat/completions",
			Stable: &routing.RouteRevision{
				Revision: stableRevision, Names: stableNames,
				ServiceName: kubeutil.ResourceName(stable, "runtime"), Weight: 100,
			},
		}
		route, err := r.Router.RenderRoute(routeConfig)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("render stable route after failed revision retirement: %w", err)
		}
		if err := r.Applier.Apply(ctx, resource, route); err != nil {
			return ctrl.Result{}, err
		}
		ready, reason, detail, err := r.routingReady(ctx, resource.Namespace, route.GetName(), stableNames)
		if err != nil {
			return ctrl.Result{}, err
		}
		status := metav1.ConditionFalse
		if ready {
			status = metav1.ConditionTrue
		}
		setCondition(resource, conditionRouteReady, status, reason, detail, now)
	} else {
		setCondition(resource, conditionRuntimeReady, metav1.ConditionFalse, failureReason, message, now)
	}
	// Preserve the original rollback condition and LastRollback snapshot. They
	// are the durable trigger evidence; this reconciliation only reports stable
	// serving availability and failed-candidate retention state.
	if err := r.persistStatus(ctx, resource, now); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}
