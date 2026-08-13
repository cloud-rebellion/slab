package eval

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// hstsMaxAgeRE captures `max-age=N` from a Strict-Transport-Security header.
var hstsMaxAgeRE = regexp.MustCompile(`(?i)max-age\s*=\s*(\d+)`)

// RunSecurityChecks evaluates HTTP headers + HTML for security best practices.
// Headers are sourced from BuildSecurityHeaders (what the site WOULD emit when
// deployed via _headers or nginx.conf).
func RunSecurityChecks(site *SiteContext) []CheckResult {
	var checks []CheckResult
	h := site.Headers

	add := func(name, section string, present bool, weight int, recIfFail string) {
		if present {
			checks = append(checks, Pass(name, section, weight, "Header present"))
		} else {
			checks = append(checks, Fail(name, section, weight, SeverityWarning, "Header missing", recIfFail))
		}
	}

	// 1-9. Core security headers
	add("Strict-Transport-Security", "headers", h["Strict-Transport-Security"] != "", 2,
		"Enable HSTS in Settings -> Security to enforce HTTPS for 1 year.")
	add("Content-Security-Policy", "headers", h["Content-Security-Policy"] != "", 3,
		"Enable CSP. Atomicsite auto-builds it from your allowed scripts.")
	add("X-Content-Type-Options: nosniff", "headers",
		strings.EqualFold(h["X-Content-Type-Options"], "nosniff"), 1,
		"Set X-Content-Type-Options to 'nosniff'.")
	add("X-Frame-Options", "headers", h["X-Frame-Options"] != "", 2,
		"Set X-Frame-Options to DENY or SAMEORIGIN to prevent clickjacking.")
	add("Referrer-Policy", "headers", h["Referrer-Policy"] != "", 1,
		"Set Referrer-Policy to strict-origin-when-cross-origin.")
	add("Permissions-Policy", "headers", h["Permissions-Policy"] != "", 1,
		"Set Permissions-Policy to disable unused browser features.")
	add("Cross-Origin-Opener-Policy", "headers", h["Cross-Origin-Opener-Policy"] != "", 1,
		"Set COOP to same-origin to isolate browsing context.")
	add("Cross-Origin-Resource-Policy", "headers", h["Cross-Origin-Resource-Policy"] != "", 1,
		"Set CORP to same-origin to prevent cross-origin reads.")

	// 10. CSP quality
	if csp := h["Content-Security-Policy"]; csp != "" {
		quality, issues := analyzeCSP(csp)
		switch quality {
		case "strong":
			checks = append(checks, Pass("CSP Quality", "csp", 3, "Strong CSP, no issues"))
		case "moderate":
			checks = append(checks, Fail("CSP Quality", "csp", 2, SeverityWarning,
				"Moderate CSP: "+strings.Join(issues, "; "),
				"Tighten CSP by removing 'unsafe-inline'/'unsafe-eval' and adding missing directives."))
		default:
			checks = append(checks, Fail("CSP Quality", "csp", 3, SeverityError,
				"Weak CSP: "+strings.Join(issues, "; "),
				"Rebuild CSP from scratch -- multiple critical issues."))
		}
	}

	// 11. SRI for cross-origin scripts/links across ALL pages
	if len(site.Pages) > 0 {
		var thirdPartyTotal, thirdPartyWithSRI int
		for _, p := range site.Pages {
			t, s := analyzeSRI(p)
			thirdPartyTotal += t
			thirdPartyWithSRI += s
		}
		if thirdPartyTotal == 0 {
			checks = append(checks, Pass("Subresource Integrity", "sri", 2, "No third-party scripts to hash"))
		} else {
			ratio := float64(thirdPartyWithSRI) / float64(thirdPartyTotal)
			if ratio >= 1 {
				checks = append(checks, Pass("Subresource Integrity", "sri", 2,
					"All third-party resources have integrity hashes"))
			} else if ratio >= 0.5 {
				checks = append(checks, Fail("Subresource Integrity", "sri", 1, SeverityWarning,
					"Some third-party resources missing SRI",
					"Add integrity= and crossorigin=anonymous to all third-party <script>/<link> tags."))
			} else {
				checks = append(checks, Fail("Subresource Integrity", "sri", 2, SeverityError,
					"Most third-party resources lack SRI",
					"Add integrity hashes to prevent supply-chain compromise."))
			}
		}
	}

	// 12. No mixed content
	checks = append(checks, siteWideCheck("No Mixed Content", "mixed", site, func() CheckResult {
		for _, p := range site.Pages {
			if found := mixedContentRefs(p.Doc); len(found) > 0 {
				r := Fail("No Mixed Content", "mixed", 2, SeverityError,
					fmt.Sprintf("%d subresource(s) loaded over http:// on %s: %s",
						len(found), p.Slug, strings.Join(firstN(found, 3), ", ")),
					"Load every image, script, stylesheet, font, iframe and media file over https:// or a relative URL. Ordinary <a href=\"http://\"> links are NOT mixed content and are not counted here.")
				r.Page = p.Slug
				return r
			}
		}
		return Pass("No Mixed Content", "mixed", 2, "All subresources load over HTTPS or a relative URL")
	}))

	// 13. security.txt
	if site.SecurityTxt != "" {
		checks = append(checks, Pass("security.txt", "well-known", 1, "RFC 9116 contact published"))
	} else {
		checks = append(checks, Fail("security.txt", "well-known", 1, SeverityWarning,
			"No /.well-known/security.txt", "Set security_email in your site profile."))
	}

	// 14. CSP on home page (verify the meta-stripped form is present in HTML)
	if len(site.Pages) > 0 {
		hasMetaCSP := false
		for _, p := range site.Pages {
			if findMetaContent(p.Doc, "Content-Security-Policy") != "" {
				hasMetaCSP = true
				break
			}
		}
		if hasMetaCSP {
			checks = append(checks, Pass("CSP meta tag", "csp", 1, "Meta CSP emitted on at least one page"))
		} else if h["Content-Security-Policy"] != "" {
			checks = append(checks, Fail("CSP meta tag", "csp", 1, SeverityInfo,
				"CSP set in headers but no meta fallback",
				"Static hosts without header support won't apply CSP. Layouts.go should emit a meta tag."))
		}
	}

	// --- New checks ported from upgraded site-inspector (2026-04-27) ---

	hsts := h["Strict-Transport-Security"]
	if hsts != "" {
		if m := hstsMaxAgeRE.FindStringSubmatch(hsts); len(m) == 2 {
			if n, err := strconv.Atoi(m[1]); err == nil && n >= 31536000 {
				checks = append(checks, Pass("HSTS max-age 1y+", "headers", 2,
					"max-age covers 1 year or more"))
			} else {
				checks = append(checks, Fail("HSTS max-age 1y+", "headers", 2, SeverityWarning,
					"max-age is below 31536000 (1 year)",
					"Set Strict-Transport-Security max-age to at least 31536000."))
			}
		} else {
			checks = append(checks, Fail("HSTS max-age 1y+", "headers", 2, SeverityWarning,
				"HSTS header has no parseable max-age",
				"Set Strict-Transport-Security max-age=31536000."))
		}
		if strings.Contains(strings.ToLower(hsts), "includesubdomains") {
			checks = append(checks, Pass("HSTS includeSubDomains", "headers", 1,
				"includeSubDomains directive present"))
		} else {
			checks = append(checks, Fail("HSTS includeSubDomains", "headers", 1, SeverityWarning,
				"includeSubDomains not set",
				"Add the includeSubDomains directive to Strict-Transport-Security."))
		}
		if strings.Contains(strings.ToLower(hsts), "preload") {
			checks = append(checks, Pass("HSTS preload directive", "headers", 1,
				"preload directive present"))
		} else {
			checks = append(checks, Fail("HSTS preload directive", "headers", 1, SeverityInfo,
				"preload directive not set",
				"After confirming max-age >= 1y and includeSubDomains, opt in via Settings -> Security -> HSTS preload to add the preload directive."))
		}
	}

	if csp := h["Content-Security-Policy"]; csp != "" {
		if strings.Contains(strings.ToLower(csp), "upgrade-insecure-requests") {
			checks = append(checks, Pass("CSP upgrade-insecure-requests", "csp", 1,
				"upgrade-insecure-requests directive present"))
		} else {
			checks = append(checks, Fail("CSP upgrade-insecure-requests", "csp", 1, SeverityWarning,
				"upgrade-insecure-requests not in CSP",
				"Add upgrade-insecure-requests to your CSP so legacy http:// embeds are auto-upgraded."))
		}
	}

	checks = append(checks, checkHeadersArtifactParity(site))

	return checks
}

// checkHeadersArtifactParity asserts the computed headers actually
// appear in the EMITTED deploy artifacts (_headers, nginx.conf). The
// category used to grade only the recomputed build config: a bug (or a
// host that ignores _headers) could ship a site serving nothing while
// eval still said A+. Info when no artifact file was emitted (nothing
// to cross-check on this deploy shape).
func checkHeadersArtifactParity(site *SiteContext) CheckResult {
	artifacts := site.HeadersFile + "\n" + site.NginxConf
	if strings.TrimSpace(artifacts) == "" {
		return Info("Headers In Deploy Artifacts", "headers",
			"No _headers or nginx.conf artifact emitted for this build; header intent could not be cross-checked against an artifact.")
	}
	var missing, mismatched []string
	for name, value := range site.Headers {
		if value == "" {
			continue
		}
		switch artifactHeaderState(artifacts, name, value) {
		case headerAbsent:
			missing = append(missing, name)
		case headerWrongValue:
			mismatched = append(mismatched, name)
		}
	}
	if len(missing) > 0 || len(mismatched) > 0 {
		sort.Strings(missing)
		sort.Strings(mismatched)
		var parts []string
		if len(missing) > 0 {
			parts = append(parts, "missing from the emitted _headers/nginx.conf: "+strings.Join(missing, ", "))
		}
		if len(mismatched) > 0 {
			parts = append(parts, "present but carrying a different value than the build computed: "+strings.Join(mismatched, ", "))
		}
		return Fail("Headers In Deploy Artifacts", "headers", 2, SeverityError,
			"computed security headers "+strings.Join(parts, "; "),
			"The build config promises these headers but the deploy artifact does not carry them as computed; rebuild, and if it persists this is a builder bug worth reporting.")
	}
	return Pass("Headers In Deploy Artifacts", "headers", 2,
		"every computed security header appears in the emitted deploy artifacts with the computed value")
}

type artifactHeaderVerdict int

const (
	headerMatches artifactHeaderVerdict = iota
	headerAbsent
	headerWrongValue
)

// artifactHeaderState cross-checks ONE header against the emitted artifacts.
// Comparing names only meant a stale or hand-edited artifact emitting
// X-Frame-Options "ALLOWALL" passed the very check written to catch a deploy
// artifact that disagrees with the build config.
func artifactHeaderState(artifacts, name, value string) artifactHeaderVerdict {
	wantName := strings.ToLower(name)
	wantValue := normalizeHeaderValue(value)
	found := false
	for _, line := range strings.Split(artifacts, "\n") {
		lower := strings.ToLower(line)
		if !strings.Contains(lower, wantName) {
			continue
		}
		found = true
		// nginx escapes embedded double quotes; _headers does not quote at all.
		got := normalizeHeaderValue(strings.ReplaceAll(line, `\"`, `"`))
		if strings.Contains(got, wantValue) {
			return headerMatches
		}
	}
	if !found {
		return headerAbsent
	}
	return headerWrongValue
}

// normalizeHeaderValue collapses whitespace and lowercases so an artifact that
// differs only in spacing or casing still counts as a match.
func normalizeHeaderValue(v string) string {
	return strings.Join(strings.Fields(strings.ToLower(v)), " ")
}

// analyzeCSP returns "strong", "moderate", or "weak" plus list of issues.
// Parses directives so unsafe-eval/unsafe-inline are only flagged when in
// security-sensitive directives (script-src/default-src), not style-src.
func analyzeCSP(csp string) (string, []string) {
	var issues []string
	directives := map[string]string{}
	for _, dir := range splitCSP(csp) {
		parts := strings.SplitN(strings.TrimSpace(dir), " ", 2)
		if len(parts) < 1 {
			continue
		}
		name := strings.ToLower(parts[0])
		val := ""
		if len(parts) == 2 {
			val = strings.ToLower(parts[1])
		}
		directives[name] = val
	}

	scriptSrc := directives["script-src"]
	defaultSrc := directives["default-src"]
	scriptish := scriptSrc
	if scriptish == "" {
		scriptish = defaultSrc
	}

	if strings.Contains(scriptish, "'unsafe-eval'") {
		issues = append(issues, "script-src has unsafe-eval")
	}
	if strings.Contains(scriptish, "'unsafe-inline'") && !strings.Contains(scriptish, "nonce-") {
		issues = append(issues, "script-src has unsafe-inline (no nonce)")
	}
	// Wildcard host in any sensitive directive
	for _, name := range []string{"script-src", "default-src", "connect-src"} {
		v := directives[name]
		if v == "*" || strings.Contains(v, " * ") || strings.HasSuffix(v, " *") || strings.HasPrefix(v, "* ") {
			issues = append(issues, "wildcard host in "+name)
		}
	}

	required := []string{"default-src", "script-src", "frame-ancestors", "base-uri", "object-src", "form-action"}
	for _, r := range required {
		if _, ok := directives[r]; !ok {
			issues = append(issues, "missing "+r)
		}
	}

	switch {
	case len(issues) == 0:
		return "strong", nil
	case len(issues) <= 2:
		return "moderate", issues
	default:
		return "weak", issues
	}
}

func splitCSP(csp string) []string {
	var out []string
	for _, p := range strings.Split(csp, ";") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// analyzeSRI counts third-party (cross-origin) script/link tags and how many have integrity attrs.
func analyzeSRI(page PageContext) (total, withSRI int) {
	for _, s := range elementsByTag(page.Doc, "script") {
		src := attr(s, "src")
		if isThirdPartyURL(src) {
			total++
			if attr(s, "integrity") != "" {
				withSRI++
			}
		}
	}
	for _, l := range elementsByTag(page.Doc, "link") {
		rel := strings.ToLower(attr(l, "rel"))
		if rel != "stylesheet" {
			continue
		}
		href := attr(l, "href")
		if isThirdPartyURL(href) {
			total++
			if attr(l, "integrity") != "" {
				withSRI++
			}
		}
	}
	return total, withSRI
}

func isThirdPartyURL(u string) bool {
	if u == "" {
		return false
	}
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "//")
}

// subresourceAttrs maps an element to the attributes through which it LOADS a
// subresource. A plain <a href="http://..."> is navigation, not a subresource:
// no browser calls it mixed content, and the old substring scan failed the
// check on it and told the tenant to break the link.
var subresourceAttrs = map[string][]string{
	"img":    {"src", "srcset"},
	"script": {"src"},
	"iframe": {"src"},
	"embed":  {"src"},
	"audio":  {"src"},
	"video":  {"src", "poster"},
	"source": {"src", "srcset"},
	"track":  {"src"},
	"object": {"data"},
	"input":  {"src"},
}

// linkRelLoads are the <link> rel values that actually fetch a resource into
// the page. preconnect/dns-prefetch establish a connection but load nothing.
var linkRelLoads = map[string]bool{
	"stylesheet": true, "icon": true, "shortcut icon": true, "apple-touch-icon": true,
	"preload": true, "prefetch": true, "manifest": true, "mask-icon": true,
}

// mixedContentRefs returns every http:// subresource URL the page loads.
func mixedContentRefs(doc *html.Node) []string {
	var out []string
	consider := func(raw string) {
		for _, candidate := range strings.Split(raw, ",") {
			fields := strings.Fields(candidate) // srcset entries carry a descriptor
			if len(fields) == 0 {
				continue
			}
			u := strings.TrimSpace(fields[0])
			if !strings.HasPrefix(strings.ToLower(u), "http://") {
				continue
			}
			host := strings.ToLower(strings.TrimPrefix(u, "http://"))
			if strings.HasPrefix(host, "localhost") || strings.HasPrefix(host, "127.") {
				continue
			}
			out = append(out, u)
		}
	}
	for _, n := range findAll(doc, func(n *html.Node) bool { return n.Type == html.ElementNode }) {
		if n.Data == "link" {
			rels := strings.Fields(strings.ToLower(attr(n, "rel")))
			loads := false
			for _, rel := range rels {
				if linkRelLoads[rel] {
					loads = true
					break
				}
			}
			if loads {
				consider(attr(n, "href"))
			}
			continue
		}
		for _, a := range subresourceAttrs[n.Data] {
			if v := attr(n, a); v != "" {
				consider(v)
			}
		}
	}
	return out
}
