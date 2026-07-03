// Package handlers: discount codes endpoint.
//
// Sprint 2 (slice A: catalog) of the WP/Webflow replacement roadmap.
// Codes apply to a tenant storefront's cart at checkout (slice B uses
// IncrementDiscountCodeUsedCount under a transaction to keep
// concurrent redemptions correct).
//
// kind=percent stores basis points (e.g. 1500 = 15.00%); kind=fixed
// stores integer cents (e.g. 1000 = €10.00 off). The handler validates
// both at write time so the storefront can trust the stored values.

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/bright-interaction/slab/internal/config"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"
)

var discountCodeRE = regexp.MustCompile(`^[A-Z0-9][A-Z0-9_-]{1,49}$`)

func validateDiscountCode(c string) error {
	c = strings.ToUpper(strings.TrimSpace(c))
	if !discountCodeRE.MatchString(c) {
		return errInvalidDiscountCode
	}
	return nil
}

var (
	errInvalidDiscountCode = simpleErr("code must be 2-50 uppercase alphanumeric / dash / underscore")
)

type simpleErr string

func (e simpleErr) Error() string { return string(e) }

func validDiscountKind(s string) bool { return s == "percent" || s == "fixed" }
func validAppliesTo(s string) bool    { return s == "all" || s == "categories" || s == "products" }

type DiscountCodeHandler struct {
	cfg     *config.Config
	queries *store.Queries
}

func NewDiscountCodeHandler(cfg *config.Config, queries *store.Queries) *DiscountCodeHandler {
	return &DiscountCodeHandler{cfg: cfg, queries: queries}
}

func discountCodeInSite(ctx context.Context, q *store.Queries, w http.ResponseWriter, id, siteID string) (store.DiscountCode, bool) {
	d, err := q.GetDiscountCodeByID(ctx, store.GetDiscountCodeByIDParams{ID: id, SiteID: siteID})
	if err != nil {
		writeError(w, http.StatusNotFound, "Discount code not found")
		return store.DiscountCode{}, false
	}
	return d, true
}

func (h *DiscountCodeHandler) List(w http.ResponseWriter, r *http.Request) {
	if authmw.GetUser(r) == nil && authmw.GetAgent(r) == nil {
		writeError(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	siteID := urlParam(r, "siteID")
	rows, err := h.queries.ListDiscountCodesBySite(r.Context(), store.ListDiscountCodesBySiteParams{
		SiteID: siteID,
		Limit:  500,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list discount codes failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"discount_codes": rows,
		"count":          len(rows),
	})
}

func (h *DiscountCodeHandler) Get(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	id := urlParam(r, "codeID")
	d, ok := discountCodeInSite(r.Context(), h.queries, w, id, siteID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, d)
}

type DiscountCodeInput struct {
	Code             string   `json:"code"`
	Kind             string   `json:"kind"`
	Value            int64    `json:"value"`
	MinSubtotalCents int64    `json:"min_subtotal_cents"`
	MaxUses          int64    `json:"max_uses"`
	StartsAt         string   `json:"starts_at"`
	EndsAt           string   `json:"ends_at"`
	AppliesTo        string   `json:"applies_to"`
	AppliesToIDs     []string `json:"applies_to_ids"`
	IsActive         bool     `json:"is_active"`
}

func normaliseDiscountInput(in *DiscountCodeInput) {
	in.Code = strings.ToUpper(strings.TrimSpace(in.Code))
	in.Kind = strings.ToLower(strings.TrimSpace(in.Kind))
	if in.Kind == "" {
		in.Kind = "percent"
	}
	in.AppliesTo = strings.ToLower(strings.TrimSpace(in.AppliesTo))
	if in.AppliesTo == "" {
		in.AppliesTo = "all"
	}
	if in.AppliesToIDs == nil {
		in.AppliesToIDs = []string{}
	}
}

func (h *DiscountCodeHandler) Create(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	var in DiscountCodeInput
	if err := parseJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	normaliseDiscountInput(&in)
	if err := validateDiscountCode(in.Code); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !validDiscountKind(in.Kind) {
		writeError(w, http.StatusBadRequest, "kind must be percent or fixed")
		return
	}
	if !validAppliesTo(in.AppliesTo) {
		writeError(w, http.StatusBadRequest, "applies_to must be all, categories, or products")
		return
	}
	if in.Kind == "percent" && (in.Value < 1 || in.Value > 10000) {
		writeError(w, http.StatusBadRequest, "percent value must be 1-10000 basis points (0.01% - 100%)")
		return
	}
	if in.Kind == "fixed" && in.Value < 1 {
		writeError(w, http.StatusBadRequest, "fixed value must be > 0 cents")
		return
	}
	appliesToIDsJSON, _ := json.Marshal(in.AppliesToIDs)

	id := newID()
	if err := h.queries.CreateDiscountCode(r.Context(), store.CreateDiscountCodeParams{
		ID:               id,
		SiteID:           siteID,
		Code:             in.Code,
		Kind:             in.Kind,
		Value:            in.Value,
		MinSubtotalCents: in.MinSubtotalCents,
		MaxUses:          in.MaxUses,
		StartsAt:         in.StartsAt,
		EndsAt:           in.EndsAt,
		AppliesTo:        in.AppliesTo,
		AppliesToIdsJson: string(appliesToIDsJSON),
		IsActive:         boolToInt(in.IsActive),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "create discount code failed: "+err.Error())
		return
	}
	created, _ := h.queries.GetDiscountCodeByID(r.Context(), store.GetDiscountCodeByIDParams{ID: id, SiteID: siteID})
	writeJSON(w, http.StatusCreated, created)
}

func (h *DiscountCodeHandler) Update(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	id := urlParam(r, "codeID")
	existing, ok := discountCodeInSite(r.Context(), h.queries, w, id, siteID)
	if !ok {
		return
	}
	var req struct {
		Code             *string   `json:"code"`
		Kind             *string   `json:"kind"`
		Value            *int64    `json:"value"`
		MinSubtotalCents *int64    `json:"min_subtotal_cents"`
		MaxUses          *int64    `json:"max_uses"`
		StartsAt         *string   `json:"starts_at"`
		EndsAt           *string   `json:"ends_at"`
		AppliesTo        *string   `json:"applies_to"`
		AppliesToIDs     *[]string `json:"applies_to_ids"`
		IsActive         *bool     `json:"is_active"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	params := store.UpdateDiscountCodeParams{
		Code:             existing.Code,
		Kind:             existing.Kind,
		Value:            existing.Value,
		MinSubtotalCents: existing.MinSubtotalCents,
		MaxUses:          existing.MaxUses,
		StartsAt:         existing.StartsAt,
		EndsAt:           existing.EndsAt,
		AppliesTo:        existing.AppliesTo,
		AppliesToIdsJson: existing.AppliesToIdsJson,
		IsActive:         existing.IsActive,
		ID:               id,
		SiteID:           siteID,
	}
	if req.Code != nil {
		c := strings.ToUpper(strings.TrimSpace(*req.Code))
		if err := validateDiscountCode(c); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		params.Code = c
	}
	if req.Kind != nil {
		k := strings.ToLower(strings.TrimSpace(*req.Kind))
		if !validDiscountKind(k) {
			writeError(w, http.StatusBadRequest, "kind must be percent or fixed")
			return
		}
		params.Kind = k
	}
	if req.Value != nil {
		params.Value = *req.Value
	}
	if req.MinSubtotalCents != nil {
		params.MinSubtotalCents = *req.MinSubtotalCents
	}
	if req.MaxUses != nil {
		params.MaxUses = *req.MaxUses
	}
	if req.StartsAt != nil {
		params.StartsAt = *req.StartsAt
	}
	if req.EndsAt != nil {
		params.EndsAt = *req.EndsAt
	}
	if req.AppliesTo != nil {
		a := strings.ToLower(strings.TrimSpace(*req.AppliesTo))
		if !validAppliesTo(a) {
			writeError(w, http.StatusBadRequest, "applies_to must be all, categories, or products")
			return
		}
		params.AppliesTo = a
	}
	if req.AppliesToIDs != nil {
		b, _ := json.Marshal(*req.AppliesToIDs)
		params.AppliesToIdsJson = string(b)
	}
	if req.IsActive != nil {
		params.IsActive = boolToInt(*req.IsActive)
	}

	if err := h.queries.UpdateDiscountCode(r.Context(), params); err != nil {
		writeError(w, http.StatusInternalServerError, "update discount code failed: "+err.Error())
		return
	}
	updated, _ := h.queries.GetDiscountCodeByID(r.Context(), store.GetDiscountCodeByIDParams{ID: id, SiteID: siteID})
	writeJSON(w, http.StatusOK, updated)
}

func (h *DiscountCodeHandler) Delete(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	id := urlParam(r, "codeID")
	if _, ok := discountCodeInSite(r.Context(), h.queries, w, id, siteID); !ok {
		return
	}
	if err := h.queries.DeleteDiscountCode(r.Context(), store.DeleteDiscountCodeParams{ID: id, SiteID: siteID}); err != nil {
		writeError(w, http.StatusInternalServerError, "delete discount code failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
