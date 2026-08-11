package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bright-interaction/slab/internal/handlers"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"
)

// registerOrderTools wires the Sprint 2 slice B orders MCP surface:
//
//   - list_orders: enumerate the site's orders (optional status filter)
//   - get_order: fetch one order + its line items
//   - update_order_status: flip state per the state machine
//     (pending->paid->fulfilled->refunded, with cancelled/failed
//     terminals)
//   - refund_order: trigger a Mollie refund + flip to refunded
//
// Write tools are RequiresWrite. Nil handler disables the tools with
// a clean "not configured" error.
// orderForAgent projects an order to the fields safe to expose over MCP. It
// drops the buyer's phone, shipping/billing addresses, payment ids + checkout
// URL, refund id, and the free-text notes/metadata (which can carry PII the
// field-level shield does not key on). customer_name/email remain so the agent
// can identify the order; the shield redacts those when enabled.
func orderForAgent(o store.Order) map[string]any {
	return map[string]any{
		"id":               o.ID,
		"order_number":     o.OrderNumber,
		"status":           o.Status,
		"customer_name":    o.CustomerName,
		"customer_email":   o.CustomerEmail,
		"subtotal_cents":   o.SubtotalCents,
		"discount_cents":   o.DiscountCents,
		"shipping_cents":   o.ShippingCents,
		"tax_cents":        o.TaxCents,
		"total_cents":      o.TotalCents,
		"currency":         o.Currency,
		"payment_provider": o.PaymentProvider,
		"payment_status":   o.PaymentStatus,
		"created_at":       o.CreatedAt,
		"updated_at":       o.UpdatedAt,
		"paid_at":          o.PaidAt,
		"fulfilled_at":     o.FulfilledAt,
		"cancelled_at":     o.CancelledAt,
		"refunded_at":      o.RefundedAt,
	}
}

func ordersForAgent(rows []store.Order) []map[string]any {
	out := make([]map[string]any, len(rows))
	for i, o := range rows {
		out[i] = orderForAgent(o)
	}
	return out
}

func (s *Server) registerOrderTools() {
	register := func(t Tool) { s.tools[t.Name] = t }

	register(Tool{
		Name:        "list_orders",
		Description: "Lists the site's orders newest-first. Optional status filter ('pending'|'paid'|'fulfilled'|'cancelled'|'refunded'|'failed'). Returns id, order_number, status, customer_name, customer_email, total_cents, currency, created_at. Use get_order to fetch a single order plus its line items.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{"status":{"type":"string"}}
		}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			if s.orders == nil {
				return "", errors.New("list_orders not configured")
			}
			var args struct {
				Status string `json:"status"`
			}
			_ = json.Unmarshal(raw, &args)
			args.Status = strings.ToLower(strings.TrimSpace(args.Status))
			var rows []store.Order
			var err error
			if args.Status != "" && args.Status != "all" {
				rows, err = s.queries.ListOrdersBySiteStatus(ctx, store.ListOrdersBySiteStatusParams{
					SiteID: agent.SiteID, Status: args.Status, Limit: 500,
				})
			} else {
				rows, err = s.queries.ListOrdersBySite(ctx, store.ListOrdersBySiteParams{
					SiteID: agent.SiteID, Limit: 500,
				})
			}
			if err != nil {
				return "", err
			}
			return mustJSON(map[string]any{"orders": ordersForAgent(rows), "count": len(rows)}), nil
		},
	})

	register(Tool{
		Name:        "get_order",
		Description: "Returns a single order plus its line items. Required: order_id.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{"order_id":{"type":"string"}},
			"required":["order_id"]
		}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			if s.orders == nil {
				return "", errors.New("get_order not configured")
			}
			var args struct {
				OrderID string `json:"order_id"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			o, err := s.queries.GetOrderByID(ctx, store.GetOrderByIDParams{ID: args.OrderID, SiteID: agent.SiteID})
			if err != nil {
				return "", errors.New("order not found")
			}
			items, _ := s.queries.ListOrderItems(ctx, args.OrderID)
			return mustJSON(map[string]any{"order": orderForAgent(o), "items": items}), nil
		},
	})

	register(Tool{
		Name:        "update_order_status",
		Description: "Flips an order through the state machine. Required: order_id, status ('paid'|'fulfilled'|'cancelled'|'refunded'|'failed'). Transitions: pending->paid|cancelled|failed; paid->fulfilled|refunded; fulfilled->refunded. The Mollie webhook handles pending->paid automatically; use this for operator-driven flips (typically 'fulfilled' once an order ships). Optional notes (admin-only freeform field).",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"order_id":{"type":"string"},
				"status":{"type":"string","enum":["paid","fulfilled","cancelled","refunded","failed"]},
				"notes":{"type":"string"}
			},
			"required":["order_id","status"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			if s.orders == nil {
				return "", errors.New("update_order_status not configured")
			}
			var args struct {
				OrderID string `json:"order_id"`
				Status  string `json:"status"`
				Notes   string `json:"notes"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			result, err := s.orders.UpdateStatusForAgent(ctx, agent.SiteID, args.OrderID, args.Status, args.Notes)
			if err != nil {
				return "", err
			}
			return mustJSON(result), nil
		},
	})

	register(Tool{
		Name:        "refund_order",
		Description: "Refunds the full order amount via Mollie. Required: order_id. Flips order to status='refunded' optimistically; the Mollie refund webhook confirms completion (which can take minutes). Cannot refund orders that are not 'paid' or 'fulfilled'. Requires the site's Mollie API key in payments.mollie_api_key settings.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{"order_id":{"type":"string"}},
			"required":["order_id"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			if s.orders == nil {
				return "", errors.New("refund_order not configured")
			}
			var args struct {
				OrderID string `json:"order_id"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			result, err := s.orders.RefundForAgent(ctx, agent.SiteID, args.OrderID)
			if errors.Is(err, handlers.ErrRefundAlreadyClaimed) {
				// A refusal, not a failure. Say so in terms the agent can act on:
				// no money moved on this call, retrying cannot succeed, and the
				// order is not left half-refunded. Without this the agent sees a
				// bare error next to a tool whose whole job is moving money, and
				// the reasonable-looking move is to retry it.
				return "", fmt.Errorf("%w. No refund was created for this call and no money moved. "+
					"Do not retry: read the order with get_order to see who owns it and what state it is in", err)
			}
			if err != nil {
				return "", err
			}
			return mustJSON(result), nil
		},
	})
}

// statusToOrderUpdateResult is the result map returned by both the
// REST and the MCP paths. Defined here so the package compiles even
// before the handler exposes its For-agent helpers; the handler-side
// file implements them under handlers.OrderHandler.
var _ = handlers.OrderHandler{}
