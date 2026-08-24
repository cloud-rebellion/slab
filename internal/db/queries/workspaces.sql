-- name: CreateWorkspace :exec
INSERT INTO workspaces (id, name, slug, plan, region, billing_email, status, trial_ends_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetWorkspaceByID :one
SELECT id, name, slug, plan, region, billing_email,
       stripe_customer_id, stripe_subscription_id,
       status, trial_ends_at, created_at, updated_at
FROM workspaces WHERE id = ?;

-- name: GetWorkspaceBySlug :one
SELECT id, name, slug, plan, region, billing_email,
       stripe_customer_id, stripe_subscription_id,
       status, trial_ends_at, created_at, updated_at
FROM workspaces WHERE slug = ?;

-- name: ListWorkspacesForUser :many
SELECT w.id, w.name, w.slug, w.plan, w.region, w.billing_email,
       w.stripe_customer_id, w.stripe_subscription_id,
       w.status, w.trial_ends_at, w.created_at, w.updated_at,
       wm.role
FROM workspaces w
JOIN workspace_members wm ON wm.workspace_id = w.id
WHERE wm.user_id = ? AND w.status != 'deleted'
ORDER BY w.created_at ASC;

-- name: ListAllWorkspaces :many
SELECT id, name, slug, plan, region, billing_email,
       stripe_customer_id, stripe_subscription_id,
       status, trial_ends_at, created_at, updated_at
FROM workspaces
ORDER BY created_at ASC;

-- name: UpdateWorkspace :exec
UPDATE workspaces
SET name = ?, billing_email = ?, updated_at = datetime('now')
WHERE id = ?;

-- name: UpdateWorkspacePlan :exec
UPDATE workspaces
SET plan = ?, status = ?, updated_at = datetime('now')
WHERE id = ?;

-- name: UpdateWorkspaceStripe :exec
UPDATE workspaces
SET stripe_customer_id = ?, stripe_subscription_id = ?, updated_at = datetime('now')
WHERE id = ?;

-- name: DeleteWorkspace :exec
DELETE FROM workspaces WHERE id = ?;

-- name: CountWorkspaces :one
SELECT COUNT(*) FROM workspaces;
