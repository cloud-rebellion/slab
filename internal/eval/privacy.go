package eval

import (
	"fmt"
	"strings"
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

	// Aggregate scripts across all pages
	scripts := []string{}
	// Track whether ANY script tag carries data-consent-platform so a
	// platform whose filename gets hashed (e.g. atomicsite's
	// _ccb.{hash}.js bundle) is still detected.
	hasConsentAttr := false
	for _, p := range site.Pages {
		for _, s := range elementsByTag(p.Doc, "script") {
			if src := attr(s, "src"); src != "" {
				scripts = append(scripts, strings.ToLower(src))
			}
			if cp := attr(s, "data-consent-platform"); cp != "" {
				hasConsentAttr = true
			}
		}
	}

	// 1. Consent banner detected
	hasConsent := hasConsentAttr
	for _, s := range scripts {
		for _, c := range consentPlatforms {
			if strings.Contains(s, c) {
				hasConsent = true
				break
			}
		}
		if hasConsent {
			break
		}
	}
	if hasConsent {
		checks = append(checks, Pass("Consent Banner", "consent", 3, "Known consent platform detected"))
	} else {
		checks = append(checks, Fail("Consent Banner", "consent", 3, SeverityWarning,
			"No consent banner detected",
			"Add a consent banner (CookieProof recommended) before any tracking script."))
	}

	// 2. Tracker count
	detectedTrackers := map[string]bool{}
	for _, s := range scripts {
		for _, t := range trackerPlatforms {
			if strings.Contains(s, t) {
				detectedTrackers[t] = true
			}
		}
	}
	switch {
	case len(detectedTrackers) == 0:
		checks = append(checks, Pass("Tracker Count", "tracking", 2, "No trackers detected"))
	case len(detectedTrackers) <= 2:
		// Honest partial split: a Pass with reduced weight scored 1/1
		// (full credit) instead of 1/2; the denominator must keep the
		// full weight.
		checks = append(checks,
			Pass("Tracker Count (within budget)", "tracking", 1,
				fmt.Sprintf("%d tracker(s) detected (budget 2)", len(detectedTrackers))),
			Fail("Tracker Count (zero-tracker goal)", "tracking", 1, SeverityInfo,
				fmt.Sprintf("%d tracker(s) present; a tracker-free site scores full marks", len(detectedTrackers)),
				"Consider the server-side atomicsite tracking (no client trackers) or Umami instead."))
	default:
		checks = append(checks, Fail("Tracker Count", "tracking", 2, SeverityWarning,
			fmt.Sprintf("%d trackers detected", len(detectedTrackers)),
			"Reduce trackers to <= 2 to limit data sharing."))
	}

	// 3. Pre-consent tracking (trackers without consent banner)
	if len(detectedTrackers) > 0 && !hasConsent {
		checks = append(checks, Fail("Pre-Consent Tracking", "consent", 3, SeverityError,
			"Trackers loaded without consent banner",
			"Gate ALL tracking scripts behind explicit consent. GDPR/ePrivacy compliance."))
	} else {
		checks = append(checks, Pass("Pre-Consent Tracking", "consent", 3,
			"No tracking before consent (or no trackers)"))
	}

	// 4. AI training bots blocked in robots.txt
	if site.RobotsTxt == "" {
		checks = append(checks, Fail("AI Training Bots Blocked", "robots", 2, SeverityWarning,
			"No robots.txt to evaluate", "Generate robots.txt; Atomicsite blocks AI bots by default."))
	} else {
		blocked := parseRobotsBlocked(site.RobotsTxt)
		blockedCount := 0
		for _, bot := range aiBots {
			if blocked[strings.ToLower(bot)] {
				blockedCount++
			}
		}
		switch {
		case blockedCount >= 3:
			checks = append(checks, Pass("AI Training Bots Blocked", "robots", 2,
				fmt.Sprintf("%d AI bots blocked", blockedCount)))
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
	if hasPrivacy {
		checks = append(checks, Pass("Privacy Policy Link", "legal", 2, "Privacy policy linked from at least one page"))
	} else {
		checks = append(checks, Fail("Privacy Policy Link", "legal", 2, SeverityError,
			"No privacy policy link found", "Add a link to /privacy in your footer."))
	}
	if hasTerms {
		checks = append(checks, Pass("Terms / Cookie Policy Link", "legal", 1, "Terms or cookie policy linked"))
	} else {
		checks = append(checks, Fail("Terms / Cookie Policy Link", "legal", 1, SeverityWarning,
			"No terms or cookie policy link", "Add /terms or /cookies link in footer."))
	}

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
	switch {
	case cookieWriters == 0:
		checks = append(checks, Pass("Cookie Count", "tracking", 1,
			"No inline cookie writers detected"))
	case cookieWriters <= 2:
		checks = append(checks, Pass("Cookie Count", "tracking", 1,
			fmt.Sprintf("%d inline cookie writer(s) detected", cookieWriters)))
	default:
		checks = append(checks, Fail("Cookie Count", "tracking", 1, SeverityWarning,
			fmt.Sprintf("%d inline cookie writers across pages", cookieWriters),
			"Move cookie writes behind consent. Inline document.cookie outside a consent gate fails GDPR."))
	}

	return checks
}

// parseRobotsBlocked walks robots.txt line-by-line, tracking which user-agents
// have a Disallow: / rule applied to them. Linear time, no regex backtracking.
// Comments (#) are ignored. Returns map[lowercase-agent]bool.
func parseRobotsBlocked(content string) map[string]bool {
	out := map[string]bool{}
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
			currentAgents = append(currentAgents, strings.ToLower(val))
		case "disallow":
			if val == "/" {
				for _, a := range currentAgents {
					out[a] = true
				}
			}
		}
	}
	return out
}
