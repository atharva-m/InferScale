package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	"github.com/inferscale/inferscale/internal/deployment"
	"github.com/inferscale/inferscale/internal/rollout"
	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	"github.com/jackc/pgx/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LoadResolution returns the immutable backend decision for one PostgreSQL
// revision. Kubernetes revision names are deliberately never accepted here.
func (r *DeploymentRepository) LoadResolution(ctx context.Context, deploymentID, revisionID string) (platformruntime.BackendResolution, bool, error) {
	var resolution platformruntime.BackendResolution
	err := r.store.pool.QueryRow(ctx, `
		SELECT COALESCE(resolved_backend,''), COALESCE(selected_profile_id,''),
		       selection_status, COALESCE(runtime_image_digest,'')
		FROM deployment_revisions WHERE deployment_id=$1 AND id=$2`,
		deploymentID, revisionID).Scan(
		&resolution.Backend, &resolution.ProfileID, &resolution.SelectionStatus,
		&resolution.RuntimeImageDigest,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return platformruntime.BackendResolution{}, false, deployment.ErrNotFound
	}
	if err != nil {
		return platformruntime.BackendResolution{}, false, err
	}
	return resolution, resolution.Backend != "", nil
}

// FreezeResolution uses a compare-and-set update. If another controller
// instance won the race, the already-persisted decision is returned unchanged.
func (r *DeploymentRepository) FreezeResolution(ctx context.Context, deploymentID, revisionID string, proposed platformruntime.BackendResolution) (platformruntime.BackendResolution, error) {
	if proposed.Backend == "" || proposed.Backend == platformruntime.BackendAuto || proposed.SelectionStatus == "" || proposed.RuntimeImageDigest == "" {
		return platformruntime.BackendResolution{}, fmt.Errorf("complete concrete backend resolution is required")
	}
	var frozen platformruntime.BackendResolution
	err := r.store.pool.QueryRow(ctx, `
		UPDATE deployment_revisions SET
			resolved_backend=$3, selection_status=$4,
			selected_profile_id=NULLIF($5,''), runtime_image_digest=$6
		WHERE deployment_id=$1 AND id=$2 AND resolved_backend IS NULL
		  AND ($5='' OR EXISTS (
			SELECT 1 FROM runtime_profiles p
			WHERE p.id=$5 AND p.eligible=true AND p.approved_at IS NOT NULL
		  ))
		RETURNING resolved_backend, COALESCE(selected_profile_id,''),
		          selection_status, runtime_image_digest`,
		deploymentID, revisionID, proposed.Backend, proposed.SelectionStatus,
		proposed.ProfileID, proposed.RuntimeImageDigest,
	).Scan(&frozen.Backend, &frozen.ProfileID, &frozen.SelectionStatus, &frozen.RuntimeImageDigest)
	if err == nil {
		return frozen, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return platformruntime.BackendResolution{}, err
	}
	existing, found, loadErr := r.LoadResolution(ctx, deploymentID, revisionID)
	if loadErr != nil {
		return platformruntime.BackendResolution{}, loadErr
	}
	if !found {
		return platformruntime.BackendResolution{}, fmt.Errorf("revision resolution was not frozen")
	}
	return existing, nil
}

// ProjectControllerStatus copies the live CR status into PostgreSQL and
// atomically transitions the matching immutable DB revision by UUID. The
// Kubernetes status contains human-readable revision names; those are retained
// only inside observed_status and never written into revision-ID columns.
func (r *DeploymentRepository) ProjectControllerStatus(
	ctx context.Context,
	deploymentID, revisionID string,
	status platformv1alpha1.InferenceDeploymentStatus,
	at time.Time,
) error {
	payload, err := json.Marshal(status)
	if err != nil {
		return err
	}
	tx, err := r.store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var stableRevisionID, candidateRevisionID string
	var previousStatusJSON []byte
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(stable_revision_id,''), COALESCE(candidate_revision_id,''), observed_status
		FROM deployments WHERE id=$1 FOR UPDATE`, deploymentID,
	).Scan(&stableRevisionID, &candidateRevisionID, &previousStatusJSON); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return deployment.ErrNotFound
		}
		return err
	}
	var revisionState deployment.RevisionState
	if revisionID != "" {
		if err := tx.QueryRow(ctx, `
			SELECT state FROM deployment_revisions
			WHERE deployment_id=$1 AND id=$2 FOR UPDATE`, deploymentID, revisionID,
		).Scan(&revisionState); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return deployment.ErrNotFound
			}
			return err
		}
	}
	activeRevision := revisionID == "" || revisionID == stableRevisionID || revisionID == candidateRevisionID
	failedTombstone := revisionID != "" && revisionState == deployment.RevisionFailed &&
		status.Revision.CandidateID == "" && status.Revision.LastFailedID == revisionID
	if !activeRevision && !failedTombstone {
		return fmt.Errorf("%w: revision %s is not active for deployment %s", deployment.ErrNotFound, revisionID, deploymentID)
	}
	transition := inferControllerTransition(
		status, revisionID, stableRevisionID, candidateRevisionID, revisionState,
	)

	if transition.Promote {
		if revisionID == "" || (revisionID != candidateRevisionID && revisionID != stableRevisionID) {
			return fmt.Errorf("cannot promote an unmapped deployment revision")
		}
		if stableRevisionID != "" && stableRevisionID != revisionID {
			if _, err := tx.Exec(ctx, `UPDATE deployment_revisions SET state=$3 WHERE deployment_id=$1 AND id=$2`, deploymentID, stableRevisionID, deployment.RevisionRetired); err != nil {
				return err
			}
		}
		command, err := tx.Exec(ctx, `UPDATE deployment_revisions SET state=$3 WHERE deployment_id=$1 AND id=$2`, deploymentID, revisionID, deployment.RevisionStable)
		if err != nil {
			return err
		}
		if command.RowsAffected() != 1 {
			return deployment.ErrNotFound
		}
		stableRevisionID, candidateRevisionID = revisionID, ""
	} else if transition.Rollback {
		if _, err := tx.Exec(ctx, `UPDATE deployment_revisions SET state=$3 WHERE deployment_id=$1 AND id=$2`, deploymentID, revisionID, deployment.RevisionFailed); err != nil {
			return err
		}
	} else if transition.RetireCandidate {
		candidateRevisionID = ""
	} else if candidateRevisionID == revisionID && revisionState == deployment.RevisionCandidate && status.Phase == platformv1alpha1.DeploymentPhaseFailed {
		if _, err := tx.Exec(ctx, `UPDATE deployment_revisions SET state=$3 WHERE deployment_id=$1 AND id=$2`, deploymentID, revisionID, deployment.RevisionFailed); err != nil {
			return err
		}
	}

	command, err := tx.Exec(ctx, `
		UPDATE deployments SET observed_status=$2, state=$3, stable_revision_id=NULLIF($4,''),
			candidate_revision_id=NULLIF($5,''), updated_at=$6, last_sync_error=''
		WHERE id=$1`, deploymentID, payload, controllerState(status.Phase),
		stableRevisionID, candidateRevisionID, at)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return deployment.ErrNotFound
	}
	var previousStatus platformv1alpha1.InferenceDeploymentStatus
	if len(previousStatusJSON) > 0 {
		if err := json.Unmarshal(previousStatusJSON, &previousStatus); err != nil {
			return fmt.Errorf("decode previous controller status: %w", err)
		}
	}
	previousCondition := rolloutCondition(previousStatus.Conditions)
	currentCondition := rolloutCondition(status.Conditions)
	if !reflect.DeepEqual(previousStatus.Revision, status.Revision) ||
		!reflect.DeepEqual(previousStatus.Rollout, status.Rollout) ||
		!reflect.DeepEqual(previousCondition, currentCondition) {
		details, err := rolloutHistoryDetails(status, currentCondition, transition)
		if err != nil {
			return err
		}
		reason := ""
		if currentCondition != nil {
			reason = currentCondition.Message
			if reason == "" {
				reason = currentCondition.Reason
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO rollout_history (
				deployment_id, stable_revision_id, candidate_revision_id,
				stage, reason, details, created_at
			) VALUES ($1,NULLIF($2,''),NULLIF($3,''),$4,$5,$6,$7)`,
			deploymentID, stableRevisionID, candidateRevisionID,
			status.Rollout.Stage, reason, details, at,
		); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

type controllerTransition struct {
	Promote         bool
	Rollback        bool
	RetireCandidate bool
}

// inferControllerTransition derives a one-shot database transition from the
// status already durably written to Kubernetes and the revision state locked in
// this transaction. This makes a projection replay safe when the status update
// succeeds but the first PostgreSQL attempt fails: the next reconciliation no
// longer needs to reproduce an ephemeral rollout Decision boolean.
func inferControllerTransition(
	status platformv1alpha1.InferenceDeploymentStatus,
	revisionID, stableRevisionID, candidateRevisionID string,
	revisionState deployment.RevisionState,
) controllerTransition {
	if revisionID == "" || candidateRevisionID != revisionID {
		return controllerTransition{}
	}
	// The first revision can recover after a failed prerequisite (for example,
	// a retried model download). There is no serving revision to roll back to,
	// and the controller keeps reconciling that same immutable revision. Only
	// complete, stable readiness may recover its failed database state; a failed
	// rollout with an existing stable revision must retain its tombstone.
	if revisionState == deployment.RevisionFailed && stableRevisionID == "" &&
		status.Phase == platformv1alpha1.DeploymentPhaseReady &&
		status.Rollout.Stage == string(rollout.StageStable) &&
		status.Rollout.LastRollback == nil && status.Revision.LastFailedID == "" &&
		status.Revision.StableID == revisionID && status.Revision.CandidateID == "" {
		return controllerTransition{Promote: true}
	}
	if revisionState == deployment.RevisionCandidate {
		if status.Revision.StableID == revisionID && status.Revision.CandidateID == "" {
			return controllerTransition{Promote: true}
		}
		if stableRevisionID != "" && status.Revision.StableID == stableRevisionID &&
			status.Revision.CandidateID == revisionID &&
			status.Rollout.Stage == string(rollout.StageFailed) && status.Rollout.LastRollback != nil {
			return controllerTransition{Rollback: true}
		}
	}
	if revisionState == deployment.RevisionFailed && stableRevisionID != "" &&
		status.Revision.StableID == stableRevisionID && status.Revision.CandidateID == "" &&
		status.Revision.LastFailedID == revisionID {
		return controllerTransition{RetireCandidate: true}
	}
	return controllerTransition{}
}

func rolloutHistoryDetails(
	status platformv1alpha1.InferenceDeploymentStatus,
	condition *metav1.Condition,
	transition controllerTransition,
) ([]byte, error) {
	return json.Marshal(map[string]any{
		"rollout": status.Rollout, "condition": condition,
		"promote": transition.Promote, "rollback": transition.Rollback,
		"retire_candidate": transition.RetireCandidate,
	})
}

func rolloutCondition(conditions []metav1.Condition) *metav1.Condition {
	for index := range conditions {
		if conditions[index].Type == "RolloutProgressing" {
			condition := conditions[index]
			return &condition
		}
	}
	return nil
}

func controllerState(phase platformv1alpha1.DeploymentPhase) deployment.State {
	switch phase {
	case platformv1alpha1.DeploymentPhaseReady:
		return deployment.StateReady
	case platformv1alpha1.DeploymentPhaseUpdating:
		return deployment.StateUpdating
	case platformv1alpha1.DeploymentPhaseDegraded:
		return deployment.StateDegraded
	case platformv1alpha1.DeploymentPhaseFailed:
		return deployment.StateFailed
	case platformv1alpha1.DeploymentPhaseDeleting:
		return deployment.StateDeleting
	default:
		return deployment.StatePending
	}
}
