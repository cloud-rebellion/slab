// Package handlers: products + variants + inventory endpoints.
//
// Sprint 2 (slice A: catalog) of the WP/Webflow replacement roadmap.
// Backs the admin Store > Products tab and the agent MCP tools for
// managing a tenant catalog. Orders + checkout land in slice B with
// the Mollie integration.
//
// Pricing is integer cents per ISO 4217 currency, single currency
// per product. VAT-inclusive per EU norms (tax_class drives the
// per-country rate at checkout time, which slice B / C add).
//
// Inventory adjustments are written to inventory_adjustments as an
// append-only audit log, so the dashboard can show "Today: -3 from
// sales, +50 from restock" without parsing notes.

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/bright-interaction/slab/internal/config"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"
)

var productSlugRE = regexp.MustCompile(`^[a-z0-9][-a-z0-9_]{0,79}$`)

func validateProductSlug(s string) error {
	if s == "" {
		return fmt.Errorf("slug required")
	}
	if len(s) > 80 {
		return fmt.Errorf("slug too long (max 80 chars)")
	}
	if !productSlugRE.MatchString(s) {
		return fmt.Errorf("slug must match ^[a-z0-9][-a-z0-9_]{0,79}$")
	}
	return nil
}

func validProductStatus(s string) bool {
	return s == "draft" || s == "active" || s == "archived"
}

func validInventoryReason(s string) bool {
	switch s {
	case "restock", "sale", "manual", "damage", "return", "correction":
		return true
	}
	return false
}

type ProductHandler struct {
	cfg     *config.Config
	queries *store.Queries
}

func NewProductHandler(cfg *config.Config, queries *store.Queries) *ProductHandler {
	return &ProductHandler{cfg: cfg, queries: queries}
}

func productInSite(ctx context.Context, q *store.Queries, w http.ResponseWriter, productID, siteID string) (store.Product, bool) {
	p, err := q.GetProductByID(ctx, store.GetProductByIDParams{ID: productID, SiteID: siteID})
	if err != nil {
		writeError(w, http.StatusNotFound, "Product not found")
		return store.Product{}, false
	}
	return p, true
}

// variantInSite walks variant -> product -> site to guard against
// cross-tenant access by knowing a variant ID directly.
func variantInSite(ctx context.Context, q *store.Queries, w http.ResponseWriter, variantID, siteID string) (store.ProductVariant, store.Product, bool) {
	v, err := q.GetProductVariantByID(ctx, variantID)
	if err != nil {
		writeError(w, http.StatusNotFound, "Variant not found")
		return store.ProductVariant{}, store.Product{}, false
	}
	p, err := q.GetProductByID(ctx, store.GetProductByIDParams{ID: v.ProductID, SiteID: siteID})
	if err != nil {
		writeError(w, http.StatusNotFound, "Variant not found")
		return store.ProductVariant{}, store.Product{}, false
	}
	return v, p, true
}

// List returns the catalog for the dashboard. Optional ?status filter.
func (h *ProductHandler) List(w http.ResponseWriter, r *http.Request) {
	if authmw.GetUser(r) == nil && authmw.GetAgent(r) == nil {
		writeError(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	siteID := urlParam(r, "siteID")
	rows, err := h.queries.ListProductsBySite(r.Context(), store.ListProductsBySiteParams{
		SiteID: siteID,
		Limit:  500,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list products failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"products": rows,
		"count":    len(rows),
	})
}

func (h *ProductHandler) Get(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	productID := urlParam(r, "productID")
	p, ok := productInSite(r.Context(), h.queries, w, productID, siteID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, p)
}

type ProductInput struct {
	Name             string   `json:"name"`
	Slug             string   `json:"slug"`
	Description      string   `json:"description"`
	Category         string   `json:"category"`
	Status           string   `json:"status"`
	Images           []string `json:"images"`
	BasePriceCents   int64    `json:"base_price_cents"`
	Currency         string   `json:"currency"`
	RequiresShipping bool     `json:"requires_shipping"`
	TaxClass         string   `json:"tax_class"`
	WeightGrams      int64    `json:"weight_grams"`
	SortOrder        int64    `json:"sort_order"`
}

func normaliseProductInput(in *ProductInput) {
	in.Name = strings.TrimSpace(in.Name)
	in.Slug = strings.ToLower(strings.TrimSpace(in.Slug))
	in.Status = strings.ToLower(strings.TrimSpace(in.Status))
	if in.Status == "" {
		in.Status = "draft"
	}
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	if in.Currency == "" {
		in.Currency = "EUR"
	}
	in.TaxClass = strings.TrimSpace(in.TaxClass)
	if in.TaxClass == "" {
		in.TaxClass = "standard"
	}
	if in.Images == nil {
		in.Images = []string{}
	}
}

func (h *ProductHandler) Create(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	var in ProductInput
	if err := parseJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	normaliseProductInput(&in)
	if in.Name == "" {
		writeError(w, http.StatusBadRequest, "name required")
		return
	}
	if err := validateProductSlug(in.Slug); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !validProductStatus(in.Status) {
		writeError(w, http.StatusBadRequest, "status must be draft, active, or archived")
		return
	}

	imagesJSON, _ := json.Marshal(in.Images)
	id := newID()
	if err := h.queries.CreateProduct(r.Context(), store.CreateProductParams{
		ID:               id,
		SiteID:           siteID,
		Name:             in.Name,
		Slug:             in.Slug,
		Description:      in.Description,
		Category:         in.Category,
		Status:           in.Status,
		ImagesJson:       string(imagesJSON),
		BasePriceCents:   in.BasePriceCents,
		Currency:         in.Currency,
		RequiresShipping: boolToInt(in.RequiresShipping),
		TaxClass:         in.TaxClass,
		WeightGrams:      in.WeightGrams,
		SortOrder:        in.SortOrder,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "create product failed: "+err.Error())
		return
	}
	created, _ := h.queries.GetProductByID(r.Context(), store.GetProductByIDParams{ID: id, SiteID: siteID})
	writeJSON(w, http.StatusCreated, created)
}

func (h *ProductHandler) Update(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	productID := urlParam(r, "productID")
	existing, ok := productInSite(r.Context(), h.queries, w, productID, siteID)
	if !ok {
		return
	}

	var req struct {
		Name             *string   `json:"name"`
		Slug             *string   `json:"slug"`
		Description      *string   `json:"description"`
		Category         *string   `json:"category"`
		Status           *string   `json:"status"`
		Images           *[]string `json:"images"`
		BasePriceCents   *int64    `json:"base_price_cents"`
		Currency         *string   `json:"currency"`
		RequiresShipping *bool     `json:"requires_shipping"`
		TaxClass         *string   `json:"tax_class"`
		WeightGrams      *int64    `json:"weight_grams"`
		SortOrder        *int64    `json:"sort_order"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}

	params := store.UpdateProductParams{
		Name:             existing.Name,
		Slug:             existing.Slug,
		Description:      existing.Description,
		Category:         existing.Category,
		Status:           existing.Status,
		ImagesJson:       existing.ImagesJson,
		BasePriceCents:   existing.BasePriceCents,
		Currency:         existing.Currency,
		RequiresShipping: existing.RequiresShipping,
		TaxClass:         existing.TaxClass,
		WeightGrams:      existing.WeightGrams,
		SortOrder:        existing.SortOrder,
		ID:               productID,
		SiteID:           siteID,
	}
	if req.Name != nil {
		params.Name = strings.TrimSpace(*req.Name)
	}
	if req.Slug != nil {
		slug := strings.ToLower(strings.TrimSpace(*req.Slug))
		if err := validateProductSlug(slug); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		params.Slug = slug
	}
	if req.Description != nil {
		params.Description = *req.Description
	}
	if req.Category != nil {
		params.Category = *req.Category
	}
	if req.Status != nil {
		s := strings.ToLower(strings.TrimSpace(*req.Status))
		if !validProductStatus(s) {
			writeError(w, http.StatusBadRequest, "status must be draft, active, or archived")
			return
		}
		params.Status = s
	}
	if req.Images != nil {
		b, _ := json.Marshal(*req.Images)
		params.ImagesJson = string(b)
	}
	if req.BasePriceCents != nil {
		if *req.BasePriceCents < 0 {
			writeError(w, http.StatusBadRequest, "base_price_cents must be >= 0")
			return
		}
		params.BasePriceCents = *req.BasePriceCents
	}
	if req.Currency != nil {
		params.Currency = strings.ToUpper(strings.TrimSpace(*req.Currency))
	}
	if req.RequiresShipping != nil {
		params.RequiresShipping = boolToInt(*req.RequiresShipping)
	}
	if req.TaxClass != nil {
		params.TaxClass = *req.TaxClass
	}
	if req.WeightGrams != nil {
		if *req.WeightGrams < 0 {
			writeError(w, http.StatusBadRequest, "weight_grams must be >= 0")
			return
		}
		params.WeightGrams = *req.WeightGrams
	}
	if req.SortOrder != nil {
		params.SortOrder = *req.SortOrder
	}

	if err := h.queries.UpdateProduct(r.Context(), params); err != nil {
		writeError(w, http.StatusInternalServerError, "update product failed: "+err.Error())
		return
	}
	updated, _ := h.queries.GetProductByID(r.Context(), store.GetProductByIDParams{ID: productID, SiteID: siteID})
	writeJSON(w, http.StatusOK, updated)
}

func (h *ProductHandler) Delete(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	productID := urlParam(r, "productID")
	if _, ok := productInSite(r.Context(), h.queries, w, productID, siteID); !ok {
		return
	}
	if err := h.queries.DeleteProduct(r.Context(), store.DeleteProductParams{ID: productID, SiteID: siteID}); err != nil {
		writeError(w, http.StatusInternalServerError, "delete product failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// Variants =================================================================

func (h *ProductHandler) ListVariants(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	productID := urlParam(r, "productID")
	if _, ok := productInSite(r.Context(), h.queries, w, productID, siteID); !ok {
		return
	}
	rows, err := h.queries.ListProductVariants(r.Context(), productID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list variants failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"variants": rows,
		"count":    len(rows),
	})
}

type VariantInput struct {
	Sku                 string         `json:"sku"`
	Name                string         `json:"name"`
	PriceCents          int64          `json:"price_cents"`
	CompareAtPriceCents int64          `json:"compare_at_price_cents"`
	InventoryCount      int64          `json:"inventory_count"`
	AllowBackorder      bool           `json:"allow_backorder"`
	SortOrder           int64          `json:"sort_order"`
	Attributes          map[string]any `json:"attributes"`
}

func (h *ProductHandler) CreateVariant(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	productID := urlParam(r, "productID")
	if _, ok := productInSite(r.Context(), h.queries, w, productID, siteID); !ok {
		return
	}
	var in VariantInput
	if err := parseJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Sku = strings.TrimSpace(in.Sku)
	if in.Attributes == nil {
		in.Attributes = map[string]any{}
	}
	if in.PriceCents < 0 {
		writeError(w, http.StatusBadRequest, "price_cents must be >= 0")
		return
	}
	attrsJSON, _ := json.Marshal(in.Attributes)
	id := newID()
	if err := h.queries.CreateProductVariant(r.Context(), store.CreateProductVariantParams{
		ID:                  id,
		ProductID:           productID,
		Sku:                 in.Sku,
		Name:                in.Name,
		PriceCents:          in.PriceCents,
		CompareAtPriceCents: in.CompareAtPriceCents,
		InventoryCount:      in.InventoryCount,
		AllowBackorder:      boolToInt(in.AllowBackorder),
		SortOrder:           in.SortOrder,
		AttributesJson:      string(attrsJSON),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "create variant failed: "+err.Error())
		return
	}
	created, _ := h.queries.GetProductVariantByID(r.Context(), id)
	writeJSON(w, http.StatusCreated, created)
}

func (h *ProductHandler) UpdateVariant(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	variantID := urlParam(r, "variantID")
	existing, _, ok := variantInSite(r.Context(), h.queries, w, variantID, siteID)
	if !ok {
		return
	}
	var req struct {
		Sku                 *string         `json:"sku"`
		Name                *string         `json:"name"`
		PriceCents          *int64          `json:"price_cents"`
		CompareAtPriceCents *int64          `json:"compare_at_price_cents"`
		InventoryCount      *int64          `json:"inventory_count"`
		AllowBackorder      *bool           `json:"allow_backorder"`
		SortOrder           *int64          `json:"sort_order"`
		Attributes          *map[string]any `json:"attributes"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	params := store.UpdateProductVariantParams{
		Sku:                 existing.Sku,
		Name:                existing.Name,
		PriceCents:          existing.PriceCents,
		CompareAtPriceCents: existing.CompareAtPriceCents,
		InventoryCount:      existing.InventoryCount,
		AllowBackorder:      existing.AllowBackorder,
		SortOrder:           existing.SortOrder,
		AttributesJson:      existing.AttributesJson,
		ID:                  variantID,
	}
	if req.Sku != nil {
		params.Sku = strings.TrimSpace(*req.Sku)
	}
	if req.Name != nil {
		params.Name = strings.TrimSpace(*req.Name)
	}
	if req.PriceCents != nil {
		if *req.PriceCents < 0 {
			writeError(w, http.StatusBadRequest, "price_cents must be >= 0")
			return
		}
		params.PriceCents = *req.PriceCents
	}
	if req.CompareAtPriceCents != nil {
		params.CompareAtPriceCents = *req.CompareAtPriceCents
	}
	if req.InventoryCount != nil {
		params.InventoryCount = *req.InventoryCount
	}
	if req.AllowBackorder != nil {
		params.AllowBackorder = boolToInt(*req.AllowBackorder)
	}
	if req.SortOrder != nil {
		params.SortOrder = *req.SortOrder
	}
	if req.Attributes != nil {
		b, _ := json.Marshal(*req.Attributes)
		params.AttributesJson = string(b)
	}
	if err := h.queries.UpdateProductVariant(r.Context(), params); err != nil {
		writeError(w, http.StatusInternalServerError, "update variant failed")
		return
	}
	updated, _ := h.queries.GetProductVariantByID(r.Context(), variantID)
	writeJSON(w, http.StatusOK, updated)
}

func (h *ProductHandler) DeleteVariant(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	variantID := urlParam(r, "variantID")
	if _, _, ok := variantInSite(r.Context(), h.queries, w, variantID, siteID); !ok {
		return
	}
	if err := h.queries.DeleteProductVariant(r.Context(), variantID); err != nil {
		writeError(w, http.StatusInternalServerError, "delete variant failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// SetInventory sets the variant's inventory_count and writes an
// inventory_adjustments row capturing the delta + reason + actor.
func (h *ProductHandler) SetInventory(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	variantID := urlParam(r, "variantID")
	existing, _, ok := variantInSite(r.Context(), h.queries, w, variantID, siteID)
	if !ok {
		return
	}
	var req struct {
		NewCount *int64 `json:"new_count"`
		Delta    *int64 `json:"delta"`
		Reason   string `json:"reason"`
		Note     string `json:"note"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	reason := strings.ToLower(strings.TrimSpace(req.Reason))
	if reason == "" {
		reason = "manual"
	}
	if !validInventoryReason(reason) {
		writeError(w, http.StatusBadRequest, "reason must be restock, sale, manual, damage, return, or correction")
		return
	}

	var newCount, delta int64
	switch {
	case req.NewCount != nil:
		newCount = *req.NewCount
		delta = newCount - existing.InventoryCount
		if err := h.queries.SetVariantInventoryCount(r.Context(), store.SetVariantInventoryCountParams{
			InventoryCount: newCount,
			ID:             variantID,
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "set inventory failed")
			return
		}
	case req.Delta != nil:
		delta = *req.Delta
		newCount = existing.InventoryCount + delta
		if err := h.queries.AdjustVariantInventoryCount(r.Context(), store.AdjustVariantInventoryCountParams{
			InventoryCount: delta,
			ID:             variantID,
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "adjust inventory failed")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "either new_count or delta required")
		return
	}

	_ = h.queries.CreateInventoryAdjustment(r.Context(), store.CreateInventoryAdjustmentParams{
		ID:        newID(),
		VariantID: variantID,
		SiteID:    siteID,
		Delta:     delta,
		NewCount:  newCount,
		Reason:    reason,
		Note:      req.Note,
		CreatedBy: snapshotCreatedBy(r),
	})

	updated, _ := h.queries.GetProductVariantByID(r.Context(), variantID)
	writeJSON(w, http.StatusOK, map[string]any{
		"variant": updated,
		"delta":   delta,
		"reason":  reason,
	})
}

func (h *ProductHandler) ListInventoryAdjustments(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	variantID := urlParam(r, "variantID")
	if _, _, ok := variantInSite(r.Context(), h.queries, w, variantID, siteID); !ok {
		return
	}
	rows, err := h.queries.ListInventoryAdjustmentsByVariant(r.Context(), store.ListInventoryAdjustmentsByVariantParams{
		VariantID: variantID,
		Limit:     200,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list adjustments failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"adjustments": rows,
		"count":       len(rows),
	})
}
