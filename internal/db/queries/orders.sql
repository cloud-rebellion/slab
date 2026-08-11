-- name: CreateOrder :exec
INSERT INTO orders (
    id, site_id, order_number, status,
    customer_name, customer_email, customer_phone,
    shipping_address_json, billing_address_json,
    subtotal_cents, discount_cents, shipping_cents, tax_cents, total_cents,
    currency, discount_code_id,
    payment_provider, payment_id, payment_status, payment_checkout_url,
    notes, metadata_json
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetOrderByID :one
SELECT * FROM orders WHERE id = ? AND site_id = ?;

-- name: GetOrderByOrderNumber :one
SELECT * FROM orders WHERE site_id = ? AND order_number = ?;

-- name: GetOrderByPaymentID :one
SELECT * FROM orders WHERE payment_id = ? LIMIT 1;

-- name: ListOrdersBySite :many
SELECT * FROM orders
WHERE site_id = ?
ORDER BY created_at DESC
LIMIT ?;

-- name: ListOrdersBySiteStatus :many
SELECT * FROM orders
WHERE site_id = ? AND status = ?
ORDER BY created_at DESC
LIMIT ?;

-- name: CountOrdersBySite :one
SELECT CAST(COUNT(*) AS INTEGER) FROM orders WHERE site_id = ?;

-- name: UpdateOrderStatusFrom :execrows
-- The ONLY way to write orders.status. There is deliberately no unguarded
-- sibling: every caller must name the state it believes the row is in, and the
-- guard lives in the WHERE clause rather than in Go.
--
-- Every state-machine caller used to read the order OUTSIDE its transaction,
-- run canTransition on that snapshot, and then write unconditionally. The
-- snapshot was never re-checked, so a concurrent writer that moved the row in
-- between was invisible: an operator cancel racing Mollie's `paid` webhook had
-- both sides pass their own canTransition on the same stale `pending`, and both
-- applied their side effects. Money was captured at Mollie, the stock was
-- restocked, an inventory_adjustments row said `reason='sale'`, and the
-- reconciliation alarm built for exactly that case never fired because the
-- snapshot still said `pending`.
--
-- rows_affected = 0 means "someone moved it under me": the caller must not
-- apply the side effects it decided on, and must re-read to decide again.
-- Same shape as ClaimOrderForRefund, which got this right first.
--
-- The unguarded UpdateOrderStatus this replaced was kept for a while for
-- "bookkeeping" writes that only meant to touch payment_status. That split was
-- maintained by a comment, and in a codebase whose dominant defect class is a
-- guard applied to one surface and not its twin, a comment is not a boundary.
-- A bookkeeping write still knows the state it expects, so it names it like
-- everyone else and the exception disappears.
--
-- internal/handlers/orders_release_coverage_test.go enforces the half of this
-- that a type system cannot: that every reservation-release path is gated on one
-- of these compare-and-sets.
UPDATE orders
SET status = ?,
    payment_status = ?,
    paid_at = CASE WHEN ? = 'paid' AND paid_at = '' THEN datetime('now') ELSE paid_at END,
    fulfilled_at = CASE WHEN ? = 'fulfilled' AND fulfilled_at = '' THEN datetime('now') ELSE fulfilled_at END,
    cancelled_at = CASE WHEN ? = 'cancelled' AND cancelled_at = '' THEN datetime('now') ELSE cancelled_at END,
    refunded_at = CASE WHEN ? = 'refunded' AND refunded_at = '' THEN datetime('now') ELSE refunded_at END,
    updated_at = datetime('now')
WHERE id = ? AND site_id = ? AND status = ?;

-- name: UpdateOrderRefundID :exec
UPDATE orders
SET refund_id = ?,
    updated_at = datetime('now')
WHERE id = ? AND site_id = ?;

-- name: UpdateOrderNotes :exec
UPDATE orders
SET notes = ?,
    updated_at = datetime('now')
WHERE id = ? AND site_id = ?;

-- name: UpdateOrderPayment :exec
UPDATE orders
SET payment_id = ?,
    payment_checkout_url = ?,
    updated_at = datetime('now')
WHERE id = ? AND site_id = ?;

-- name: ListStaleUnpaidOrders :many
-- Reaper feed: orders that committed their inventory + discount
-- reservations and then never acquired a payment. Checkout commits the
-- reservation tx BEFORE calling Mollie, so a Mollie outage, a missing API
-- key or an unresolvable public URL all leave the row at 'pending' with an
-- empty payment_id. No webhook will ever arrive for those, so the release
-- path was unreachable and the stock stayed consumed forever.
--
-- payment_id = '' is the load-bearing predicate: an order that DID reach
-- Mollie has one, and its reservation is legitimately held while the
-- customer sits on the payment page. Only the never-got-that-far rows are
-- safe to reap, so a slow customer is never false-reaped.
SELECT id, site_id, status, payment_status, payment_id, discount_code_id, total_cents, currency, created_at
FROM orders
WHERE status = 'pending'
  AND payment_id = ''
  AND created_at < ?
ORDER BY created_at
LIMIT ?;

-- name: ClaimOrderForRefund :execrows
-- Atomically claim an order for refund. Refund used to be check-then-act
-- across a network call: read status, canTransition, then CreateRefund at
-- Mollie. Two concurrent requests (an operator double-click, or a dashboard
-- retry on a slow response) both read 'paid', both passed the check, and both
-- called Mollie for the full amount.
--
-- Claiming locally FIRST makes the second caller lose: rows_affected = 0 means
-- someone else already owns this refund, so return early instead of calling
-- Mollie again. The guard is on the from-state, not a flag, so it also refuses
-- an order that was never refundable.
UPDATE orders
SET status = 'refunded', updated_at = datetime('now')
WHERE id = ? AND site_id = ? AND status IN ('paid', 'fulfilled');

-- name: PurgePaymentEventPayloadsOlderThan :execrows
-- Blanks the stored provider payload past the window WITHOUT deleting the row.
-- The row is the webhook idempotency key (UNIQUE on provider,payment_id,
-- event_type), so deleting it would let a replayed Mollie delivery re-run the
-- side effects. raw_json is the whole marshalled payment object and is not the
-- accounting record, so it is the part that should not be kept forever.
UPDATE payment_events
SET raw_json = '{}'
WHERE site_id = ? AND created_at < ? AND raw_json != '{}';
