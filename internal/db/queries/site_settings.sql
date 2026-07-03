-- name: ListSettingsBySite :many
SELECT * FROM site_settings WHERE site_id = ? ORDER BY category, key;

-- name: ListSettingsByCategory :many
SELECT * FROM site_settings WHERE site_id = ? AND category = ? ORDER BY key;

-- name: GetSetting :one
SELECT * FROM site_settings WHERE site_id = ? AND category = ? AND key = ?;

-- name: UpsertSetting :exec
INSERT INTO site_settings (id, site_id, category, key, value, updated_at)
VALUES (?, ?, ?, ?, ?, datetime('now'))
ON CONFLICT(site_id, category, key) DO UPDATE SET value = excluded.value, updated_at = datetime('now');

-- name: DeleteSetting :exec
DELETE FROM site_settings WHERE id = ?;

-- name: DeleteSettingsByCategory :exec
DELETE FROM site_settings WHERE site_id = ? AND category = ?;

-- Site profiles
-- name: GetSiteProfile :one
SELECT * FROM site_profiles WHERE site_id = ?;

-- name: UpsertSiteProfile :exec
INSERT INTO site_profiles (id, site_id, business_name, registration_nr, country, contact_email, contact_phone, privacy_email, security_email, address_line1, address_line2, city, postal_code, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'), datetime('now'))
ON CONFLICT(site_id) DO UPDATE SET
    business_name = excluded.business_name,
    registration_nr = excluded.registration_nr,
    country = excluded.country,
    contact_email = excluded.contact_email,
    contact_phone = excluded.contact_phone,
    privacy_email = excluded.privacy_email,
    security_email = excluded.security_email,
    address_line1 = excluded.address_line1,
    address_line2 = excluded.address_line2,
    city = excluded.city,
    postal_code = excluded.postal_code,
    updated_at = datetime('now');

-- Site architecture
-- name: GetSiteArchitecture :one
SELECT * FROM site_architecture WHERE site_id = ?;

-- name: UpsertSiteArchitecture :exec
INSERT INTO site_architecture (id, site_id, structure_type, max_depth, created_at, updated_at)
VALUES (?, ?, ?, ?, datetime('now'), datetime('now'))
ON CONFLICT(site_id) DO UPDATE SET
    structure_type = excluded.structure_type,
    max_depth = excluded.max_depth,
    updated_at = datetime('now');

-- Silos
-- name: ListSilosBySite :many
SELECT * FROM site_silos WHERE site_id = ? ORDER BY sort_order;

-- The by-id silo queries are site-scoped preventively: they have no
-- callers yet, and an unscoped WHERE id = ? is exactly the shape that
-- caused the agent-API block IDORs. Whoever wires them inherits the
-- tenant guard for free.
-- name: GetSiloByID :one
SELECT * FROM site_silos WHERE id = ? AND site_id = ?;

-- name: CreateSilo :exec
INSERT INTO site_silos (id, site_id, name, slug_prefix, silo_type, sort_order, created_at)
VALUES (?, ?, ?, ?, ?, ?, datetime('now'));

-- name: UpdateSilo :exec
UPDATE site_silos SET name = ?, slug_prefix = ?, silo_type = ?, sort_order = ? WHERE id = ? AND site_id = ?;

-- name: DeleteSilo :exec
DELETE FROM site_silos WHERE id = ? AND site_id = ?;

-- Allowed scripts
-- name: ListAllowedScriptsBySite :many
SELECT * FROM allowed_scripts WHERE site_id = ? ORDER BY domain;

-- name: CreateAllowedScript :exec
INSERT INTO allowed_scripts (id, site_id, domain, purpose, kind, is_active, created_at)
VALUES (?, ?, ?, ?, ?, 1, datetime('now'));

-- name: UpdateAllowedScript :exec
UPDATE allowed_scripts SET domain = ?, purpose = ?, kind = ?, is_active = ? WHERE id = ?;

-- name: DeleteAllowedScript :exec
DELETE FROM allowed_scripts WHERE id = ?;
