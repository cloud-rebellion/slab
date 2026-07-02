-- name: ListRedirectsBySite :many
SELECT * FROM redirects WHERE site_id = ? ORDER BY from_path;

-- name: GetRedirectByPath :one
SELECT * FROM redirects WHERE site_id = ? AND from_path = ?;

-- name: GetRedirectByID :one
SELECT * FROM redirects WHERE id = ?;

-- name: CountRedirectsBySite :one
SELECT COUNT(*) FROM redirects WHERE site_id = ?;

-- name: CreateRedirect :exec
INSERT INTO redirects (id, site_id, from_path, to_path, status_code, is_auto, created_at)
VALUES (?, ?, ?, ?, ?, ?, datetime('now'));

-- name: UpsertRedirect :one
-- Sprint 4 (2026-05-06): re-import upsert. ON CONFLICT(site_id, from_path)
-- DO UPDATE leaves created_at alone and returns the existing row's id, so
-- the porter detects insert-vs-update by comparing returned ID.
INSERT INTO redirects (id, site_id, from_path, to_path, status_code, is_auto, created_at)
VALUES (?, ?, ?, ?, ?, ?, datetime('now'))
ON CONFLICT(site_id, from_path) DO UPDATE SET
    to_path     = excluded.to_path,
    status_code = excluded.status_code,
    is_auto     = excluded.is_auto
RETURNING *;

-- name: ListExistingRedirectFromPathsBySite :many
-- Sprint 4 (2026-05-06): mirror of ListExistingPageSlugsBySite.
SELECT from_path FROM redirects WHERE site_id = ?;

-- name: UpdateRedirect :exec
UPDATE redirects SET from_path = ?, to_path = ?, status_code = ? WHERE id = ?;

-- name: DeleteRedirect :exec
DELETE FROM redirects WHERE id = ?;

-- Forms
-- name: ListFormsBySite :many
SELECT * FROM forms WHERE site_id = ? ORDER BY name;

-- name: GetFormByID :one
SELECT * FROM forms WHERE id = ?;

-- name: CreateForm :exec
INSERT INTO forms (id, site_id, name, fields_json, action, action_config, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, datetime('now'), datetime('now'));

-- name: UpdateForm :exec
UPDATE forms
SET name = ?, fields_json = ?, action = ?, action_config = ?, updated_at = datetime('now')
WHERE id = ?;

-- name: DeleteForm :exec
DELETE FROM forms WHERE id = ?;

-- Form submissions
-- name: ListFormSubmissions :many
SELECT * FROM form_submissions WHERE form_id = ? ORDER BY created_at DESC;

-- name: ListFormSubmissionsBySite :many
SELECT * FROM form_submissions WHERE site_id = ? ORDER BY created_at DESC LIMIT ?;

-- name: CreateFormSubmission :exec
INSERT INTO form_submissions (id, form_id, site_id, data_json, ip_hash, created_at)
VALUES (?, ?, ?, ?, ?, datetime('now'));

-- name: DeleteFormSubmission :exec
DELETE FROM form_submissions WHERE id = ?;

-- Evaluations
-- name: ListEvaluationsByBuild :many
SELECT * FROM evaluations WHERE build_id = ? ORDER BY category;

-- name: ListEvaluationsByBuildAndSite :many
-- Site-scoped variant so the agent API can only read its own site's
-- evaluation results, never another tenant's by guessing a build_id.
SELECT * FROM evaluations WHERE build_id = ? AND site_id = ? ORDER BY category;

-- name: ListEvaluationsBySite :many
SELECT * FROM evaluations WHERE site_id = ? ORDER BY created_at DESC LIMIT ?;

-- name: CreateEvaluation :exec
INSERT INTO evaluations (id, build_id, site_id, category, score, max_score, grade, checks_json, profile, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'));

-- name: DeleteEvaluationsByBuild :exec
DELETE FROM evaluations WHERE build_id = ?;
