-- Conversion goals + events (Phase 31.1, 2026-05-06).
-- The CRUD surface for goals plus the ingest + read queries for events.
-- Aggregations stay simple here; richer attribution windows live behind
-- the analytics handlers which compose these into per-range views.

-- name: ListActiveGoalsBySite :many
SELECT * FROM conversion_goals
WHERE site_id = ? AND active = 1
ORDER BY created_at DESC;

-- name: ListAllGoalsBySite :many
SELECT * FROM conversion_goals
WHERE site_id = ?
ORDER BY created_at DESC;

-- name: GetGoalByID :one
SELECT * FROM conversion_goals
WHERE id = ?;

-- name: GetGoalBySiteSlug :one
SELECT * FROM conversion_goals
WHERE site_id = ? AND slug = ?;

-- name: CreateGoal :exec
INSERT INTO conversion_goals (
    id, site_id, slug, name, match_type, match_value,
    value_cents, value_currency, active
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: UpdateGoal :exec
UPDATE conversion_goals
SET name = ?,
    match_type = ?,
    match_value = ?,
    value_cents = ?,
    value_currency = ?,
    active = ?,
    updated_at = datetime('now')
WHERE id = ?;

-- name: DeleteGoal :exec
DELETE FROM conversion_goals WHERE id = ?;

-- name: RecordConversionEvent :exec
INSERT INTO conversion_events (
    id, site_id, goal_id, fingerprint, session_id, path, ts,
    value_cents, value_currency, properties_json
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: CountConversionsByGoal :one
SELECT COUNT(*) AS count FROM conversion_events
WHERE goal_id = ? AND ts >= ?;

-- name: CountUniqueConvertersByGoal :one
SELECT COUNT(DISTINCT fingerprint) AS count FROM conversion_events
WHERE goal_id = ? AND ts >= ?;

-- name: SumValueByGoal :one
SELECT COALESCE(SUM(value_cents), 0) AS total_cents FROM conversion_events
WHERE goal_id = ? AND ts >= ?;

-- name: ListConversionsByGoal :many
SELECT * FROM conversion_events
WHERE goal_id = ? AND ts >= ?
ORDER BY ts DESC
LIMIT ?;

-- name: ListConversionsBySite :many
SELECT * FROM conversion_events
WHERE site_id = ? AND ts >= ?
ORDER BY ts DESC
LIMIT ?;

-- name: ListConversionsBySiteFingerprint :many
SELECT * FROM conversion_events
WHERE site_id = ? AND fingerprint = ?
ORDER BY ts DESC
LIMIT ?;

-- name: DeleteConversionEventsBySiteOlderThan :execrows
-- Per-site retention sweep. conversion_events was created without one, so
-- every row survived the analytics window indefinitely while the sessions and
-- events it references were deleted on schedule. Each row carries the visitor
-- `fingerprint` plus `path`, so a "deleted" visitor stayed re-identifiable
-- here and a DSAR erasure could not be satisfied by any code path.
DELETE FROM conversion_events
WHERE site_id = ? AND ts < ?;
