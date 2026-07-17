-- name: ListDomainsBySite :many
SELECT id, site_id, hostname, status, verify_token, cert_path,
       last_check_at, last_error, is_canonical, created_at, updated_at
FROM site_domains
WHERE site_id = ?
ORDER BY is_canonical DESC, created_at ASC;

-- name: CountDomainsBySite :one
SELECT COUNT(*) FROM site_domains WHERE site_id = ?;

-- name: GetDomainByID :one
SELECT id, site_id, hostname, status, verify_token, cert_path,
       last_check_at, last_error, is_canonical, created_at, updated_at
FROM site_domains
WHERE id = ?;

-- name: GetDomainByHostname :one
SELECT id, site_id, hostname, status, verify_token, cert_path,
       last_check_at, last_error, is_canonical, created_at, updated_at
FROM site_domains
WHERE hostname = ?;

-- name: GetDomainByVerifyToken :one
SELECT id, site_id, hostname, status, verify_token, cert_path,
       last_check_at, last_error, is_canonical, created_at, updated_at
FROM site_domains
WHERE verify_token = ?;

-- name: ListDomainsByStatus :many
SELECT id, site_id, hostname, status, verify_token, cert_path,
       last_check_at, last_error, is_canonical, created_at, updated_at
FROM site_domains
WHERE status = ?
ORDER BY created_at ASC;

-- name: ListAllDomains :many
SELECT id, site_id, hostname, status, verify_token, cert_path,
       last_check_at, last_error, is_canonical, created_at, updated_at
FROM site_domains
ORDER BY created_at ASC;

-- name: CreateDomain :exec
INSERT INTO site_domains (id, site_id, hostname, status, verify_token, is_canonical)
VALUES (?, ?, ?, 'pending', ?, ?);

-- name: UpdateDomainStatus :exec
UPDATE site_domains
SET status = ?, last_error = ?, last_check_at = datetime('now'), updated_at = datetime('now')
WHERE id = ?;

-- name: UpdateDomainCertPath :exec
UPDATE site_domains
SET cert_path = ?, updated_at = datetime('now')
WHERE id = ?;

-- name: SetDomainCanonical :exec
-- Flips this row to is_canonical=1 and clears the flag on every other
-- domain row for the same site so exactly one canonical exists.
UPDATE site_domains
SET is_canonical = CASE WHEN id = ? THEN 1 ELSE 0 END,
    updated_at = datetime('now')
WHERE site_id = ?;

-- name: DeleteDomain :exec
DELETE FROM site_domains WHERE id = ?;
