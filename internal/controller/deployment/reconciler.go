package deployment

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	"github.com/inferscale/inferscale/internal/autoscaling"
	"github.com/inferscale/inferscale/internal/controller/modelcache"
	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	"github.com/inferscale/inferscale/internal/rollout"
	"github.com/inferscale/inferscale/internal/routing"
	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	finalizerName = "platform.inferscale.io/finalizer"

	conditionModelCached     = "ModelCached"
	conditionRuntimeArtifact = "RuntimeArtifactsReady"
	conditionRuntimeReady    = "RuntimeReady"
	conditionRouteReady      = "RouteReady"
	conditionRollout         = "RolloutProgressing"
)

type Config struct {
	Images                platformruntime.Images
	ControlNamespace      string
	ModelCacheRoot        string
	EngineCacheRoot       string
	ImagePullSecret       string
	HFTokenSecret         string
	RuntimeServiceAccount string
	GatewayName           string
	GatewayNamespace      string
	MonitoringNamespace   string
	PrometheusURL         string
	OTLPEndpoint          string
	PublicBaseURL         string
	GPUNodeSelectors      map[string]map[string]string
	ProgressiveRollout    bool
	ScaleToZero           bool
	BackendAutoEnabled    bool
	TensorRTEnabled       bool
}

type RolloutMetricsProvider interface {
	Snapshot(context.Context, string, string, string, time.Time) (rollout.Metrics, error)
}

// StageRolloutMetricsProvider lets telemetry accumulate minimum-sample and
// safety evidence from the beginning of the current rollout stage while
// preserving the original provider contract for lightweight test fakes.
type StageRolloutMetricsProvider interface {
	SnapshotForStage(context.Context, string, string, string, time.Time, time.Time) (rollout.Metrics, error)
}

// RolloutStageMetricsProvider selects a source that actually observes the
// traffic path for the stage (direct runtime Service during Shadow, EPP from
// Canary5 onward).
type RolloutStageMetricsProvider interface {
	SnapshotForRolloutStage(context.Context, string, string, string, rollout.Stage, time.Time, time.Time) (rollout.Metrics, error)
}

type StatusProjector interface {
	ProjectControllerStatus(
		context.Context,
		string,
		string,
		platformv1alpha1.InferenceDeploymentStatus,
		time.Time,
	) error
}

type Reconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	Config         Config
	Registry       *platformruntime.Registry
	Resolver       BackendResolver
	Projector      StatusProjector
	CacheRenderer  modelcache.Renderer
	Router         routing.Renderer
	Autoscaler     autoscaling.Renderer
	Rollout        rollout.Machine
	RolloutMetrics RolloutMetricsProvider
	Applier        kubeutil.Applier
	Now            func() time.Time
}

// +kubebuilder:rbac:groups=platform.inferscale.io,resources=inferencedeployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=platform.inferscale.io,resources=inferencedeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.inferscale.io,resources=inferencedeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods;endpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services;serviceaccounts;configmaps,verbs=get;list;watch;create;update;patch;delete;deletecollection
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete;deletecollection
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete;deletecollection
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete;deletecollection
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete;deletecollection
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=inference.networking.k8s.io,resources=inferencepools,verbs=get;list;watch;create;update;patch;delete;deletecollection
// +kubebuilder:rbac:groups=llm-d.ai,resources=inferenceobjectives,verbs=get;list;watch;create;update;patch;delete;deletecollection
// +kubebuilder:rbac:groups=llm-d.ai,resources=inferencemodelrewrites,verbs=get;list;watch
// +kubebuilder:rbac:groups=keda.sh,resources=scaledobjects,verbs=get;list;watch;create;update;patch;delete;deletecollection
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;create;update;patch;delete;deletecollection

func (r *Reconciler) SetupWithManager(manager ctrl.Manager) error {
	if r.Client == nil {
		r.Client = manager.GetClient()
	}
	if r.Scheme == nil {
		r.Scheme = manager.GetScheme()
	}
	if r.Applier.Client == nil {
		r.Applier = kubeutil.Applier{Client: r.Client, Scheme: r.Scheme, FieldOwner: kubeutil.ManagedByValue}
	}
	return ctrl.NewControllerManagedBy(manager).
		For(&platformv1alpha1.InferenceDeployment{}, builder.WithPredicates(deploymentPredicate())).
		Owns(&appsv1.Deployment{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.Service{}).
		Complete(r)
}

func (r *Reconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	resource := &platformv1alpha1.InferenceDeployment{}
	if err := r.Get(ctx, request.NamespacedName, resource); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	r.defaults()
	now := r.Now().UTC()

	if !resource.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, resource, now)
	}
	if !controllerutil.ContainsFinalizer(resource, finalizerName) {
		controllerutil.AddFinalizer(resource, finalizerName)
		if err := r.Update(ctx, resource); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	if err := resource.Validate(); err != nil {
		return r.fail(ctx, resource, platformv1alpha1.DeploymentPhaseFailed, "InvalidSpec", err.Error(), now)
	}

	requested, err := normalizeSpec(resource, "", r.Config.Images)
	if err != nil {
		return r.fail(ctx, resource, platformv1alpha1.DeploymentPhaseFailed, "InvalidBackend", err.Error(), now)
	}
	resolution, err := r.Resolver.Resolve(ctx, RevisionIdentity{
		DeploymentID: publicDeploymentID(resource), RevisionID: databaseRevisionID(resource),
	}, requested)
	if err != nil {
		return r.fail(ctx, resource, platformv1alpha1.DeploymentPhaseFailed, "BackendSelectionFailed", err.Error(), now)
	}
	frozenImages, err := imagesForResolution(r.Config.Images, resolution)
	if err != nil {
		return r.fail(ctx, resource, platformv1alpha1.DeploymentPhaseFailed, "RuntimeIdentityInvalid", err.Error(), now)
	}
	spec, err := normalizeSpec(resource, resolution.Backend, frozenImages)
	if err != nil {
		return r.fail(ctx, resource, platformv1alpha1.DeploymentPhaseFailed, "InvalidSpec", err.Error(), now)
	}
	if r.Registry == nil {
		return r.fail(ctx, resource, platformv1alpha1.DeploymentPhaseFailed, "RuntimeUnavailable", "runtime registry is not configured", now)
	}
	adapter, err := r.Registry.Get(spec.ResolvedBackend)
	if err != nil {
		return r.fail(ctx, resource, platformv1alpha1.DeploymentPhaseFailed, "RuntimeUnavailable", err.Error(), now)
	}
	if err := adapter.Validate(spec); err != nil {
		return r.fail(ctx, resource, platformv1alpha1.DeploymentPhaseFailed, "UnsupportedRuntimeConfiguration", err.Error(), now)
	}
	revision, err := platformruntime.RevisionFor(spec)
	if err != nil {
		return r.fail(ctx, resource, platformv1alpha1.DeploymentPhaseFailed, "RevisionFailed", err.Error(), now)
	}
	nodeSelector, err := r.nodeSelectorFor(spec.AcceleratorType)
	if err != nil {
		return r.fail(ctx, resource, platformv1alpha1.DeploymentPhaseFailed, "UnsupportedAcceleratorInventory", err.Error(), now)
	}
	cacheRenderer := r.CacheRenderer
	cacheRenderer.Config.NodeSelector = nodeSelector
	cacheRequired := adapter.Capabilities().RequiresModelCache
	if cacheRequired {
		// Repair deliberately removes runtime readiness. Complete that safety
		// protocol before treating zero workers as a rollout failure, accepting
		// an abort, or deleting prerequisites of a superseded revision.
		if handled, result, err := r.reconcilePriorityCacheRepair(ctx, resource, cacheRenderer, spec, revision, now); handled || err != nil {
			return result, err
		}
	}
	prunedCurrentFailure, err := r.pruneExpiredRetiredRevisions(ctx, resource, revision.Name, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	if prunedCurrentFailure {
		return r.reconcileExpiredFailedRevision(ctx, resource, revision.Name, now)
	}
	if retainedFailedCandidate(resource, revision.Name) {
		// A failed candidate remains part of the immutable desired spec until a
		// later API mutation creates another revision. During its 24-hour
		// inspection window it is a tombstone, not desired serving state: never
		// re-apply its workload/autoscaler or wait for it to become Ready.
		return r.reconcileRetainedFailedRevision(ctx, resource, revision.Name, spec.ResolvedBackend, now)
	}
	if waiting, err := r.retireSupersededRevisions(ctx, resource, revision.Name, now); err != nil {
		return ctrl.Result{}, err
	} else if waiting {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if resource.Status.Revision.Stable != "" && resource.Status.Revision.Stable != revision.Name &&
		strings.EqualFold(strings.TrimSpace(resource.Annotations[kubeutil.AnnotationRolloutControl]), "abort") {
		return r.rollbackCandidate(ctx, resource, revision.Name, spec.ResolvedBackend, "operator aborted rollout", now)
	}
	if resource.Status.Revision.Stable != "" && resource.Status.Revision.Candidate == revision.Name &&
		resource.Status.Rollout.Stage != "" && resource.Status.Rollout.Stage != string(rollout.StagePending) {
		candidate := &appsv1.Deployment{}
		err := r.Get(ctx, types.NamespacedName{Namespace: resource.Namespace, Name: workloadName(revision.Name, spec.ResolvedBackend)}, candidate)
		if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if apierrors.IsNotFound(err) || candidate.Status.AvailableReplicas == 0 {
			return r.rollbackCandidate(ctx, resource, revision.Name, spec.ResolvedBackend, "candidate lost runtime readiness", now)
		}
	}

	modelPath := ""
	if cacheRequired {
		modelPath = cacheRenderer.Path(spec)
		ready, result, err := r.reconcileModelCache(ctx, resource, cacheRenderer, spec, revision, now)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ready {
			return result, nil
		}
	} else {
		setCondition(resource, conditionModelCached, metav1.ConditionTrue, "NotRequired", "runtime adapter does not require a model cache", now)
	}
	resource.Status.Cache.Weights = "NotRequired"
	if cacheRequired {
		resource.Status.Cache.Weights = "Warm"
	}

	renderContext := platformruntime.RenderContext{
		Spec: spec, Revision: revision, Images: frozenImages,
		ModelCacheHostPath:  r.Config.ModelCacheRoot,
		ModelPath:           modelPath,
		EngineCacheHostPath: r.Config.EngineCacheRoot,
		EnginePath:          filepath.Join(r.Config.EngineCacheRoot, revision.Digest),
		ServiceAccountName:  r.Config.RuntimeServiceAccount,
		ImagePullSecret:     r.Config.ImagePullSecret,
		GPUNodeSelector:     nodeSelector,
		KVEventsPort:        5557,
		OTLPEndpoint:        r.Config.OTLPEndpoint,
	}
	rendered, err := adapter.Render(ctx, renderContext)
	if err != nil {
		return r.fail(ctx, resource, platformv1alpha1.DeploymentPhaseFailed, "RuntimeRenderFailed", err.Error(), now)
	}
	if len(rendered.Prerequisites) > 0 {
		if err := r.Applier.ApplyAll(ctx, resource, rendered.Prerequisites); err != nil {
			return ctrl.Result{}, err
		}
		ready, failed, message, err := r.prerequisitesReady(ctx, rendered.Prerequisites)
		if err != nil {
			return ctrl.Result{}, err
		}
		if failed {
			return r.fail(ctx, resource, platformv1alpha1.DeploymentPhaseFailed, "RuntimeArtifactFailed", message, now)
		}
		if !ready {
			return r.wait(ctx, resource, platformv1alpha1.DeploymentPhaseDeploying, conditionRuntimeArtifact, "Building", "waiting for runtime artifacts", now, 15*time.Second)
		}
		setCondition(resource, conditionRuntimeArtifact, metav1.ConditionTrue, "ArtifactsReady", "runtime artifacts are ready", now)
	}
	if err := r.Applier.ApplyAll(ctx, resource, rendered.Serving); err != nil {
		return ctrl.Result{}, err
	}
	revisionRouting, err := r.Router.RenderRevision(spec, revision, rendered.ServiceName)
	if err != nil {
		return r.fail(ctx, resource, platformv1alpha1.DeploymentPhaseFailed, "RoutingRenderFailed", err.Error(), now)
	}
	if err := r.Applier.ApplyAll(ctx, resource, revisionRouting.Objects); err != nil {
		return ctrl.Result{}, err
	}

	workload := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: spec.Namespace, Name: rendered.WorkloadName}, workload); err != nil {
		if apierrors.IsNotFound(err) {
			return r.wait(ctx, resource, platformv1alpha1.DeploymentPhaseDeploying, conditionRuntimeReady, "Creating", "waiting for runtime workload", now, 10*time.Second)
		}
		return ctrl.Result{}, err
	}
	// KEDA must exist before readiness: it revives a retained zero-replica
	// workload when a previous serving contract becomes desired again.
	if err := r.reactivateWorkload(ctx, workload); err != nil {
		return ctrl.Result{}, err
	}
	effectiveMin := spec.MinReplicas
	if resource.Status.Revision.Stable != revision.Name {
		effectiveMin = max32(1, effectiveMin)
	}
	if err := r.applyAutoscaler(ctx, resource, spec, revision.Name, rendered.WorkloadName, revisionRouting.Names, effectiveMin, spec.MaxReplicas); err != nil {
		return ctrl.Result{}, err
	}
	candidateReady := workload.Status.ObservedGeneration >= workload.Generation && workload.Status.AvailableReplicas > 0
	if !candidateReady && resource.Status.Revision.Stable == "" {
		return r.wait(ctx, resource, platformv1alpha1.DeploymentPhaseDeploying, conditionRuntimeReady, "PodsNotReady", "waiting for a ready runtime worker", now, 10*time.Second)
	}
	if candidateReady {
		setCondition(resource, conditionRuntimeReady, metav1.ConditionTrue, "WorkersReady", "runtime workers are ready", now)
	} else {
		setCondition(resource, conditionRuntimeReady, metav1.ConditionFalse, "PodsNotReady", "waiting for a ready runtime worker", now)
		if resource.Status.Revision.Stable == revision.Name && (spec.MinReplicas != 0 || !r.Config.ScaleToZero) {
			return r.wait(ctx, resource, platformv1alpha1.DeploymentPhaseDegraded, conditionRuntimeReady, "PodsNotReady", "waiting for the stable runtime to recover", now, 10*time.Second)
		}
		// A stable scale-to-zero pool stays admission-active. Its queued
		// requests are the signal that wakes the KEDA activator.
	}

	routeConfig, decision, oldStable, err := r.planRoute(ctx, resource, spec, revision, rendered.ServiceName, revisionRouting.Names, candidateReady, now)
	if err != nil {
		return r.fail(ctx, resource, platformv1alpha1.DeploymentPhaseDegraded, "RolloutEvaluationFailed", err.Error(), now)
	}
	if decision.Rollback {
		return r.rollbackCandidate(ctx, resource, revision.Name, spec.ResolvedBackend, decision.Reason, now)
	}
	routeObject, err := r.Router.RenderRoute(routeConfig)
	if err != nil {
		return r.fail(ctx, resource, platformv1alpha1.DeploymentPhaseFailed, "RouteRenderFailed", err.Error(), now)
	}
	if err := r.Applier.Apply(ctx, resource, routeObject); err != nil {
		return ctrl.Result{}, err
	}

	readinessNames := revisionRouting.Names
	stableReadinessNames := routing.RevisionNames{}
	if oldStable != "" {
		stableReadinessNames = routing.Names(platformruntime.Revision{Name: oldStable})
	}
	if routeConfig.Candidate == nil && oldStable != "" && oldStable != revision.Name {
		readinessNames = stableReadinessNames
	}
	routeReady, routeReason, routeMessage, err := r.routingReady(
		ctx, spec.Namespace, routeObject.GetName(), readinessNames,
	)
	if err != nil {
		return ctrl.Result{}, err
	}
	if routeReady && oldStable != "" && oldStable != revision.Name && decision.StableWeight > 0 && readinessNames.Pool != stableReadinessNames.Pool {
		routeReady, routeReason, routeMessage, err = r.routingReady(
			ctx, spec.Namespace, routeObject.GetName(), stableReadinessNames,
		)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !routeReady {
			routeReason = "Stable" + routeReason
			routeMessage = "stable revision: " + routeMessage
		}
	}
	if !routeReady {
		phase := platformv1alpha1.DeploymentPhaseDeploying
		if resource.Status.Revision.Stable != "" {
			phase = platformv1alpha1.DeploymentPhaseUpdating
			if resource.Status.Revision.Stable == revision.Name && resource.Status.Revision.Candidate == "" {
				phase = platformv1alpha1.DeploymentPhaseDegraded
			}
		}
		return r.wait(ctx, resource, phase, conditionRouteReady, routeReason, routeMessage, now, 10*time.Second)
	}

	resource.Status.ObservedGeneration = resource.Generation
	resource.Status.Cache.Weights = "NotRequired"
	if cacheRequired {
		resource.Status.Cache.Weights = "Warm"
	}
	resource.Status.Cache.WorkersWarm = workload.Status.AvailableReplicas
	resource.Status.Replicas.Desired = desiredReplicas(workload)
	resource.Status.Replicas.Ready = workload.Status.ReadyReplicas
	previousRolloutStage := resource.Status.Rollout.Stage
	resource.Status.Rollout.Stage = string(decision.Stage)
	if previousRolloutStage != string(decision.Stage) || resource.Status.Rollout.StageStartedAt == nil {
		started := metav1.NewTime(now)
		resource.Status.Rollout.StageStartedAt = &started
	}
	resource.Status.Rollout.CandidateWeight = decision.CandidateWeight
	resource.Status.Rollout.RegressionWindows = decision.RegressionWindows
	if decision.LastRegressionAt.IsZero() {
		resource.Status.Rollout.LastRegressionWindow = nil
	} else {
		lastRegression := metav1.NewTime(decision.LastRegressionAt)
		resource.Status.Rollout.LastRegressionWindow = &lastRegression
	}
	if oldStable != "" && oldStable != revision.Name && !decision.Promote {
		resource.Status.Phase = platformv1alpha1.DeploymentPhaseUpdating
	} else {
		resource.Status.Phase = platformv1alpha1.DeploymentPhaseReady
	}
	if decision.Promote || resource.Status.Revision.Stable == "" {
		resource.Status.Revision.Stable = revision.Name
		resource.Status.Revision.Candidate = ""
		resource.Status.Revision.StableID = databaseRevisionID(resource)
		resource.Status.Revision.CandidateID = ""
		resource.Status.Revision.LastFailed = ""
		resource.Status.Revision.LastFailedID = ""
		resource.Status.Runtime = platformv1alpha1.RuntimeStatus{
			Backend: publicBackend(spec.ResolvedBackend), Version: spec.RuntimeVersion,
			ModelRevision: spec.ModelRevision, TensorParallelism: spec.TensorParallel,
		}
	} else if resource.Status.Revision.Stable != revision.Name {
		resource.Status.Revision.Candidate = revision.Name
		resource.Status.Revision.CandidateID = databaseRevisionID(resource)
	}
	if r.Config.PublicBaseURL != "" {
		resource.Status.Endpoint.URL = strings.TrimRight(r.Config.PublicBaseURL, "/") + routeConfig.Path
	}
	setCondition(resource, conditionRouteReady, metav1.ConditionTrue, routeReason, routeMessage, now)
	setCondition(resource, conditionRollout, conditionForDecision(decision), string(decision.Stage), decision.Reason, now)
	if err := r.persistStatus(ctx, resource, now); err != nil {
		return ctrl.Result{}, err
	}
	if decision.Promote && oldStable != "" && oldStable != revision.Name {
		// Persist the new stable identity before retiring the old one. A
		// restart or API update resumes cleanup through label discovery.
		return ctrl.Result{Requeue: true}, nil
	}
	if decision.RequeueAfter > 0 {
		return ctrl.Result{RequeueAfter: decision.RequeueAfter}, nil
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *Reconciler) defaults() {
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.Resolver == nil {
		r.Resolver = MeasuredBackendResolver{
			AutoEnabled: r.Config.BackendAutoEnabled, TensorRTEnabled: r.Config.TensorRTEnabled,
			ExpectedRuntimes: defaultRuntimeIdentities(r.Config.Images, r.Config.TensorRTEnabled),
		}
	}
	if r.Rollout.Policy.StageDuration == 0 {
		r.Rollout = rollout.Machine{Policy: rollout.DefaultPolicy()}
	}
	if r.Applier.Client == nil {
		r.Applier = kubeutil.Applier{Client: r.Client, Scheme: r.Scheme, FieldOwner: kubeutil.ManagedByValue}
	}
	if r.CacheRenderer.Config.Image == "" {
		r.CacheRenderer.Config.Image = r.Config.Images.ModelPrefetch
	}
	if r.Config.ControlNamespace == "" {
		r.Config.ControlNamespace = "inferscale-system"
	}
	if r.CacheRenderer.Config.CacheRoot == "" {
		r.CacheRenderer.Config.CacheRoot = r.Config.ModelCacheRoot
	}
	if r.CacheRenderer.Config.ImagePullSecret == "" {
		r.CacheRenderer.Config.ImagePullSecret = r.Config.ImagePullSecret
	}
	if r.CacheRenderer.Config.HFTokenSecret == "" {
		r.CacheRenderer.Config.HFTokenSecret = r.Config.HFTokenSecret
	}
	if r.Router.Config.EndpointPickerImage == "" {
		r.Router.Config.EndpointPickerImage = r.Config.Images.EndpointPicker
	}
	if r.Router.Config.ImagePullSecret == "" {
		r.Router.Config.ImagePullSecret = r.Config.ImagePullSecret
	}
	if r.Router.Config.GatewayName == "" {
		r.Router.Config.GatewayName = r.Config.GatewayName
	}
	if r.Router.Config.GatewayNamespace == "" {
		r.Router.Config.GatewayNamespace = r.Config.GatewayNamespace
	}
	if r.Router.Config.MonitoringNamespace == "" {
		r.Router.Config.MonitoringNamespace = r.Config.MonitoringNamespace
	}
	if r.Router.Config.OTLPEndpoint == "" {
		r.Router.Config.OTLPEndpoint = r.Config.OTLPEndpoint
	}
}

func (r *Reconciler) planRoute(
	ctx context.Context,
	resource *platformv1alpha1.InferenceDeployment,
	spec platformruntime.Spec,
	revision platformruntime.Revision,
	serviceName string,
	names routing.RevisionNames,
	candidateReady bool,
	now time.Time,
) (routing.RouteConfig, rollout.Decision, string, error) {
	config := routing.RouteConfig{
		DeploymentName: resource.Name, Namespace: resource.Namespace,
		GatewayName: r.Config.GatewayName, GatewayNamespace: r.Config.GatewayNamespace,
		Path: "/v1/deployments/" + publicDeploymentID(resource) + "/chat/completions",
	}
	oldStable := resource.Status.Revision.Stable
	if oldStable == "" || (oldStable == revision.Name && resource.Status.Revision.StableID == databaseRevisionID(resource)) {
		current := &routing.RouteRevision{Revision: revision, Names: names, ServiceName: serviceName, Weight: 100}
		config.Stable = current
		return config, rollout.Decision{Stage: rollout.StageStable, StableWeight: 0, CandidateWeight: 100, Promote: oldStable == ""}, oldStable, nil
	}
	if oldStable == revision.Name {
		// A forced backend:auto reselection can create a distinct immutable DB
		// revision whose resolved serving contract is byte-identical. There is
		// no second workload to canary, so promote the measured decision
		// atomically while reusing the already-stable Kubernetes resources.
		current := &routing.RouteRevision{Revision: revision, Names: names, ServiceName: serviceName, Weight: 100}
		config.Stable = current
		return config, rollout.Decision{
			Stage: rollout.StageStable, CandidateWeight: 100, Promote: true,
			Reason: "reselected backend resolved to the existing serving contract",
		}, oldStable, nil
	}

	stage := rollout.Stage(resource.Status.Rollout.Stage)
	if resource.Status.Revision.Candidate != revision.Name {
		stage = rollout.StagePending
	}
	state := rollout.State{
		Stage: stage, Since: rolloutStateSince(resource, stage, now),
		RegressionWindows: resource.Status.Rollout.RegressionWindows,
	}
	if resource.Status.Rollout.LastRegressionWindow != nil {
		state.LastRegressionAt = resource.Status.Rollout.LastRegressionWindow.Time
	}
	metrics := rollout.Metrics{}
	if r.RolloutMetrics != nil && stage != rollout.StagePending && stage != rollout.StageCandidateReady {
		var err error
		if provider, ok := r.RolloutMetrics.(RolloutStageMetricsProvider); ok {
			metrics, err = provider.SnapshotForRolloutStage(ctx, resource.Namespace, oldStable, revision.Name, stage, state.Since, now)
		} else if provider, ok := r.RolloutMetrics.(StageRolloutMetricsProvider); ok {
			metrics, err = provider.SnapshotForStage(ctx, resource.Namespace, oldStable, revision.Name, state.Since, now)
		} else {
			metrics, err = r.RolloutMetrics.Snapshot(ctx, resource.Namespace, oldStable, revision.Name, now)
		}
		if err != nil {
			source := rollout.MetricsSourceEndpointPicker
			if stage == rollout.StageShadow {
				source = rollout.MetricsSourceShadowRuntime
			}
			metrics = rollout.Metrics{Source: source, Available: false, UnavailableReason: boundedEvidenceText(err.Error())}
		}
	}
	if metrics.Available && spec.TTFT != nil && spec.TTFT.Percentile == 95 && metrics.CandidateTTFTP95MS > float64(spec.TTFT.TargetMS) {
		metrics.CandidateSLOViolation = true
	}
	if metrics.Available && spec.TPOT != nil && spec.TPOT.Percentile == 95 && metrics.CandidateTPOTP95MS > float64(spec.TPOT.TargetMS) {
		metrics.CandidateSLOViolation = true
	}
	decision := r.Rollout.Evaluate(state, candidateReady, metrics, now)
	if !candidateReady && stage != rollout.StagePending && !decision.Rollback {
		decision = rollout.Decision{Stage: rollout.StageFailed, StableWeight: 100, Rollback: true, Reason: "candidate lost runtime readiness"}
	}
	if !r.Config.ProgressiveRollout && candidateReady {
		decision = rollout.Decision{Stage: rollout.StageStable, CandidateWeight: 100, Promote: true, Reason: "progressive rollout feature is disabled"}
	}
	control := strings.ToLower(strings.TrimSpace(resource.Annotations[kubeutil.AnnotationRolloutControl]))
	switch control {
	case "", "resume":
	case "pause":
		// A pause blocks promotion but never suppresses an immediate safety
		// rollback produced by OOM, XID, or restart evidence.
		if !decision.Rollback {
			decision = holdRollout(stage, "operator paused rollout")
		}
	case "abort":
		decision = rollout.Decision{
			Stage: rollout.StageFailed, StableWeight: 100, CandidateWeight: 0,
			Rollback: true, Reason: "operator aborted rollout",
		}
	default:
		return routing.RouteConfig{}, rollout.Decision{}, oldStable, fmt.Errorf("unsupported rollout control %q", control)
	}
	if decision.Rollback {
		resource.Status.Rollout.LastRollback = rollbackEvidence(stage, state.Since, now, decision.Reason, metrics, r.Rollout.Policy)
	}
	config.Stable = &routing.RouteRevision{
		Revision: platformruntime.Revision{Name: oldStable}, Names: routing.Names(platformruntime.Revision{Name: oldStable}),
		ServiceName: kubeutil.ResourceName(oldStable, "runtime"), Weight: decision.StableWeight,
	}
	if !decision.Rollback && candidateReady {
		config.Candidate = &routing.RouteRevision{
			Revision: revision, Names: names, ServiceName: serviceName, Weight: decision.CandidateWeight,
		}
	}
	config.Shadow = decision.Shadow
	config.ShadowPercent = resource.Spec.Rollout.ShadowPercent
	return config, decision, oldStable, nil
}

func rollbackEvidence(stage rollout.Stage, windowStarted, observedAt time.Time, reason string, metrics rollout.Metrics, policy rollout.Policy) *platformv1alpha1.RollbackEvidenceStatus {
	source := string(metrics.Source)
	if source == "" {
		source = "unavailable"
	}
	return &platformv1alpha1.RollbackEvidenceStatus{
		Source: source, Stage: string(stage), ObservedAt: metav1.NewTime(observedAt), WindowStartedAt: metav1.NewTime(windowStarted),
		Available: metrics.Available, CandidateRequests: metrics.CandidateRequests,
		StableErrorRatePPM: ratePPM(metrics.StableErrorRate), CandidateErrorRatePPM: ratePPM(metrics.CandidateErrorRate),
		StableTTFTP95MS: roundedMetric(metrics.StableTTFTP95MS), CandidateTTFTP95MS: roundedMetric(metrics.CandidateTTFTP95MS),
		StableTPOTP95MS: roundedMetric(metrics.StableTPOTP95MS), CandidateTPOTP95MS: roundedMetric(metrics.CandidateTPOTP95MS),
		StableQueueP95MS: roundedMetric(metrics.StableQueueP95MS), CandidateQueueP95MS: roundedMetric(metrics.CandidateQueueP95MS),
		CandidateOOMs: metrics.CandidateOOMs, CandidateXIDErrors: metrics.CandidateXIDErrors,
		CandidateRestarts: metrics.CandidateRestarts, CandidateSLOViolation: metrics.CandidateSLOViolation,
		MaxCandidateErrorRatePPM: ratePPM(policy.MaxCandidateErrorRate), MaxErrorRateIncreasePPM: ratePPM(policy.MaxErrorRateIncrease),
		MaxTTFTRatioPPM: ratePPM(policy.MaxTTFTRatio), MaxTPOTRatioPPM: ratePPM(policy.MaxTPOTRatio),
		MaxQueueRatioPPM: ratePPM(policy.MaxQueueRatio), MaxRestarts: policy.MaxRestarts,
		UnavailableReason: boundedEvidenceText(metrics.UnavailableReason), Reason: boundedEvidenceText(reason),
	}
}

func ratePPM(value float64) int64 { return roundedMetric(value * 1_000_000) }

func roundedMetric(value float64) int64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return 0
	}
	if value >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(math.Round(value))
}

func boundedEvidenceText(value string) string {
	const maximumBytes = 256
	value = strings.TrimSpace(strings.ToValidUTF8(value, ""))
	if len(value) <= maximumBytes {
		return value
	}
	cut := maximumBytes
	for cut > 0 && !utf8.ValidString(value[:cut]) {
		cut--
	}
	return value[:cut]
}

func (r *Reconciler) applyAutoscaler(
	ctx context.Context,
	owner *platformv1alpha1.InferenceDeployment,
	spec platformruntime.Spec,
	revision, target string,
	names routing.RevisionNames,
	minReplicas, maxReplicas int32,
) error {
	if minReplicas == 0 && !r.Config.ScaleToZero {
		return fmt.Errorf("minReplicas=0 requires the scale-to-zero feature gate")
	}
	object, err := r.Autoscaler.Render(autoscaling.RenderConfig{
		Namespace: spec.Namespace, DeploymentName: spec.DeploymentName, Revision: revision,
		TargetDeployment: target, InferencePool: names.Pool, EndpointPickerSvc: names.EndpointPickerSvc,
		PrometheusURL: r.Config.PrometheusURL, MinReplicas: minReplicas, MaxReplicas: maxReplicas,
		Policy: autoscaling.DefaultPolicy(spec.MaxConcurrentRequests, spec.MaxQueuedRequests, maxReplicas),
	})
	if err != nil {
		return fmt.Errorf("render autoscaler: %w", err)
	}
	return r.Applier.Apply(ctx, owner, object)
}

func (r *Reconciler) prerequisitesReady(ctx context.Context, objects []client.Object) (bool, bool, string, error) {
	for _, object := range objects {
		if _, ok := object.(*batchv1.Job); !ok {
			continue
		}
		job := &batchv1.Job{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(object), job); err != nil {
			if apierrors.IsNotFound(err) {
				return false, false, "", nil
			}
			return false, false, "", err
		}
		complete, failed, message := modelcache.JobState(job)
		if failed || !complete {
			return false, failed, message, nil
		}
	}
	return true, false, "", nil
}

func (r *Reconciler) wait(
	ctx context.Context,
	resource *platformv1alpha1.InferenceDeployment,
	phase platformv1alpha1.DeploymentPhase,
	conditionType, reason, message string,
	now time.Time,
	after time.Duration,
) (ctrl.Result, error) {
	resource.Status.Phase = startupPhase(resource, phase)
	resource.Status.ObservedGeneration = resource.Generation
	setCondition(resource, conditionType, metav1.ConditionFalse, reason, message, now)
	if err := r.persistStatus(ctx, resource, now); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: after}, nil
}

func (r *Reconciler) fail(
	ctx context.Context,
	resource *platformv1alpha1.InferenceDeployment,
	phase platformv1alpha1.DeploymentPhase,
	reason, message string,
	now time.Time,
) (ctrl.Result, error) {
	resource.Status.Phase = startupPhase(resource, phase)
	resource.Status.ObservedGeneration = resource.Generation
	setCondition(resource, conditionRuntimeReady, metav1.ConditionFalse, reason, message, now)
	if err := r.persistStatus(ctx, resource, now); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *Reconciler) finalize(ctx context.Context, resource *platformv1alpha1.InferenceDeployment, now time.Time) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(resource, finalizerName) {
		return ctrl.Result{}, nil
	}
	resource.Status.Phase = platformv1alpha1.DeploymentPhaseDeleting
	resource.Status.ObservedGeneration = resource.Generation
	if err := r.persistStatus(ctx, resource, now); err != nil {
		return ctrl.Result{}, err
	}

	// Stop accepting traffic before owner-reference garbage collection removes
	// the runtime workloads. A subsequent reconciliation observes that the route
	// is gone and only then releases the deployment finalizer.
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute",
	})
	routeKey := types.NamespacedName{
		Namespace: resource.Namespace,
		Name:      kubeutil.ResourceName(resource.Name, "inference"),
	}
	if err := r.Get(ctx, routeKey, route); err == nil {
		if route.GetDeletionTimestamp().IsZero() {
			if err := r.Delete(ctx, route); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("delete public HTTPRoute: %w", err)
			}
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("get public HTTPRoute during finalization: %w", err)
	}

	// Remaining namespaced children are now safe for Kubernetes garbage
	// collection. The shared, content-addressed host cache is retained.
	controllerutil.RemoveFinalizer(resource, finalizerName)
	if err := r.Update(ctx, resource); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func publicDeploymentID(resource *platformv1alpha1.InferenceDeployment) string {
	if id := strings.TrimSpace(resource.Annotations[kubeutil.AnnotationDeploymentID]); id != "" {
		return id
	}
	return resource.Name
}

func databaseRevisionID(resource *platformv1alpha1.InferenceDeployment) string {
	return strings.TrimSpace(resource.Annotations[kubeutil.AnnotationRevisionID])
}

func (r *Reconciler) persistStatus(
	ctx context.Context,
	resource *platformv1alpha1.InferenceDeployment,
	now time.Time,
) error {
	if err := r.Status().Update(ctx, resource); err != nil {
		return err
	}
	if r.Projector == nil {
		return nil
	}
	deploymentID := strings.TrimSpace(resource.Annotations[kubeutil.AnnotationDeploymentID])
	if deploymentID == "" {
		return fmt.Errorf("database deployment ID annotation is required for status projection")
	}
	if err := r.Projector.ProjectControllerStatus(
		ctx, deploymentID, databaseRevisionID(resource), resource.Status,
		now,
	); err != nil {
		return fmt.Errorf("project controller status to PostgreSQL: %w", err)
	}
	return nil
}

func deploymentPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(update event.UpdateEvent) bool {
			oldResource, oldOK := update.ObjectOld.(*platformv1alpha1.InferenceDeployment)
			newResource, newOK := update.ObjectNew.(*platformv1alpha1.InferenceDeployment)
			if !oldOK || !newOK {
				return false
			}
			if oldResource.Generation != newResource.Generation {
				return true
			}
			if oldResource.DeletionTimestamp.IsZero() != newResource.DeletionTimestamp.IsZero() {
				return true
			}
			if oldResource.Annotations[kubeutil.AnnotationRevisionID] != newResource.Annotations[kubeutil.AnnotationRevisionID] {
				// An explicit backend:auto reselection creates a new immutable DB
				// revision even when the public spec is byte-identical, so the
				// Kubernetes generation legitimately remains unchanged.
				return true
			}
			return oldResource.Annotations[kubeutil.AnnotationRolloutControl] !=
				newResource.Annotations[kubeutil.AnnotationRolloutControl]
		},
	}
}

func setCondition(resource *platformv1alpha1.InferenceDeployment, conditionType string, status metav1.ConditionStatus, reason, message string, now time.Time) {
	kubeutil.SetCondition(&resource.Status.Conditions, metav1.Condition{
		Type: conditionType, Status: status, Reason: reason, Message: message,
		ObservedGeneration: resource.Generation,
	}, metav1.NewTime(now))
}

func currentConditionTrue(conditions []metav1.Condition, conditionType string, generation int64) bool {
	condition := kubeutil.FindCondition(conditions, conditionType)
	return condition != nil && condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == generation
}

func rolloutTransitionTime(resource *platformv1alpha1.InferenceDeployment, stage rollout.Stage, fallback time.Time) time.Time {
	if resource.Status.Rollout.Stage == string(stage) && resource.Status.Rollout.StageStartedAt != nil {
		return resource.Status.Rollout.StageStartedAt.Time
	}
	condition := kubeutil.FindCondition(resource.Status.Conditions, conditionRollout)
	if condition != nil && condition.Reason == string(stage) && condition.Status != metav1.ConditionUnknown {
		return condition.LastTransitionTime.Time
	}
	return fallback
}

func rolloutStateSince(resource *platformv1alpha1.InferenceDeployment, stage rollout.Stage, fallback time.Time) time.Time {
	since := rolloutTransitionTime(resource, stage, fallback)
	if strings.EqualFold(strings.TrimSpace(resource.Annotations[kubeutil.AnnotationRolloutControl]), "resume") {
		condition := kubeutil.FindCondition(resource.Status.Conditions, conditionRollout)
		if condition != nil && condition.Status == metav1.ConditionUnknown && condition.Message == "operator paused rollout" {
			// Operator-paused wall time is intentionally excluded. Automatic
			// insufficient-data pauses keep StageStartedAt so the stage-wide 200
			// completed-request gate can continue accumulating.
			started := metav1.NewTime(fallback)
			resource.Status.Rollout.StageStartedAt = &started
			return fallback
		}
	}
	return since
}

func conditionForDecision(decision rollout.Decision) metav1.ConditionStatus {
	if decision.Rollback {
		return metav1.ConditionFalse
	}
	if decision.Paused {
		return metav1.ConditionUnknown
	}
	return metav1.ConditionTrue
}

func desiredReplicas(deployment *appsv1.Deployment) int32 {
	if deployment.Spec.Replicas != nil {
		return *deployment.Spec.Replicas
	}
	return deployment.Status.Replicas
}

func workloadName(revision string, backend platformruntime.Backend) string {
	component := "vllm"
	if backend == platformruntime.BackendTRTLLM {
		component = "trtllm"
	}
	return kubeutil.ResourceName(revision, component)
}

func max32(first, second int32) int32 {
	if first > second {
		return first
	}
	return second
}

func holdRollout(stage rollout.Stage, reason string) rollout.Decision {
	if stage == "" {
		stage = rollout.StagePending
	}
	decision := rollout.Decision{Stage: stage, Paused: true, Reason: reason, RequeueAfter: time.Minute}
	switch stage {
	case rollout.StagePending, rollout.StageCandidateReady:
		decision.StableWeight, decision.CandidateWeight = 100, 0
	case rollout.StageShadow:
		decision.StableWeight, decision.CandidateWeight, decision.Shadow = 100, 0, true
	case rollout.StageCanary5:
		decision.StableWeight, decision.CandidateWeight = 95, 5
	case rollout.StageCanary25:
		decision.StableWeight, decision.CandidateWeight = 75, 25
	case rollout.StageCanary50:
		decision.StableWeight, decision.CandidateWeight = 50, 50
	case rollout.StageCanary100, rollout.StageStable:
		decision.StableWeight, decision.CandidateWeight = 0, 100
	case rollout.StageFailed:
		decision.StableWeight, decision.CandidateWeight, decision.Rollback = 100, 0, true
	}
	return decision
}
