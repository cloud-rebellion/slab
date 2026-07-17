-- name: CreateDiscountCode :exec
INSERT INTO discount_codes (id, site_id, code, kind, value, min_subtotal_cents, max_uses, starts_at, ends_at, applies_to, applies_to_ids_json, is_active)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetDiscountCodeByID :one
SELECT * FROM discount_codes WHERE id = ? AND site_id = ?;

-- name: GetDiscountCodeByCode :one
SELECT * FROM discount_codes WHERE site_id = ? AND code = ?;

-- name: ListDiscountCodesBySite :many
SELECT * FROM discount_codes
WHERE site_id = ?
ORDER BY is_active DESC, created_at DESC
LIMIT ?;

-- name: UpdateDiscountCode :exec
UPDATE discount_codes
SET code = ?,
    kind = ?,
    value = ?,
    min_subtotal_cents = ?,
    max_uses = ?,
    starts_at = ?,
    ends_at = ?,
    applies_to = ?,
    applies_to_ids_json = ?,
    is_active = ?,
    updated_at = datetime('now')
WHERE id = ? AND site_id = ?;

-- name: IncrementDiscountCodeUsedCount :exec
UPDATE discount_codes
SET used_count = used_count + 1,
    updated_at = datetime('now')
WHERE id = ? AND site_id = ?;

-- name: ReserveDiscountCodeUse :execrows
-- Atomically claim one use of a code, but only if it is still under its cap.
-- The single SQLite writer serializes concurrent checkouts, so a max_uses code
-- cannot be over-redeemed (the read-then-act check at checkout was a TOCTOU:
-- the increment only happened later on the paid webhook).
UPDATE discount_codes
SET used_count = used_count + 1,
    updated_at = datetime('now')
WHERE id = ? AND site_id = ? AND (max_uses = 0 OR used_count < max_uses);

-- name: ReleaseDiscountCodeUse :execrows
-- Return a reserved use when the payment ultimately fails/expires/cancels.
UPDATE discount_codes
SET used_count = used_count - 1,
    updated_at = datetime('now')
WHERE id = ? AND site_id = ? AND used_count > 0;

-- name: DeleteDiscountCode :exec
DELETE FROM discount_codes WHERE id = ? AND site_id = ?;
