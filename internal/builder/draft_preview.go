package builder

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/bright-interaction/slab/internal/store"
)

// RenderPageDraft assembles a complete, browser-renderable HTML document
// for a single page using the current draft DB state. NOT a build:
//   - skips bun + astro entirely (no dist write, no compile step)
//   - reuses renderBlock + extractHTMLFromGlobalBlock + BuildCSS, the same
//     primitives the production builder uses, so visual fidelity is high
//   - emits noindex + same-origin-only headers via the calling handler so
//     the document is safe to load via iframe srcdoc inside the admin app
//
// Output is a single self-contained HTML string. No external requests other
// than the site's CSS classes (already inlined) and uploaded fonts. All
// component blocks (Astro `<PascalName />` syntax) degrade to a notice in
// place of a render attempt. Astro compilation is what resolves them, and
// the preview path stops short of that to keep the round-trip fast.
func RenderPageDraft(ctx context.Context, queries *store.Queries, siteID, pageID string) (string, error) {
	page, err := queries.GetPageByID(ctx, pageID)
	if err != nil {
		return "", fmt.Errorf("get page: %w", err)
	}
	if page.SiteID != siteID {
		return "", fmt.Errorf("page does not belong to site")
	}

	site, err := queries.GetSiteByID(ctx, siteID)
	if err != nil {
		return "", fmt.Errorf("get site: %w", err)
	}

	blocks, err := queries.ListBlocksByPage(ctx, pageID)
	if err != nil {
		return "", fmt.Errorf("list blocks: %w", err)
	}

	componentList, _ := queries.ListComponentsBySite(ctx, siteID)
	componentExts := make(map[string]string, len(componentList))
	componentTemplates := make(map[string]string, len(componentList))
	for _, c := range componentList {
		componentExts[c.Name] = pickComponentExt(c.Template)
		componentTemplates[c.Name] = c.Template
	}

	mediaByID := loadBlockMedia(ctx, queries, blocks)

	var blocksHTML strings.Builder
	for _, bl := range blocks {
		if bl.IsVisible == 0 {
			continue
		}
		// Component blocks need Astro to compile their `<PascalName />` syntax
		// for production, but the preview short-circuits Astro to keep the
		// round-trip fast. Inline-render the registered template by parsing
		// the frontmatter for destructured props and substituting them into
		// the body. This makes the preview match the deployed output for the
		// component classes we author (no JSX expressions beyond simple prop
		// references + optional `|| "default"` fallbacks).
		if compName := extractComponentName(bl); compName != "" {
			if tpl, ok := componentTemplates[compName]; ok {
				html, ok := renderRegisteredComponentInline(tpl, bl.DataJson)
				if ok {
					blocksHTML.WriteString(html)
					continue
				}
			}
		}
		raw := renderBlock(bl, componentExts, mediaByID, siteID)
		// Last-resort fallback: if the renderer still emitted Astro syntax
		// (component template missing, or inline render bailed), keep the
		// notice in place of swallowing the block silently.
		if isComponentBlockOutput(raw, componentExts) {
			blocksHTML.WriteString(componentBlockPlaceholder(bl, componentExts))
			continue
		}
		blocksHTML.WriteString(raw)
	}

	var headerHTML, footerHTML string
	if page.HideGlobalBlocks == 0 {
		globalBlocks, _ := queries.ListActiveGlobalBlocksBySite(ctx, siteID)
		for _, gb := range globalBlocks {
			html := extractHTMLFromGlobalBlock(gb)
			switch gb.Slot {
			case "header":
				headerHTML = html
			case "footer":
				footerHTML = html
			}
		}
	}

	css, err := BuildCSS(ctx, queries, siteID)
	if err != nil {
		return "", fmt.Errorf("build css: %w", err)
	}
	fontFace := SiteFontsCSS(ctx, queries, siteID)

	title := strings.TrimSpace(page.Title)
	if title == "" {
		title = strings.TrimSpace(site.Name)
	}
	if title == "" {
		title = "Preview"
	}
	lang := strings.TrimSpace(site.Lang)
	if lang == "" {
		lang = "en"
	}

	var doc strings.Builder
	doc.WriteString("<!DOCTYPE html>\n")
	doc.WriteString(fmt.Sprintf(`<html lang="%s">`, escapeAttrForAstro(lang)))
	doc.WriteString("<head>")
	doc.WriteString(`<meta charset="UTF-8">`)
	doc.WriteString(`<meta name="viewport" content="width=device-width, initial-scale=1.0">`)
	doc.WriteString(`<meta name="robots" content="noindex, nofollow">`)
	doc.WriteString(fmt.Sprintf(`<title>%s · Preview</title>`, escapeAttrForAstro(title)))
	doc.WriteString(`<style data-atomicsite-preview="reset">html,body{margin:0;padding:0}body{min-height:100vh;background:#fafaf9;color:#18181b;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif}*{box-sizing:border-box}img{max-width:100%;height:auto}.atomicsite-preview-component-stub{margin:1.5rem auto;max-width:60rem;padding:1.25rem 1.5rem;border:1px dashed #d6d3d1;border-radius:0.75rem;background:#fff7ed;color:#9a3412;font-family:ui-monospace,Menlo,Consolas,monospace;font-size:0.8125rem;line-height:1.55}</style>`)
	if fontFace != "" {
		doc.WriteString(`<style data-atomicsite-preview="fonts">`)
		doc.WriteString(fontFace)
		doc.WriteString(`</style>`)
	}
	doc.WriteString(`<style data-atomicsite-preview="site">`)
	doc.WriteString(css)
	doc.WriteString(`</style>`)
	doc.WriteString("</head><body>")
	doc.WriteString(headerHTML)
	doc.WriteString(`<main>`)
	doc.WriteString(blocksHTML.String())
	doc.WriteString(`</main>`)
	doc.WriteString(footerHTML)
	doc.WriteString("</body></html>")

	return doc.String(), nil
}

// isComponentBlockOutput recognises renderBlock output that begins with
// `  <PascalName` for any component the site has registered. Component
// blocks need Astro to compile and aren't browser-renderable as-is.
func isComponentBlockOutput(raw string, componentExts map[string]string) bool {
	if len(componentExts) == 0 {
		return false
	}
	trimmed := strings.TrimLeft(raw, " \t\n")
	if !strings.HasPrefix(trimmed, "<") {
		return false
	}
	rest := trimmed[1:]
	end := strings.IndexAny(rest, " />\n")
	if end <= 0 {
		return false
	}
	tag := rest[:end]
	if tag == "" || !isUpperASCII(tag[0]) {
		return false
	}
	for name := range componentExts {
		if pascalCase(name) == tag {
			return true
		}
	}
	return false
}

func componentBlockPlaceholder(bl store.Block, componentExts map[string]string) string {
	compName := extractComponentName(bl)
	if compName == "" {
		compName = bl.BlockType
	}
	return fmt.Sprintf(
		`<aside class="atomicsite-preview-component-stub" data-block-id="%s" data-block-type="%s"><strong>Component block:</strong> &lt;%s /&gt;. Preview shows the rendered output after the next build.</aside>`,
		escapeAttrForAstro(bl.ID),
		escapeAttrForAstro(bl.BlockType),
		escapeAttrForAstro(pascalCase(compName)),
	)
}

func isUpperASCII(b byte) bool {
	return b >= 'A' && b <= 'Z'
}

// renderRegisteredComponentInline parses a registered Astro template enough
// to produce browser-renderable HTML for the admin preview iframe. It is
// NOT a full Astro compiler; it covers exactly the patterns our registered
// components use: a single frontmatter block destructuring props from
// Astro.props, body with `{name}` interpolation, optional
// `{name || "default"}` fallbacks, and verbatim <style> / <script> blocks.
//
// Returns the rendered HTML + true on success, "" + false if the template
// shape is too unusual to handle. Callers fall back to the placeholder in
// that case.
func renderRegisteredComponentInline(template, dataJSON string) (string, bool) {
	body, ok := stripAstroFrontmatter(template)
	if !ok {
		return "", false
	}

	var data map[string]any
	if err := json.Unmarshal([]byte(dataJSON), &data); err != nil {
		return "", false
	}
	props, ok := data["props"].(map[string]any)
	if !ok {
		props = data
	}

	body = substituteAstroProps(body, props)
	return "  " + body + "\n", true
}

// stripAstroFrontmatter removes the leading `---\n...---\n` block from an
// Astro template, returning the body. Returns ok=false if no frontmatter
// fence is found (we still try to render with empty props).
func stripAstroFrontmatter(tpl string) (string, bool) {
	tpl = strings.TrimLeft(tpl, " \t\n\r")
	tpl = strings.TrimPrefix(tpl, "\uFEFF")
	if !strings.HasPrefix(tpl, "---") {
		return tpl, true
	}
	rest := tpl[3:]
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return "", false
	}
	body := rest[idx+4:]
	body = strings.TrimLeft(body, "\n")
	return body, true
}

var (
	// Capture group 1 distinguishes three contexts:
	//   `=` -> JSX attribute (`href={x}`); wrap substituted value in quotes
	//   `$` -> JS template literal (`${x}` inside backticks); re-emit verbatim
	//   ``  -> text node (`<p>{x}</p>`); substitute raw
	astroPropWithDefault = regexp.MustCompile(`([=$]?)\{([a-zA-Z_][a-zA-Z0-9_]*)\s*\|\|\s*"([^"]*)"\}`)
	astroPropBare        = regexp.MustCompile(`([=$]?)\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)
)

// substituteAstroProps replaces `{propname}` and `{propname || "default"}`
// occurrences in an Astro body with the corresponding prop value. When the
// expression appears in JSX attribute position (`attr={prop}`), the result
// is wrapped in double quotes so values containing whitespace stay parsed
// as a single attribute value. Unknown props collapse to empty string.
// Values are HTML-escaped so user-controlled props can't break out of the
// rendered markup.
func substituteAstroProps(body string, props map[string]any) string {
	body = astroPropWithDefault.ReplaceAllStringFunc(body, func(m string) string {
		sub := astroPropWithDefault.FindStringSubmatch(m)
		prefix, name, fallback := sub[1], sub[2], sub[3]
		if prefix == "$" {
			return m
		}
		val, ok := propAsString(props, name)
		if !ok {
			val = escapeTextForAstro(fallback)
		}
		return wrapAttrIfNeeded(prefix, val)
	})
	body = astroPropBare.ReplaceAllStringFunc(body, func(m string) string {
		sub := astroPropBare.FindStringSubmatch(m)
		prefix, name := sub[1], sub[2]
		if prefix == "$" {
			return m
		}
		val, _ := propAsString(props, name)
		return wrapAttrIfNeeded(prefix, val)
	})
	return body
}

func wrapAttrIfNeeded(prefix, val string) string {
	if prefix == "=" {
		// val arrives already HTML-escaped (& < >) but NOT quote-escaped.
		// Inside ="..." an unescaped quote breaks out of the attribute and
		// injects new attributes (e.g. `" onmouseover=...`). Escape the quotes
		// the text-context escaper skipped to make this attribute-safe. Not
		// re-running the full escaper avoids double-encoding the & it handled.
		val = strings.ReplaceAll(val, `"`, "&quot;")
		val = strings.ReplaceAll(val, "'", "&#39;")
		return `="` + val + `"`
	}
	return val
}

// propAsString extracts a prop value as an HTML-safe string. Returns
// ok=false when the prop is missing (lets callers fall back to a default).
func propAsString(props map[string]any, name string) (string, bool) {
	if props == nil {
		return "", false
	}
	v, ok := props[name]
	if !ok {
		return "", false
	}
	switch val := v.(type) {
	case string:
		return escapeTextForAstro(val), true
	case float64:
		return fmt.Sprintf("%g", val), true
	case bool:
		if val {
			return "true", true
		}
		return "false", true
	case nil:
		return "", true
	default:
		b, _ := json.Marshal(val)
		return escapeTextForAstro(string(b)), true
	}
}
