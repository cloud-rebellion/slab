// Package handlers: OrderHandler MCP-shaped helpers. Mirrors the
// REST UpdateStatus + Refund handlers but as context-only methods so
// the MCP tools can share the same validation + state machine.
//
// Sprint 2 slice B of the WP/Webflow replacement roadmap.

package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bright-interaction/slab/internal/payments/mollie"
	"github.com/bright-interaction/slab/internal/store"
)

// UpdateStatusForAgent applies a state-machine flip for the MCP path.
func (h *OrderHandler) UpdateStatusForAgent(ctx context.Context, siteID, orderID, status, notes string) (map[string]any, error) {
	next := strings.ToLower(strings.TrimSpace(status))
	if !validOrderStatus(next) {
		return nil, errors.New("invalid status")
	}
	o, err := h.queries.GetOrderByID(ctx, store.GetOrderByIDParams{ID: orderID, SiteID: siteID})
	if err != nil {
		return nil, errors.New("order not found")
	}
	if !canTransition(o.Status, next) {
		return nil, fmt.Errorf("cannot transition from %q to %q", o.Status, next)
	}
	if err := h.queries.UpdateOrderStatus(ctx, store.UpdateOrderStatusParams{
		Status:        next,
		PaymentStatus: o.PaymentStatus,
		Column3:       next, Column4: next, Column5: next, Column6: next,
		ID:     orderID,
		SiteID: siteID,
	}); err != nil {
		return nil, err
	}
	if strings.TrimSpace(notes) != "" {
		_ = h.queries.UpdateOrderNotes(ctx, store.UpdateOrderNotesParams{
			Notes: notes, ID: orderID, SiteID: siteID,
		})
	}
	updated, _ := h.queries.GetOrderByID(ctx, store.GetOrderByIDParams{ID: orderID, SiteID: siteID})
	items, _ := h.queries.ListOrderItems(ctx, orderID)
	return map[string]any{"order": updated, "items": items}, nil
}

// RefundForAgent triggers the same Mollie refund the REST handler
// does, returning the updated order.
func (h *OrderHandler) RefundForAgent(ctx context.Context, siteID, orderID string) (map[string]any, error) {
	o, err := h.queries.GetOrderByID(ctx, store.GetOrderByIDParams{ID: orderID, SiteID: siteID})
	if err != nil {
		return nil, errors.New("order not found")
	}
	if !canTransition(o.Status, "refunded") {
		return nil, fmt.Errorf("cannot refund order in status %q", o.Status)
	}
	if o.PaymentID == "" {
		return nil, errors.New("order has no payment_id; nothing to refund at Mollie")
	}
	apiKey, err := h.mollieAPIKey(ctx, siteID)
	if err != nil {
		return nil, err
	}
	client := mollie.NewClient(apiKey)
	refund, err := client.CreateRefund(ctx, o.PaymentID, mollie.CreateRefundInput{
		Amount:      mollie.MoneyValue{Currency: o.Currency, Value: mollie.FormatAmount(o.TotalCents)},
		Description: "Refund " + o.OrderNumber,
	})
	if err != nil {
		return nil, fmt.Errorf("mollie refund failed: %w", err)
	}
	// Money has already moved at Mollie; local write failures must be
	// loud and surfaced to the agent so a human reconciles, never
	// silently swallowed behind a success payload.
	var reconcileWarnings []string
	if err := h.queries.UpdateOrderRefundID(ctx, store.UpdateOrderRefundIDParams{
		RefundID: refund.ID, ID: orderID, SiteID: siteID,
	}); err != nil {
		slog.Error("refund: Mollie refund executed but persisting refund_id failed",
			"order_id", orderID, "site_id", siteID, "refund_id", refund.ID, "err", err)
		reconcileWarnings = append(reconcileWarnings, "refund_id not persisted; reconcile manually against Mollie refund "+refund.ID)
	}
	if err := h.queries.UpdateOrderStatus(ctx, store.UpdateOrderStatusParams{
		Status:        "refunded",
		PaymentStatus: o.PaymentStatus,
		Column3:       "refunded", Column4: "refunded", Column5: "refunded", Column6: "refunded",
		ID:     orderID,
		SiteID: siteID,
	}); err != nil {
		slog.Error("refund: Mollie refund executed but status flip failed",
			"order_id", orderID, "site_id", siteID, "refund_id", refund.ID, "err", err)
		reconcileWarnings = append(reconcileWarnings, "order status still shows the pre-refund value; flag to the operator")
	}
	updated, _ := h.queries.GetOrderByID(ctx, store.GetOrderByIDParams{ID: orderID, SiteID: siteID})
	items, _ := h.queries.ListOrderItems(ctx, orderID)
	out := map[string]any{"order": updated, "items": items, "mollie_refund_id": refund.ID}
	if len(reconcileWarnings) > 0 {
		out["reconcile_warnings"] = reconcileWarnings
	}
	return out, nil
}
