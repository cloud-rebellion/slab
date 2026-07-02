package eval

import (
	"fmt"
	"strings"

	"github.com/bright-interaction/slab/internal/agent"
)

// perfBudget holds the fidelity-dependent performance thresholds. The
// balanced values are the historical literals, so existing sites grade
// byte-identically; showcase trades page weight for design ambition
// (the operator opted in via design.fidelity) while every check stays
// visible and scored. Performance fidelity keeps the balanced budgets:
// they already permit perfect scores, the discipline is authorial.
type perfBudget struct {
	pageHTMLBytes  int64   // per-page HTML size cap
	inlineCSSBytes int     // total <style> content cap per page
	totalHTMLBytes int64   // site-wide dist HTML cap
	lazyRatioNum   int     // required lazy-loaded fraction (num/den) of non-first images
	lazyRatioDen   int
}

func perfBudgetFor(f agent.DesignFidelity) perfBudget {
	if f == agent.FidelityShowcase {
		return perfBudget{
			pageHTMLBytes:  400 * 1024,
			inlineCSSBytes: 150 * 1024,
			totalHTMLBytes: 8 * 1024 * 1024,
			lazyRatioNum:   3, lazyRatioDen: 5,
		}
	}
	return perfBudget{
		pageHTMLBytes:  200 * 1024,
		inlineCSSBytes: 50 * 1024,
		totalHTMLBytes: 5 * 1024 * 1024,
		lazyRatioNum:   4, lazyRatioDen: 5,
	}
}

// budgetLabel annotates a check detail with the active budget so the
// stored row states which threshold applied.
func (b perfBudget) label(f agent.DesignFidelity) string {
	if f == "" {
		f = agent.FidelityBalanced
	}
	return string(f)
}

// RunPerformanceChecks does file-size + static-HTML based perf checks.
// Skips runtime metrics (LCP, INP, TTFB) which need a real browser.
// Budgets follow site.Fidelity (see perfBudgetFor).
func RunPerformanceChecks(site *SiteContext) []CheckResult {
	var checks []CheckResult
	budget := perfBudgetFor(site.Fidelity)

	// 1. Page weight (per-page HTML size — small approximation since we don't bundle assets)
	checks = append(checks, perPageCheck(fmt.Sprintf("HTML Size < %dKB", budget.pageHTMLBytes/1024), "weight", 2, site, func(p PageContext) (bool, string) {
		if p.FileSize > budget.pageHTMLBytes {
			return false, fmt.Sprintf("%d bytes (budget %dKB, %s fidelity)", p.FileSize, budget.pageHTMLBytes/1024, budget.label(site.Fidelity))
		}
		return true, ""
	}, "Reduce inline CSS/HTML; move large content to separate files."))

	// 2. Render-blocking scripts (synchronous <script> in <head>)
	checks = append(checks, perPageCheck("No Render-Blocking Scripts", "scripts", 2, site, func(p PageContext) (bool, string) {
		head := firstElementByTag(p.Doc, "head")
		if head == nil {
			return true, ""
		}
		blocking := 0
		for _, s := range elementsByTag(head, "script") {
			if attr(s, "src") == "" {
				continue // inline scripts don't block in the same way
			}
			if attr(s, "async") == "" && attr(s, "defer") == "" && attr(s, "type") != "module" {
				blocking++
			}
		}
		if blocking >= 3 {
			return false, fmt.Sprintf("%d render-blocking scripts in <head>", blocking)
		}
		return true, ""
	}, "Add async or defer to <script> tags in <head>."))

	// 3. Modern image formats (WebP/AVIF presence in markup)
	checks = append(checks, perPageCheck("Modern Image Formats", "images", 2, site, func(p PageContext) (bool, string) {
		hasModern := false
		hasAnyImg := false
		for _, img := range elementsByTag(p.Doc, "img") {
			hasAnyImg = true
			src := strings.ToLower(attr(img, "src"))
			if strings.HasSuffix(src, ".webp") || strings.HasSuffix(src, ".avif") ||
				strings.HasSuffix(src, ".svg") {
				hasModern = true
			}
		}
		for _, src := range elementsByTag(p.Doc, "source") {
			t := strings.ToLower(attr(src, "type"))
			if strings.Contains(t, "webp") || strings.Contains(t, "avif") {
				hasModern = true
			}
		}
		if !hasAnyImg || hasModern {
			return true, ""
		}
		return false, "no WebP/AVIF/SVG images detected"
	}, "Use Atomicsite's media pipeline for automatic WebP variants."))

	// 4. Lazy-loaded images (recommend loading=lazy for below-fold)
	checks = append(checks, perPageCheck("Lazy-Loaded Images", "images", 2, site, func(p PageContext) (bool, string) {
		imgs := elementsByTag(p.Doc, "img")
		if len(imgs) <= 1 {
			return true, "" // single or no images, no benefit from lazy loading
		}
		lazyCount := 0
		for _, img := range imgs {
			if strings.EqualFold(attr(img, "loading"), "lazy") {
				lazyCount++
			}
		}
		// Expect at least the budgeted fraction of non-first images to
		// be lazy (showcase relaxes for art-directed eager galleries).
		expected := len(imgs) - 1
		if lazyCount < expected*budget.lazyRatioNum/budget.lazyRatioDen {
			return false, fmt.Sprintf("%d/%d images use loading=lazy (%s fidelity budget)", lazyCount, len(imgs), budget.label(site.Fidelity))
		}
		return true, ""
	}, "Add loading=\"lazy\" to all below-the-fold images."))

	// 5. Images have explicit dimensions (prevents CLS)
	checks = append(checks, perPageCheck("Images Have Dimensions", "images", 1, site, func(p PageContext) (bool, string) {
		for _, img := range elementsByTag(p.Doc, "img") {
			src := strings.ToLower(attr(img, "src"))
			if strings.HasSuffix(src, ".svg") {
				continue
			}
			if !hasAttr(img, "width") || !hasAttr(img, "height") {
				return false, "image missing width or height attribute"
			}
		}
		return true, ""
	}, "Add width= and height= to all <img> tags to prevent layout shift."))

	// 6. Inline CSS size (warn if huge)
	checks = append(checks, perPageCheck("Inline CSS Size", "css", 1, site, func(p PageContext) (bool, string) {
		total := 0
		for _, s := range elementsByTag(p.Doc, "style") {
			total += len(textContent(s))
		}
		if total > budget.inlineCSSBytes {
			return false, fmt.Sprintf("%d bytes inline CSS (budget %dKB, %s fidelity)", total, budget.inlineCSSBytes/1024, budget.label(site.Fidelity))
		}
		return true, ""
	}, "Move oversized CSS into external files for caching."))

	// 7. Resource hints
	checks = append(checks, perPageCheck("Resource Hints", "hints", 1, site, func(p PageContext) (bool, string) {
		head := firstElementByTag(p.Doc, "head")
		if head == nil {
			return false, "no <head>"
		}
		for _, l := range elementsByTag(head, "link") {
			rel := strings.ToLower(attr(l, "rel"))
			if strings.Contains(rel, "preconnect") || strings.Contains(rel, "dns-prefetch") ||
				strings.Contains(rel, "preload") {
				return true, ""
			}
		}
		return false, "no preconnect/dns-prefetch/preload"
	}, "Add <link rel=preconnect> for critical third-party origins."))

	// 8. Total dist/ size sanity check
	totalSize := int64(0)
	for _, p := range site.Pages {
		totalSize += p.FileSize
	}
	if totalSize > 0 {
		if totalSize < budget.totalHTMLBytes {
			checks = append(checks, Pass("Total HTML Size", "weight", 1,
				fmt.Sprintf("%.1f KB across %d pages (%s fidelity budget)", float64(totalSize)/1024, len(site.Pages), budget.label(site.Fidelity))))
		} else {
			checks = append(checks, Fail("Total HTML Size", "weight", 1, SeverityWarning,
				fmt.Sprintf("%.1f MB across %d pages (budget %dMB, %s fidelity)", float64(totalSize)/1024/1024, len(site.Pages), budget.totalHTMLBytes/1024/1024, budget.label(site.Fidelity)),
				"Consider splitting large pages or moving content to dynamic loading."))
		}
	}

	return checks
}
