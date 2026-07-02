package critique

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/bright-interaction/slab/internal/agent"
	"github.com/bright-interaction/slab/internal/eval"
)

// This file replaces the old prose-substring needle extraction
// (extractAntiPatternNeedle) with a per-entry structured matcher registry.
//
// The old approach matched a hardcoded known[] token list, so of the 25
// playbook anti-patterns only ~6 produced a real Pass/Fail check; the rest
// fell through to zero-weight Info, and two ('AI gradient', cliche icons)
// extracted garbage single-quote needles that ALWAYS passed. "25 anti-patterns
// checked" was therefore misleading.
//
// Now each entry maps to either:
//   - a real detector that runs against the BUILT output (HTML/CSS/text) and
//     emits a weighted Pass/Fail, or
//   - advisory=true, meaning the pattern cannot be reliably detected from
//     rendered output (layout intent, icon-name choice lost at render, color
//     temperature judgement) and is emitted as an HONEST Info instead of a
//     fake Pass.
//
// The playbook (agent.DefaultDesignPlaybook) stays the single source of truth:
// a rule attaches to whatever entry contains its `key` substring, so adding a
// playbook entry just defaults to advisory Info until a matcher is written.

// antiPatternInput is the built-output surface the detectors scan. The lower
// fields are pre-lowercased once; headings keep their original case for the
// title-case heuristic.
type antiPatternInput struct {
	htmlLower string
	cssLower  string
	textLower string
	headings  []string
}

// antiPatternClass separates the rules the fidelity dial may relax from
// the ones it never touches. classAuthenticity (slop, fake data, cliche
// copy) and classCorrectness (real bugs like 100vh on iOS) score in
// EVERY fidelity; classTaste demotes to advisory Info in showcase so
// the grade honestly reflects the showcase rubric (the entry leaves
// both numerator and denominator, it is not a gifted Pass).
type antiPatternClass int

const (
	classTaste antiPatternClass = iota
	classAuthenticity
	classCorrectness
)

// antiPatternRule binds a playbook entry (matched by `key`) to a detector.
type antiPatternRule struct {
	key      string           // lowercased substring uniquely identifying the playbook Banned text
	advisory bool             // true => no reliable rendered-output signal; emit Info
	class    antiPatternClass // which rules the fidelity dial may demote
	detect   func(in antiPatternInput) bool
}

var antiPatternRules = []antiPatternRule{
	{key: "inter font", class: classTaste, detect: detectInterHeading},
	{key: "roboto, open sans", class: classTaste, detect: detectGenericHeadingFont},
	{key: "pure black", class: classTaste, detect: detectPureBlack},
	{key: "shadow-md", advisory: true},     // tailwind class; renderer emits computed CSS, token-coherence covers shadow drift
	{key: "oversaturated", advisory: true}, // saturation judgement too noisy from hex alone
	{key: "ai gradient", class: classTaste, detect: detectAIGradient},
	{key: "warm and cool grays", advisory: true},
	{key: "linear or ease-in-out", class: classTaste, detect: detectBlandTransition},
	{key: "lucide / feather", advisory: true}, // icons render as generic <svg class="icon">, default-state not visible
	{key: "cliché icons", advisory: true},     // icon NAME is stripped at render, can't tell rocket/shield/cog apart
	{key: "cliche icons", advisory: true},
	{key: "three equal cards", advisory: true},
	{key: "centered hero with text over an image", class: classTaste, detect: detectHeroOverImage},
	{key: "h-screen", class: classCorrectness, detect: detectFixedViewportHeight},
	{key: "edge-to-edge content", advisory: true},
	{key: "symmetric vertical padding", advisory: true},
	{key: "1px solid gray border", advisory: true}, // renderer uses color-mix borders; raw-border drift is too noisy to flag cleanly
	{key: "sticky navbars", advisory: true},
	{key: "filling sections", advisory: true},
	{key: "john doe", class: classAuthenticity, detect: detectFakeNames},
	{key: "round fake numbers", class: classAuthenticity, detect: detectFakeNumbers},
	{key: "acme corp", class: classAuthenticity, detect: detectFakeCompanies},
	{key: "title case on every heading", class: classTaste, detect: detectTitleCaseHeadings},
	{key: "exclamation marks", class: classAuthenticity, detect: detectExclamationCTAs},
	{key: "ai copywriting clichés", class: classAuthenticity, detect: detectAICliches},
	{key: "ai copywriting cliches", class: classAuthenticity, detect: detectAICliches},
	{key: "lorem ipsum", class: classAuthenticity, detect: detectLorem},
}

// matchAntiPattern returns the rule whose key is a substring of the playbook
// entry's Banned text, or nil when the entry has no registered matcher.
func matchAntiPattern(banned string) *antiPatternRule {
	low := strings.ToLower(banned)
	for i := range antiPatternRules {
		if strings.Contains(low, antiPatternRules[i].key) {
			return &antiPatternRules[i]
		}
	}
	return nil
}

// antiPatternChecks scans built output for each playbook anti-pattern. Detected
// entries produce a weighted Pass/Fail; entries with no reliable rendered-output
// signal produce an honest advisory Info (zero weight) rather than a fake Pass.
// In showcase fidelity, taste-class detectors run but emit advisory Info
// (visible, unscored, out of the denominator); authenticity and correctness
// detectors score identically in every fidelity.
func antiPatternChecks(patterns []agent.AntiPattern, html, css, text string, headings []string, fidelity agent.DesignFidelity) []eval.CheckResult {
	in := antiPatternInput{
		htmlLower: strings.ToLower(html),
		cssLower:  strings.ToLower(css),
		textLower: strings.ToLower(text),
		headings:  headings,
	}
	out := make([]eval.CheckResult, 0, len(patterns))
	for _, ap := range patterns {
		name := truncate(ap.Banned, 60)
		rule := matchAntiPattern(ap.Banned)
		if rule == nil || rule.advisory {
			out = append(out, eval.Info(name, "anti_patterns",
				"Advisory (not auto-detectable from rendered output): "+ap.Preferred))
			continue
		}
		if fidelity == agent.FidelityShowcase && rule.class == classTaste {
			detail := "Advisory in showcase fidelity (taste rule, unscored): no match in built output."
			if rule.detect(in) {
				detail = "Advisory in showcase fidelity (taste rule, unscored): pattern present. " + ap.Preferred
			}
			out = append(out, eval.Info(name, "anti_patterns", detail))
			continue
		}
		if rule.detect(in) {
			out = append(out, eval.Fail(
				name, "anti_patterns", 5, eval.SeverityWarning,
				"Banned pattern detected in built output: "+ap.Banned,
				ap.Preferred,
			))
		} else {
			out = append(out, eval.Pass(name, "anti_patterns", 5, "no match in built output"))
		}
	}
	return out
}

// --- detectors -------------------------------------------------------------

var (
	hexAnyRE           = regexp.MustCompile(`#[0-9a-f]{3,8}\b`)
	fontHeadingVarRE   = regexp.MustCompile(`--font-heading\s*:\s*([^;}\n]+)`)
	fontFamilyDeclRE   = regexp.MustCompile(`font-family\s*:\s*([^;}\n]+)`)
	gradientRE         = regexp.MustCompile(`(?:linear|radial|conic)-gradient\s*\(([^)]*)\)`)
	transitionEaseRE   = regexp.MustCompile(`transition[^;{}]*\bease-in-out\b`)
	transitionLinearRE = regexp.MustCompile(`transition-timing-function\s*:\s*linear\b`)
	fixedViewportVHRE  = regexp.MustCompile(`\b100vh\b`)
	critiqueHeadingRE  = regexp.MustCompile(`(?is)<h[1-3][^>]*>(.*?)</h[1-3]>`)
	ctaElementRE       = regexp.MustCompile(`(?is)<(?:button|a)[^>]*class="[^"]*(?:cta|btn|button)[^"]*"[^>]*>(.*?)</(?:button|a)>`)
)

// primaryFamily returns the first (primary) family in a font-family value,
// stripped of quotes. e.g. "'Space Grotesk', Inter, sans-serif" -> "space grotesk".
func primaryFamily(value string) string {
	v := strings.TrimSpace(value)
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.Trim(v, " '\"")
}

// primaryFontFamilies extracts the primary (first) family of every
// --font-heading and font-family declaration in the CSS (lowercased input),
// pipe-joined so a caller can substring-test. Scanning the primary family only
// is what makes "Inter as a body fallback" pass while "font-family: Inter,
// sans-serif" (Inter primary) fails, matching the playbook's intent.
func primaryFontFamilies(cssLower string) string {
	var sb strings.Builder
	collect := func(re *regexp.Regexp) {
		for _, m := range re.FindAllStringSubmatch(cssLower, -1) {
			sb.WriteString(primaryFamily(m[1]))
			sb.WriteByte('|')
		}
	}
	collect(fontHeadingVarRE)
	collect(fontFamilyDeclRE)
	return sb.String()
}

// detectInterHeading flags Inter as the PRIMARY family anywhere (Inter is fine
// as a non-primary fallback per the playbook, so a trailing fallback passes).
func detectInterHeading(in antiPatternInput) bool {
	return strings.Contains(primaryFontFamilies(in.cssLower), "inter")
}

func detectGenericHeadingFont(in antiPatternInput) bool {
	fams := primaryFontFamilies(in.cssLower)
	for _, f := range []string{"roboto", "open sans", "helvetica", "arial"} {
		if strings.Contains(fams, f) {
			return true
		}
	}
	return false
}

func detectPureBlack(in antiPatternInput) bool {
	for _, h := range hexAnyRE.FindAllString(in.cssLower+"\n"+in.htmlLower, -1) {
		if h == "#000" || h == "#000000" {
			return true
		}
	}
	return strings.Contains(in.cssLower, "rgb(0,0,0)") || strings.Contains(in.cssLower, "rgb(0, 0, 0)")
}

// detectAIGradient flags a CSS gradient whose stops carry a purple/blue hue,
// the classic AI fingerprint. Conservative: a gradient without a purple/blue
// cue is not flagged, so a legitimate brand-accent gradient passes.
func detectAIGradient(in antiPatternInput) bool {
	cues := []string{"purple", "violet", "indigo", "blueviolet", "rebeccapurple",
		"#6366f1", "#818cf8", "#8b5cf6", "#a855f7", "#7c3aed", "#4f46e5", "#c084fc", "#a78bfa"}
	for _, m := range gradientRE.FindAllStringSubmatch(in.cssLower+"\n"+in.htmlLower, -1) {
		stops := m[1]
		for _, c := range cues {
			if strings.Contains(stops, c) {
				return true
			}
		}
	}
	return false
}

func detectBlandTransition(in antiPatternInput) bool {
	return transitionEaseRE.MatchString(in.cssLower) || transitionLinearRE.MatchString(in.cssLower)
}

// detectHeroOverImage flags a centered hero (block--hero, not the approved
// image-right split_hero) that ships an inline image, i.e. text-over-image.
func detectHeroOverImage(in antiPatternInput) bool {
	chunk := heroSectionRE.FindString(in.htmlLower)
	if chunk == "" {
		return false
	}
	return strings.Contains(chunk, "<img") || strings.Contains(chunk, "<picture")
}

// detectFixedViewportHeight flags the iOS-buggy 100vh (the playbook wants
// 100dvh; "100dvh" does not contain "100vh" so there is no overlap) and the
// tailwind h-screen utility if it ever leaks into rendered markup.
func detectFixedViewportHeight(in antiPatternInput) bool {
	return fixedViewportVHRE.MatchString(in.cssLower) || strings.Contains(in.htmlLower, "h-screen")
}

func detectFakeNames(in antiPatternInput) bool {
	return textContainsAny(in.textLower, "john doe", "jane smith", "sarah chan", "john smith")
}

func detectFakeNumbers(in antiPatternInput) bool {
	return textContainsAny(in.textLower, "99.99%", "$100.00", "1234567", "1,234,567")
}

func detectFakeCompanies(in antiPatternInput) bool {
	return textContainsAny(in.textLower, "acme corp", "acme inc", "smartflow", "techflow")
}

func detectAICliches(in antiPatternInput) bool {
	return textContainsAny(in.textLower, "elevate", "seamless", "unleash", "next-gen",
		"game-changer", "game changer", "delve", "tapestry", "in the world of")
}

func detectLorem(in antiPatternInput) bool {
	return strings.Contains(in.textLower, "lorem ipsum")
}

// detectExclamationCTAs flags exclamation marks in headings or CTA/button
// labels, the loud-and-cheap tell the playbook bans.
func detectExclamationCTAs(in antiPatternInput) bool {
	for _, h := range in.headings {
		if strings.Contains(h, "!") {
			return true
		}
	}
	for _, m := range ctaElementRE.FindAllStringSubmatch(in.htmlLower, -1) {
		if strings.Contains(m[1], "!") {
			return true
		}
	}
	return false
}

// detectTitleCaseHeadings flags when the strict majority of multi-word headings
// are Title Case (the playbook prefers sentence case for headlines).
func detectTitleCaseHeadings(in antiPatternInput) bool {
	multiWord, titleCase := 0, 0
	for _, h := range in.headings {
		words := strings.Fields(h)
		if len(words) < 3 {
			continue
		}
		multiWord++
		if isTitleCase(words) {
			titleCase++
		}
	}
	return multiWord >= 2 && titleCase >= 2 && titleCase*2 > multiWord
}

var titleStopwords = map[string]bool{
	"a": true, "an": true, "the": true, "and": true, "or": true, "of": true,
	"to": true, "in": true, "on": true, "for": true, "with": true, "at": true,
	"by": true, "as": true, "is": true, "vs": true,
}

// isTitleCase reports whether every significant (non-stopword) word in the
// heading starts with an uppercase letter.
func isTitleCase(words []string) bool {
	sig, capd := 0, 0
	for _, w := range words {
		trimmed := strings.Trim(w, ".,:;!?\"'")
		if trimmed == "" {
			continue
		}
		if titleStopwords[strings.ToLower(trimmed)] {
			continue
		}
		sig++
		r := []rune(trimmed)
		if len(r) > 0 && unicode.IsUpper(r[0]) {
			capd++
		}
	}
	return sig >= 2 && capd == sig
}

func textContainsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// extractHeadings pulls visible <h1-3> text (tags stripped) from every page,
// preserving original case for the title-case heuristic.
func extractHeadings(site *eval.SiteContext) []string {
	var out []string
	for _, p := range site.Pages {
		for _, m := range critiqueHeadingRE.FindAllStringSubmatch(p.HTML, -1) {
			t := strings.TrimSpace(tagRE.ReplaceAllString(m[1], ""))
			if t != "" {
				out = append(out, t)
			}
		}
	}
	return out
}
