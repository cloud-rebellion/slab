// Package handlers: ProductHandler + DiscountCodeHandler MCP-shaped
// helpers. The REST handlers operate on http.ResponseWriter +
// *http.Request; the MCP tools call these context-only methods so the
// validation + cross-tenant guards live in exactly one place.
//
// Sprint 2 slice A of the WP/Webflow replacement roadmap.

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bright-interaction/slab/internal/store"
)

// CreateForAgent scaffolds a product from an agent-supplied input.
// Mirrors the REST Create validation pipeline.
func (h *ProductHandler) CreateForAgent(ctx context.Context, siteID string, in ProductInput) (store.Product, error) {
	normaliseProductInput(&in)
	if in.Name == "" {
		return store.Product{}, errors.New("name required")
	}
	if err := validateProductSlug(in.Slug); err != nil {
		return store.Product{}, err
	}
	if err := validateProductMoney(&in); err != nil {
		return store.Product{}, err
	}
	if !validProductStatus(in.Status) {
		return store.Product{}, errors.New("status must be draft, active, or archived")
	}
	imagesJSON, _ := json.Marshal(in.Images)
	id := newID()
	if err := h.queries.CreateProduct(ctx, store.CreateProductParams{
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
		return store.Product{}, fmt.Errorf("create product: %w", err)
	}
	return h.queries.GetProductByID(ctx, store.GetProductByIDParams{ID: id, SiteID: siteID})
}

// UpdateForAgent applies a partial update with the raw JSON payload
// so optional fields stay optional. Mirrors REST Update.
func (h *ProductHandler) UpdateForAgent(ctx context.Context, siteID, productID string, raw json.RawMessage) (store.Product, error) {
	existing, err := h.queries.GetProductByID(ctx, store.GetProductByIDParams{ID: productID, SiteID: siteID})
	if err != nil {
		return store.Product{}, errors.New("product not found")
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
	if err := json.Unmarshal(raw, &req); err != nil {
		return store.Product{}, err
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
			return store.Product{}, err
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
			return store.Product{}, errors.New("status must be draft, active, or archived")
		}
		params.Status = s
	}
	if req.Images != nil {
		b, _ := json.Marshal(*req.Images)
		params.ImagesJson = string(b)
	}
	if req.BasePriceCents != nil {
		if *req.BasePriceCents < 0 {
			return store.Product{}, errors.New("base_price_cents must be >= 0")
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
			return store.Product{}, errors.New("weight_grams must be >= 0")
		}
		params.WeightGrams = *req.WeightGrams
	}
	if req.SortOrder != nil {
		params.SortOrder = *req.SortOrder
	}
	if err := h.queries.UpdateProduct(ctx, params); err != nil {
		return store.Product{}, fmt.Errorf("update product: %w", err)
	}
	return h.queries.GetProductByID(ctx, store.GetProductByIDParams{ID: productID, SiteID: siteID})
}

// DeleteForAgent removes a product. Cascade deletes variants +
// inventory adjustments via the FK schema. Cross-tenant guard: the
// site_id is fixed by the agent identity.
func (h *ProductHandler) DeleteForAgent(ctx context.Context, siteID, productID string) error {
	if _, err := h.queries.GetProductByID(ctx, store.GetProductByIDParams{ID: productID, SiteID: siteID}); err != nil {
		return errors.New("product not found")
	}
	return h.queries.DeleteProduct(ctx, store.DeleteProductParams{ID: productID, SiteID: siteID})
}

// CreateVariantForAgent adds a variant under a product.
func (h *ProductHandler) CreateVariantForAgent(ctx context.Context, siteID, productID string, raw json.RawMessage) (store.ProductVariant, error) {
	if _, err := h.queries.GetProductByID(ctx, store.GetProductByIDParams{ID: productID, SiteID: siteID}); err != nil {
		return store.ProductVariant{}, errors.New("product not found")
	}
	var in VariantInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return store.ProductVariant{}, err
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Sku = strings.TrimSpace(in.Sku)
	if in.Attributes == nil {
		in.Attributes = map[string]any{}
	}
	if in.PriceCents < 0 {
		return store.ProductVariant{}, errors.New("price_cents must be >= 0")
	}
	attrsJSON, _ := json.Marshal(in.Attributes)
	id := newID()
	if err := h.queries.CreateProductVariant(ctx, store.CreateProductVariantParams{
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
		return store.ProductVariant{}, err
	}
	return h.queries.GetProductVariantByID(ctx, id)
}

// UpdateVariantForAgent applies a partial update to a variant.
func (h *ProductHandler) UpdateVariantForAgent(ctx context.Context, siteID, variantID string, raw json.RawMessage) (store.ProductVariant, error) {
	existing, err := h.queries.GetProductVariantByID(ctx, variantID)
	if err != nil {
		return store.ProductVariant{}, errors.New("variant not found")
	}
	parent, err := h.queries.GetProductByID(ctx, store.GetProductByIDParams{ID: existing.ProductID, SiteID: siteID})
	if err != nil || parent.SiteID != siteID {
		return store.ProductVariant{}, errors.New("variant not found")
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
	if err := json.Unmarshal(raw, &req); err != nil {
		return store.ProductVariant{}, err
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
			return store.ProductVariant{}, errors.New("price_cents must be >= 0")
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
	if err := h.queries.UpdateProductVariant(ctx, params); err != nil {
		return store.ProductVariant{}, err
	}
	return h.queries.GetProductVariantByID(ctx, variantID)
}

// DeleteVariantForAgent removes a variant.
func (h *ProductHandler) DeleteVariantForAgent(ctx context.Context, siteID, variantID string) error {
	existing, err := h.queries.GetProductVariantByID(ctx, variantID)
	if err != nil {
		return errors.New("variant not found")
	}
	if _, err := h.queries.GetProductByID(ctx, store.GetProductByIDParams{ID: existing.ProductID, SiteID: siteID}); err != nil {
		return errors.New("variant not found")
	}
	return h.queries.DeleteProductVariant(ctx, variantID)
}

// SetInventoryForAgent mirrors the REST SetInventory: applies an
// absolute (new_count) or relative (delta) change AND writes an
// inventory_adjustments row with the reason + note + actor.
func (h *ProductHandler) SetInventoryForAgent(ctx context.Context, siteID, variantID, actor string, raw json.RawMessage) (map[string]any, error) {
	existing, err := h.queries.GetProductVariantByID(ctx, variantID)
	if err != nil {
		return nil, errors.New("variant not found")
	}
	if _, err := h.queries.GetProductByID(ctx, store.GetProductByIDParams{ID: existing.ProductID, SiteID: siteID}); err != nil {
		return nil, errors.New("variant not found")
	}
	var req struct {
		NewCount *int64 `json:"new_count"`
		Delta    *int64 `json:"delta"`
		Reason   string `json:"reason"`
		Note     string `json:"note"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	reason := strings.ToLower(strings.TrimSpace(req.Reason))
	if reason == "" {
		reason = "manual"
	}
	if !validInventoryReason(reason) {
		return nil, errors.New("reason must be restock, sale, manual, damage, return, or correction")
	}
	var newCount, delta int64
	switch {
	case req.NewCount != nil:
		newCount = *req.NewCount
		delta = newCount - existing.InventoryCount
		if err := h.queries.SetVariantInventoryCount(ctx, store.SetVariantInventoryCountParams{
			InventoryCount: newCount, ID: variantID,
		}); err != nil {
			return nil, err
		}
	case req.Delta != nil:
		delta = *req.Delta
		newCount = existing.InventoryCount + delta
		if err := h.queries.AdjustVariantInventoryCount(ctx, store.AdjustVariantInventoryCountParams{
			InventoryCount: delta, ID: variantID,
		}); err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("either new_count or delta required")
	}
	_ = h.queries.CreateInventoryAdjustment(ctx, store.CreateInventoryAdjustmentParams{
		ID: newID(), VariantID: variantID, SiteID: siteID,
		Delta: delta, NewCount: newCount, Reason: reason, Note: req.Note, CreatedBy: actor,
	})
	updated, _ := h.queries.GetProductVariantByID(ctx, variantID)
	return map[string]any{
		"variant": updated,
		"delta":   delta,
		"reason":  reason,
	}, nil
}

// CreateForAgent / UpdateForAgent / DeleteForAgent on DiscountCodeHandler.

func (h *DiscountCodeHandler) CreateForAgent(ctx context.Context, siteID string, raw json.RawMessage) (store.DiscountCode, error) {
	var in DiscountCodeInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return store.DiscountCode{}, err
	}
	normaliseDiscountInput(&in)
	if err := validateDiscountCode(in.Code); err != nil {
		return store.DiscountCode{}, err
	}
	if err := validateDiscountWindow(in.StartsAt, in.EndsAt); err != nil {
		return store.DiscountCode{}, err
	}
	if !validDiscountKind(in.Kind) {
		return store.DiscountCode{}, errors.New("kind must be percent or fixed")
	}
	if !validAppliesTo(in.AppliesTo) {
		return store.DiscountCode{}, errors.New("applies_to must be all, categories, or products")
	}
	if in.Kind == "percent" && (in.Value < 1 || in.Value > 10000) {
		return store.DiscountCode{}, errors.New("percent value must be 1-10000 basis points")
	}
	if in.Kind == "fixed" && in.Value < 1 {
		return store.DiscountCode{}, errors.New("fixed value must be > 0 cents")
	}
	appliesToIDsJSON, _ := json.Marshal(in.AppliesToIDs)
	id := newID()
	if err := h.queries.CreateDiscountCode(ctx, store.CreateDiscountCodeParams{
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
		return store.DiscountCode{}, fmt.Errorf("create discount code: %w", err)
	}
	return h.queries.GetDiscountCodeByID(ctx, store.GetDiscountCodeByIDParams{ID: id, SiteID: siteID})
}

func (h *DiscountCodeHandler) UpdateForAgent(ctx context.Context, siteID, id string, raw json.RawMessage) (store.DiscountCode, error) {
	existing, err := h.queries.GetDiscountCodeByID(ctx, store.GetDiscountCodeByIDParams{ID: id, SiteID: siteID})
	if err != nil {
		return store.DiscountCode{}, errors.New("discount code not found")
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
	if err := json.Unmarshal(raw, &req); err != nil {
		return store.DiscountCode{}, err
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
			return store.DiscountCode{}, err
		}
		params.Code = c
	}
	if req.Kind != nil {
		k := strings.ToLower(strings.TrimSpace(*req.Kind))
		if !validDiscountKind(k) {
			return store.DiscountCode{}, errors.New("kind must be percent or fixed")
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
			return store.DiscountCode{}, errors.New("applies_to must be all, categories, or products")
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
	if err := h.queries.UpdateDiscountCode(ctx, params); err != nil {
		return store.DiscountCode{}, err
	}
	return h.queries.GetDiscountCodeByID(ctx, store.GetDiscountCodeByIDParams{ID: id, SiteID: siteID})
}

func (h *DiscountCodeHandler) DeleteForAgent(ctx context.Context, siteID, id string) error {
	if _, err := h.queries.GetDiscountCodeByID(ctx, store.GetDiscountCodeByIDParams{ID: id, SiteID: siteID}); err != nil {
		return errors.New("discount code not found")
	}
	return h.queries.DeleteDiscountCode(ctx, store.DeleteDiscountCodeParams{ID: id, SiteID: siteID})
}
