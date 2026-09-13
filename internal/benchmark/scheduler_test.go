package benchmark

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

type pendingRepository struct {
	run          Run
	claimError   error
	failedID     string
	failureCause string
	prepared     bool
	renewedFor   time.Duration
}

func (r *pendingRepository) ClaimPendingRun(context.Context, string, time.Duration) (Run, error) {
	return r.run, r.claimError
}
func (r *pendingRepository) MarkRunFailed(_ context.Context, id, reason string) error {
	r.failedID, r.failureCause = id, reason
	return nil
}
func (r *pendingRepository) PrepareClaimedRun(_ context.Context, run Run, _ string) (Run, error) {
	r.prepared = true
	return run, nil
}
func (r *pendingRepository) RenewRunLease(_ context.Context, _, _ string, lease time.Duration) error {
	r.renewedFor = lease
	return nil
}

type submitterFunc func(context.Context, Run) error

func (f submitterFunc) Submit(ctx context.Context, run Run) error { return f(ctx, run) }
func (f submitterFunc) Prepare(run Run) (Run, error)              { return run, nil }

func TestSchedulerMarksSubmissionFailure(t *testing.T) {
	repository := &pendingRepository{run: Run{ID: "run"}}
	scheduler := NewScheduler(repository, submitterFunc(func(context.Context, Run) error {
		return errors.New("Kubernetes unavailable")
	}), "worker", time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := scheduler.runOnce(context.Background()); err == nil {
		t.Fatal("expected scheduling error")
	}
	if repository.failedID != "run" || repository.failureCause != "submit benchmark run: Kubernetes unavailable" {
		t.Fatalf("failure = %q %q", repository.failedID, repository.failureCause)
	}
}

func TestSchedulerPersistsSnapshotAndExtendsLeaseAfterSubmission(t *testing.T) {
	repository := &pendingRepository{run: Run{ID: "run"}}
	scheduler := NewScheduler(repository, submitterFunc(func(context.Context, Run) error {
		return nil
	}), "worker", time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := scheduler.runOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !repository.prepared {
		t.Fatal("execution snapshot was not persisted before submission")
	}
	if repository.renewedFor < benchmarkJobDeadline {
		t.Fatalf("renewed lease = %v, must cover job deadline %v", repository.renewedFor, benchmarkJobDeadline)
	}
}
