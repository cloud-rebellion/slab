package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/bright-interaction/slab/internal/config"
	"github.com/bright-interaction/slab/internal/payments/mollie"
	"github.com/bright-interaction/slab/internal/store"
)

// End-to-end happy paths for the two money flows.
//
// These exist because the compare-and-set sweep put a from-state predicate on
// every write to orders.status, including the refund and checkout bookkeeping
// writes. A predicate that is subtly wrong does not fail loudly; it silently
// writes nothing, and the symptom shows up later as an order stuck in the
// wrong state. So both flows are driven through the real handlers against a
// stubbed Mollie, and the resulting rows are asserted.
//
// They also close the gap that made this possible: the Mollie base-URL override
// was applied in the webhook only, so checkout and both refund paths could not
// be pointed at a stub and had no end-to-end coverage at all.

// mollieStub serves the three Mollie endpoints these flows touch. Counters are
// mutex-guarded because the double-payout test drives it from two goroutines,
// and a counter that races is exactly the counter you cannot trust.
type mollieStub struct {
	mu        sync.Mutex
	payStatus string
	payValue  string
	payCur    string
	refundID  string

	createdPayments int
	createdRefunds  int
}

func (m *mollieStub) refunds() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.createdRefunds
}

func (m *mollieStub) payments() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.createdPayments
}

func (m *mollieStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		payStatus, payCur, payValue, refundID := m.payStatus, m.payCur, m.payValue, m.refundID
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/refunds") && r.Method == http.MethodPost:
			m.createdRefunds++
		case path == "/payments" && r.Method == http.MethodPost:
			m.createdPayments++
		}
		m.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(path, "/refunds") && r.Method == http.MethodPost:
			_, _ = fmt.Fprintf(w, `{"id":"%s","status":"refunded","amount":{"currency":"%s","value":"%s"}}`,
				refundID, payCur, payValue)
		case path == "/payments" && r.Method == http.MethodPost:
			_, _ = fmt.Fprintf(w, `{"id":"tr_created_1","status":"%s","amount":{"currency":"%s","value":"%s"},`+
				`"_links":{"checkout":{"href":"https://pay.example.test/tr_created_1","type":"text/html"}}}`,
				payStatus, payCur, payValue)
		default: // GET /payments/{id}
			_, _ = fmt.Fprintf(w, `{"id":"%s","status":"%s","amount":{"currency":"%s","value":"%s"}}`,
				strings.TrimPrefix(path, "/payments/"), payStatus, payCur, payValue)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// TestOrders_CheckoutThroughPaidWebhook_HappyPath walks a real storefront
// checkout that reaches Mollie, then the paid webhook, and asserts the order
// lands paid with its stock committed and the sale recorded.
func TestOrders_CheckoutThroughPaidWebhook_HappyPath(t *testing.T) {
	db, q := setupDeployTestDB(t)
	ctx := context.Background()
	siteID := "ohappy00000000000aaa01"
	seedSite(t, q, siteID)
	_, v := seedActiveProductWithVariant(t, q, siteID, "kettle", 10)

	if err := q.UpsertSetting(ctx, store.UpsertSettingParams{
		SiteID: siteID, Category: "payments", Key: "mollie_api_key", Value: "test_key",
	}); err != nil {
		t.Fatalf("seed mollie key: %v", err)
	}

	stub := &mollieStub{payStatus: "open", payCur: "EUR", payValue: mollie.FormatAmount(2000)}
	srv := stub.server(t)

	// A resolvable public URL is what makes checkout actually call Mollie.
	h := NewOrderHandler(&config.Config{BaseURL: "https://shop.example.test"}, q, db)
	h.mollieBaseURL = srv.URL
	router := chi.NewRouter()
	router.Post("/api/sites/{siteID}/checkout", h.Checkout)
	router.Post("/webhooks/{siteID}/mollie", h.Webhook)

	code, body := postJSON(t, router, "/api/sites/"+siteID+"/checkout", map[string]any{
		"items":    []map[string]any{{"variant_id": v.ID, "quantity": 2}},
		"customer": map[string]any{"email": "buyer@example.com", "name": "Buyer"},
	})
	if code != http.StatusCreated {
		t.Fatalf("checkout = %d; want 201. body=%v", code, body)
	}
	if stub.payments() != 1 {
		t.Fatalf("Mollie CreatePayment calls = %d; want 1 (the stub was not reached)", stub.payments())
	}
	if body["checkout_url"] != "https://pay.example.test/tr_created_1" {
		t.Errorf("checkout_url = %v; want the stub's checkout link", body["checkout_url"])
	}

	order := body["order"].(map[string]any)
	orderID := order["id"].(string)

	// Checkout reserves stock inside the order transaction.
	if got := variantStock(t, q, v.ID); got != 8 {
		t.Errorf("inventory after checkout = %d; want 8 (10 less the reserved 2)", got)
	}
	// The bookkeeping write now carries a from-state; prove it still landed.
	o, err := q.GetOrderByID(ctx, store.GetOrderByIDParams{ID: orderID, SiteID: siteID})
	if err != nil {
		t.Fatalf("read order: %v", err)
	}
	if o.Status != "pending" {
		t.Errorf("status after checkout = %q; want pending", o.Status)
	}
	if o.PaymentStatus != "open" {
		t.Errorf("payment_status after checkout = %q; want open. The guarded bookkeeping write did not land", o.PaymentStatus)
	}
	if o.PaymentID != "tr_created_1" {
		t.Errorf("payment_id = %q; want tr_created_1", o.PaymentID)
	}

	// Now Mollie confirms the payment.
	stub.payStatus = "paid"
	req := httptest.NewRequest(http.MethodPost, "/webhooks/"+siteID+"/mollie",
		strings.NewReader("id=tr_created_1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	var out map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if rr.Code != http.StatusOK || out["new_order_status"] != "paid" {
		t.Fatalf("paid webhook = %d %v; want 200 paid", rr.Code, out)
	}

	o, _ = q.GetOrderByID(ctx, store.GetOrderByIDParams{ID: orderID, SiteID: siteID})
	if o.Status != "paid" {
		t.Errorf("status after webhook = %q; want paid", o.Status)
	}
	if o.PaidAt == "" {
		t.Errorf("paid_at is empty; the CASE column did not stamp")
	}
	if o.PaymentStatus != "paid" {
		t.Errorf("payment_status = %q; want paid", o.PaymentStatus)
	}
	if got := variantStock(t, q, v.ID); got != 8 {
		t.Errorf("inventory after paid = %d; want 8 (reserved at checkout, not decremented again)", got)
	}
	if n := saleRowsForOrder(t, q, v.ID, o.OrderNumber); n != 1 {
		t.Errorf("sale adjustment rows = %d; want exactly 1", n)
	}
}

// TestOrders_FullRefund_HappyPath drives the real Refund handler end to end.
// Both of its status writes now carry a from-state, and both are self
// transitions off the claim, which is exactly the shape most likely to be
// written wrong and fail silently.
func TestOrders_FullRefund_HappyPath(t *testing.T) {
	db, q := setupDeployTestDB(t)
	ctx := context.Background()
	siteID := "ohappy00000000000aaa02"
	seedSite(t, q, siteID)
	_, v := seedActiveProductWithVariant(t, q, siteID, "lamp", 10)

	if err := q.UpsertSetting(ctx, store.UpsertSettingParams{
		SiteID: siteID, Category: "payments", Key: "mollie_api_key", Value: "test_key",
	}); err != nil {
		t.Fatalf("seed mollie key: %v", err)
	}

	orderID := "ord-refund-happy"
	if err := q.CreateOrder(ctx, store.CreateOrderParams{
		ID: orderID, SiteID: siteID, OrderNumber: orderID, Status: "paid",
		CustomerEmail: "buyer@example.com", SubtotalCents: 2000, TotalCents: 2000,
		Currency: "EUR", PaymentProvider: "mollie", PaymentID: "tr_refund_1", PaymentStatus: "paid",
	}); err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := q.CreateOrderItem(ctx, store.CreateOrderItemParams{
		ID: newID(), OrderID: orderID, VariantID: v.ID, ProductID: "p", ProductName: "Lamp",
		Quantity: 2, UnitPriceCents: 1000, TotalCents: 2000,
	}); err != nil {
		t.Fatalf("create order item: %v", err)
	}

	stub := &mollieStub{payStatus: "paid", payCur: "EUR", payValue: mollie.FormatAmount(2000), refundID: "re_happy_1"}
	srv := stub.server(t)
	h := NewOrderHandler(nil, q, db)
	h.mollieBaseURL = srv.URL
	router := chi.NewRouter()
	router.Post("/api/sites/{siteID}/orders/{orderID}/refund", h.Refund)

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodPost,
		"/api/sites/"+siteID+"/orders/"+orderID+"/refund", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("refund = %d; want 200. body=%s", rr.Code, rr.Body.String())
	}
	if stub.refunds() != 1 {
		t.Fatalf("Mollie CreateRefund calls = %d; want exactly 1", stub.refunds())
	}

	var payload map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &payload)
	if w, ok := payload["reconcile_warnings"]; ok {
		t.Errorf("happy-path refund returned reconcile_warnings %v; every local write should have landed", w)
	}

	o, err := q.GetOrderByID(ctx, store.GetOrderByIDParams{ID: orderID, SiteID: siteID})
	if err != nil {
		t.Fatalf("read order: %v", err)
	}
	if o.Status != "refunded" {
		t.Errorf("status = %q; want refunded", o.Status)
	}
	if o.RefundID != "re_happy_1" {
		t.Errorf("refund_id = %q; want re_happy_1", o.RefundID)
	}
	if o.RefundedAt == "" {
		t.Errorf("refunded_at is empty; the guarded stamping write did not land")
	}

	// A second refund must not reach Mollie again.
	rr2 := httptest.NewRecorder()
	router.ServeHTTP(rr2, httptest.NewRequest(http.MethodPost,
		"/api/sites/"+siteID+"/orders/"+orderID+"/refund", nil))
	if rr2.Code == http.StatusOK {
		t.Errorf("second refund = 200; a refunded order must not be refundable again")
	}
	if stub.refunds() != 1 {
		t.Errorf("Mollie CreateRefund calls after double refund = %d; want still 1 (double payout)", stub.refunds())
	}
}

// TestOrders_ConcurrentAgentRefundsPayOutOnce is the double-payout proof.
//
// RefundForAgent was check-then-act across a network call: read the order,
// canTransition on that snapshot, then CreateRefund at Mollie. Nothing wrote the
// status until AFTER the network call, so two overlapping agent refunds did not
// merely risk a duplicate payout, they guaranteed one. The REST twin was
// hardened with ClaimOrderForRefund; this path was missed, which is the
// incomplete-migration pattern landing on real customer money.
//
// The assertion is on the stub's request counter, so it counts payouts at the
// provider rather than rows in our database. Two refunds for the same order
// reaching Mollie is the actual harm; what we persisted locally is not.
func TestOrders_ConcurrentAgentRefundsPayOutOnce(t *testing.T) {
	db, q := setupDeployTestDB(t)
	ctx := context.Background()
	siteID := "orefund000000000aaa01"
	seedSite(t, q, siteID)
	_, v := seedActiveProductWithVariant(t, q, siteID, "chair", 10)

	if err := q.UpsertSetting(ctx, store.UpsertSettingParams{
		SiteID: siteID, Category: "payments", Key: "mollie_api_key", Value: "test_key",
	}); err != nil {
		t.Fatalf("seed mollie key: %v", err)
	}

	orderID := "ord-agent-refund-race"
	if err := q.CreateOrder(ctx, store.CreateOrderParams{
		ID: orderID, SiteID: siteID, OrderNumber: orderID, Status: "paid",
		CustomerEmail: "buyer@example.com", SubtotalCents: 2000, TotalCents: 2000,
		Currency: "EUR", PaymentProvider: "mollie", PaymentID: "tr_agent_race", PaymentStatus: "paid",
	}); err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := q.CreateOrderItem(ctx, store.CreateOrderItemParams{
		ID: newID(), OrderID: orderID, VariantID: v.ID, ProductID: "p", ProductName: "Chair",
		Quantity: 2, UnitPriceCents: 1000, TotalCents: 2000,
	}); err != nil {
		t.Fatalf("create order item: %v", err)
	}

	stub := &mollieStub{payStatus: "paid", payCur: "EUR", payValue: mollie.FormatAmount(2000), refundID: "re_race_1"}
	srv := stub.server(t)
	h := NewOrderHandler(nil, q, db)
	h.mollieBaseURL = srv.URL

	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = h.RefundForAgent(ctx, siteID, orderID)
		}(i)
	}
	close(start)
	wg.Wait()

	if got := stub.refunds(); got != 1 {
		t.Errorf("Mollie CreateRefund calls = %d; want exactly 1. "+
			"A second payout for the same order left the account for a real customer", got)
	}

	// The loser may read "refunded" before reaching the atomic claim.
	// Both refusals are valid; every other error remains unexpected.
	won, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrRefundAlreadyClaimed) || err.Error() == `cannot refund order in status "refunded"`:
			refused++
		default:
			t.Errorf("unexpected refund error: %v", err)
		}
	}
	if won != 1 || refused != 1 {
		t.Errorf("outcomes: %d succeeded, %d refused; want exactly 1 and 1. errs=%v", won, refused, errs)
	}

	o, err := q.GetOrderByID(ctx, store.GetOrderByIDParams{ID: orderID, SiteID: siteID})
	if err != nil {
		t.Fatalf("read order: %v", err)
	}
	if o.Status != "refunded" {
		t.Errorf("status = %q; want refunded", o.Status)
	}
	if o.RefundID != "re_race_1" {
		t.Errorf("refund_id = %q; want re_race_1 persisted by the winner", o.RefundID)
	}
	if o.RefundedAt == "" {
		t.Errorf("refunded_at is empty; the winner did not stamp it")
	}
}

// TestOrders_AgentRefundRefusalIsActionable pins the refusal contract the MCP
// tool relies on. An agent holding a money tool needs to distinguish "this did
// not happen and retrying cannot help" from a transient failure, so the sentinel
// is part of the surface, not an implementation detail.
func TestOrders_AgentRefundRefusalIsActionable(t *testing.T) {
	db, q := setupDeployTestDB(t)
	ctx := context.Background()
	siteID := "orefund000000000aaa02"
	seedSite(t, q, siteID)

	if err := q.UpsertSetting(ctx, store.UpsertSettingParams{
		SiteID: siteID, Category: "payments", Key: "mollie_api_key", Value: "test_key",
	}); err != nil {
		t.Fatalf("seed mollie key: %v", err)
	}

	orderID := "ord-agent-refund-twice"
	if err := q.CreateOrder(ctx, store.CreateOrderParams{
		ID: orderID, SiteID: siteID, OrderNumber: orderID, Status: "paid",
		CustomerEmail: "buyer@example.com", SubtotalCents: 2000, TotalCents: 2000,
		Currency: "EUR", PaymentProvider: "mollie", PaymentID: "tr_agent_twice", PaymentStatus: "paid",
	}); err != nil {
		t.Fatalf("create order: %v", err)
	}

	stub := &mollieStub{payStatus: "paid", payCur: "EUR", payValue: mollie.FormatAmount(2000), refundID: "re_twice_1"}
	srv := stub.server(t)
	h := NewOrderHandler(nil, q, db)
	h.mollieBaseURL = srv.URL

	if _, err := h.RefundForAgent(ctx, siteID, orderID); err != nil {
		t.Fatalf("first refund: %v", err)
	}
	_, err := h.RefundForAgent(ctx, siteID, orderID)
	if err == nil {
		t.Fatal("second refund succeeded; a refunded order must not be refundable again")
	}
	if stub.refunds() != 1 {
		t.Errorf("Mollie CreateRefund calls = %d; want 1", stub.refunds())
	}
	// The second call is refused by canTransition before the claim is reached,
	// which is the friendlier error of the two. Either refusal is acceptable,
	// but it must be a refusal and it must not have moved money.
	if errors.Is(err, ErrRefundAlreadyClaimed) || strings.Contains(err.Error(), "cannot refund order in status") {
		return
	}
	t.Errorf("second refund error = %q; want a clear refusal naming the state or the claim", err)
}

func variantStock(t *testing.T, q *store.Queries, variantID string) int64 {
	t.Helper()
	v, err := q.GetProductVariantByID(context.Background(), variantID)
	if err != nil {
		t.Fatalf("read variant: %v", err)
	}
	return v.InventoryCount
}

func saleRowsForOrder(t *testing.T, q *store.Queries, variantID, orderNumber string) int {
	t.Helper()
	rows, err := q.ListInventoryAdjustmentsByVariant(context.Background(),
		store.ListInventoryAdjustmentsByVariantParams{VariantID: variantID, Limit: 1000})
	if err != nil {
		t.Fatalf("list adjustments: %v", err)
	}
	n := 0
	for _, r := range rows {
		if r.Reason == "sale" && strings.Contains(r.Note, orderNumber) {
			n++
		}
	}
	return n
}
