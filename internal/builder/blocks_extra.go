package builder

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bright-interaction/slab/internal/store"
)

// renderHeroGraphic emits the markup for one of the curated hero
// graphics (mesh, pulse, monogram, audit-receipt). Each graphic ships
// pre-tuned CSS in css.go so the agent picks a coherent visual
// instead of inventing one via the custom block. All graphics are
// inspector-pre-vetted: A+ performance, WCAG-coloured, prefers-reduced-
// motion honored. circuit is the legacy bg mode (not handled here).
//
// Empty name = no graphic. Caller decides where the wrapper lives
// (positioned background for centered hero, right-column slot for
// split-hero).
func renderHeroGraphic(name string, data map[string]any) string {
	if name == "" {
		return ""
	}
	cls := "hero-graphic hero-graphic--" + escapeAttr(name)
	switch name {
	// aurora: full-bleed drifting aurora gradient, the showcase-fidelity
	// signature. Renders in any fidelity; reduced motion freezes it to a
	// static gradient via the CSS gate.
	case "mesh", "pulse", "gradient-orb", "aurora":
		return fmt.Sprintf(`<div class="%s" aria-hidden="true"></div>`, cls)
	case "globe-wire":
		// Pure SVG rendition of the marketing site's hero globe.
		// Five latitude ellipses + four connecting arcs + city dots
		// pulse-breathing at the Stockholm hub. No JS, no canvas,
		// honours prefers-reduced-motion via the @media gate in CSS.
		var b strings.Builder
		b.WriteString(fmt.Sprintf(`<div class="%s" aria-hidden="true">`, cls))
		b.WriteString(`<svg viewBox="0 0 400 400" class="hero-graphic__globe" xmlns="http://www.w3.org/2000/svg">`)
		b.WriteString(`<g class="hero-graphic__grid">`)
		b.WriteString(`<ellipse cx="200" cy="200" rx="180" ry="180" fill="none"/>`)
		b.WriteString(`<ellipse cx="200" cy="200" rx="180" ry="120" fill="none"/>`)
		b.WriteString(`<ellipse cx="200" cy="200" rx="180" ry="60" fill="none"/>`)
		b.WriteString(`<ellipse cx="200" cy="200" rx="120" ry="180" fill="none"/>`)
		b.WriteString(`<ellipse cx="200" cy="200" rx="60" ry="180" fill="none"/>`)
		b.WriteString(`</g>`)
		b.WriteString(`<g class="hero-graphic__arcs">`)
		b.WriteString(`<path d="M200,200 Q140,120 80,180" fill="none"/>`)
		b.WriteString(`<path d="M200,200 Q260,140 320,200" fill="none"/>`)
		b.WriteString(`<path d="M200,200 Q160,260 100,280" fill="none"/>`)
		b.WriteString(`<path d="M200,200 Q280,260 340,300" fill="none"/>`)
		b.WriteString(`</g>`)
		b.WriteString(`<g class="hero-graphic__cities">`)
		b.WriteString(`<circle cx="200" cy="200" r="6" class="hero-graphic__hub"/>`)
		b.WriteString(`<circle cx="80" cy="180" r="3"/>`)
		b.WriteString(`<circle cx="320" cy="200" r="3"/>`)
		b.WriteString(`<circle cx="100" cy="280" r="3"/>`)
		b.WriteString(`<circle cx="340" cy="300" r="3"/>`)
		b.WriteString(`</g>`)
		b.WriteString(`</svg>`)
		b.WriteString(`</div>`)
		return b.String()
	case "monogram":
		char := strings.TrimSpace(dataString(data, "monogram_char"))
		if char == "" {
			// Fall back to first letter of headline so the graphic never blanks.
			if h := strings.TrimSpace(dataString(data, "headline")); h != "" {
				char = string([]rune(h)[0])
			} else {
				char = "A"
			}
		}
		return fmt.Sprintf(`<div class="%s" aria-hidden="true"><span>%s</span></div>`,
			cls, escapeHTML(char))
	case "audit-receipt":
		label := dataString(data, "audit_label")
		if label == "" {
			label = "Live site audit"
		}
		score := dataString(data, "audit_score")
		if score == "" {
			score = "100"
		}
		max := dataString(data, "audit_score_max")
		if max == "" {
			max = "100"
		}
		baseline := dataString(data, "audit_baseline")
		if baseline == "" {
			baseline = "62"
		}
		var b strings.Builder
		b.WriteString(fmt.Sprintf(`<div class="%s" role="img" aria-label="%s, %s out of %s. Industry average %s.">`,
			cls, escapeAttr(label), escapeAttr(score), escapeAttr(max), escapeAttr(baseline)))
		b.WriteString(`<div class="hero-graphic__chrome"><span></span><span></span><span></span></div>`)
		b.WriteString(`<p class="hero-graphic__label">` + escapeHTML(label) + `</p>`)
		b.WriteString(fmt.Sprintf(`<p class="hero-graphic__score"><strong>%s</strong><span>/%s</span></p>`,
			escapeHTML(score), escapeHTML(max)))
		b.WriteString(fmt.Sprintf(`<p class="hero-graphic__baseline">Industry average %s</p>`,
			escapeHTML(baseline)))
		b.WriteString(`</div>`)
		return b.String()
	}
	return ""
}

// renderSplitHeroBlock renders a side-by-side hero (image right + text left
// on desktop, stacked on mobile). Use when the page wants a SaaS-marketing
// shape rather than the centered hero. Layout flips to centered when the
// site setting general.hero_layout=centered or the block's data.layout=centered.
//
// Optional bg modes via data.bg:
//   - "circuit": ships an animated PCB-style canvas behind the content
//     (auto-tints to --color-primary, prefers-reduced-motion respected).
//   - "circuit-static": same SVG pattern but static (no canvas / no JS).
func renderSplitHeroBlock(data map[string]any, mediaByID map[string]store.Medium) string {
	var b strings.Builder
	bg := dataString(data, "bg")
	cls := "block block--split_hero"
	if bg == "circuit" || bg == "circuit-static" {
		cls += " has-circuit-bg"
	}
	b.WriteString(fmt.Sprintf("  <section class=\"%s\"", cls))
	if bg == "circuit-static" {
		b.WriteString(` data-bg="circuit"`)
	}
	b.WriteString(">\n")
	if bg == "circuit" {
		b.WriteString("    <canvas data-circuit-canvas class=\"block-circuit-canvas\" aria-hidden=\"true\"></canvas>\n")
		b.WriteString("    <script>")
		b.Write(CircuitBgScript)
		b.WriteString("</script>\n")
	}
	b.WriteString("    <div class=\"split-hero-content\">\n")
	if eyebrow := dataString(data, "eyebrow"); eyebrow != "" {
		b.WriteString(fmt.Sprintf("      <p class=\"eyebrow\">%s</p>\n", escapeHTML(eyebrow)))
	}
	if headline := dataString(data, "headline"); headline != "" {
		b.WriteString(fmt.Sprintf("      <h1>%s</h1>\n", renderHeadlineWithAccent(headline, dataString(data, "headline_accent"))))
	}
	if sub := dataString(data, "subheading"); sub != "" {
		b.WriteString(fmt.Sprintf("      <p class=\"subheading\">%s</p>\n", escapeHTML(sub)))
	}
	ctaText := dataString(data, "cta_text")
	secLabel := dataString(data, "secondary_label")
	if ctaText != "" || secLabel != "" {
		b.WriteString("      <div class=\"hero-actions\">\n")
		if ctaText != "" {
			ctaURL := dataString(data, "cta_url")
			if ctaURL == "" {
				ctaURL = "#"
			}
			b.WriteString(fmt.Sprintf("        <a href=\"%s\" class=\"btn-primary\">%s</a>\n",
				escapeURL(ctaURL), escapeHTML(ctaText)))
		}
		if secLabel != "" {
			secURL := dataString(data, "secondary_url")
			if secURL == "" {
				secURL = "#"
			}
			b.WriteString(fmt.Sprintf("        <a href=\"%s\" class=\"btn-secondary\">%s</a>\n",
				escapeURL(secURL), escapeHTML(secLabel)))
		}
		b.WriteString("      </div>\n")
	}
	b.WriteString("    </div>\n")
	b.WriteString("    <div class=\"split-hero-image\">\n")
	graphic := dataString(data, "hero_graphic")
	if graphic != "" {
		b.WriteString("      " + renderHeroGraphic(graphic, data) + "\n")
	} else if imageID := dataString(data, "image_id"); imageID != "" {
		b.WriteString("      " + renderMediaImgWithPriority(imageID, dataString(data, "image_alt"), dataString(data, "headline"), "split-hero-img", mediaByID, true) + "\n")
	}
	b.WriteString("    </div>\n")
	b.WriteString("  </section>\n")
	return b.String()
}

// renderStatGridBlock renders a horizontal grid of large numbers + labels.
// Used for trust signals, social proof, hero stats. Each item: {value, label,
// description?}. Distinct from feature_grid (no icons, number-first hierarchy).
func renderStatGridBlock(data map[string]any) string {
	var b strings.Builder
	b.WriteString("  <section class=\"block block--stat_grid\">\n")
	if heading := dataString(data, "heading"); heading != "" {
		b.WriteString(fmt.Sprintf("    <h2>%s</h2>\n", escapeHTML(heading)))
	}
	if sub := dataString(data, "subheading"); sub != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"subheading\">%s</p>\n", escapeHTML(sub)))
	}
	if itemsRaw, ok := data["items"].([]any); ok && len(itemsRaw) > 0 {
		b.WriteString("    <ul class=\"stat-grid\">\n")
		for _, it := range itemsRaw {
			item, ok := it.(map[string]any)
			if !ok {
				continue
			}
			b.WriteString("      <li class=\"stat-grid-item\">\n")
			if v := dataString(item, "value"); v != "" {
				b.WriteString(fmt.Sprintf("        <div class=\"stat-value\">%s</div>\n", escapeHTML(v)))
			}
			if l := dataString(item, "label"); l != "" {
				b.WriteString(fmt.Sprintf("        <div class=\"stat-label\">%s</div>\n", escapeHTML(l)))
			}
			if c := dataString(item, "context"); c != "" {
				b.WriteString(fmt.Sprintf("        <div class=\"stat-context\">%s</div>\n", escapeHTML(c)))
			}
			b.WriteString("      </li>\n")
		}
		b.WriteString("    </ul>\n")
	}
	b.WriteString("  </section>\n")
	return b.String()
}

// renderAccordionFAQBlock renders a Q&A accordion via native <details>/<summary>
// (no JS needed). Each item: {question, answer}.
func renderAccordionFAQBlock(data map[string]any) string {
	var b strings.Builder
	b.WriteString("  <section class=\"block block--accordion_faq\">\n")
	if heading := dataString(data, "heading"); heading != "" {
		b.WriteString(fmt.Sprintf("    <h2>%s</h2>\n", escapeHTML(heading)))
	}
	if sub := dataString(data, "subheading"); sub != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"subheading\">%s</p>\n", escapeHTML(sub)))
	}
	if itemsRaw, ok := data["items"].([]any); ok && len(itemsRaw) > 0 {
		// Emit FAQPage JSON-LD so the eval picks up structured FAQ.
		var faqLDItems []map[string]any
		for _, it := range itemsRaw {
			item, ok := it.(map[string]any)
			if !ok {
				continue
			}
			q := dataString(item, "question")
			a := dataString(item, "answer")
			if q == "" {
				continue
			}
			faqLDItems = append(faqLDItems, map[string]any{
				"@type": "Question",
				"name":  q,
				"acceptedAnswer": map[string]any{
					"@type": "Answer",
					"text":  a,
				},
			})
		}
		if len(faqLDItems) > 0 {
			ld := map[string]any{
				"@context":   "https://schema.org",
				"@type":      "FAQPage",
				"mainEntity": faqLDItems,
			}
			ldBytes, _ := json.Marshal(ld)
			b.WriteString("    <script type=\"application/ld+json\">")
			b.Write(ldBytes)
			b.WriteString("</script>\n")
		}
		b.WriteString("    <div class=\"faq-accordion\">\n")
		for _, it := range itemsRaw {
			item, ok := it.(map[string]any)
			if !ok {
				continue
			}
			q := dataString(item, "question")
			a := dataString(item, "answer")
			if q == "" {
				continue
			}
			b.WriteString("      <details class=\"faq-item\">\n")
			b.WriteString(fmt.Sprintf("        <summary>%s</summary>\n", escapeHTML(q)))
			if a != "" {
				b.WriteString("        <div class=\"faq-body\">\n")
				renderTextParagraphs(&b, a)
				b.WriteString("        </div>\n")
			}
			b.WriteString("      </details>\n")
		}
		b.WriteString("    </div>\n")
	}
	b.WriteString("  </section>\n")
	return b.String()
}

// renderPricingBlock renders a 3-up tier card grid. Each tier:
// {name, price, price_period, description, features[], cta_text, cta_url, featured?}.
func renderPricingBlock(data map[string]any) string {
	var b strings.Builder
	b.WriteString("  <section class=\"block block--pricing\">\n")
	if heading := dataString(data, "heading"); heading != "" {
		b.WriteString(fmt.Sprintf("    <h2>%s</h2>\n", escapeHTML(heading)))
	}
	if sub := dataString(data, "subheading"); sub != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"subheading\">%s</p>\n", escapeHTML(sub)))
	}
	if tiersRaw, ok := data["tiers"].([]any); ok && len(tiersRaw) > 0 {
		b.WriteString("    <ul class=\"pricing-grid\">\n")
		for _, t := range tiersRaw {
			tier, ok := t.(map[string]any)
			if !ok {
				continue
			}
			cls := "pricing-tier"
			if featured, _ := tier["featured"].(bool); featured {
				cls += " is-featured"
			}
			b.WriteString(fmt.Sprintf("      <li class=\"%s\">\n", cls))
			if step := dataString(tier, "step"); step != "" {
				b.WriteString(fmt.Sprintf("        <div class=\"tier-step\">%s</div>\n", escapeHTML(step)))
			}
			if n := dataString(tier, "name"); n != "" {
				b.WriteString(fmt.Sprintf("        <div class=\"tier-name\">%s</div>\n", escapeHTML(n)))
			}
			if p := dataString(tier, "price"); p != "" {
				b.WriteString(fmt.Sprintf("        <div class=\"tier-price\">%s</div>\n", escapeHTML(p)))
			}
			if pp := dataString(tier, "price_period"); pp != "" {
				b.WriteString(fmt.Sprintf("        <div class=\"tier-price-period\">%s</div>\n", escapeHTML(pp)))
			}
			if d := dataString(tier, "description"); d != "" {
				b.WriteString(fmt.Sprintf("        <p class=\"tier-description\">%s</p>\n", escapeHTML(d)))
			}
			if featuresRaw, ok := tier["features"].([]any); ok && len(featuresRaw) > 0 {
				b.WriteString("        <ul class=\"tier-features\">\n")
				for _, f := range featuresRaw {
					if fs, ok := f.(string); ok && fs != "" {
						b.WriteString(fmt.Sprintf("          <li>%s</li>\n", escapeHTML(fs)))
					}
				}
				b.WriteString("        </ul>\n")
			}
			if ctaText := dataString(tier, "cta_text"); ctaText != "" {
				ctaURL := dataString(tier, "cta_url")
				if ctaURL == "" {
					ctaURL = "#"
				}
				cls := "btn-primary"
				if featured, _ := tier["featured"].(bool); !featured {
					cls = "btn-secondary"
				}
				b.WriteString(fmt.Sprintf("        <div class=\"tier-cta\"><a href=\"%s\" class=\"%s\">%s</a></div>\n",
					escapeURL(ctaURL), cls, escapeHTML(ctaText)))
			}
			b.WriteString("      </li>\n")
		}
		b.WriteString("    </ul>\n")
	}
	b.WriteString("  </section>\n")
	return b.String()
}

// renderComparisonTableBlock renders a feature-matrix table comparing
// the site's offering against named alternatives. Schema:
//
//	{
//	  "heading": "...",
//	  "subheading": "...",
//	  "us_label": "Atomicsite",   // column label for our column (defaults to "Us")
//	  "columns": ["Lovable", "Webflow"],
//	  "rows": [
//	    {"feature": "SSR Astro output", "us": true,  "values": [false, true]},
//	    {"feature": "BYO AI key",        "us": true,  "values": [false, false]},
//	    {"feature": "Inspector built-in","us": "100/100", "values": ["-", "-"]}
//	  ]
//	}
//
// Each `us` / `values[i]` may be:
//   - bool: rendered as a tick or cross
//   - string: rendered as raw text (e.g. "100/100", "-", "Free")
//
// values[i] aligns with columns[i] by index; longer values arrays beyond
// columns are dropped, shorter rows render dashes for missing cells.
// The "us" column is always emitted as the leftmost data column (after
// the feature label) so the read is "is the feature there for us, then
// who else has it." This is the Stripe/Vercel comparison-page pattern.
func renderComparisonTableBlock(data map[string]any) string {
	var b strings.Builder
	b.WriteString("  <section class=\"block block--comparison_table\">\n")
	if heading := dataString(data, "heading"); heading != "" {
		b.WriteString(fmt.Sprintf("    <h2>%s</h2>\n", escapeHTML(heading)))
	}
	if sub := dataString(data, "subheading"); sub != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"subheading\">%s</p>\n", escapeHTML(sub)))
	}
	usLabel := dataString(data, "us_label")
	if usLabel == "" {
		usLabel = "Us"
	}
	colsRaw, _ := data["columns"].([]any)
	rowsRaw, _ := data["rows"].([]any)
	cols := make([]string, 0, len(colsRaw))
	for _, c := range colsRaw {
		if s, ok := c.(string); ok && s != "" {
			cols = append(cols, s)
		}
	}
	// We always render at least the us column even with zero competitor
	// columns. That way a single-product "what you get" matrix still
	// reads as a comparison table rather than a generic feature list.
	b.WriteString("    <div class=\"comparison-table-wrap\">\n")
	b.WriteString("    <table class=\"comparison-table\">\n")
	b.WriteString("      <thead>\n")
	b.WriteString("        <tr>\n")
	b.WriteString("          <th scope=\"col\">Feature</th>\n")
	b.WriteString(fmt.Sprintf("          <th scope=\"col\" class=\"comparison-us\">%s</th>\n", escapeHTML(usLabel)))
	for _, c := range cols {
		b.WriteString(fmt.Sprintf("          <th scope=\"col\">%s</th>\n", escapeHTML(c)))
	}
	b.WriteString("        </tr>\n")
	b.WriteString("      </thead>\n")
	b.WriteString("      <tbody>\n")
	for _, r := range rowsRaw {
		row, ok := r.(map[string]any)
		if !ok {
			continue
		}
		feature := dataString(row, "feature")
		if feature == "" {
			continue
		}
		b.WriteString("        <tr>\n")
		b.WriteString(fmt.Sprintf("          <th scope=\"row\">%s</th>\n", escapeHTML(feature)))
		b.WriteString(fmt.Sprintf("          <td class=\"comparison-us\">%s</td>\n", renderComparisonCell(row["us"])))
		valsRaw, _ := row["values"].([]any)
		for i := 0; i < len(cols); i++ {
			var v any
			if i < len(valsRaw) {
				v = valsRaw[i]
			}
			b.WriteString(fmt.Sprintf("          <td>%s</td>\n", renderComparisonCell(v)))
		}
		b.WriteString("        </tr>\n")
	}
	b.WriteString("      </tbody>\n")
	b.WriteString("    </table>\n")
	b.WriteString("    </div>\n")
	b.WriteString("  </section>\n")
	return b.String()
}

// renderComparisonCell turns one cell value into displayable HTML.
// nil + false render as a cross, true as a tick, strings pass through.
// Numbers stringify with %v. The aria-label is set so screen readers
// announce "yes" / "no" instead of the visual mark.
func renderComparisonCell(v any) string {
	switch x := v.(type) {
	case nil:
		return `<span class="comparison-cell comparison-cell--none" aria-label="not included">-</span>`
	case bool:
		if x {
			return `<span class="comparison-cell comparison-cell--yes" aria-label="included">&#10003;</span>`
		}
		return `<span class="comparison-cell comparison-cell--no" aria-label="not included">&times;</span>`
	case string:
		if x == "" {
			return `<span class="comparison-cell comparison-cell--none" aria-label="not included">-</span>`
		}
		return fmt.Sprintf(`<span class="comparison-cell comparison-cell--text">%s</span>`, escapeHTML(x))
	}
	return fmt.Sprintf(`<span class="comparison-cell comparison-cell--text">%v</span>`, v)
}

// renderMegaMenuBlock emits a navigation bar with multi-column dropdown
// panels. Built on native <details> + <summary> so keyboard nav works
// without JS: Tab to focus, Enter to toggle. CSS opens panels on hover
// for pointer devices (desktop), tap-to-toggle for touch (mobile). The
// panel layout is grid-based with up to four columns + optional featured
// slot.
func renderMegaMenuBlock(data map[string]any) string {
	var b strings.Builder
	itemsRaw, _ := data["items"].([]any)
	if len(itemsRaw) == 0 {
		return ""
	}
	b.WriteString("  <nav class=\"block block--mega_menu\" aria-label=\"Primary navigation\">\n")
	b.WriteString("    <ul class=\"mega-menu-list\">\n")
	for _, it := range itemsRaw {
		item, ok := it.(map[string]any)
		if !ok {
			continue
		}
		label := dataString(item, "label")
		if label == "" {
			continue
		}
		href := dataString(item, "href")
		columnsRaw, _ := item["columns"].([]any)
		hasDropdown := len(columnsRaw) > 0 ||
			dataString(item, "featured_heading") != "" ||
			dataString(item, "featured_body") != ""

		switch {
		case !hasDropdown && href != "":
			b.WriteString(fmt.Sprintf("      <li class=\"mega-menu-item mega-menu-item--link\"><a href=\"%s\">%s</a></li>\n",
				escapeURL(href), escapeHTML(label)))
		case hasDropdown:
			b.WriteString("      <li class=\"mega-menu-item mega-menu-item--dropdown\">\n")
			b.WriteString("        <details>\n")
			b.WriteString(fmt.Sprintf("          <summary>%s<span class=\"mega-menu-caret\" aria-hidden=\"true\">▾</span></summary>\n", escapeHTML(label)))
			b.WriteString("          <div class=\"mega-panel\">\n")
			b.WriteString("            <div class=\"mega-panel-columns\">\n")
			for _, c := range columnsRaw {
				col, ok := c.(map[string]any)
				if !ok {
					continue
				}
				b.WriteString("              <div class=\"mega-column\">\n")
				if h := dataString(col, "heading"); h != "" {
					b.WriteString(fmt.Sprintf("                <p class=\"mega-column-heading\">%s</p>\n", escapeHTML(h)))
				}
				colItems, _ := col["items"].([]any)
				if len(colItems) > 0 {
					b.WriteString("                <ul>\n")
					for _, ci := range colItems {
						link, ok := ci.(map[string]any)
						if !ok {
							continue
						}
						lLabel := dataString(link, "label")
						lHref := dataString(link, "href")
						if lLabel == "" || lHref == "" {
							continue
						}
						desc := dataString(link, "description")
						b.WriteString(fmt.Sprintf("                  <li><a href=\"%s\"><span class=\"mega-link-label\">%s</span>", escapeURL(lHref), escapeHTML(lLabel)))
						if desc != "" {
							b.WriteString(fmt.Sprintf("<span class=\"mega-link-desc\">%s</span>", escapeHTML(desc)))
						}
						b.WriteString("</a></li>\n")
					}
					b.WriteString("                </ul>\n")
				}
				b.WriteString("              </div>\n")
			}
			b.WriteString("            </div>\n")
			// Featured slot.
			if fh := dataString(item, "featured_heading"); fh != "" {
				b.WriteString("            <aside class=\"mega-featured\">\n")
				b.WriteString(fmt.Sprintf("              <p class=\"mega-featured-heading\">%s</p>\n", escapeHTML(fh)))
				if fb := dataString(item, "featured_body"); fb != "" {
					b.WriteString(fmt.Sprintf("              <p class=\"mega-featured-body\">%s</p>\n", escapeHTML(fb)))
				}
				fctaT := dataString(item, "featured_cta_text")
				fctaU := dataString(item, "featured_cta_url")
				if fctaT != "" && fctaU != "" {
					b.WriteString(fmt.Sprintf("              <a class=\"mega-featured-cta\" href=\"%s\">%s</a>\n", escapeURL(fctaU), escapeHTML(fctaT)))
				}
				b.WriteString("            </aside>\n")
			}
			b.WriteString("          </div>\n")
			b.WriteString("        </details>\n")
			b.WriteString("      </li>\n")
		default:
			// Label only, no href and no dropdown content. Render as
			// plain text so the menu item still appears (operator can
			// edit later) without dead links.
			b.WriteString(fmt.Sprintf("      <li class=\"mega-menu-item mega-menu-item--text\"><span>%s</span></li>\n", escapeHTML(label)))
		}
	}
	b.WriteString("    </ul>\n")
	b.WriteString("  </nav>\n")
	return b.String()
}

// renderScrollRevealBlock emits a vertical sequence of text+image pairs
// that fade and lift into view as the user scrolls. The animation runs
// entirely in the browser compositor via the CSS animation-timeline:
// view() spec (supported in evergreen browsers from 2024). Older
// browsers see the items appear normally with full opacity, so the
// fallback is graceful, not broken. prefers-reduced-motion disables
// the animation entirely.
func renderScrollRevealBlock(data map[string]any, mediaByID map[string]store.Medium) string {
	var b strings.Builder
	b.WriteString("  <section class=\"block block--scroll_reveal\">\n")
	if eyebrow := dataString(data, "eyebrow"); eyebrow != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"eyebrow\">%s</p>\n", escapeHTML(eyebrow)))
	}
	if heading := dataString(data, "heading"); heading != "" {
		b.WriteString(fmt.Sprintf("    <h2>%s</h2>\n", escapeHTML(heading)))
	}
	if sub := dataString(data, "subheading"); sub != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"subheading\">%s</p>\n", escapeHTML(sub)))
	}
	itemsRaw, _ := data["items"].([]any)
	if len(itemsRaw) == 0 {
		b.WriteString("  </section>\n")
		return b.String()
	}
	b.WriteString("    <ol class=\"scroll-reveal-list\">\n")
	for _, it := range itemsRaw {
		item, ok := it.(map[string]any)
		if !ok {
			continue
		}
		stepHeading := dataString(item, "heading")
		if stepHeading == "" {
			continue
		}
		imagePos := dataString(item, "image_position")
		if imagePos != "left" {
			imagePos = "right"
		}
		b.WriteString(fmt.Sprintf("      <li class=\"scroll-reveal-item is-image-%s\">\n", escapeAttr(imagePos)))
		b.WriteString("        <div class=\"scroll-reveal-content\">\n")
		b.WriteString(fmt.Sprintf("          <h3>%s</h3>\n", escapeHTML(stepHeading)))
		if body := dataString(item, "body"); body != "" {
			for _, para := range strings.Split(body, "\n\n") {
				para = strings.TrimSpace(para)
				if para != "" {
					b.WriteString(fmt.Sprintf("          <p>%s</p>\n", escapeHTML(para)))
				}
			}
		}
		b.WriteString("        </div>\n")
		if imageID := dataString(item, "image_id"); imageID != "" {
			b.WriteString("        <div class=\"scroll-reveal-media\">\n")
			b.WriteString("          " + renderMediaImg(imageID, stepHeading, stepHeading, "scroll-reveal-img", mediaByID) + "\n")
			b.WriteString("        </div>\n")
		}
		b.WriteString("      </li>\n")
	}
	b.WriteString("    </ol>\n")
	b.WriteString("  </section>\n")
	return b.String()
}

// renderTabsBlock emits a tabbed content block. Pure CSS via hidden radio
// inputs plus the sibling-selector trick: each `:checked` radio reveals its
// matching panel. No JS at runtime; native keyboard nav (arrow keys cycle
// radio group) and screen-reader semantics via role="tablist" + role="tab"
// labels. Per-block unique ID derived from a deterministic hash of the
// data so two tabs blocks on the same page do not collide.
func renderTabsBlock(data map[string]any, mediaByID map[string]store.Medium) string {
	var b strings.Builder
	itemsRaw, _ := data["items"].([]any)
	if len(itemsRaw) == 0 {
		return ""
	}

	groupID := tabsGroupID(data)
	defaultTab := 0
	if v := parseRating(dataString(data, "default_tab")); v >= 0 && v < len(itemsRaw) {
		defaultTab = v
	}

	b.WriteString("  <section class=\"block block--tabs\">\n")
	if eyebrow := dataString(data, "eyebrow"); eyebrow != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"eyebrow\">%s</p>\n", escapeHTML(eyebrow)))
	}
	if heading := dataString(data, "heading"); heading != "" {
		b.WriteString(fmt.Sprintf("    <h2>%s</h2>\n", escapeHTML(heading)))
	}
	if sub := dataString(data, "subheading"); sub != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"subheading\">%s</p>\n", escapeHTML(sub)))
	}

	b.WriteString("    <div class=\"tabs-root\">\n")

	// Radios first, then labels, then panels. The CSS uses
	// `input:nth-of-type(N):checked ~ .tab-panels > .tab-panel:nth-child(N)`
	// to reveal the matching panel.
	for i := range itemsRaw {
		checked := ""
		if i == defaultTab {
			checked = " checked"
		}
		b.WriteString(fmt.Sprintf("      <input type=\"radio\" class=\"tab-radio\" name=\"%s\" id=\"%s-%d\"%s />\n",
			escapeAttr(groupID), escapeAttr(groupID), i, checked))
	}

	b.WriteString("      <div class=\"tabs-list\" role=\"tablist\">\n")
	for i, it := range itemsRaw {
		item, ok := it.(map[string]any)
		if !ok {
			continue
		}
		label := dataString(item, "label")
		if label == "" {
			label = fmt.Sprintf("Tab %d", i+1)
		}
		b.WriteString(fmt.Sprintf("        <label for=\"%s-%d\" class=\"tab-trigger\" role=\"tab\">%s</label>\n",
			escapeAttr(groupID), i, escapeHTML(label)))
	}
	b.WriteString("      </div>\n")

	b.WriteString("      <div class=\"tab-panels\">\n")
	for _, it := range itemsRaw {
		item, ok := it.(map[string]any)
		if !ok {
			continue
		}
		b.WriteString("        <div class=\"tab-panel\" role=\"tabpanel\">\n")
		if h := dataString(item, "heading"); h != "" {
			b.WriteString(fmt.Sprintf("          <h3>%s</h3>\n", escapeHTML(h)))
		}
		if body := dataString(item, "body"); body != "" {
			for _, para := range strings.Split(body, "\n\n") {
				para = strings.TrimSpace(para)
				if para != "" {
					b.WriteString(fmt.Sprintf("          <p>%s</p>\n", escapeHTML(para)))
				}
			}
		}
		if imageID := dataString(item, "image_id"); imageID != "" {
			alt := dataString(item, "heading")
			b.WriteString("          " + renderMediaImg(imageID, alt, alt, "tab-panel-img", mediaByID) + "\n")
		}
		ctaText := dataString(item, "cta_text")
		ctaURL := dataString(item, "cta_url")
		if ctaText != "" && ctaURL != "" {
			b.WriteString(fmt.Sprintf("          <a class=\"cta cta--secondary\" href=\"%s\">%s</a>\n", escapeURL(ctaURL), escapeHTML(ctaText)))
		}
		b.WriteString("        </div>\n")
	}
	b.WriteString("      </div>\n")
	b.WriteString("    </div>\n")
	b.WriteString("  </section>\n")
	return b.String()
}

// tabsGroupID returns a stable 10-char identifier derived from the tab
// labels concatenated. Same content = same ID, so a re-build does not
// change the rendered markup. Two tabs blocks with different labels get
// different IDs. Collisions are theoretically possible but require two
// blocks on the same page with identical label sequences, which is itself
// a UX bug worth flagging.
func tabsGroupID(data map[string]any) string {
	itemsRaw, _ := data["items"].([]any)
	var seed strings.Builder
	seed.WriteString("tabs-")
	for _, it := range itemsRaw {
		item, _ := it.(map[string]any)
		seed.WriteString(stringOfAny(item, "label"))
		seed.WriteString("|")
	}
	return fmt.Sprintf("tabs-%010x", fnv1aHash(seed.String()))
}

// fnv1aHash is the 64-bit FNV-1a hash used purely for content-derived
// element IDs. NOT a security primitive.
func fnv1aHash(s string) uint64 {
	const offset uint64 = 14695981039346656037
	const prime uint64 = 1099511628211
	h := offset
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

// stringOfAny mirrors stringOf but takes the map directly so callers that
// hold a parsed map[string]any do not have to wrap it.
func stringOfAny(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// renderVideoHeroBlock emits a hero with a background video. Three
// sources supported: self_hosted (uploaded mp4/webm), youtube (via
// youtube-nocookie.com so no third-party cookies fire pre-consent), and
// vimeo (player.vimeo.com background mode). The poster image is the LCP
// element and is rendered separately with fetchpriority=high; the video
// loads after first paint and replaces the poster client-side via the
// browser's native poster attribute / iframe load.
func renderVideoHeroBlock(data map[string]any, mediaByID map[string]store.Medium) string {
	var b strings.Builder
	headline := dataString(data, "headline")
	source := strings.TrimSpace(strings.ToLower(dataString(data, "video_source")))
	videoID := strings.TrimSpace(dataString(data, "video_id"))
	posterID := dataString(data, "poster_image_id")
	overlay := parseOverlayOpacity(dataString(data, "overlay_opacity"))

	// Emit the overlay as a unitless number (0.0-1.0) so CSS rgba()
	// can consume it directly. Percent-typed custom properties cannot
	// be divided to a unitless alpha in calc().
	style := fmt.Sprintf("--video-hero-overlay: %.2f;", float64(overlay)/100.0)
	b.WriteString(fmt.Sprintf("  <section class=\"block block--video_hero\" style=\"%s\">\n", style))

	// Background video layer (or static poster fallback when no video_id).
	b.WriteString("    <div class=\"video-hero-media\" aria-hidden=\"true\">\n")
	switch source {
	case "youtube":
		if videoID != "" {
			src := fmt.Sprintf("https://www.youtube-nocookie.com/embed/%s?autoplay=1&mute=1&loop=1&playlist=%s&controls=0&modestbranding=1&playsinline=1", escapeAttr(videoID), escapeAttr(videoID))
			b.WriteString(fmt.Sprintf("      <iframe src=\"%s\" title=\"Background video\" loading=\"lazy\" referrerpolicy=\"strict-origin-when-cross-origin\" allow=\"autoplay; encrypted-media; picture-in-picture\" allowfullscreen></iframe>\n", src))
		}
	case "vimeo":
		if videoID != "" {
			src := fmt.Sprintf("https://player.vimeo.com/video/%s?background=1&muted=1&loop=1&autoplay=1", escapeAttr(videoID))
			b.WriteString(fmt.Sprintf("      <iframe src=\"%s\" title=\"Background video\" loading=\"lazy\" referrerpolicy=\"strict-origin-when-cross-origin\" allow=\"autoplay; picture-in-picture\" allowfullscreen></iframe>\n", src))
		}
	default:
		// self_hosted
		if videoID != "" {
			posterAttr := ""
			if m, ok := mediaByID[posterID]; ok && len(m.VariantsJson) > 0 {
				// Use the smallest variant path as a quick poster.
				posterAttr = fmt.Sprintf(" poster=\"/media/%s/original\"", escapeAttr(m.ID))
			}
			b.WriteString(fmt.Sprintf("      <video%s autoplay muted loop playsinline preload=\"metadata\"><source src=\"/media/%s/original.mp4\" type=\"video/mp4\" /></video>\n", posterAttr, escapeAttr(videoID)))
		}
	}
	// Static poster underneath the video keeps LCP grounded. fetchpriority=high
	// ensures the poster wins the LCP race over any iframe / <video>.
	if posterID != "" {
		b.WriteString("      " + renderMediaImgWithPriority(posterID, headline, headline, "video-hero-poster", mediaByID, true) + "\n")
	}
	b.WriteString("    </div>\n")
	// Overlay tint.
	b.WriteString("    <div class=\"video-hero-overlay\" aria-hidden=\"true\"></div>\n")

	// Content layer.
	b.WriteString("    <div class=\"video-hero-content\">\n")
	if eyebrow := dataString(data, "eyebrow"); eyebrow != "" {
		b.WriteString(fmt.Sprintf("      <p class=\"eyebrow\">%s</p>\n", escapeHTML(eyebrow)))
	}
	if headline != "" {
		b.WriteString(fmt.Sprintf("      <h1>%s</h1>\n", escapeHTML(headline)))
	}
	if sub := dataString(data, "subheading"); sub != "" {
		b.WriteString(fmt.Sprintf("      <p class=\"subheading\">%s</p>\n", escapeHTML(sub)))
	}
	cta := dataString(data, "cta_text")
	ctaURL := dataString(data, "cta_url")
	secLabel := dataString(data, "secondary_label")
	secURL := dataString(data, "secondary_url")
	if cta != "" || secLabel != "" {
		b.WriteString("      <div class=\"cta-group\">\n")
		if cta != "" && ctaURL != "" {
			b.WriteString(fmt.Sprintf("        <a class=\"cta cta--primary\" href=\"%s\">%s</a>\n", escapeURL(ctaURL), escapeHTML(cta)))
		}
		if secLabel != "" && secURL != "" {
			b.WriteString(fmt.Sprintf("        <a class=\"cta cta--secondary\" href=\"%s\">%s</a>\n", escapeURL(secURL), escapeHTML(secLabel)))
		}
		b.WriteString("      </div>\n")
	}
	b.WriteString("    </div>\n")
	b.WriteString("  </section>\n")
	return b.String()
}

// parseOverlayOpacity coerces a user-supplied "0-100" string into a
// clamped int. Empty / unparseable values default to 35, the operator-
// friendly default that keeps a poster image legible behind white text.
func parseOverlayOpacity(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 35
	}
	n := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			n = n*10 + int(r-'0')
			if n > 1000 {
				break
			}
			continue
		}
		break
	}
	if n < 0 {
		return 0
	}
	if n > 100 {
		return 100
	}
	if n == 0 && s != "0" {
		return 35
	}
	return n
}

// renderTestimonialWallBlock emits a customer-quote wall in one of three
// layouts (wall / carousel / single_featured). Pure HTML + CSS; the
// carousel layout uses scroll-snap so no JS runs at runtime, keeping the
// SSG-wedge intact.
func renderTestimonialWallBlock(data map[string]any, mediaByID map[string]store.Medium) string {
	var b strings.Builder
	layout := strings.TrimSpace(dataString(data, "layout"))
	switch layout {
	case "carousel", "single_featured":
		// keep
	default:
		layout = "wall"
	}
	b.WriteString(fmt.Sprintf("  <section class=\"block block--testimonial_wall block--testimonial_wall--%s\">\n", escapeAttr(layout)))
	if eyebrow := dataString(data, "eyebrow"); eyebrow != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"eyebrow\">%s</p>\n", escapeHTML(eyebrow)))
	}
	if heading := dataString(data, "heading"); heading != "" {
		b.WriteString(fmt.Sprintf("    <h2>%s</h2>\n", escapeHTML(heading)))
	}
	if sub := dataString(data, "subheading"); sub != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"subheading\">%s</p>\n", escapeHTML(sub)))
	}
	itemsRaw, _ := data["items"].([]any)
	if len(itemsRaw) > 0 {
		listClass := "testimonial-wall-list"
		if layout == "carousel" {
			listClass = "testimonial-carousel-list"
		} else if layout == "single_featured" {
			listClass = "testimonial-featured-list"
		}
		b.WriteString(fmt.Sprintf("    <ul class=\"%s\">\n", listClass))
		for _, it := range itemsRaw {
			item, ok := it.(map[string]any)
			if !ok {
				continue
			}
			quote := dataString(item, "quote")
			if quote == "" {
				continue
			}
			b.WriteString("      <li class=\"testimonial-card\">\n")

			// Rating pips (optional). Renders as 5 stars with filled pips
			// for the given rating.
			rating := parseRating(dataString(item, "rating"))
			if rating > 0 {
				b.WriteString("        <div class=\"testimonial-rating\" aria-label=\"")
				b.WriteString(fmt.Sprintf("Rated %d out of 5", rating))
				b.WriteString("\">")
				for i := 1; i <= 5; i++ {
					if i <= rating {
						b.WriteString("<span class=\"is-filled\" aria-hidden=\"true\">★</span>")
					} else {
						b.WriteString("<span aria-hidden=\"true\">☆</span>")
					}
				}
				b.WriteString("</div>\n")
			}

			// The quote itself. Wrapped in blockquote for semantics +
			// quote marks dropped in by CSS so the text stays plain.
			if sourceURL := dataString(item, "source_url"); sourceURL != "" {
				b.WriteString(fmt.Sprintf("        <blockquote cite=\"%s\">%s</blockquote>\n",
					escapeAttr(sourceURL), escapeHTML(quote)))
			} else {
				b.WriteString(fmt.Sprintf("        <blockquote>%s</blockquote>\n", escapeHTML(quote)))
			}

			// Cite block: optional author photo, name, role @ company.
			b.WriteString("        <figcaption class=\"testimonial-cite\">\n")
			if photoID := dataString(item, "author_image_id"); photoID != "" {
				name := dataString(item, "author_name")
				b.WriteString("          " + renderMediaImg(photoID, name, name, "testimonial-author-img", mediaByID) + "\n")
			}
			b.WriteString("          <div class=\"testimonial-author\">\n")
			if name := dataString(item, "author_name"); name != "" {
				b.WriteString(fmt.Sprintf("            <span class=\"testimonial-author-name\">%s</span>\n", escapeHTML(name)))
			}
			role := dataString(item, "author_role")
			company := dataString(item, "author_company")
			switch {
			case role != "" && company != "":
				b.WriteString(fmt.Sprintf("            <span class=\"testimonial-author-meta\">%s, %s</span>\n", escapeHTML(role), escapeHTML(company)))
			case role != "":
				b.WriteString(fmt.Sprintf("            <span class=\"testimonial-author-meta\">%s</span>\n", escapeHTML(role)))
			case company != "":
				b.WriteString(fmt.Sprintf("            <span class=\"testimonial-author-meta\">%s</span>\n", escapeHTML(company)))
			}
			b.WriteString("          </div>\n")
			if logoID := dataString(item, "company_logo_id"); logoID != "" {
				b.WriteString("          " + renderMediaImg(logoID, dataString(item, "author_company"), dataString(item, "author_company"), "testimonial-company-logo", mediaByID) + "\n")
			}
			b.WriteString("        </figcaption>\n")
			b.WriteString("      </li>\n")
		}
		b.WriteString("    </ul>\n")
	}
	if footer := dataString(data, "footer"); footer != "" {
		if footerURL := dataString(data, "footer_url"); footerURL != "" {
			b.WriteString(fmt.Sprintf("    <a class=\"testimonial-footer\" href=\"%s\">%s</a>\n", escapeURL(footerURL), escapeHTML(footer)))
		} else {
			b.WriteString(fmt.Sprintf("    <p class=\"testimonial-footer\">%s</p>\n", escapeHTML(footer)))
		}
	}
	b.WriteString("  </section>\n")
	return b.String()
}

// parseRating coerces an operator-supplied rating string (e.g. "5", "4.5",
// "3 stars") into an integer in [0, 5]. Returns 0 when the value is empty
// or unparseable so the renderer skips the rating row entirely.
func parseRating(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	// Take the leading digit (operators sometimes type "5 stars" / "4/5").
	for _, r := range s {
		if r >= '0' && r <= '9' {
			v := int(r - '0')
			if v < 0 {
				return 0
			}
			if v > 5 {
				return 5
			}
			return v
		}
		break
	}
	return 0
}

// renderLogoStripBlock renders a row of customer/partner logos. Each item:
// {image_id, alt, href?}.
func renderLogoStripBlock(data map[string]any, mediaByID map[string]store.Medium) string {
	var b strings.Builder
	b.WriteString("  <section class=\"block block--logo_strip\">\n")
	if label := dataString(data, "label"); label != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"logo-strip-label\">%s</p>\n", escapeHTML(label)))
	}
	if itemsRaw, ok := data["items"].([]any); ok && len(itemsRaw) > 0 {
		b.WriteString("    <ul class=\"logo-strip\">\n")
		for _, it := range itemsRaw {
			item, ok := it.(map[string]any)
			if !ok {
				continue
			}
			imageID := dataString(item, "image_id")
			if imageID == "" {
				continue
			}
			alt := dataString(item, "alt")
			img := renderMediaImg(imageID, alt, alt, "", mediaByID)
			if href := dataString(item, "href"); href != "" {
				b.WriteString(fmt.Sprintf("      <li><a href=\"%s\">%s</a></li>\n", escapeURL(href), img))
			} else {
				b.WriteString(fmt.Sprintf("      <li>%s</li>\n", img))
			}
		}
		b.WriteString("    </ul>\n")
	}
	b.WriteString("  </section>\n")
	return b.String()
}

// renderCodeBlock renders a monospace code presentation with optional
// language label. Data: {language?, code, label?}. The eval-friendly
// shape so technical-blog-style pages can ship code without raw HTML.
func renderCodeBlock(data map[string]any) string {
	var b strings.Builder
	b.WriteString("  <section class=\"block block--code_block\">\n")
	if heading := dataString(data, "heading"); heading != "" {
		b.WriteString(fmt.Sprintf("    <h2>%s</h2>\n", escapeHTML(heading)))
	}
	if lang := dataString(data, "language"); lang != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"code-label\">%s</p>\n", escapeHTML(lang)))
	}
	if code := dataString(data, "code"); code != "" {
		b.WriteString(fmt.Sprintf("    <pre><code>%s</code></pre>\n", escapeHTML(code)))
	}
	b.WriteString("  </section>\n")
	return b.String()
}

// renderFormBlock renders a basic HTML form. Data shape:
// {heading?, subheading?, action, method?, fields: [{name, type, label, placeholder?, required?, options?, visible_if_field?, visible_if_equals?, step?}], submit_label, next_label?, previous_label?}.
// type can be: text, email, tel, url, textarea, select, checkbox, radio.
// Browser submits to action; the operator wires the receiving endpoint.
//
// Sprint 5 quick-win (2026-05-24) extends the renderer with two
// additive behaviours wired by a single inline form script:
//  1. Conditional field visibility via visible_if_field +
//     visible_if_equals. Empty visible_if_equals = "any non-empty
//     value on the gating field". Required fields are auto-unrequired
//     while hidden so the form doesn't fail submit on data the user
//     can't see.
//  2. Multi-step forms via step (number, default 0). When any field
//     has step >= 1 the form auto-groups all fields by step and
//     renders Next / Previous buttons; the submit button only shows
//     on the highest step. Honeypot + ungrouped fields (step=0) live
//     on step 1.
func renderFormBlock(data map[string]any) string {
	var b strings.Builder
	b.WriteString("  <section class=\"block block--form\">\n")
	if heading := dataString(data, "heading"); heading != "" {
		b.WriteString(fmt.Sprintf("    <h2>%s</h2>\n", escapeHTML(heading)))
	}
	if sub := dataString(data, "subheading"); sub != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"subheading\">%s</p>\n", escapeHTML(sub)))
	}
	action := dataString(data, "action")
	formID := dataString(data, "form_id")
	// Sprint 3 (2026-05-04): when the editor wired the block to a
	// stored form via form_id but the operator did not supply a
	// custom action URL, default to the platform's public submit
	// endpoint. Same-origin via Caddy in front of the API; CORS is
	// handled at the API for the cross-origin case.
	if action == "" && formID != "" {
		action = "/t/forms/" + formID + "/submit"
	}
	if action == "" {
		action = "#"
	}
	method := strings.ToLower(dataString(data, "method"))
	if method != "get" {
		method = "post"
	}
	formIDAttr := ""
	if formID != "" {
		formIDAttr = fmt.Sprintf(" data-form-id=\"%s\"", escapeAttr(formID))
	}

	// Multi-step detection: scan once for the highest step value. If
	// every field has step=0 (or omitted) the form renders the
	// classic single-step layout and we skip the step-wrapper markup.
	maxStep := int64(0)
	fieldsRaw, _ := data["fields"].([]any)
	for _, f := range fieldsRaw {
		field, ok := f.(map[string]any)
		if !ok {
			continue
		}
		if s := numberFromData(field, "step"); s > maxStep {
			maxStep = s
		}
	}
	hasMultiStep := maxStep > 0
	// Conditional visibility detection: any field carries a
	// visible_if_field? Used to decide whether to emit the runtime
	// script at all so single-step / no-conditional forms stay
	// markup-only.
	hasConditional := false
	for _, f := range fieldsRaw {
		field, ok := f.(map[string]any)
		if !ok {
			continue
		}
		if dataString(field, "visible_if_field") != "" {
			hasConditional = true
			break
		}
	}

	formClasses := "block-form"
	if hasMultiStep {
		formClasses += " block-form--multi-step"
	}
	b.WriteString(fmt.Sprintf("    <form class=\"%s\" action=\"%s\" method=\"%s\"%s>\n", formClasses, escapeURL(action), method, formIDAttr))
	// Honeypot field. Bots that auto-fill all visible inputs trip
	// this and the public submit endpoint silently drops the row.
	// Real users never see the field (off-screen + tabindex=-1).
	b.WriteString("      <input type=\"text\" name=\"website\" tabindex=\"-1\" autocomplete=\"off\" aria-hidden=\"true\" style=\"position:absolute;left:-9999px;width:1px;height:1px;opacity:0;\" />\n")

	// Group fields by step for multi-step rendering. step=0 (or
	// missing) lands on step 1 alongside any other ungrouped fields.
	// Single-step forms see this collapse into one group at "1".
	stepGroups := map[int64][]map[string]any{}
	stepOrder := []int64{}
	for _, f := range fieldsRaw {
		field, ok := f.(map[string]any)
		if !ok {
			continue
		}
		step := numberFromData(field, "step")
		if step <= 0 {
			step = 1
		}
		if _, seen := stepGroups[step]; !seen {
			stepOrder = append(stepOrder, step)
		}
		stepGroups[step] = append(stepGroups[step], field)
	}
	// Sort step keys ascending so step 1 renders first.
	for i := 1; i < len(stepOrder); i++ {
		for j := i; j > 0 && stepOrder[j-1] > stepOrder[j]; j-- {
			stepOrder[j-1], stepOrder[j] = stepOrder[j], stepOrder[j-1]
		}
	}
	if len(stepOrder) == 0 {
		stepOrder = []int64{1}
	}

	for idx, step := range stepOrder {
		isFirst := idx == 0
		isLast := idx == len(stepOrder)-1
		// Wrap each step in a <fieldset> with data-form-step so the
		// runtime script can toggle visibility step-by-step. When the
		// form has only one step the fieldset stays visible
		// unconditionally and the Next/Previous buttons never render,
		// so the markup degrades cleanly without JS.
		hiddenAttr := ""
		if hasMultiStep && !isFirst {
			hiddenAttr = " hidden"
		}
		if hasMultiStep {
			b.WriteString(fmt.Sprintf("      <fieldset class=\"form-step\" data-form-step=\"%d\"%s>\n", step, hiddenAttr))
		}
		for _, field := range stepGroups[step] {
			name := dataString(field, "name")
			ftype := dataString(field, "type")
			label := dataString(field, "label")
			placeholder := dataString(field, "placeholder")
			required, _ := field["required"].(bool)
			if name == "" || ftype == "" {
				continue
			}
			visIfField := dataString(field, "visible_if_field")
			visIfEquals := dataString(field, "visible_if_equals")
			fieldAttrs := ""
			fieldHidden := ""
			if visIfField != "" {
				fieldAttrs = fmt.Sprintf(" data-visible-if-field=\"%s\" data-visible-if-equals=\"%s\"",
					escapeAttr(visIfField), escapeAttr(visIfEquals))
				// Hidden by default; the script reveals on first
				// evaluation so flash-of-visible-content never
				// happens. The hidden attribute also drops the field
				// from form submission per native browser semantics
				// + tabbing order.
				fieldHidden = " hidden"
			}
			b.WriteString(fmt.Sprintf("      <div class=\"form-field\"%s%s>\n", fieldAttrs, fieldHidden))
			if label != "" {
				b.WriteString(fmt.Sprintf("        <label for=\"f-%s\">%s</label>\n", escapeAttr(name), escapeHTML(label)))
			}
			reqAttr := ""
			if required {
				// Conditional fields use data-required + skip the
				// native required attribute, so the form doesn't
				// reject submit on a field the user can't see. The
				// runtime script re-applies the native required only
				// while visible.
				if visIfField != "" {
					reqAttr = " data-required"
				} else {
					reqAttr = " required"
				}
			}
			switch ftype {
			case "textarea":
				b.WriteString(fmt.Sprintf("        <textarea id=\"f-%s\" name=\"%s\" placeholder=\"%s\"%s></textarea>\n",
					escapeAttr(name), escapeAttr(name), escapeAttr(placeholder), reqAttr))
			case "select":
				b.WriteString(fmt.Sprintf("        <select id=\"f-%s\" name=\"%s\"%s>\n",
					escapeAttr(name), escapeAttr(name), reqAttr))
				if optsRaw, ok := field["options"].([]any); ok {
					for _, o := range optsRaw {
						if os, ok := o.(string); ok {
							b.WriteString(fmt.Sprintf("          <option value=\"%s\">%s</option>\n",
								escapeAttr(os), escapeHTML(os)))
						}
					}
				}
				b.WriteString("        </select>\n")
			default:
				// text, email, tel, url, checkbox, radio (basic: no group support yet).
				b.WriteString(fmt.Sprintf("        <input type=\"%s\" id=\"f-%s\" name=\"%s\" placeholder=\"%s\"%s />\n",
					escapeAttr(ftype), escapeAttr(name), escapeAttr(name), escapeAttr(placeholder), reqAttr))
			}
			b.WriteString("      </div>\n")
		}
		if hasMultiStep {
			// Step nav buttons inside the fieldset so they hide
			// alongside their step.
			previousLabel := dataString(data, "previous_label")
			if previousLabel == "" {
				previousLabel = "Back"
			}
			nextLabel := dataString(data, "next_label")
			if nextLabel == "" {
				nextLabel = "Next"
			}
			b.WriteString("        <div class=\"form-step-nav\">\n")
			if !isFirst {
				b.WriteString(fmt.Sprintf("          <button type=\"button\" data-form-prev>%s</button>\n", escapeHTML(previousLabel)))
			}
			if !isLast {
				b.WriteString(fmt.Sprintf("          <button type=\"button\" data-form-next>%s</button>\n", escapeHTML(nextLabel)))
			}
			b.WriteString("        </div>\n")
			b.WriteString("      </fieldset>\n")
		}
	}

	submitLabel := dataString(data, "submit_label")
	if submitLabel == "" {
		submitLabel = "Submit"
	}
	// Submit button lives OUTSIDE any step fieldset so single-step
	// forms render it inline AND multi-step forms only show it on
	// the final step (the script toggles hidden on it based on which
	// step is active). When single-step, the script doesn't run and
	// the button stays visible by default.
	submitHidden := ""
	if hasMultiStep {
		submitHidden = " hidden"
	}
	b.WriteString(fmt.Sprintf("      <button type=\"submit\" data-form-submit%s>%s</button>\n", submitHidden, escapeHTML(submitLabel)))
	b.WriteString("    </form>\n")

	// Inline form-logic script: toggles conditional fields + steps.
	// Same CSP-safe inline pattern as the other small per-form
	// scripts in the codebase (visitor hydration, storefront
	// island). Emitted only when needed.
	if hasConditional || hasMultiStep {
		b.WriteString(formLogicScript(hasConditional, hasMultiStep))
	}

	b.WriteString("  </section>\n")
	return b.String()
}

// formLogicScript returns the inline vanilla-JS handler that powers
// conditional-visibility + multi-step gating on the form. CSP-safe
// (inline allowed under the existing 'unsafe-inline' relaxation OR
// hashed by Caddy when the operator pins a strict CSP). Idle cost on
// non-form pages is zero because the script lives inside the form
// block's own markup; no global event listeners.
func formLogicScript(conditional, multiStep bool) string {
	var b strings.Builder
	b.WriteString("    <script>(function(){\n")
	b.WriteString("      var script=document.currentScript; var form=script&&script.previousElementSibling; while(form&&form.tagName!=='FORM'){form=form.previousElementSibling;}\n")
	b.WriteString("      if(!form){return;}\n")
	if conditional {
		b.WriteString("      function evalConditional(){\n")
		b.WriteString("        var gated=form.querySelectorAll('[data-visible-if-field]');\n")
		b.WriteString("        for(var i=0;i<gated.length;i++){\n")
		b.WriteString("          var el=gated[i]; var fieldName=el.getAttribute('data-visible-if-field'); var needs=el.getAttribute('data-visible-if-equals')||'';\n")
		b.WriteString("          var src=form.querySelector('[name=\"'+fieldName+'\"]'); var val=src?(src.type==='checkbox'?(src.checked?'1':''):src.value):'';\n")
		b.WriteString("          var show=needs===''?(val!==''):(val===needs);\n")
		b.WriteString("          if(show){el.removeAttribute('hidden');}else{el.setAttribute('hidden','');}\n")
		b.WriteString("          var dr=el.querySelectorAll('[data-required]'); for(var j=0;j<dr.length;j++){ if(show){dr[j].setAttribute('required','');}else{dr[j].removeAttribute('required');} }\n")
		b.WriteString("        }\n")
		b.WriteString("      }\n")
		b.WriteString("      form.addEventListener('input',evalConditional); form.addEventListener('change',evalConditional); evalConditional();\n")
	}
	if multiStep {
		b.WriteString("      var steps=form.querySelectorAll('[data-form-step]'); var current=0; var submitBtn=form.querySelector('[data-form-submit]');\n")
		b.WriteString("      function showStep(idx){ for(var i=0;i<steps.length;i++){ if(i===idx){steps[i].removeAttribute('hidden');}else{steps[i].setAttribute('hidden','');} } if(submitBtn){ if(idx===steps.length-1){submitBtn.removeAttribute('hidden');}else{submitBtn.setAttribute('hidden','');} } current=idx; }\n")
		b.WriteString("      form.addEventListener('click',function(e){ var t=e.target; if(t.matches&&t.matches('[data-form-next]')){e.preventDefault(); if(current<steps.length-1)showStep(current+1);} else if(t.matches&&t.matches('[data-form-prev]')){e.preventDefault(); if(current>0)showStep(current-1);} });\n")
		b.WriteString("      showStep(0);\n")
	}
	b.WriteString("    })();</script>\n")
	return b.String()
}

// renderLogoCarouselBlock renders a horizontal infinite-scroll marquee of
// customer / partner logos. Pure CSS animation (no JS). Items duplicate
// once in markup so the loop is seamless. Use under hero on a real
// production marketing site to anchor trust before the first feature
// section. Each item: {image_id, alt, href?} OR {label, href?} for
// text-mark logos when an image isn't available.
func renderLogoCarouselBlock(data map[string]any, mediaByID map[string]store.Medium) string {
	var b strings.Builder
	b.WriteString("  <section class=\"block block--logo_carousel\">\n")
	if label := dataString(data, "label"); label != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"logo-carousel-label\">%s</p>\n", escapeHTML(label)))
	}
	itemsRaw, ok := data["items"].([]any)
	if !ok || len(itemsRaw) == 0 {
		b.WriteString("  </section>\n")
		return b.String()
	}
	// Render the inner ul twice so the CSS animation can loop seamlessly.
	b.WriteString("    <div class=\"logo-carousel\" aria-label=\"Trusted by\">\n")
	for pass := 0; pass < 2; pass++ {
		ariaHidden := ""
		if pass == 1 {
			ariaHidden = ` aria-hidden="true"`
		}
		b.WriteString(fmt.Sprintf("      <ul class=\"logo-carousel-track\"%s>\n", ariaHidden))
		for _, it := range itemsRaw {
			item, ok := it.(map[string]any)
			if !ok {
				continue
			}
			imageID := dataString(item, "image_id")
			label := dataString(item, "label")
			alt := dataString(item, "alt")
			if alt == "" {
				alt = label
			}
			if imageID == "" && label == "" {
				continue
			}
			href := dataString(item, "href")
			b.WriteString("        <li>")
			if href != "" {
				b.WriteString(fmt.Sprintf(`<a href="%s">`, escapeURL(href)))
			}
			if imageID != "" {
				b.WriteString(renderMediaImg(imageID, alt, label, "carousel-img", mediaByID))
			} else {
				b.WriteString(fmt.Sprintf(`<span class="carousel-mark">%s</span>`, escapeHTML(label)))
			}
			if href != "" {
				b.WriteString("</a>")
			}
			b.WriteString("</li>\n")
		}
		b.WriteString("      </ul>\n")
	}
	b.WriteString("    </div>\n")
	b.WriteString("  </section>\n")
	return b.String()
}

// renderReplacementGridBlock renders a bento-style grid of "old → new"
// replacement cards. Each card shows the SaaS being replaced with a
// strikethrough on the "from" name + arrow + bold replacement, plus a
// short description below. Some items can span 2 columns via
// items[i].span ("wide") to break up the grid rhythm. Matches the
// brightinteraction.com /#services bento pattern.
//
// Each item: {from, to, description, span?, icon?}.
// span values: "" (1 col, default), "wide" (2 cols on md+).
func renderReplacementGridBlock(data map[string]any) string {
	var b strings.Builder
	b.WriteString("  <section class=\"block block--replacement_grid\">\n")
	if eyebrow := dataString(data, "eyebrow"); eyebrow != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"eyebrow\">%s</p>\n", escapeHTML(eyebrow)))
	}
	if heading := dataString(data, "heading"); heading != "" {
		b.WriteString(fmt.Sprintf("    <h2>%s</h2>\n", escapeHTML(heading)))
	}
	if sub := dataString(data, "subheading"); sub != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"subheading\">%s</p>\n", escapeHTML(sub)))
	}
	if itemsRaw, ok := data["items"].([]any); ok && len(itemsRaw) > 0 {
		b.WriteString("    <ul class=\"replacement-grid\">\n")
		for _, it := range itemsRaw {
			item, ok := it.(map[string]any)
			if !ok {
				continue
			}
			from := dataString(item, "from")
			to := dataString(item, "to")
			desc := dataString(item, "description")
			span := dataString(item, "span")
			cls := "replacement-card"
			if span == "wide" {
				cls += " is-wide"
			}
			b.WriteString(fmt.Sprintf("      <li class=\"%s\">\n", cls))
			b.WriteString("        <div class=\"replacement-pair\">\n")
			if from != "" {
				b.WriteString(fmt.Sprintf("          <span class=\"replacement-from\">%s</span>\n", escapeHTML(from)))
			}
			b.WriteString("          <svg class=\"replacement-arrow\" viewBox=\"0 0 24 24\" width=\"16\" height=\"16\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"2\" stroke-linecap=\"round\" stroke-linejoin=\"round\" aria-hidden=\"true\"><path d=\"M5 12h14\"/><path d=\"m12 5 7 7-7 7\"/></svg>\n")
			if to != "" {
				b.WriteString(fmt.Sprintf("          <span class=\"replacement-to\">%s</span>\n", escapeHTML(to)))
			}
			b.WriteString("        </div>\n")
			if desc != "" {
				b.WriteString(fmt.Sprintf("        <p class=\"replacement-desc\">%s</p>\n", escapeHTML(desc)))
			}
			b.WriteString("      </li>\n")
		}
		b.WriteString("    </ul>\n")
	}
	if footer := dataString(data, "footer"); footer != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"replacement-footer\">%s</p>\n", escapeHTML(footer)))
	}
	b.WriteString("  </section>\n")
	return b.String()
}

// renderCalculatorBlock is intentionally NOT registered in renderDataBlock's
// switch. Calculators are bespoke per customer (the cost formula, the
// services list, the currency, the layout, all site-specific). Atomicsite
// provides the general primitives (form, feature_grid, replacement_grid,
// embed, raw) so an agent or developer can hand-build the calculator for
// the customer who needs one. This function is kept as a reference shape
// for that custom build, demonstrating the right pattern: data-config JSON
// + inline <script> hydrated by a small vanilla-JS reader. See the demo
// site's component catalog if you need the actual implementation.
func renderCalculatorBlock(data map[string]any) string {
	var b strings.Builder
	b.WriteString("  <section class=\"block block--calculator\">\n")
	if eyebrow := dataString(data, "eyebrow"); eyebrow != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"eyebrow\">%s</p>\n", escapeHTML(eyebrow)))
	}
	if heading := dataString(data, "heading"); heading != "" {
		b.WriteString(fmt.Sprintf("    <h2>%s</h2>\n", escapeHTML(heading)))
	}
	if sub := dataString(data, "subheading"); sub != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"subheading\">%s</p>\n", escapeHTML(sub)))
	}

	// JSON config blob the embedded JS reads. We pass it via a JSON-encoded
	// data attribute (rather than a separate <script> tag) so the CSP path
	// stays clean.
	configBytes, _ := json.Marshal(map[string]any{
		"services":           data["services"],
		"currency_symbol":    dataString(data, "currency_symbol"),
		"currency_rate":      data["currency_rate"],
		"team_size_min":      data["team_size_min"],
		"team_size_max":      data["team_size_max"],
		"team_size_default":  data["team_size_default"],
		"hardware_monthly":   data["hardware_monthly"],
		"maintenance_yearly": data["maintenance_yearly"],
		"labels":             data["labels"],
	})

	b.WriteString(fmt.Sprintf("    <div class=\"calculator-root\" data-config='%s'>\n",
		escapeAttr(string(configBytes))))

	// Service tile grid. Initial markup uses the operator-supplied selected
	// flags so the form looks right pre-hydration; the JS replays the same
	// state on load and then takes over.
	b.WriteString("      <div class=\"calculator-services\" role=\"group\" aria-label=\"Pick the tools you currently pay for\">\n")
	if servicesRaw, ok := data["services"].([]any); ok {
		for _, sv := range servicesRaw {
			svc, ok := sv.(map[string]any)
			if !ok {
				continue
			}
			key := dataString(svc, "key")
			label := dataString(svc, "label")
			brand := dataString(svc, "saas_brand")
			selected, _ := svc["selected"].(bool)
			selCls := ""
			if selected {
				selCls = " is-selected"
			}
			b.WriteString(fmt.Sprintf("        <button type=\"button\" class=\"calculator-service%s\" data-key=\"%s\" data-selected=\"%t\">\n",
				selCls, escapeAttr(key), selected))
			b.WriteString("          <span class=\"calculator-service-body\">\n")
			b.WriteString(fmt.Sprintf("            <span class=\"calculator-service-label\">%s</span>\n", escapeHTML(label)))
			if brand != "" {
				b.WriteString(fmt.Sprintf("            <span class=\"calculator-service-brand\">%s</span>\n", escapeHTML(brand)))
			}
			b.WriteString("          </span>\n")
			b.WriteString("          <span class=\"calculator-service-check\" aria-hidden=\"true\">✓</span>\n")
			b.WriteString("        </button>\n")
		}
	}
	b.WriteString("      </div>\n")

	// Team-size slider.
	teamMin := intOrDefault(data["team_size_min"], 1)
	teamMax := intOrDefault(data["team_size_max"], 100)
	teamDefault := intOrDefault(data["team_size_default"], 10)
	b.WriteString("      <div class=\"calculator-slider\">\n")
	b.WriteString("        <label for=\"calc-team-size\">Team size</label>\n")
	b.WriteString(fmt.Sprintf("        <input type=\"range\" id=\"calc-team-size\" min=\"%d\" max=\"%d\" value=\"%d\" />\n",
		teamMin, teamMax, teamDefault))
	b.WriteString(fmt.Sprintf("        <output for=\"calc-team-size\" id=\"calc-team-display\">%d</output>\n", teamDefault))
	b.WriteString("        <span class=\"calculator-slider-unit\">people</span>\n")
	b.WriteString("      </div>\n")

	// Three-column result. JS fills in the numbers; markup is the skeleton.
	b.WriteString("      <div class=\"calculator-results\">\n")
	for _, col := range []struct {
		key, title, kind string
	}{
		{"saas", "Your SaaS cost", "saas"},
		{"hardware", "Hardware only", "hardware"},
		{"selfhosted", "Self-hosted with us", "selfhosted"},
	} {
		b.WriteString(fmt.Sprintf("        <div class=\"calculator-col is-%s\">\n", col.kind))
		b.WriteString(fmt.Sprintf("          <p class=\"calculator-col-label\">%s</p>\n", col.title))
		b.WriteString(fmt.Sprintf("          <p class=\"calculator-col-monthly\" data-out=\"%s-monthly\">—</p>\n", col.key))
		b.WriteString(fmt.Sprintf("          <p class=\"calculator-col-yearly\" data-out=\"%s-yearly\">—</p>\n", col.key))
		b.WriteString("        </div>\n")
	}
	b.WriteString("      </div>\n")

	if ctaText := dataString(data, "cta_text"); ctaText != "" {
		ctaURL := dataString(data, "cta_url")
		if ctaURL == "" {
			ctaURL = "#"
		}
		b.WriteString(fmt.Sprintf("      <div class=\"calculator-cta\"><a href=\"%s\" class=\"btn-primary\">%s</a></div>\n",
			escapeURL(ctaURL), escapeHTML(ctaText)))
	}
	b.WriteString("    </div>\n")
	b.WriteString("  </section>\n")
	return b.String()
}

func intOrDefault(v any, def int) int {
	if v == nil {
		return def
	}
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		// best-effort parse
		var n int
		_, err := fmt.Sscanf(x, "%d", &n)
		if err == nil {
			return n
		}
	}
	return def
}

// renderCustomBlock is the agnostic escape-hatch block. The agent writes
// arbitrary markup (Tailwind classes, atomicsite design tokens, semantic
// HTML); atomicsite emits it verbatim wrapped in a <section> with auto-id
// from the block's name. Used when a customer needs a layout that doesn't
// fit the typed templates (hero, feature_grid, pricing, etc.).
//
// Data shape: {name, markup, eyebrow?, props?}.
//   - name: human label, also drives the section id (slugified) so cross-
//     section anchors resolve.
//   - markup: full HTML for the section body. atomicsite's design-token
//     CSS variables (--color-primary, --color-text, --color-bg, --bp-*,
//     --container-width, etc.) are available; baseline utility classes
//     (.btn-primary, .btn-secondary, .actions, .container) work too.
//   - eyebrow: optional small-caps label rendered before the markup.
//   - props: optional object the agent can read at edit time (e.g. for
//     decisions in a future code-mod step). Not rendered.
//
// Security: the existing block-time guardrail rejects <script> and
// <iframe> in markup unless explicitly approved; agent-supplied alt
// text + plaintext-email rules still apply. For iframes use the `embed`
// block (CSP-aware); for scripts use a registered component (component
// block_type).
func renderCustomBlock(data map[string]any) string {
	var b strings.Builder
	b.WriteString("  <section class=\"block block--custom\">\n")
	if eyebrow := dataString(data, "eyebrow"); eyebrow != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"eyebrow\">%s</p>\n", escapeHTML(eyebrow)))
	}
	if markup := dataString(data, "markup"); markup != "" {
		b.WriteString("    " + markup + "\n")
	}
	b.WriteString("  </section>\n")
	return b.String()
}

// renderRawAstroBlock emits author-supplied Astro/HTML/Tailwind code
// verbatim into the page Astro source. The escape hatch for sections
// where the typed block_types can't reach pixel parity (e.g. cloning
// brightinteraction.com's hero exactly).
//
// Data:
//
//	code:  full Astro markup string. Wrapped in a <section> only when
//	       the supplied code doesn't already start with one.
//	class: optional class added to the auto-generated <section>.
//
// Trade-off: NO renderer-side validation, NO auto-section-id, NO
// alternating-bg, NO eyebrow CSS, NO accent-span helper. Author owns
// the markup. If the build fails because the code has a syntax error,
// trigger_build returns the Astro error log so the agent can fix.
//
// Security: this block runs through the same _headers / CSP pipeline
// as everything else. A `<script>` here that isn't covered by
// security.allowed_scripts will be blocked at runtime by the meta CSP.
func renderRawAstroBlock(data map[string]any) string {
	code := dataString(data, "code")
	if code == "" {
		return "  <!-- raw_astro block: empty code -->\n"
	}
	trimmed := strings.TrimLeft(code, " \t\n")
	// If author already opens with a section/div/header/footer, emit
	// verbatim. Otherwise wrap in a <section> so the renderer's
	// auto-section-id helper has a hook.
	hasOuterTag := strings.HasPrefix(trimmed, "<section") ||
		strings.HasPrefix(trimmed, "<div") ||
		strings.HasPrefix(trimmed, "<header") ||
		strings.HasPrefix(trimmed, "<footer") ||
		strings.HasPrefix(trimmed, "<nav") ||
		strings.HasPrefix(trimmed, "<article") ||
		strings.HasPrefix(trimmed, "<aside") ||
		strings.HasPrefix(trimmed, "<main")
	if hasOuterTag {
		return "  " + code + "\n"
	}
	cls := "block block--raw_astro"
	if extra := dataString(data, "class"); extra != "" {
		cls += " " + extra
	}
	return fmt.Sprintf("  <section class=\"%s\">\n    %s\n  </section>\n", cls, code)
}

// renderProcessStepsBlock renders a numbered 4-up grid of process steps.
// Each step shows a big primary-coloured number + h3 title + body. Use for
// "How it works" / "Our process" / "How to get started" sections, the
// canonical 4-step marketing pattern.
//
// Each item: {number, title, description}. Number is a string (e.g. "01",
// "02") so the renderer doesn't second-guess formatting.
func renderProcessStepsBlock(data map[string]any) string {
	var b strings.Builder
	b.WriteString("  <section class=\"block block--process_steps\">\n")
	if eyebrow := dataString(data, "eyebrow"); eyebrow != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"eyebrow\">%s</p>\n", escapeHTML(eyebrow)))
	}
	if heading := dataString(data, "heading"); heading != "" {
		b.WriteString(fmt.Sprintf("    <h2>%s</h2>\n", escapeHTML(heading)))
	}
	if sub := dataString(data, "subheading"); sub != "" {
		b.WriteString(fmt.Sprintf("    <p class=\"subheading\">%s</p>\n", escapeHTML(sub)))
	}
	if itemsRaw, ok := data["items"].([]any); ok && len(itemsRaw) > 0 {
		b.WriteString("    <ol class=\"process-steps\">\n")
		for i, it := range itemsRaw {
			item, ok := it.(map[string]any)
			if !ok {
				continue
			}
			num := dataString(item, "number")
			if num == "" {
				num = fmt.Sprintf("%02d", i+1)
			}
			b.WriteString("      <li class=\"process-step\">\n")
			b.WriteString(fmt.Sprintf("        <div class=\"process-step-num\">%s</div>\n", escapeHTML(num)))
			if title := dataString(item, "title"); title != "" {
				b.WriteString(fmt.Sprintf("        <h3 class=\"process-step-title\">%s</h3>\n", escapeHTML(title)))
			}
			if desc := dataString(item, "description"); desc != "" {
				b.WriteString(fmt.Sprintf("        <p class=\"process-step-desc\">%s</p>\n", escapeHTML(desc)))
			}
			b.WriteString("      </li>\n")
		}
		b.WriteString("    </ol>\n")
	}
	b.WriteString("  </section>\n")
	return b.String()
}

// renderAboutSplitBlock renders a side-by-side about / founder section
// with content on the left + photo on the right (stacks on mobile, photo
// above content). Includes optional stats row + secondary text link CTA
// underneath. Use for founder bios, "About us", team intros, anywhere
// you want a personal photo paired with narrative.
//
// Data: {eyebrow, heading, paragraphs[], image_id, image_alt, image_position,
//
//	stats: [{value, label}], cta_text, cta_url}.
//
// image_position: "right" (default) or "left".
func renderAboutSplitBlock(data map[string]any, mediaByID map[string]store.Medium) string {
	var b strings.Builder
	pos := dataString(data, "image_position")
	cls := "block block--about_split"
	if pos == "left" {
		cls += " is-image-left"
	}
	b.WriteString(fmt.Sprintf("  <section class=\"%s\">\n", cls))
	b.WriteString("    <div class=\"about-split-content\">\n")
	if eyebrow := dataString(data, "eyebrow"); eyebrow != "" {
		b.WriteString(fmt.Sprintf("      <p class=\"eyebrow\">%s</p>\n", escapeHTML(eyebrow)))
	}
	if heading := dataString(data, "heading"); heading != "" {
		b.WriteString(fmt.Sprintf("      <h2>%s</h2>\n", escapeHTML(heading)))
	}
	if parasRaw, ok := data["paragraphs"].([]any); ok {
		for _, p := range parasRaw {
			if ps, ok := p.(string); ok && ps != "" {
				b.WriteString(fmt.Sprintf("      <p>%s</p>\n", escapeHTML(ps)))
			}
		}
	} else if text := dataString(data, "text"); text != "" {
		// Fall back to the same paragraph splitter the text block uses.
		renderTextParagraphs(&b, text)
	}
	if statsRaw, ok := data["stats"].([]any); ok && len(statsRaw) > 0 {
		b.WriteString("      <ul class=\"about-split-stats\">\n")
		for _, s := range statsRaw {
			st, ok := s.(map[string]any)
			if !ok {
				continue
			}
			b.WriteString("        <li>\n")
			if v := dataString(st, "value"); v != "" {
				b.WriteString(fmt.Sprintf("          <div class=\"about-split-stat-value\">%s</div>\n", escapeHTML(v)))
			}
			if l := dataString(st, "label"); l != "" {
				b.WriteString(fmt.Sprintf("          <div class=\"about-split-stat-label\">%s</div>\n", escapeHTML(l)))
			}
			b.WriteString("        </li>\n")
		}
		b.WriteString("      </ul>\n")
	}
	if ctaText := dataString(data, "cta_text"); ctaText != "" {
		ctaURL := dataString(data, "cta_url")
		if ctaURL == "" {
			ctaURL = "#"
		}
		b.WriteString(fmt.Sprintf("      <a href=\"%s\" class=\"about-split-link\">%s →</a>\n",
			escapeURL(ctaURL), escapeHTML(ctaText)))
	}
	b.WriteString("    </div>\n")
	b.WriteString("    <div class=\"about-split-image\">\n")
	if imageID := dataString(data, "image_id"); imageID != "" {
		b.WriteString("      " + renderMediaImg(imageID, dataString(data, "image_alt"), dataString(data, "heading"), "about-split-img", mediaByID) + "\n")
	}
	b.WriteString("    </div>\n")
	b.WriteString("  </section>\n")
	return b.String()
}

// renderEmbedBlock renders an iframe wrapped in an aspect-ratio container.
// The src host MUST already be in the trusted-domains allowlist (kind=frame)
// or the build will pass but the browser will block via CSP frame-src. The
// admin grants frame access via /sites/{id}/settings/allowed-scripts.
// Data: {src, title, aspect_ratio?}.
func renderEmbedBlock(data map[string]any) string {
	var b strings.Builder
	b.WriteString("  <section class=\"block block--embed\">\n")
	if heading := dataString(data, "heading"); heading != "" {
		b.WriteString(fmt.Sprintf("    <h2>%s</h2>\n", escapeHTML(heading)))
	}
	src := dataString(data, "src")
	if src != "" {
		title := dataString(data, "title")
		if title == "" {
			title = "Embedded content"
		}
		b.WriteString("    <div class=\"embed-wrapper\">\n")
		b.WriteString(fmt.Sprintf("      <iframe src=\"%s\" title=\"%s\" loading=\"lazy\" referrerpolicy=\"no-referrer-when-downgrade\"></iframe>\n",
			escapeURL(src), escapeAttr(title)))
		b.WriteString("    </div>\n")
	}
	b.WriteString("  </section>\n")
	return b.String()
}
