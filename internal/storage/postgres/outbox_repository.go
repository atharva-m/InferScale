package postgres

import (
	"context"
	"time"

	platformsync "github.com/inferscale/inferscale/internal/sync"
)

type OutboxRepository struct{ store *Store }

func NewOutboxRepository(store *Store) *OutboxRepository { return &OutboxRepository{store: store} }

func (r *OutboxRepository) PendingCount(ctx context.Context) (int64, error) {
	var count int64
	err := r.store.pool.QueryRow(ctx, `SELECT count(*) FROM sync_outbox WHERE processed_at IS NULL`).Scan(&count)
	return count, err
}

func (r *OutboxRepository) Claim(ctx context.Context, limit int, now time.Time, lease time.Duration) ([]platformsync.Event, error) {
	rows, err := r.store.pool.Query(ctx, `
		WITH selected AS (
			SELECT id FROM sync_outbox
			WHERE processed_at IS NULL AND next_attempt_at <= $1
			  AND (locked_until IS NULL OR locked_until < $1)
			ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $2
		)
		UPDATE sync_outbox o SET locked_until=$3
		FROM selected WHERE o.id=selected.id
		RETURNING o.id, o.aggregate_id, o.event_type, o.payload, o.attempts,
		          o.created_at, o.next_attempt_at`, now, limit, now.Add(lease))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]platformsync.Event, 0)
	for rows.Next() {
		var event platformsync.Event
		var eventType string
		if err := rows.Scan(&event.ID, &event.AggregateID, &eventType, &event.Payload,
			&event.Attempts, &event.CreatedAt, &event.NextAttemptAt); err != nil {
			return nil, err
		}
		event.Type = platformsync.EventType(eventType)
		events = append(events, event)
	}
	return events, rows.Err()
}

func (r *OutboxRepository) Complete(ctx context.Context, id int64, at time.Time) error {
	_, err := r.store.pool.Exec(ctx, `
		WITH completed AS (
			UPDATE sync_outbox SET processed_at=$2, locked_until=NULL, last_error=''
			WHERE id=$1 RETURNING operation_id
		)
		UPDATE operations SET status='succeeded', updated_at=$2, completed_at=$2, error=''
		WHERE id=(SELECT operation_id FROM completed)`, id, at)
	return err
}

func (r *OutboxRepository) Retry(ctx context.Context, id int64, message string, next time.Time) error {
	_, err := r.store.pool.Exec(ctx, `
		WITH retried AS (
			UPDATE sync_outbox SET attempts=attempts+1, next_attempt_at=$2,
				locked_until=NULL, last_error=$3 WHERE id=$1 AND processed_at IS NULL
			RETURNING operation_id
		)
		UPDATE operations SET status='retrying', updated_at=now(),
			error='Deployment synchronization failed; retry scheduled.'
		WHERE id=(SELECT operation_id FROM retried) AND status<>'succeeded'`, id, next, message)
	return err
}
