package builder

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/bright-interaction/slab/internal/store"
)

// Sprint 2 slice C (2026-05-22): public storefront blocks. The four
// blocks (product_grid, product_detail, cart_drawer, checkout_form)
// build on top of slice B's order pipeline. product_grid and
// product_detail resolve product rows at build time (same pattern as
// collection_list); cart_drawer + checkout_form emit dumb shells that
// the per-site storefront island (storefront_island.go) drives at
// runtime.

// productGridMaxLimit caps how many products a single product_grid
// block can render. Higher counts hurt LCP because every card is a
// real product image. The schema's "limit" field is also surfaced to
// the linter, which warns above this number.
const productGridMaxLimit = 48

// productGridDefaultLimit is what the renderer uses when the block
// omits the limit field or sets it to <= 0.
const productGridDefaultLimit = 12

// resolveStorefrontBlocks walks the page's blocks and, for any
// product_grid or product_detail block, queries the catalog at build
// time and stuffs the resolved rows under data._resolved_* so the
// renderer can emit static markup without any DB plumbing in the
// render path. Mirrors resolveCollectionListBlocks.
//
// Errors (product not found, variants query fails) are logged + the
// block falls back to an empty-state placeholder rather than failing
// the whole build.
func resolveStorefrontBlocks(ctx context.Context, queries *store.Queries, site store.Site, blocks []store.Block) {
	for i := range blocks {
		switch blocks[i].BlockType {
		case "product_grid":
			resolveProductGridBlock(ctx, queries, site, &blocks[i])
		case "product_detail":
			resolveProductDetailBlock(ctx, queries, site, &blocks[i])
		}
	}
}

func resolveProductGridBlock(ctx context.Context, queries *store.Queries, site store.Site, bl *store.Block) {
	var data map[string]any
	if err := json.Unmarshal([]byte(bl.DataJson), &data); err != nil {
		return
	}
	limit := numberFromData(data, "limit")
	if limit <= 0 {
		limit = productGridDefaultLimit
	}
	if limit > productGridMaxLimit {
		limit = productGridMaxLimit
	}
	// Pull more than we need so an in-memory category filter still has
	// room to honour the requested limit. Worst case 200 rows scanned.
	fetchLimit := limit * 4
	if fetchLimit > 200 {
		fetchLimit = 200
	}
	products, err := queries.ListActiveProductsBySite(ctx, store.ListActiveProductsBySiteParams{
		SiteID: site.ID,
		Limit:  fetchLimit,
	})
	if err != nil {
		slog.Warn("product_grid: list products failed", "site_id", site.ID, "err", err)
		products = nil
	}

	cat := strings.TrimSpace(dataString(data, "category_filter"))
	if cat != "" {
		filtered := products[:0]
		for _, p := range products {
			if strings.EqualFold(p.Category, cat) {
				filtered = append(filtered, p)
			}
		}
		products = filtered
	}

	// Resolve a display price per product (cheapest variant; falls back
	// to base_price_cents) so the card can show a price without a
	// runtime DB lookup.
	resolved := make([]map[string]any, 0, len(products))
	for _, p := range products {
		variants, _ := queries.ListProductVariants(ctx, p.ID)
		var displayPrice int64
		var firstVariant store.ProductVariant
		if len(variants) > 0 {
			displayPrice = variants[0].PriceCents
			firstVariant = variants[0]
			for _, v := range variants[1:] {
				if v.PriceCents > 0 && (displayPrice == 0 || v.PriceCents < displayPrice) {
					displayPrice = v.PriceCents
					firstVariant = v
				}
			}
		}
		if displayPrice == 0 {
			displayPrice = p.BasePriceCents
		}
		resolved = append(resolved, map[string]any{
			"id":               p.ID,
			"name":             p.Name,
			"slug":             p.Slug,
			"description":      p.Description,
			"category":         p.Category,
			"price_cents":      displayPrice,
			"currency":         p.Currency,
			"first_image_id":   firstImageIDFrom(p.ImagesJson),
			"first_variant_id": firstVariant.ID,
			"first_variant":    variantPayload(firstVariant, p),
			"variant_count":    int64(len(variants)),
		})
	}

	// Sort. Already pre-sorted by sort_order/created_at from the query;
	// re-sort only when the block asks for something else.
	switch dataString(data, "sort_by") {
	case "name":
		sort.SliceStable(resolved, func(i, j int) bool {
			return strings.ToLower(stringOrEmpty(resolved[i]["name"])) < strings.ToLower(stringOrEmpty(resolved[j]["name"]))
		})
	case "price_asc":
		sort.SliceStable(resolved, func(i, j int) bool {
			return int64OrZero(resolved[i]["price_cents"]) < int64OrZero(resolved[j]["price_cents"])
		})
	case "price_desc":
		sort.SliceStable(resolved, func(i, j int) bool {
			return int64OrZero(resolved[i]["price_cents"]) > int64OrZero(resolved[j]["price_cents"])
		})
	}

	if int64(len(resolved)) > limit {
		resolved = resolved[:limit]
	}
	data["_resolved_products"] = resolved
	raw, _ := json.Marshal(data)
	bl.DataJson = string(raw)
}

func resolveProductDetailBlock(ctx context.Context, queries *store.Queries, site store.Site, bl *store.Block) {
	var data map[string]any
	if err := json.Unmarshal([]byte(bl.DataJson), &data); err != nil {
		return
	}
	slug := strings.TrimSpace(dataString(data, "product_slug"))
	if slug == "" {
		return
	}
	p, err := queries.GetProductBySlug(ctx, store.GetProductBySlugParams{
		SiteID: site.ID, Slug: slug,
	})
	if err != nil {
		slog.Warn("product_detail: product not found", "slug", slug, "site_id", site.ID)
		data["_resolved_missing"] = true
		raw, _ := json.Marshal(data)
		bl.DataJson = string(raw)
		return
	}
	variants, _ := queries.ListProductVariants(ctx, p.ID)
	variantPayloads := make([]map[string]any, 0, len(variants))
	for _, v := range variants {
		variantPayloads = append(variantPayloads, variantPayload(v, p))
	}
	data["_resolved_product"] = map[string]any{
		"id":             p.ID,
		"name":           p.Name,
		"slug":           p.Slug,
		"description":    p.Description,
		"category":       p.Category,
		"status":         p.Status,
		"base_price":     p.BasePriceCents,
		"currency":       p.Currency,
		"first_image_id": firstImageIDFrom(p.ImagesJson),
		"variants":       variantPayloads,
	}
	raw, _ := json.Marshal(data)
	bl.DataJson = string(raw)
}

// firstImageIDFrom parses the products.images_json (a JSON array of
// media IDs) and returns the first ID or "" when none.
func firstImageIDFrom(imagesJSON string) string {
	imagesJSON = strings.TrimSpace(imagesJSON)
	if imagesJSON == "" || imagesJSON == "null" {
		return ""
	}
	var ids []string
	if err := json.Unmarshal([]byte(imagesJSON), &ids); err == nil && len(ids) > 0 {
		return ids[0]
	}
	// Some legacy rows stored a single string instead of an array.
	var single string
	if err := json.Unmarshal([]byte(imagesJSON), &single); err == nil {
		return single
	}
	return ""
}

func variantPayload(v store.ProductVariant, p store.Product) map[string]any {
	price := v.PriceCents
	if price == 0 {
		price = p.BasePriceCents
	}
	return map[string]any{
		"id":              v.ID,
		"product_id":      v.ProductID,
		"sku":             v.Sku,
		"name":            v.Name,
		"price_cents":     price,
		"currency":        p.Currency,
		"inventory_count": v.InventoryCount,
		"allow_backorder": v.AllowBackorder,
	}
}

// formatPriceForServerRender returns a non-localised "12.34 EUR" string
// for product cards rendered at build time. The cart island re-formats
// prices using Intl.NumberFormat on the client side when a locale is
// configured; the server fallback stays locale-neutral.
func formatPriceForServerRender(cents int64, currency string) string {
	if currency == "" {
		currency = "EUR"
	}
	return fmt.Sprintf("%s %s", formatCents(cents), currency)
}

func formatCents(cents int64) string {
	negative := cents < 0
	if negative {
		cents = -cents
	}
	whole := cents / 100
	frac := cents % 100
	s := fmt.Sprintf("%d.%02d", whole, frac)
	if negative {
		s = "-" + s
	}
	return s
}

func stringOrEmpty(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func int64OrZero(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}

func boolFromData(data map[string]any, key string, def bool) bool {
	switch v := data[key].(type) {
	case bool:
		return v
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		if s == "true" || s == "1" || s == "yes" || s == "on" {
			return true
		}
		if s == "false" || s == "0" || s == "no" || s == "off" {
			return false
		}
	case float64:
		return v != 0
	}
	return def
}

// cartItemSnapshot builds the JSON payload the cart island reads from a
// button's data-cart-item attribute. Keep field names in lockstep with
// CheckoutInput in handlers/orders.go so the cart can submit verbatim.
func cartItemSnapshot(p map[string]any, v map[string]any, imageURL string) string {
	snap := map[string]any{
		"variant_id":  stringOrEmpty(v["id"]),
		"product_id":  stringOrEmpty(v["product_id"]),
		"name":        stringOrEmpty(p["name"]),
		"price_cents": int64OrZero(v["price_cents"]),
		"currency":    stringOrEmpty(v["currency"]),
		"qty":         1,
	}
	if vn := stringOrEmpty(v["name"]); vn != "" {
		snap["variant_name"] = vn
	}
	if imageURL != "" {
		snap["image_url"] = imageURL
	}
	raw, _ := json.Marshal(snap)
	return string(raw)
}

func mediaPublicURL(mediaID string, mediaByID map[string]store.Medium) string {
	if mediaID == "" {
		return ""
	}
	if m, ok := mediaByID[mediaID]; ok && m.ID != "" {
		return fmt.Sprintf("/media/%s/original.%s", m.ID, mediaExt(m))
	}
	return fmt.Sprintf("/media/%s/original.png", mediaID)
}

func mediaExt(m store.Medium) string {
	if m.MimeType == "" {
		return "png"
	}
	switch m.MimeType {
	case "image/jpeg", "image/jpg":
		return "jpg"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	case "image/svg+xml":
		return "svg"
	}
	return "png"
}

// renderProductGridBlock emits a static grid of product cards. Reads
// _resolved_products written by resolveProductGridBlock.
func renderProductGridBlock(data map[string]any, mediaByID map[string]store.Medium) string {
	var b strings.Builder
	cols := dataString(data, "columns")
	if cols == "" {
		cols = "3"
	}
	ctaLabel := dataString(data, "cta_label")
	if ctaLabel == "" {
		ctaLabel = "Add to cart"
	}
	linkToDetail := boolFromData(data, "link_to_detail", true)

	b.WriteString(fmt.Sprintf("  <section class=\"block block--product_grid block--product_grid--cols-%s\">\n", escapeAttrForAstro(cols)))
	if heading := dataString(data, "heading"); heading != "" {
		b.WriteString(fmt.Sprintf("    <h2>%s</h2>\n", escapeTextForAstro(heading)))
	}
	if sub := dataString(data, "subheading"); sub != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"subheading\">%s</p>\n", escapeTextForAstro(sub)))
	}

	resolved, _ := data["_resolved_products"].([]any)
	if len(resolved) == 0 {
		b.WriteString("    <p class=\"empty\">No products yet.</p>\n")
		b.WriteString("  </section>\n")
		return b.String()
	}

	b.WriteString("    <ul class=\"product-grid\">\n")
	for _, raw := range resolved {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name := stringOrEmpty(p["name"])
		slug := stringOrEmpty(p["slug"])
		price := int64OrZero(p["price_cents"])
		currency := stringOrEmpty(p["currency"])
		imageID := stringOrEmpty(p["first_image_id"])
		imageURL := mediaPublicURL(imageID, mediaByID)
		variant, _ := p["first_variant"].(map[string]any)
		snapshot := ""
		if variant != nil {
			snapshot = cartItemSnapshot(p, variant, imageURL)
		}
		b.WriteString("      <li class=\"product-card\">\n")
		if imageID != "" {
			imgHTML := renderMediaImg(imageID, name, name, "product-card-img", mediaByID)
			if linkToDetail && slug != "" {
				b.WriteString(fmt.Sprintf("        <a class=\"product-card-imglink\" href=\"/products/%s\">%s</a>\n", escapeAttrForAstro(slug), imgHTML))
			} else {
				b.WriteString("        " + imgHTML + "\n")
			}
		}
		if linkToDetail && slug != "" {
			b.WriteString(fmt.Sprintf("        <a class=\"product-card-title\" href=\"/products/%s\"><h3>%s</h3></a>\n", escapeAttrForAstro(slug), escapeTextForAstro(name)))
		} else {
			b.WriteString(fmt.Sprintf("        <h3 class=\"product-card-title\">%s</h3>\n", escapeTextForAstro(name)))
		}
		b.WriteString(fmt.Sprintf("        <p class=\"product-card-price\">%s</p>\n", escapeTextForAstro(formatPriceForServerRender(price, currency))))
		if snapshot != "" {
			b.WriteString(fmt.Sprintf("        <button type=\"button\" class=\"btn btn-primary product-card-cta\" data-cart-add data-cart-item=\"%s\">%s</button>\n", escapeAttrForAstro(snapshot), escapeTextForAstro(ctaLabel)))
		} else {
			b.WriteString("        <p class=\"product-card-unavailable\">Currently unavailable.</p>\n")
		}
		b.WriteString("      </li>\n")
	}
	b.WriteString("    </ul>\n  </section>\n")
	return b.String()
}

// renderProductDetailBlock renders a single product. Reads
// _resolved_product written by resolveProductDetailBlock. When the
// product is missing the block emits an editor-visible notice rather
// than blanking the page.
func renderProductDetailBlock(data map[string]any, mediaByID map[string]store.Medium) string {
	var b strings.Builder
	if missing, _ := data["_resolved_missing"].(bool); missing {
		slug := escapeTextForAstro(dataString(data, "product_slug"))
		b.WriteString(fmt.Sprintf("  <section class=\"block block--product_detail block--product_detail--missing\">\n    <p class=\"empty\">Product not found: <code>%s</code></p>\n  </section>\n", slug))
		return b.String()
	}
	p, ok := data["_resolved_product"].(map[string]any)
	if !ok {
		b.WriteString("  <section class=\"block block--product_detail block--product_detail--missing\">\n    <p class=\"empty\">Configure a product slug.</p>\n  </section>\n")
		return b.String()
	}
	ctaLabel := dataString(data, "cta_label")
	if ctaLabel == "" {
		ctaLabel = "Add to cart"
	}
	layout := dataString(data, "gallery_layout")
	if layout == "" {
		layout = "side-by-side"
	}
	showVariants := boolFromData(data, "show_variants", true)

	name := stringOrEmpty(p["name"])
	desc := stringOrEmpty(p["description"])
	currency := stringOrEmpty(p["currency"])
	imageID := stringOrEmpty(p["first_image_id"])
	imageURL := mediaPublicURL(imageID, mediaByID)
	variantsRaw, _ := p["variants"].([]any)

	variants := make([]map[string]any, 0, len(variantsRaw))
	for _, vr := range variantsRaw {
		if vm, ok := vr.(map[string]any); ok {
			variants = append(variants, vm)
		}
	}

	defaultVariant := map[string]any{}
	if len(variants) > 0 {
		defaultVariant = variants[0]
	} else {
		defaultVariant = map[string]any{
			"id":          "",
			"product_id":  p["id"],
			"price_cents": p["base_price"],
			"currency":    currency,
		}
	}
	displayPrice := int64OrZero(defaultVariant["price_cents"])
	snapshot := cartItemSnapshot(p, defaultVariant, imageURL)

	b.WriteString(fmt.Sprintf("  <section class=\"block block--product_detail block--product_detail--%s\" data-product-detail data-product-slug=\"%s\">\n", escapeAttrForAstro(layout), escapeAttrForAstro(stringOrEmpty(p["slug"]))))
	b.WriteString("    <div class=\"product-detail-gallery\">\n")
	if imageID != "" {
		b.WriteString("      " + renderMediaImgWithPriority(imageID, name, name, "product-detail-img", mediaByID, true) + "\n")
	}
	b.WriteString("    </div>\n")
	b.WriteString("    <div class=\"product-detail-body\">\n")
	b.WriteString(fmt.Sprintf("      <h1>%s</h1>\n", escapeTextForAstro(name)))
	b.WriteString(fmt.Sprintf("      <p class=\"product-detail-price\" data-product-price>%s</p>\n", escapeTextForAstro(formatPriceForServerRender(displayPrice, currency))))
	if desc != "" {
		b.WriteString(fmt.Sprintf("      <div class=\"product-detail-description\"><p>%s</p></div>\n", escapeTextForAstro(desc)))
	}
	if showVariants && len(variants) > 1 {
		b.WriteString("      <fieldset class=\"product-detail-variants\">\n        <legend>Options</legend>\n")
		for i, v := range variants {
			vsnap := cartItemSnapshot(p, v, imageURL)
			vID := escapeAttrForAstro(stringOrEmpty(v["id"]))
			label := stringOrEmpty(v["name"])
			if label == "" {
				label = "Option " + fmt.Sprintf("%d", i+1)
			}
			checked := ""
			if i == 0 {
				checked = " checked"
			}
			b.WriteString(fmt.Sprintf("        <label class=\"variant-option\"><input type=\"radio\" name=\"variant\" value=\"%s\" data-variant-pick data-cart-item=\"%s\"%s /> %s</label>\n", vID, escapeAttrForAstro(vsnap), checked, escapeTextForAstro(label)))
		}
		b.WriteString("      </fieldset>\n")
	}
	b.WriteString(fmt.Sprintf("      <button type=\"button\" class=\"btn btn-primary product-detail-cta\" data-cart-add data-cart-item=\"%s\">%s</button>\n", escapeAttrForAstro(snapshot), escapeTextForAstro(ctaLabel)))
	b.WriteString("    </div>\n  </section>\n")
	return b.String()
}

// renderCartDrawerBlock emits the floating cart trigger + the hidden
// <dialog> shell that the storefront island populates on open.
func renderCartDrawerBlock(data map[string]any) string {
	triggerLabel := dataString(data, "trigger_label")
	if triggerLabel == "" {
		triggerLabel = "Cart"
	}
	emptyMessage := dataString(data, "empty_message")
	if emptyMessage == "" {
		emptyMessage = "Your cart is empty."
	}
	checkoutURL := dataString(data, "checkout_url")
	if checkoutURL == "" {
		checkoutURL = "/checkout"
	}
	locale := dataString(data, "currency_locale")

	var b strings.Builder
	b.WriteString("  <div class=\"block block--cart_drawer\">\n")
	b.WriteString(fmt.Sprintf("    <button type=\"button\" class=\"cart-trigger\" data-cart-open aria-label=\"%s\">\n", escapeAttrForAstro(triggerLabel)))
	b.WriteString(fmt.Sprintf("      <span class=\"cart-trigger-label\">%s</span>\n", escapeTextForAstro(triggerLabel)))
	b.WriteString("      <span class=\"cart-trigger-count\" data-cart-count hidden></span>\n")
	b.WriteString("    </button>\n")
	b.WriteString(fmt.Sprintf("    <dialog class=\"cart-drawer\" data-cart-drawer data-currency-locale=\"%s\">\n", escapeAttrForAstro(locale)))
	b.WriteString("      <header class=\"cart-drawer-header\">\n")
	b.WriteString("        <h2>Cart</h2>\n")
	b.WriteString("        <button type=\"button\" class=\"cart-drawer-close\" data-cart-close aria-label=\"Close cart\">&times;</button>\n")
	b.WriteString("      </header>\n")
	b.WriteString(fmt.Sprintf("      <p class=\"cart-drawer-empty\" data-cart-empty>%s</p>\n", escapeTextForAstro(emptyMessage)))
	b.WriteString("      <ul class=\"cart-drawer-items\" data-cart-items hidden></ul>\n")
	b.WriteString("      <footer class=\"cart-drawer-footer\">\n")
	b.WriteString("        <p class=\"cart-drawer-subtotal\"><span>Subtotal</span><span data-cart-total></span></p>\n")
	b.WriteString(fmt.Sprintf("        <a class=\"btn btn-primary cart-drawer-checkout\" href=\"%s\" data-cart-checkout hidden>Checkout</a>\n", escapeAttrForAstro(checkoutURL)))
	b.WriteString("      </footer>\n")
	b.WriteString("    </dialog>\n  </div>\n")
	return b.String()
}

// renderCheckoutFormBlock emits the visitor-facing checkout form. The
// storefront island intercepts submit, reads the cart from
// localStorage, POSTs to /api/sites/{siteID}/checkout, and redirects
// to the Mollie checkout_url returned by the server. siteID is
// embedded via data-site-id at render time.
func renderCheckoutFormBlock(data map[string]any, siteID string) string {
	heading := dataString(data, "heading")
	if heading == "" {
		heading = "Checkout"
	}
	subheading := dataString(data, "subheading")
	returnURL := dataString(data, "return_url")
	submitLabel := dataString(data, "submit_label")
	if submitLabel == "" {
		submitLabel = "Pay now"
	}
	requirePhone := boolFromData(data, "require_phone", false)
	requireShipping := boolFromData(data, "require_shipping_address", true)
	shippingFlag := "0"
	if requireShipping {
		shippingFlag = "1"
	}

	var b strings.Builder
	b.WriteString("  <section class=\"block block--checkout_form\">\n")
	b.WriteString(fmt.Sprintf("    <h2>%s</h2>\n", escapeTextForAstro(heading)))
	if subheading != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"subheading\">%s</p>\n", escapeTextForAstro(subheading)))
	}
	b.WriteString(fmt.Sprintf("    <form class=\"checkout-form\" data-checkout-form data-site-id=\"%s\" data-return-url=\"%s\" data-require-shipping=\"%s\" novalidate>\n",
		escapeAttrForAstro(siteID), escapeAttrForAstro(returnURL), shippingFlag))
	b.WriteString("      <div class=\"checkout-field\"><label for=\"checkout-name\">Full name</label><input id=\"checkout-name\" name=\"name\" type=\"text\" autocomplete=\"name\" required /></div>\n")
	b.WriteString("      <div class=\"checkout-field\"><label for=\"checkout-email\">Email</label><input id=\"checkout-email\" name=\"email\" type=\"email\" autocomplete=\"email\" required /></div>\n")
	if requirePhone {
		b.WriteString("      <div class=\"checkout-field\"><label for=\"checkout-phone\">Phone</label><input id=\"checkout-phone\" name=\"phone\" type=\"tel\" autocomplete=\"tel\" required /></div>\n")
	} else {
		b.WriteString("      <div class=\"checkout-field\"><label for=\"checkout-phone\">Phone (optional)</label><input id=\"checkout-phone\" name=\"phone\" type=\"tel\" autocomplete=\"tel\" /></div>\n")
	}
	if requireShipping {
		b.WriteString("      <fieldset class=\"checkout-shipping\"><legend>Shipping address</legend>\n")
		b.WriteString("        <div class=\"checkout-field\"><label for=\"checkout-street\">Street</label><input id=\"checkout-street\" name=\"ship_street\" type=\"text\" autocomplete=\"street-address\" required /></div>\n")
		b.WriteString("        <div class=\"checkout-field\"><label for=\"checkout-city\">City</label><input id=\"checkout-city\" name=\"ship_city\" type=\"text\" autocomplete=\"address-level2\" required /></div>\n")
		b.WriteString("        <div class=\"checkout-field\"><label for=\"checkout-postal\">Postal code</label><input id=\"checkout-postal\" name=\"ship_postal_code\" type=\"text\" autocomplete=\"postal-code\" required /></div>\n")
		b.WriteString("        <div class=\"checkout-field\"><label for=\"checkout-country\">Country (ISO 2)</label><input id=\"checkout-country\" name=\"ship_country\" type=\"text\" autocomplete=\"country\" maxlength=\"2\" required /></div>\n")
		b.WriteString("      </fieldset>\n")
	}
	b.WriteString("      <div class=\"checkout-field\"><label for=\"checkout-discount\">Discount code (optional)</label><input id=\"checkout-discount\" name=\"discount_code\" type=\"text\" autocapitalize=\"characters\" /></div>\n")
	b.WriteString("      <p class=\"checkout-error\" data-checkout-error hidden></p>\n")
	b.WriteString(fmt.Sprintf("      <button type=\"submit\" class=\"btn btn-primary checkout-submit\">%s</button>\n", escapeTextForAstro(submitLabel)))
	b.WriteString("    </form>\n  </section>\n")
	return b.String()
}

// hasStorefrontBlock returns true when any block on the page uses one
// of the Sprint 2C storefront block types. Used by the layout writer
// to decide whether to inject the storefront island script.
func hasStorefrontBlock(blocks []store.Block) bool {
	for _, bl := range blocks {
		switch bl.BlockType {
		case "product_grid", "product_detail", "cart_drawer", "checkout_form":
			return true
		}
	}
	return false
}
