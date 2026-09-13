package postgres

import (
	"bytes"
	"testing"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	"github.com/inferscale/inferscale/internal/deployment"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestControllerState(t *testing.T) {
	t.Parallel()
	tests := map[platformv1alpha1.DeploymentPhase]deployment.State{
		platformv1alpha1.DeploymentPhasePending:   deployment.StatePending,
		platformv1alpha1.DeploymentPhaseReady:     deployment.StateReady,
		platformv1alpha1.DeploymentPhaseUpdating:  deployment.StateUpdating,
		platformv1alpha1.DeploymentPhaseDegraded:  deployment.StateDegraded,
		platformv1alpha1.DeploymentPhaseFailed:    deployment.StateFailed,
		platformv1alpha1.DeploymentPhaseDeleting:  deployment.StateDeleting,
		platformv1alpha1.DeploymentPhaseDeploying: deployment.StatePending,
	}
	for phase, want := range tests {
		if got := controllerState(phase); got != want {
			t.Fatalf("controllerState(%q)=%q, want %q", phase, got, want)
		}
	}
}

func TestRolloutHistoryDetailsRetainsBoundedRollbackEvidence(t *testing.T) {
	t.Parallel()
	now := metav1.NewTime(time.Unix(1_700_000_000, 0).UTC())
	status := platformv1alpha1.InferenceDeploymentStatus{Rollout: platformv1alpha1.RolloutStatus{
		Stage: "Failed",
		LastRollback: &platformv1alpha1.RollbackEvidenceStatus{
			Source: "shadow-runtime", Stage: "Shadow", ObservedAt: now, WindowStartedAt: now,
			Available: true, CandidateRequests: 203, CandidateOOMs: 1,
			CandidateErrorRatePPM: 10_000, CandidateTTFTP95MS: 42, Reason: "candidate reported an OOM",
		},
	}}
	details, err := rolloutHistoryDetails(
		status,
		&metav1.Condition{Type: "RolloutProgressing", Reason: "Failed"},
		controllerTransition{Rollback: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range [][]byte{
		[]byte(`"lastRollback"`), []byte(`"source":"shadow-runtime"`),
		[]byte(`"candidateRequests":203`), []byte(`"candidateOoms":1`),
		[]byte(`"rollback":true`),
	} {
		if !bytes.Contains(details, expected) {
			t.Fatalf("rollout history details omit %s: %s", expected, details)
		}
	}
}

func TestInferControllerTransitionReplaysDurablePromotionOnce(t *testing.T) {
	t.Parallel()
	status := platformv1alpha1.InferenceDeploymentStatus{
		Revision: platformv1alpha1.RevisionStatus{StableID: "candidate"},
		Rollout:  platformv1alpha1.RolloutStatus{Stage: "Stable"},
	}
	transition := inferControllerTransition(
		status, "candidate", "old-stable", "candidate", deployment.RevisionCandidate,
	)
	if !transition.Promote || transition.Rollback {
		t.Fatalf("replayed promotion transition=%#v", transition)
	}

	// Once the transaction commits, the deployment candidate pointer is clear
	// and the revision is stable. Re-projecting the same Kubernetes status must
	// not emit another transition/history marker.
	transition = inferControllerTransition(
		status, "candidate", "candidate", "", deployment.RevisionStable,
	)
	if transition != (controllerTransition{}) {
		t.Fatalf("already-applied promotion transition=%#v", transition)
	}
}

func TestInferControllerTransitionRecoversInitialPrerequisiteFailure(t *testing.T) {
	t.Parallel()
	ready := platformv1alpha1.InferenceDeploymentStatus{
		Phase:    platformv1alpha1.DeploymentPhaseReady,
		Revision: platformv1alpha1.RevisionStatus{StableID: "first"},
		Rollout:  platformv1alpha1.RolloutStatus{Stage: "Stable"},
	}
	if got := inferControllerTransition(ready, "first", "", "first", deployment.RevisionFailed); !got.Promote {
		t.Fatalf("recovered initial revision was not promoted: %#v", got)
	}
	for name, mutate := range map[string]func(*platformv1alpha1.InferenceDeploymentStatus){
		"still failed":   func(s *platformv1alpha1.InferenceDeploymentStatus) { s.Phase = platformv1alpha1.DeploymentPhaseFailed },
		"rollout failed": func(s *platformv1alpha1.InferenceDeploymentStatus) { s.Rollout.Stage = "Failed" },
		"rollback evidence": func(s *platformv1alpha1.InferenceDeploymentStatus) {
			s.Rollout.LastRollback = &platformv1alpha1.RollbackEvidenceStatus{}
		},
		"failed tombstone":  func(s *platformv1alpha1.InferenceDeploymentStatus) { s.Revision.LastFailedID = "first" },
		"candidate remains": func(s *platformv1alpha1.InferenceDeploymentStatus) { s.Revision.CandidateID = "first" },
		"different stable":  func(s *platformv1alpha1.InferenceDeploymentStatus) { s.Revision.StableID = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			status := ready
			mutate(&status)
			if got := inferControllerTransition(status, "first", "", "first", deployment.RevisionFailed); got.Promote {
				t.Fatalf("incomplete recovery promoted: %#v", got)
			}
		})
	}
	if got := inferControllerTransition(ready, "first", "old-stable", "first", deployment.RevisionFailed); got.Promote {
		t.Fatal("failed rollout candidate was resurrected")
	}
	if got := inferControllerTransition(ready, "first", "first", "", deployment.RevisionStable); got != (controllerTransition{}) {
		t.Fatalf("recovered promotion was replayed: %#v", got)
	}
}

func TestInferControllerTransitionReplaysDurableRollbackOnce(t *testing.T) {
	t.Parallel()
	now := metav1.NewTime(time.Unix(1_700_000_000, 0).UTC())
	status := platformv1alpha1.InferenceDeploymentStatus{
		Revision: platformv1alpha1.RevisionStatus{
			StableID: "stable", CandidateID: "candidate",
		},
		Rollout: platformv1alpha1.RolloutStatus{
			Stage: "Failed",
			LastRollback: &platformv1alpha1.RollbackEvidenceStatus{
				Source: "endpoint-picker", Stage: "Canary5",
				ObservedAt: now, WindowStartedAt: now, Reason: "candidate regression",
			},
		},
	}
	transition := inferControllerTransition(
		status, "candidate", "stable", "candidate", deployment.RevisionCandidate,
	)
	if transition.Promote || !transition.Rollback {
		t.Fatalf("replayed rollback transition=%#v", transition)
	}

	// Rollback intentionally retains the deployment's candidate pointer for the
	// inspection window, so the immutable revision state is the one-shot guard.
	transition = inferControllerTransition(
		status, "candidate", "stable", "candidate", deployment.RevisionFailed,
	)
	if transition != (controllerTransition{}) {
		t.Fatalf("already-applied rollback transition=%#v", transition)
	}
}

func TestInferControllerTransitionRequiresCompleteDurableRollbackEvidence(t *testing.T) {
	t.Parallel()
	base := platformv1alpha1.InferenceDeploymentStatus{
		Revision: platformv1alpha1.RevisionStatus{StableID: "stable", CandidateID: "candidate"},
		Rollout: platformv1alpha1.RolloutStatus{
			Stage: "Failed", LastRollback: &platformv1alpha1.RollbackEvidenceStatus{},
		},
	}
	tests := map[string]platformv1alpha1.InferenceDeploymentStatus{
		"missing evidence": func() platformv1alpha1.InferenceDeploymentStatus {
			value := base
			value.Rollout.LastRollback = nil
			return value
		}(),
		"non-failed stage": func() platformv1alpha1.InferenceDeploymentStatus {
			value := base
			value.Rollout.Stage = "Canary5"
			return value
		}(),
		"wrong stable identity": func() platformv1alpha1.InferenceDeploymentStatus {
			value := base
			value.Revision.StableID = "other-stable"
			return value
		}(),
		"wrong candidate identity": func() platformv1alpha1.InferenceDeploymentStatus {
			value := base
			value.Revision.CandidateID = "other-candidate"
			return value
		}(),
	}
	for name, status := range tests {
		status := status
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			transition := inferControllerTransition(
				status, "candidate", "stable", "candidate", deployment.RevisionCandidate,
			)
			if transition != (controllerTransition{}) {
				t.Fatalf("transition=%#v", transition)
			}
		})
	}
}

func TestInferControllerTransitionRetiresFailedCandidateOnce(t *testing.T) {
	t.Parallel()
	status := platformv1alpha1.InferenceDeploymentStatus{
		Revision: platformv1alpha1.RevisionStatus{
			StableID: "stable", LastFailed: "chat-failed", LastFailedID: "candidate",
		},
		Rollout: platformv1alpha1.RolloutStatus{Stage: "Failed"},
	}
	transition := inferControllerTransition(
		status, "candidate", "stable", "candidate", deployment.RevisionFailed,
	)
	if transition.Promote || transition.Rollback || !transition.RetireCandidate {
		t.Fatalf("failed-candidate retirement transition=%#v", transition)
	}

	// PostgreSQL clears candidate_revision_id in the retirement transaction.
	// Replaying the same durable tombstone must not emit another transition or
	// duplicate its rollout-history marker.
	transition = inferControllerTransition(
		status, "candidate", "stable", "", deployment.RevisionFailed,
	)
	if transition != (controllerTransition{}) {
		t.Fatalf("already-retired candidate transition=%#v", transition)
	}
}

func TestRolloutConditionSelectsOnlyControllerEvidence(t *testing.T) {
	t.Parallel()
	conditions := []metav1.Condition{
		{Type: "RuntimeReady", Reason: "Ready"},
		{Type: "RolloutProgressing", Reason: "Canary25", Message: "gates pending"},
	}
	condition := rolloutCondition(conditions)
	if condition == nil || condition.Reason != "Canary25" || condition.Message != "gates pending" {
		t.Fatalf("condition=%#v", condition)
	}
}
