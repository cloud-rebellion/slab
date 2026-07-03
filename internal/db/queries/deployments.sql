-- name: ListDeploymentsBySite :many
SELECT * FROM deployments WHERE site_id = ? ORDER BY created_at DESC;

-- name: ListRecentDeploymentsBySite :many
SELECT * FROM deployments WHERE site_id = ? ORDER BY created_at DESC LIMIT ?;

-- name: GetDeploymentByID :one
SELECT * FROM deployments WHERE id = ?;

-- name: CreateDeployment :exec
INSERT INTO deployments (id, site_id, status, deploy_target, deploy_config, created_at)
VALUES (?, ?, 'pending', ?, ?, datetime('now'));

-- name: UpdateDeploymentStatus :exec
UPDATE deployments
SET status = ?, build_log = ?, pages_built = ?, duration_ms = ?, error = ?, completed_at = datetime('now')
WHERE id = ?;

-- name: UpdateDeploymentDeployed :exec
UPDATE deployments
SET target_id = ?, deploy_url = ?, deployed_at = datetime('now')
WHERE id = ?;

-- name: DeleteDeployment :exec
DELETE FROM deployments WHERE id = ?;

-- name: ReapStaleDeployments :execrows
-- Boot-time reaper: builds run in-process, so a crash or redeploy
-- mid-build strands rows in 'pending'/'building' forever (the UI then
-- shows a permanently-running build). Mirrors ReapStaleVerifyJobs.
UPDATE deployments
SET status = 'failed',
    error = 'interrupted: server restarted during build',
    completed_at = datetime('now')
WHERE status IN ('pending', 'building');
