package builder

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/slab/internal/store"
)

// Sprint 3 slice A (2026-05-22): 2-locale build smoke test. Verifies
// the multi-locale render loop emits the expected output paths AND
// applies the per-locale overlay content. No assertions on hreflang
// here because that's covered by the existing i18n_test.go suite.

func TestRenderPages_TwoLocalesEmitsBothPaths(t *testing.T) {
	_, q, siteID, wsDir := newBuilderTestDB(t)
	ctx := context.Background()

	// Configure two locales on the site: en (default) + sv.
	if err := q.UpsertSiteLocale(ctx, store.UpsertSiteLocaleParams{
		SiteID: siteID, Locale: "en", IsDefault: 1, SortOrder: 0,
	}); err != nil {
		t.Fatal(err)
	}
	if err := q.UpsertSiteLocale(ctx, store.UpsertSiteLocaleParams{
		SiteID: siteID, Locale: "sv", IsDefault: 0, SortOrder: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// Create one base page.
	pageID := "p_about"
	if err := q.CreatePage(ctx, store.CreatePageParams{
		ID: pageID, SiteID: siteID, Title: "About", Slug: "about",
		Layout: "default", SortOrder: 1, ShowInNav: 1,
	}); err != nil {
		t.Fatal(err)
	}
	// Publish the page so RenderPages picks it up.
	if err := q.UpdatePage(ctx, store.UpdatePageParams{
		ID: pageID, Title: "About", Slug: "about", Status: "published",
		Layout: "default", SortOrder: 1, ShowInNav: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := q.CreateBlock(ctx, store.CreateBlockParams{
		ID: "bl_text", PageID: pageID, BlockType: "text", SortOrder: 1, IsVisible: 1,
		DataJson: `{"text":"Hello English"}`,
	}); err != nil {
		t.Fatal(err)
	}

	// Add a sv overlay: page metadata + block content.
	if err := q.UpsertPageLocale(ctx, store.UpsertPageLocaleParams{
		PageID: pageID, Locale: "sv",
		Title: "Om oss", MetaTitle: "Om oss", MetaDescription: "Svensk beskrivning",
		Status: "published",
	}); err != nil {
		t.Fatal(err)
	}
	if err := q.UpsertBlockLocale(ctx, store.UpsertBlockLocaleParams{
		BlockID: "bl_text", Locale: "sv", DataJson: `{"text":"Hej svenska"}`, IsVisible: 1,
	}); err != nil {
		t.Fatal(err)
	}

	n, err := RenderPages(ctx, q, siteID, wsDir)
	if err != nil {
		t.Fatalf("RenderPages: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 emitted files (en + sv), got %d", n)
	}

	// Default locale at /about.
	enPath := filepath.Join(wsDir, "src", "pages", "about.astro")
	enBytes, err := os.ReadFile(enPath)
	if err != nil {
		t.Fatalf("missing en page at %s: %v", enPath, err)
	}
	if !strings.Contains(string(enBytes), "Hello English") {
		t.Errorf("en page should carry base content; got:\n%s", string(enBytes))
	}
	if strings.Contains(string(enBytes), "Hej svenska") {
		t.Errorf("en page should NOT carry sv overlay; got:\n%s", string(enBytes))
	}

	// Additional locale at /sv/about.
	svPath := filepath.Join(wsDir, "src", "pages", "sv", "about.astro")
	svBytes, err := os.ReadFile(svPath)
	if err != nil {
		t.Fatalf("missing sv page at %s: %v", svPath, err)
	}
	if !strings.Contains(string(svBytes), "Hej svenska") {
		t.Errorf("sv page should carry sv overlay content; got:\n%s", string(svBytes))
	}
}

func TestRenderPages_DraftLocaleNotEmitted(t *testing.T) {
	_, q, siteID, wsDir := newBuilderTestDB(t)
	ctx := context.Background()

	if err := q.UpsertSiteLocale(ctx, store.UpsertSiteLocaleParams{
		SiteID: siteID, Locale: "en", IsDefault: 1, SortOrder: 0,
	}); err != nil {
		t.Fatal(err)
	}
	if err := q.UpsertSiteLocale(ctx, store.UpsertSiteLocaleParams{
		SiteID: siteID, Locale: "sv", IsDefault: 0, SortOrder: 1,
	}); err != nil {
		t.Fatal(err)
	}

	pageID := "p_draft"
	if err := q.CreatePage(ctx, store.CreatePageParams{
		ID: pageID, SiteID: siteID, Title: "Draft", Slug: "draft-locale",
		Layout: "default", SortOrder: 1, ShowInNav: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := q.UpdatePage(ctx, store.UpdatePageParams{
		ID: pageID, Title: "Draft", Slug: "draft-locale", Status: "published",
		Layout: "default", SortOrder: 1, ShowInNav: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// sv overlay exists but is draft, must not emit.
	if err := q.UpsertPageLocale(ctx, store.UpsertPageLocaleParams{
		PageID: pageID, Locale: "sv", Title: "Svensk utkast", Status: "draft",
	}); err != nil {
		t.Fatal(err)
	}

	n, err := RenderPages(ctx, q, siteID, wsDir)
	if err != nil {
		t.Fatalf("RenderPages: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 emitted file (en only), got %d", n)
	}
	svPath := filepath.Join(wsDir, "src", "pages", "sv", "draft-locale.astro")
	if _, err := os.Stat(svPath); !os.IsNotExist(err) {
		t.Errorf("sv draft locale should NOT be emitted at %s", svPath)
	}
}

func TestBuildLocaleSpecs_SlugOverride(t *testing.T) {
	page := store.Page{ID: "p1", Slug: "about", Title: "About"}
	overlays := map[string]store.PageLocale{
		"sv": {PageID: "p1", Locale: "sv", SlugOverride: "om-oss", Status: "published"},
	}
	specs := buildLocaleSpecs(page, "en", []string{"sv"}, overlays)
	if len(specs) != 2 {
		t.Fatalf("expected 2 specs, got %d", len(specs))
	}
	if specs[0].outSlug != "about" || specs[0].lang != "en" {
		t.Errorf("default spec wrong: %+v", specs[0])
	}
	if specs[1].outSlug != "sv/om-oss" || specs[1].lang != "sv" {
		t.Errorf("sv spec should honour slug_override, got: %+v", specs[1])
	}
}

func TestBuildLocaleSpecs_NoOverlayFallbackToBaseSlug(t *testing.T) {
	page := store.Page{ID: "p1", Slug: "contact"}
	overlays := map[string]store.PageLocale{
		"sv": {PageID: "p1", Locale: "sv", Status: "published"},
	}
	specs := buildLocaleSpecs(page, "en", []string{"sv"}, overlays)
	if len(specs) != 2 {
		t.Fatalf("expected 2 specs, got %d", len(specs))
	}
	if specs[1].outSlug != "sv/contact" {
		t.Errorf("sv spec should default to <lang>/<base>, got: %s", specs[1].outSlug)
	}
}

func TestBuildLocaleSpecs_NoLocalesReturnsSingleSpec(t *testing.T) {
	page := store.Page{ID: "p1", Slug: "about"}
	specs := buildLocaleSpecs(page, "en", nil, nil)
	if len(specs) != 1 {
		t.Fatalf("expected 1 spec, got %d", len(specs))
	}
	if specs[0].outSlug != "about" || specs[0].lang != "en" {
		t.Errorf("single-locale spec wrong: %+v", specs[0])
	}
}

func TestApplyBlockLocaleOverlay_UsesOverlayDataJson(t *testing.T) {
	bl := store.Block{ID: "b1", BlockType: "text", DataJson: `{"text":"base"}`, IsVisible: 1}
	overlays := map[string]map[string]store.BlockLocale{
		"b1": {
			"sv": {BlockID: "b1", Locale: "sv", DataJson: `{"text":"sv"}`, IsVisible: 1},
		},
	}
	out := applyBlockLocaleOverlay(bl, "sv", overlays)
	if !strings.Contains(out.DataJson, "sv") {
		t.Errorf("expected sv content, got %s", out.DataJson)
	}
	// Wrong locale falls back to base.
	out2 := applyBlockLocaleOverlay(bl, "de", overlays)
	if !strings.Contains(out2.DataJson, "base") {
		t.Errorf("de fallback should use base, got %s", out2.DataJson)
	}
}

func TestResolveLocaleSwitcherBlocks_StuffsResolvedList(t *testing.T) {
	blocks := []store.Block{
		{ID: "b1", BlockType: "locale_switcher", DataJson: `{"label":"Language"}`, IsVisible: 1},
	}
	i18n := I18nConfig{
		DefaultLang:     "en",
		AdditionalLangs: []string{"sv"},
		Strategy:        "path",
		CanonicalBase:   "https://example.com",
		PageSlugs:       map[string]bool{"/about": true, "/sv/about": true},
	}
	out := resolveLocaleSwitcherBlocks(blocks, "sv", "example.com", "about", i18n)
	if len(out) != 1 {
		t.Fatalf("expected 1 block, got %d", len(out))
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(out[0].DataJson), &data); err != nil {
		t.Fatalf("post-resolve json: %v", err)
	}
	resolved, ok := data["_resolved_locales"].([]any)
	if !ok || len(resolved) < 2 {
		t.Fatalf("expected resolved list with >=2 locales, got %v", data)
	}
	if cur, _ := data["_resolved_current"].(string); cur != "sv" {
		t.Errorf("expected _resolved_current=sv, got %q", cur)
	}
}

func TestRenderLocaleSwitcherBlock_InlineMarksCurrent(t *testing.T) {
	data := map[string]any{
		"style":             "inline",
		"show_label":        false,
		"_resolved_current": "en",
		"_resolved_locales": []any{
			map[string]any{"locale": "en", "url": "https://example.com/", "is_current": true},
			map[string]any{"locale": "sv", "url": "https://example.com/sv/", "is_current": false},
		},
	}
	out := renderLocaleSwitcherBlock(data)
	if !strings.Contains(out, `aria-current="true"`) {
		t.Errorf("expected aria-current on current locale; got:\n%s", out)
	}
	if !strings.Contains(out, `href="https://example.com/sv/"`) {
		t.Errorf("expected sv link emitted; got:\n%s", out)
	}
	if strings.Contains(out, `href="https://example.com/"`) {
		t.Errorf("current locale should NOT be a link; got:\n%s", out)
	}
}

func TestRenderLocaleSwitcherBlock_EmptyResolvedRendersNothing(t *testing.T) {
	out := renderLocaleSwitcherBlock(map[string]any{"_resolved_locales": []any{}})
	if out != "" {
		t.Errorf("expected empty output on monolingual page, got:\n%s", out)
	}
}

func TestApplyBlockLocaleOverlay_InvalidJSONFallsBackToBase(t *testing.T) {
	bl := store.Block{ID: "b1", BlockType: "text", DataJson: `{"text":"base"}`, IsVisible: 1}
	overlays := map[string]map[string]store.BlockLocale{
		"b1": {"sv": {BlockID: "b1", Locale: "sv", DataJson: `{bad json`, IsVisible: 1}},
	}
	out := applyBlockLocaleOverlay(bl, "sv", overlays)
	if !strings.Contains(out.DataJson, "base") {
		t.Errorf("invalid overlay JSON should fall back to base, got %s", out.DataJson)
	}
}
