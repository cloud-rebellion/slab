package perfectfoundation

import (
	"sort"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/bright-interaction/slab/internal/eval"
)

// fixtureCannotTrigger lists CheckOwnership entries the fixture corpus
// cannot reasonably emit as SCORED checks, each with the reason. Every
// other ownership entry MUST be emitted by the corpus, and every
// scored emission MUST be owned; both directions fail the build.
// An entry here is a hole in the coverage, not a pass: when a page
// shape exists that would emit the check, write that fixture instead.
// Measured CWV checks are excluded by policy: they need real-user
// beacon rows in the DB and are graded, not enforced.
var fixtureCannotTrigger = map[string]string{
	"Collection JSON-LD present":                         "needs rendered collection-item markup from the real builder",
	"Collection hreflang alternates":                     "needs rendered collection-item markup from the real builder",
	"Collection internal links":                          "needs rendered collection-index markup from the real builder",
	"Hreflang Tags":                                      "fixture is single-language by design (multi-language detection is tested in eval)",
	"Hreflang Self-Reference":                            "fixture is single-language by design",
	"Hreflang x-default":                                 "fixture is single-language by design",
	"Organization Schema Completeness (core)":            "partial-credit split variant; the full-pass name is emitted instead",
	"Organization Schema Completeness (entity depth)":    "partial-credit split variant; the full-pass name is emitted instead",
	"Noindex Pages Excluded From Sitemap (mostly clean)": "partial-credit split variant; the clean-pass name is emitted instead",
	"Noindex Pages Excluded From Sitemap (leaks)":        "partial-credit split variant; the clean-pass name is emitted instead",
}

func mustParse(t *testing.T, raw string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("parse fixture html: %v", err)
	}
	return doc
}

// fixtureSite is a rich static site that triggers every conditional
// emission branch the ownership matrix covers: full security headers
// (HSTS with max-age+includeSubDomains+preload, CSP with
// upgrade-insecure-requests), robots/sitemap/llms/security.txt/_headers
// artifacts, pages with meta/OG/canonical/JSON-LD, images, forms,
// links, a cross-origin script with SRI, and a noindex page.
func fixtureSite(t *testing.T) *eval.SiteContext {
	t.Helper()
	page := `<!DOCTYPE html><html lang="en"><head>
<title>Fixture page title that is long enough here</title>
<meta name="description" content="A fixture meta description with a concrete number 42 and enough characters to satisfy the length window comfortably, try it today." />
<link rel="canonical" href="https://example.com/" />
<meta property="og:title" content="Fixture" />
<meta property="og:description" content="Fixture og description" />
<meta property="og:image" content="https://example.com/media/og/original.png" />
<meta name="twitter:card" content="summary_large_image" />
<link rel="icon" href="/favicon.ico" /><link rel="apple-touch-icon" href="/apple-touch-icon.png" />
<link rel="preconnect" href="https://example.com" />
<script type="application/ld+json">{"@context":"https://schema.org","@type":"Organization","name":"Fixture AB","url":"https://example.com","logo":"https://example.com/logo.png","sameAs":["https://linkedin.com/company/fixture"],"address":{"@type":"PostalAddress","addressLocality":"Stockholm"}}</script>
<script type="application/ld+json">{"@context":"https://schema.org","@type":"FAQPage","mainEntity":[{"@type":"Question","name":"What is this?","acceptedAnswer":{"@type":"Answer","text":"A fixture with 42 details."}}]}</script>
<script src="https://cdn.example.net/lib.js" integrity="sha384-abc" crossorigin="anonymous" defer></script>
</head><body>
<a class="skip-link" href="#main">Skip to content</a>
<header><nav aria-label="Main"><ul><li><a href="/about">About the fixture</a></li></ul></nav></header>
<main id="main"><h1>Fixture heading</h1><h2>Section one</h2>
<p>` + strings.Repeat("Real words with substance and specifics for the content length check. ", 30) + `</p>
<ul><li>First</li><li>Second</li><li>Third</li></ul>
<p>What is this? A fixture with 42 details.</p>
<img src="/media/a.webp" width="640" height="480" alt="A descriptive fixture image" loading="eager" />
<img src="/media/b.webp" width="640" height="480" alt="Another descriptive image" loading="lazy" />
<form><label for="e">Email</label><input id="e" type="email" required /><button type="submit">Send the form</button></form>
</main>
<footer><a href="/privacy">Privacy policy</a> <a href="/terms">Terms of service</a></footer>
</body></html>`
	noindex := strings.Replace(page, "<title>", `<meta name="robots" content="noindex" /><title>`, 1)

	headers := map[string]string{
		"Strict-Transport-Security":    "max-age=63072000; includeSubDomains; preload",
		"Content-Security-Policy":      "default-src 'self'; script-src 'self'; upgrade-insecure-requests",
		"X-Frame-Options":              "DENY",
		"X-Content-Type-Options":       "nosniff",
		"Referrer-Policy":              "strict-origin-when-cross-origin",
		"Permissions-Policy":           "camera=(), microphone=()",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
	}
	var artifact strings.Builder
	artifact.WriteString("/*\n")
	for name, value := range headers {
		artifact.WriteString("  " + name + ": " + value + "\n")
	}

	return &eval.SiteContext{
		Pages: []eval.PageContext{
			{URL: "/index.html", Slug: "/", HTML: page, Doc: mustParse(t, page), Headers: headers, FileSize: int64(len(page))},
			{URL: "/about/index.html", Slug: "/about", HTML: page, Doc: mustParse(t, page), Headers: headers, FileSize: int64(len(page))},
			{URL: "/internal/index.html", Slug: "/internal", HTML: noindex, Doc: mustParse(t, noindex), Headers: headers, FileSize: int64(len(noindex))},
		},
		RobotsTxt:   "User-agent: *\nAllow: /\nSitemap: https://example.com/sitemap.xml\nUser-agent: GPTBot\nDisallow: /",
		SitemapXML:  `<?xml version="1.0"?><urlset><url><loc>https://example.com/</loc></url><url><loc>https://example.com/about</loc></url></urlset>`,
		LLMsTxt:     "# Fixture\nAbout the fixture site.",
		SecurityTxt: "Contact: mailto:security@example.com\nExpires: 2030-01-01T00:00:00Z",
		HeadersFile: artifact.String(),
		Headers:     headers,
	}
}

// consentFixtureSite is the second fixture in the corpus. The rich fixture
// above is tracker-free by design, which meant three privacy checks had no
// page shape that could emit them and were waved through with
// fixtureCannotTrigger reasons instead. An exemption is not coverage: the
// consent path is exactly where a check that cannot fail costs a tenant a GDPR
// fine, so it gets a real fixture.
//
// This is the compliant installation: Cookiebot loads first, and the canonical
// inline GTM loader below it is held back with type="text/plain" plus a CMP
// category, so Consent Banner passes, Pre-Consent Tracking passes on something
// it actually tested, and Tracker Count emits its partial-credit split.
func consentFixtureSite(t *testing.T) *eval.SiteContext {
	t.Helper()
	page := `<!DOCTYPE html><html lang="en"><head>
<title>Consent fixture page with a long enough title</title>
<meta name="description" content="A consent fixture description with a concrete number 42 and enough characters to satisfy the length window comfortably, try it today." />
<link rel="canonical" href="https://example.com/" />
<script id="Cookiebot" src="https://consent.cookiebot.com/uc.js" data-cbid="fixture-cbid" data-blockingmode="auto"></script>
<script type="text/plain" data-cookieconsent="statistics">
(function(w,d,s,l,i){w[l]=w[l]||[];w[l].push({'gtm.start':new Date().getTime(),event:'gtm.js'});
var f=d.getElementsByTagName(s)[0],j=d.createElement(s);j.async=true;
j.src='https://www.googletagmanager.com/gtm.js?id='+i;f.parentNode.insertBefore(j,f);
})(window,document,'script','dataLayer','GTM-FIXTURE');
</script>
</head><body>
<main id="main"><h1>Consent fixture heading</h1>
<p>` + strings.Repeat("Real words with substance and specifics for the content length check. ", 30) + `</p>
</main>
<footer><a href="/privacy">Privacy policy</a> <a href="/terms">Terms of service</a></footer>
</body></html>`

	return &eval.SiteContext{
		Pages: []eval.PageContext{
			{URL: "/index.html", Slug: "/", HTML: page, Doc: mustParse(t, page), FileSize: int64(len(page))},
		},
		RobotsTxt: "User-agent: *\nAllow: /\nUser-agent: GPTBot\nDisallow: /",
	}
}

// TestConsentFixtureIsTheCompliantInstall pins what consentFixtureSite is FOR.
// Without it the fixture could drift into a non-compliant shape and still
// satisfy the ownership diff below, which only cares that names are emitted.
func TestConsentFixtureIsTheCompliantInstall(t *testing.T) {
	checks := eval.RunPrivacyChecks(consentFixtureSite(t))
	want := map[string]bool{
		"Consent Banner":                    true,
		"Pre-Consent Tracking":              true,
		"Tracker Count (within budget)":     true,
		"Tracker Count (zero-tracker goal)": false,
	}
	seen := map[string]bool{}
	for _, c := range checks {
		pass, ok := want[c.Name]
		if !ok {
			continue
		}
		seen[c.Name] = true
		if c.Passed == nil {
			t.Errorf("%s: expected a scored result, got Info: %s", c.Name, c.Detail)
			continue
		}
		if *c.Passed != pass {
			t.Errorf("%s: Passed=%v, want %v: %s", c.Name, *c.Passed, pass, c.Detail)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("consent fixture never emitted %q", name)
		}
	}
}

// TestOwnershipMatchesEmittedChecks is the two-way drift gate the old
// TestEveryCheckHasOwner could not be: it runs the REAL eval check
// functions against a rich fixture and diffs the emitted scored names
// against CheckOwnership in both directions. An eval check added
// without an owner fails; an ownership entry no eval emits (phantom)
// fails too.
func TestOwnershipMatchesEmittedChecks(t *testing.T) {
	// The corpus is the union of the fixtures: some checks are mutually
	// exclusive on one page (a site cannot be both tracker-free and
	// tracker-gated), and the honest way to cover both branches is a second
	// fixture, not an exemption entry saying the branch is untestable.
	var all []eval.CheckResult
	for _, site := range []*eval.SiteContext{fixtureSite(t), consentFixtureSite(t)} {
		all = append(all, eval.RunSecurityChecks(site)...)
		all = append(all, eval.RunSEOChecks(site)...)
		all = append(all, eval.RunPerformanceChecks(site)...)
		all = append(all, eval.RunAccessibilityChecks(site)...)
		all = append(all, eval.RunPrivacyChecks(site)...)
	}

	emitted := map[string]bool{}
	for _, c := range all {
		if c.Passed != nil { // scored checks only; Info carries no points
			emitted[c.Name] = true
		}
	}

	owned := map[string]bool{}
	for _, o := range CheckOwnership {
		owned[o.Name] = true
	}

	var unowned []string
	for name := range emitted {
		if !owned[name] {
			unowned = append(unowned, name)
		}
	}
	sort.Strings(unowned)
	for _, name := range unowned {
		t.Errorf("eval emits scored check %q with no CheckOwnership entry; assign bake/block/teach + owner", name)
	}

	var phantom []string
	for name := range owned {
		if !emitted[name] && fixtureCannotTrigger[name] == "" {
			phantom = append(phantom, name)
		}
	}
	sort.Strings(phantom)
	for _, name := range phantom {
		t.Errorf("CheckOwnership entry %q is never emitted by eval against the fixture (phantom or renamed check); update the matrix or fixtureCannotTrigger with a reason", name)
	}
}
