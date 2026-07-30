// Package handlers: orders + checkout + Mollie webhook endpoints.
//
// Sprint 2 slice B of the WP/Webflow replacement roadmap. Picks up
// where slice A's catalog left off and ships the order pipeline:
// public storefront checkout creates a pending order + Mollie
// payment, Mollie's webhook updates the order status idempotently,
// the admin dashboard + agent MCP manage post-purchase state
// (fulfillment, refund).
//
// State machine:
//   pending  -> paid       (Mollie webhook 'paid')
//   pending  -> failed     (Mollie webhook 'failed' / 'expired' / 'canceled')
//   paid     -> fulfilled  (operator marks shipped)
//   paid     -> refunded   (operator triggers refund)
//   fulfilled -> refunded  (post-delivery refund)
//
// Inventory side-effects on 'paid': decrement each line item's
// variant inventory_count and write an inventory_adjustments row
// with reason='sale'. Discount code used_count is incremented if a
// code was applied. All inside the webhook handler, gated by the
// idempotent payment_events insert.

package handlers

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/bright-interaction/slab/internal/config"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/payments/mollie"
	"github.com/bright-interaction/slab/internal/store"
)

// Order status transition map. Empty target list = terminal.
var orderTransitions = map[string][]string{
	"pending":   {"paid", "cancelled", "failed"},
	"paid":      {"fulfilled", "refunded"},
	"fulfilled": {"refunded"},
	"cancelled": {},
	"refunded":  {},
	"failed":    {},
}

func validOrderStatus(s string) bool {
	switch s {
	case "pending", "paid", "fulfilled", "cancelled", "refunded", "failed":
		return true
	}
	return false
}

// canTransition reports whether `from -> to` is a legal edge.
//
// The `return from == to` fallback is gone. It made every self-transition legal,
// including from a TERMINAL state, so `refunded -> refunded` passed the refund
// gate: o.PaymentID is still populated on a refunded order, so Refund called
// mollie.CreateRefund a second time for the full amount and Mollie's own
// remaining-balance check was the only thing between that and a double payout.
// An upstream API contract is not an acceptable place to keep our money guard.
// It also let `fulfilled -> fulfilled` and `cancelled -> cancelled` through
// every gate that asks this question.
//
// Callers that genuinely want idempotency should compare states themselves
// before asking, which is clearer than encoding it here for everyone.
func canTransition(from, to string) bool {
	for _, t := range orderTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

type OrderHandler struct {
	cfg     *config.Config
	queries *store.Queries
	db      *sql.DB
	// mollieBaseURL overrides the Mollie API base in the webhook path.
	// Empty in production (uses the real api.mollie.com); tests set it
	// to an httptest stub. Same-package field, no constructor change.
	mollieBaseURL string
}

func NewOrderHandler(cfg *config.Config, queries *store.Queries, db *sql.DB) *OrderHandler {
	return &OrderHandler{cfg: cfg, queries: queries, db: db}
}

func orderInSite(ctx context.Context, q *store.Queries, w http.ResponseWriter, orderID, siteID string) (store.Order, bool) {
	o, err := q.GetOrderByID(ctx, store.GetOrderByIDParams{ID: orderID, SiteID: siteID})
	if err != nil {
		writeError(w, http.StatusNotFound, "Order not found")
		return store.Order{}, false
	}
	return o, true
}

// orderOut wraps the store row plus the line items for the response.
type orderOut struct {
	Order store.Order       `json:"order"`
	Items []store.OrderItem `json:"items"`
}

func (h *OrderHandler) loadOut(ctx context.Context, o store.Order) (orderOut, error) {
	items, err := h.queries.ListOrderItems(ctx, o.ID)
	if err != nil {
		return orderOut{}, err
	}
	return orderOut{Order: o, Items: items}, nil
}

// List returns orders for the dashboard. Optional ?status filter.
func (h *OrderHandler) List(w http.ResponseWriter, r *http.Request) {
	if authmw.GetUser(r) == nil && authmw.GetAgent(r) == nil {
		writeError(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	siteID := urlParam(r, "siteID")
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	var rows []store.Order
	var err error
	if status != "" && status != "all" {
		if !validOrderStatus(status) {
			writeError(w, http.StatusBadRequest, "invalid status filter")
			return
		}
		rows, err = h.queries.ListOrdersBySiteStatus(r.Context(), store.ListOrdersBySiteStatusParams{
			SiteID: siteID, Status: status, Limit: 500,
		})
	} else {
		rows, err = h.queries.ListOrdersBySite(r.Context(), store.ListOrdersBySiteParams{
			SiteID: siteID, Limit: 500,
		})
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list orders failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"orders": rows, "count": len(rows)})
}

func (h *OrderHandler) Get(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	orderID := urlParam(r, "orderID")
	o, ok := orderInSite(r.Context(), h.queries, w, orderID, siteID)
	if !ok {
		return
	}
	out, err := h.loadOut(r.Context(), o)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load items failed")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// UpdateStatus admin-side flips the order through the state machine.
// Body: { status: "fulfilled" }. Reject illegal transitions.
func (h *OrderHandler) UpdateStatus(w http.ResponseWriter, r *http.Request) {
	// Order state transitions (cancel/fulfil) are money-adjacent; gate human
	// sessions to owner/admin, leave agent callers on their site scope.
	if authmw.GetUser(r) != nil && !authmw.RequireOwnerOrAdmin(w, r, h.queries) {
		return
	}
	siteID := urlParam(r, "siteID")
	orderID := urlParam(r, "orderID")
	o, ok := orderInSite(r.Context(), h.queries, w, orderID, siteID)
	if !ok {
		return
	}
	var req struct {
		Status string `json:"status"`
		Notes  string `json:"notes"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	next := strings.ToLower(strings.TrimSpace(req.Status))
	if !validOrderStatus(next) {
		writeError(w, http.StatusBadRequest, "invalid status")
		return
	}
	if !canTransition(o.Status, next) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("cannot transition from %q to %q", o.Status, next))
		return
	}
	// Releasing the reservation is part of leaving `pending`, not something
	// only the Mollie `failed` webhook does. pending -> cancelled is a legal
	// transition here and used to write the status and nothing else, so an
	// operator cancelling an unpaid order left the stock decremented and the
	// discount use burned, with no inventory_adjustments row to explain it.
	// The last unit of a variant became permanently unsellable and the
	// discrepancy was invisible in the audit view.
	//
	// Gated on the FROM state, so this cannot double-release: once the row is
	// not `pending` the branch cannot be taken again, and `paid -> failed` is
	// not an edge in orderTransitions.
	releasing := o.Status == "pending" && (next == "cancelled" || next == "failed")

	tx, err := h.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "update status failed")
		return
	}
	defer tx.Rollback()
	qtx := h.queries.WithTx(tx)

	if err := qtx.UpdateOrderStatus(r.Context(), store.UpdateOrderStatusParams{
		Status:        next,
		PaymentStatus: o.PaymentStatus,
		Column3:       next,
		Column4:       next,
		Column5:       next,
		Column6:       next,
		ID:            orderID,
		SiteID:        siteID,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "update status failed")
		return
	}
	if releasing {
		if err := h.releaseReservations(r.Context(), qtx, o); err != nil {
			// Same tx, so the status write rolls back with it. Better to fail
			// the cancel than to record a cancel that silently ate the stock.
			writeError(w, http.StatusInternalServerError, "update status failed: could not release reservations")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "update status failed: could not commit")
		return
	}
	if req.Notes != "" {
		_ = h.queries.UpdateOrderNotes(r.Context(), store.UpdateOrderNotesParams{
			Notes: req.Notes, ID: orderID, SiteID: siteID,
		})
	}
	updated, _ := h.queries.GetOrderByID(r.Context(), store.GetOrderByIDParams{ID: orderID, SiteID: siteID})
	out, _ := h.loadOut(r.Context(), updated)
	writeJSON(w, http.StatusOK, out)
}

// Refund triggers a Mollie refund for the full order amount and
// flips the order to status='refunded' optimistically. The webhook
// then confirms the refund completion (Mollie can take minutes).
func (h *OrderHandler) Refund(w http.ResponseWriter, r *http.Request) {
	// Refund moves money; gate human sessions to owner/admin (a low-trust
	// workspace member must not issue refunds). Agent callers keep their existing
	// site-scoped authorization.
	if authmw.GetUser(r) != nil && !authmw.RequireOwnerOrAdmin(w, r, h.queries) {
		return
	}
	siteID := urlParam(r, "siteID")
	orderID := urlParam(r, "orderID")
	o, ok := orderInSite(r.Context(), h.queries, w, orderID, siteID)
	if !ok {
		return
	}
	if !canTransition(o.Status, "refunded") {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("cannot refund order in status %q", o.Status))
		return
	}
	if o.PaymentID == "" {
		writeError(w, http.StatusBadRequest, "order has no payment_id; nothing to refund at Mollie")
		return
	}
	apiKey, err := h.mollieAPIKey(r.Context(), siteID)
	if err != nil {
		writeError(w, http.StatusFailedDependency, err.Error())
		return
	}

	// Claim the refund locally BEFORE calling Mollie. This was check-then-act
	// across a network call: two concurrent requests (an operator
	// double-clicking, or the dashboard retrying a slow response) both read
	// 'paid' above, both passed canTransition, and both created a refund at
	// Mollie for the full amount. Only the last refund_id was persisted, so
	// the second payout was invisible locally.
	//
	// Whoever wins the guarded UPDATE owns the refund; the loser sees zero
	// rows and stops here without touching Mollie.
	claimed, err := h.queries.ClaimOrderForRefund(r.Context(), store.ClaimOrderForRefundParams{
		ID:     orderID,
		SiteID: siteID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "refund failed: could not claim order")
		return
	}
	if claimed == 0 {
		writeError(w, http.StatusConflict, "this order is already being refunded or is no longer refundable")
		return
	}

	client := mollie.NewClient(apiKey)
	refund, err := client.CreateRefund(r.Context(), o.PaymentID, mollie.CreateRefundInput{
		Amount:      mollie.MoneyValue{Currency: o.Currency, Value: mollie.FormatAmount(o.TotalCents)},
		Description: "Refund " + o.OrderNumber,
	})
	if err != nil {
		// Release the claim: we marked it 'refunded' optimistically, and no
		// money moved. Leaving it claimed would strand the order in a state
		// that says refunded while the customer still holds their charge.
		if rerr := h.queries.UpdateOrderStatus(r.Context(), store.UpdateOrderStatusParams{
			Status:        o.Status,
			PaymentStatus: o.PaymentStatus,
			Column3:       o.Status, Column4: o.Status, Column5: o.Status, Column6: o.Status,
			ID:     orderID,
			SiteID: siteID,
		}); rerr != nil {
			slog.Error("refund: Mollie failed AND un-claiming the order failed; it now reads refunded with no money moved",
				"order_id", orderID, "site_id", siteID, "err", rerr)
		}
		writeError(w, http.StatusBadGateway, "Mollie refund failed: "+err.Error())
		return
	}
	// Money has already moved at Mollie; a failed local write must not
	// fail the refund, but it MUST be loud and land in the response so
	// the operator reconciles the row instead of trusting a stale
	// 'paid' status.
	var reconcileWarnings []string
	if err := h.queries.UpdateOrderRefundID(r.Context(), store.UpdateOrderRefundIDParams{
		RefundID: refund.ID, ID: orderID, SiteID: siteID,
	}); err != nil {
		slog.Error("refund: Mollie refund executed but persisting refund_id failed",
			"order_id", orderID, "site_id", siteID, "refund_id", refund.ID, "err", err)
		reconcileWarnings = append(reconcileWarnings, "refund_id not persisted; reconcile manually against Mollie refund "+refund.ID)
	}
	if err := h.queries.UpdateOrderStatus(r.Context(), store.UpdateOrderStatusParams{
		Status:        "refunded",
		PaymentStatus: o.PaymentStatus,
		Column3:       "refunded", Column4: "refunded", Column5: "refunded", Column6: "refunded",
		ID:     orderID,
		SiteID: siteID,
	}); err != nil {
		slog.Error("refund: Mollie refund executed but status flip failed",
			"order_id", orderID, "site_id", siteID, "refund_id", refund.ID, "err", err)
		reconcileWarnings = append(reconcileWarnings, "order status still shows the pre-refund value; set it to refunded manually")
	}
	updated, _ := h.queries.GetOrderByID(r.Context(), store.GetOrderByIDParams{ID: orderID, SiteID: siteID})
	out, _ := h.loadOut(r.Context(), updated)
	if len(reconcileWarnings) > 0 {
		writeJSON(w, http.StatusOK, map[string]any{"order": out, "reconcile_warnings": reconcileWarnings})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// mollieAPIKey reads the per-tenant Mollie API key from site_settings.
// category='payments', key='mollie_api_key'. Empty = not configured.
func (h *OrderHandler) mollieAPIKey(ctx context.Context, siteID string) (string, error) {
	row, err := h.queries.GetSetting(ctx, store.GetSettingParams{
		SiteID: siteID, Category: "payments", Key: "mollie_api_key",
	})
	if err != nil || strings.TrimSpace(row.Value) == "" {
		return "", errors.New("Mollie API key not configured for this site (set payments.mollie_api_key in Settings)")
	}
	return strings.TrimSpace(row.Value), nil
}

// CheckoutInput is the body of POST /api/sites/{siteID}/checkout.
// Server-side validation: every variant must exist + belong to the
// site + (have inventory >= quantity OR allow_backorder). Prices are
// re-read from the DB, not trusted from the client.
type CheckoutInput struct {
	Items []struct {
		VariantID string `json:"variant_id"`
		Quantity  int64  `json:"quantity"`
	} `json:"items"`
	Customer struct {
		Name  string `json:"name"`
		Email string `json:"email"`
		Phone string `json:"phone"`
	} `json:"customer"`
	ShippingAddress map[string]any `json:"shipping_address"`
	BillingAddress  map[string]any `json:"billing_address"`
	DiscountCode    string         `json:"discount_code"`
	ReturnURL       string         `json:"return_url"`
	Locale          string         `json:"locale"`
}

// Checkout is the public-facing endpoint. No session auth (visitor
// is anonymous) but it IS scoped by siteID in the URL. Rate limiting
// is enforced by the existing per-site middleware.
//
// Returns { checkout_url, order_number, total_cents, currency } on
// success. The frontend redirects the browser to checkout_url.
func (h *OrderHandler) Checkout(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	var in CheckoutInput
	if err := parseJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	if len(in.Items) == 0 {
		writeError(w, http.StatusBadRequest, "items required")
		return
	}
	if !strings.Contains(in.Customer.Email, "@") {
		writeError(w, http.StatusBadRequest, "valid customer email required")
		return
	}
	ctx := r.Context()

	// Validate items + recompute prices server-side.
	type validatedItem struct {
		Variant   store.ProductVariant
		Product   store.Product
		UnitPrice int64
		LineTotal int64
		Quantity  int64
	}
	validated := make([]validatedItem, 0, len(in.Items))
	var subtotal int64
	var currency string
	for _, it := range in.Items {
		if it.Quantity <= 0 || it.Quantity > 999 {
			writeError(w, http.StatusBadRequest, "item quantity must be 1-999")
			return
		}
		v, err := h.queries.GetProductVariantByID(ctx, it.VariantID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "variant not found: "+it.VariantID)
			return
		}
		p, err := h.queries.GetProductByID(ctx, store.GetProductByIDParams{ID: v.ProductID, SiteID: siteID})
		if err != nil {
			writeError(w, http.StatusBadRequest, "product not found for variant "+it.VariantID)
			return
		}
		if p.Status != "active" {
			writeError(w, http.StatusBadRequest, "product not active: "+p.Slug)
			return
		}
		if v.AllowBackorder != 1 && v.InventoryCount < it.Quantity {
			writeError(w, http.StatusConflict, fmt.Sprintf("insufficient inventory for %s (have %d, requested %d)", p.Slug, v.InventoryCount, it.Quantity))
			return
		}
		unit := v.PriceCents
		if unit == 0 {
			unit = p.BasePriceCents
		}
		if currency == "" {
			currency = p.Currency
		} else if currency != p.Currency {
			writeError(w, http.StatusBadRequest, "cart contains mixed currencies; this is not yet supported")
			return
		}
		line := unit * it.Quantity
		validated = append(validated, validatedItem{Variant: v, Product: p, UnitPrice: unit, LineTotal: line, Quantity: it.Quantity})
		subtotal += line
	}

	// Apply discount code if provided.
	var discountID string
	var discountCents int64
	if code := strings.ToUpper(strings.TrimSpace(in.DiscountCode)); code != "" {
		dc, err := h.queries.GetDiscountCodeByCode(ctx, store.GetDiscountCodeByCodeParams{SiteID: siteID, Code: code})
		if err != nil {
			writeError(w, http.StatusBadRequest, "discount code not found")
			return
		}
		if dc.IsActive != 1 {
			writeError(w, http.StatusBadRequest, "discount code is not active")
			return
		}
		if dc.MaxUses > 0 && dc.UsedCount >= dc.MaxUses {
			writeError(w, http.StatusBadRequest, "discount code has reached max uses")
			return
		}
		if dc.MinSubtotalCents > 0 && subtotal < dc.MinSubtotalCents {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("discount code requires min subtotal of %d cents", dc.MinSubtotalCents))
			return
		}
		// Date window check. Fails CLOSED on an unparseable value: the `err == nil`
		// conjunct used to mean a malformed timestamp SKIPPED the window entirely,
		// so a long-expired 100%-off code was honoured. That is not a hypothetical
		// value either, it is exactly what the two obvious HTML inputs submit:
		// datetime-local gives "2026-01-01T00:00" and date gives "2026-01-01",
		// neither of which is RFC3339. Both were proven redeemable after expiry.
		if dc.StartsAt != "" {
			t, err := time.Parse(time.RFC3339, dc.StartsAt)
			if err != nil {
				writeError(w, http.StatusBadRequest, "discount code has an unreadable start date; ask an admin to re-save it")
				return
			}
			if time.Now().Before(t) {
				writeError(w, http.StatusBadRequest, "discount code is not yet active")
				return
			}
		}
		if dc.EndsAt != "" {
			t, err := time.Parse(time.RFC3339, dc.EndsAt)
			if err != nil {
				writeError(w, http.StatusBadRequest, "discount code has an unreadable end date; ask an admin to re-save it")
				return
			}
			if time.Now().After(t) {
				writeError(w, http.StatusBadRequest, "discount code has expired")
				return
			}
		}
		// Scope: a code restricted to specific products/categories only discounts
		// the matching lines, not the whole cart. applies_to/applies_to_ids were
		// stored + validated on create but never enforced at redemption, so a
		// "50% off Accessories" code discounted every item.
		eligible := subtotal
		if dc.AppliesTo == "products" || dc.AppliesTo == "categories" {
			var ids []string
			_ = json.Unmarshal([]byte(dc.AppliesToIdsJson), &ids)
			idset := make(map[string]struct{}, len(ids))
			for _, id := range ids {
				idset[id] = struct{}{}
			}
			eligible = 0
			for _, v := range validated {
				key := v.Product.ID
				if dc.AppliesTo == "categories" {
					key = v.Product.Category
				}
				if _, ok := idset[key]; ok {
					eligible += v.LineTotal
				}
			}
			if eligible == 0 {
				writeError(w, http.StatusBadRequest, "discount code does not apply to any item in the cart")
				return
			}
		}
		switch dc.Kind {
		case "percent":
			discountCents = eligible * dc.Value / 10000
		case "fixed":
			discountCents = dc.Value
		}
		if discountCents > eligible {
			discountCents = eligible
		}
		discountID = dc.ID
	}

	total := subtotal - discountCents
	if total < 0 {
		total = 0
	}

	// Create order + items.
	orderID := newID()
	orderNumber, err := h.nextOrderNumber(ctx, siteID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not allocate order number")
		return
	}
	shippingJSON, _ := json.Marshal(in.ShippingAddress)
	billingJSON, _ := json.Marshal(in.BillingAddress)

	// Order + items are one atomic write: a swallowed item insert used
	// to leave a short order in the DB while Mollie charged the FULL
	// total computed from all items (customer pays for a line the
	// fulfilment view never shows).
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create order failed: could not start transaction")
		return
	}
	defer tx.Rollback()
	qtx := h.queries.WithTx(tx)
	// Reserve inventory atomically inside the order tx. The checkout-time
	// availability check (above) is only a fast pre-check; the guarded decrement
	// here is the authoritative gate, so two overlapping checkouts cannot both
	// sell the last unit (the decrement used to happen minutes later on the paid
	// webhook, allowing silent oversell / negative stock).
	for _, v := range validated {
		n, rerr := qtx.ReserveVariantInventory(ctx, store.ReserveVariantInventoryParams{
			InventoryCount: v.Quantity, ID: v.Variant.ID, InventoryCount_2: v.Quantity,
		})
		if rerr != nil {
			writeError(w, http.StatusInternalServerError, "create order failed: inventory reserve")
			return
		}
		if n == 0 {
			writeError(w, http.StatusConflict, "insufficient inventory for "+v.Product.Slug)
			return
		}
	}
	// Reserve the discount use atomically, so a max_uses-capped code cannot be
	// over-redeemed by concurrent checkouts (the count used to be bumped only on
	// the paid webhook, so a read-check at checkout never saw it).
	if discountID != "" {
		n, rerr := qtx.ReserveDiscountCodeUse(ctx, store.ReserveDiscountCodeUseParams{ID: discountID, SiteID: siteID})
		if rerr != nil {
			writeError(w, http.StatusInternalServerError, "create order failed: discount reserve")
			return
		}
		if n == 0 {
			writeError(w, http.StatusBadRequest, "discount code has reached max uses")
			return
		}
	}
	if err := qtx.CreateOrder(ctx, store.CreateOrderParams{
		ID:                  orderID,
		SiteID:              siteID,
		OrderNumber:         orderNumber,
		Status:              "pending",
		CustomerName:        strings.TrimSpace(in.Customer.Name),
		CustomerEmail:       strings.TrimSpace(in.Customer.Email),
		CustomerPhone:       strings.TrimSpace(in.Customer.Phone),
		ShippingAddressJson: string(shippingJSON),
		BillingAddressJson:  string(billingJSON),
		SubtotalCents:       subtotal,
		DiscountCents:       discountCents,
		ShippingCents:       0,
		TaxCents:            0,
		TotalCents:          total,
		Currency:            currency,
		DiscountCodeID:      discountID,
		PaymentProvider:     "mollie",
		PaymentID:           "",
		PaymentStatus:       "",
		PaymentCheckoutUrl:  "",
		Notes:               "",
		MetadataJson:        "{}",
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "create order failed: "+err.Error())
		return
	}
	for _, v := range validated {
		if err := qtx.CreateOrderItem(ctx, store.CreateOrderItemParams{
			ID:             newID(),
			OrderID:        orderID,
			VariantID:      v.Variant.ID,
			ProductID:      v.Product.ID,
			ProductName:    v.Product.Name,
			VariantName:    v.Variant.Name,
			Sku:            v.Variant.Sku,
			Quantity:       v.Quantity,
			UnitPriceCents: v.UnitPrice,
			TotalCents:     v.LineTotal,
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "create order failed: could not persist order items")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "create order failed: could not commit")
		return
	}

	// Create Mollie payment. Skip if API key not configured (order stays
	// pending and the operator finalises manually). Also skip if no public
	// URL is resolvable: Mollie needs absolute https URLs, and rather than
	// fall back to someone else's hostname we keep the order pending and
	// surface the misconfiguration to the operator.
	checkoutURL := ""
	publicBase := strings.TrimRight(h.publicURL(ctx, siteID), "/")
	apiKey, err := h.mollieAPIKey(ctx, siteID)
	if err == nil && publicBase != "" {
		client := mollie.NewClient(apiKey)
		returnURL := in.ReturnURL
		if returnURL == "" {
			returnURL = fmt.Sprintf("%s/checkout/done?order=%s", publicBase, orderNumber)
		}
		webhookURL := fmt.Sprintf("%s/api/sites/%s/payments/mollie/webhook", publicBase, siteID)
		pay, err := client.CreatePayment(ctx, mollie.CreatePaymentInput{
			Amount:      mollie.MoneyValue{Currency: currency, Value: mollie.FormatAmount(total)},
			Description: orderNumber,
			RedirectURL: returnURL,
			WebhookURL:  webhookURL,
			Metadata:    map[string]any{"order_id": orderID, "order_number": orderNumber, "site_id": siteID},
			Locale:      in.Locale,
		})
		if err != nil {
			writeError(w, http.StatusBadGateway, "Mollie create payment failed: "+err.Error())
			return
		}
		if pay.Links.Checkout != nil {
			checkoutURL = pay.Links.Checkout.Href
		}
		_ = h.queries.UpdateOrderStatus(ctx, store.UpdateOrderStatusParams{
			Status:        "pending",
			PaymentStatus: pay.Status,
			Column3:       "pending", Column4: "pending", Column5: "pending", Column6: "pending",
			ID:     orderID,
			SiteID: siteID,
		})
		// Store payment_id + checkout_url via a small follow-up.
		if err := h.setOrderPayment(ctx, siteID, orderID, pay.ID, checkoutURL); err != nil {
			writeError(w, http.StatusInternalServerError, "store payment id failed")
			return
		}
	}

	updated, _ := h.queries.GetOrderByID(ctx, store.GetOrderByIDParams{ID: orderID, SiteID: siteID})
	writeJSON(w, http.StatusCreated, map[string]any{
		"order":        updated,
		"checkout_url": checkoutURL,
	})
}

func (h *OrderHandler) setOrderPayment(ctx context.Context, siteID, orderID, paymentID, checkoutURL string) error {
	// We don't have a dedicated SetPayment query (would clutter the sqlc
	// surface for two columns). Use UpdateOrderStatus, then a raw SQL
	// patch via the queries' underlying DB. For now we encode the
	// payment_id + checkout_url via metadata + a separate JSON write.
	// Simpler: add a small inline UPDATE via the queries object's db.
	// To stay sqlc-clean, we'll keep an UpdateOrderPayment query.
	return h.queries.UpdateOrderPayment(ctx, store.UpdateOrderPaymentParams{
		PaymentID:          paymentID,
		PaymentCheckoutUrl: checkoutURL,
		ID:                 orderID,
		SiteID:             siteID,
	})
}

// publicURL returns the customer-facing root URL for redirects +
// webhook callbacks. Mollie needs an absolute https:// URL.
//
// Resolution order, picked so an OSS self-hoster always lands on their
// own hostname rather than ours:
//  1. The canonical site_domains row for this site (operator-configured,
//     always the right answer when present).
//  2. cfg.PrimaryDomain (env: ATOMICSITE_PRIMARY_DOMAIN), the multi-tenant
//     apex set at boot.
//  3. cfg.BaseURL (env: BASE_URL), the dev / single-site fallback.
//  4. Empty string. Caller-side checkout flow logs and skips Mollie when
//     no public URL is resolvable; better than redirecting customers to
//     Bright Interaction's hostname on a self-hosted instance.
func (h *OrderHandler) publicURL(ctx context.Context, siteID string) string {
	if domains, err := h.queries.ListDomainsBySite(ctx, siteID); err == nil {
		for _, d := range domains {
			if d.Hostname != "" && (d.IsCanonical == 1 || d.Status == "active" || d.Status == "verified") {
				return "https://" + d.Hostname
			}
		}
	}
	if h.cfg != nil && h.cfg.PrimaryDomain != "" {
		return "https://" + h.cfg.PrimaryDomain
	}
	if h.cfg != nil && h.cfg.BaseURL != "" {
		return h.cfg.BaseURL
	}
	return ""
}

// nextOrderNumber generates a human-friendly order number with a
// random suffix to avoid collisions during concurrent checkouts. The
// UNIQUE(site_id, order_number) constraint catches duplicates if the
// random suffix ever collides.
func (h *OrderHandler) nextOrderNumber(ctx context.Context, siteID string) (string, error) {
	year := time.Now().UTC().Year()
	for i := 0; i < 5; i++ {
		suffix, err := randomDigits(6)
		if err != nil {
			return "", err
		}
		candidate := fmt.Sprintf("ATM-%d-%s", year, suffix)
		if _, err := h.queries.GetOrderByOrderNumber(ctx, store.GetOrderByOrderNumberParams{SiteID: siteID, OrderNumber: candidate}); err != nil {
			return candidate, nil
		}
	}
	return "", errors.New("could not allocate unique order number after 5 attempts")
}

func randomDigits(n int) (string, error) {
	const digits = "0123456789"
	b := make([]byte, n)
	max := big.NewInt(int64(len(digits)))
	for i := range b {
		k, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b[i] = digits[k.Int64()]
	}
	return string(b), nil
}

// Webhook is the Mollie callback. Mollie POSTs id=<paymentID> in
// application/x-www-form-urlencoded; the server then fetches the
// payment from Mollie to read status. Idempotency is enforced via
// payment_events: INSERT-OR-IGNORE on (provider, payment_id,
// event_type), then guard the side-effects with the processed flag.
//
// This route is OUTSIDE the session-auth middleware (Mollie is
// anonymous). Cross-tenant safety comes from the {siteID} URL param
// being used to load that site's API key, and the fact that the
// payment_id belongs to that key (so a foreign key cannot drive
// state on a different tenant).
func (h *OrderHandler) Webhook(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid form body")
		return
	}
	paymentID := strings.TrimSpace(r.Form.Get("id"))
	if paymentID == "" {
		writeError(w, http.StatusBadRequest, "id required")
		return
	}
	ctx := r.Context()

	apiKey, err := h.mollieAPIKey(ctx, siteID)
	if err != nil {
		// 200 so Mollie stops retrying; the misconfig will surface
		// through the order staying pending in the dashboard.
		writeJSON(w, http.StatusOK, map[string]string{"status": "no api key configured"})
		return
	}
	mc := mollie.NewClient(apiKey)
	if h.mollieBaseURL != "" {
		mc = mc.WithBaseURL(h.mollieBaseURL)
	}
	pay, err := mc.GetPayment(ctx, paymentID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "mollie get payment failed: "+err.Error())
		return
	}

	order, err := h.queries.GetOrderByPaymentID(ctx, paymentID)
	if err != nil {
		// Unknown payment ID for this site; respond 200 so Mollie
		// stops retrying but log the gap via the response body.
		writeJSON(w, http.StatusOK, map[string]string{"status": "order not found for payment"})
		return
	}
	if order.SiteID != siteID {
		// Cross-tenant: the URL siteID does not own this payment.
		writeError(w, http.StatusForbidden, "payment does not belong to this site")
		return
	}

	rawJSON, _ := json.Marshal(pay)
	eventType := pay.Status
	eventID := newID()

	// Everything below runs in one transaction on the single SQLite
	// writer. The (provider, payment_id, event_type) row is the
	// idempotency key: MarkPaymentEventProcessed is a compare-and-set
	// (WHERE processed=0) so exactly one of N retried/parallel
	// deliveries claims the event and runs the side-effects. Wrapping
	// the claim + status flip + inventory/discount writes together means
	// a mid-flight failure rolls the whole thing back (processed stays
	// 0) and Mollie's retry re-processes cleanly, with no double
	// inventory decrement or double discount increment.
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start transaction")
		return
	}
	defer tx.Rollback()
	qtx := h.queries.WithTx(tx)

	if err := qtx.CreatePaymentEvent(ctx, store.CreatePaymentEventParams{
		ID:        eventID,
		SiteID:    siteID,
		OrderID:   order.ID,
		Provider:  "mollie",
		PaymentID: paymentID,
		EventType: eventType,
		RawJson:   string(rawJSON),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "could not record payment event")
		return
	}
	claimed, err := qtx.MarkPaymentEventProcessed(ctx, store.MarkPaymentEventProcessedParams{
		Provider: "mollie", PaymentID: paymentID, EventType: eventType,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not claim payment event")
		return
	}
	if claimed == 0 {
		// A prior delivery already processed this exact event.
		writeJSON(w, http.StatusOK, map[string]string{"status": "already processed"})
		return
	}

	// Map Mollie status -> our order.status.
	newStatus := order.Status
	switch pay.Status {
	case "paid":
		// Never mark an order paid for an amount/currency that does not
		// match what we charged. Mollie pins the amount at payment
		// creation, so a mismatch means the payment does not correspond
		// to this order; leave it pending for an operator to inspect.
		if pay.Amount.Value != mollie.FormatAmount(order.TotalCents) || !strings.EqualFold(pay.Amount.Currency, order.Currency) {
			slog.Warn("mollie webhook: paid amount mismatch; leaving order unpaid",
				"order_id", order.ID, "site_id", siteID,
				"want", mollie.FormatAmount(order.TotalCents)+" "+order.Currency,
				"got", pay.Amount.Value+" "+pay.Amount.Currency)
			if err := tx.Commit(); err != nil {
				writeError(w, http.StatusInternalServerError, "commit failed")
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": "amount mismatch", "new_order_status": order.Status})
			return
		}
		if canTransition(order.Status, "paid") {
			newStatus = "paid"
		} else {
			// A CAPTURED payment we cannot apply. This branch used to fall
			// through silently: newStatus stayed as-is, the `newStatus !=
			// order.Status` guard below skipped the whole update block so
			// payment_status was never written either, and the handler returned
			// a bare 200 {"status":"ok"}. Money had moved at Mollie and the only
			// trace anywhere in the database was the payment_events row. No log
			// line, no operator signal.
			//
			// Reachable whenever a `paid` event lands late on an order that has
			// since gone terminal: cancelled (empty transition set) or failed
			// (after releaseReservations already restocked). Loud is the whole
			// point here, so this logs at ERROR and says so in the response.
			slog.Error("mollie webhook: PAID event for an order that cannot accept it; payment captured but not applied, needs manual reconciliation",
				"order_id", order.ID, "site_id", siteID, "order_status", order.Status,
				"payment_id", paymentID, "amount", pay.Amount.Value+" "+pay.Amount.Currency)
			if err := tx.Commit(); err != nil {
				writeError(w, http.StatusInternalServerError, "commit failed")
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{
				"status":           "payment captured but order is not in a state that can accept it; flagged for reconciliation",
				"new_order_status": order.Status,
			})
			return
		}
	case "failed", "expired", "canceled":
		if canTransition(order.Status, "failed") {
			newStatus = "failed"
		}
	}
	if newStatus != order.Status {
		if err := qtx.UpdateOrderStatus(ctx, store.UpdateOrderStatusParams{
			Status:        newStatus,
			PaymentStatus: pay.Status,
			Column3:       newStatus, Column4: newStatus, Column5: newStatus, Column6: newStatus,
			ID:     order.ID,
			SiteID: siteID,
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "could not update order status")
			return
		}
		if newStatus == "paid" {
			if err := h.applyPaidSideEffects(ctx, qtx, order); err != nil {
				writeError(w, http.StatusInternalServerError, "could not apply paid side effects")
				return
			}
		}
		if newStatus == "failed" {
			// Return the inventory + discount use reserved at checkout.
			if err := h.releaseReservations(ctx, qtx, order); err != nil {
				writeError(w, http.StatusInternalServerError, "could not release reservations")
				return
			}
		}
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "commit failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "new_order_status": newStatus})
}

// applyPaidSideEffects records the confirmed sale. Inventory and the discount
// use are already reserved at checkout (inside the order tx), so this does NOT
// decrement stock or bump used_count again - it only writes the inventory_adjustments
// audit rows for the sale. It takes the caller's tx-bound Queries so the writes
// commit atomically with the status flip + the payment_events claim, and returns
// an error so a failure rolls the whole transaction back. Idempotency is the
// payment_events compare-and-set; this only runs for the single delivery that
// claims the 'paid' event.
func (h *OrderHandler) applyPaidSideEffects(ctx context.Context, q *store.Queries, order store.Order) error {
	items, err := q.ListOrderItems(ctx, order.ID)
	if err != nil {
		return err
	}
	for _, it := range items {
		v, err := q.GetProductVariantByID(ctx, it.VariantID)
		if err != nil {
			// Variant deleted after purchase: skip its audit write.
			continue
		}
		if err := q.CreateInventoryAdjustment(ctx, store.CreateInventoryAdjustmentParams{
			ID:        newID(),
			VariantID: it.VariantID,
			SiteID:    order.SiteID,
			Delta:     -it.Quantity,
			NewCount:  v.InventoryCount, // already reflects the checkout reservation
			Reason:    "sale",
			Note:      "Order " + order.OrderNumber,
			CreatedBy: "system:checkout",
		}); err != nil {
			return err
		}
	}
	return nil
}

// releaseReservations returns the inventory + discount use that checkout reserved
// when a payment ultimately fails/expires/cancels, so an abandoned checkout does
// not permanently hold stock or a max_uses slot. Runs exactly once per order (the
// caller gates it on the pending->failed transition).
func (h *OrderHandler) releaseReservations(ctx context.Context, q *store.Queries, order store.Order) error {
	items, err := q.ListOrderItems(ctx, order.ID)
	if err != nil {
		return err
	}
	for _, it := range items {
		if err := q.AdjustVariantInventoryCount(ctx, store.AdjustVariantInventoryCountParams{
			InventoryCount: it.Quantity, // restock
			ID:             it.VariantID,
		}); err != nil {
			return err
		}
	}
	if order.DiscountCodeID != "" {
		if _, err := q.ReleaseDiscountCodeUse(ctx, store.ReleaseDiscountCodeUseParams{
			ID: order.DiscountCodeID, SiteID: order.SiteID,
		}); err != nil {
			return err
		}
	}
	return nil
}
