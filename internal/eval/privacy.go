package eval

import (
	"fmt"
	"sort"
	"strings"

	"golang.org/x/net/html"
)

// Known consent platforms by script substring.
var consentPlatforms = []string{
	"cookiebot", "onetrust", "cookieconsent", "usercentrics",
	"trustarc", "didomi", "iubenda", "termly",
	"cookieproof", // Bright Interaction's own
}

// Known tracker platforms.
var trackerPlatforms = []string{
	"google-analytics", "googletagmanager", "facebook.net/tr",
	"hotjar", "clarity.ms", "linkedin.com/insight",
	"googleads", "tiktok.com/i18n/pixel", "pinterest.com/ct",
	"hubspot", "segment.com", "mixpanel", "fullstory",
	"mouseflow", "crazyegg", "heap.io", "amplitude",
	"posthog",
}

// trackerInlineAliases maps a substring that only ever shows up in an INLINE
// bootstrap snippet onto its canonical key in trackerPlatforms. The canonical
// GTM, GA4, Meta Pixel, LinkedIn and TikTok installs are inline loaders that
// inject their own <script src=...> at runtime, so the platform's scoring
// domain never appears in a src= attribute anywhere in the built HTML.
var trackerInlineAliases = map[string]string{
	"connect.facebook.net": "facebook.net/tr",
	"fbq(":                 "facebook.net/tr",
	"snap.licdn.com":       "linkedin.com/insight",
	"_linkedin_partner_id": "linkedin.com/insight",
	"analytics.tiktok.com": "tiktok.com/i18n/pixel",
	"ttq.load":             "tiktok.com/i18n/pixel",
	"static.hotjar.com":    "hotjar",
	"gtag(":                "googletagmanager",
	"datalayer.push":       "googletagmanager",
	"js.hs-scripts.com":    "hubspot",
	"cdn.segment.com":      "segment.com",
	"ct.pinterest.com":     "pinterest.com/ct",
	"pintrk(":              "pinterest.com/ct",
	"clarity(":             "clarity.ms",
}

// consentGateAttrs are the attributes a CMP uses to hold a tag back until the
// visitor opts in. Combined with type="text/plain" (the canonical Cookiebot /
// OneTrust / Usercentrics gate), they mark a tracker as compliant.
var consentGateAttrs = []string{
	"data-cookieconsent", "data-cookie-consent", "data-consent",
	"data-usercentrics", "data-cmp-ab", "data-ot-category",
}

// AI training bots that should be blocked in robots.txt.
var aiBots = []string{
	"GPTBot", "ChatGPT-User", "OAI-SearchBot",
	"anthropic-ai", "Claude-Web", "ClaudeBot",
	"Google-Extended", "Applebot-Extended", "FacebookBot",
	"PerplexityBot", "Bytespider", "Amazonbot", "YouBot",
	"DuckAssistBot", "CCBot",
}

// RunPrivacyChecks evaluates consent banners, trackers, AI bot blocking.
func RunPrivacyChecks(site *SiteContext) []CheckResult {
	var checks []CheckResult

	// Aggregate every <script> on every page, in DOM order, with the inline
	// body folded into the matched text. Reading src= alone made the canonical
	// inline GTM/GA4/Meta Pixel installs invisible, so a site loading GA4
	// unconditionally for EU visitors read "No trackers detected".
	scripts := collectScriptRefs(site)

	// Track whether ANY script tag carries data-consent-platform so a
	// platform whose filename gets hashed (e.g. atomicsite's
	// _ccb.{hash}.js bundle) is still detected.
	hasConsentAttr := false
	hasConsent := false
	for _, s := range scripts {
		if s.consentAttr {
			hasConsentAttr = true
		}
		if s.consent {
			hasConsent = true
		}
	}
	hasConsent = hasConsent || hasConsentAttr

	// 1. Consent banner detected
	checks = append(checks, siteWideCheck("Consent Banner", "consent", site, func() CheckResult {
		if hasConsent {
			return Pass("Consent Banner", "consent", 3, "Known consent platform detected")
		}
		return Fail("Consent Banner", "consent", 3, SeverityWarning,
			"No consent banner detected",
			"Add a consent banner (CookieProof recommended) before any tracking script.")
	}))

	// 2. Tracker count
	detectedTrackers := map[string]bool{}
	for _, s := range scripts {
		for _, t := range trackersIn(s.haystack) {
			detectedTrackers[t] = true
		}
	}
	checks = append(checks, siteWideCheckMulti("Tracker Count", "tracking", site, func() []CheckResult {
		switch {
		case len(detectedTrackers) == 0:
			return []CheckResult{Pass("Tracker Count", "tracking", 2, "No trackers detected")}
		case len(detectedTrackers) <= 2:
			// Honest partial split: a Pass with reduced weight scored 1/1
			// (full credit) instead of 1/2; the denominator must keep the
			// full weight.
			return []CheckResult{
				Pass("Tracker Count (within budget)", "tracking", 1,
					fmt.Sprintf("%d tracker(s) detected (budget 2): %s", len(detectedTrackers), strings.Join(sortedKeys(detectedTrackers), ", "))),
				Fail("Tracker Count (zero-tracker goal)", "tracking", 1, SeverityInfo,
					fmt.Sprintf("%d tracker(s) present; a tracker-free site scores full marks", len(detectedTrackers)),
					"Consider the server-side atomicsite tracking (no client trackers) or Umami instead."),
			}
		default:
			return []CheckResult{Fail("Tracker Count", "tracking", 2, SeverityWarning,
				fmt.Sprintf("%d trackers detected: %s", len(detectedTrackers), strings.Join(sortedKeys(detectedTrackers), ", ")),
				"Reduce trackers to <= 2 to limit data sharing.")}
		}
	})...)

	// 3. Pre-consent tracking. This used to be a boolean restatement of the
	// two checks above ("trackers exist AND no banner"), so a CMP in <head>
	// beside an ungated gtm.js passed at weight 3 with the detail "No tracking
	// before consent" -- a wrong statement about the tenant's legal position
	// that the check had not tested. It now inspects what it claims: per page,
	// in DOM order, is every tracker either consent-gated markup or placed
	// after the consent platform's own script.
	checks = append(checks, siteWideCheck("Pre-Consent Tracking", "consent", site, func() CheckResult {
		if len(detectedTrackers) == 0 {
			return Info("Pre-Consent Tracking", "consent",
				"No tracker scripts on this site; there is nothing to gate.")
		}
		violations := preConsentViolations(scripts)
		if len(violations) == 0 {
			return Pass("Pre-Consent Tracking", "consent", 3,
				"Every tracker script is consent-gated or loads after the consent platform")
		}
		return Fail("Pre-Consent Tracking", "consent", 3, SeverityError,
			"tracking executes before consent: "+strings.Join(firstN(violations, 3), "; "),
			"Gate every tracking tag behind explicit consent (type=\"text/plain\" plus your CMP's category attribute), or inject it only after the platform reports an opt-in. GDPR/ePrivacy compliance.")
	}))

	// 4. AI training bots blocked in robots.txt
	if site.RobotsTxt == "" {
		checks = append(checks, Fail("AI Training Bots Blocked", "robots", 2, SeverityWarning,
			"No robots.txt to evaluate", "Generate robots.txt; Atomicsite blocks AI bots by default."))
	} else {
		blocked, declared := parseRobotsBlocked(site.RobotsTxt)
		// A wildcard record is a real block. robots.txt precedence gives a
		// bot its OWN record when one exists, so the wildcard only applies to
		// bots with no group of their own; without this, a robots.txt that
		// disallows every crawler scored "No AI training bots blocked".
		blockedCount := 0
		wildcard := blocked["*"]
		for _, bot := range aiBots {
			b := strings.ToLower(bot)
			if declared[b] {
				if blocked[b] {
					blockedCount++
				}
				continue
			}
			if wildcard {
				blockedCount++
			}
		}
		wildcardNote := ""
		if wildcard {
			wildcardNote = " (via the wildcard User-agent: * Disallow: / record)"
		}
		switch {
		case blockedCount >= 3:
			checks = append(checks, Pass("AI Training Bots Blocked", "robots", 2,
				fmt.Sprintf("%d AI bots blocked%s", blockedCount, wildcardNote)))
		case blockedCount > 0:
			checks = append(checks, Fail("AI Training Bots Blocked", "robots", 1, SeverityWarning,
				fmt.Sprintf("Only %d AI bots blocked (recommend >= 3)", blockedCount),
				"Block GPTBot, ClaudeBot, Google-Extended, PerplexityBot, CCBot at minimum."))
		default:
			checks = append(checks, Fail("AI Training Bots Blocked", "robots", 2, SeverityWarning,
				"No AI training bots blocked",
				"Add Disallow: / for GPTBot, ClaudeBot, etc. in robots.txt."))
		}
	}

	// 5. Privacy policy link present
	hasPrivacy := false
	hasTerms := false
	for _, p := range site.Pages {
		for _, a := range elementsByTag(p.Doc, "a") {
			href := strings.ToLower(attr(a, "href"))
			text := strings.ToLower(textContent(a))
			if strings.Contains(href, "privacy") || strings.Contains(text, "privacy") ||
				strings.Contains(href, "integritetspolicy") || strings.Contains(href, "dataskydd") {
				hasPrivacy = true
			}
			if strings.Contains(href, "terms") || strings.Contains(text, "terms") ||
				strings.Contains(href, "villkor") || strings.Contains(href, "/legal") {
				hasTerms = true
			}
		}
		if hasPrivacy && hasTerms {
			break
		}
	}
	checks = append(checks, siteWideCheck("Privacy Policy Link", "legal", site, func() CheckResult {
		if hasPrivacy {
			return Pass("Privacy Policy Link", "legal", 2, "Privacy policy linked from at least one page")
		}
		return Fail("Privacy Policy Link", "legal", 2, SeverityError,
			"No privacy policy link found", "Add a link to /privacy in your footer.")
	}))
	checks = append(checks, siteWideCheck("Terms / Cookie Policy Link", "legal", site, func() CheckResult {
		if hasTerms {
			return Pass("Terms / Cookie Policy Link", "legal", 1, "Terms or cookie policy linked")
		}
		return Fail("Terms / Cookie Policy Link", "legal", 1, SeverityWarning,
			"No terms or cookie policy link", "Add /terms or /cookies link in footer.")
	}))

	// --- New checks ported from upgraded site-inspector (2026-04-27) ---

	// Cookie Count: heuristic on cookie-bearing scripts and document.cookie
	// writes inside inline <script>. Atomicsite-built sites are static so we
	// can't read Set-Cookie headers at eval time; what we CAN do is flag
	// pages that ship inline scripts that write cookies, which is the same
	// signal Site Inspector surfaces.
	cookieWriters := 0
	for _, p := range site.Pages {
		for _, s := range elementsByTag(p.Doc, "script") {
			if attr(s, "src") != "" {
				continue // external scripts go through Tracker Count above
			}
			body := textContent(s)
			if strings.Contains(body, "document.cookie") {
				cookieWriters++
			}
		}
	}
	checks = append(checks, siteWideCheck("Cookie Count", "tracking", site, func() CheckResult {
		switch {
		case cookieWriters == 0:
			return Pass("Cookie Count", "tracking", 1,
				"No inline cookie writers detected")
		case cookieWriters <= 2:
			return Pass("Cookie Count", "tracking", 1,
				fmt.Sprintf("%d inline cookie writer(s) detected", cookieWriters))
		default:
			return Fail("Cookie Count", "tracking", 1, SeverityWarning,
				fmt.Sprintf("%d inline cookie writers across pages", cookieWriters),
				"Move cookie writes behind consent. Inline document.cookie outside a consent gate fails GDPR.")
		}
	}))

	return checks
}

// scriptRef is one <script> element with everything the privacy checks need:
// which page it sits on, its DOM position within that page, the lowercased
// text platform signatures are matched against (src AND inline body), and
// whether a CMP holds it back until the visitor opts in.
type scriptRef struct {
	page        string
	order       int
	haystack    string
	consent     bool
	consentAttr bool
	gated       bool
}

// collectScriptRefs walks every page in DOM order. The inline body matters as
// much as src=: GTM, GA4, the Meta Pixel and the LinkedIn Insight Tag are all
// installed as inline loaders that inject their own <script src=...> at
// runtime, so their scoring domain never appears in a src= attribute.
func collectScriptRefs(site *SiteContext) []scriptRef {
	var out []scriptRef
	for _, p := range site.Pages {
		for i, s := range elementsByTag(p.Doc, "script") {
			ref := scriptRef{
				page:  p.Slug,
				order: i,
				haystack: strings.ToLower(
					attr(s, "src") + "\n" + textContent(s)),
				consentAttr: attr(s, "data-consent-platform") != "",
				gated:       isConsentGated(s),
			}
			ref.consent = ref.consentAttr || matchesAny(ref.haystack, consentPlatforms)
			out = append(out, ref)
		}
	}
	return out
}

// isConsentGated reports whether a CMP is holding this tag back. The canonical
// gate is a non-executable type (type="text/plain"), which Cookiebot,
// OneTrust and Usercentrics rewrite to text/javascript after opt-in.
func isConsentGated(s *html.Node) bool {
	t := strings.ToLower(strings.TrimSpace(attr(s, "type")))
	if t == "text/plain" || t == "text/cookieconsent" {
		return true
	}
	for _, a := range consentGateAttrs {
		if hasAttr(s, a) {
			return true
		}
	}
	return false
}

// trackersIn returns the canonical tracker-platform keys the text identifies,
// matching both the src= domain forms and the inline-snippet aliases.
func trackersIn(haystack string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(key string) {
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	for _, t := range trackerPlatforms {
		if strings.Contains(haystack, t) {
			add(t)
		}
	}
	for alias, key := range trackerInlineAliases {
		if strings.Contains(haystack, alias) {
			add(key)
		}
	}
	sort.Strings(out)
	return out
}

// preConsentViolations lists, per page and in DOM order, every tracker script
// that is neither consent-gated nor placed after the consent platform's own
// script. An ungated tag above the CMP loader has already fired by the time
// the banner paints.
func preConsentViolations(scripts []scriptRef) []string {
	byPage := map[string][]scriptRef{}
	var pages []string
	for _, s := range scripts {
		if _, ok := byPage[s.page]; !ok {
			pages = append(pages, s.page)
		}
		byPage[s.page] = append(byPage[s.page], s)
	}
	var out []string
	for _, page := range pages {
		consentAt := -1
		for _, s := range byPage[page] {
			if s.consent {
				consentAt = s.order
				break
			}
		}
		for _, s := range byPage[page] {
			if s.gated || s.consent {
				continue
			}
			names := trackersIn(s.haystack)
			if len(names) == 0 {
				continue
			}
			switch {
			case consentAt < 0:
				out = append(out, fmt.Sprintf("%s on %s loads with no consent platform on the page", names[0], page))
			case s.order < consentAt:
				out = append(out, fmt.Sprintf("%s on %s is script #%d, ahead of the consent script at #%d", names[0], page, s.order+1, consentAt+1))
			}
		}
	}
	return out
}

func matchesAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// parseRobotsBlocked walks robots.txt line-by-line, tracking which user-agents
// have a Disallow: / rule applied to them. Linear time, no regex backtracking.
// Comments (#) are ignored.
//
// Returns two maps keyed by lowercase agent: blocked (has a Disallow: / in its
// own record, including the "*" wildcard record) and declared (has a record at
// all). Callers need both because robots.txt precedence gives a bot its own
// record when one exists and falls back to "*" otherwise.
func parseRobotsBlocked(content string) (blocked map[string]bool, declared map[string]bool) {
	out := map[string]bool{}
	seen := map[string]bool{}
	var currentAgents []string
	for _, line := range strings.Split(content, "\n") {
		// Strip comments
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			// Blank line = end of record. Reset agent group.
			currentAgents = nil
			continue
		}
		colon := strings.Index(line, ":")
		if colon < 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:colon]))
		val := strings.TrimSpace(line[colon+1:])
		switch key {
		case "user-agent":
			agent := strings.ToLower(val)
			currentAgents = append(currentAgents, agent)
			seen[agent] = true
		case "disallow":
			if val == "/" {
				for _, a := range currentAgents {
					out[a] = true
				}
			}
		}
	}
	return out, seen
}
