// Package usage materializes bounded hourly aggregates from Prometheus. Raw
// samples remain in Prometheus and are never copied into PostgreSQL.
package usage

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

type Hourly struct {
	TenantID         string
	DeploymentID     string
	Revision         string
	Hour             time.Time
	Requests         int64
	InputTokens      int64
	OutputTokens     int64
	RejectedRequests int64
	GPUSeconds       float64
	ShadowGPUSeconds float64
}

type Source interface {
	CompletedHour(ctx context.Context, start, end time.Time) ([]Hourly, error)
}

type Repository interface {
	UpsertHourly(ctx context.Context, aggregate Hourly) error
}

type Aggregator struct {
	source     Source
	repository Repository
	now        func() time.Time
}

func NewAggregator(source Source, repository Repository) *Aggregator {
	return &Aggregator{source: source, repository: repository, now: time.Now}
}

func (a *Aggregator) AggregatePreviousHours(ctx context.Context, lookback int) error {
	if lookback < 1 {
		lookback = 1
	}
	end := a.now().UTC().Truncate(time.Hour)
	start := end.Add(-time.Duration(lookback) * time.Hour)
	aggregates, err := a.source.CompletedHour(ctx, start, end)
	if err != nil {
		return fmt.Errorf("query hourly usage: %w", err)
	}
	for _, aggregate := range aggregates {
		aggregate.Hour = aggregate.Hour.UTC().Truncate(time.Hour)
		if err := a.repository.UpsertHourly(ctx, aggregate); err != nil {
			return fmt.Errorf("upsert usage for %s/%s: %w", aggregate.TenantID, aggregate.DeploymentID, err)
		}
	}
	return nil
}

type Observer interface {
	ObserveUsageAggregation(error, time.Duration, time.Time)
}

// Run recomputes the most recent completed hours immediately and then on each
// interval. Prometheus outages are logged and retried: they do not terminate
// the management API and they never write partial zero-valued replacements.
func (a *Aggregator) Run(ctx context.Context, interval time.Duration, lookback int, logger *slog.Logger, observer Observer) error {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	if logger == nil {
		logger = slog.Default()
	}
	run := func() {
		started := a.now()
		err := a.AggregatePreviousHours(ctx, lookback)
		if observer != nil {
			observer.ObserveUsageAggregation(err, a.now().Sub(started), a.now().UTC())
		}
		if err != nil && ctx.Err() == nil {
			logger.Error("hourly usage aggregation failed", "error", err)
		}
	}
	run()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			run()
		}
	}
}
