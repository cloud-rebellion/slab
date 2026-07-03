package builder

// Collections renderer (Sprint 4, 2026-05-04).
//
// Emits per-collection index pages + per-item detail pages reusing
// the existing Astro Base.astro layout, slug→file mapping, hreflang
// computation, and personalization DSL. Each item page also carries
// a <script type="application/ld+json"> generated from the
// Collection's settings.schema_org_type.
//
// File layout:
//   /{collection.slug}/                  → index page (lists items)
//   /{collection.slug}/{item.slug}/      → detail page
//   /{locale}/{collection.slug}/{item.slug}/  → locale variant
//
// Items with status != "published" are skipped at render time. Items
// with locale = "" inherit the site default lang. Items whose locale
// matches a configured additional_lang get a localised path AND
// hreflang alternates linking to the variants in other locales.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/bright-interaction/slab/internal/store"
)

// collectionSettings is the shape stored in collections.settings_json.
// All fields optional; zero values mean "use defaults".
type collectionSettings struct {
	RenderAsPages      bool   `json:"render_as_pages"`
	SchemaOrgType      string `json:"schema_org_type"`
	ListPageTemplate   string `json:"list_page_template"`
	DetailPageTemplate string `json:"detail_page_template"`
	ItemsPerIndexPage  int    `json:"items_per_index_page"`
	RequiresMemberRole string `json:"requires_member_role"` // Sprint 5 hook
}

// RenderCollectionPages walks every Collection for the site whose
// settings.render_as_pages is true and emits the index + per-item
// .astro files into the workspace src/pages/ tree. Reuses the Astro
// Base layout and the existing i18n + canonical helpers.
//
// Wired into Build() right after RenderPages.
func RenderCollectionPages(ctx context.Context, queries *store.Queries, siteID, wsDir string) (int, error) {
	collections, err := queries.ListCollectionsBySite(ctx, siteID)
	if err != nil {
		return 0, fmt.Errorf("list collections: %w", err)
	}
	if len(collections) == 0 {
		return 0, nil
	}

	site, err := queries.GetSiteByID(ctx, siteID)
	if err != nil {
		return 0, fmt.Errorf("get site: %w", err)
	}

	settings, _ := queries.ListSettingsBySite(ctx, siteID)
	sm := make(map[string]string, len(settings))
	for _, s := range settings {
		sm[s.Category+"."+s.Key] = s.Value
	}

	i18n, err := LoadI18nConfig(ctx, queries, siteID)
	if err != nil {
		return 0, fmt.Errorf("load i18n config: %w", err)
	}

	pagesEmitted := 0
	for _, c := range collections {
		var cs collectionSettings
		_ = json.Unmarshal([]byte(c.SettingsJson), &cs)
		if !cs.RenderAsPages {
			continue
		}
		if !collectionSlugSafe(c.Slug) {
			slog.Warn("collections: skipping unsafe slug", "site_id", siteID, "slug", c.Slug)
			continue
		}
		var fields []collectionFieldDef
		_ = json.Unmarshal([]byte(c.SchemaJson), &fields)

		items, err := queries.ListPublishedItemsByCollection(ctx, c.ID)
		if err != nil {
			return pagesEmitted, fmt.Errorf("list items %s: %w", c.Slug, err)
		}

		mediaByID := loadCollectionItemMedia(ctx, queries, items)

		// Group items by locale for hreflang lookup.
		bySlugLocale := make(map[string]map[string]string) // slug -> locale -> path
		for _, item := range items {
			if !collectionSlugSafe(item.Slug) {
				continue
			}
			loc := normalizeLocale(item.Locale, site.Lang)
			if _, ok := bySlugLocale[item.Slug]; !ok {
				bySlugLocale[item.Slug] = make(map[string]string)
			}
			bySlugLocale[item.Slug][loc] = collectionItemPath(c.Slug, item.Slug, loc, site.Lang)
		}

		// Index page per locale.
		locales := uniqueLocales(items, site.Lang)
		for _, loc := range locales {
			content := renderCollectionIndex(c, fields, items, mediaByID, site, sm, i18n, loc, cs)
			path := collectionIndexPath(c.Slug, loc, site.Lang, wsDir)
			if err := WriteFile(path, content); err != nil {
				return pagesEmitted, fmt.Errorf("write collection index %s/%s: %w", c.Slug, loc, err)
			}
			pagesEmitted++
		}

		// Detail page per item.
		for _, item := range items {
			if !collectionSlugSafe(item.Slug) {
				continue
			}
			loc := normalizeLocale(item.Locale, site.Lang)
			content := renderCollectionItem(c, fields, item, mediaByID, site, sm, i18n, cs, bySlugLocale[item.Slug])
			rel := collectionItemPath(c.Slug, item.Slug, loc, site.Lang)
			path := filepath.Join(wsDir, "src", "pages", rel+".astro")
			if err := WriteFile(path, content); err != nil {
				return pagesEmitted, fmt.Errorf("write collection item %s/%s: %w", c.Slug, item.Slug, err)
			}
			pagesEmitted++
		}
	}
	return pagesEmitted, nil
}

// collectionFieldDef matches the FieldDef struct declared in the
// handlers package without importing across the layer (the builder
// must remain handler-agnostic). Field names are stable JSON contract.
type collectionFieldDef struct {
	Name          string   `json:"name"`
	Type          string   `json:"type"`
	Label         string   `json:"label"`
	Required      bool     `json:"required"`
	Options       []string `json:"options,omitempty"`
	MaxLength     int      `json:"max_length,omitempty"`
	RefCollection string   `json:"ref_collection,omitempty"`
}

func loadCollectionItemMedia(ctx context.Context, queries *store.Queries, items []store.CollectionItem) map[string]store.Medium {
	out := make(map[string]store.Medium)
	for _, item := range items {
		var data map[string]any
		if err := json.Unmarshal([]byte(item.DataJson), &data); err != nil {
			continue
		}
		// Pull image_id and any image-typed fields by walking values.
		// We do a shallow walk: top-level strings whose values look like
		// IDs and any nested gallery arrays.
		for _, v := range data {
			collectMediaIDs(v, ctx, queries, out)
		}
	}
	return out
}

func collectMediaIDs(v any, ctx context.Context, queries *store.Queries, out map[string]store.Medium) {
	switch val := v.(type) {
	case string:
		// Heuristic: media IDs are 24-char hex (newID format). The
		// renderer falls back to a bare <img> when the lookup misses,
		// so a false-positive lookup costs one DB miss and recovers.
		if len(val) == 24 && isHexLower(val) {
			if _, dup := out[val]; dup {
				return
			}
			if m, err := queries.GetMediaByID(ctx, val); err == nil {
				out[val] = m
			}
		}
	case []any:
		for _, item := range val {
			collectMediaIDs(item, ctx, queries, out)
		}
	}
}

func isHexLower(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// renderCollectionIndex builds the Astro source for a Collection's
// list/index page in one locale. Lists every published item in that
// locale with link to the detail page.
func renderCollectionIndex(c store.Collection, fields []collectionFieldDef, items []store.CollectionItem, mediaByID map[string]store.Medium, site store.Site, settingsMap map[string]string, i18n I18nConfig, locale string, cs collectionSettings) string {
	var b strings.Builder
	prefix := collectionImportPrefix(c.Slug, locale, site.Lang, "")

	b.WriteString("---\n")
	b.WriteString(fmt.Sprintf("import Base from '%slayouts/Base.astro';\n", prefix))
	b.WriteString("---\n\n")

	title := c.Name
	desc := fmt.Sprintf("All %s", strings.ToLower(c.Name))
	b.WriteString(fmt.Sprintf("<Base title=\"%s\" description=\"%s\"", escapeAttr(title), escapeAttr(desc)))
	if locale != "" && locale != site.Lang {
		b.WriteString(fmt.Sprintf(` lang="%s"`, escapeAttr(locale)))
	}
	if alts := computeCollectionIndexAlternates(c.Slug, items, site, i18n); len(alts) > 0 {
		b.WriteString(" alternates={")
		b.WriteString(hreflangPropExpression(alts))
		b.WriteString("}")
	}
	b.WriteString(">\n")

	b.WriteString(fmt.Sprintf("  <section class=\"block block--collection-index\" data-collection=\"%s\">\n", escapeAttr(c.Slug)))
	b.WriteString(fmt.Sprintf("    <h1>%s</h1>\n", escapeHTML(title)))
	b.WriteString("    <ul class=\"collection-list\">\n")
	emitted := 0
	for _, item := range items {
		if !collectionSlugSafe(item.Slug) {
			continue
		}
		loc := normalizeLocale(item.Locale, site.Lang)
		if loc != locale {
			continue
		}
		href := "/" + collectionItemPath(c.Slug, item.Slug, loc, site.Lang) + "/"
		b.WriteString("      <li>\n")
		b.WriteString(fmt.Sprintf("        <a href=\"%s\">%s</a>\n", escapeAttr(href), escapeHTML(item.Title)))
		b.WriteString("      </li>\n")
		emitted++
	}
	if emitted == 0 {
		b.WriteString("      <li>No entries yet.</li>\n")
	}
	b.WriteString("    </ul>\n")
	b.WriteString("  </section>\n")
	b.WriteString("</Base>\n")
	return b.String()
}

// renderCollectionItem builds the Astro source for one item's detail
// page. Includes JSON-LD, hreflang alternates, personalization DSL.
func renderCollectionItem(c store.Collection, fields []collectionFieldDef, item store.CollectionItem, mediaByID map[string]store.Medium, site store.Site, settingsMap map[string]string, i18n I18nConfig, cs collectionSettings, slugLocaleMap map[string]string) string {
	var b strings.Builder
	loc := normalizeLocale(item.Locale, site.Lang)
	prefix := collectionImportPrefix(c.Slug, loc, site.Lang, item.Slug)

	b.WriteString("---\n")
	b.WriteString(fmt.Sprintf("import Base from '%slayouts/Base.astro';\n", prefix))
	b.WriteString("---\n\n")

	var data map[string]any
	_ = json.Unmarshal([]byte(item.DataJson), &data)
	if data == nil {
		data = map[string]any{}
	}

	title := item.Title
	desc := stringField(data, "description", "summary", "lead")

	b.WriteString(fmt.Sprintf("<Base title=\"%s\"", escapeAttr(title)))
	if desc != "" {
		b.WriteString(fmt.Sprintf(" description=\"%s\"", escapeAttr(desc)))
	}
	if loc != "" && loc != site.Lang {
		b.WriteString(fmt.Sprintf(` lang="%s"`, escapeAttr(loc)))
	}
	if alts := computeCollectionItemAlternates(c.Slug, item.Slug, slugLocaleMap, site, i18n); len(alts) > 0 {
		b.WriteString(" alternates={")
		b.WriteString(hreflangPropExpression(alts))
		b.WriteString("}")
	}
	b.WriteString(">\n")

	// Personalization condition wrapper.
	if cond, _ := data["condition"].(string); cond != "" {
		b.WriteString(fmt.Sprintf("  <div data-asp-when=\"%s\" hidden>\n", escapeAttr(cond)))
	}

	b.WriteString(fmt.Sprintf("  <article class=\"block block--collection-item\" data-collection=\"%s\" data-item=\"%s\">\n", escapeAttr(c.Slug), escapeAttr(item.Slug)))
	b.WriteString(fmt.Sprintf("    <h1>%s</h1>\n", escapeHTML(title)))
	if desc != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"lead\">%s</p>\n", escapeHTML(desc)))
	}

	// Render each schema field in declared order.
	for _, f := range fields {
		v, ok := data[f.Name]
		if !ok || v == nil {
			continue
		}
		b.WriteString(renderCollectionFieldHTML(f, v, mediaByID))
	}
	b.WriteString("  </article>\n")

	// JSON-LD.
	if jsonld := buildJSONLD(c, fields, item, data, mediaByID, site); jsonld != "" {
		b.WriteString("  <script type=\"application/ld+json\" set:html={`")
		b.WriteString(jsonld)
		b.WriteString("`}></script>\n")
	}

	if cond, _ := data["condition"].(string); cond != "" {
		_ = cond
		b.WriteString("  </div>\n")
	}
	b.WriteString("</Base>\n")
	return b.String()
}

func renderCollectionFieldHTML(f collectionFieldDef, v any, mediaByID map[string]store.Medium) string {
	var b strings.Builder
	label := f.Label
	if label == "" {
		label = f.Name
	}
	switch f.Type {
	case "text", "email", "url", "color":
		s, _ := v.(string)
		if s == "" {
			return ""
		}
		b.WriteString(fmt.Sprintf("    <p class=\"field field-%s\"><strong>%s:</strong> %s</p>\n", escapeAttr(f.Name), escapeHTML(label), escapeHTML(s)))
	case "textarea", "richtext":
		s, _ := v.(string)
		if s == "" {
			return ""
		}
		b.WriteString(fmt.Sprintf("    <div class=\"field field-%s\">%s</div>\n", escapeAttr(f.Name), escapeHTML(s)))
	case "image":
		id, _ := v.(string)
		if id == "" {
			return ""
		}
		alt := label
		b.WriteString("    " + renderMediaImg(id, alt, alt, "field field-"+f.Name, mediaByID) + "\n")
	case "gallery":
		arr, _ := v.([]any)
		if len(arr) == 0 {
			return ""
		}
		b.WriteString(fmt.Sprintf("    <div class=\"gallery field-%s\">\n", escapeAttr(f.Name)))
		for _, item := range arr {
			id, _ := item.(string)
			if id == "" {
				continue
			}
			b.WriteString("      " + renderMediaImg(id, label, label, "gallery-item", mediaByID) + "\n")
		}
		b.WriteString("    </div>\n")
	case "number":
		b.WriteString(fmt.Sprintf("    <p class=\"field field-%s\"><strong>%s:</strong> %v</p>\n", escapeAttr(f.Name), escapeHTML(label), v))
	case "boolean":
		bv, _ := v.(bool)
		val := "no"
		if bv {
			val = "yes"
		}
		b.WriteString(fmt.Sprintf("    <p class=\"field field-%s\"><strong>%s:</strong> %s</p>\n", escapeAttr(f.Name), escapeHTML(label), val))
	case "date", "datetime":
		s, _ := v.(string)
		if s == "" {
			return ""
		}
		b.WriteString(fmt.Sprintf("    <p class=\"field field-%s\"><strong>%s:</strong> <time datetime=\"%s\">%s</time></p>\n", escapeAttr(f.Name), escapeHTML(label), escapeAttr(s), escapeHTML(s)))
	case "select":
		s, _ := v.(string)
		if s == "" {
			return ""
		}
		b.WriteString(fmt.Sprintf("    <p class=\"field field-%s\"><strong>%s:</strong> %s</p>\n", escapeAttr(f.Name), escapeHTML(label), escapeHTML(s)))
	case "multiselect":
		arr, _ := v.([]any)
		if len(arr) == 0 {
			return ""
		}
		strs := make([]string, 0, len(arr))
		for _, item := range arr {
			if s, ok := item.(string); ok {
				strs = append(strs, s)
			}
		}
		b.WriteString(fmt.Sprintf("    <p class=\"field field-%s\"><strong>%s:</strong> %s</p>\n", escapeAttr(f.Name), escapeHTML(label), escapeHTML(strings.Join(strs, ", "))))
	}
	return b.String()
}

func collectionImportPrefix(collectionSlug, locale, defaultLang, itemSlug string) string {
	depth := 1 // collection slug
	if itemSlug != "" {
		depth++
	}
	if locale != "" && locale != defaultLang {
		depth++
	}
	return strings.Repeat("../", depth+1)
}

// collectionItemPath returns the path component (slug-relative) for
// an item, e.g. "case-studies/acme" or "sv/case-studies/akme". The
// returned string does NOT include leading or trailing slashes.
func collectionItemPath(collectionSlug, itemSlug, locale, defaultLang string) string {
	if locale != "" && locale != defaultLang {
		return locale + "/" + collectionSlug + "/" + itemSlug
	}
	return collectionSlug + "/" + itemSlug
}

func collectionIndexPath(collectionSlug, locale, defaultLang, wsDir string) string {
	if locale != "" && locale != defaultLang {
		return filepath.Join(wsDir, "src", "pages", locale, collectionSlug, "index.astro")
	}
	return filepath.Join(wsDir, "src", "pages", collectionSlug, "index.astro")
}

func collectionSlugSafe(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return false
		}
	}
	return true
}

func normalizeLocale(loc, fallback string) string {
	loc = strings.TrimSpace(loc)
	if loc == "" {
		return fallback
	}
	return loc
}

func uniqueLocales(items []store.CollectionItem, fallback string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, it := range items {
		loc := normalizeLocale(it.Locale, fallback)
		if !seen[loc] {
			seen[loc] = true
			out = append(out, loc)
		}
	}
	if len(out) == 0 {
		return []string{fallback}
	}
	return out
}

func computeCollectionIndexAlternates(collectionSlug string, items []store.CollectionItem, site store.Site, i18n I18nConfig) []HreflangAlternate {
	if strings.ToLower(i18n.Strategy) == "off" {
		return nil
	}
	seen := map[string]bool{}
	out := []HreflangAlternate{}
	for _, it := range items {
		loc := normalizeLocale(it.Locale, site.Lang)
		if seen[loc] {
			continue
		}
		seen[loc] = true
		var rel string
		if loc != "" && loc != site.Lang {
			rel = "/" + loc + "/" + collectionSlug + "/"
		} else {
			rel = "/" + collectionSlug + "/"
		}
		out = append(out, HreflangAlternate{Lang: loc, URL: absoluteURL(site, rel)})
	}
	return out
}

func computeCollectionItemAlternates(collectionSlug, itemSlug string, slugLocaleMap map[string]string, site store.Site, i18n I18nConfig) []HreflangAlternate {
	if strings.ToLower(i18n.Strategy) == "off" {
		return nil
	}
	out := make([]HreflangAlternate, 0, len(slugLocaleMap))
	for loc := range slugLocaleMap {
		rel := "/" + slugLocaleMap[loc] + "/"
		out = append(out, HreflangAlternate{Lang: loc, URL: absoluteURL(site, rel)})
	}
	return out
}

func absoluteURL(site store.Site, rel string) string {
	base := strings.TrimRight(site.Domain, "/")
	if base == "" {
		base = ""
	} else if !strings.Contains(base, "://") {
		base = "https://" + base
	}
	return base + rel
}

// resolveCollectionListBlocks walks the page's blocks and, for any
// `collection_list` block, queries the referenced Collection's
// published items at build time and stuffs them under data._resolved_items
// so the renderer can emit static markup. Mutates blocks in-place.
//
// Errors (collection not found, items query fails, JSON corrupt) are
// logged + skipped: the block falls back to an empty list rather than
// failing the entire build over one stale block.
func resolveCollectionListBlocks(ctx context.Context, queries *store.Queries, site store.Site, blocks []store.Block) {
	for i := range blocks {
		if blocks[i].BlockType != "collection_list" {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(blocks[i].DataJson), &data); err != nil {
			continue
		}
		collectionID, _ := data["collection_id"].(string)
		if collectionID == "" {
			continue
		}
		c, err := queries.GetCollectionByID(ctx, collectionID)
		if err != nil || c.SiteID != site.ID {
			slog.Warn("collection_list: collection not found or wrong site", "collection_id", collectionID, "block_id", blocks[i].ID)
			continue
		}
		items, err := queries.ListPublishedItemsByCollection(ctx, collectionID)
		if err != nil {
			slog.Warn("collection_list: failed to load items", "collection_id", collectionID, "err", err)
			continue
		}

		sortBy, _ := data["sort_by"].(string)
		sortDir, _ := data["sort_dir"].(string)
		items = sortCollectionItems(items, sortBy, sortDir)

		// Apply static filter (personalization DSL evaluated at build time).
		if filter, _ := data["filter"].(string); strings.TrimSpace(filter) != "" {
			items = filterCollectionItemsByDSL(items, filter)
		}

		// Limit. 0 / negative -> default 12; cap at 200.
		limit := numberFromData(data, "limit")
		if limit <= 0 {
			limit = 12
		}
		if limit > 200 {
			limit = 200
		}
		if int(limit) < len(items) {
			items = items[:int(limit)]
		}

		// Project to a slim shape the renderer wants.
		resolved := make([]map[string]any, 0, len(items))
		for _, item := range items {
			var d map[string]any
			_ = json.Unmarshal([]byte(item.DataJson), &d)
			loc := normalizeLocale(item.Locale, site.Lang)
			resolved = append(resolved, map[string]any{
				"id":           item.ID,
				"slug":         item.Slug,
				"title":        item.Title,
				"href":         "/" + collectionItemPath(c.Slug, item.Slug, loc, site.Lang) + "/",
				"data":         d,
				"locale":       loc,
				"published_at": item.PublishedAt,
			})
		}

		data["_resolved_items"] = resolved
		data["_collection_slug"] = c.Slug
		data["_collection_name"] = c.Name
		raw, _ := json.Marshal(data)
		blocks[i].DataJson = string(raw)
	}
}

func numberFromData(data map[string]any, key string) int64 {
	switch n := data[key].(type) {
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	}
	return 0
}

func sortCollectionItems(items []store.CollectionItem, sortBy, sortDir string) []store.CollectionItem {
	out := make([]store.CollectionItem, len(items))
	copy(out, items)
	less := func(i, j int) bool {
		switch sortBy {
		case "title":
			return out[i].Title < out[j].Title
		case "created_at":
			return out[i].CreatedAt < out[j].CreatedAt
		case "published_at":
			return out[i].PublishedAt < out[j].PublishedAt
		default:
			if out[i].SortOrder != out[j].SortOrder {
				return out[i].SortOrder < out[j].SortOrder
			}
			return out[i].PublishedAt < out[j].PublishedAt
		}
	}
	if sortDir == "desc" {
		base := less
		less = func(i, j int) bool { return !base(i, j) }
	}
	// Insertion sort: items per block are small (<=200) so the
	// O(n^2) cost is invisible vs the bun build wall-clock.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && less(j, j-1); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// filterCollectionItemsByDSL evaluates the personalization DSL against
// each item's data_json. Matching items pass; non-matching are
// dropped. Unparseable filter strings are treated as a pass-all.
func filterCollectionItemsByDSL(items []store.CollectionItem, filter string) []store.CollectionItem {
	// Static DSL is intentionally narrow: support only the common
	// `field == "value"` and `field present` forms at build time.
	// Visitor-driven dynamic filters live in data-asp-when on the
	// rendered cards, evaluated by the visitor hydration script.
	out := items[:0:0]
	for _, item := range items {
		var data map[string]any
		_ = json.Unmarshal([]byte(item.DataJson), &data)
		if matchesStaticDSL(filter, data) {
			out = append(out, item)
		}
	}
	return out
}

func matchesStaticDSL(filter string, data map[string]any) bool {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return true
	}
	// `field present`
	if rest, ok := strings.CutSuffix(filter, " present"); ok {
		key := strings.TrimSpace(rest)
		v, present := data[key]
		return present && v != nil && v != ""
	}
	// `field == "value"`
	if i := strings.Index(filter, "=="); i >= 0 {
		key := strings.TrimSpace(filter[:i])
		val := strings.Trim(strings.TrimSpace(filter[i+2:]), `"' `)
		actual, _ := data[key].(string)
		return actual == val
	}
	return true
}

// renderCollectionListBlock emits the static markup for a
// collection_list block. The pre-resolved items live in
// data._resolved_items (added by resolveCollectionListBlocks before
// the renderer runs).
func renderCollectionListBlock(data map[string]any, mediaByID map[string]store.Medium) string {
	var b strings.Builder
	heading, _ := data["heading"].(string)
	subheading, _ := data["subheading"].(string)
	cardTemplate, _ := data["card_template"].(string)
	if cardTemplate == "" {
		cardTemplate = "title_image"
	}
	collectionSlug, _ := data["_collection_slug"].(string)

	b.WriteString(fmt.Sprintf("  <section class=\"block block--collection-list block--collection-list--%s\" data-collection=\"%s\">\n", escapeAttr(cardTemplate), escapeAttr(collectionSlug)))
	if heading != "" {
		b.WriteString(fmt.Sprintf("    <h2>%s</h2>\n", escapeHTML(heading)))
	}
	if subheading != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"subheading\">%s</p>\n", escapeHTML(subheading)))
	}

	resolved, _ := data["_resolved_items"].([]any)
	if len(resolved) == 0 {
		b.WriteString("    <p class=\"empty\">No entries yet.</p>\n")
		b.WriteString("  </section>\n")
		return b.String()
	}

	listClass := "collection-cards"
	switch cardTemplate {
	case "compact":
		listClass = "collection-list--compact"
	case "grid_2":
		listClass = "collection-cards collection-cards--grid-2"
	case "grid_3":
		listClass = "collection-cards collection-cards--grid-3"
	}
	b.WriteString(fmt.Sprintf("    <ul class=\"%s\">\n", escapeAttr(listClass)))
	for _, raw := range resolved {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		title, _ := item["title"].(string)
		href, _ := item["href"].(string)
		idata, _ := item["data"].(map[string]any)
		var imageHTML string
		if idata != nil {
			if id, _ := idata["image_id"].(string); id != "" {
				imageHTML = renderMediaImg(id, title, title, "card-image", mediaByID)
			} else if id, _ := idata["image"].(string); id != "" && len(id) == 24 && isHexLower(id) {
				imageHTML = renderMediaImg(id, title, title, "card-image", mediaByID)
			}
		}
		b.WriteString("      <li class=\"collection-card\">\n")
		switch cardTemplate {
		case "image_title":
			if imageHTML != "" {
				b.WriteString("        " + imageHTML + "\n")
			}
			b.WriteString(fmt.Sprintf("        <a href=\"%s\"><h3>%s</h3></a>\n", escapeAttr(href), escapeHTML(title)))
		case "compact":
			b.WriteString(fmt.Sprintf("        <a href=\"%s\">%s</a>\n", escapeAttr(href), escapeHTML(title)))
		default: // title_image, grid_2, grid_3
			b.WriteString(fmt.Sprintf("        <a href=\"%s\"><h3>%s</h3></a>\n", escapeAttr(href), escapeHTML(title)))
			if imageHTML != "" {
				b.WriteString("        " + imageHTML + "\n")
			}
		}
		if idata != nil {
			if lead, _ := idata["lead"].(string); lead != "" {
				b.WriteString(fmt.Sprintf("        <p class=\"card-lead\">%s</p>\n", escapeHTML(lead)))
			} else if desc, _ := idata["description"].(string); desc != "" {
				b.WriteString(fmt.Sprintf("        <p class=\"card-lead\">%s</p>\n", escapeHTML(desc)))
			}
		}
		b.WriteString("      </li>\n")
	}
	b.WriteString("    </ul>\n")
	b.WriteString("  </section>\n")
	return b.String()
}

func stringField(data map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := data[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}
