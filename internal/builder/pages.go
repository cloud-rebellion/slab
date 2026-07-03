package builder

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bright-interaction/slab/internal/store"
)

// RenderSingleBlock returns the rendered Astro source for a single block.
// Used by the admin "View source" feature so developers can see the exact
// code each block produces without triggering a full build. The output is
// the same string the build pipeline writes to disk for this block.
func RenderSingleBlock(ctx context.Context, queries *store.Queries, siteID, blockID string) (string, error) {
	block, err := queries.GetBlockByID(ctx, blockID)
	if err != nil {
		return "", fmt.Errorf("get block: %w", err)
	}
	components, _ := queries.ListComponentsBySite(ctx, siteID)
	componentExts := make(map[string]string, len(components))
	for _, c := range components {
		componentExts[c.Name] = pickComponentExt(c.Template)
	}
	mediaByID := loadBlockMedia(ctx, queries, []store.Block{block})
	return renderBlock(block, componentExts, mediaByID, siteID), nil
}

// RenderBlockDraft renders a block using draft data (not the saved
// data_json). Used by the live preview endpoint so the iframe reflects
// in-progress edits before save. blockType + dataJSON come from the
// editor; siteID resolves the component allowlist + media lookup.
func RenderBlockDraft(ctx context.Context, queries *store.Queries, siteID, blockType, dataJSON string) (string, error) {
	if dataJSON == "" {
		dataJSON = "{}"
	}
	components, _ := queries.ListComponentsBySite(ctx, siteID)
	componentExts := make(map[string]string, len(components))
	for _, c := range components {
		componentExts[c.Name] = pickComponentExt(c.Template)
	}
	draft := store.Block{
		BlockType: blockType,
		DataJson:  dataJSON,
		StyleJson: "{}",
		IsVisible: 1,
	}
	mediaByID := loadBlockMedia(ctx, queries, []store.Block{draft})
	return renderBlock(draft, componentExts, mediaByID, siteID), nil
}

// RenderPagePreview returns the rendered Astro source for a single page.
// Same output renderPageWithContext produces during a build, but
// materialised in-memory so the admin "View source" dialog can show the
// assembled page without triggering bun build. Bypasses status filters
// so drafts also render. Loads the full pageRenderContext so the preview
// reflects current meta-template + hreflang settings.
func RenderPagePreview(ctx context.Context, queries *store.Queries, siteID, pageID string) (string, error) {
	page, err := queries.GetPageByID(ctx, pageID)
	if err != nil {
		return "", fmt.Errorf("get page: %w", err)
	}
	if page.SiteID != siteID {
		return "", fmt.Errorf("page does not belong to site")
	}
	blocks, err := queries.ListBlocksByPage(ctx, pageID)
	if err != nil {
		return "", fmt.Errorf("list blocks: %w", err)
	}
	components, _ := queries.ListComponentsBySite(ctx, siteID)
	componentExts := make(map[string]string, len(components))
	for _, c := range components {
		componentExts[c.Name] = pickComponentExt(c.Template)
	}
	site, err := queries.GetSiteByID(ctx, siteID)
	if err != nil {
		return "", fmt.Errorf("get site: %w", err)
	}
	settings, _ := queries.ListSettingsBySite(ctx, siteID)
	sm := make(map[string]string, len(settings))
	for _, s := range settings {
		sm[s.Category+"."+s.Key] = s.Value
	}
	i18n, _ := LoadI18nConfig(ctx, queries, siteID)
	pageCtx := pageRenderContext{
		site:     site,
		i18n:     i18n,
		titleTpl: sm["seo.meta_title_template"],
		descTpl:  sm["seo.meta_description_template"],
	}
	return renderPageWithContext(page, blocks, componentExts, pageCtx), nil
}

// RenderPages generates .astro page files from published pages and their blocks.
func RenderPages(ctx context.Context, queries *store.Queries, siteID string, wsDir string) (int, error) {
	pages, err := queries.ListPublishedPagesBySite(ctx, siteID)
	if err != nil {
		return 0, fmt.Errorf("list pages: %w", err)
	}

	components, _ := queries.ListComponentsBySite(ctx, siteID)
	componentExts := make(map[string]string)
	for _, c := range components {
		componentExts[c.Name] = pickComponentExt(c.Template)
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

	pageCtx := pageRenderContext{
		site:     site,
		i18n:     i18n,
		titleTpl: sm["seo.meta_title_template"],
		descTpl:  sm["seo.meta_description_template"],
	}

	written := 0
	for _, page := range pages {
		blocks, err := queries.ListBlocksByPage(ctx, page.ID)
		if err != nil {
			return 0, fmt.Errorf("list blocks for page %s: %w", page.Slug, err)
		}
		slog.Info("build: rendering page", "slug", page.Slug, "blocks", len(blocks))

		// Pre-fetch every media row referenced by blocks so the renderer can
		// emit a proper <picture> with WebP source + width/height instead of
		// a bare <img src="/media/<id>"> that 404s. Lookup once per page,
		// pass the map into renderPageWithContext via pageCtx.
		mediaByID := loadBlockMedia(ctx, queries, blocks)
		pageCtx.mediaByID = mediaByID

		// Sprint 4 (2026-05-04): pre-resolve any collection_list blocks
		// by querying their Collection's published items and stuffing
		// them into the block's data_json under "_resolved_items".
		// Keeps renderDataBlock's signature stable while letting the
		// renderer access live data.
		resolveCollectionListBlocks(ctx, queries, site, blocks)

		// Sprint 2 slice C (2026-05-22): pre-resolve product_grid and
		// product_detail blocks the same way (catalog rows + variants
		// stuffed into _resolved_products / _resolved_product).
		resolveStorefrontBlocks(ctx, queries, site, blocks)

		// Sprint 3 (multilingual v1, 2026-05-22): load the page +
		// block locale overlays once per page; build a list of
		// (locale, slug) specs to emit. Single-locale sites get one
		// spec at the base slug; multi-locale sites get the default
		// at /<slug> plus one /<lang>/<slug-or-override> per
		// published page_locale row.
		pageOverlays := loadPageLocaleOverlays(ctx, queries, page.ID)
		blockOverlays := loadBlockLocaleOverlays(ctx, queries, page.ID)
		specs := buildLocaleSpecs(page, i18n.DefaultLang, i18n.AdditionalLangs, pageOverlays)
		for _, spec := range specs {
			localizedBlocks := applyBlockLocaleOverlays(blocks, spec.lang, blockOverlays)
			// Sprint 3 slice B (2026-05-23): resolve any locale_switcher
			// blocks per-spec so each emitted page carries the correct
			// active-locale marker + only-real-counterpart links. Cheap
			// no-op on pages without a locale_switcher block.
			localizedBlocks = resolveLocaleSwitcherBlocks(localizedBlocks, spec.lang, site.Domain, page.Slug, i18n)
			content := renderPageWithContext(spec.page, localizedBlocks, componentExts, pageCtx)
			pagePath, err := slugToFilePath(spec.outSlug, wsDir)
			if err != nil {
				return 0, fmt.Errorf("page %s (locale %s): %w", spec.outSlug, spec.lang, err)
			}
			if err := WriteFile(pagePath, content); err != nil {
				return 0, fmt.Errorf("write page %s (locale %s): %w", spec.outSlug, spec.lang, err)
			}
			written++
		}
	}

	return written, nil
}

// loadBlockMedia walks every block's data_json for image_id references and
// returns a map from media id to the full row so the renderer can pick a
// WebP variant + width/height. Empty map when no blocks reference media or
// the lookups fail; the renderer falls back to a bare <img> in that case.
func loadBlockMedia(ctx context.Context, queries *store.Queries, blocks []store.Block) map[string]store.Medium {
	out := make(map[string]store.Medium)
	for _, bl := range blocks {
		var data map[string]any
		if err := json.Unmarshal([]byte(bl.DataJson), &data); err != nil {
			continue
		}
		if id, ok := data["image_id"].(string); ok && id != "" {
			if _, dup := out[id]; dup {
				continue
			}
			if m, err := queries.GetMediaByID(ctx, id); err == nil {
				out[id] = m
			}
		}
	}
	return out
}

// pageRenderContext bundles everything needed to render one page that
// isn't on the page row itself: the parent site, the i18n config, and
// the seo template strings. RenderPages computes this once per build
// and passes a copy to every renderPageWithContext call so renderPage
// stays cheap and free of context.Context plumbing.
type pageRenderContext struct {
	site      store.Site
	i18n      I18nConfig
	titleTpl  string
	descTpl   string
	mediaByID map[string]store.Medium
}

// renderPage is the legacy entry point used by the standalone preview
// helpers (RenderSingleBlock / RenderPagePreview). It renders a page
// without site-level context (no meta-template expansion, no hreflang).
// The build pipeline calls renderPageWithContext for the full output.
func renderPage(page store.Page, blocks []store.Block, components map[string]string) string {
	return renderPageWithContext(page, blocks, components, pageRenderContext{})
}

// renderPageWithContext builds the page Astro source with the full
// site-level rendering context: meta-title/description templates,
// hreflang alternates, canonical override. Empty pageRenderContext
// degrades to the legacy behaviour for the preview helpers.
func renderPageWithContext(page store.Page, blocks []store.Block, components map[string]string, ctx pageRenderContext) string {
	var b strings.Builder

	// Depth-aware import prefix. Pages live under src/pages/{slug}.astro;
	// nested slugs like /tjanster/avtal land at src/pages/tjanster/avtal.astro
	// which needs ../../components/ to climb back to src/. Without this,
	// every page deeper than one level fails astro build with "could not
	// resolve '../components/foo.astro'".
	prefix := strings.Repeat("../", pageDepth(page.Slug)+1)

	// Frontmatter
	b.WriteString("---\n")
	b.WriteString(fmt.Sprintf("import Base from '%slayouts/Base.astro';\n", prefix))

	// Collect component imports. Map value is the component's file extension
	// (".astro" or ".svelte") so the import statement points at the right
	// file. Svelte components compile via @astrojs/svelte at build time.
	imports := make(map[string]string)
	for _, bl := range blocks {
		if bl.IsVisible == 0 {
			continue
		}
		compName := extractComponentName(bl)
		if compName == "" {
			continue
		}
		if ext, ok := components[compName]; ok {
			imports[compName] = ext
		}
	}
	for name, ext := range imports {
		b.WriteString(fmt.Sprintf("import %s from '%scomponents/%s%s';\n", pascalCase(name), prefix, name, ext))
	}

	b.WriteString("---\n\n")

	// Page wrapper. Title + description run through the operator-set
	// templates from seo.meta_title_template / seo.meta_description_template
	// when those settings are non-empty. Tokens supported:
	// {page_title}, {page_description}, {site_name}, {lang}, {separator}.
	pageTitle := page.MetaTitle
	if pageTitle == "" {
		pageTitle = page.Title
	}
	pageDesc := page.MetaDescription

	vars := MetaTemplateVars{
		PageTitle:       pageTitle,
		PageDescription: pageDesc,
		SiteName:        ctx.site.Name,
		Lang:            ctx.site.Lang,
	}
	title := ExpandMetaTemplate(ctx.titleTpl, pageTitle, vars)
	description := ExpandMetaTemplate(ctx.descTpl, pageDesc, vars)

	b.WriteString(fmt.Sprintf("<Base title=\"%s\"", escapeAttr(title)))
	if description != "" {
		b.WriteString(fmt.Sprintf(" description=\"%s\"", escapeAttr(description)))
	}
	// Per-page lang stamp. Defaults to site.Lang in the layout; override
	// here when the page slug carries a locale prefix the renderer
	// recognises (drives <html lang> + the :lang() CSS that hides the
	// current-locale link in the nav switcher).
	if pageLang := ctx.i18n.LangForSlug(page.Slug); pageLang != "" && pageLang != ctx.site.Lang {
		b.WriteString(fmt.Sprintf(` lang="%s"`, escapeAttr(pageLang)))
	}
	if page.NoIndex == 1 {
		b.WriteString(` robots="noindex, nofollow"`)
	}
	// Per-page suppression of global blocks. Used by landing pages that
	// want to drop the site-wide nav and footer for higher conversion.
	// Layout reads {hideGlobalBlocks} and skips header + footer when true.
	if page.HideGlobalBlocks == 1 {
		b.WriteString(` hideGlobalBlocks={true}`)
	}
	// Hreflang alternates. Multi-language sites get one <link rel="alternate">
	// per locale that has a counterpart page, plus an x-default. Mono-language
	// sites and hreflang_strategy=off drop the prop entirely.
	if alts := ctx.i18n.ComputeAlternates(ctx.site.Domain, page.Slug); len(alts) > 0 {
		b.WriteString(" alternates={")
		b.WriteString(hreflangPropExpression(alts))
		b.WriteString("}")
	}
	b.WriteString(">\n")

	// Render blocks
	for _, bl := range blocks {
		if bl.IsVisible == 0 {
			continue
		}
		b.WriteString(renderBlock(bl, components, ctx.mediaByID, ctx.site.ID))
	}

	b.WriteString("</Base>\n")
	return b.String()
}

func renderBlock(bl store.Block, components map[string]string, mediaByID map[string]store.Medium, siteID string) string {
	var data map[string]any
	if err := json.Unmarshal([]byte(bl.DataJson), &data); err != nil {
		return fmt.Sprintf("  <!-- block %s: invalid data -->\n", bl.ID)
	}

	var inner string
	// Check if this block uses a component
	compName := extractComponentName(bl)
	_, hasComp := components[compName]
	switch {
	case compName != "" && hasComp:
		inner = renderComponentBlock(compName, data)
	case dataHasHTML(data):
		// Raw HTML, sanitized at this render choke point so agent-authored
		// markup cannot inject <script>/event-handlers/javascript: into the
		// built page or the editor preview (both renderers route through here).
		rawHTML, _ := data["html"].(string)
		inner = fmt.Sprintf("  %s\n", sanitizeRawHTML(rawHTML))
	default:
		// Default: render as a section with data-driven content
		inner = renderDataBlock(bl.BlockType, data, mediaByID, siteID)
	}

	// Auto-id: inject id="<slug>" on the <section> opening tag so cross-section
	// anchors like href="#what-i-replace" resolve without per-block manual
	// wiring. Skip when the block already declares an id (e.g. raw HTML).
	if id := sectionIDFromHeading(data); id != "" && strings.HasPrefix(inner, "  <section ") && !strings.Contains(inner[:strings.Index(inner, "\n")], " id=\"") {
		inner = strings.Replace(inner, "  <section ", "  <section id=\""+escapeAttr(id)+"\" ", 1)
	}

	// Phase 18.3: optional per-visitor condition. The hydration script
	// loaded by RenderVisitorHydration walks every [data-asp-when] node
	// and removes `hidden` when the embedded DSL matches. Empty / missing
	// condition renders verbatim, no wrapper.
	if cond, ok := data["condition"].(string); ok && strings.TrimSpace(cond) != "" {
		return fmt.Sprintf("  <div data-asp-when=\"%s\" hidden>\n%s  </div>\n", escapeAttr(cond), inner)
	}
	return inner
}

func dataHasHTML(data map[string]any) bool {
	if v, ok := data["html"]; ok {
		if s, ok := v.(string); ok && s != "" {
			return true
		}
	}
	return false
}

// componentPropKeyRE matches a safe Astro/HTML prop name. Keys outside this
// shape are dropped (not written into the .astro tag) to prevent markup
// injection through a prop key.
var componentPropKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_:-]*$`)

func renderComponentBlock(name string, data map[string]any) string {
	pascal := pascalCase(name)
	props, ok := data["props"].(map[string]any)
	if !ok {
		// Treat the entire data as props
		props = data
		delete(props, "component")
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("  <%s", pascal))
	for k, v := range props {
		// The prop KEY is written into the .astro tag verbatim; an unsafe key
		// (e.g. "onload=x " or "><script>") would inject markup. Only the value
		// was being escaped. Skip keys that aren't a safe attribute/prop name.
		if !componentPropKeyRE.MatchString(k) {
			continue
		}
		switch val := v.(type) {
		case string:
			b.WriteString(fmt.Sprintf(" %s=\"%s\"", k, escapeAttr(val)))
		case float64:
			b.WriteString(fmt.Sprintf(" %s={%g}", k, val))
		case bool:
			if val {
				b.WriteString(fmt.Sprintf(" %s", k))
			}
		default:
			// Complex values passed as JSON expression
			j, _ := json.Marshal(val)
			b.WriteString(fmt.Sprintf(" %s={%s}", k, string(j)))
		}
	}
	b.WriteString(" />\n")
	return b.String()
}

func renderDataBlock(blockType string, data map[string]any, mediaByID map[string]store.Medium, siteID string) string {
	switch blockType {
	case "hero":
		return renderHeroBlock(data, mediaByID)
	case "split_hero":
		return renderSplitHeroBlock(data, mediaByID)
	case "feature_grid":
		return renderFeatureGridBlock(data)
	case "stat_grid":
		return renderStatGridBlock(data)
	case "accordion_faq":
		return renderAccordionFAQBlock(data)
	case "pricing":
		return renderPricingBlock(data)
	case "comparison_table":
		return renderComparisonTableBlock(data)
	case "video_hero":
		return renderVideoHeroBlock(data, mediaByID)
	case "tabs":
		return renderTabsBlock(data, mediaByID)
	case "scroll_reveal":
		return renderScrollRevealBlock(data, mediaByID)
	case "mega_menu":
		return renderMegaMenuBlock(data)
	case "testimonial_wall":
		return renderTestimonialWallBlock(data, mediaByID)
	case "logo_strip":
		return renderLogoStripBlock(data, mediaByID)
	case "logo_carousel":
		return renderLogoCarouselBlock(data, mediaByID)
	case "replacement_grid":
		return renderReplacementGridBlock(data)
	case "process_steps":
		return renderProcessStepsBlock(data)
	case "about_split":
		return renderAboutSplitBlock(data, mediaByID)
	case "custom":
		return renderCustomBlock(data)
	case "raw_astro":
		return renderRawAstroBlock(data)
	case "code_block":
		return renderCodeBlock(data)
	case "form":
		return renderFormBlock(data)
	case "embed":
		return renderEmbedBlock(data)
	case "text":
		return renderTextBlock(data)
	case "cta":
		return renderCTABlock(data)
	case "image":
		return renderImageBlock(data, mediaByID)
	case "quote":
		return renderQuoteBlock(data)
	case "collection_list":
		return renderCollectionListBlock(data, mediaByID)
	case "product_grid":
		return renderProductGridBlock(data, mediaByID)
	case "product_detail":
		return renderProductDetailBlock(data, mediaByID)
	case "cart_drawer":
		return renderCartDrawerBlock(data)
	case "checkout_form":
		return renderCheckoutFormBlock(data, siteID)
	case "locale_switcher":
		return renderLocaleSwitcherBlock(data)
	case "search_box":
		return renderSearchBoxBlock(data)
	default:
		return renderGenericBlock(blockType, data)
	}
}

func dataString(data map[string]any, key string) string {
	if v, ok := data[key].(string); ok {
		return v
	}
	return ""
}

// sectionIDFromHeading turns a heading like "What I replace" into a slug
// "what-i-replace" the renderer can use as an id="..." anchor target. Empty
// when no usable heading is set. Agents writing internal anchors like
// "#what-i-replace" don't need to manually wire anything; the renderer
// adds the id automatically.
func sectionIDFromHeading(data map[string]any) string {
	for _, k := range []string{"id", "section_id", "headline", "heading"} {
		if v := dataString(data, k); v != "" {
			s := transliterateForSlug(strings.ToLower(v))
			s = strings.Map(func(r rune) rune {
				switch {
				case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
					return r
				case r == ' ' || r == '-' || r == '_':
					return '-'
				default:
					return -1
				}
			}, s)
			for strings.Contains(s, "--") {
				s = strings.ReplaceAll(s, "--", "-")
			}
			s = strings.Trim(s, "-")
			if s != "" {
				return s
			}
		}
	}
	return ""
}

// transliterateForSlug folds common non-ASCII characters into their closest
// ASCII equivalent so slugifying produces stable, readable section ids across
// languages (sv, de, da, no, fr, es). Without this, "Vad jag ersätter" would
// drop the ä entirely and become "vad-jag-erstter".
func transliterateForSlug(s string) string {
	repl := []struct{ from, to string }{
		{"ä", "a"}, {"å", "a"}, {"ö", "o"}, {"ø", "o"}, {"æ", "ae"},
		{"ü", "u"}, {"ß", "ss"}, {"é", "e"}, {"è", "e"}, {"ê", "e"},
		{"ë", "e"}, {"á", "a"}, {"à", "a"}, {"â", "a"}, {"ã", "a"},
		{"í", "i"}, {"ì", "i"}, {"î", "i"}, {"ï", "i"}, {"ó", "o"},
		{"ò", "o"}, {"ô", "o"}, {"õ", "o"}, {"ú", "u"}, {"ù", "u"},
		{"û", "u"}, {"ñ", "n"}, {"ç", "c"}, {"ý", "y"}, {"ÿ", "y"},
	}
	for _, r := range repl {
		s = strings.ReplaceAll(s, r.from, r.to)
	}
	return s
}

// renderTextParagraphs splits a multi-line text body on blank lines and emits
// one <p> per paragraph. Single-line breaks within a paragraph become <br>
// so authored line wraps survive without inflating paragraph count.
func renderTextParagraphs(b *strings.Builder, text string) {
	for _, para := range strings.Split(strings.TrimSpace(text), "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		lines := strings.Split(para, "\n")
		var inner strings.Builder
		for i, ln := range lines {
			if i > 0 {
				inner.WriteString("<br>")
			}
			inner.WriteString(escapeHTML(strings.TrimSpace(ln)))
		}
		b.WriteString(fmt.Sprintf("    <p>%s</p>\n", inner.String()))
	}
}

func renderHeroBlock(data map[string]any, mediaByID map[string]store.Medium) string {
	var b strings.Builder
	bg := dataString(data, "bg")
	graphic := dataString(data, "hero_graphic")
	cls := "block block--hero"
	if bg == "circuit" || bg == "circuit-static" {
		cls += " has-circuit-bg"
	}
	if graphic != "" {
		cls += " has-graphic has-graphic--" + escapeAttr(graphic)
	}
	b.WriteString(fmt.Sprintf("  <section class=\"%s\"", cls))
	if bg == "circuit-static" {
		b.WriteString(` data-bg="circuit"`)
	}
	if graphic != "" {
		b.WriteString(fmt.Sprintf(` data-hero-graphic="%s"`, escapeAttr(graphic)))
	}
	b.WriteString(">\n")
	if bg == "circuit" {
		b.WriteString("    <canvas data-circuit-canvas class=\"block-circuit-canvas\" aria-hidden=\"true\"></canvas>\n")
		b.WriteString("    <script>")
		b.Write(CircuitBgScript)
		b.WriteString("</script>\n")
	}
	if graphic != "" {
		b.WriteString("    " + renderHeroGraphic(graphic, data) + "\n")
	}
	if eyebrow := dataString(data, "eyebrow"); eyebrow != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"eyebrow\">%s</p>\n", escapeHTML(eyebrow)))
	}
	if headline := dataString(data, "headline"); headline != "" {
		b.WriteString(fmt.Sprintf("    <h1>%s</h1>\n", renderHeadlineWithAccent(headline, dataString(data, "headline_accent"))))
	}
	if sub := dataString(data, "subheading"); sub != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"subheading\">%s</p>\n", escapeHTML(sub)))
	}
	if imageID := dataString(data, "image_id"); imageID != "" && graphic == "" {
		b.WriteString("    " + renderMediaImgWithPriority(imageID, dataString(data, "image_alt"), dataString(data, "headline"), "hero-image", mediaByID, true) + "\n")
	}
	ctaText := dataString(data, "cta_text")
	secLabel := dataString(data, "secondary_label")
	if ctaText != "" || secLabel != "" {
		b.WriteString("    <div class=\"hero-actions\">\n")
		if ctaText != "" {
			ctaURL := dataString(data, "cta_url")
			if ctaURL == "" {
				ctaURL = "#"
			}
			b.WriteString(fmt.Sprintf("      <a href=\"%s\" class=\"btn-primary\">%s</a>\n",
				escapeURL(ctaURL), escapeHTML(ctaText)))
		}
		if secLabel != "" {
			secURL := dataString(data, "secondary_url")
			if secURL == "" {
				secURL = "#"
			}
			b.WriteString(fmt.Sprintf("      <a href=\"%s\" class=\"btn-secondary\">%s</a>\n",
				escapeURL(secURL), escapeHTML(secLabel)))
		}
		b.WriteString("    </div>\n")
	}
	b.WriteString("  </section>\n")
	return b.String()
}

// renderHeadlineWithAccent emits an h1/h2 inner that supports an optional
// accent fragment shown in --color-primary. Two ways to flag the accent:
// (1) pass a separate `headline_accent` field which gets appended after a
// line break, or (2) wrap an inline span of the headline itself with
// `[[accent]]` markers (unicode-safe, no HTML in the input).
func renderHeadlineWithAccent(headline, accent string) string {
	convertNewlines := func(s string) string {
		// Authors writing multi-line headlines (e.g. "Stop renting\nyour
		// business.\nOwn [[it]].") want each segment on its own visual line
		// like brightinteraction.com. Render each \n as <br>; CSS handles
		// the wrapping otherwise.
		return strings.ReplaceAll(s, "\n", "<br>")
	}
	if strings.Contains(headline, "[[") && strings.Contains(headline, "]]") {
		out := escapeHTML(headline)
		out = strings.ReplaceAll(out, "[[", `<span class="headline-accent">`)
		out = strings.ReplaceAll(out, "]]", `</span>`)
		return convertNewlines(out)
	}
	if accent != "" {
		return fmt.Sprintf("%s<br><span class=\"headline-accent\">%s</span>",
			convertNewlines(escapeHTML(headline)), escapeHTML(accent))
	}
	return convertNewlines(escapeHTML(headline))
}

// renderMediaImg returns a <picture> element when the media row is found in
// mediaByID (with WebP source + width/height + alt + lazy), or a fallback
// <img> with /media/<id>/original.png when the lookup misses. The alt
// argument wins over the medium's stored alt_text; fallbackAlt is used when
// alt is empty (typically the hero headline). Defaults to lazy loading.
func renderMediaImg(mediaID, alt, fallbackAlt, cssClass string, mediaByID map[string]store.Medium) string {
	return renderMediaImgWithPriority(mediaID, alt, fallbackAlt, cssClass, mediaByID, false)
}

// renderMediaImgWithPriority is the LCP-aware variant. priority=true on hero
// blocks emits loading=eager + fetchpriority=high so the largest contentful
// paint is not deferred behind layout-related JS.
func renderMediaImgWithPriority(mediaID, alt, fallbackAlt, cssClass string, mediaByID map[string]store.Medium, priority bool) string {
	if alt == "" {
		alt = fallbackAlt
	}
	if m, ok := mediaByID[mediaID]; ok {
		if alt != "" {
			m.AltText = alt
		}
		out := RenderPictureWithPriority(m, cssClass, priority)
		if out != "" {
			return out
		}
	}
	loadAttrs := `loading="lazy"`
	if priority {
		loadAttrs = `loading="eager" fetchpriority="high"`
	}
	return fmt.Sprintf(`<img src="/media/%s/original.png" alt="%s" %s />`,
		escapeAttr(mediaID), escapeAttr(alt), loadAttrs)
}

func renderFeatureGridBlock(data map[string]any) string {
	var b strings.Builder
	b.WriteString("  <section class=\"block block--feature_grid\">\n")
	if heading := dataString(data, "heading"); heading != "" {
		b.WriteString(fmt.Sprintf("    <h2>%s</h2>\n", escapeHTML(heading)))
	}
	if sub := dataString(data, "subheading"); sub != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"subheading\">%s</p>\n", escapeHTML(sub)))
	}
	if itemsRaw, ok := data["items"].([]any); ok && len(itemsRaw) > 0 {
		b.WriteString("    <ul class=\"feature-grid\">\n")
		for _, it := range itemsRaw {
			item, ok := it.(map[string]any)
			if !ok {
				continue
			}
			b.WriteString("      <li class=\"feature-grid-item\">\n")
			if iconName := dataString(item, "icon"); iconName != "" {
				if svg := renderLucideIcon(iconName); svg != "" {
					b.WriteString("        " + svg + "\n")
				}
			}
			if title := dataString(item, "title"); title != "" {
				b.WriteString(fmt.Sprintf("        <h3>%s</h3>\n", escapeHTML(title)))
			}
			if body := dataString(item, "body"); body != "" {
				b.WriteString(fmt.Sprintf("        <p>%s</p>\n", escapeHTML(body)))
			}
			b.WriteString("      </li>\n")
		}
		b.WriteString("    </ul>\n")
	}
	b.WriteString("  </section>\n")
	return b.String()
}

func renderTextBlock(data map[string]any) string {
	var b strings.Builder
	b.WriteString("  <section class=\"block block--text\">\n")
	if eyebrow := dataString(data, "eyebrow"); eyebrow != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"eyebrow\">%s</p>\n", escapeHTML(eyebrow)))
	}
	if heading := dataString(data, "heading"); heading != "" {
		b.WriteString(fmt.Sprintf("    <h2>%s</h2>\n", escapeHTML(heading)))
	}
	if sub := dataString(data, "subheading"); sub != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"subheading\">%s</p>\n", escapeHTML(sub)))
	}
	if text := dataString(data, "text"); text != "" {
		renderTextParagraphs(&b, text)
	}
	if ctaText := dataString(data, "cta_text"); ctaText != "" {
		ctaURL := dataString(data, "cta_url")
		if ctaURL == "" {
			ctaURL = "#"
		}
		b.WriteString(fmt.Sprintf("    <a href=\"%s\" class=\"btn-primary\">%s</a>\n",
			escapeURL(ctaURL), escapeHTML(ctaText)))
	}
	b.WriteString("  </section>\n")
	return b.String()
}

func renderCTABlock(data map[string]any) string {
	var b strings.Builder
	variant := dataString(data, "variant")
	if variant == "" {
		variant = "primary"
	}
	b.WriteString(fmt.Sprintf("  <section class=\"block block--cta block--cta-%s\">\n", escapeAttr(variant)))
	if heading := dataString(data, "heading"); heading != "" {
		b.WriteString(fmt.Sprintf("    <h2>%s</h2>\n", escapeHTML(heading)))
	}
	if text := dataString(data, "text"); text != "" {
		b.WriteString(fmt.Sprintf("    <p>%s</p>\n", escapeHTML(text)))
	}
	if ctaText := dataString(data, "cta_text"); ctaText != "" {
		ctaURL := dataString(data, "cta_url")
		if ctaURL == "" {
			ctaURL = "#"
		}
		btnClass := "btn-primary"
		if variant == "secondary" {
			btnClass = "btn-secondary"
		}
		b.WriteString(fmt.Sprintf("    <a href=\"%s\" class=\"%s\">%s</a>\n",
			escapeURL(ctaURL), btnClass, escapeHTML(ctaText)))
	}
	b.WriteString("  </section>\n")
	return b.String()
}

func renderImageBlock(data map[string]any, mediaByID map[string]store.Medium) string {
	var b strings.Builder
	b.WriteString("  <section class=\"block block--image\">\n")
	if imageID := dataString(data, "image_id"); imageID != "" {
		b.WriteString("    " + renderMediaImg(imageID, dataString(data, "alt"), "", "", mediaByID) + "\n")
	}
	if caption := dataString(data, "caption"); caption != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"caption\">%s</p>\n", escapeHTML(caption)))
	}
	b.WriteString("  </section>\n")
	return b.String()
}

func renderQuoteBlock(data map[string]any) string {
	var b strings.Builder
	b.WriteString("  <section class=\"block block--quote\">\n")
	if quote := dataString(data, "quote"); quote != "" {
		b.WriteString(fmt.Sprintf("    <blockquote><p>%s</p></blockquote>\n", escapeHTML(quote)))
	}
	if attribution := dataString(data, "attribution"); attribution != "" {
		b.WriteString(fmt.Sprintf("    <cite>%s</cite>\n", escapeHTML(attribution)))
	}
	b.WriteString("  </section>\n")
	return b.String()
}

// renderGenericBlock is the fallback for unknown block types and the legacy
// shape used by tests that pass arbitrary data shapes through. Renders the
// lowest-common-denominator fields (heading / subheading / text / cta).
func renderGenericBlock(blockType string, data map[string]any) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("  <section class=\"block block--%s\">\n", escapeAttr(blockType)))

	if heading, ok := data["heading"].(string); ok {
		b.WriteString(fmt.Sprintf("    <h2>%s</h2>\n", escapeHTML(heading)))
	}
	if subheading, ok := data["subheading"].(string); ok {
		b.WriteString(fmt.Sprintf("    <p>%s</p>\n", escapeHTML(subheading)))
	}
	if text, ok := data["text"].(string); ok {
		b.WriteString(fmt.Sprintf("    <div>%s</div>\n", escapeHTML(text)))
	}
	if ctaText, ok := data["cta_text"].(string); ok {
		ctaURL, _ := data["cta_url"].(string)
		if ctaURL == "" {
			ctaURL = "#"
		}
		b.WriteString(fmt.Sprintf("    <a href=\"%s\" class=\"btn-primary\">%s</a>\n",
			escapeURL(ctaURL), escapeHTML(ctaText)))
	}

	b.WriteString("  </section>\n")
	return b.String()
}

// escapeHTML escapes text content for safe injection into HTML body.
func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// escapeURL sanitizes a URL for href/src attribute use. Rejects javascript: and
// data: schemes, then escapes for attribute context.
func escapeURL(s string) string {
	lower := strings.ToLower(strings.TrimSpace(s))
	// Block dangerous schemes; fall back to a safe anchor.
	for _, bad := range []string{"javascript:", "data:", "vbscript:"} {
		if strings.HasPrefix(lower, bad) {
			return "#"
		}
	}
	return escapeAttr(s)
}

func extractComponentName(bl store.Block) string {
	var data map[string]any
	if err := json.Unmarshal([]byte(bl.DataJson), &data); err != nil {
		return ""
	}
	if comp, ok := data["component"].(string); ok {
		return comp
	}
	return ""
}

// slugToFilePath maps a page slug to its .astro output path and
// guarantees the result stays within <wsDir>/src/pages. A slug that
// escapes (via .. segments or otherwise) returns an error so the build
// fails closed instead of writing another tenant's workspace. Slugs are
// validated at the write boundary (handlers.ValidatePageSlug on the REST,
// MCP, and locale paths); this is the last line of defense.
func slugToFilePath(slug string, wsDir string) (string, error) {
	base := filepath.Join(wsDir, "src", "pages")
	slug = strings.Trim(slug, "/")
	if slug == "" {
		return filepath.Join(base, "index.astro"), nil
	}
	p := filepath.Join(base, slug+".astro")
	rel, err := filepath.Rel(base, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("page slug %q escapes the pages directory", slug)
	}
	return p, nil
}

// pageDepth returns how many subdirectories deep under src/pages the page
// file sits. Used to emit the correct number of ../ in import paths so
// pages at any depth can resolve ../layouts/Base.astro and
// ../components/{name}.astro consistently.
func pageDepth(slug string) int {
	slug = strings.Trim(slug, "/")
	if slug == "" {
		return 0
	}
	return strings.Count(slug, "/")
}

func pascalCase(s string) string {
	parts := strings.Split(s, "-")
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "")
}

func escapeAttr(s string) string {
	// Order matters: & must be first, otherwise we double-escape the entities below.
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	s = strings.ReplaceAll(s, "'", "&#39;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}
