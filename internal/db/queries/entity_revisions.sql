-- name: CreateEntityRevision :one
-- version_number is computed inside the INSERT so read and write are one
-- atomic statement under SQLite's writer lock: two concurrent Record
-- calls can no longer race SELECT MAX+1 into duplicate versions (which
-- broke GetEntityRevisionByVersion :one and therefore restore). The
-- unique index idx_entity_revisions_version is the schema backstop.
INSERT INTO entity_revisions (id, site_id, entity_type, entity_id, version_number, snapshot_json, change_summary, created_by, created_at)
VALUES (
    ?1, ?2, ?3, ?4,
    (SELECT COALESCE(MAX(version_number), 0) + 1
       FROM entity_revisions
      WHERE site_id = ?2 AND entity_type = ?3 AND entity_id = ?4),
    ?5, ?6, ?7, datetime('now')
)
RETURNING version_number;

-- name: GetEntityRevisionByVersion :one
SELECT * FROM entity_revisions
WHERE site_id = ? AND entity_type = ? AND entity_id = ? AND version_number = ?;

-- name: ListEntityRevisions :many
SELECT * FROM entity_revisions
WHERE site_id = ? AND entity_type = ? AND entity_id = ?
ORDER BY version_number DESC
LIMIT ?;

-- name: CountEntityRevisions :one
SELECT CAST(COUNT(*) AS INTEGER) FROM entity_revisions
WHERE site_id = ? AND entity_type = ? AND entity_id = ?;

-- name: PruneEntityRevisionsOverLimit :exec
DELETE FROM entity_revisions AS outer_r
WHERE outer_r.site_id = ?1
  AND outer_r.entity_type = ?2
  AND outer_r.entity_id = ?3
  AND outer_r.id NOT IN (
    SELECT inner_r.id FROM entity_revisions AS inner_r
    WHERE inner_r.site_id = ?1
      AND inner_r.entity_type = ?2
      AND inner_r.entity_id = ?3
    ORDER BY inner_r.version_number DESC
    LIMIT ?4
);
