-- name: CreateWorkspaceInvite :exec
INSERT INTO workspace_invites (id, workspace_id, email, role, token, created_by, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetWorkspaceInviteByToken :one
SELECT id, workspace_id, email, role, token, created_by, created_at, expires_at, used_at
FROM workspace_invites WHERE token = ?;

-- name: ListPendingWorkspaceInvites :many
SELECT id, workspace_id, email, role, token, created_by, created_at, expires_at, used_at
FROM workspace_invites
WHERE workspace_id = ? AND used_at = '' AND expires_at > datetime('now')
ORDER BY created_at DESC;

-- name: MarkWorkspaceInviteUsed :exec
UPDATE workspace_invites SET used_at = datetime('now') WHERE id = ?;

-- name: DeleteWorkspaceInvite :exec
DELETE FROM workspace_invites WHERE id = ? AND workspace_id = ?;

-- name: ListSitesByWorkspace :many
SELECT id, workspace_id, name, slug, status, created_at, updated_at
FROM sites WHERE workspace_id = ?
ORDER BY created_at ASC;

-- name: BackfillSitesWorkspace :exec
-- Bootstrap helper: assign the given workspace_id to every site that
-- currently has an empty workspace_id. Idempotent. Runs once at boot
-- after ensureDefaultWorkspace creates the default workspace.
UPDATE sites SET workspace_id = ? WHERE workspace_id = '';
