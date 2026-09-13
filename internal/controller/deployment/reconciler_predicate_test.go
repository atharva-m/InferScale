package deployment

import (
	"context"
	"testing"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	"github.com/inferscale/inferscale/internal/rollout"
	"github.com/inferscale/inferscale/internal/routing"
	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

type fixedRolloutMetrics struct{ metrics rollout.Metrics }

func (f fixedRolloutMetrics) Snapshot(context.Context, string, string, string, time.Time) (rollout.Metrics, error) {
	return f.metrics, nil
}

func TestDeploymentPredicateAcceptsDeletionAndRolloutControl(t *testing.T) {
	t.Parallel()
	predicate := deploymentPredicate()
	base := &platformv1alpha1.InferenceDeployment{ObjectMeta: metav1.ObjectMeta{
		Name: "chat", Namespace: "tenant-a", Generation: 4,
	}}
	statusOnly := base.DeepCopy()
	statusOnly.Status.Phase = platformv1alpha1.DeploymentPhaseReady
	if predicate.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: statusOnly}) {
		t.Fatal("status-only updates must not trigger the primary watch")
	}

	deleting := base.DeepCopy()
	stamp := metav1.NewTime(time.Now())
	deleting.DeletionTimestamp = &stamp
	if !predicate.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: deleting}) {
		t.Fatal("deletion timestamp transition must trigger finalization")
	}

	paused := base.DeepCopy()
	paused.Annotations = map[string]string{kubeutil.AnnotationRolloutControl: "pause"}
	if !predicate.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: paused}) {
		t.Fatal("rollout control annotation must trigger reconciliation")
	}

	reselected := base.DeepCopy()
	reselected.Annotations = map[string]string{kubeutil.AnnotationRevisionID: "new-revision-uuid"}
	if !predicate.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: reselected}) {
		t.Fatal("metadata-only backend:auto reselection must trigger reconciliation")
	}
}

func TestForcedReselectionPromotesIdenticalServingContractByDatabaseID(t *testing.T) {
	t.Parallel()
	revision := platformruntime.Revision{Name: "chat-immutable", Digest: "digest"}
	resource := &platformv1alpha1.InferenceDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "chat", Namespace: "tenant-a",
			Annotations: map[string]string{
				kubeutil.AnnotationDeploymentID: "deployment-uuid",
				kubeutil.AnnotationRevisionID:   "new-revision-uuid",
			},
		},
		Status: platformv1alpha1.InferenceDeploymentStatus{
			Revision: platformv1alpha1.RevisionStatus{
				Stable: revision.Name, StableID: "old-revision-uuid",
			},
		},
	}
	config, decision, oldStable, err := (&Reconciler{Config: Config{GatewayName: "gateway"}}).planRoute(
		context.Background(), resource, platformruntime.Spec{}, revision,
		"chat-runtime", routing.Names(revision), true, time.Unix(1, 0),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Promote || decision.Stage != rollout.StageStable || oldStable != revision.Name {
		t.Fatalf("decision=%#v oldStable=%q", decision, oldStable)
	}
	if config.Stable == nil || config.Stable.Revision.Name != revision.Name || config.Candidate != nil {
		t.Fatalf("route config=%#v", config)
	}
}

func TestHoldRolloutPreservesTrafficWeights(t *testing.T) {
	t.Parallel()
	decision := holdRollout(rollout.StageCanary25, "paused")
	if !decision.Paused || decision.StableWeight != 75 || decision.CandidateWeight != 25 || decision.Rollback {
		t.Fatalf("decision=%#v", decision)
	}
	failed := holdRollout(rollout.StageFailed, "paused")
	if !failed.Rollback || failed.CandidateWeight != 0 || failed.StableWeight != 100 {
		t.Fatalf("failed decision=%#v", failed)
	}
}

func TestRollbackRouteExcludesFailedCandidateAndCapturesEvidence(t *testing.T) {
	t.Parallel()
	now := time.Unix(2_000, 0).UTC()
	started := metav1.NewTime(now.Add(-time.Minute))
	revision := platformruntime.Revision{Name: "chat-candidate", Digest: "candidate-digest"}
	resource := &platformv1alpha1.InferenceDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "chat", Namespace: "tenant-a"},
		Status: platformv1alpha1.InferenceDeploymentStatus{
			Revision: platformv1alpha1.RevisionStatus{Stable: "chat-stable", Candidate: revision.Name},
			Rollout:  platformv1alpha1.RolloutStatus{Stage: string(rollout.StageCanary5), StageStartedAt: &started},
		},
	}
	reconciler := &Reconciler{
		Config:  Config{GatewayName: "gateway", ProgressiveRollout: true},
		Rollout: rollout.Machine{Policy: rollout.DefaultPolicy()},
		RolloutMetrics: fixedRolloutMetrics{metrics: rollout.Metrics{
			Source: rollout.MetricsSourceEndpointPicker, Available: true,
			CandidateRequests: 210, CandidateOOMs: 1,
		}},
	}
	config, decision, _, err := reconciler.planRoute(
		context.Background(), resource, platformruntime.Spec{}, revision,
		"chat-candidate-runtime", routing.Names(revision), true, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Rollback || config.Stable == nil || config.Candidate != nil {
		t.Fatalf("rollback decision/config=%#v %#v", decision, config)
	}
	evidence := resource.Status.Rollout.LastRollback
	if evidence == nil || evidence.Source != "endpoint-picker" || evidence.Stage != "Canary5" || evidence.CandidateRequests != 210 || evidence.CandidateOOMs != 1 {
		t.Fatalf("rollback evidence=%#v", evidence)
	}
}

func TestResumeRestartsStageSoPausedTimeDoesNotCount(t *testing.T) {
	t.Parallel()
	pausedAt := time.Unix(1000, 0)
	resumedAt := pausedAt.Add(time.Hour)
	started := metav1.NewTime(pausedAt)
	resource := &platformv1alpha1.InferenceDeployment{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{kubeutil.AnnotationRolloutControl: "resume"}},
		Status: platformv1alpha1.InferenceDeploymentStatus{
			Rollout: platformv1alpha1.RolloutStatus{Stage: string(rollout.StageCanary25), StageStartedAt: &started},
			Conditions: []metav1.Condition{{
				Type: conditionRollout, Status: metav1.ConditionUnknown,
				Reason: string(rollout.StageCanary25), Message: "operator paused rollout", LastTransitionTime: metav1.NewTime(pausedAt),
			}},
		},
	}
	if got := rolloutStateSince(resource, rollout.StageCanary25, resumedAt); !got.Equal(resumedAt) {
		t.Fatalf("resume transition time=%s, want %s", got, resumedAt)
	}
	if resource.Status.Rollout.StageStartedAt == nil || !resource.Status.Rollout.StageStartedAt.Time.Equal(resumedAt) {
		t.Fatalf("stage start was not reset on operator resume: %#v", resource.Status.Rollout.StageStartedAt)
	}
}

func TestAutomaticSamplePauseKeepsStageWideWindow(t *testing.T) {
	t.Parallel()
	startedAt := time.Unix(1000, 0)
	now := startedAt.Add(20 * time.Minute)
	started := metav1.NewTime(startedAt)
	resource := &platformv1alpha1.InferenceDeployment{Status: platformv1alpha1.InferenceDeploymentStatus{
		Rollout: platformv1alpha1.RolloutStatus{Stage: string(rollout.StageShadow), StageStartedAt: &started},
		Conditions: []metav1.Condition{{
			Type: conditionRollout, Status: metav1.ConditionUnknown,
			Reason: string(rollout.StageShadow), Message: "insufficient candidate samples", LastTransitionTime: metav1.NewTime(startedAt.Add(10 * time.Minute)),
		}},
	}}
	if got := rolloutStateSince(resource, rollout.StageShadow, now); !got.Equal(startedAt) {
		t.Fatalf("automatic pause reset stage window to %s, want %s", got, startedAt)
	}
}
