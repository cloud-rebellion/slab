-- name: ListAgentKeysBySite :many
SELECT id, site_id, name, capabilities, is_active, last_used_at, created_at
FROM agent_keys
WHERE site_id = ? ORDER BY created_at DESC;

-- name: GetAgentKeyByID :one
SELECT id, site_id, name, key_hash, capabilities, is_active, last_used_at, created_at
FROM agent_keys
WHERE id = ?;

-- name: GetAgentKeyByHash :one
SELECT id, site_id, name, key_hash, capabilities, is_active, last_used_at, created_at
FROM agent_keys
WHERE key_hash = ? AND is_active = 1;

-- name: CreateAgentKey :exec
INSERT INTO agent_keys (id, site_id, name, key_hash, capabilities, is_active, created_at)
VALUES (?, ?, ?, ?, ?, 1, datetime('now'));

-- name: UpdateAgentKeyLastUsed :exec
UPDATE agent_keys SET last_used_at = datetime('now') WHERE id = ?;

-- name: DeactivateAgentKey :execrows
UPDATE agent_keys SET is_active = 0 WHERE id = ? AND site_id = ?;

-- name: DeleteAgentKey :exec
DELETE FROM agent_keys WHERE id = ?;
