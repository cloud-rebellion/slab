-- name: AddWorkspaceMember :exec
INSERT INTO workspace_members (workspace_id, user_id, role)
VALUES (?, ?, ?)
ON CONFLICT(workspace_id, user_id) DO UPDATE SET role = excluded.role;

-- name: RemoveWorkspaceMember :exec
DELETE FROM workspace_members WHERE workspace_id = ? AND user_id = ?;

-- name: GetWorkspaceMembership :one
SELECT workspace_id, user_id, role, created_at
FROM workspace_members
WHERE workspace_id = ? AND user_id = ?;

-- name: ListWorkspaceMembers :many
SELECT wm.workspace_id, wm.user_id, wm.role, wm.created_at,
       u.email, u.name
FROM workspace_members wm
JOIN users u ON u.id = wm.user_id
WHERE wm.workspace_id = ?
ORDER BY wm.created_at ASC;

-- name: ListWorkspaceIDsForUser :many
-- Ordered so "the user's default workspace" (the first row, used by
-- resolveWorkspaceForCreate when no workspace_id is supplied) is stable
-- across calls. Unordered, a multi-workspace agency user got an
-- arbitrary client workspace for a new site.
SELECT wm.workspace_id
FROM workspace_members wm
JOIN workspaces w ON w.id = wm.workspace_id
WHERE wm.user_id = ? AND w.status != 'deleted'
ORDER BY w.created_at ASC, wm.workspace_id ASC;

-- name: BackfillWorkspaceMembersForAdmin :exec
-- Bootstrap helper: grant the admin owner-role on every existing
-- workspace that has zero member rows. Idempotent: once any member
-- exists, the workspace is skipped. Used by ensureDefaultWorkspace
-- on first boot of a Phase 30+ binary against an existing DB.
INSERT INTO workspace_members (workspace_id, user_id, role)
SELECT w.id, ?, 'owner' FROM workspaces w
WHERE NOT EXISTS (SELECT 1 FROM workspace_members WHERE workspace_id = w.id);

-- name: RemoveWorkspaceMemberGuarded :execrows
-- Removes a member, but refuses to remove the LAST owner. The guard lives in
-- the statement rather than in Go on purpose: a read-then-write pair
-- (count owners, then delete) is check-then-act, and two concurrent removals
-- both read the same stale count and both proceed, leaving a workspace with
-- zero owners and nobody able to manage it. Measured at 299/300 races under
-- WAL with busy_timeout, and 300/300 under the production connection config
-- (WAL only makes the second writer WAIT for the lock; it does not stop two
-- readers acting on the same stale count).
--
-- Returns rows-affected: 0 means the delete was refused (or the row was
-- already gone), so callers must distinguish those with a prior existence
-- read and report a 409.
DELETE FROM workspace_members
WHERE workspace_members.workspace_id = ? AND workspace_members.user_id = ?
  AND (
    workspace_members.role != 'owner'
    OR (SELECT COUNT(*) FROM workspace_members m2
        WHERE m2.workspace_id = ? AND m2.role = 'owner') > 1
  );

-- name: DemoteWorkspaceMemberGuarded :execrows
-- Same atomic guard for a role change that takes someone OUT of owner.
-- Promotions and owner->owner no-ops do not go through here; they use the
-- plain upsert, because neither can reduce the owner count.
UPDATE workspace_members
SET role = ?
WHERE workspace_members.workspace_id = ? AND workspace_members.user_id = ?
  AND (
    workspace_members.role != 'owner'
    OR (SELECT COUNT(*) FROM workspace_members m2
        WHERE m2.workspace_id = ? AND m2.role = 'owner') > 1
  );

-- name: UpdateWorkspaceMemberRoleIfMember :execrows
-- Role change for someone who is ALREADY a member. A plain UPDATE, never an
-- upsert: AddWorkspaceMember's INSERT..ON CONFLICT would RESURRECT a row that
-- a concurrent RemoveMember had just deleted, because the caller's existence
-- read happened before that delete. UPDATE touches zero rows when the row is
-- gone, which is the correct outcome. Callers report 0 rows as 404.
UPDATE workspace_members
SET role = ?
WHERE workspace_members.workspace_id = ? AND workspace_members.user_id = ?;
