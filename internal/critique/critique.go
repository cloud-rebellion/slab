// Package critique audits a built site against the DesignPlaybook
// anti-patterns and content-authenticity rules. The single source of
// truth is agent.DefaultDesignPlaybook(): every check derives its
// banned pattern, recommendation, and section label from the playbook
// so editing the playbook is the only place to add or remove rules.
//
// Mirrors the eval package shape (Pass/Fail/Info, CategoryReport)
// and persists results to the same evaluations table with
// Category="design" so the existing /api/sites/{id}/evaluations and
// /api/agent/evaluation/{buildID} endpoints surface critique findings
// alongside security/SEO without schema changes.
package critique

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/bright-interaction/slab/internal/agent"
	"github.com/bright-interaction/slab/internal/builder"
	"github.com/bright-interaction/slab/internal/eval"
	"github.com/bright-interaction/slab/internal/store"
)

// Timeout caps total critique time per build.
const Timeout = 15 * time.Second

// Run loads the same SiteContext eval uses, runs every design check
// against it, and persists one evaluations row with Category="design".
// Bounded by Timeout. Failures are non-fatal at the caller level.
//
// The rubric follows the site's design.fidelity dial: the playbook is
// resolved per fidelity (same choke point the agent reads), taste
// detectors demote to advisory in showcase, and the graded fidelity is
// stamped on the persisted row so historical grades stay honest.
func Run(ctx context.Context, queries *store.Queries, siteID, buildID, distDir string) (eval.CategoryReport, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	site, err := eval.LoadSiteContext(ctx, queries, siteID, distDir)
	if err != nil {
		return eval.CategoryReport{}, fmt.Errorf("load site context: %w", err)
	}

	fidelity := agent.FidelityForSite(ctx, queries, siteID)
	playbook := agent.DesignPlaybookFor(fidelity)
	checks := RunChecksFor(site, playbook, fidelity)

	// Showcase grants a real motion budget, so it also verifies the
	// a11y contract on the agent's own CSS at the source: css_classes
	// rows carrying animation without any reduced-motion guard fail.
	if fidelity == agent.FidelityShowcase {
		var cssSrc strings.Builder
		if rows, err := queries.ListCSSClassesBySite(ctx, siteID); err == nil {
			for _, r := range rows {
				cssSrc.WriteString(r.Css)
				cssSrc.WriteString("\n")
			}
		}
		checks = append(checks, reducedMotionCoverageChecks(cssSrc.String())...)
	}

	score, max := eval.ComputeScore(checks)
	report := eval.CategoryReport{
		Category: "design",
		Score:    score,
		MaxScore: max,
		Grade:    eval.ScoreToGrade(score, max),
		Checks:   checks,
	}

	checksJSON, _ := json.Marshal(checks)
	if err := queries.CreateEvaluation(ctx, store.CreateEvaluationParams{
		ID:         newID(),
		BuildID:    buildID,
		SiteID:     siteID,
		Category:   "design",
		Score:      int64(score),
		MaxScore:   int64(max),
		Grade:      report.Grade,
		ChecksJson: string(checksJSON),
		Profile:    string(fidelity),
	}); err != nil {
		slog.Error("critique: persist failed", "site_id", siteID, "build_id", buildID, "err", err)
	}

	slog.Info("critique: complete", "site_id", siteID, "build_id", buildID, "grade", report.Grade, "fidelity", string(fidelity), "checks", len(checks))
	return report, nil
}

// RunChecks runs every design check with the balanced rubric. Kept for
// callers (and tests) that predate the fidelity dial.
func RunChecks(site *eval.SiteContext, playbook agent.DesignPlaybookInfo) []eval.CheckResult {
	return RunChecksFor(site, playbook, agent.FidelityBalanced)
}

// RunChecksFor runs every design check against the site context using
// the playbook as the rule source and the fidelity as the rubric
// selector. Pure function, no I/O, easy to unit test.
func RunChecksFor(site *eval.SiteContext, playbook agent.DesignPlaybookInfo, fidelity agent.DesignFidelity) []eval.CheckResult {
	var checks []eval.CheckResult

	// Concatenate all page HTML once for whole-site searches. Per-page
	// scope is preserved for findings that need to attribute the page.
	allHTML := concatHTML(site)
	allText := concatVisibleText(site)
	allCSS := readSiteCSS(site)
	headings := extractHeadings(site)

	// Grading-context stamp: zero-weight, excluded from scoring, but
	// every stored report names the rubric that produced it.
	checks = append(checks, eval.Info("graded_fidelity", "meta",
		fmt.Sprintf("Design checks graded under the %q fidelity rubric (design.fidelity setting).", string(fidelity))))

	checks = append(checks, antiPatternChecks(playbook.AntiPatterns, allHTML, allCSS, allText, headings, fidelity)...)
	checks = append(checks, contentAuthenticityChecks(playbook.ContentAuthenticity, allText, site)...)
	checks = append(checks, motionChecks(playbook.Motion, allCSS, allHTML)...)
	checks = append(checks, iconPolicyChecks(playbook.IconPolicy, allHTML)...)
	checks = append(checks, designTokenCoherenceChecks(allHTML, allCSS, fidelity)...)
	checks = append(checks, motionDensityChecks(allCSS, site, fidelity)...)
	checks = append(checks, metaCompletenessChecks(site)...)
	checks = append(checks, heroQualityChecks(site, fidelity)...)
	checks = append(checks, aboveTheFoldTrustChecks(site, fidelity)...)
	checks = append(checks, auditChecklistInfo(playbook.AuditChecklist)...)

	return checks
}

// contentAuthenticityChecks scans visible text for banned slop terms
// (names, numbers, companies, phrases). Each match becomes one Fail.
func contentAuthenticityChecks(rules agent.ContentRules, text string, site *eval.SiteContext) []eval.CheckResult {
	var out []eval.CheckResult
	textLower := strings.ToLower(text)

	scan := func(category string, terms []string) {
		for _, t := range terms {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			needle := strings.ToLower(t)
			name := fmt.Sprintf("slop:%s:%s", category, truncate(t, 40))
			if strings.Contains(textLower, needle) {
				out = append(out, eval.Fail(
					name, "content_authenticity", 3, eval.SeverityWarning,
					fmt.Sprintf("Banned %s %q appears in visible copy.", category, t),
					"Replace with specific, real, named example. Generic AI-slop terms damage credibility.",
				))
			}
		}
	}

	scan("name", rules.BannedNames)
	scan("number", rules.BannedNumbers)
	scan("company", rules.BannedCompanies)
	scan("phrase", rules.BannedPhrases)

	if len(out) == 0 {
		out = append(out, eval.Pass("slop_terms", "content_authenticity", 5, "no banned slop terms in visible copy"))
	}
	_ = site // reserved for per-page attribution if we expand later
	return out
}

// motionChecks looks for transition: all and similarly-broad CSS rules
// the playbook bans. Only CSS is scanned; atomicsite is static-Astro
// so JS motion libs aren't a concern here.
var transitionAllRE = regexp.MustCompile(`(?i)transition\s*:\s*all\b`)

func motionChecks(_ agent.MotionGuidance, css, _ string) []eval.CheckResult {
	var out []eval.CheckResult
	if transitionAllRE.MatchString(css) {
		out = append(out, eval.Fail(
			"transition_all", "motion", 3, eval.SeverityInfo,
			"`transition: all` matched in built CSS.",
			"Be explicit: list the properties you actually animate (transform, opacity).",
		))
	} else {
		out = append(out, eval.Pass("transition_all", "motion", 3, "no broad `transition: all` rules"))
	}
	return out
}

// iconPolicyChecks emits Info entries when icon imports outside the
// curated set are detected. Hard to be precise from rendered HTML;
// stays informational.
func iconPolicyChecks(_ agent.IconRules, html string) []eval.CheckResult {
	var out []eval.CheckResult
	if strings.Contains(strings.ToLower(html), "font-awesome") {
		out = append(out, eval.Fail(
			"icon_set:font-awesome", "icon_policy", 2, eval.SeverityInfo,
			"Font Awesome reference detected. Playbook prefers Lucide.",
			"Swap to a Lucide icon. See playbook IconPolicy.Available for the curated 52-item set.",
		))
	} else {
		out = append(out, eval.Pass("icon_set", "icon_policy", 2, "no banned icon set references"))
	}
	return out
}

// auditChecklistInfo turns each AuditChecklist item into an Info entry
// so the agent (or human reviewer) sees the full pre-flight checklist
// rendered alongside critique findings.
func auditChecklistInfo(items []agent.AuditItem) []eval.CheckResult {
	out := make([]eval.CheckResult, 0, len(items))
	for _, item := range items {
		out = append(out, eval.Info(
			truncate(item.Check, 80), "audit_checklist", item.Why,
		))
	}
	return out
}

// designTokenCoherenceChecks scans inline/block CSS for hard-coded
// values that drift from the canonical token allowlist exported from
// builder. Custom blocks ship raw HTML/CSS and previously bypassed
// every other check, this catches "border-radius: 7px",
// "box-shadow: 0 4px 8px #000", "font-family: 'Comic Sans'" and
// other one-off drift. Severity stays warning (not error) because
// some drift is intentional in hero custom blocks.
var (
	hexColorRE     = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`)
	borderRadiusRE = regexp.MustCompile(`border-radius\s*:\s*([^;}]+)`)
	boxShadowRE    = regexp.MustCompile(`box-shadow\s*:\s*([^;}]+)`)
	fontFamilyRE   = regexp.MustCompile(`font-family\s*:\s*([^;}]+)`)
)

func designTokenCoherenceChecks(html, css string, fidelity agent.DesignFidelity) []eval.CheckResult {
	combined := html + "\n" + css
	var findings []string

	// Showcase widens the palette budget: bespoke work legitimately
	// carries more distinct hues (gradients, aurora bands, dark vibes).
	hexCap := 12
	if fidelity == agent.FidelityShowcase {
		hexCap = 24
	}

	// 1) Radii that aren't in the canonical squircle set.
	radiiMatches := borderRadiusRE.FindAllStringSubmatch(combined, -1)
	driftRadii := map[string]bool{}
	for _, m := range radiiMatches {
		val := strings.TrimSpace(m[1])
		// Allow var() references and computed values.
		if strings.HasPrefix(val, "var(") || strings.HasPrefix(val, "calc(") || strings.HasPrefix(val, "inherit") || strings.HasPrefix(val, "0") || strings.HasPrefix(val, "0px") {
			continue
		}
		canonical := false
		for _, ok := range builder.CanonicalRadiiRem {
			if strings.Contains(val, ok) {
				canonical = true
				break
			}
		}
		if !canonical && len(driftRadii) < 8 {
			driftRadii[val] = true
		}
	}
	for v := range driftRadii {
		findings = append(findings, fmt.Sprintf("border-radius %q drifts from canonical squircle set (%v)", truncate(v, 40), builder.CanonicalRadiiRem))
	}

	// 2) Box-shadows that aren't tinted. Looking for the color-mix+oklab+var(--color-text) recipe.
	shadowMatches := boxShadowRE.FindAllStringSubmatch(combined, -1)
	shadowDriftSeen := 0
	for _, m := range shadowMatches {
		val := strings.ToLower(strings.TrimSpace(m[1]))
		if strings.HasPrefix(val, "none") || strings.HasPrefix(val, "var(") || strings.HasPrefix(val, "inset 0 1px 0") {
			continue
		}
		tinted := strings.Contains(val, "color-mix") && strings.Contains(val, "oklab")
		hasVarText := strings.Contains(val, "var(--color-text)") || strings.Contains(val, "var(--color-primary)")
		if !tinted && !hasVarText && shadowDriftSeen < 3 {
			findings = append(findings, fmt.Sprintf("box-shadow %q isn't a tinted recipe; renderer uses color-mix(in oklab, var(--color-text) X%%, transparent)", truncate(val, 60)))
			shadowDriftSeen++
		}
	}

	// 3) Font-family declarations that bypass the CSS variable system.
	fontMatches := fontFamilyRE.FindAllStringSubmatch(combined, -1)
	fontDriftSeen := 0
	for _, m := range fontMatches {
		val := strings.ToLower(strings.TrimSpace(m[1]))
		canonical := false
		for _, ok := range builder.CanonicalFontVars {
			if strings.Contains(val, ok) {
				canonical = true
				break
			}
		}
		// System fallback inside the var() default is fine, only flag
		// hard-coded family-name lists that bypass var() entirely.
		if !canonical && !strings.Contains(val, "var(") && fontDriftSeen < 3 {
			findings = append(findings, fmt.Sprintf("font-family %q hard-codes a family list; use var(--font-heading|body|mono) so per-site uploads take effect", truncate(val, 50)))
			fontDriftSeen++
		}
	}

	// 4) Animation timing functions that aren't in the canonical bezier set.
	// Quick scan: if `cubic-bezier(` appears, every occurrence should match
	// one of the approved curves. Linear / ease-in-out are caught by the
	// existing motionChecks; this catches custom-but-wrong cubic-beziers.
	beziers := strings.Count(strings.ToLower(combined), "cubic-bezier(")
	if beziers > 0 {
		approved := 0
		for _, b := range builder.CanonicalBeziers {
			approved += strings.Count(strings.ToLower(combined), strings.ToLower(b))
		}
		if approved < beziers {
			findings = append(findings, fmt.Sprintf("%d cubic-bezier() declarations found, only %d match the playbook's curve set (0.16,1,0.3,1 / 0.32,0.72,0,1)", beziers, approved))
		}
	}

	// 5) Hex colors outside the most-used neutrals + primary. Loose check:
	// many sites legitimately use multiple hex values in branding settings.
	// Just count distinct hexes; > 8 implies a custom block hard-coded a
	// rainbow palette.
	hexes := hexColorRE.FindAllString(combined, -1)
	distinct := map[string]bool{}
	for _, h := range hexes {
		distinct[strings.ToLower(h)] = true
	}
	if len(distinct) > hexCap {
		findings = append(findings, fmt.Sprintf("%d distinct hex colours found in HTML/CSS (budget %d), likely a custom block hard-coded a palette instead of using var(--color-*)", len(distinct), hexCap))
	}

	if len(findings) == 0 {
		return []eval.CheckResult{eval.Pass("design_token_coherence", "design_tokens", 5, "every measured token matches the canonical allowlist")}
	}

	// Showcase: bespoke tokens are the point. Findings surface as
	// advisory Info (visible for the cohesion review, unscored) instead
	// of weighted Fails, leaving the denominator honestly.
	if fidelity == agent.FidelityShowcase {
		var out []eval.CheckResult
		for i, f := range findings {
			if i >= 5 {
				break
			}
			out = append(out, eval.Info(
				fmt.Sprintf("token_drift_%d", i+1), "design_tokens",
				"Advisory in showcase fidelity: "+f+". Bespoke tokens are allowed; verify they cohere with the site's DESIGN.md / chosen vibe.",
			))
		}
		return out
	}

	// One Fail per finding, capped at 5 to keep the report readable.
	var out []eval.CheckResult
	for i, f := range findings {
		if i >= 5 {
			break
		}
		out = append(out, eval.Fail(
			fmt.Sprintf("token_drift_%d", i+1), "design_tokens", 1, eval.SeverityWarning,
			f,
			"Read builder/css.go for the canonical token sets (CanonicalRadiiRem, CanonicalShadowFormulaSubstrings, CanonicalFontVars, CanonicalBeziers). Update the custom block to read the CSS variables instead of hard-coded values.",
		))
	}
	return out
}

// motionDensityChecks counts perpetual animations per viewport against
// the fidelity's motion budget: 1 for performance/balanced (the
// playbook's pick-one-signal rule), 4 for showcase (layered ambient
// motion is sanctioned there; the Inspector warns at budget+1 and
// errors at budget+3).
//
// Two signal sources are combined:
//  1. inline <style> @keyframes used outside :hover/:focus/:active
//     (custom blocks), site-wide as before, and
//  2. rendered ambient-motion markers in the page HTML (hero graphics,
//     marquee, fx utilities). The renderer's keyframe library lives in
//     the external global.css bundle, so DOM presence, not CSS text, is
//     the honest signal for what actually animates on a page. Each
//     marker type counts once per page: five .fx-float cards are one
//     visual signal, not five.
var keyframesNameRE = regexp.MustCompile(`@keyframes\s+([a-zA-Z0-9_-]+)`)

// ambientMotionMarkers maps rendered class markers to the perpetual
// signal they represent. Kept in sync with the builder's emitted CSS
// (hero graphics in css.go, fx utilities in css_fx.go).
var ambientMotionMarkers = []string{
	"hero-graphic--mesh",
	"hero-graphic--pulse",
	"hero-graphic--aurora",
	"block-circuit-canvas",
	"logo-carousel-track",
	"fx-float",
	"fx-shimmer",
	"fx-aurora-bg",
}

func motionBudgetFor(fidelity agent.DesignFidelity) int {
	if fidelity == agent.FidelityShowcase {
		return 4
	}
	return 1
}

func motionDensityChecks(css string, site *eval.SiteContext, fidelity agent.DesignFidelity) []eval.CheckResult {
	matches := keyframesNameRE.FindAllStringSubmatch(css, -1)
	// Count keyframes that are actually used outside of :hover/:focus.
	// Heuristic: for each keyframe name, check if it appears in an
	// `animation:` declaration that doesn't sit inside a hover/focus
	// selector. Quick scan: split on `}` then check each rule block.
	perpetualNames := map[string]bool{}
	rules := strings.Split(css, "}")
	for _, rule := range rules {
		ruleLower := strings.ToLower(rule)
		if !strings.Contains(ruleLower, "animation") {
			continue
		}
		// Skip hover/focus/active states.
		if strings.Contains(ruleLower, ":hover") || strings.Contains(ruleLower, ":focus") || strings.Contains(ruleLower, ":active") {
			continue
		}
		// Skip the prefers-reduced-motion guard.
		if strings.Contains(ruleLower, "prefers-reduced-motion") {
			continue
		}
		for _, m := range matches {
			name := strings.ToLower(m[1])
			if strings.Contains(ruleLower, name) {
				perpetualNames[name] = true
			}
		}
	}

	// Rendered ambient markers: worst page wins ("per viewport").
	inlineCount := len(perpetualNames)
	signals := mapKeys(perpetualNames)
	renderedWorst := 0
	for _, p := range site.Pages {
		pageCount := 0
		var pageSignals []string
		for _, marker := range ambientMotionMarkers {
			if strings.Contains(p.HTML, marker) {
				pageCount++
				pageSignals = append(pageSignals, marker)
			}
		}
		if pageCount > renderedWorst {
			renderedWorst = pageCount
			signals = append(mapKeys(perpetualNames), pageSignals...)
		}
	}

	budget := motionBudgetFor(fidelity)
	count := inlineCount + renderedWorst
	if count <= budget {
		return []eval.CheckResult{eval.Pass("motion_density", "motion", 5,
			fmt.Sprintf("%d perpetual animation signal(s) per viewport (budget %d under %s fidelity)", count, budget, string(fidelity)))}
	}

	severity := eval.SeverityWarning
	deduct := 2
	msg := fmt.Sprintf("%d perpetual animation signals detected: %v. The %s fidelity budget is %d per viewport.", count, signals, string(fidelity), budget)
	if count >= budget+3 {
		severity = eval.SeverityError
		deduct = 5
		msg = fmt.Sprintf("%d perpetual animation signals detected: %v. Stacking this far past the %d-signal budget reads as AI-builder demo, not premium product.", count, signals, budget)
	}
	rec := "Pick ONE perpetual signal (marquee, circuit, pulse, mesh). Convert the rest to load-once entry choreography (transform + opacity, one-shot transition)."
	if fidelity == agent.FidelityShowcase {
		rec = "Layering is allowed in showcase, soup is not: keep at most 4 ambient signals that belong to one visual system, convert the rest to scroll-driven or load-once choreography (.fx-reveal, .fx-stagger)."
	}
	return []eval.CheckResult{eval.Fail("motion_density", "motion", deduct, severity, msg, rec)}
}

// reducedMotionCoverageChecks (showcase-only) verifies the a11y side of
// the motion budget at the source: css_classes rows that declare
// @keyframes or animation properties must ship a
// prefers-reduced-motion guard somewhere in the site's class set. The
// renderer gates its own fx layer; this covers agent-authored CSS.
func reducedMotionCoverageChecks(cssClassesSrc string) []eval.CheckResult {
	low := strings.ToLower(cssClassesSrc)
	hasMotion := strings.Contains(low, "@keyframes") || strings.Contains(low, "animation:") || strings.Contains(low, "animation-name")
	if !hasMotion {
		return []eval.CheckResult{eval.Pass("reduced_motion_coverage", "motion", 3, "no agent-authored css_classes animations to gate")}
	}
	if strings.Contains(low, "prefers-reduced-motion") {
		return []eval.CheckResult{eval.Pass("reduced_motion_coverage", "motion", 3, "agent-authored css_classes animations ship a prefers-reduced-motion guard")}
	}
	return []eval.CheckResult{eval.Fail(
		"reduced_motion_coverage", "motion", 3, eval.SeverityError,
		"css_classes declare animations but no prefers-reduced-motion guard exists in the site's class set.",
		"Add a guard: @media (prefers-reduced-motion: reduce) { .your-animated-class { animation: none; transition: none; } }. The showcase motion budget is conditional on this a11y invariant.",
	)}
}

// metaCompletenessChecks runs Lovable-style per-page meta sanity on
// every page. Title length 30-60 chars, description 120-160 chars and
// not generic, canonical absolute, OG image present. Closes the gap
// where Lovable users had to manually fix every page's meta after
// generation.
var (
	titleRE       = regexp.MustCompile(`(?i)<title>([^<]*)</title>`)
	metaDescRE    = regexp.MustCompile(`(?i)<meta[^>]+name=["']description["'][^>]+content=["']([^"']*)["']`)
	ogImageRE     = regexp.MustCompile(`(?i)<meta[^>]+property=["']og:image["'][^>]+content=["']([^"']*)["']`)
	canonicalRE   = regexp.MustCompile(`(?i)<link[^>]+rel=["']canonical["'][^>]+href=["']([^"']*)["']`)
)

func metaCompletenessChecks(site *eval.SiteContext) []eval.CheckResult {
	var findings []string
	for _, p := range site.Pages {
		t := strings.TrimSpace(firstMatch(titleRE, p.HTML))
		if t == "" {
			findings = append(findings, fmt.Sprintf("%s: missing <title>", p.Slug))
		} else if l := len(t); l < 30 || l > 65 {
			findings = append(findings, fmt.Sprintf("%s: <title> is %d chars (target 30-60)", p.Slug, l))
		}

		d := strings.TrimSpace(firstMatch(metaDescRE, p.HTML))
		if d == "" {
			findings = append(findings, fmt.Sprintf("%s: missing meta description", p.Slug))
		} else if l := len(d); l < 100 || l > 175 {
			findings = append(findings, fmt.Sprintf("%s: meta description is %d chars (target 120-160)", p.Slug, l))
		}

		og := firstMatch(ogImageRE, p.HTML)
		if og == "" {
			findings = append(findings, fmt.Sprintf("%s: missing og:image", p.Slug))
		}

		canon := firstMatch(canonicalRE, p.HTML)
		if canon == "" {
			findings = append(findings, fmt.Sprintf("%s: missing rel=canonical", p.Slug))
		} else if !strings.HasPrefix(canon, "http") {
			findings = append(findings, fmt.Sprintf("%s: canonical %q isn't absolute", p.Slug, truncate(canon, 60)))
		}
	}

	if len(findings) == 0 {
		return []eval.CheckResult{eval.Pass("meta_completeness", "meta_audit", 5, fmt.Sprintf("title + description + og:image + canonical present on all %d pages", len(site.Pages)))}
	}

	// One Fail per page-level finding, capped at 6 to keep the report tight.
	var out []eval.CheckResult
	for i, f := range findings {
		if i >= 6 {
			break
		}
		out = append(out, eval.Fail(
			fmt.Sprintf("meta_gap_%d", i+1), "meta_audit", 2, eval.SeverityWarning,
			f,
			"Set page.meta_title (30-60 chars), page.meta_description (120-160 chars with a number or proof), seo.og_default_image_id, and seo.canonical_base via bulk_upsert_settings.",
		))
	}
	return out
}

// firstMatch returns the first capture group from a regex, or empty
// string if there's no match.
func firstMatch(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// concatHTML joins every page's HTML into one big string so checks
// can do site-wide pattern matches without per-page bookkeeping.
func concatHTML(site *eval.SiteContext) string {
	var sb strings.Builder
	for _, p := range site.Pages {
		sb.WriteString(p.HTML)
		sb.WriteString("\n")
	}
	return sb.String()
}

// concatVisibleText extracts visible text content (what users read)
// from each page. Strips tags + script/style. Used for slop-term scans.
var (
	tagRE        = regexp.MustCompile(`<[^>]+>`)
	scriptStyleRE = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	wsRE          = regexp.MustCompile(`\s+`)
)

func concatVisibleText(site *eval.SiteContext) string {
	var sb strings.Builder
	for _, p := range site.Pages {
		stripped := scriptStyleRE.ReplaceAllString(p.HTML, " ")
		stripped = tagRE.ReplaceAllString(stripped, " ")
		stripped = wsRE.ReplaceAllString(stripped, " ")
		sb.WriteString(stripped)
		sb.WriteString("\n")
	}
	return sb.String()
}

// readSiteCSS reads any built CSS files in dist/ that the site
// context exposes. Falls back to scanning <style> tags inside HTML
// when no separate CSS file is loaded.
func readSiteCSS(site *eval.SiteContext) string {
	var sb strings.Builder
	for _, p := range site.Pages {
		// Pull <style> blocks from the page HTML so inlined Astro
		// styles are scanned. The eval package already loads the
		// per-site computed CSS bundle elsewhere; the design checks
		// only need broad pattern matches, not perfect coverage.
		matches := scriptStyleRE.FindAllStringSubmatch(p.HTML, -1)
		for _, m := range matches {
			if len(m) > 1 && strings.EqualFold(m[1], "style") {
				sb.WriteString(m[0])
				sb.WriteString("\n")
			}
		}
	}
	return sb.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "."
}

// newID generates an evaluations row id (12-byte hex). Same shape as
// eval.newID() but local to this package.
func newID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

var (
	// blockSectionRE captures every rendered block wrapper: the renderer
	// emits each block as `<section class="block block--XXX">`. The
	// captured group is the block_type slug.
	blockSectionRE = regexp.MustCompile(`<section[^>]+class="block\s+block--(\w+)`)
	// heroSectionRE matches the page-0 hero specifically. Used to scope
	// hero_quality content checks to the right snippet.
	heroSectionRE = regexp.MustCompile(`(?is)<section[^>]+class="block\s+block--hero(?:\s|")[^>]*>(.+?)</section>`)
	// splitHeroSectionRE matches split_hero (also a valid above-the-fold hero).
	splitHeroSectionRE = regexp.MustCompile(`(?is)<section[^>]+class="block\s+block--split_hero(?:\s|")[^>]*>(.+?)</section>`)
	// heroGraphicAnyRE matches any rendered hero_graphic variant child.
	heroGraphicAnyRE = regexp.MustCompile(`class="hero-graphic\s+hero-graphic--(\w+)"`)
)

// isHomepageRoot reports whether the slug is the EN or SV home page.
// The design_quality checks (hero_quality + above_the_fold_trust)
// should only grade the homepage. Legal pages, error pages, hub pages
// and article surfaces have different content patterns that do not
// need a curated hero or above-the-fold trust block.
func isHomepageRoot(slug string) bool {
	switch slug {
	case "/", "/sv", "/sv/":
		return true
	}
	return false
}

// heroQualityChecks fails when the homepage hero block ships without a
// curated hero_graphic, circuit background, or image. A naked hero
// reads as "AI builder demo" and is the single biggest first-impression
// regression. Scoped to homepage roots only (EN /, SV /sv) because
// every other page surface legitimately ships different patterns.
//
// Severity is warning by default; an env-flagged strict mode (read by
// the build handler) escalates warnings to errors so a non-Inspector
// hero blocks publish.
func heroQualityChecks(site *eval.SiteContext, fidelity agent.DesignFidelity) []eval.CheckResult {
	const max = 5
	var fails []string
	for _, p := range site.Pages {
		if !isHomepageRoot(p.Slug) {
			continue
		}
		if !hasQualityHero(p.HTML) {
			fails = append(fails, p.Slug)
		}
	}
	if len(fails) == 0 {
		return []eval.CheckResult{eval.Pass("hero_quality", "design_quality", max, "every landing-layout page-0 hero ships a curated graphic, circuit background, or image")}
	}
	// Showcase heroes are often bespoke custom blocks the block--hero
	// matcher can't judge; a deliberately plain typographic hero is also
	// a legitimate showcase statement. Advisory there, scored elsewhere.
	if fidelity == agent.FidelityShowcase {
		return []eval.CheckResult{eval.Info("hero_quality", "design_quality",
			fmt.Sprintf("Advisory in showcase fidelity: %d landing page(s) ship a plain block--hero (%s). If this is a deliberate typographic statement, fine; if not, set hero_graphic (mesh/pulse/monogram/audit-receipt/aurora), bg=circuit, an image, or a bespoke custom hero.", len(fails), strings.Join(fails, ", ")))}
	}
	deduct := max
	if deduct > len(fails)+1 {
		deduct = len(fails) + 1
	}
	msg := fmt.Sprintf("%d landing page(s) ship a plain text hero: %s. A plain hero on / reads as 'AI-builder demo' and is the single biggest first-impression regression.", len(fails), strings.Join(fails, ", "))
	rec := "Set data.hero_graphic to one of mesh / pulse / monogram / audit-receipt / gradient-orb / globe-wire, OR set bg=circuit, OR add an image_id. The kits ship a default; if you cleared it, restore one."
	return []eval.CheckResult{eval.Fail("hero_quality", "design_quality", deduct, eval.SeverityWarning, msg, rec)}
}

// hasQualityHero is true when the rendered hero block on this page
// carries a hero_graphic variant, a circuit background, or an inline
// image tag. False when the hero is plain text.
//
// We extract the WHOLE hero section (open tag + body + close tag) so
// modifier classes on the section element itself (has-circuit-bg,
// has-graphic) are visible to the predicate.
func hasQualityHero(pageHTML string) bool {
	// Locate the hero or split_hero opening tag and read everything up
	// to its closing </section>. firstMatch on the wrapped regex would
	// only return the inner capture; we need the entire matched chunk
	// (FindString) so the opening-tag class attribute is included.
	heroChunk := heroSectionRE.FindString(pageHTML)
	if heroChunk == "" {
		heroChunk = splitHeroSectionRE.FindString(pageHTML)
	}
	if heroChunk == "" {
		// No hero block at all on this page. Treated as not a quality
		// regression here, required-block rules live in the agent
		// guardrails and the eval engine's Has-H1 check.
		return true
	}
	if heroGraphicAnyRE.MatchString(heroChunk) {
		return true
	}
	if strings.Contains(heroChunk, "has-circuit-bg") || strings.Contains(heroChunk, "has-graphic") {
		return true
	}
	if strings.Contains(heroChunk, "<img ") || strings.Contains(heroChunk, "<picture") {
		return true
	}
	return false
}

// aboveTheFoldTrustChecks fails when none of {logo_strip, logo_carousel,
// stat_grid, quote, accordion_faq} appears in the first FIVE blocks of
// any landing page. Visitors decide in 100ms; if the first 5 blocks are
// all "hero + features + features + features + cta" with no trust
// signal (logos, numbers, quotes, real answers), the site reads as a
// pitch deck. Five blocks is the canonical Base44 / Lovable / Stripe
// pattern: hero, logos, stats, features, proof.
func aboveTheFoldTrustChecks(site *eval.SiteContext, fidelity agent.DesignFidelity) []eval.CheckResult {
	const max = 5
	trustTypes := map[string]bool{
		"logo_strip":      true,
		"logo_carousel":   true,
		"stat_grid":       true,
		"quote":           true,
		"accordion_faq":   true,
		"testimonial":     true, // schema isn't registered yet but reserve.
	}
	var fails []string
	for _, p := range site.Pages {
		if !isHomepageRoot(p.Slug) {
			continue
		}
		matches := blockSectionRE.FindAllStringSubmatch(p.HTML, 5)
		var seen bool
		for _, m := range matches {
			if len(m) < 2 {
				continue
			}
			if trustTypes[m[1]] {
				seen = true
				break
			}
		}
		if !seen && len(matches) > 0 {
			fails = append(fails, p.Slug)
		}
	}
	if len(fails) == 0 {
		return []eval.CheckResult{eval.Pass("above_the_fold_trust", "design_quality", max, "every landing page ships a trust signal (logos / stats / proof / FAQ) in the first 5 blocks")}
	}
	// Showcase narratives (full-screen scroll stories, launch teasers)
	// legitimately break the hero-logos-stats landing formula. Advisory.
	if fidelity == agent.FidelityShowcase {
		return []eval.CheckResult{eval.Info("above_the_fold_trust", "design_quality",
			fmt.Sprintf("Advisory in showcase fidelity: %d landing page(s) ship no conventional trust signal in the first 5 blocks (%s). Unconventional narratives are allowed; make sure SOMETHING earns trust above the fold (craft, proof, or specificity).", len(fails), strings.Join(fails, ", ")))}
	}
	deduct := max
	if deduct > len(fails)+1 {
		deduct = len(fails) + 1
	}
	msg := fmt.Sprintf("%d landing page(s) ship no trust signal in the first 5 blocks: %s. Without logos, numbers, or a real quote above the fold, the site reads as a pitch deck.", len(fails), strings.Join(fails, ", "))
	rec := "Add at least one of logo_strip / logo_carousel / stat_grid / accordion_faq / quote in the first 5 blocks. The 6 starter kits all ship this pattern out of the box; if you removed it, restore."
	return []eval.CheckResult{eval.Fail("above_the_fold_trust", "design_quality", deduct, eval.SeverityWarning, msg, rec)}
}
