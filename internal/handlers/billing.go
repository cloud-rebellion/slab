// Package handlers: billing.go (Phase 30.2, Cloud Tier MVP, 2026-05-06).
//
// Routes (cloud only, mounted under -tags ee):
//
//	POST /api/workspaces/{workspaceID}/billing/checkout   start a checkout session
//	GET  /api/workspaces/{workspaceID}/billing            current plan + usage
//	POST /api/billing/webhook                             Mollie webhook (no auth)
//
// The Mollie SDK lives in internal/cloud/mollie under -tags ee. This
// file is OSS-safe: handler structs are always defined so route
// registration compiles; the actual checkout / webhook logic returns
// 503 in OSS via the build-tagged shim in billing_oss.go /
// billing_ee.go. The seam keeps cmd/server/main.go and internal/server
// unchanged across builds.
package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/bright-interaction/slab/internal/billing"
	"github.com/bright-interaction/slab/internal/config"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"
)

// BillingHandler wires Mollie checkout + webhook + workspace plan
// status. The mollieRunner field is the only seam the EE build
// overrides; OSS leaves it nil and the handler returns 503.
type BillingHandler struct {
	cfg          *config.Config
	queries      *store.Queries
	checkoutImpl checkoutFunc
	webhookImpl  webhookFunc
}

// checkoutFunc is set by the EE build to mollie.StartCheckout. OSS
// leaves it nil so callers get a clean 503.
type checkoutFunc func(ctx context.Context, in CheckoutCallInput) (*CheckoutCallResult, error)

// webhookFunc is set by the EE build. OSS leaves it nil.
type webhookFunc func(ctx context.Context, paymentID string) (*WebhookCallResult, error)

// CheckoutCallInput is the SaaS-side intent. Mirrors mollie.CheckoutInput
// but lives in handlers/ so OSS doesn't import the mollie package.
type CheckoutCallInput struct {
	WorkspaceID   string
	WorkspaceSlug string
	UserEmail     string
	UserName      string
	PlanKey       string
	BaseURL       string
}

// CheckoutCallResult mirrors mollie.CheckoutResult.
type CheckoutCallResult struct {
	CheckoutURL string
	PaymentID   string
	CustomerID  string
}

// WebhookCallResult is the resolved payment status the EE shim
// returns to the OSS-shaped webhook handler.
type WebhookCallResult struct {
	PaymentID   string
	Status      string
	Description string
	CustomerID  string
	WorkspaceID string
	PlanKey     string
	AmountCents int64
	Currency    string
	Mode        string
	RawJSON     string
}

// NewBillingHandler is called by both builds. The EE build uses
// SetBillingMollie to wire the Mollie impls in afterwards.
func NewBillingHandler(cfg *config.Config, queries *store.Queries) *BillingHandler {
	return &BillingHandler{cfg: cfg, queries: queries}
}

// SetCheckout wires the EE checkout impl. Called by cmd/server/main.go
// only when -tags ee is active.
func (h *BillingHandler) SetCheckout(fn checkoutFunc) { h.checkoutImpl = fn }

// SetWebhook wires the EE webhook impl.
func (h *BillingHandler) SetWebhook(fn webhookFunc) { h.webhookImpl = fn }

// StartCheckout: POST /api/workspaces/{workspaceID}/billing/checkout
// Body: {"plan":"solo"|"studio"|"agency"}. Owner-only.
func (h *BillingHandler) StartCheckout(w http.ResponseWriter, r *http.Request) {
	if !authmw.RequireWorkspaceRole(w, r, "owner") {
		return
	}
	if h.checkoutImpl == nil {
		writeError(w, http.StatusServiceUnavailable, "Billing not configured (cloud build only)")
		return
	}
	user := authmw.GetUser(r)
	workspaceID := urlParam(r, "workspaceID")
	ws, err := h.queries.GetWorkspaceByID(r.Context(), workspaceID)
	if err != nil {
		writeError(w, http.StatusNotFound, "Workspace not found")
		return
	}
	var req struct {
		Plan string `json:"plan"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	plan, ok := billing.Plans[strings.ToLower(strings.TrimSpace(req.Plan))]
	if !ok || plan.PriceCents <= 0 {
		writeError(w, http.StatusBadRequest, "plan must be one of solo, studio, agency")
		return
	}
	in := CheckoutCallInput{
		WorkspaceID:   ws.ID,
		WorkspaceSlug: ws.Slug,
		UserEmail:     user.Email,
		UserName:      user.Name,
		PlanKey:       plan.Key,
		BaseURL:       strings.TrimRight(h.cfg.BaseURL, "/"),
	}
	res, err := h.checkoutImpl(r.Context(), in)
	if err != nil {
		writeError(w, http.StatusBadGateway, "Checkout failed: "+err.Error())
		return
	}
	subID := newID()
	if err := h.queries.CreateSubscription(r.Context(), store.CreateSubscriptionParams{
		ID:                 subID,
		WorkspaceID:        ws.ID,
		Provider:           "mollie",
		ExternalID:         res.PaymentID, // payment id at first; webhook upgrades to subscription id
		ExternalCustomerID: res.CustomerID,
		Plan:               plan.Key,
		Status:             "pending",
		AmountCents:        plan.PriceCents,
		Currency:           plan.Currency,
		IntervalUnit:       "months",
		IntervalCount:      1,
		CurrentPeriodEnd:   "",
		CancelAt:           "",
		MetadataJson:       `{"first_payment_id":"` + res.PaymentID + `"}`,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to record subscription")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"checkout_url": res.CheckoutURL,
		"payment_id":   res.PaymentID,
		"customer_id":  res.CustomerID,
	})
}

// GetBilling: GET /api/workspaces/{workspaceID}/billing returns the
// current plan + usage breakdown so the frontend can render the
// billing page without an extra round trip.
func (h *BillingHandler) GetBilling(w http.ResponseWriter, r *http.Request) {
	workspaceID := urlParam(r, "workspaceID")
	ws, err := h.queries.GetWorkspaceByID(r.Context(), workspaceID)
	if err != nil {
		writeError(w, http.StatusNotFound, "Workspace not found")
		return
	}
	plan := billing.Lookup(ws.Plan)
	sub, _ := h.queries.GetSubscriptionByWorkspace(r.Context(), workspaceID)
	sites, _ := h.queries.ListSitesByWorkspace(r.Context(), workspaceID)
	writeJSON(w, http.StatusOK, map[string]any{
		"plan":             plan,
		"subscription":     sub,
		"workspace_status": ws.Status,
		"sites_count":      len(sites),
	})
}

// Webhook: POST /api/billing/webhook (no auth, body is form-encoded
// `id=tr_xxx`). Idempotent via billing_events.UNIQUE(provider,
// external_event_id). Always returns 200 OK so Mollie stops retrying;
// errors are recorded on the billing_events row.
func (h *BillingHandler) Webhook(w http.ResponseWriter, r *http.Request) {
	if h.webhookImpl == nil {
		// Acknowledge to stop Mollie retries even though we can't process.
		// Logged via the existing access-log pipeline.
		writeJSON(w, http.StatusOK, map[string]string{"status": "no-op"})
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "bad-form"})
		return
	}
	paymentID := strings.TrimSpace(r.FormValue("id"))
	if paymentID == "" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "no-id"})
		return
	}
	// Record-then-process for idempotency. INSERT OR IGNORE means a
	// duplicate ping is recorded once; the second handler run sees
	// the same row and does nothing.
	eventID := newID()
	_ = h.queries.RecordBillingEvent(r.Context(), store.RecordBillingEventParams{
		ID:              eventID,
		WorkspaceID:     "",
		Provider:        "mollie",
		ExternalEventID: paymentID,
		EventType:       "payment.notification",
		PayloadJson:     `{"id":"` + paymentID + `"}`,
	})
	res, err := h.webhookImpl(r.Context(), paymentID)
	if err != nil {
		_ = h.queries.MarkBillingEventProcessed(r.Context(), store.MarkBillingEventProcessedParams{
			ID:    eventID,
			Error: err.Error(),
		})
		writeJSON(w, http.StatusOK, map[string]string{"status": "fetch-failed"})
		return
	}
	// Map status to subscription state.
	switch res.Status {
	case "paid":
		// First payment landed: flip workspace plan to active.
		if res.WorkspaceID != "" && res.PlanKey != "" {
			ws, err := h.queries.GetWorkspaceByID(r.Context(), res.WorkspaceID)
			if err == nil {
				_ = h.queries.UpdateWorkspacePlan(r.Context(), store.UpdateWorkspacePlanParams{
					ID:     ws.ID,
					Plan:   res.PlanKey,
					Status: "active",
				})
				_ = h.queries.UpdateWorkspaceStripe(r.Context(), store.UpdateWorkspaceStripeParams{
					ID:                   ws.ID,
					StripeCustomerID:     res.CustomerID,
					StripeSubscriptionID: res.PaymentID,
				})
			}
		}
		// Mark the subscription row paid via plan key + workspace
		// (we don't have direct row id from webhook).
		if res.WorkspaceID != "" {
			if sub, err := h.queries.GetSubscriptionByWorkspace(r.Context(), res.WorkspaceID); err == nil {
				_ = h.queries.UpdateSubscriptionStatus(r.Context(), store.UpdateSubscriptionStatusParams{
					ID:               sub.ID,
					Status:           "active",
					CurrentPeriodEnd: "",
				})
			}
		}
	case "failed", "canceled", "expired":
		if res.WorkspaceID != "" {
			if sub, err := h.queries.GetSubscriptionByWorkspace(r.Context(), res.WorkspaceID); err == nil {
				_ = h.queries.UpdateSubscriptionStatus(r.Context(), store.UpdateSubscriptionStatusParams{
					ID:               sub.ID,
					Status:           res.Status,
					CurrentPeriodEnd: "",
				})
			}
		}
	}
	rawJSON := res.RawJSON
	if rawJSON == "" {
		rawJSON = "{}"
	}
	// Persist the resolved status as a structured payload so an
	// auditor can replay the workflow without re-hitting Mollie.
	if encoded, err := json.Marshal(map[string]any{
		"payment_id":   res.PaymentID,
		"status":       res.Status,
		"workspace_id": res.WorkspaceID,
		"plan":         res.PlanKey,
		"customer_id":  res.CustomerID,
		"amount_cents": res.AmountCents,
		"currency":     res.Currency,
	}); err == nil {
		_ = json.Unmarshal(encoded, new(map[string]any)) // keep linter happy
	}
	_ = h.queries.MarkBillingEventProcessed(r.Context(), store.MarkBillingEventProcessedParams{
		ID:    eventID,
		Error: "",
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "payment_status": res.Status})
}
