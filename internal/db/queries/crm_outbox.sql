-- name: EnqueueCRMOutbox :exec
-- Records the obligation to tell BrightCRM about an identified visitor. Called
-- in the same tx as the visit_sessions write, so the local claim and the
-- outbound obligation cannot diverge.
INSERT INTO crm_outbox (id, site_id, event_type, payload_json)
VALUES (?, ?, ?, ?);

-- name: ListDueCRMOutbox :many
-- Drain feed: pending or retrying rows whose backoff has elapsed.
SELECT id, site_id, event_type, payload_json, status, attempts, next_attempt_at, last_error, created_at, completed_at
FROM crm_outbox
WHERE status IN ('pending', 'retrying') AND next_attempt_at <= datetime('now')
ORDER BY created_at
LIMIT ?;

-- name: MarkCRMOutboxSucceeded :exec
UPDATE crm_outbox
SET status = 'succeeded', completed_at = datetime('now'), last_error = ''
WHERE id = ?;

-- name: MarkCRMOutboxRetrying :exec
-- attempts is incremented here rather than by the caller so a crash between
-- the send and the bookkeeping cannot lose the count and loop forever.
UPDATE crm_outbox
SET status = 'retrying', attempts = attempts + 1, next_attempt_at = ?, last_error = ?
WHERE id = ?;

-- name: MarkCRMOutboxDropped :exec
-- Terminal. Kept rather than deleted so an operator can see WHAT was lost;
-- the retention sweep purges it later.
UPDATE crm_outbox
SET status = 'dropped', attempts = attempts + 1, completed_at = datetime('now'), last_error = ?
WHERE id = ?;

-- name: CountPendingCRMOutbox :one
-- For an operator health surface: a growing number here means the CRM has been
-- unreachable and identifications are queued, which is exactly the state that
-- used to be invisible.
SELECT COUNT(*) FROM crm_outbox WHERE status IN ('pending', 'retrying');

-- name: DeleteCRMOutboxBySiteOlderThan :execrows
-- Retention sweep. payload_json carries the identified visitor's email, so this
-- table holds PII and must not outlive the window.
DELETE FROM crm_outbox
WHERE site_id = ? AND created_at < ? AND status IN ('succeeded', 'dropped');
