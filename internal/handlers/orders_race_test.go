package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/bright-interaction/slab/internal/payments/mollie"
	"github.com/bright-interaction/slab/internal/store"
)

// Regression tests for the 2026-08-11 audit findings 1 (CRITICAL) and 11 (HIGH).
//
// Both are the same root cause: the order is read OUTSIDE the transaction, the
// side-effect decision is taken from that snapshot, and the transaction never
// re-checks it. UpdateOrderStatus carried no from-state predicate, so the write
// landed unconditionally on whatever the row had become.
//
// The seam these tests use to make the race deterministic is the request body.
// UpdateStatus reads the order (orders.go, orderInSite) and only THEN calls
// parseJSON, which is the first thing that touches r.Body. A body that blocks
// on its first Read therefore freezes a caller at exactly the point where it
// holds a stale snapshot and no database connection. No production code knows
// anything about it, and it works under the production pool setting of
// SetMaxOpenConns(1), where a caller that held a connection would deadlock.

// gatedBody blocks the first Read until release is closed, then serves body.
// parked, when non-nil, is closed at the moment the caller blocks, which is the
// moment it is known to be holding a stale order snapshot.
type gatedBody struct {
	release <-chan struct{}
	parked  chan struct{}
	body    io.Reader
	opened  bool
}

func (g *gatedBody) Read(p []byte) (int, error) {
	if !g.opened {
		g.opened = true
		if g.parked != nil {
			close(g.parked)
		}
		<-g.release
	}
	return g.body.Read(p)
}

// hookedDBTX wraps a store.DBTX and fires a callback immediately after a query
// whose SQL contains `match` has been issued, which is how a test lets a
// competing writer loose at a precise point inside a handler.
//
// It signals and returns; it never blocks. Under the production pool setting of
// SetMaxOpenConns(1) the *sql.Row returned here still owns the single
// connection until it is scanned, so a wrapper that blocked would deadlock the
// very writer it is trying to admit. Signalling instead puts that writer at the
// head of the pool's waiter queue, so it acquires the connection the moment the
// hooked read finishes and commits while the hooked caller is still between its
// read and its transaction.
type hookedDBTX struct {
	store.DBTX
	mu    sync.Mutex
	match string
	after func()
}

func (h *hookedDBTX) set(match string, after func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.match, h.after = match, after
}

func (h *hookedDBTX) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	row := h.DBTX.QueryRowContext(ctx, query, args...)
	h.mu.Lock()
	match, after := h.match, h.after
	h.mu.Unlock()
	if after != nil && match != "" && strings.Contains(query, match) {
		after()
	}
	return row
}

// sqlGetOrderByPaymentID is the distinctive fragment of the webhook's
// pre-transaction order read (store.GetOrderByPaymentID).
const sqlGetOrderByPaymentID = "FROM orders WHERE payment_id = ?"

// captureErrorLogs redirects the default slog logger into a buffer for the
// duration of the test so a test can assert that an alarm actually fired.
func captureErrorLogs(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type raceFixture struct {
	db      *sql.DB
	q       *store.Queries
	h       *OrderHandler
	router  chi.Router
	siteID  string
	variant store.ProductVariant
	// payStatus/payValue/payCurrency drive the stubbed Mollie payment response.
	payStatus   string
	payValue    string
	payCurrency string

	// hook is the handler's view of the database, so a test can fire a callback
	// at a chosen query inside a handler.
	hook *hookedDBTX
}

// newRaceFixture builds a site, a stocked variant and a router carrying both the
// operator status route and the Mollie webhook, over the PRODUCTION database
// configuration (setupDeployTestDB uses the cmd/server/main.go DSN and
// SetMaxOpenConns(1)).
func newRaceFixture(t *testing.T, siteID string, inventory int64) *raceFixture {
	t.Helper()
	db, q := setupDeployTestDB(t)
	seedSite(t, q, siteID)
	_, v := seedActiveProductWithVariant(t, q, siteID, "race-mug", inventory)

	if err := q.UpsertSetting(context.Background(), store.UpsertSettingParams{
		SiteID: siteID, Category: "payments", Key: "mollie_api_key", Value: "test_key",
	}); err != nil {
		t.Fatalf("seed mollie key: %v", err)
	}

	f := &raceFixture{db: db, q: q, siteID: siteID, variant: v}
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"%s","status":"%s","amount":{"currency":"%s","value":"%s"}}`,
			strings.TrimPrefix(r.URL.Path, "/payments/"), f.payStatus, f.payCurrency, f.payValue)
	}))
	t.Cleanup(stub.Close)

	// The handler gets a hookable view of the same database. f.q stays
	// unhooked so seeding and assertions never trip a test's hook.
	f.hook = &hookedDBTX{DBTX: db}
	f.h = NewOrderHandler(nil, store.New(f.hook), db)
	f.h.mollieBaseURL = stub.URL
	router := chi.NewRouter()
	router.Post("/api/sites/{siteID}/orders/{orderID}/status", f.h.UpdateStatus)
	router.Post("/webhooks/{siteID}/mollie", f.h.Webhook)
	f.router = router
	return f
}

// mkPendingOrder creates a pending order holding a reservation of qty units.
func (f *raceFixture) mkPendingOrder(t *testing.T, orderID string, qty int64) {
	t.Helper()
	ctx := context.Background()
	total := qty * 1000
	if err := f.q.CreateOrder(ctx, store.CreateOrderParams{
		ID: orderID, SiteID: f.siteID, OrderNumber: orderID, Status: "pending",
		CustomerEmail: "buyer@example.com", SubtotalCents: total, TotalCents: total,
		Currency: "EUR", PaymentProvider: "mollie", PaymentID: "tr_" + orderID, PaymentStatus: "open",
	}); err != nil {
		t.Fatalf("create order %s: %v", orderID, err)
	}
	if err := f.q.CreateOrderItem(ctx, store.CreateOrderItemParams{
		ID: newID(), OrderID: orderID, VariantID: f.variant.ID, ProductID: "p", ProductName: "Mug",
		Quantity: qty, UnitPriceCents: 1000, TotalCents: total,
	}); err != nil {
		t.Fatalf("create order item %s: %v", orderID, err)
	}
}

func (f *raceFixture) cancelRequest(orderID string, body io.Reader) *http.Request {
	req := httptest.NewRequest(http.MethodPost,
		"/api/sites/"+f.siteID+"/orders/"+orderID+"/status", body)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func (f *raceFixture) fireCancel(orderID string, body io.Reader) int {
	rr := httptest.NewRecorder()
	f.router.ServeHTTP(rr, f.cancelRequest(orderID, body))
	return rr.Code
}

func (f *raceFixture) fireWebhook(orderID string) (int, map[string]string) {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/"+f.siteID+"/mollie",
		strings.NewReader("id=tr_"+orderID))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	f.router.ServeHTTP(rr, req)
	var out map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

func (f *raceFixture) inventory(t *testing.T) int64 {
	t.Helper()
	v, err := f.q.GetProductVariantByID(context.Background(), f.variant.ID)
	if err != nil {
		t.Fatalf("read variant: %v", err)
	}
	return v.InventoryCount
}

func (f *raceFixture) orderStatus(t *testing.T, orderID string) string {
	t.Helper()
	o, err := f.q.GetOrderByID(context.Background(), store.GetOrderByIDParams{ID: orderID, SiteID: f.siteID})
	if err != nil {
		t.Fatalf("read order %s: %v", orderID, err)
	}
	return o.Status
}

func (f *raceFixture) saleAdjustments(t *testing.T, orderNumber string) int {
	t.Helper()
	rows, err := f.q.ListInventoryAdjustmentsByVariant(context.Background(),
		store.ListInventoryAdjustmentsByVariantParams{VariantID: f.variant.ID, Limit: 1000})
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

const cancelBody = `{"status":"cancelled"}`

// AUDIT FINDING 11 (HIGH), deterministic.
//
// Two concurrent cancels both read `pending` and both released the reservation,
// so the tenant's stock was inflated by the order quantity on every
// double-clicked cancel and they then oversold. Measured 38/40 rounds by the
// audit. The first caller is frozen mid-request holding the stale `pending`
// snapshot, the second runs to completion, and then the first is released.
func TestOrders_ConcurrentCancelReleasesReservationExactlyOnce(t *testing.T) {
	f := newRaceFixture(t, "ositerace0000000aaa01", 100)
	const qty = 3
	f.mkPendingOrder(t, "ord-double-cancel", qty)

	before := f.inventory(t)

	release := make(chan struct{})
	gated := &gatedBody{release: release, parked: make(chan struct{}), body: strings.NewReader(cancelBody)}

	var firstCode int
	done := make(chan struct{})
	go func() {
		defer close(done)
		firstCode = f.fireCancel("ord-double-cancel", gated)
	}()

	// Wait until the gated caller is past its order read and parked in parseJSON
	// holding a stale `pending`. Without this the second cancel can finish before
	// the first has read anything, and the first is then refused by the ordinary
	// canTransition check rather than by the compare-and-set under test.
	<-gated.parked
	secondCode := f.fireCancel("ord-double-cancel", strings.NewReader(cancelBody))
	if secondCode != http.StatusOK {
		t.Fatalf("uncontended cancel = %d; want 200", secondCode)
	}
	close(release)
	<-done

	if got := f.inventory(t); got != before+qty {
		t.Errorf("inventory = %d; want %d (restocked exactly once from %d)", got, before+qty, before)
	}
	if firstCode == http.StatusOK {
		t.Errorf("the stale-snapshot cancel returned 200; it must be refused once the order left `pending`")
	}
	if firstCode != http.StatusConflict {
		t.Errorf("stale-snapshot cancel = %d; want 409", firstCode)
	}
	if got := f.orderStatus(t, "ord-double-cancel"); got != "cancelled" {
		t.Errorf("order status = %q; want cancelled", got)
	}
}

// AUDIT FINDING 11 (HIGH), 50 rounds under the production DB config.
//
// The deterministic test above pins the exact interleaving. This one hammers the
// real one: two genuinely concurrent cancels per round, no seam, no
// coordination. Inventory must move by exactly the order quantity every round
// and exactly one caller may win.
func TestOrders_ConcurrentCancelRoundsRestockExactlyOnce(t *testing.T) {
	f := newRaceFixture(t, "ositerace0000000aaa02", 10000)
	const rounds = 50
	const qty = 3

	bad := 0
	for i := 0; i < rounds; i++ {
		orderID := fmt.Sprintf("ord-hammer-%03d", i)
		f.mkPendingOrder(t, orderID, qty)
		before := f.inventory(t)

		start := make(chan struct{})
		codes := make([]int, 2)
		var wg sync.WaitGroup
		for j := range codes {
			wg.Add(1)
			go func(j int) {
				defer wg.Done()
				<-start
				codes[j] = f.fireCancel(orderID, strings.NewReader(cancelBody))
			}(j)
		}
		close(start)
		wg.Wait()

		delta := f.inventory(t) - before
		ok := 0
		for _, c := range codes {
			if c == http.StatusOK {
				ok++
			}
		}
		if delta != qty || ok != 1 {
			bad++
			if bad <= 3 {
				t.Errorf("round %d: inventory delta = %d (want %d), winners = %d (want 1), codes = %v",
					i, delta, qty, ok, codes)
			}
		}
	}
	if bad > 0 {
		t.Fatalf("%d/%d rounds double-released inventory or double-won", bad, rounds)
	}
}

// AUDIT FINDING 1 (CRITICAL), deterministic: the reconciliation branch must fire
// and must be LOUD when a captured `paid` payment cannot be applied.
func TestOrders_PaidWebhookOnCancelledOrderFiresReconciliationAlarm(t *testing.T) {
	logs := captureErrorLogs(t)
	f := newRaceFixture(t, "ositerace0000000aaa03", 100)
	const qty = 3
	f.mkPendingOrder(t, "ord-late-paid", qty)
	f.payStatus, f.payCurrency, f.payValue = "paid", "EUR", mollie.FormatAmount(qty*1000)

	if code := f.fireCancel("ord-late-paid", strings.NewReader(cancelBody)); code != http.StatusOK {
		t.Fatalf("cancel = %d; want 200", code)
	}
	afterCancel := f.inventory(t)

	code, out := f.fireWebhook("ord-late-paid")
	if code != http.StatusOK {
		t.Fatalf("webhook = %d; want 200 so Mollie stops retrying", code)
	}
	if !strings.Contains(out["status"], "flagged for reconciliation") {
		t.Errorf("webhook status = %q; want the reconciliation-flagged response", out["status"])
	}
	if out["new_order_status"] != "cancelled" {
		t.Errorf("new_order_status = %q; want cancelled", out["new_order_status"])
	}
	if got := f.orderStatus(t, "ord-late-paid"); got != "cancelled" {
		t.Errorf("order status = %q; a captured payment must not resurrect a cancelled order", got)
	}
	if got := f.inventory(t); got != afterCancel {
		t.Errorf("inventory = %d; want unchanged %d", got, afterCancel)
	}
	if n := f.saleAdjustments(t, "ord-late-paid"); n != 0 {
		t.Errorf("sale adjustment rows = %d; want 0 for a payment that was never applied", n)
	}
	if !strings.Contains(logs(), "needs manual reconciliation") {
		t.Errorf("reconciliation alarm did not fire; captured ERROR logs:\n%s", logs())
	}
}

// AUDIT FINDING 1 (CRITICAL), deterministic.
//
// The audit's exact observed state: an operator cancel racing Mollie's `paid`
// webhook produced order.status="cancelled", inventory restocked to its
// pre-checkout level, an inventory_adjustments row with reason='sale' saying it
// sold, and a webhook that answered 200 {"new_order_status":"paid"} and marked
// the event processed so Mollie never retried. Money moved at Mollie and nothing
// anywhere recorded it.
//
// The cancel is frozen holding a stale `pending` snapshot, the paid webhook runs
// to completion, and the cancel is then released against a row that is now paid.
func TestOrders_CancelWithStaleSnapshotCannotUnsellAPaidOrder(t *testing.T) {
	f := newRaceFixture(t, "ositerace0000000aaa04", 100)
	const qty = 3
	f.mkPendingOrder(t, "ord-paid-race", qty)
	f.payStatus, f.payCurrency, f.payValue = "paid", "EUR", mollie.FormatAmount(qty*1000)

	before := f.inventory(t)

	release := make(chan struct{})
	gated := &gatedBody{release: release, parked: make(chan struct{}), body: strings.NewReader(cancelBody)}
	var cancelCode int
	done := make(chan struct{})
	go func() {
		defer close(done)
		cancelCode = f.fireCancel("ord-paid-race", gated)
	}()

	// Wait until the cancel is parked in parseJSON holding a stale `pending`,
	// then capture the money.
	<-gated.parked
	code, out := f.fireWebhook("ord-paid-race")
	if code != http.StatusOK || out["new_order_status"] != "paid" {
		t.Fatalf("paid webhook = %d %v; want 200 paid", code, out)
	}
	close(release)
	<-done

	if got := f.orderStatus(t, "ord-paid-race"); got != "paid" {
		t.Errorf("order status = %q; want paid. A stale-snapshot cancel unsold an order whose money was captured", got)
	}
	if got := f.inventory(t); got != before {
		t.Errorf("inventory = %d; want unchanged %d. Stock for a SOLD order was restocked and the tenant will oversell", got, before)
	}
	if cancelCode != http.StatusConflict {
		t.Errorf("stale-snapshot cancel = %d; want 409", cancelCode)
	}
	if n := f.saleAdjustments(t, "ord-paid-race"); n != 1 {
		t.Errorf("sale adjustment rows = %d; want exactly 1", n)
	}
}

// AUDIT FINDING 1 (CRITICAL), 40 rounds under the production DB config.
//
// This is the finding's own window. The webhook reads the order and only THEN
// opens its transaction, so the round is staged to drop a cancel into exactly
// that gap: the cancel is parked holding its own `pending` snapshot, and it is
// released the instant the webhook issues its own order read. The cancel then
// commits `cancelled` while the webhook sits on a snapshot that still says
// `pending`.
//
// The invariant is the one the audit found broken. Either the payment is applied
// and the order ends `paid` with its stock still committed to the sale, or it is
// not applied and no sale row exists for it. There is no round in which money is
// captured, the stock is returned, and an inventory_adjustments row says
// reason='sale' anyway.
func TestOrders_PaidWebhookRacingCancelNeverCapturesSilently(t *testing.T) {
	logs := captureErrorLogs(t)
	f := newRaceFixture(t, "ositerace0000000aaa05", 10000)
	const rounds = 40
	const qty = 3
	f.payStatus, f.payCurrency, f.payValue = "paid", "EUR", mollie.FormatAmount(qty*1000)

	applied, notApplied, bad := 0, 0, 0
	for i := 0; i < rounds; i++ {
		orderID := fmt.Sprintf("ord-wh-race-%03d", i)
		f.mkPendingOrder(t, orderID, qty)
		before := f.inventory(t)

		release := make(chan struct{})
		gated := &gatedBody{
			release: release,
			parked:  make(chan struct{}),
			body:    strings.NewReader(cancelBody),
		}
		var once sync.Once
		f.hook.set(sqlGetOrderByPaymentID, func() { once.Do(func() { close(release) }) })

		var wg sync.WaitGroup
		var whOut map[string]string
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.fireCancel(orderID, gated)
		}()
		// The cancel now holds a `pending` snapshot and is parked before its
		// transaction. Start the webhook; its own order read releases the cancel.
		<-gated.parked
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, whOut = f.fireWebhook(orderID)
		}()
		wg.Wait()
		f.hook.set("", nil)

		status := f.orderStatus(t, orderID)
		delta := f.inventory(t) - before
		sales := f.saleAdjustments(t, orderID)
		paidApplied := whOut["status"] == "ok" && whOut["new_order_status"] == "paid"

		if paidApplied {
			applied++
			// Money captured AND applied: the order must be paid and the stock
			// must stay committed to the sale.
			if status != "paid" || delta != 0 || sales != 1 {
				bad++
				if bad <= 3 {
					t.Errorf("round %d: webhook applied paid but status=%q inventory_delta=%d sale_rows=%d; "+
						"want paid/0/1. Money was captured and the stock was returned anyway", i, status, delta, sales)
				}
			}
			continue
		}
		notApplied++
		// Payment not applied: no sale row may claim it sold.
		if sales != 0 {
			bad++
			if bad <= 3 {
				t.Errorf("round %d: webhook did not apply paid (%v) yet wrote %d sale rows; status=%q inventory_delta=%d",
					i, whOut, sales, status, delta)
			}
		}
	}
	t.Logf("rounds=%d paid-applied=%d not-applied=%d error-log-bytes=%d", rounds, applied, notApplied, len(logs()))
	if notApplied == 0 {
		t.Fatalf("no round exercised the cancel-wins interleaving, so finding 1's window was never entered")
	}
	if !strings.Contains(logs(), "needs manual reconciliation") {
		t.Errorf("no round raised the reconciliation alarm, so a captured payment went unrecorded")
	}
	if bad > 0 {
		t.Fatalf("%d/%d rounds captured money without a consistent record", bad, rounds)
	}
}
