package usage

import (
	"context"
	"errors"
	"testing"
	"time"
)

type sourceStub struct {
	values []Hourly
	err    error
}

func (s sourceStub) CompletedHour(context.Context, time.Time, time.Time) ([]Hourly, error) {
	return s.values, s.err
}

type repositoryStub struct{ values []Hourly }

func (r *repositoryStub) UpsertHourly(_ context.Context, value Hourly) error {
	r.values = append(r.values, value)
	return nil
}

func TestAggregatePreviousHoursNormalizesHourAndIsReplayable(t *testing.T) {
	repository := &repositoryStub{}
	aggregator := NewAggregator(sourceStub{values: []Hourly{{
		TenantID: "tenant-1", DeploymentID: "deployment-1", Revision: "rev-1",
		Hour: time.Date(2026, 8, 17, 12, 37, 0, 0, time.FixedZone("offset", 3600)), Requests: 4,
	}}}, repository)
	aggregator.now = func() time.Time { return time.Date(2026, 8, 17, 15, 30, 0, 0, time.UTC) }

	if err := aggregator.AggregatePreviousHours(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	if err := aggregator.AggregatePreviousHours(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	if len(repository.values) != 2 {
		t.Fatalf("got %d upserts, want two idempotent replays", len(repository.values))
	}
	want := time.Date(2026, 8, 17, 11, 0, 0, 0, time.UTC)
	if !repository.values[0].Hour.Equal(want) {
		t.Fatalf("hour = %s, want %s", repository.values[0].Hour, want)
	}
}

func TestAggregatePreviousHoursDoesNotWriteOnSourceFailure(t *testing.T) {
	repository := &repositoryStub{}
	aggregator := NewAggregator(sourceStub{err: errors.New("metrics unavailable")}, repository)
	if err := aggregator.AggregatePreviousHours(context.Background(), 1); err == nil {
		t.Fatal("expected source failure")
	}
	if len(repository.values) != 0 {
		t.Fatalf("unexpected writes: %#v", repository.values)
	}
}
