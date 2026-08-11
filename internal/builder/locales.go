package builder

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bright-interaction/slab/internal/store"
)

// Sprint 3 (multilingual v1, 2026-05-22): helpers for emitting one
// HTML file per (page row, locale) combination using the page_locales
// + block_locales overlay tables. When site_locales is empty, the
// build falls back to the existing slug-prefix convention untouched
// (every existing site keeps building byte-identical output).

// localeBuildSpec describes one render of one page in one locale.
// Default locale renders at outSlug=page.Slug, lang=defaultLang.
// Additional locales render at outSlug="<lang>/<override-or-base>"
// and lang=the additional locale.
type localeBuildSpec struct {
	lang    string
	outSlug string
	page    store.Page
}

// loadPageLocaleOverlays returns a map of (locale -> overlay row) for
// the page. Used to walk locales when site_locales has any rows.
func loadPageLocaleOverlays(ctx context.Context, queries *store.Queries, pageID string) map[string]store.PageLocale {
	rows, err := queries.ListPageLocalesByPage(ctx, pageID)
	if err != nil {
		return nil
	}
	out := make(map[string]store.PageLocale, len(rows))
	for _, r := range rows {
		out[strings.ToLower(strings.TrimSpace(r.Locale))] = r
	}
	return out
}

// loadBlockLocaleOverlays returns a map keyed (block_id, locale) ->
// overlay row for every block on the page. One query covers them all.
func loadBlockLocaleOverlays(ctx context.Context, queries *store.Queries, pageID string) map[string]map[string]store.BlockLocale {
	rows, err := queries.ListBlockLocalesByPage(ctx, pageID)
	if err != nil {
		return nil
	}
	out := make(map[string]map[string]store.BlockLocale)
	for _, r := range rows {
		loc := strings.ToLower(strings.TrimSpace(r.Locale))
		if loc == "" {
			continue
		}
		if out[r.BlockID] == nil {
			out[r.BlockID] = make(map[string]store.BlockLocale)
		}
		out[r.BlockID][loc] = r
	}
	return out
}

// buildLocaleSpecs returns the list of per-locale renders to emit for
// one page. Returns one spec when site has no locales configured (the
// default render). Otherwise: one spec for the default locale at the
// base slug + one spec for every additional locale whose page_locales
// row has status='published' (locales without an overlay row are
// skipped so unpublished translations don't accidentally ship).
func buildLocaleSpecs(page store.Page, defaultLang string, additional []string, pageLocaleOverlays map[string]store.PageLocale) []localeBuildSpec {
	if len(additional) == 0 {
		return []localeBuildSpec{{lang: defaultLang, outSlug: page.Slug, page: page}}
	}
	specs := []localeBuildSpec{{lang: defaultLang, outSlug: page.Slug, page: page}}
	for _, lang := range additional {
		overlay, ok := pageLocaleOverlays[lang]
		if !ok {
			// No row for this locale on this page; do not emit.
			continue
		}
		if strings.ToLower(strings.TrimSpace(overlay.Status)) != "published" {
			continue
		}
		// Apply page-level overlay.
		localized := page
		if overlay.Title != "" {
			localized.Title = overlay.Title
		}
		if overlay.MetaTitle != "" {
			localized.MetaTitle = overlay.MetaTitle
		}
		if overlay.MetaDescription != "" {
			localized.MetaDescription = overlay.MetaDescription
		}
		base := strings.TrimSpace(overlay.SlugOverride)
		if base == "" {
			base = strings.TrimPrefix(page.Slug, "/")
		} else {
			base = strings.TrimPrefix(base, "/")
		}
		localized.Slug = strings.TrimSuffix(lang+"/"+base, "/")
		specs = append(specs, localeBuildSpec{lang: lang, outSlug: localized.Slug, page: localized})
	}
	return specs
}

// applyBlockLocaleOverlay returns a shallow copy of the block with
// data_json + is_visible replaced from the per-locale overlay row.
// When no overlay exists for the locale, returns the block unchanged.
// data_json from the overlay is a full JSON object (not a diff): the
// editor writes the entire localized content blob so the renderer
// doesn't have to merge field-by-field.
func applyBlockLocaleOverlay(bl store.Block, locale string, overlays map[string]map[string]store.BlockLocale) store.Block {
	if overlays == nil {
		return bl
	}
	byLocale, ok := overlays[bl.ID]
	if !ok {
		return bl
	}
	overlay, ok := byLocale[locale]
	if !ok {
		return bl
	}
	out := bl
	trimmed := strings.TrimSpace(overlay.DataJson)
	if trimmed != "" && trimmed != "{}" && trimmed != "null" {
		// Validate the overlay JSON parses; if not, fall back to base.
		var probe map[string]any
		if err := json.Unmarshal([]byte(trimmed), &probe); err == nil {
			out.DataJson = trimmed
		} else {
			slog.Warn("block_locales: invalid JSON, falling back to base", "block_id", bl.ID, "locale", locale, "err", err)
		}
	}
	out.IsVisible = overlay.IsVisible
	return out
}

// applyBlockLocaleOverlays returns a new slice with every block
// replaced by its locale-applied variant. Preserves order.
func applyBlockLocaleOverlays(blocks []store.Block, locale string, overlays map[string]map[string]store.BlockLocale) []store.Block {
	if overlays == nil || locale == "" {
		return blocks
	}
	out := make([]store.Block, len(blocks))
	for i, bl := range blocks {
		out[i] = applyBlockLocaleOverlay(bl, locale, overlays)
	}
	return out
}

// resolveLocaleSwitcherBlocks walks the blocks for one (page, locale)
// spec and, for any locale_switcher block, stuffs the locale URL map
// + current-locale flag under data._resolved_locales so the renderer
// can emit a static link list without re-deriving the URL math per
// block. URL computation reuses I18nConfig.ComputeAlternates so the
// switcher's link targets match the hreflang <link> targets exactly.
func resolveLocaleSwitcherBlocks(blocks []store.Block, currentLocale string, siteDomain, basePageSlug string, i18n I18nConfig) []store.Block {
	if len(blocks) == 0 {
		return blocks
	}
	hasSwitcher := false
	for _, bl := range blocks {
		if bl.BlockType == "locale_switcher" {
			hasSwitcher = true
			break
		}
	}
	if !hasSwitcher {
		return blocks
	}
	alternates := i18n.ComputeAlternates(siteDomain, basePageSlug)
	resolved := make([]map[string]any, 0, len(alternates))
	for _, alt := range alternates {
		if alt.Lang == "x-default" {
			continue
		}
		resolved = append(resolved, map[string]any{
			"locale":     alt.Lang,
			"url":        alt.URL,
			"is_current": alt.Lang == currentLocale,
		})
	}
	out := make([]store.Block, len(blocks))
	for i, bl := range blocks {
		if bl.BlockType != "locale_switcher" {
			out[i] = bl
			continue
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(bl.DataJson), &data); err != nil {
			data = map[string]any{}
		}
		data["_resolved_locales"] = resolved
		data["_resolved_current"] = currentLocale
		raw, _ := json.Marshal(data)
		copy := bl
		copy.DataJson = string(raw)
		out[i] = copy
	}
	return out
}

// renderLocaleSwitcherBlock emits the static link list for a
// locale_switcher block. Single-locale sites get nothing visible
// (the resolved list is empty when ComputeAlternates returns nothing).
// CSP-safe: no inline JS. Native <select> dropdown style relies on
// the browser navigating via <form> submit on change (handled by the
// browser's default submit behaviour, no script).
func renderLocaleSwitcherBlock(data map[string]any) string {
	resolved, _ := data["_resolved_locales"].([]any)
	if len(resolved) == 0 {
		return ""
	}
	current, _ := data["_resolved_current"].(string)
	style, _ := data["style"].(string)
	if style == "" {
		style = "inline"
	}
	showLabel := true
	if v, ok := data["show_label"].(bool); ok {
		showLabel = v
	}
	label, _ := data["label"].(string)
	if label == "" {
		label = "Language"
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("  <nav class=\"block block--locale_switcher block--locale_switcher--%s\" aria-label=\"%s\">\n", escapeAttrForAstro(style), escapeAttrForAstro(label)))
	if showLabel {
		b.WriteString(fmt.Sprintf("    <span class=\"locale-switcher-label\">%s</span>\n", escapeTextForAstro(label)))
	}
	switch style {
	case "dropdown":
		// Native <select> wrapped in a no-JS <form>. Submitting the
		// form navigates to the chosen URL because <option value> is
		// a path the browser fetches via the form's GET action. No
		// inline JS, no CSP exemption.
		b.WriteString("    <form class=\"locale-switcher-dropdown\" method=\"get\" data-locale-switcher action=\"\">\n")
		b.WriteString("      <label class=\"sr-only\" for=\"locale-switcher-target\">Choose language</label>\n")
		b.WriteString("      <select id=\"locale-switcher-target\" name=\"_locale\" onchange=\"this.form.action=this.value;this.form.submit()\">\n")
		for _, raw := range resolved {
			m, _ := raw.(map[string]any)
			loc := stringOrEmpty(m["locale"])
			url := stringOrEmpty(m["url"])
			isCurr, _ := m["is_current"].(bool)
			selected := ""
			if isCurr {
				selected = " selected"
			}
			b.WriteString(fmt.Sprintf("        <option value=\"%s\"%s>%s</option>\n", escapeAttrForAstro(url), selected, escapeTextForAstro(loc)))
		}
		b.WriteString("      </select>\n")
		b.WriteString("    </form>\n")
	default:
		// "inline" and "list" both emit a <ul> of links; CSS controls
		// orientation.
		ulClass := "locale-switcher-list"
		if style == "inline" {
			ulClass += " locale-switcher-list--inline"
		}
		b.WriteString(fmt.Sprintf("    <ul class=\"%s\">\n", escapeAttrForAstro(ulClass)))
		for _, raw := range resolved {
			m, _ := raw.(map[string]any)
			loc := stringOrEmpty(m["locale"])
			url := stringOrEmpty(m["url"])
			isCurr, _ := m["is_current"].(bool)
			if isCurr || loc == current {
				b.WriteString(fmt.Sprintf("      <li><span class=\"locale-switcher-current\" aria-current=\"true\">%s</span></li>\n", escapeTextForAstro(loc)))
			} else {
				b.WriteString(fmt.Sprintf("      <li><a href=\"%s\" hreflang=\"%s\">%s</a></li>\n", escapeAttrForAstro(url), escapeAttrForAstro(loc), escapeTextForAstro(loc)))
			}
		}
		b.WriteString("    </ul>\n")
	}
	b.WriteString("  </nav>\n")
	return b.String()
}
