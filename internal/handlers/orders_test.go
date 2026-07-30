package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/bright-interaction/slab/internal/payments/mollie"
	"github.com/bright-interaction/slab/internal/store"
)

// Sprint 2 slice B (WP/Webflow roadmap): order pipeline tests.

func seedActiveProductWithVariant(t *testing.T, q *store.Queries, siteID, slug string, inventoryCount int64) (store.Product, store.ProductVariant) {
	t.Helper()
	ctx := context.Background()
	ph := NewProductHandler(nil, q)
	p, err := ph.CreateForAgent(ctx, siteID, ProductInput{
		Name: slug, Slug: slug, Status: "active", BasePriceCents: 1000, Currency: "EUR",
	})
	if err != nil {
		t.Fatalf("seed product: %v", err)
	}
	rawV, _ := json.Marshal(VariantInput{
		Name: "default", PriceCents: 1000, InventoryCount: inventoryCount,
	})
	v, err := ph.CreateVariantForAgent(ctx, siteID, p.ID, rawV)
	if err != nil {
		t.Fatalf("seed variant: %v", err)
	}
	return p, v
}

func checkoutRouter(h *OrderHandler) chi.Router {
	r := chi.NewRouter()
	r.Post("/api/sites/{siteID}/checkout", h.Checkout)
	r.Post("/api/sites/{siteID}/orders/{orderID}/status", h.UpdateStatus)
	r.Get("/api/sites/{siteID}/orders", h.List)
	return r
}

func TestOrders_CheckoutHappyPathNoMollie(t *testing.T) {
	db, q := setupDeployTestDB(t)
	siteID := "ositea0000000000aaaa01"
	seedSite(t, q, siteID)
	_, v := seedActiveProductWithVariant(t, q, siteID, "hat", 10)
	h := NewOrderHandler(nil, q, db)
	r := checkoutRouter(h)

	code, body := postJSON(t, r, "/api/sites/"+siteID+"/checkout", map[string]any{
		"items":    []map[string]any{{"variant_id": v.ID, "quantity": 2}},
		"customer": map[string]any{"email": "buyer@example.com", "name": "Buyer"},
	})
	if code != http.StatusCreated {
		t.Fatalf("status = %d", code)
	}
	order := body["order"].(map[string]any)
	if order["status"] != "pending" {
		t.Errorf("status = %v; want pending", order["status"])
	}
	if order["total_cents"].(float64) != 2000 {
		t.Errorf("total = %v; want 2000", order["total_cents"])
	}
	if body["checkout_url"] != "" {
		t.Errorf("checkout_url should be empty (no Mollie configured); got %v", body["checkout_url"])
	}
}

func TestOrders_CheckoutRejectsInactiveProduct(t *testing.T) {
	db, q := setupDeployTestDB(t)
	siteID := "ositea0000000000aaaa02"
	seedSite(t, q, siteID)
	ph := NewProductHandler(nil, q)
	p, _ := ph.CreateForAgent(context.Background(), siteID, ProductInput{
		Name: "Draft", Slug: "draft", Status: "draft", BasePriceCents: 1000,
	})
	rawV, _ := json.Marshal(VariantInput{Name: "x", PriceCents: 1000, InventoryCount: 5})
	v, _ := ph.CreateVariantForAgent(context.Background(), siteID, p.ID, rawV)

	h := NewOrderHandler(nil, q, db)
	r := checkoutRouter(h)
	code, _ := postJSON(t, r, "/api/sites/"+siteID+"/checkout", map[string]any{
		"items":    []map[string]any{{"variant_id": v.ID, "quantity": 1}},
		"customer": map[string]any{"email": "x@example.com"},
	})
	if code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (inactive product)", code)
	}
}

func TestOrders_CheckoutRejectsInsufficientInventory(t *testing.T) {
	db, q := setupDeployTestDB(t)
	siteID := "ositea0000000000aaaa03"
	seedSite(t, q, siteID)
	_, v := seedActiveProductWithVariant(t, q, siteID, "scarce", 2)
	h := NewOrderHandler(nil, q, db)
	r := checkoutRouter(h)
	code, _ := postJSON(t, r, "/api/sites/"+siteID+"/checkout", map[string]any{
		"items":    []map[string]any{{"variant_id": v.ID, "quantity": 5}},
		"customer": map[string]any{"email": "x@example.com"},
	})
	if code != http.StatusConflict {
		t.Errorf("status = %d; want 409 (insufficient inventory)", code)
	}
}

func TestOrders_CheckoutAppliesDiscountCode(t *testing.T) {
	db, q := setupDeployTestDB(t)
	siteID := "ositea0000000000aaaa04"
	seedSite(t, q, siteID)
	_, v := seedActiveProductWithVariant(t, q, siteID, "tshirt", 100)
	dh := NewDiscountCodeHandler(nil, q)
	raw, _ := json.Marshal(map[string]any{
		"code": "PCT10", "kind": "percent", "value": 1000, "is_active": true,
	})
	if _, err := dh.CreateForAgent(context.Background(), siteID, raw); err != nil {
		t.Fatalf("seed code: %v", err)
	}
	h := NewOrderHandler(nil, q, db)
	r := checkoutRouter(h)
	code, body := postJSON(t, r, "/api/sites/"+siteID+"/checkout", map[string]any{
		"items":         []map[string]any{{"variant_id": v.ID, "quantity": 3}},
		"customer":      map[string]any{"email": "x@example.com"},
		"discount_code": "pct10",
	})
	if code != http.StatusCreated {
		t.Fatalf("status = %d", code)
	}
	order := body["order"].(map[string]any)
	if order["subtotal_cents"].(float64) != 3000 {
		t.Errorf("subtotal = %v; want 3000", order["subtotal_cents"])
	}
	if order["discount_cents"].(float64) != 300 {
		t.Errorf("discount = %v; want 300 (10%% of 3000)", order["discount_cents"])
	}
	if order["total_cents"].(float64) != 2700 {
		t.Errorf("total = %v; want 2700", order["total_cents"])
	}
}

func TestOrders_StateMachineRejectsIllegalTransition(t *testing.T) {
	db, q := setupDeployTestDB(t)
	siteID := "ositea0000000000aaaa05"
	seedSite(t, q, siteID)
	_, v := seedActiveProductWithVariant(t, q, siteID, "shirt", 50)
	h := NewOrderHandler(nil, q, db)
	r := checkoutRouter(h)
	code, body := postJSON(t, r, "/api/sites/"+siteID+"/checkout", map[string]any{
		"items":    []map[string]any{{"variant_id": v.ID, "quantity": 1}},
		"customer": map[string]any{"email": "x@example.com"},
	})
	if code != http.StatusCreated {
		t.Fatalf("seed order: status=%d", code)
	}
	orderID := body["order"].(map[string]any)["id"].(string)

	// pending -> fulfilled is illegal (must go through paid first).
	code, _ = postJSON(t, r, "/api/sites/"+siteID+"/orders/"+orderID+"/status", map[string]any{
		"status": "fulfilled",
	})
	if code != http.StatusBadRequest {
		t.Errorf("pending->fulfilled status = %d; want 400", code)
	}
	// pending -> cancelled is legal.
	code, _ = postJSON(t, r, "/api/sites/"+siteID+"/orders/"+orderID+"/status", map[string]any{
		"status": "cancelled",
	})
	if code != http.StatusOK {
		t.Errorf("pending->cancelled status = %d; want 200", code)
	}
	// cancelled is terminal.
	code, _ = postJSON(t, r, "/api/sites/"+siteID+"/orders/"+orderID+"/status", map[string]any{
		"status": "fulfilled",
	})
	if code != http.StatusBadRequest {
		t.Errorf("cancelled->fulfilled status = %d; want 400", code)
	}
}

func TestOrders_PaidSideEffectsDecrementInventoryAndIncrementDiscount(t *testing.T) {
	db, q := setupDeployTestDB(t)
	siteID := "ositea0000000000aaaa06"
	seedSite(t, q, siteID)
	_, v := seedActiveProductWithVariant(t, q, siteID, "thing", 10)
	dh := NewDiscountCodeHandler(nil, q)
	raw, _ := json.Marshal(map[string]any{
		"code": "Z5", "kind": "fixed", "value": 100, "is_active": true,
	})
	dc, err := dh.CreateForAgent(context.Background(), siteID, raw)
	if err != nil {
		t.Fatalf("seed code: %v", err)
	}
	h := NewOrderHandler(nil, q, db)
	r := checkoutRouter(h)
	code, body := postJSON(t, r, "/api/sites/"+siteID+"/checkout", map[string]any{
		"items":         []map[string]any{{"variant_id": v.ID, "quantity": 3}},
		"customer":      map[string]any{"email": "x@example.com"},
		"discount_code": "z5",
	})
	if code != http.StatusCreated {
		t.Fatalf("status=%d", code)
	}
	// Apply side-effects (the webhook handler does this on 'paid').
	orderID := body["order"].(map[string]any)["id"].(string)
	order, _ := q.GetOrderByID(context.Background(), store.GetOrderByIDParams{ID: orderID, SiteID: siteID})
	if err := h.applyPaidSideEffects(context.Background(), q, order); err != nil {
		t.Fatalf("applyPaidSideEffects: %v", err)
	}

	// Inventory decremented.
	v2, _ := q.GetProductVariantByID(context.Background(), v.ID)
	if v2.InventoryCount != 7 {
		t.Errorf("inventory after paid = %d; want 7", v2.InventoryCount)
	}
	adjs, _ := q.ListInventoryAdjustmentsByVariant(context.Background(), store.ListInventoryAdjustmentsByVariantParams{
		VariantID: v.ID, Limit: 5,
	})
	if len(adjs) != 1 || adjs[0].Reason != "sale" || adjs[0].Delta != -3 {
		t.Errorf("expected one sale adjustment delta=-3; got %#v", adjs)
	}
	dc2, _ := q.GetDiscountCodeByID(context.Background(), store.GetDiscountCodeByIDParams{ID: dc.ID, SiteID: siteID})
	if dc2.UsedCount != 1 {
		t.Errorf("discount used_count = %d; want 1", dc2.UsedCount)
	}
}

func TestOrders_PaymentEventIdempotencyByUniqueIndex(t *testing.T) {
	_, q := setupDeployTestDB(t)
	siteID := "ositea0000000000aaaa07"
	seedSite(t, q, siteID)
	ctx := context.Background()

	// Two inserts with the same (provider, payment_id, event_type) should
	// result in exactly one row (INSERT OR IGNORE on the unique index).
	for i := 0; i < 2; i++ {
		_ = q.CreatePaymentEvent(ctx, store.CreatePaymentEventParams{
			ID: newID(), SiteID: siteID, OrderID: "dummy-order",
			Provider: "mollie", PaymentID: "tr_test_abc", EventType: "paid",
			RawJson: "{}",
		})
	}
	row, err := q.GetPaymentEventByLookup(ctx, store.GetPaymentEventByLookupParams{
		Provider: "mollie", PaymentID: "tr_test_abc", EventType: "paid",
	})
	if err != nil {
		t.Fatalf("get event: %v", err)
	}
	if row.Processed != 0 {
		t.Errorf("processed = %d; want 0 before marker", row.Processed)
	}
	// CAS claim: the first mark wins (1 row), a second concurrent/retried
	// mark loses (0 rows). This single-winner guarantee is what stops a
	// retried Mollie 'paid' delivery from double-applying side-effects.
	claimed, err := q.MarkPaymentEventProcessed(ctx, store.MarkPaymentEventProcessedParams{
		Provider: "mollie", PaymentID: "tr_test_abc", EventType: "paid",
	})
	if err != nil || claimed != 1 {
		t.Fatalf("first claim: rows=%d err=%v; want 1", claimed, err)
	}
	claimed2, err := q.MarkPaymentEventProcessed(ctx, store.MarkPaymentEventProcessedParams{
		Provider: "mollie", PaymentID: "tr_test_abc", EventType: "paid",
	})
	if err != nil || claimed2 != 0 {
		t.Fatalf("second claim: rows=%d err=%v; want 0 (already processed)", claimed2, err)
	}
	row, _ = q.GetPaymentEventByLookup(ctx, store.GetPaymentEventByLookupParams{
		Provider: "mollie", PaymentID: "tr_test_abc", EventType: "paid",
	})
	if row.Processed != 1 {
		t.Errorf("processed = %d; want 1", row.Processed)
	}
}

// TestOrders_WebhookPaidIdempotentAndAmountChecked drives the real Mollie
// webhook handler end-to-end against a stub. It is the regression guard
// for two audit fixes: a retried 'paid' delivery must not double-apply
// inventory (CAS claim + tx), and a payment whose amount does not match
// the order total must not mark the order paid.
func TestOrders_WebhookPaidIdempotentAndAmountChecked(t *testing.T) {
	db, q := setupDeployTestDB(t)
	ctx := context.Background()
	siteID := "ositewh00000000aaaa01"
	seedSite(t, q, siteID)
	_, v := seedActiveProductWithVariant(t, q, siteID, "mug", 10)

	if err := q.UpsertSetting(ctx, store.UpsertSettingParams{
		SiteID: siteID, Category: "payments", Key: "mollie_api_key", Value: "test_key",
	}); err != nil {
		t.Fatalf("seed mollie key: %v", err)
	}

	mkOrder := func(orderID, paymentID string, total int64) {
		if err := q.CreateOrder(ctx, store.CreateOrderParams{
			ID: orderID, SiteID: siteID, OrderNumber: orderID, Status: "pending",
			CustomerEmail: "b@e.com", SubtotalCents: total, TotalCents: total,
			Currency: "EUR", PaymentProvider: "mollie", PaymentID: paymentID, PaymentStatus: "open",
		}); err != nil {
			t.Fatalf("create order: %v", err)
		}
		if err := q.CreateOrderItem(ctx, store.CreateOrderItemParams{
			ID: newID(), OrderID: orderID, VariantID: v.ID, ProductID: "p", ProductName: "Mug",
			Quantity: 3, UnitPriceCents: total / 3, TotalCents: total,
		}); err != nil {
			t.Fatalf("create order item: %v", err)
		}
	}

	var payStatus, payValue, payCurrency string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"%s","status":"%s","amount":{"currency":"%s","value":"%s"}}`,
			strings.TrimPrefix(r.URL.Path, "/payments/"), payStatus, payCurrency, payValue)
	}))
	defer stub.Close()

	h := NewOrderHandler(nil, q, db)
	h.mollieBaseURL = stub.URL
	router := chi.NewRouter()
	router.Post("/webhooks/{siteID}/mollie", h.Webhook)

	fire := func(paymentID string) (int, map[string]string) {
		req := httptest.NewRequest(http.MethodPost, "/webhooks/"+siteID+"/mollie", strings.NewReader("id="+paymentID))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		var out map[string]string
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		return rr.Code, out
	}

	// Inventory is now reserved at CHECKOUT (inside the order tx), not on the paid
	// webhook, to close the oversell window. These orders are created directly
	// (bypassing checkout), so the webhook leaves inventory_count at the seeded 10;
	// the webhook's paid side-effect is now the inventory_adjustments audit row.
	// Idempotency is asserted via the "already processed" duplicate-delivery check.
	mkOrder("ord-paid-1", "tr_paid_1", 6000)
	payStatus, payCurrency, payValue = "paid", "EUR", mollie.FormatAmount(6000)
	if code, out := fire("tr_paid_1"); code != http.StatusOK || out["new_order_status"] != "paid" {
		t.Fatalf("first paid delivery: code=%d out=%v; want 200 paid", code, out)
	}
	if v1, _ := q.GetProductVariantByID(ctx, v.ID); v1.InventoryCount != 10 {
		t.Fatalf("inventory after first paid = %d; want 10 (reserved at checkout, not the webhook)", v1.InventoryCount)
	}
	// Retried duplicate delivery: no second side-effect.
	if code, out := fire("tr_paid_1"); code != http.StatusOK || out["status"] != "already processed" {
		t.Fatalf("duplicate delivery: code=%d out=%v; want 'already processed'", code, out)
	}
	if v2, _ := q.GetProductVariantByID(ctx, v.ID); v2.InventoryCount != 10 {
		t.Fatalf("inventory after duplicate = %d; want still 10", v2.InventoryCount)
	}

	// Wrong amount: order must NOT be marked paid.
	mkOrder("ord-bad-amt", "tr_bad_amt", 6000)
	payStatus, payCurrency, payValue = "paid", "EUR", "1.00"
	if code, out := fire("tr_bad_amt"); code != http.StatusOK || out["status"] != "amount mismatch" {
		t.Fatalf("amount mismatch delivery: code=%d out=%v; want 'amount mismatch'", code, out)
	}
	if ord, _ := q.GetOrderByPaymentID(ctx, "tr_bad_amt"); ord.Status != "pending" {
		t.Errorf("order with wrong paid amount = %q; want still pending", ord.Status)
	}
	if v3, _ := q.GetProductVariantByID(ctx, v.ID); v3.InventoryCount != 10 {
		t.Errorf("inventory after mismatch = %d; want unchanged 10", v3.InventoryCount)
	}
}

// The CRITICAL this reaper exists for. Checkout commits the reservation before
// creating the Mollie payment, so when payment creation never happens (Mollie
// down, no API key, no public URL) the order sits at pending with no
// payment_id, no webhook ever arrives, and the release path is unreachable.
// The stock and the discount use were consumed permanently by an order nobody
// paid for. This test walks a real checkout, ages it, and asserts recovery.
func TestOrders_ReaperReleasesStrandedUnpaidReservations(t *testing.T) {
	db, q := setupDeployTestDB(t)
	siteID := "ositea0000000000reap01"
	seedSite(t, q, siteID)
	_, v := seedActiveProductWithVariant(t, q, siteID, "hat", 1)
	h := NewOrderHandler(nil, q, db)
	r := checkoutRouter(h)

	// A real checkout. No Mollie is configured in this harness, which is
	// exactly the stranding condition: the order gets no payment_id.
	code, body := postJSON(t, r, "/api/sites/"+siteID+"/checkout", map[string]any{
		"items":    []map[string]any{{"variant_id": v.ID, "quantity": 1}},
		"customer": map[string]any{"email": "buyer@example.com", "name": "Buyer"},
	})
	if code != http.StatusCreated {
		t.Fatalf("checkout status = %d", code)
	}
	orderID := body["order"].(map[string]any)["id"].(string)

	after, err := q.GetProductVariantByID(t.Context(), v.ID)
	if err != nil {
		t.Fatalf("read variant: %v", err)
	}
	if after.InventoryCount != 0 {
		t.Fatalf("precondition: inventory = %d, want 0 (the reservation should be held)", after.InventoryCount)
	}

	// Not yet stale: reaping here would rob a customer still on the payment page.
	if n, err := h.ReapUnpaidOrders(t.Context(), time.Now().UTC()); err != nil || n != 0 {
		t.Fatalf("fresh order was reaped (n=%d, err=%v); a customer mid-payment would lose their cart", n, err)
	}

	// Past the TTL.
	n, err := h.ReapUnpaidOrders(t.Context(), time.Now().UTC().Add(UnpaidOrderTTL+time.Minute))
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaped %d orders, want 1", n)
	}

	restocked, err := q.GetProductVariantByID(t.Context(), v.ID)
	if err != nil {
		t.Fatalf("re-read variant: %v", err)
	}
	if restocked.InventoryCount != 1 {
		t.Errorf("inventory = %d, want 1: the stranded reservation was never released, so the last unit is unsellable forever", restocked.InventoryCount)
	}
	o, err := q.GetOrderByID(t.Context(), store.GetOrderByIDParams{ID: orderID, SiteID: siteID})
	if err != nil {
		t.Fatalf("re-read order: %v", err)
	}
	if o.Status != "failed" {
		t.Errorf("order status = %q, want failed", o.Status)
	}

	// Idempotent: a second pass must not restock again and invent stock.
	if n, err := h.ReapUnpaidOrders(t.Context(), time.Now().UTC().Add(UnpaidOrderTTL+time.Minute)); err != nil || n != 0 {
		t.Fatalf("second pass reaped %d (err=%v), want 0", n, err)
	}
	again, _ := q.GetProductVariantByID(t.Context(), v.ID)
	if again.InventoryCount != 1 {
		t.Errorf("inventory = %d after a second reap pass, want 1: double release invents stock", again.InventoryCount)
	}
}

// pending -> cancelled is a legal operator transition that used to write the
// status and nothing else, leaving the stock decremented with no
// inventory_adjustments row to explain it.
func TestOrders_CancellingPendingOrderReleasesReservation(t *testing.T) {
	db, q := setupDeployTestDB(t)
	siteID := "ositea0000000000canc01"
	seedSite(t, q, siteID)
	_, v := seedActiveProductWithVariant(t, q, siteID, "hat", 1)
	h := NewOrderHandler(nil, q, db)
	r := checkoutRouter(h)
	r.Patch("/api/sites/{siteID}/orders/{orderID}/status", h.UpdateStatus)

	code, body := postJSON(t, r, "/api/sites/"+siteID+"/checkout", map[string]any{
		"items":    []map[string]any{{"variant_id": v.ID, "quantity": 1}},
		"customer": map[string]any{"email": "buyer@example.com", "name": "Buyer"},
	})
	if code != http.StatusCreated {
		t.Fatalf("checkout status = %d", code)
	}
	orderID := body["order"].(map[string]any)["id"].(string)

	req := httptest.NewRequest(http.MethodPatch,
		"/api/sites/"+siteID+"/orders/"+orderID+"/status",
		strings.NewReader(`{"status":"cancelled"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, body = %s", rec.Code, rec.Body.String())
	}

	after, err := q.GetProductVariantByID(t.Context(), v.ID)
	if err != nil {
		t.Fatalf("read variant: %v", err)
	}
	if after.InventoryCount != 1 {
		t.Errorf("inventory = %d after cancelling an unpaid order, want 1: the last unit is unsellable and no adjustment row explains it", after.InventoryCount)
	}
}

// From the edge-case lens, which PROVED this on the pre-fix tree:
// "NEGATIVE LINE ACCEPTED: subtotal_cents=-4000 total_cents=0". Update rejected
// base_price_cents < 0 and create did not, so a negative line drove the cart
// total down and the buyer got free goods. Both create paths now share one
// validator so the guard cannot land on one twin again.
func TestProducts_CreateRejectsNegativeMoneyOnBothPaths(t *testing.T) {
	_, q := setupDeployTestDB(t)
	siteID := "ositea0000000000negpx1"
	seedSite(t, q, siteID)
	ph := NewProductHandler(nil, q)

	if _, err := ph.CreateForAgent(t.Context(), siteID, ProductInput{
		Name: "Bad", Slug: "badhat", Status: "active", BasePriceCents: -5000, Currency: "EUR",
	}); err == nil {
		t.Error("agent create accepted base_price_cents=-5000")
	}
	if _, err := ph.CreateForAgent(t.Context(), siteID, ProductInput{
		Name: "Bad2", Slug: "badhat2", Status: "active", BasePriceCents: 100, WeightGrams: -1, Currency: "EUR",
	}); err == nil {
		t.Error("agent create accepted weight_grams=-1")
	}
	// A legitimate product must still be creatable.
	if _, err := ph.CreateForAgent(t.Context(), siteID, ProductInput{
		Name: "Good", Slug: "goodhat", Status: "active", BasePriceCents: 1000, Currency: "EUR",
	}); err != nil {
		t.Errorf("a valid product was rejected: %v", err)
	}
}

// The lens proved a long-expired 100%-off code redeemable because the window
// check was `err == nil && after`, so an unparseable value SKIPPED the check.
// The two values that trip it are what plain HTML inputs submit.
func TestDiscount_MalformedWindowIsRefusedAtWriteAndFailsClosedAtRedemption(t *testing.T) {
	_, q := setupDeployTestDB(t)
	siteID := "ositea0000000000dcwin1"
	seedSite(t, q, siteID)
	dh := NewDiscountCodeHandler(nil, q)

	for _, ends := range []string{"2020-01-01T00:00", "2020-01-01", "not-a-date", "31/01/2026"} {
		raw, _ := json.Marshal(map[string]any{
			"code": "EXPIRED", "kind": "percent", "value": 10000,
			"is_active": true, "ends_at": ends,
		})
		if _, err := dh.CreateForAgent(t.Context(), siteID, raw); err == nil {
			t.Errorf("ends_at=%q was accepted at write; a stored unparseable window is unusable", ends)
		}
	}
	// RFC3339 must still work, or no operator can set an expiry.
	raw, _ := json.Marshal(map[string]any{
		"code": "GOOD10", "kind": "percent", "value": 1000,
		"is_active": true, "ends_at": "2030-01-31T23:59:59Z",
	})
	if _, err := dh.CreateForAgent(t.Context(), siteID, raw); err != nil {
		t.Errorf("a valid RFC3339 ends_at was rejected: %v", err)
	}
}

// canTransition returned `from == to`, so refunded -> refunded passed the refund
// gate and Mollie's own remaining-balance check was the only guard against a
// second full payout.
func TestOrders_SelfTransitionFromTerminalStateIsRefused(t *testing.T) {
	for _, st := range []string{"refunded", "cancelled", "fulfilled", "failed"} {
		if canTransition(st, st) {
			t.Errorf("canTransition(%q, %q) = true; a terminal self-transition passes every gate that asks", st, st)
		}
	}
	// Real edges must still work.
	if !canTransition("pending", "paid") {
		t.Error("canTransition(pending, paid) = false; a legitimate edge was broken")
	}
	if !canTransition("paid", "refunded") {
		t.Error("canTransition(paid, refunded) = false; a legitimate edge was broken")
	}
}

// The release hook was added to the REST UpdateStatus and MISSED on the agent
// twin, which is the incomplete-migration pattern inside the fix for it.
func TestOrders_AgentCancelAlsoReleasesReservation(t *testing.T) {
	db, q := setupDeployTestDB(t)
	siteID := "ositea0000000000agcan1"
	seedSite(t, q, siteID)
	_, v := seedActiveProductWithVariant(t, q, siteID, "agenthat", 10)
	h := NewOrderHandler(nil, q, db)

	_, body := postJSON(t, checkoutRouter(h), "/api/sites/"+siteID+"/checkout", map[string]any{
		"items":    []map[string]any{{"variant_id": v.ID, "quantity": 4}},
		"customer": map[string]any{"email": "buyer@example.com"},
	})
	orderID := body["order"].(map[string]any)["id"].(string)

	if _, err := h.UpdateStatusForAgent(t.Context(), siteID, orderID, "cancelled", ""); err != nil {
		t.Fatalf("agent cancel: %v", err)
	}
	final, err := q.GetProductVariantByID(t.Context(), v.ID)
	if err != nil {
		t.Fatalf("read variant: %v", err)
	}
	if final.InventoryCount != 10 {
		t.Errorf("inventory_count = %d after an AGENT cancel of a pending order, want 10", final.InventoryCount)
	}
}
