package benchmark

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

type PendingRunRepository interface {
	ClaimPendingRun(ctx context.Context, workerID string, lease time.Duration) (Run, error)
	PrepareClaimedRun(ctx context.Context, run Run, workerID string) (Run, error)
	RenewRunLease(ctx context.Context, runID, workerID string, lease time.Duration) error
	MarkRunFailed(ctx context.Context, runID, reason string) error
}

type JobSubmitter interface {
	Prepare(run Run) (Run, error)
	Submit(ctx context.Context, run Run) error
}

type Scheduler struct {
	repository PendingRunRepository
	submitter  JobSubmitter
	workerID   string
	poll       time.Duration
	logger     *slog.Logger
}

func NewScheduler(repository PendingRunRepository, submitter JobSubmitter, workerID string, poll time.Duration, logger *slog.Logger) *Scheduler {
	if poll <= 0 {
		poll = 5 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{repository: repository, submitter: submitter, workerID: workerID, poll: poll, logger: logger}
}

func (s *Scheduler) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.poll)
	defer ticker.Stop()
	for {
		if err := s.runOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.ErrorContext(ctx, "benchmark scheduling iteration failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Scheduler) runOnce(ctx context.Context) error {
	run, err := s.repository.ClaimPendingRun(ctx, s.workerID, 2*time.Minute)
	if err != nil {
		return err
	}
	if run.ID == "" {
		return nil
	}
	prepared, err := s.submitter.Prepare(run)
	if err != nil {
		return s.failRun(ctx, run.ID, fmt.Errorf("prepare benchmark %s: %w", run.ID, err))
	}
	prepared, err = s.repository.PrepareClaimedRun(ctx, prepared, s.workerID)
	if err != nil {
		return s.failRun(ctx, run.ID, fmt.Errorf("persist benchmark execution %s: %w", run.ID, err))
	}
	if err := s.submitter.Submit(ctx, prepared); err != nil {
		return s.failRun(ctx, run.ID, fmt.Errorf("submit benchmark %s: %w", run.ID, err))
	}
	// A short claim lease permits quick recovery if the scheduler dies before
	// Job creation. Once an idempotent Job exists, cover its full six-hour
	// deadline (plus cleanup/callback margin) to avoid repeated active-Job
	// adoption every two minutes.
	if err := s.repository.RenewRunLease(ctx, run.ID, s.workerID, benchmarkJobDeadline+15*time.Minute); err != nil {
		return fmt.Errorf("renew benchmark %s lease: %w", run.ID, err)
	}
	return nil
}

func (s *Scheduler) failRun(ctx context.Context, runID string, cause error) error {
	markErr := s.repository.MarkRunFailed(ctx, runID, cause.Error())
	return errors.Join(cause, markErr)
}
