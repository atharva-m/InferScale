package deployment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type State string

const (
	StatePending  State = "Pending"
	StateReady    State = "Ready"
	StateUpdating State = "Updating"
	StateDegraded State = "Degraded"
	StateFailed   State = "Failed"
	StateDeleting State = "Deleting"
)

type RevisionState string

type OperationKind string
type OperationStatus string

const (
	OperationCreate    OperationKind   = "create"
	OperationUpdate    OperationKind   = "update"
	OperationDelete    OperationKind   = "delete"
	OperationAccepted  OperationStatus = "accepted"
	OperationRetrying  OperationStatus = "retrying"
	OperationSucceeded OperationStatus = "succeeded"
	OperationFailed    OperationStatus = "failed"
)

const (
	RevisionCandidate RevisionState = "candidate"
	RevisionStable    RevisionState = "stable"
	RevisionFailed    RevisionState = "failed"
	RevisionRetired   RevisionState = "retired"
)

type Deployment struct {
	ID                  string
	TenantID            string
	Name                string
	Namespace           string
	Generation          int64
	Spec                platformv1alpha1.InferenceDeploymentSpec
	State               State
	StableRevisionID    string
	CandidateRevisionID string
	ObservedStatus      platformv1alpha1.InferenceDeploymentStatus
	LastSyncError       string
	CreatedAt           time.Time
	UpdatedAt           time.Time
	DeletedAt           *time.Time
}

type Revision struct {
	ID                 string
	DeploymentID       string
	Number             int64
	ServingDigest      string
	Spec               platformv1alpha1.InferenceDeploymentSpec
	State              RevisionState
	RequestedBackend   platformv1alpha1.RuntimeBackend
	ResolvedBackend    platformv1alpha1.RuntimeBackend
	SelectionStatus    string
	SelectedProfileID  string
	RuntimeImageDigest string
	CreatedAt          time.Time
}

type Operation struct {
	ID             string          `json:"id"`
	TenantID       string          `json:"tenantId"`
	DeploymentID   string          `json:"deploymentId"`
	Kind           OperationKind   `json:"kind"`
	Status         OperationStatus `json:"status"`
	RequestID      string          `json:"requestId,omitempty"`
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
	CompletedAt    *time.Time      `json:"completedAt,omitempty"`
	Error          string          `json:"error,omitempty"`
}

type Mutation struct {
	Deployment *Deployment `json:"-"`
	Operation  *Operation  `json:"operation"`
}

type CreateInput struct {
	TenantID       string
	Namespace      string
	Name           string
	Spec           platformv1alpha1.InferenceDeploymentSpec
	RequestID      string
	IdempotencyKey string
}

type UpdateInput struct {
	TenantID           string
	DeploymentID       string
	ExpectedGeneration int64
	Spec               platformv1alpha1.InferenceDeploymentSpec
	RequestID          string
	IdempotencyKey     string
	ForceReselect      bool
}

type DeleteInput struct {
	TenantID           string
	DeploymentID       string
	ExpectedGeneration int64
	RequestID          string
	IdempotencyKey     string
}

type ListOptions struct {
	Limit  int
	Offset int
	Cursor *ListCursor
}

type ListCursor struct {
	CreatedAt time.Time
	ID        string
}

type ListPage struct {
	Items      []Deployment
	NextCursor *ListCursor
}

func New(input CreateInput, now time.Time) (*Deployment, *Revision, error) {
	d := &Deployment{
		ID:         newID("dep"),
		TenantID:   input.TenantID,
		Name:       input.Name,
		Namespace:  input.Namespace,
		Generation: 1,
		Spec:       input.Spec,
		State:      StatePending,
		CreatedAt:  now.UTC(),
		UpdatedAt:  now.UTC(),
	}
	if err := Validate(d); err != nil {
		return nil, nil, err
	}
	revision := &Revision{
		ID:               newID("rev"),
		DeploymentID:     d.ID,
		Number:           1,
		ServingDigest:    ServingDigest(d.Spec),
		Spec:             d.Spec,
		State:            RevisionCandidate,
		RequestedBackend: d.Spec.Runtime.Backend,
		SelectionStatus:  "pending",
		CreatedAt:        now.UTC(),
	}
	d.CandidateRevisionID = revision.ID
	return d, revision, nil
}

func Validate(d *Deployment) error {
	if d == nil {
		return fmt.Errorf("%w: deployment is required", ErrInvalid)
	}
	if d.TenantID == "" {
		return fmt.Errorf("%w: tenant is required", ErrInvalid)
	}
	if d.Namespace == "" {
		return fmt.Errorf("%w: namespace is required", ErrInvalid)
	}
	resource := platformv1alpha1.InferenceDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: d.Name, Namespace: d.Namespace},
		Spec:       d.Spec,
	}
	if err := resource.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return nil
}

func ServingDigest(spec platformv1alpha1.InferenceDeploymentSpec) string {
	serving := struct {
		Model        platformv1alpha1.ModelSpec
		Runtime      platformv1alpha1.RuntimeSpec
		Accelerator  platformv1alpha1.AcceleratorSpec
		SelectionSLO *platformv1alpha1.SLOSpec `json:",omitempty"`
	}{Model: spec.Model, Runtime: spec.Runtime, Accelerator: spec.Accelerator}
	// SLO targets are operational policy for an explicitly selected runtime,
	// but they are part of the measured selection input for backend:auto. A
	// changed target must therefore create a candidate and rerun selection even
	// when the model/runtime/hardware fields are byte-identical.
	if spec.Runtime.Backend == platformv1alpha1.RuntimeBackendAuto {
		serving.SelectionSLO = spec.SLO
	}
	payload, _ := json.Marshal(serving)
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func SpecDigest(spec platformv1alpha1.InferenceDeploymentSpec) string {
	payload, _ := json.Marshal(spec)
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func NewOperation(tenantID, deploymentID string, kind OperationKind, requestID, idempotencyKey string, now time.Time) *Operation {
	return &Operation{
		ID: newID("operation"), TenantID: tenantID, DeploymentID: deploymentID,
		Kind: kind, Status: OperationAccepted, RequestID: requestID,
		IdempotencyKey: idempotencyKey, CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
	}
}

func newID(prefix string) string {
	id, err := uuid.NewV7()
	if err != nil {
		panic(fmt.Sprintf("generate %s UUIDv7: %v", prefix, err))
	}
	return id.String()
}
