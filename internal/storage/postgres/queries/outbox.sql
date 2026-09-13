-- name: ClaimSyncEvents :many
WITH selected AS (
  SELECT id FROM sync_outbox
  WHERE processed_at IS NULL AND next_attempt_at <= $1
    AND (locked_until IS NULL OR locked_until < $1)
  ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $2
)
UPDATE sync_outbox o SET locked_until = $3
FROM selected WHERE o.id = selected.id
RETURNING o.id, o.aggregate_id, o.event_type, o.payload, o.attempts,
          o.created_at, o.next_attempt_at;
