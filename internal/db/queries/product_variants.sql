-- name: CreateProductVariant :exec
INSERT INTO product_variants (id, product_id, sku, name, price_cents, compare_at_price_cents, inventory_count, allow_backorder, sort_order, attributes_json)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetProductVariantByID :one
SELECT * FROM product_variants WHERE id = ?;

-- name: ListProductVariants :many
SELECT * FROM product_variants
WHERE product_id = ?
ORDER BY sort_order ASC, created_at ASC;

-- name: UpdateProductVariant :exec
UPDATE product_variants
SET sku = ?,
    name = ?,
    price_cents = ?,
    compare_at_price_cents = ?,
    inventory_count = ?,
    allow_backorder = ?,
    sort_order = ?,
    attributes_json = ?,
    updated_at = datetime('now')
WHERE id = ?;

-- name: SetVariantInventoryCount :exec
UPDATE product_variants
SET inventory_count = ?,
    updated_at = datetime('now')
WHERE id = ?;

-- name: AdjustVariantInventoryCount :exec
UPDATE product_variants
SET inventory_count = inventory_count + ?,
    updated_at = datetime('now')
WHERE id = ?;

-- name: ReserveVariantInventory :execrows
-- Atomically decrement stock at checkout, but only when backorder is allowed or
-- there is enough on hand. The single SQLite writer serializes concurrent
-- checkouts, so the last unit cannot be sold twice (availability was previously
-- only checked at checkout and decremented later on the paid webhook, a window
-- of minutes that allowed silent oversell / negative inventory).
UPDATE product_variants
SET inventory_count = inventory_count - ?,
    updated_at = datetime('now')
WHERE id = ? AND (allow_backorder = 1 OR inventory_count >= ?);

-- name: DeleteProductVariant :exec
DELETE FROM product_variants WHERE id = ?;
