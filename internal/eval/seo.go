package eval

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"

	"github.com/bright-interaction/slab/internal/agent"
)

// CTA keywords used by Site Inspector's Meta Description Has CTA check.
// Matches the EN/SV/DE word lists in site-inspector/analyzers/seo.js so the
// two evaluators agree on whether a description "calls to action".
var ctaKeywords = regexp.MustCompile(`(?i)\b(learn|get|discover|find|try|start|book|free|download|read|see|check|explore|calculate|compare|shop|buy|order|save|join|sign up|subscribe|request|claim|unlock|läs|hämta|hitta|prova|börja|boka|gratis|ladda|köp|handla|beställ|spara|upptäck|jämför|registrera|testa|få|erfahren|entdecken|testen|starten|buchen|kostenlos|herunterladen|lesen|vergleichen|kaufen|bestellen|sparen|registrieren|anmelden|sehen|bekommen)\b`)

// genericAltRE matches a non-descriptive alt value on its own (case-insensitive).
var genericAltRE = regexp.MustCompile(`(?i)^\s*(image|photo|picture|img|graphic|icon)\.?\s*$`)

// emailRE finds plaintext email addresses in visible text. Same spirit as
// site-inspector's checkPlaintextEmails — used at Info severity in the eval
// because the real enforcement lives in the block-time guardrail.
var emailRE = regexp.MustCompile(`[\w.+-]+@[\w.-]+\.[A-Za-z]{2,}`)

// RunSEOChecks evaluates on-page + technical SEO across all pages, reporting
// per-page failures so the agent can see exactly which pages need fixes.
func RunSEOChecks(site *SiteContext) []CheckResult {
	var checks []CheckResult

	// Site-wide once-only checks
	checks = append(checks, checkRobotsTxt(site))
	checks = append(checks, checkSitemap(site))
	checks = append(checks, checkLLMsTxt(site))

	// Custom Collections checks (Sprint 4, 2026-05-04). All three
	// short-circuit when the site has no Collection-rendered pages
	// so single-page-builder sites never see a "fail".
	checks = append(checks, CheckCollectionJSONLDPresent(site))
	checks = append(checks, CheckCollectionHreflangAlternates(site))
	checks = append(checks, CheckCollectionInternalLinks(site))

	// Per-page checks aggregated into single results (reports first failing page)
	checks = append(checks, perPageCheck("Has Title", "on-page", 2, site, func(p PageContext) (bool, string) {
		t := titleOf(p.Doc)
		if t == "" {
			return false, "missing <title>"
		}
		return true, ""
	}, "Add a <title> tag via meta_title in the page settings."))

	checks = append(checks, perPageCheck("Title Length 30-60", "on-page", 1, site, func(p PageContext) (bool, string) {
		t := titleOf(p.Doc)
		n := utf8.RuneCountInString(t)
		if n < 30 || n > 60 {
			return false, fmt.Sprintf("title is %d chars (want 30-60)", n)
		}
		return true, ""
	}, "Aim for 30-60 character titles for optimal SERP display."))

	checks = append(checks, perPageCheck("Has Meta Description", "on-page", 2, site, func(p PageContext) (bool, string) {
		d := findMetaContent(p.Doc, "description")
		if d == "" {
			return false, "missing meta description"
		}
		return true, ""
	}, "Set meta_description in page settings."))

	checks = append(checks, perPageCheck("Meta Description 120-160", "on-page", 1, site, func(p PageContext) (bool, string) {
		d := findMetaContent(p.Doc, "description")
		n := utf8.RuneCountInString(d)
		if d == "" || n < 120 || n > 160 {
			return false, fmt.Sprintf("description is %d chars (want 120-160)", n)
		}
		return true, ""
	}, "Aim for 120-160 character meta descriptions."))

	checks = append(checks, perPageCheck("Has H1", "on-page", 2, site, func(p PageContext) (bool, string) {
		h1s := elementsByTag(p.Doc, "h1")
		if len(h1s) == 0 {
			return false, "no <h1> on page"
		}
		return true, ""
	}, "Every page must have exactly one <h1>."))

	checks = append(checks, perPageCheck("Single H1", "on-page", 1, site, func(p PageContext) (bool, string) {
		h1s := elementsByTag(p.Doc, "h1")
		if len(h1s) != 1 {
			return false, fmt.Sprintf("%d <h1> elements (want exactly 1)", len(h1s))
		}
		return true, ""
	}, "Use only one <h1> per page; subsequent headings should be <h2>+."))

	checks = append(checks, perPageCheck("Heading Hierarchy", "on-page", 2, site, func(p PageContext) (bool, string) {
		if ok, msg := validateHeadingOrder(p.Doc); !ok {
			return false, msg
		}
		return true, ""
	}, "Don't skip heading levels (e.g., H1 -> H3 without an H2 between)."))

	checks = append(checks, perPageCheck("Images Have Alt Text", "on-page", 2, site, func(p PageContext) (bool, string) {
		missing := 0
		for _, img := range elementsByTag(p.Doc, "img") {
			if !hasAttr(img, "alt") {
				missing++
			}
		}
		if missing > 0 {
			return false, fmt.Sprintf("%d image(s) missing alt attribute", missing)
		}
		return true, ""
	}, "Add alt text to every <img>. Use alt=\"\" for decorative images."))

	checks = append(checks, perPageCheck("Canonical URL", "technical", 2, site, func(p PageContext) (bool, string) {
		if findLinkHref(p.Doc, "canonical") == "" {
			return false, "no <link rel=canonical>"
		}
		return true, ""
	}, "Layouts.go emits canonical from the site domain."))

	checks = append(checks, perPageCheck("Language Declared", "technical", 1, site, func(p PageContext) (bool, string) {
		htmlNode := firstElementByTag(p.Doc, "html")
		if htmlNode == nil || attr(htmlNode, "lang") == "" {
			return false, "no lang attribute on <html>"
		}
		return true, ""
	}, "Set the lang= attribute on <html> from site settings."))

	checks = append(checks, perPageCheck("Viewport Meta", "technical", 2, site, func(p PageContext) (bool, string) {
		v := findMetaContent(p.Doc, "viewport")
		if v == "" {
			return false, "no viewport meta tag"
		}
		return true, ""
	}, "Add <meta name=viewport content=\"width=device-width, initial-scale=1\">."))

	checks = append(checks, perPageCheck("Charset Declared", "technical", 1, site, func(p PageContext) (bool, string) {
		for _, m := range elementsByTag(p.Doc, "meta") {
			if attr(m, "charset") != "" {
				return true, ""
			}
		}
		return false, "no <meta charset>"
	}, "Add <meta charset=\"UTF-8\"> as the first child of <head>."))

	checks = append(checks, perPageCheck("HTML5 Doctype", "technical", 1, site, func(p PageContext) (bool, string) {
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(p.HTML)), "<!doctype html>") {
			return false, "missing <!DOCTYPE html>"
		}
		return true, ""
	}, "Ensure pages start with <!DOCTYPE html>."))

	checks = append(checks, perPageCheck("Open Graph Tags", "social", 1, site, func(p PageContext) (bool, string) {
		missing := []string{}
		for _, prop := range []string{"og:title", "og:description", "og:image", "og:type", "og:url"} {
			if findMetaContent(p.Doc, prop) == "" {
				missing = append(missing, prop)
			}
		}
		if len(missing) > 0 {
			return false, "missing OG tags: " + strings.Join(missing, ", ")
		}
		return true, ""
	}, "Add Open Graph tags via the layout (og:title, og:description, og:image, og:type, og:url)."))

	checks = append(checks, perPageCheck("Not Noindexed", "technical", 3, site, func(p PageContext) (bool, string) {
		// Multiple sources: <meta name=robots>, the Base layout's robots prop
		robots := strings.ToLower(findMetaContent(p.Doc, "robots"))
		if strings.Contains(robots, "noindex") {
			return false, "page has noindex meta tag"
		}
		return true, ""
	}, "If you want this page indexed, set no_index=false on the page."))

	checks = append(checks, perPageCheck("URL Length < 75", "url", 1, site, func(p PageContext) (bool, string) {
		if len(p.Slug) >= 75 {
			return false, fmt.Sprintf("URL is %d chars (want < 75)", len(p.Slug))
		}
		return true, ""
	}, "Shorter URLs improve sharing and SERP display."))

	checks = append(checks, perPageCheck("URL Lowercase", "url", 1, site, func(p PageContext) (bool, string) {
		if p.Slug != strings.ToLower(p.Slug) {
			return false, "URL contains uppercase letters"
		}
		return true, ""
	}, "Use lowercase URLs only."))

	checks = append(checks, perPageCheck("URL No Underscores", "url", 1, site, func(p PageContext) (bool, string) {
		if strings.Contains(p.Slug, "_") {
			return false, "URL contains underscores"
		}
		return true, ""
	}, "Use hyphens (-), not underscores (_), in URLs."))

	checks = append(checks, perPageCheck("Has Internal Links", "on-page", 1, site, func(p PageContext) (bool, string) {
		count := 0
		for _, a := range elementsByTag(p.Doc, "a") {
			href := attr(a, "href")
			if href == "" || strings.HasPrefix(href, "#") {
				continue
			}
			if strings.HasPrefix(href, "/") || !strings.Contains(href, "://") {
				count++
			}
		}
		if count == 0 {
			return false, "no internal links on page"
		}
		return true, ""
	}, "Add internal links between pages to support crawling and topic clustering."))

	// --- New checks ported from upgraded site-inspector (2026-04-27) ---

	checks = append(checks, perPageCheck("Title Not Truncated", "on-page", 1, site, func(p PageContext) (bool, string) {
		t := titleOf(p.Doc)
		n := utf8.RuneCountInString(t)
		if n > 60 {
			return false, fmt.Sprintf("title is %d chars and will be truncated in SERPs", n)
		}
		return true, ""
	}, "Shorten title to 60 characters or fewer to prevent SERP truncation."))

	// Copy-taste checks: scored normally, but advisory in showcase
	// fidelity (a visual showcase landing legitimately runs sparse,
	// brand-led copy; the operator opted into that trade).
	if site.Fidelity == agent.FidelityShowcase {
		checks = append(checks,
			Info("Meta Description Has CTA", "on-page", "Advisory in showcase fidelity: CTA-verb descriptions still lift SERP click-through; unscored here."),
			Info("Content Length 300+", "on-page", "Advisory in showcase fidelity: sparse brand-led copy is allowed; note thin pages still rank worse."),
		)
	} else {
		checks = append(checks, perPageCheck("Meta Description Has CTA", "on-page", 1, site, func(p PageContext) (bool, string) {
			d := findMetaContent(p.Doc, "description")
			if d == "" {
				return true, "" // separate check covers missing description
			}
			if !ctaKeywords.MatchString(d) {
				return false, "no call-to-action verbs found in description"
			}
			return true, ""
		}, "Include action verbs (learn, discover, get, try, free, läs, prova, entdecken) in meta descriptions."))

		checks = append(checks, perPageCheck("Content Length 300+", "on-page", 1, site, func(p PageContext) (bool, string) {
			body := firstElementByTag(p.Doc, "body")
			if body == nil {
				return false, "no <body>"
			}
			words := len(strings.Fields(textContent(body)))
			if words < 300 {
				return false, fmt.Sprintf("%d words (want 300+)", words)
			}
			return true, ""
		}, "Pages under 300 words tend to underperform; expand the content or remove the page."))
	}

	checks = append(checks, perPageCheck("Descriptive Alt Text", "on-page", 2, site, func(p PageContext) (bool, string) {
		for _, img := range elementsByTag(p.Doc, "img") {
			alt := strings.TrimSpace(attr(img, "alt"))
			if alt == "" {
				continue // covered by Images Have Alt Text
			}
			if genericAltRE.MatchString(alt) {
				return false, fmt.Sprintf("generic alt text %q", alt)
			}
		}
		return true, ""
	}, "Replace generic alts like \"image\" or \"photo\" with descriptions of what the image shows."))

	checks = append(checks, perPageCheck("No Empty Links", "on-page", 1, site, func(p PageContext) (bool, string) {
		for _, a := range elementsByTag(p.Doc, "a") {
			if textContent(a) != "" || attr(a, "aria-label") != "" || attr(a, "title") != "" {
				continue
			}
			imgs := elementsByTag(a, "img")
			hasAltImg := false
			for _, im := range imgs {
				if strings.TrimSpace(attr(im, "alt")) != "" {
					hasAltImg = true
					break
				}
			}
			if hasAltImg {
				continue
			}
			return false, "anchor with no text, aria-label, or alt-bearing image"
		}
		return true, ""
	}, "Empty links confuse crawlers. Add visible text, aria-label, or wrap a non-empty alt image."))

	checks = append(checks, perPageCheck("No Broken Anchor Links", "on-page", 1, site, func(p PageContext) (bool, string) {
		ids := map[string]bool{}
		for _, n := range findAll(p.Doc, func(n *html.Node) bool {
			return n.Type == html.ElementNode && hasAttr(n, "id")
		}) {
			ids[attr(n, "id")] = true
		}
		for _, a := range elementsByTag(p.Doc, "a") {
			href := attr(a, "href")
			if !strings.HasPrefix(href, "#") || href == "#" {
				continue
			}
			target := strings.TrimPrefix(href, "#")
			if !ids[target] {
				return false, fmt.Sprintf("anchor href=\"#%s\" has no matching id on page", target)
			}
		}
		return true, ""
	}, "Fix or remove anchors pointing at IDs that don't exist on the page."))

	checks = append(checks, perPageCheck("Canonical Matches URL", "technical", 1, site, func(p PageContext) (bool, string) {
		canon := findLinkHref(p.Doc, "canonical")
		if canon == "" {
			return true, "" // covered by Canonical URL
		}
		// Scheme-tolerant compare: normalize the canonical's path tail vs the
		// page's slug. Site Inspector requires equality of the path; atomicsite
		// likewise expects the canonical to point at the page's own slug.
		canonPath := canon
		if i := strings.Index(canon, "://"); i >= 0 {
			rest := canon[i+3:]
			if j := strings.Index(rest, "/"); j >= 0 {
				canonPath = rest[j:]
			} else {
				canonPath = "/"
			}
		}
		canonPath = strings.TrimSuffix(canonPath, "/")
		want := strings.TrimSuffix(p.Slug, "/")
		if canonPath != want && canonPath != want+"/" && want != canonPath+"/" {
			return false, fmt.Sprintf("canonical %q does not match page slug %q", canon, p.Slug)
		}
		return true, ""
	}, "Canonical href must point at this page's own URL, not a parent or unrelated route."))

	checks = append(checks, perPageCheck("Favicon", "technical", 1, site, func(p PageContext) (bool, string) {
		for _, l := range elementsByTag(p.Doc, "link") {
			rel := strings.ToLower(attr(l, "rel"))
			if strings.Contains(rel, "icon") && !strings.Contains(rel, "apple-touch-icon") {
				return true, ""
			}
		}
		return false, "no <link rel=\"icon\">"
	}, "Layouts.go should emit <link rel=\"icon\" href=\"/favicon.ico\"> by default."))

	checks = append(checks, perPageCheck("Apple Touch Icon", "technical", 1, site, func(p PageContext) (bool, string) {
		for _, l := range elementsByTag(p.Doc, "link") {
			if strings.Contains(strings.ToLower(attr(l, "rel")), "apple-touch-icon") {
				return true, ""
			}
		}
		return false, "no <link rel=\"apple-touch-icon\">"
	}, "Layouts.go should emit <link rel=\"apple-touch-icon\" href=\"/apple-touch-icon.png\"> by default."))

	checks = append(checks, perPageCheck("No Duplicate Meta Tags", "technical", 1, site, func(p PageContext) (bool, string) {
		// Title and canonical must appear at most once. Description, og:image
		// and twitter:card same rule.
		if titles := elementsByTag(p.Doc, "title"); len(titles) > 1 {
			return false, fmt.Sprintf("%d <title> elements", len(titles))
		}
		canonCount := 0
		for _, l := range elementsByTag(p.Doc, "link") {
			if strings.EqualFold(attr(l, "rel"), "canonical") {
				canonCount++
			}
		}
		if canonCount > 1 {
			return false, fmt.Sprintf("%d <link rel=canonical> elements", canonCount)
		}
		seen := map[string]int{}
		for _, m := range elementsByTag(p.Doc, "meta") {
			for _, key := range []string{"name", "property"} {
				if v := strings.ToLower(attr(m, key)); v != "" {
					seen[v]++
				}
			}
		}
		for _, k := range []string{"description", "og:title", "og:description", "og:image", "og:url", "og:type", "twitter:card", "twitter:image"} {
			if seen[k] > 1 {
				return false, fmt.Sprintf("%d <meta %s> elements", seen[k], k)
			}
		}
		return true, ""
	}, "Emit each meta tag exactly once. Duplicates confuse search engines."))

	checks = append(checks, perPageCheck("OG Completeness", "social", 1, site, func(p PageContext) (bool, string) {
		var missing []string
		for _, prop := range []string{"og:title", "og:description", "og:image", "og:type", "og:url", "og:site_name", "og:locale"} {
			if findMetaContent(p.Doc, prop) == "" {
				missing = append(missing, prop)
			}
		}
		if len(missing) > 0 {
			return false, "missing " + strings.Join(missing, ", ")
		}
		return true, ""
	}, "Emit og:site_name and og:locale on every page in addition to title/description/image/type/url."))

	checks = append(checks, perPageCheck("OG Image Size 1200x630", "social", 1, site, func(p PageContext) (bool, string) {
		w := findMetaContent(p.Doc, "og:image:width")
		h := findMetaContent(p.Doc, "og:image:height")
		if w == "" || h == "" {
			return false, "missing og:image:width / og:image:height"
		}
		if w != "1200" || h != "630" {
			return false, fmt.Sprintf("og:image declares %sx%s (want 1200x630)", w, h)
		}
		return true, ""
	}, "Emit og:image:width=1200 and og:image:height=630 to match the recommended OG aspect ratio."))

	checks = append(checks, perPageCheck("Twitter Card", "social", 1, site, func(p PageContext) (bool, string) {
		c := findMetaContent(p.Doc, "twitter:card")
		if c == "" {
			return false, "no <meta name=twitter:card>"
		}
		return true, ""
	}, "Emit <meta name=twitter:card content=summary_large_image> in the layout."))

	checks = append(checks, perPageCheck("Twitter Image", "social", 1, site, func(p PageContext) (bool, string) {
		if findMetaContent(p.Doc, "twitter:image") == "" {
			return false, "no <meta name=twitter:image>"
		}
		return true, ""
	}, "Emit <meta name=twitter:image content=...> referencing the same OG image."))

	// No Plaintext Emails: Info severity only at eval time. Real enforcement
	// lives in the block-time guardrail (validateBlockEvalChecks).
	for _, p := range site.Pages {
		if body := firstElementByTag(p.Doc, "body"); body != nil {
			text := textContent(body)
			emails := emailRE.FindAllString(text, -1)
			// Filter out emails inside mailto: hrefs.
			var leaks []string
			for _, e := range emails {
				if !mailtoCovers(p.Doc, e) {
					leaks = append(leaks, e)
				}
			}
			if len(leaks) > 0 {
				r := Info("No Plaintext Emails", "technical",
					fmt.Sprintf("%d plaintext email(s) on %s: %s", len(leaks), p.Slug, strings.Join(firstN(leaks, 3), ", ")))
				r.Page = p.Slug
				r.Recommendation = "Use a contact form or wrap emails in <a href=\"mailto:\">. Block-time guardrail rejects new copy that leaks emails."
				checks = append(checks, r)
				break // one info row per build is enough
			}
		}
	}

	checks = append(checks, checkFAQVisibility(site))

	// Hreflang Tags. Multi-language sites should emit <link rel="alternate"
	// hreflang="X"> on every page that has a counterpart in another locale.
	// Mono-language sites pass automatically. Site Inspector tracks three
	// related checks (Tags / Self-Referencing / x-default); we collapse them
	// into one Pass-with-detail when the page emits a coherent set.
	checks = append(checks, perPageCheck("Hreflang Tags", "technical", 1, site, func(p PageContext) (bool, string) {
		alts := collectHreflangs(p.Doc)
		if !pageNeedsHreflang(p, site) {
			return true, "" // mono-language site or single-locale page
		}
		if len(alts) == 0 {
			return false, "page has multi-language counterparts but no <link rel=\"alternate\" hreflang> tags"
		}
		// Self-referencing: at least one alt must point at the current page's lang.
		htmlNode := firstElementByTag(p.Doc, "html")
		pageLang := strings.ToLower(strings.TrimSpace(attr(htmlNode, "lang")))
		hasSelf := false
		for _, a := range alts {
			if strings.EqualFold(a.lang, pageLang) {
				hasSelf = true
				break
			}
		}
		if !hasSelf {
			return false, fmt.Sprintf("hreflang set has no self-referencing entry for current locale %q", pageLang)
		}
		// x-default is a soft signal: warn-only if missing on the home page.
		return true, ""
	}, "Multi-language pages need <link rel=\"alternate\" hreflang=\"X\"> for every locale that has a counterpart, plus a self-referencing entry. Atomicsite emits these automatically when general.additional_langs is set."))

	// GEO / AEO checks (AI search readiness), same category, distinct section.
	checks = append(checks, RunGEOChecks(site)...)

	return checks
}

// hreflangAlt is one parsed <link rel="alternate" hreflang="X" href="Y">.
type hreflangAlt struct {
	lang string
	href string
}

// collectHreflangs walks the head and returns every alternate link with a
// hreflang attribute.
func collectHreflangs(doc *html.Node) []hreflangAlt {
	var out []hreflangAlt
	for _, l := range elementsByTag(doc, "link") {
		if !strings.Contains(strings.ToLower(attr(l, "rel")), "alternate") {
			continue
		}
		hl := attr(l, "hreflang")
		if hl == "" {
			continue
		}
		out = append(out, hreflangAlt{lang: hl, href: attr(l, "href")})
	}
	return out
}

// pageNeedsHreflang reports whether the page has any reason to be checked
// for hreflang emission. A site is multi-language only when there is
// REAL evidence: two distinct locale prefixes, or a prefixed page whose
// unprefixed counterpart also exists (/sv/about alongside /about). A
// single 2-letter first segment used to flip the entire site to
// "multi-language" and fail hreflang on every page (e.g. a monolingual
// site with a /go/tools section).
func pageNeedsHreflang(p PageContext, site *SiteContext) bool {
	if len(site.Pages) < 2 {
		return false
	}
	slugs := map[string]bool{}
	for _, q := range site.Pages {
		slugs[strings.TrimPrefix(q.Slug, "/")] = true
	}
	isLocalePrefix := func(s string) bool {
		if len(s) != 2 {
			return false
		}
		return s[0] >= 'a' && s[0] <= 'z' && s[1] >= 'a' && s[1] <= 'z'
	}
	prefixes := map[string]bool{}
	counterpart := false
	for s := range slugs {
		if s == "" {
			continue
		}
		first := strings.SplitN(s, "/", 2)[0]
		if !isLocalePrefix(first) {
			continue
		}
		prefixes[first] = true
		if rest := strings.TrimPrefix(strings.TrimPrefix(s, first), "/"); rest != "" && slugs[rest] {
			counterpart = true
		}
	}
	return len(prefixes) >= 2 || counterpart
}

// mailtoCovers reports whether an email address appears inside a
// <a href="mailto:..."> on the page. Used to allow emails the page deliberately
// surfaces as clickable links.
func mailtoCovers(doc *html.Node, email string) bool {
	for _, a := range elementsByTag(doc, "a") {
		href := strings.ToLower(attr(a, "href"))
		if strings.HasPrefix(href, "mailto:") && strings.Contains(href, strings.ToLower(email)) {
			return true
		}
	}
	return false
}

func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// checkFAQVisibility ensures any page emitting FAQPage JSON-LD also renders
// the questions visibly in the DOM. Mirrors site-inspector's
// checkFaqSchemaVisibility rule: schema without matching visible Q&A is
// treated as an SEO trap because Google rejects mismatches.
func checkFAQVisibility(site *SiteContext) CheckResult {
	for _, p := range site.Pages {
		var questions []string
		walkJSONLD(p.Doc, func(item map[string]any) {
			if !typeMatches(item["@type"], "FAQPage") {
				return
			}
			list, ok := item["mainEntity"].([]any)
			if !ok {
				return
			}
			for _, e := range list {
				m, ok := e.(map[string]any)
				if !ok {
					continue
				}
				if name, ok := m["name"].(string); ok && strings.TrimSpace(name) != "" {
					questions = append(questions, name)
				}
			}
		})
		if len(questions) == 0 {
			continue
		}
		body := firstElementByTag(p.Doc, "body")
		bodyText := strings.ToLower(textContent(body))
		for _, q := range questions {
			needle := strings.ToLower(strings.TrimSpace(q))
			if needle == "" {
				continue
			}
			if !strings.Contains(bodyText, needle) {
				r := Fail("FAQ Schema Has Visible Content", "social", 2, SeverityWarning,
					fmt.Sprintf("FAQPage schema on %s has question %q with no matching visible text", p.Slug, q),
					"Render every FAQ question in the visible DOM, or remove the FAQPage schema. Google rejects mismatches.")
				r.Page = p.Slug
				return r
			}
		}
	}
	return Pass("FAQ Schema Has Visible Content", "social", 2, "FAQ schemas align with visible content (or no FAQPage schema present)")
}

// perPageCheck runs predicate on every page, fails on first page that fails.
// Detail reports which page failed and why; recommendation is shared.
func perPageCheck(name, section string, weight int, site *SiteContext,
	predicate func(PageContext) (bool, string), recommendation string) CheckResult {
	for _, p := range site.Pages {
		ok, detail := predicate(p)
		if !ok {
			r := Fail(name, section, weight, SeverityError, detail, recommendation)
			r.Page = p.Slug
			return r
		}
	}
	if len(site.Pages) == 0 {
		return Info(name, section, "no pages to evaluate")
	}
	return Pass(name, section, weight, fmt.Sprintf("All %d pages pass", len(site.Pages)))
}

// titleOf reads the <title> text content.
func titleOf(doc *html.Node) string {
	t := firstElementByTag(doc, "title")
	if t == nil {
		return ""
	}
	return textContent(t)
}

// validateHeadingOrder checks that h1-h6 levels never skip downward
// (e.g. h1 -> h3 without an h2 between). Returns (ok, errorMessage).
func validateHeadingOrder(doc *html.Node) (bool, string) {
	var levels []int
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n == nil {
			return
		}
		if n.Type == html.ElementNode && len(n.Data) == 2 && n.Data[0] == 'h' &&
			n.Data[1] >= '1' && n.Data[1] <= '6' {
			levels = append(levels, int(n.Data[1]-'0'))
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	for i := 1; i < len(levels); i++ {
		if levels[i] > levels[i-1]+1 {
			return false, fmt.Sprintf("h%d follows h%d (skipped a level)", levels[i], levels[i-1])
		}
	}
	return true, ""
}

// checkRobotsTxt validates robots.txt presence + minimal content.
func checkRobotsTxt(site *SiteContext) CheckResult {
	if site.RobotsTxt == "" {
		return Fail("robots.txt", "files", 2, SeverityError,
			"no robots.txt in dist/", "Atomicsite generates this -- check build pipeline.")
	}
	if !strings.Contains(strings.ToLower(site.RobotsTxt), "user-agent") {
		return Fail("robots.txt", "files", 2, SeverityWarning,
			"robots.txt has no User-agent directive", "Ensure at least one User-agent rule.")
	}
	return Pass("robots.txt", "files", 2, "robots.txt present and valid")
}

// checkSitemap validates sitemap.xml or sitemap-index.xml.
func checkSitemap(site *SiteContext) CheckResult {
	if site.SitemapXML == "" {
		return Fail("XML Sitemap", "files", 2, SeverityWarning,
			"no sitemap-index.xml or sitemap.xml found", "Astro's @astrojs/sitemap should emit one. Verify build succeeded.")
	}
	return Pass("XML Sitemap", "files", 2, "Sitemap present")
}

// checkLLMsTxt checks for AI-search-ready llms.txt.
func checkLLMsTxt(site *SiteContext) CheckResult {
	if site.LLMsTxt == "" {
		return Fail("llms.txt", "files", 1, SeverityInfo,
			"no /llms.txt", "Atomicsite generates this from site profile + pages.")
	}
	return Pass("llms.txt", "files", 1, "llms.txt present (AI search readiness)")
}

// --- Custom Collections checks (Sprint 4, 2026-05-04) ---

// isCollectionItemPage is the heuristic: the renderer wraps each
// item in <article class="block--collection-item" data-collection=
// "..." data-item="...">. If the parsed page has any element with
// both data-collection AND data-item attributes, treat it as a
// Collection item detail page.
func isCollectionItemPage(p PageContext) bool {
	if p.Doc == nil {
		return false
	}
	var found bool
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if found || n == nil {
			return
		}
		if n.Type == html.ElementNode {
			hasCollection := false
			hasItem := false
			for _, a := range n.Attr {
				if a.Key == "data-collection" && a.Val != "" {
					hasCollection = true
				}
				if a.Key == "data-item" && a.Val != "" {
					hasItem = true
				}
			}
			if hasCollection && hasItem {
				found = true
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(p.Doc)
	return found
}

// isCollectionIndexPage detects the collection index page. The
// renderer wraps the list in <section data-collection="..."> WITHOUT
// a data-item, so the index has data-collection elsewhere on the
// page but never data-item.
func isCollectionIndexPage(p PageContext) bool {
	if p.Doc == nil {
		return false
	}
	hasCollection := false
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n == nil {
			return
		}
		if n.Type == html.ElementNode {
			for _, a := range n.Attr {
				if a.Key == "data-collection" && a.Val != "" {
					hasCollection = true
					break
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(p.Doc)
	return hasCollection && !isCollectionItemPage(p)
}

// hasCollectionItemPages reports whether the site renders any
// Collection item pages at all. The three collection checks emit Info
// (not a weighted Pass) when there is nothing to check: a weighted
// N/A-Pass handed every collection-less site 5 free points and diluted
// the categories that were actually measured.
func hasCollectionItemPages(site *SiteContext) bool {
	for _, p := range site.Pages {
		if isCollectionItemPage(p) {
			return true
		}
	}
	return false
}

// CheckCollectionJSONLDPresent: every Collection-item page must
// embed a <script type="application/ld+json"> with a non-empty @type.
func CheckCollectionJSONLDPresent(site *SiteContext) CheckResult {
	if !hasCollectionItemPages(site) {
		return Info("Collection JSON-LD present", "collections", "No Collection item pages on this site; nothing to check.")
	}
	for _, p := range site.Pages {
		if !isCollectionItemPage(p) {
			continue
		}
		var any404 bool
		var hasType bool
		walkJSONLD(p.Doc, func(node map[string]any) {
			any404 = true
			if t, _ := node["@type"].(string); t != "" {
				hasType = true
			}
		})
		if !any404 {
			return Fail("Collection JSON-LD present", "collections", 2, SeverityWarning,
				fmt.Sprintf("page %s renders a Collection item but has no <script type=\"application/ld+json\">", p.Slug),
				"Set settings.schema_org_type on the Collection (Article, Product, Person, Event, etc.) so the renderer emits structured data automatically.")
		}
		if !hasType {
			return Fail("Collection JSON-LD present", "collections", 2, SeverityWarning,
				fmt.Sprintf("page %s emits JSON-LD but no @type", p.Slug),
				"Pick a schema.org type that fits the Collection (Article for posts, Product for ecommerce, Person for team profiles).")
		}
	}
	return Pass("Collection JSON-LD present", "collections", 2, "Every Collection item page carries structured data.")
}

// CheckCollectionHreflangAlternates: an item page with locale
// variants must emit <link rel="alternate" hreflang="..."> for each.
// Heuristic: if any pages share the same path stub but differ by
// locale prefix, every such page should advertise the others.
func CheckCollectionHreflangAlternates(site *SiteContext) CheckResult {
	if !hasCollectionItemPages(site) {
		return Info("Collection hreflang alternates", "collections", "No Collection item pages on this site; nothing to check.")
	}
	// Group item pages by their item slug component (last path segment).
	// If the same slug exists at multiple locale prefixes, all pages
	// with that slug must advertise hreflang.
	bySlug := map[string]int{}
	for _, p := range site.Pages {
		if !isCollectionItemPage(p) {
			continue
		}
		bySlug[lastPathSegment(p.Slug)]++
	}
	for _, p := range site.Pages {
		if !isCollectionItemPage(p) {
			continue
		}
		if bySlug[lastPathSegment(p.Slug)] < 2 {
			continue // single-locale, no hreflang needed
		}
		alts := elementsByTag(p.Doc, "link")
		var ok bool
		for _, l := range alts {
			if attr(l, "rel") == "alternate" && attr(l, "hreflang") != "" {
				ok = true
				break
			}
		}
		if !ok {
			return Fail("Collection hreflang alternates", "collections", 2, SeverityWarning,
				fmt.Sprintf("page %s shares its slug across locales but emits no hreflang", p.Slug),
				"Set seo.hreflang_strategy=path and ensure each item has a locale variant with the same slug.")
		}
	}
	return Pass("Collection hreflang alternates", "collections", 2, "Multi-locale Collection items advertise their alternates.")
}

// CheckCollectionInternalLinks: every Collection index page should
// link to at least one item; every item page should link back to the
// index.
func CheckCollectionInternalLinks(site *SiteContext) CheckResult {
	hasIndex := false
	for _, p := range site.Pages {
		if !isCollectionIndexPage(p) {
			continue
		}
		hasIndex = true
		links := elementsByTag(p.Doc, "a")
		if len(links) == 0 {
			return Fail("Collection internal links", "collections", 1, SeverityInfo,
				fmt.Sprintf("Collection index %s has no item links", p.Slug),
				"Index pages should link to every published item. Re-run the build after publishing items.")
		}
	}
	if !hasIndex {
		return Info("Collection internal links", "collections", "No Collection index pages on this site; nothing to check.")
	}
	return Pass("Collection internal links", "collections", 1, "Collection index pages link to items.")
}

func lastPathSegment(slug string) string {
	slug = strings.TrimRight(slug, "/")
	if i := strings.LastIndex(slug, "/"); i >= 0 {
		return slug[i+1:]
	}
	return slug
}
