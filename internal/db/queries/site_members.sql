-- name: AddSiteMember :exec
INSERT INTO site_members (site_id, user_id, role) VALUES (?, ?, ?)
ON CONFLICT(site_id, user_id) DO UPDATE SET role = excluded.role;

-- name: RemoveSiteMember :exec
DELETE FROM site_members WHERE site_id = ? AND user_id = ?;

-- name: GetSiteMembership :one
SELECT site_id, user_id, role, created_at
FROM site_members WHERE site_id = ? AND user_id = ?;

-- name: ListSiteMembers :many
SELECT sm.site_id, sm.user_id, sm.role, sm.created_at,
       u.email, u.name
FROM site_members sm
JOIN users u ON u.id = sm.user_id
WHERE sm.site_id = ?
ORDER BY sm.created_at ASC;

-- name: ListSiteIDsForUser :many
SELECT site_id FROM site_members WHERE user_id = ?;

-- name: ListAccessibleSiteIDsForUser :many
-- Every site the user may open, which is NOT the same as their
-- site_members rows: SiteAccessMiddleware also clears a request when the
-- user is a member of the site's workspace (site_access.go branch 3),
-- and nothing writes a site_members row except site creation. Listing on
-- site_members alone therefore showed an invited workspace member an
-- empty dashboard while every one of those sites was reachable by URL.
--
-- Deliberately a SUBSET of what the gate clears: it omits the legacy
-- workspace_id='' admin branch and the audited cross-workspace bypass,
-- so the list can never be a superset of access. The workspace_id != ''
-- guard is what keeps legacy un-backfilled rows from being handed to
-- every workspace member at once.
--
-- Same user id is bound twice.
SELECT s.id AS site_id
FROM sites s
WHERE s.id IN (SELECT sm.site_id FROM site_members sm WHERE sm.user_id = ?)
   OR (s.workspace_id != ''
       AND s.workspace_id IN (
         SELECT wm.workspace_id FROM workspace_members wm
         JOIN workspaces w ON w.id = wm.workspace_id
         WHERE wm.user_id = ? AND w.status != 'deleted'
       ));

-- name: BackfillSiteMembersForAdmin :exec
-- One-shot bootstrap: grant the admin ownership of every existing site
-- that currently has zero member rows. Idempotent: once a site has any
-- member, this becomes a no-op for that site. Preserves the legacy
-- single-workspace behaviour through the C1 migration without a
-- separate data migration script.
INSERT INTO site_members (site_id, user_id, role)
SELECT s.id, ?, 'owner' FROM sites s
WHERE NOT EXISTS (SELECT 1 FROM site_members WHERE site_id = s.id);
