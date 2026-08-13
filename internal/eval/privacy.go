package eval

import (
	"fmt"
	"sort"
	"strings"

	"golang.org/x/net/html"
)

// consentPlatforms are the domain fragments that name a CMP's own vendor
// bundle. They are matched two ways, both of which are genuine LOAD
// evidence -- the browser (or the page's own code) is telling a script
// element to fetch that host -- rather than a mere textual mention:
//
//  1. Against a <script>'s src= attribute (the browser fetched it).
//  2. Against a string literal assigned to a "src"-shaped property inside
//     an inline script, in CODE position with comments excluded (see
//     srcAssignedDomains in jslex.go). Several CMPs -- Termly's official
//     embed snippet among them -- and the canonical GTM/Meta Pixel/LinkedIn
//     bootstrap loaders all self-insert this way: create a <script>
//     element, set its .src, insert it, with no static src= attribute
//     anywhere in the built HTML. Treating only the HTML attribute as
//     evidence is what turned patch 3 into a false negative on exactly the
//     inline-bootstrap shape the original audit finding was about.
//
// Script TEXT beyond a src-assignment is deliberately NOT enough for full
// credit: see consentSignatures for why a bare API call or reference stays
// in the INDETERMINATE tier instead.
var consentPlatforms = []string{
	"cookiebot", "onetrust", "cookieconsent", "usercentrics",
	"trustarc", "didomi", "iubenda", "termly",
	"cookieproof", // Bright Interaction's own
}

// cmpSignal is one dotted-identifier-chain pattern that suggests a CMP's API
// is in use. "*" matches any single identifier segment (a namespace's
// varying method name).
type cmpSignal struct {
	key   string
	chain []string
}

// consentSignatures are code-position markers -- never proof of a load, only
// suggestive of one -- for the INDETERMINATE tier: an identifier chain that
// appears in real code (not a comment, not a string, not a regex; see
// codeChainUsed) immediately followed by a call `(` or by a plain assignment
// `=` to it.
//
// A match here is deliberately NOT enough for Consent Banner to pass. Every
// one of these signatures is a namespace or global the CMP's bundle DEFINES
// once it loads; referencing it is consistent with the platform being live
// elsewhere (a CDN edge, an SPA router the crawler cannot see), and equally
// consistent with dead code calling an undefined global that would throw at
// runtime. Static analysis cannot tell those two apart -- that is the
// premise of this whole redesign, not a gap in it -- so a match is reported
// as unresolved and scored as zero, the same as no evidence at all, with
// wording that tells the tenant to verify manually rather than a silent
// pass. See consentPlatforms for what DOES count as full credit.
var consentSignatures = []cmpSignal{
	{"cookiebot", []string{"cookiebot", "*"}},         // Cookiebot.init(), .renew(), .show(), .consent = ...
	{"onetrust", []string{"onetrust", "*"}},            // OneTrust.InsertScript(), .ToggleInfoDisplay()
	{"onetrust", []string{"optanonwrapper"}},           // OneTrust's required page-defined callback
	{"cookieconsent", []string{"cookieconsent", "*"}},  // CookieConsent.run(), .initialise()
	{"cookieconsent", []string{"initcookieconsent"}},
	{"usercentrics", []string{"uc_ui", "*"}},
	{"usercentrics", []string{"usercentrics", "*"}},
	{"trustarc", []string{"trustarc", "*"}},
	{"didomi", []string{"didomi", "*"}},
	{"didomi", []string{"window", "didomionready"}},
	{"didomi", []string{"window", "didomiconfig"}},
	{"iubenda", []string{"_iub", "*"}},
	{"termly", []string{"termly", "*"}},
	{"termly", []string{"window", "termly"}},
	{"cookieproof", []string{"cookieproof", "*"}},
	{"cookieproof", []string{"window", "cookieproof"}},
	{"tcf", []string{"__tcfapi"}}, // IAB TCF entry point; only a loaded CMP registers it
}

// consentCallbackPrefixes catches page-defined callback FUNCTIONS whose name
// a CMP's own docs mandate, with a developer-chosen suffix a dotted chain
// cannot express: Cookiebot requires `function CookiebotCallback_OnAccept()
// {...}`-shaped names.
var consentCallbackPrefixes = []struct{ key, prefix string }{
	{"cookiebot", "cookiebotcallback_"},
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

	// Aggregate every <script> on every page, in DOM order. Tracker matching
	// folds the inline body in, because reading src= alone made the canonical
	// inline GTM/GA4/Meta Pixel installs invisible and a site loading GA4
	// unconditionally for EU visitors read "No trackers detected". Consent
	// matching does not: see collectScriptRefs.
	scripts := collectScriptRefs(site)

	// hasConsent is full LOAD evidence: a src= attribute or inline .src=
	// assignment naming a CMP's own domain, or atomicsite's own
	// data-consent-platform attribute (a platform whose filename gets
	// hashed, e.g. the _ccb.{hash}.js bundle, and so cannot be matched by
	// domain at all). Only this tier awards Consent Banner credit.
	//
	// weakKeys is code-position REFERENCE evidence -- a call into or
	// assignment to a CMP's API surface, with no matching load anywhere on
	// the page. It is real (the tokenizer in jslex.go excludes comments,
	// strings and regex bodies, so it cannot be spoofed the way three prior
	// remediations' text matching could), but it does not prove the bundle
	// that defines the API is actually loaded: the reference could be dead
	// code, or code a CDN/SPA layer this crawler cannot see makes live.
	// Static analysis cannot resolve that, so it is scored as INDETERMINATE
	// -- zero credit, distinct wording telling the tenant to verify by
	// hand -- rather than folded into either a hard pass or a hard fail.
	hasConsent := false
	weakKeys := map[string]bool{}
	for _, s := range scripts {
		if s.consentAttr || s.consent {
			hasConsent = true
		}
		if s.consentWeak && s.weakKey != "" {
			weakKeys[s.weakKey] = true
		}
	}

	// 1. Consent banner detected
	checks = append(checks, siteWideCheck("Consent Banner", "consent", site, func() CheckResult {
		if hasConsent {
			return Pass("Consent Banner", "consent", 3, "Known consent platform detected")
		}
		if len(weakKeys) > 0 {
			return Fail("Consent Banner", "consent", 3, SeverityInfo,
				fmt.Sprintf("Inline code references %s, but no script on this site actually loads it (no src=, no data-consent-platform, no inline src assignment to its domain) -- this cannot be confirmed statically",
					strings.Join(sortedKeys(weakKeys), ", ")),
				"Verify manually that the referenced consent platform is really bootstrapped, or make the load visible to static analysis (a real <script src>, or an inline .src= assignment to the platform's domain).")
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
// text TRACKER signatures are matched against (src AND inline body), and
// whether a CMP is loaded by it, merely referenced by it, or holds it back
// until the visitor opts in.
type scriptRef struct {
	page        string
	order       int
	haystack    string // src + code + string-literal text, comments excluded -- see collectScriptRefs
	consent     bool   // strong: src=/inline .src= domain match, or data-consent-platform
	consentAttr bool
	consentWeak bool   // weak: a CMP API referenced in code position, no load evidence anywhere
	weakKey     string // which CMP consentWeak names, for messaging
	gated       bool
}

// collectScriptRefs walks every page in DOM order, tokenizing each inline
// script body with the JS lexer in jslex.go so every downstream check
// reasons about actual token kinds -- code, string, regex, comment -- instead
// of raw substrings. That distinction is the whole fix: three prior
// remediations of this file all tried to patch the SHAPE of a text match
// (fold in bodies, strip comments, revert to src-only) and each patch's gap
// was just the next text shape a comment/string/regex could take.
//
// For TRACKERS the inline body matters as much as src=: GTM, GA4, the Meta
// Pixel and the LinkedIn Insight Tag are all installed as inline loaders that
// inject their own <script src=...> at runtime, so their scoring domain never
// appears in a src= attribute. A tracker false positive costs a tenant an
// investigation, so the bar stays low: any code or string-literal token
// naming the platform, comments excluded (see trackersIn / codeHaystack /
// stringHaystack).
//
// For CONSENT a false positive hands a non-compliant site a compliance pass
// it will act on, so the bar is load evidence only: the src= URL names a
// platform, a data-consent-platform attribute is present, or an inline
// `.src = "..."` assignment in code position names a platform's domain (see
// consentPlatforms). A CMP's API referenced in code -- a call or an
// assignment to it, per consentSignatures -- is real but weaker evidence and
// is tracked separately as consentWeak for the Consent Banner check's
// INDETERMINATE tier; it never sets consent=true.
func collectScriptRefs(site *SiteContext) []scriptRef {
	var out []scriptRef
	for _, p := range site.Pages {
		for i, s := range elementsByTag(p.Doc, "script") {
			src := strings.ToLower(attr(s, "src"))
			body := textContent(s)
			toks := tokenizeJS(body)
			ct := codeTokens(toks)
			codeText := codeHaystack(ct)
			strText := stringHaystack(ct)

			ref := scriptRef{
				page:        p.Slug,
				order:       i,
				haystack:    src + "\n" + codeText + "\n" + strText,
				consentAttr: attr(s, "data-consent-platform") != "",
				gated:       isConsentGated(s),
			}

			ref.consent = ref.consentAttr || matchesAny(src, consentPlatforms)
			if !ref.consent {
				for _, domain := range srcAssignedDomains(ct) {
					if matchesAny(domain, consentPlatforms) {
						ref.consent = true
						break
					}
				}
			}

			if !ref.consent {
				for _, sig := range consentSignatures {
					if codeChainUsed(ct, sig.chain) {
						ref.consentWeak = true
						ref.weakKey = sig.key
						break
					}
				}
				if !ref.consentWeak {
					for _, cb := range consentCallbackPrefixes {
						if identPrefixDeclared(ct, cb.prefix) {
							ref.consentWeak = true
							ref.weakKey = cb.key
							break
						}
					}
				}
			}

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
// that is not consent-gated. Document order is still evaluated on the node
// tree (byPage groups the real scriptRefs in the order collectScriptRefs
// walked them) and still drives the diagnostic wording, but it is NOT, by
// itself, an excuse any more.
//
// It used to be: a tracker script ordered after some other script carrying
// consent evidence was treated as safe (the "s.order > consentAt" case fell
// through every switch branch with no violation reported at all). That is
// false safety. GDPR/ePrivacy cares whether the SCRIPT ITSELF is prevented
// from executing until consent, which in static HTML is a technical
// property of the tag (type="text/plain" plus a CMP category attribute --
// isConsentGated) or of the tracker being injected FROM INSIDE a consent
// callback (out of scope here, see the package doc comment). It is not a
// property of what else happens to sit earlier in the document: a CMP
// loader placed above an ordinary, ungated <script src=gtm.js> does not stop
// that tag from firing the instant the parser reaches it. A verifier
// reproduced the original six-point fabrication through exactly this gap:
// put any script carrying a CMP signature -- including a merely WEAK,
// unverified one -- ahead of an otherwise-ungated tracker, and the old code
// never reported it. Only isConsentGated excuses a tracker now.
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
		// consentAt is diagnostic context only (which message to print), not
		// a decision input: see the switch below, whose default case now
		// covers what used to be silently excused.
		consentAt := -1
		for _, s := range byPage[page] {
			if s.consent {
				consentAt = s.order
				break
			}
		}
		for _, s := range byPage[page] {
			if s.gated {
				continue
			}
			names := trackersIn(s.haystack)
			if len(names) == 0 {
				continue
			}
			switch {
			case s.consent || s.consentWeak:
				// Carrying a CMP signature -- even a merely referenced,
				// unverified one -- is NOT an excuse. A script that
				// bootstraps (or references) the consent platform AND a
				// tracker in one blob runs the tracker unconditionally,
				// which is precisely the misconfiguration this check exists
				// to catch.
				out = append(out, fmt.Sprintf("%s on %s runs in the same script tag as a consent platform reference, so it is not actually gated by it", names[0], page))
			case consentAt < 0:
				out = append(out, fmt.Sprintf("%s on %s loads with no consent platform on the page", names[0], page))
			case s.order < consentAt:
				out = append(out, fmt.Sprintf("%s on %s is script #%d, ahead of the consent script at #%d", names[0], page, s.order+1, consentAt+1))
			default:
				out = append(out, fmt.Sprintf("%s on %s is script #%d; a consent platform loads earlier at #%d but does not gate this tag (no type=\"text/plain\" or consent-category attribute)", names[0], page, s.order+1, consentAt+1))
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
