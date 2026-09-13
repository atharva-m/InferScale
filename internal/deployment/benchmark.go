package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
)

type BenchmarkState string

const (
	BenchmarkQueued    BenchmarkState = "queued"
	BenchmarkRunning   BenchmarkState = "running"
	BenchmarkSucceeded BenchmarkState = "succeeded"
	BenchmarkFailed    BenchmarkState = "failed"
)

type BenchmarkRun struct {
	ID             string          `json:"id"`
	TenantID       string          `json:"tenantId"`
	DeploymentID   string          `json:"deploymentId"`
	RevisionID     string          `json:"revisionId"`
	Scenario       string          `json:"scenario"`
	ScenarioDigest string          `json:"scenarioDigest"`
	Configuration  json.RawMessage `json:"configuration"`
	ArtifactURI    string          `json:"artifactUri,omitempty"`
	Provenance     json.RawMessage `json:"provenance,omitempty"`
	State          BenchmarkState  `json:"state"`
	RentalPriceUSD float64         `json:"rentalPriceUsdPerGPUHour"`
	CreatedAt      time.Time       `json:"createdAt"`
	StartedAt      *time.Time      `json:"startedAt,omitempty"`
	CompletedAt    *time.Time      `json:"completedAt,omitempty"`
	Error          string          `json:"error,omitempty"`
	IdempotencyKey string          `json:"-"`
}

type BenchmarkRepository interface {
	CreateBenchmark(context.Context, *BenchmarkRun) error
	FindBenchmarkByIdempotency(context.Context, string, string) (*BenchmarkRun, error)
	ListBenchmarks(context.Context, string, string, ListOptions) (*BenchmarkPage, error)
}

type BenchmarkPage struct {
	Items      []BenchmarkRun
	NextCursor *ListCursor
}

type BenchmarkService struct {
	deployments Repository
	benchmarks  BenchmarkRepository
	now         func() time.Time
}

func NewBenchmarkService(deployments Repository, benchmarks BenchmarkRepository) *BenchmarkService {
	return &BenchmarkService{deployments: deployments, benchmarks: benchmarks, now: time.Now}
}

func (s *BenchmarkService) Create(ctx context.Context, tenantID, deploymentID, scenario string, configuration json.RawMessage, rentalPrice float64, idempotencyKey string) (*BenchmarkRun, error) {
	if idempotencyKey == "" || len(idempotencyKey) > 200 {
		return nil, fmt.Errorf("%w: benchmark idempotency key is required", ErrInvalid)
	}
	if idempotencyKey != "" {
		if run, err := s.benchmarks.FindBenchmarkByIdempotency(ctx, tenantID, idempotencyKey); err == nil {
			if !benchmarkReplayMatches(run, deploymentID, scenario, configuration, rentalPrice) {
				return nil, ErrIdempotencyConflict
			}
			return run, nil
		} else if !errors.Is(err, ErrBenchmarkNotFound) {
			return nil, err
		}
	}
	d, err := s.deployments.Get(ctx, tenantID, deploymentID)
	if err != nil {
		return nil, err
	}
	if scenario == "" || !json.Valid(configuration) || rentalPrice <= 0 {
		return nil, fmt.Errorf("%w: benchmark scenario, JSON configuration, and positive rental price are required", ErrInvalid)
	}
	revisionID, err := benchmarkStableRevision(d)
	if err != nil {
		return nil, err
	}
	run := &BenchmarkRun{
		ID:             newID("bench"),
		TenantID:       tenantID,
		DeploymentID:   deploymentID,
		RevisionID:     revisionID,
		Scenario:       scenario,
		ScenarioDigest: benchmarkDigest(configuration),
		Configuration:  append(json.RawMessage(nil), configuration...),
		State:          BenchmarkQueued,
		RentalPriceUSD: rentalPrice,
		CreatedAt:      s.now().UTC(),
		IdempotencyKey: idempotencyKey,
	}
	if err := s.benchmarks.CreateBenchmark(ctx, run); err != nil {
		if idempotencyKey != "" {
			if replay, replayErr := s.benchmarks.FindBenchmarkByIdempotency(ctx, tenantID, idempotencyKey); replayErr == nil {
				if !benchmarkReplayMatches(replay, deploymentID, scenario, configuration, rentalPrice) {
					return nil, ErrIdempotencyConflict
				}
				return replay, nil
			}
		}
		return nil, err
	}
	return run, nil
}

func benchmarkReplayMatches(run *BenchmarkRun, deploymentID, scenario string, configuration json.RawMessage, rentalPrice float64) bool {
	return run != nil && run.DeploymentID == deploymentID && run.Scenario == scenario &&
		canonicalJSONEqual(run.Configuration, configuration) && run.RentalPriceUSD == rentalPrice
}

func canonicalJSONEqual(left, right json.RawMessage) bool {
	leftValue, leftErr := decodeCanonicalJSON(left)
	rightValue, rightErr := decodeCanonicalJSON(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	leftCanonical, leftErr := json.Marshal(leftValue)
	rightCanonical, rightErr := json.Marshal(rightValue)
	return leftErr == nil && rightErr == nil && string(leftCanonical) == string(rightCanonical)
}

func decodeCanonicalJSON(raw json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("JSON value must contain exactly one value")
	}
	return value, nil
}

func benchmarkDigest(configuration json.RawMessage) string {
	sum := sha256.Sum256(configuration)
	return hex.EncodeToString(sum[:])
}

func benchmarkStableRevision(d *Deployment) (string, error) {
	if d == nil || d.DeletedAt != nil || d.State != StateReady ||
		d.StableRevisionID == "" || d.CandidateRevisionID != "" ||
		d.ObservedStatus.Phase != platformv1alpha1.DeploymentPhaseReady ||
		d.ObservedStatus.ObservedGeneration != d.Generation ||
		d.ObservedStatus.Revision.StableID != d.StableRevisionID ||
		d.ObservedStatus.Revision.Candidate != "" || d.ObservedStatus.Revision.CandidateID != "" {
		return "", fmt.Errorf("%w: benchmarks require a fully reconciled Ready deployment with one stable revision and no candidate", ErrBenchmarkTargetNotReady)
	}
	return d.StableRevisionID, nil
}

func (s *BenchmarkService) List(ctx context.Context, tenantID, deploymentID string, options ListOptions) (*BenchmarkPage, error) {
	if _, err := s.deployments.Get(ctx, tenantID, deploymentID); err != nil {
		return nil, err
	}
	if options.Limit <= 0 || options.Limit > 200 {
		options.Limit = 50
	}
	return s.benchmarks.ListBenchmarks(ctx, tenantID, deploymentID, options)
}

type MetricsReader interface {
	ReadDeploymentMetrics(context.Context, string, string, time.Time, time.Time) (any, error)
}
