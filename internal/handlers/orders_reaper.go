package handlers

// Reaper for orders that reserved stock and never acquired a payment.
//
// Checkout commits the reservation transaction BEFORE calling Mollie
// (orders.go: tx.Commit then client.CreatePayment). That ordering is not
// itself wrong, since reserving after payment would oversell. What was wrong
// is that nothing ever undid it: if CreatePayment fails, or the Mollie API key
// is missing, or no public URL resolves, the order stays `pending` with an
// empty payment_id. No webhook will ever arrive for such an order, and the
// release path only runs from the `failed` webhook, so the stock and the
// discount max_uses slot were consumed permanently by an order nobody paid
// for. A single Mollie outage could zero a shop's inventory.
//
// The reaper closes the loop. It is keyed on payment_id = '' so it only
// touches orders that never reached Mollie at all: an order that DID get a
// payment is legitimately holding its reservation while the customer sits on
// the payment page, and reaping that would be its own bug.

import (
	"context"
	"log/slog"
	"time"

	"github.com/bright-interaction/slab/internal/store"
)

const (
	// UnpaidOrderTTL is how long an order may sit at pending with no payment
	// before its reservation is released. Comfortably longer than the window
	// in which CreatePayment either succeeds or returns, and unrelated to
	// Mollie's own payment expiry because those orders are excluded by the
	// payment_id predicate.
	UnpaidOrderTTL = 60 * time.Minute
	// reapInterval is deliberately coarse. Stock recovery is not urgent, and
	// a tight loop would add contention to the same rows checkout writes.
	reapInterval = 10 * time.Minute
	// reapBatchSize bounds one pass so a large backlog cannot hold a write
	// transaction open long enough to stall checkout.
	reapBatchSize = 200
)

// StartUnpaidOrderReaper runs an immediate pass (to recover anything stranded
// by a previous process) and then one every reapInterval until ctx is done.
func (h *OrderHandler) StartUnpaidOrderReaper(ctx context.Context) {
	go func() {
		if n, err := h.ReapUnpaidOrders(ctx, time.Now().UTC()); err != nil {
			slog.Warn("orders: boot reap of unpaid orders", "err", err)
		} else if n > 0 {
			slog.Info("orders: released reservations for stranded unpaid orders", "count", n)
		}
		t := time.NewTicker(reapInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n, err := h.ReapUnpaidOrders(ctx, time.Now().UTC()); err != nil {
					slog.Warn("orders: reap unpaid orders", "err", err)
				} else if n > 0 {
					slog.Info("orders: released reservations for unpaid orders", "count", n)
				}
			}
		}
	}()
}

// ReapUnpaidOrders flips stale unpaid orders to `failed` and releases their
// reservations. Exported so a test can drive one pass deterministically
// instead of waiting on the ticker. Returns how many orders were reaped.
func (h *OrderHandler) ReapUnpaidOrders(ctx context.Context, now time.Time) (int, error) {
	cutoff := now.Add(-UnpaidOrderTTL).Format("2006-01-02 15:04:05")
	rows, err := h.queries.ListStaleUnpaidOrders(ctx, store.ListStaleUnpaidOrdersParams{
		CreatedAt: cutoff,
		Limit:     reapBatchSize,
	})
	if err != nil {
		return 0, err
	}

	reaped := 0
	for _, row := range rows {
		// One transaction per order. A single order that fails to release
		// must not roll back the ones already recovered in this pass.
		if err := h.reapOne(ctx, row); err != nil {
			slog.Warn("orders: reap order", "order_id", row.ID, "err", err)
			continue
		}
		reaped++
	}
	return reaped, nil
}

func (h *OrderHandler) reapOne(ctx context.Context, row store.ListStaleUnpaidOrdersRow) error {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	qtx := h.queries.WithTx(tx)

	// Re-read inside the transaction and re-check the state. Between the list
	// above and here the customer's payment could have landed, and reaping a
	// paid order would release stock that is genuinely sold.
	o, err := qtx.GetOrderByID(ctx, store.GetOrderByIDParams{ID: row.ID, SiteID: row.SiteID})
	if err != nil {
		return err
	}
	if o.Status != "pending" || o.PaymentID != "" {
		return nil
	}

	if err := qtx.UpdateOrderStatus(ctx, store.UpdateOrderStatusParams{
		Status:        "failed",
		PaymentStatus: o.PaymentStatus,
		Column3:       "failed",
		Column4:       "failed",
		Column5:       "failed",
		Column6:       "failed",
		ID:            o.ID,
		SiteID:        o.SiteID,
	}); err != nil {
		return err
	}
	if err := h.releaseReservations(ctx, qtx, o); err != nil {
		return err
	}
	return tx.Commit()
}
