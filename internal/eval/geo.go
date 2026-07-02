package eval

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/bright-interaction/slab/internal/agent"
)

// RunGEOChecks evaluates AI-search / Generative Engine Optimization signals.
// These overlap with classic SEO but score AI-citability surface specifically.
// They are returned as part of the seo category so site-inspector and atomicsite
// stay aligned.
func RunGEOChecks(site *SiteContext) []CheckResult {
	var checks []CheckResult

	checks = append(checks, perPageCheck("Robots max-snippet directive", "geo", 2, site, func(p PageContext) (bool, string) {
		robots := strings.ToLower(findMetaContent(p.Doc, "robots"))
		if strings.Contains(robots, "noindex") {
			return true, "" // skip noindex pages
		}
		if !strings.Contains(robots, "max-snippet:-1") {
			return false, "missing max-snippet:-1 directive"
		}
		return true, ""
	}, "Add max-snippet:-1 to <meta name=robots> so AI Overviews and SERPs can surface full snippets."))

	checks = append(checks, perPageCheck("Robots max-image-preview directive", "geo", 1, site, func(p PageContext) (bool, string) {
		robots := strings.ToLower(findMetaContent(p.Doc, "robots"))
		if strings.Contains(robots, "noindex") {
			return true, ""
		}
		if !strings.Contains(robots, "max-image-preview:large") {
			return false, "missing max-image-preview:large directive"
		}
		return true, ""
	}, "Add max-image-preview:large to <meta name=robots> for richer image surfacing."))

	checks = append(checks, perPageCheck("BreadcrumbList Schema", "geo", 2, site, func(p PageContext) (bool, string) {
		// Home page is exempt, plus per-locale roots like /sv or /en. Single
		// path-segment routes act as the "homepage" of their locale and
		// shouldn't carry a breadcrumb (Site Inspector applies the same rule
		// via `pathname === '/'`; atomicsite extends it to depth-1 locale roots).
		if isLocaleRoot(p.Slug) {
			return true, ""
		}
		count := breadcrumbItemCount(p.Doc)
		if count >= 2 {
			return true, ""
		}
		if count == 1 {
			return false, "BreadcrumbList present but only 1 itemListElement"
		}
		return false, "no BreadcrumbList JSON-LD"
	}, "Emit a BreadcrumbList schema with itemListElement so AI search can render breadcrumb context."))

	checks = append(checks, checkOrganizationSchemaComplete(site))

	checks = append(checks, perPageCheck("Content Freshness Signal", "geo", 2, site, func(p PageContext) (bool, string) {
		if !isArticlePage(p.Doc) {
			return true, "" // article-only check
		}
		fromSchema := articleDateModifiedFromSchema(p.Doc)
		fromMeta := findMetaContent(p.Doc, "article:modified_time")
		value := fromSchema
		if value == "" {
			value = fromMeta
		}
		if value == "" {
			return false, "no dateModified in JSON-LD or article:modified_time meta"
		}
		t, err := parseFreshness(value)
		if err != nil {
			return false, fmt.Sprintf("unparseable freshness value %q", value)
		}
		ageMonths := time.Since(t).Hours() / 24 / 30.44
		if ageMonths > 18 {
			return false, fmt.Sprintf("last modified %s (%.0f months ago, older than 18 months)", t.Format("2006-01-02"), ageMonths)
		}
		return true, ""
	}, "Add dateModified to Article schema or article:modified_time meta so AI search can reason about freshness."))

	checks = append(checks, checkNoindexNotInSitemap(site))

	// Structure-taste check: advisory in showcase fidelity (free-form
	// showcase layouts legitimately skip the list/table pattern).
	if site.Fidelity == agent.FidelityShowcase {
		checks = append(checks, Info("AI-Friendly Formatting", "geo",
			"Advisory in showcase fidelity: lists/tables/dense H2s still help LLM passage extraction; unscored here."))
	} else {
		checks = append(checks, perPageCheck("AI-Friendly Formatting", "geo", 1, site, func(p PageContext) (bool, string) {
			if hasAIFormatSignal(p.Doc) {
				return true, ""
			}
			return false, "no list (≥3 items), table with <th>, definition list, or sufficient h2 density"
		}, "Use lists, tables with <th>, or denser H2 sections so LLMs can extract passages cleanly."))
	}

	return checks
}

// isLocaleRoot reports whether a slug is the homepage or a depth-1 locale
// root (e.g. "/", "/sv", "/sv/", "/en"). Site Inspector exempts the
// homepage from BreadcrumbList; atomicsite extends to locale roots.
func isLocaleRoot(slug string) bool {
	s := strings.Trim(slug, "/")
	if s == "" {
		return true
	}
	return !strings.Contains(s, "/")
}

// breadcrumbItemCount returns the maximum itemListElement length found across
// any BreadcrumbList JSON-LD block on the page.
func breadcrumbItemCount(doc *html.Node) int {
	max := 0
	walkJSONLD(doc, func(item map[string]any) {
		if typeMatches(item["@type"], "BreadcrumbList") {
			if list, ok := item["itemListElement"].([]any); ok {
				if len(list) > max {
					max = len(list)
				}
			}
		}
	})
	return max
}

// articleDateModifiedFromSchema returns dateModified from the first
// Article/BlogPosting/NewsArticle JSON-LD block, or "".
func articleDateModifiedFromSchema(doc *html.Node) string {
	var found string
	walkJSONLD(doc, func(item map[string]any) {
		if found != "" {
			return
		}
		if typeMatchesAny(item["@type"], "Article", "BlogPosting", "NewsArticle", "TechArticle", "ScholarlyArticle") {
			if v, ok := item["dateModified"].(string); ok {
				found = v
			}
		}
	})
	return found
}

// isArticlePage returns true if any JSON-LD on the page declares an Article subtype.
func isArticlePage(doc *html.Node) bool {
	hit := false
	walkJSONLD(doc, func(item map[string]any) {
		if typeMatchesAny(item["@type"], "Article", "BlogPosting", "NewsArticle", "TechArticle", "ScholarlyArticle") {
			hit = true
		}
	})
	return hit
}

// hasAIFormatSignal returns true if at least one AI-friendly formatting marker
// is present: list with >=3 items, table with <th>, definition list, or
// >=1 h2 per 300 words.
func hasAIFormatSignal(doc *html.Node) bool {
	for _, list := range append(elementsByTag(doc, "ul"), elementsByTag(doc, "ol")...) {
		liCount := 0
		for c := list.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && c.Data == "li" {
				liCount++
			}
		}
		if liCount >= 3 {
			return true
		}
	}
	for _, t := range elementsByTag(doc, "table") {
		if firstElementByTag(t, "th") != nil {
			return true
		}
	}
	if len(elementsByTag(doc, "dl")) > 0 {
		return true
	}
	body := firstElementByTag(doc, "body")
	if body == nil {
		return false
	}
	wordCount := len(strings.Fields(textContent(body)))
	h2Count := len(elementsByTag(doc, "h2"))
	if wordCount > 0 && float64(h2Count)/(float64(wordCount)/300.0) >= 1.0 {
		return true
	}
	return false
}

// checkOrganizationSchemaComplete is a site-wide check: at least one page must
// emit a complete Organization schema. Layout typically embeds it on every
// page, so we sample any.
func checkOrganizationSchemaComplete(site *SiteContext) CheckResult {
	for _, p := range site.Pages {
		ok, partial, missing := organizationSchemaStatus(p.Doc)
		if ok {
			return Pass("Organization Schema Completeness", "geo", 2,
				"Organization schema has name, url, logo, sameAs, and founder/address/contactPoint")
		}
		if partial {
			r := Fail("Organization Schema Completeness", "geo", 2, SeverityWarning,
				"Has name, url, logo, sameAs but missing founder/address/contactPoint",
				"Add founder, address, or contactPoint to Organization schema for stronger entity signal.")
			// PARTIAL: half credit
			r.Weight = 1
			t := true
			r.Passed = &t
			return r
		}
		if len(missing) > 0 {
			// Continue to scan further pages; first page might lack schema if
			// layout was customized. Track best result so far.
			_ = missing
		}
	}
	return Fail("Organization Schema Completeness", "geo", 2, SeverityError,
		"No complete Organization (or LocalBusiness) JSON-LD found",
		"Emit Organization schema with name, url, logo, sameAs(>=1), and address/founder/contactPoint to anchor your entity in AI knowledge graphs.")
}

// organizationSchemaStatus returns (fullyComplete, partiallyComplete, missingFields).
func organizationSchemaStatus(doc *html.Node) (full bool, partial bool, missing []string) {
	orgTypes := []string{"Organization", "Corporation", "LocalBusiness", "OnlineBusiness", "NGO", "EducationalOrganization", "GovernmentOrganization"}
	walkJSONLD(doc, func(item map[string]any) {
		if !typeMatchesAny(item["@type"], orgTypes...) {
			return
		}
		// Best result wins (first complete schema short-circuits).
		if full {
			return
		}
		hasName := item["name"] != nil
		hasURL := item["url"] != nil
		hasLogo := item["logo"] != nil
		sameAsCount := 0
		switch v := item["sameAs"].(type) {
		case []any:
			sameAsCount = len(v)
		case string:
			if v != "" {
				sameAsCount = 1
			}
		}
		hasContact := item["founder"] != nil || item["address"] != nil || item["contactPoint"] != nil
		if hasName && hasURL && hasLogo && sameAsCount >= 1 && hasContact {
			full = true
			return
		}
		if hasName && hasURL && hasLogo && sameAsCount >= 1 {
			partial = true
		}
		miss := []string{}
		if !hasName {
			miss = append(miss, "name")
		}
		if !hasURL {
			miss = append(miss, "url")
		}
		if !hasLogo {
			miss = append(miss, "logo")
		}
		if sameAsCount < 1 {
			miss = append(miss, "sameAs")
		}
		if !hasContact {
			miss = append(miss, "founder/address/contactPoint")
		}
		missing = miss
	})
	return
}

// checkNoindexNotInSitemap is a site-wide check: any page with a noindex meta
// must not appear in sitemap.xml. Crawl-budget hygiene for AI/search engines.
func checkNoindexNotInSitemap(site *SiteContext) CheckResult {
	if site.SitemapXML == "" {
		return Info("Noindex Pages Excluded From Sitemap", "geo", "no sitemap to cross-reference")
	}
	var leaks []string
	for _, p := range site.Pages {
		robots := strings.ToLower(findMetaContent(p.Doc, "robots"))
		if !strings.Contains(robots, "noindex") {
			continue
		}
		// Look for the page's slug in sitemap (tolerant: exact slug or trailing slash variant)
		needles := []string{p.Slug, p.Slug + "/"}
		for _, n := range needles {
			if n == "/" {
				continue
			}
			if strings.Contains(site.SitemapXML, n+"<") || strings.Contains(site.SitemapXML, n+`"`) {
				leaks = append(leaks, p.Slug)
				break
			}
		}
	}
	if len(leaks) == 0 {
		return Pass("Noindex Pages Excluded From Sitemap", "geo", 3, "no noindex pages found in sitemap")
	}
	if len(leaks) <= 2 {
		// PARTIAL
		r := Fail("Noindex Pages Excluded From Sitemap", "geo", 3, SeverityWarning,
			fmt.Sprintf("%d noindex page(s) appear in sitemap: %s", len(leaks), strings.Join(leaks, ", ")),
			"Filter noindex routes out of the sitemap; they waste crawl budget and confuse AI engines.")
		r.Weight = 1
		t := true
		r.Passed = &t
		return r
	}
	return Fail("Noindex Pages Excluded From Sitemap", "geo", 3, SeverityError,
		fmt.Sprintf("%d noindex page(s) appear in sitemap: %s", len(leaks), strings.Join(leaks[:3], ", ")+"..."),
		"Filter noindex routes out of the sitemap; they waste crawl budget and confuse AI engines.")
}

// walkJSONLD finds every <script type="application/ld+json"> block on the page,
// parses it, and calls fn for every item, including items inside an @graph
// array or top-level array.
func walkJSONLD(doc *html.Node, fn func(map[string]any)) {
	for _, s := range elementsByTag(doc, "script") {
		if strings.ToLower(attr(s, "type")) != "application/ld+json" {
			continue
		}
		raw := strings.TrimSpace(textContent(s))
		if raw == "" {
			continue
		}
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			continue
		}
		visitJSONLD(v, fn)
	}
}

func visitJSONLD(v any, fn func(map[string]any)) {
	switch x := v.(type) {
	case map[string]any:
		fn(x)
		if g, ok := x["@graph"].([]any); ok {
			for _, item := range g {
				visitJSONLD(item, fn)
			}
		}
	case []any:
		for _, item := range x {
			visitJSONLD(item, fn)
		}
	}
}

// typeMatches returns true if v matches the wanted type (exact, case-sensitive).
// Schema.org @type may be a string or a []string.
func typeMatches(v any, want string) bool {
	switch t := v.(type) {
	case string:
		return t == want
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

func typeMatchesAny(v any, wants ...string) bool {
	for _, w := range wants {
		if typeMatches(v, w) {
			return true
		}
	}
	return false
}

// parseFreshness accepts ISO 8601 dates with or without time. Returns the
// parsed time or an error.
func parseFreshness(s string) (time.Time, error) {
	formats := []string{
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02T15:04:05Z",
		"2006-01-02",
		"2006-01-02T15:04:05.000Z",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("no matching format")
}

