// Package sync delivers durable desired-state changes from PostgreSQL to the
// Kubernetes control plane. It deliberately contains no Kubernetes client so
// the API and controller can be deployed independently.
package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	"github.com/inferscale/inferscale/internal/deployment"
	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type EventType string

const (
	EventDeploymentUpsert EventType = "deployment.upsert"
	EventDeploymentDelete EventType = "deployment.delete"
)

type Event struct {
	ID            int64
	AggregateID   string
	Type          EventType
	Payload       json.RawMessage
	Attempts      int
	CreatedAt     time.Time
	NextAttemptAt time.Time
}

type DeleteTarget struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type EventRepository interface {
	Claim(context.Context, int, time.Time, time.Duration) ([]Event, error)
	Complete(context.Context, int64, time.Time) error
	Retry(context.Context, int64, string, time.Time) error
}

type DeploymentReader interface {
	GetByID(context.Context, string) (*deployment.Deployment, error)
}

// DriftReader pages through the PostgreSQL desired state. The outbox is the
// low-latency delivery path; this reader is the periodic repair path for CRs
// that were edited or partially overwritten after their outbox event was
// acknowledged.
type DriftReader interface {
	ListForDriftAudit(context.Context, string, int) ([]deployment.Deployment, string, error)
}

type Applier interface {
	Apply(context.Context, *platformv1alpha1.InferenceDeployment) error
	Delete(context.Context, string, string) error
}

type Worker struct {
	events      EventRepository
	deployments DeploymentReader
	applier     Applier
	logger      *slog.Logger
	now         func() time.Time
	batchSize   int
	lease       time.Duration
	drift       DriftReader
	driftCursor string
	driftBatch  int
	driftEvery  time.Duration
}

func NewWorker(events EventRepository, deployments DeploymentReader, applier Applier, logger *slog.Logger) *Worker {
	if logger == nil {
		logger = slog.Default()
	}
	worker := &Worker{
		events: events, deployments: deployments, applier: applier, logger: logger,
		now: time.Now, batchSize: 20, lease: 30 * time.Second,
		driftBatch: 100, driftEvery: 5 * time.Minute,
	}
	worker.drift, _ = deployments.(DriftReader)
	return worker
}

func (w *Worker) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var driftTicker *time.Ticker
	var driftC <-chan time.Time
	if w.drift != nil && w.driftEvery > 0 {
		driftTicker = time.NewTicker(w.driftEvery)
		driftC = driftTicker.C
		defer driftTicker.Stop()
	}
	for {
		if _, err := w.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.logger.Error("deployment sync batch failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-driftC:
			if _, err := w.RunDriftAuditOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				w.logger.Error("deployment drift audit failed", "error", err)
			}
		}
	}
}

// RunDriftAuditOnce reapplies one bounded page of authoritative PostgreSQL
// state with server-side apply. Reapplying an unchanged object is idempotent,
// while any manual CR serving-spec mutation is restored on the next pass.
func (w *Worker) RunDriftAuditOnce(ctx context.Context) (int, error) {
	if w.drift == nil {
		return 0, nil
	}
	values, next, err := w.drift.ListForDriftAudit(ctx, w.driftCursor, w.driftBatch)
	if err != nil {
		return 0, err
	}
	for index := range values {
		var applyErr error
		if values[index].DeletedAt != nil {
			applyErr = w.applier.Delete(ctx, values[index].Namespace, values[index].Name)
		} else {
			applyErr = w.applier.Apply(ctx, ToCustomResource(&values[index]))
		}
		if applyErr != nil {
			// Keep the current cursor so the failed object and the rest of the page
			// are retried rather than being silently skipped.
			return index, fmt.Errorf("repair deployment %s drift: %w", values[index].ID, applyErr)
		}
	}
	w.driftCursor = next
	return len(values), nil
}

func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	now := w.now().UTC()
	events, err := w.events.Claim(ctx, w.batchSize, now, w.lease)
	if err != nil {
		return 0, err
	}
	completed := 0
	for _, event := range events {
		if err := w.handle(ctx, event); err != nil {
			next := now.Add(retryDelay(event.Attempts + 1))
			if retryErr := w.events.Retry(ctx, event.ID, err.Error(), next); retryErr != nil {
				return completed, fmt.Errorf("retry sync event %d after %v: %w", event.ID, err, retryErr)
			}
			continue
		}
		if err := w.events.Complete(ctx, event.ID, now); err != nil {
			return completed, err
		}
		completed++
	}
	return completed, nil
}

func (w *Worker) handle(ctx context.Context, event Event) error {
	switch event.Type {
	case EventDeploymentUpsert:
		d, err := w.deployments.GetByID(ctx, event.AggregateID)
		if err != nil {
			return err
		}
		if d.DeletedAt != nil {
			return w.applier.Delete(ctx, d.Namespace, d.Name)
		}
		return w.applier.Apply(ctx, ToCustomResource(d))
	case EventDeploymentDelete:
		var target DeleteTarget
		if err := json.Unmarshal(event.Payload, &target); err != nil {
			return fmt.Errorf("decode delete target: %w", err)
		}
		if target.Namespace == "" || target.Name == "" {
			return errors.New("delete target is incomplete")
		}
		return w.applier.Delete(ctx, target.Namespace, target.Name)
	default:
		return fmt.Errorf("unsupported sync event type %q", event.Type)
	}
}

func ToCustomResource(d *deployment.Deployment) *platformv1alpha1.InferenceDeployment {
	revisionID := d.CandidateRevisionID
	if revisionID == "" {
		revisionID = d.ObservedStatus.Revision.LastFailedID
	}
	if revisionID == "" {
		revisionID = d.StableRevisionID
	}
	return &platformv1alpha1.InferenceDeployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: platformv1alpha1.GroupVersion.String(),
			Kind:       "InferenceDeployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      d.Name,
			Namespace: d.Namespace,
			Labels: map[string]string{
				"inferscale.io/deployment": d.Name,
				"inferscale.io/tenant":     d.TenantID,
			},
			Annotations: map[string]string{
				kubeutil.AnnotationDeploymentID: d.ID,
				"inferscale.io/spec-digest":     deployment.SpecDigest(d.Spec),
				kubeutil.AnnotationRevisionID:   revisionID,
			},
		},
		Spec: d.Spec,
	}
}

func retryDelay(attempt int) time.Duration {
	seconds := math.Pow(2, math.Min(float64(attempt), 8))
	return time.Duration(seconds) * time.Second
}
