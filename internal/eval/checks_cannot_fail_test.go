package eval

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/bright-interaction/slab/internal/agent"
)

// This file is the regression suite for the "checks that cannot fail" class:
// every case below FAILS against the pre-fix implementation, which is the only
// thing that makes a guard worth keeping.

func page(t *testing.T, slug, raw string) PageContext {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("parse %s: %v", slug, err)
	}
	return PageContext{URL: slug, Slug: slug, HTML: raw, Doc: doc, FileSize: int64(len(raw))}
}

func mustCheck(t *testing.T, checks []CheckResult, name string) CheckResult {
	t.Helper()
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("check %q not emitted; got %d checks", name, len(checks))
	return CheckResult{}
}

func assertFailed(t *testing.T, c CheckResult, why string) {
	t.Helper()
	if c.Passed == nil {
		t.Fatalf("%s: expected a FAIL, got Info (%s): %s", c.Name, why, c.Detail)
	}
	if *c.Passed {
		t.Fatalf("%s: expected a FAIL (%s), got Pass: %s", c.Name, why, c.Detail)
	}
}

func assertPassed(t *testing.T, c CheckResult, why string) {
	t.Helper()
	if c.Passed == nil || !*c.Passed {
		t.Fatalf("%s: expected a Pass (%s), got %+v", c.Name, why, c)
	}
}

func assertInfo(t *testing.T, c CheckResult, why string) {
	t.Helper()
	if c.Passed != nil {
		t.Fatalf("%s: expected Info (%s), got Passed=%v: %s", c.Name, why, *c.Passed, c.Detail)
	}
}

// --- 1. BreadcrumbList on a flat site -------------------------------------

func TestBreadcrumbSchemaFailsOnFlatMarketingSite(t *testing.T) {
	body := `<!DOCTYPE html><html lang="sv"><body><h1>Hej</h1></body></html>`
	site := &SiteContext{Pages: []PageContext{
		page(t, "/", body),
		page(t, "/om-oss", body),
		page(t, "/kontakt", body),
	}}
	c := mustCheck(t, RunGEOChecks(site), "BreadcrumbList Schema")
	assertFailed(t, c, "no page emits BreadcrumbList JSON-LD; only / is exempt")
}

func TestBreadcrumbSchemaExemptsRealLocaleRoot(t *testing.T) {
	body := `<!DOCTYPE html><html lang="sv"><body><h1>Hej</h1></body></html>`
	crumbed := `<!DOCTYPE html><html lang="sv"><body><script type="application/ld+json">
{"@context":"https://schema.org","@type":"BreadcrumbList","itemListElement":[{"@type":"ListItem","position":1,"name":"Hem"},{"@type":"ListItem","position":2,"name":"Om oss"}]}
</script></body></html>`
	site := &SiteContext{Pages: []PageContext{
		page(t, "/", body),
		page(t, "/sv", body),
		page(t, "/sv/om-oss", crumbed),
	}}
	assertPassed(t, mustCheck(t, RunGEOChecks(site), "BreadcrumbList Schema"),
		"/sv is a locale root because /sv/om-oss exists, and that page carries a breadcrumb")
}

func TestIsLocaleRootRejectsOrdinarySlugs(t *testing.T) {
	site := &SiteContext{Pages: []PageContext{
		page(t, "/", "<html></html>"),
		page(t, "/faq", "<html></html>"),
		page(t, "/sv/priser", "<html></html>"),
	}}
	cases := map[string]bool{
		"/":        true,
		"/sv":      true,  // locale-shaped and has children
		"/faq":     false, // three letters but not a locale tag
		"/about":   false,
		"/en":      false, // locale-shaped but the site serves nothing under it
		"/pricing": false,
	}
	for slug, want := range cases {
		if got := isLocaleRoot(site, slug); got != want {
			t.Errorf("isLocaleRoot(%q) = %v, want %v", slug, got, want)
		}
	}
}

// --- 2. Inline tracker snippets -------------------------------------------

const inlineGTM = `<!DOCTYPE html><html><head><script>
(function(w,d,s,l,i){w[l]=w[l]||[];w[l].push({'gtm.start':new Date().getTime(),event:'gtm.js'});
var f=d.getElementsByTagName(s)[0],j=d.createElement(s);j.async=true;
j.src='https://www.googletagmanager.com/gtm.js?id='+i;f.parentNode.insertBefore(j,f);
})(window,document,'script','dataLayer','GTM-ABCDEF');
</script></head><body><h1>Flat</h1></body></html>`

const inlineMetaPixel = `<!DOCTYPE html><html><head><script>
!function(f,b,e,v,n,t,s){n=f.fbq=function(){n.callMethod?n.callMethod.apply(n,arguments):n.queue.push(arguments)};
t.src='https://connect.facebook.net/en_US/fbevents.js';}(window,document,'script');
fbq('init','1234567890');fbq('track','PageView');
</script></head><body><h1>Flat</h1></body></html>`

func TestTrackerCountSeesInlineGTM(t *testing.T) {
	site := &SiteContext{Pages: []PageContext{page(t, "/", inlineGTM)}}
	checks := RunPrivacyChecks(site)
	for _, c := range checks {
		if c.Name == "Tracker Count" && c.Passed != nil && *c.Passed {
			t.Fatalf("inline GTM read as %q; the canonical GTM install is an inline loader", c.Detail)
		}
	}
	within := mustCheck(t, checks, "Tracker Count (within budget)")
	if !strings.Contains(within.Detail, "googletagmanager") {
		t.Errorf("expected googletagmanager in detail, got %q", within.Detail)
	}
}

func TestTrackerCountSeesInlineMetaPixel(t *testing.T) {
	site := &SiteContext{Pages: []PageContext{page(t, "/", inlineMetaPixel)}}
	within := mustCheck(t, RunPrivacyChecks(site), "Tracker Count (within budget)")
	if !strings.Contains(within.Detail, "facebook") {
		t.Errorf("expected the Meta Pixel to be detected, got %q", within.Detail)
	}
}

// --- 3. Pre-consent tracking ----------------------------------------------

func TestPreConsentTrackingFailsWhenBannerLoadsAfterTracker(t *testing.T) {
	raw := `<!DOCTYPE html><html><head>
<script src="https://www.googletagmanager.com/gtag/js?id=G-XYZ"></script>
<script src="https://consent.cookiebot.com/uc.js" data-cbid="x"></script>
</head><body><h1>Hi</h1></body></html>`
	site := &SiteContext{Pages: []PageContext{page(t, "/", raw)}}
	checks := RunPrivacyChecks(site)
	assertPassed(t, mustCheck(t, checks, "Consent Banner"), "cookiebot is present")
	c := mustCheck(t, checks, "Pre-Consent Tracking")
	assertFailed(t, c, "GA4 runs ahead of the consent script")
	if !strings.Contains(c.Detail, "ahead of the consent script") {
		t.Errorf("detail must state what was tested, got %q", c.Detail)
	}
}

func TestPreConsentTrackingPassesWhenTrackerIsGated(t *testing.T) {
	raw := `<!DOCTYPE html><html><head>
<script src="https://consent.cookiebot.com/uc.js" data-cbid="x"></script>
<script type="text/plain" data-cookieconsent="statistics" src="https://www.googletagmanager.com/gtag/js?id=G-XYZ"></script>
</head><body><h1>Hi</h1></body></html>`
	site := &SiteContext{Pages: []PageContext{page(t, "/", raw)}}
	assertPassed(t, mustCheck(t, RunPrivacyChecks(site), "Pre-Consent Tracking"),
		"the tag is type=text/plain behind a CMP category and sits after the CMP")
}

func TestPreConsentTrackingIsInfoWithoutTrackers(t *testing.T) {
	raw := `<!DOCTYPE html><html><body><h1>Hi</h1></body></html>`
	site := &SiteContext{Pages: []PageContext{page(t, "/", raw)}}
	assertInfo(t, mustCheck(t, RunPrivacyChecks(site), "Pre-Consent Tracking"),
		"no trackers means nothing was tested, so no points may be awarded")
}

// --- 3b. Consent evidence must be a LOADED platform -----------------------
//
// Folding inline script text into the text matched against consentPlatforms
// made the checks fabricate compliance: the word "cookiebot" anywhere in a
// script body bought a Consent Banner pass AND a Pre-Consent Tracking pass,
// six points, for a page whose GTM loader is completely ungated. Detecting a
// TRACKER from inline text is sound (the canonical GTM install is an inline
// loader, so the snippet IS the tracker). Detecting a CMP from inline text is
// not, because the snippet is not the CMP; the CMP is a remote bundle. The two
// are not symmetric and must not share one haystack.

// gtmLoaderWithCMPComment is the exact shape that scored 6 fabricated points:
// an ungated inline GTM loader whose only mention of a consent platform is a
// comment saying somebody plans to add one.
const gtmLoaderWithCMPComment = `<!DOCTYPE html><html><head>
<script>
// consent: cookiebot is on the roadmap, ticket AS-4711
/* onetrust was evaluated and rejected */
(function(w,d,s,l,i){w[l]=w[l]||[];w[l].push({'gtm.start':new Date().getTime(),event:'gtm.js'});
var f=d.getElementsByTagName(s)[0],j=d.createElement(s);j.async=true;
j.src='https://www.googletagmanager.com/gtm.js?id='+i;f.parentNode.insertBefore(j,f);
})(window,document,'script','dataLayer','GTM-ABCDEF');
</script>
</head><body><h1>Hi</h1></body></html>`

func TestConsentChecksIgnoreACMPNamedOnlyInScriptText(t *testing.T) {
	site := &SiteContext{Pages: []PageContext{page(t, "/", gtmLoaderWithCMPComment)}}
	checks := RunPrivacyChecks(site)

	assertFailed(t, mustCheck(t, checks, "Consent Banner"),
		"naming a CMP in a comment is not a loaded CMP")

	c := mustCheck(t, checks, "Pre-Consent Tracking")
	assertFailed(t, c, "GTM fires unconditionally and no consent platform is loaded")
	if !strings.Contains(c.Detail, "no consent platform") {
		t.Errorf("detail must state that no CMP was found, got %q", c.Detail)
	}

	// The genuine improvement from the same round must survive: the inline
	// loader is still recognised as a tracker.
	within := mustCheck(t, checks, "Tracker Count (within budget)")
	if !strings.Contains(within.Detail, "googletagmanager") {
		t.Errorf("inline GTM must still be detected as a tracker, got %q", within.Detail)
	}
}

func TestConsentBannerIgnoresACMPInitCallInsideAComment(t *testing.T) {
	raw := `<!DOCTYPE html><html><head><script>
// TODO before launch: call Cookiebot.init() and window.CookieProof.init() here
var ready = true;
</script></head><body><h1>Hi</h1></body></html>`
	site := &SiteContext{Pages: []PageContext{page(t, "/", raw)}}
	assertFailed(t, mustCheck(t, RunPrivacyChecks(site), "Consent Banner"),
		"an init call that only exists inside a comment loads nothing")
}

// TestConsentBannerIsIndeterminateOnAReferenceWithNoLoadEvidence pins the
// third state this check can return. `Cookiebot.renew()` is a genuine call
// shape -- an identifier chain in code position, immediately followed by
// `(` -- and the tokenizer in jslex.go proves it is not sitting inside a
// comment or a string. It is still not proof: nothing on this page's <script>
// tags actually loads cookiebot.com or carries data-consent-platform, so the
// call could just as easily be dead code referencing an undefined global as
// a CMP bootstrapped by JS this crawler cannot see. A GDPR auditor shown
// only this evidence would not sign off; they would ask the tenant to show
// the load. Full credit here would be exactly the kind of fabrication this
// file's history is about, just moved one bar higher, so this scores zero
// with distinct wording rather than either a silent pass or a generic "no
// consent banner" fail.
func TestConsentBannerIsIndeterminateOnAReferenceWithNoLoadEvidence(t *testing.T) {
	raw := `<!DOCTYPE html><html><head><script>
Cookiebot.renew();
</script></head><body><h1>Hi</h1></body></html>`
	site := &SiteContext{Pages: []PageContext{page(t, "/", raw)}}
	c := mustCheck(t, RunPrivacyChecks(site), "Consent Banner")
	assertFailed(t, c, "a call with no matching load anywhere on the page is unresolved, not proof")
	if !strings.Contains(c.Detail, "cookiebot") {
		t.Errorf("detail must name which platform was only referenced, got %q", c.Detail)
	}
	if !strings.Contains(c.Recommendation, "Verify manually") {
		t.Errorf("recommendation must tell the tenant to verify by hand, got %q", c.Recommendation)
	}
}

func TestPreConsentTrackingFailsWhenOneBlobLoadsBothCMPAndTracker(t *testing.T) {
	// The second face of the same defect: excusing any script that merely
	// mentioned a CMP made the single worst misconfiguration -- CMP
	// reference and ungated tracker bootstrapped in one blob -- the one
	// shape the check could never report. Cookiebot.renew() is a real call
	// shape (see the Indeterminate test above for why that still is not
	// full consent credit); the point here is narrower: whatever tier of
	// consent evidence a script carries, it does not excuse a tracker
	// sitting right beside it in the same tag, because nothing about
	// naming a CMP makes the JS engine hold back the rest of that same
	// <script> body until opt-in.
	raw := `<!DOCTYPE html><html><head><script>
Cookiebot.renew();
(function(w,d,s,l,i){w[l]=w[l]||[];w[l].push({'gtm.start':new Date().getTime(),event:'gtm.js'});
var f=d.getElementsByTagName(s)[0],j=d.createElement(s);j.async=true;
j.src='https://www.googletagmanager.com/gtm.js?id='+i;f.parentNode.insertBefore(j,f);
})(window,document,'script','dataLayer','GTM-ABCDEF');
</script></head><body><h1>Hi</h1></body></html>`
	site := &SiteContext{Pages: []PageContext{page(t, "/", raw)}}
	checks := RunPrivacyChecks(site)
	// No script anywhere loads cookiebot.com or googletagmanager.com by
	// domain, and the only consent evidence is the weak call reference, so
	// Consent Banner must NOT pass either.
	assertFailed(t, mustCheck(t, checks, "Consent Banner"), "only weak reference evidence exists; nothing is actually loaded")
	c := mustCheck(t, checks, "Pre-Consent Tracking")
	assertFailed(t, c, "the GTM loader in that same blob is not gated by the CMP reference beside it")
	if !strings.Contains(c.Detail, "same script tag") {
		t.Errorf("detail must name the shared-tag misconfiguration, got %q", c.Detail)
	}
}

// TestPreConsentTrackingFailsWhenConsentLoadsEarlierButDoesNotGate pins the
// exact bypass a verifier reproduced against the previous remediation: put
// ANY script carrying consent evidence earlier in document order than an
// ungated tracker, and the old "s.order > consentAt" branch fell through
// every switch case with no violation reported. Document order between the
// consent node and the tracker node is not, by itself, proof of gating --
// only isConsentGated (type="text/plain" plus a CMP category attribute) is.
func TestPreConsentTrackingFailsWhenConsentLoadsEarlierButDoesNotGate(t *testing.T) {
	raw := `<!DOCTYPE html><html><head>
<script src="https://consent.cookiebot.com/uc.js" data-cbid="x"></script>
<script src="https://www.googletagmanager.com/gtag/js?id=G-XYZ"></script>
</head><body><h1>Hi</h1></body></html>`
	site := &SiteContext{Pages: []PageContext{page(t, "/", raw)}}
	checks := RunPrivacyChecks(site)
	assertPassed(t, mustCheck(t, checks, "Consent Banner"), "cookiebot really is loaded here, by src=")
	c := mustCheck(t, checks, "Pre-Consent Tracking")
	assertFailed(t, c, "the CMP loading first does not stop the very next tag from firing unconditionally")
	if !strings.Contains(c.Detail, "does not gate this tag") {
		t.Errorf("detail must state that loading earlier is not the same as gating, got %q", c.Detail)
	}
}

// --- 4. CWV query error ---------------------------------------------------

func TestRunCWVChecksEmitsInfoOnQueryError(t *testing.T) {
	sqlDB, queries := openTestDB(t)
	siteID := "abcdef0123456789abcdef07"
	seedSite(t, sqlDB, siteID)
	if _, err := sqlDB.Exec(`DROP TABLE cwv_events`); err != nil {
		t.Fatalf("drop cwv_events: %v", err)
	}

	checks := RunCWVChecks(context.Background(), queries, siteID, agent.FidelityBalanced)
	if len(checks) != len(cwvMetricOrder) {
		t.Fatalf("a query error must not delete the measured-CWV checks: got %d, want %d", len(checks), len(cwvMetricOrder))
	}
	for _, c := range checks {
		assertInfo(t, c, "a failed measurement query grades nothing")
		if !strings.Contains(c.Detail, "query failed") {
			t.Errorf("%s: detail should name the cause, got %q", c.Name, c.Detail)
		}
	}
}

// --- 5. BuildSecurityHeaders error ----------------------------------------

func TestLoadSiteContextFailsWhenHeadersCannotBeComputed(t *testing.T) {
	sqlDB, queries := openTestDB(t)
	siteID := "abcdef0123456789abcdef08"
	seedSite(t, sqlDB, siteID)
	dist := t.TempDir()
	if err := os.WriteFile(filepath.Join(dist, "index.html"), []byte("<html></html>"), 0o600); err != nil {
		t.Fatalf("write fixture page: %v", err)
	}
	if _, err := sqlDB.Exec(`DROP TABLE site_settings`); err != nil {
		t.Fatalf("drop site_settings: %v", err)
	}

	site, err := LoadSiteContext(context.Background(), queries, siteID, dist)
	if err == nil {
		t.Fatalf("expected an error; instead got a context with %d headers, which grades every header check as missing and fabricates a security F", len(site.Headers))
	}
}

// --- 6. eval.Run persistence ----------------------------------------------

func TestRunFailsWhenNothingCouldBePersisted(t *testing.T) {
	sqlDB, queries := openTestDB(t)
	siteID := "abcdef0123456789abcdef09"
	seedSite(t, sqlDB, siteID)
	dist := t.TempDir()
	if err := os.WriteFile(filepath.Join(dist, "index.html"), []byte("<html><body><h1>x</h1></body></html>"), 0o600); err != nil {
		t.Fatalf("write fixture page: %v", err)
	}
	// The DELETE still succeeds, so this exercises the persist path: the run
	// wipes the previous grades and stores none of the new ones.
	if _, err := sqlDB.Exec(`CREATE TRIGGER no_eval_insert BEFORE INSERT ON evaluations BEGIN SELECT RAISE(ABORT, 'persist blocked'); END;`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	if _, err := Run(context.Background(), queries, siteID, "build_persist_fail", dist); err == nil {
		t.Fatal("Run reported success after persisting nothing; the publish gate reads this error and would ship an ungraded build")
	}

	var n int
	if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM evaluations WHERE build_id = ?`, "build_persist_fail").Scan(&n); err != nil {
		t.Fatalf("count evaluations: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 persisted rows in this scenario, got %d", n)
	}
}

func TestRunFailsWhenPreviousRowsCannotBeCleared(t *testing.T) {
	sqlDB, queries := openTestDB(t)
	siteID := "abcdef0123456789abcdef10"
	seedSite(t, sqlDB, siteID)
	dist := t.TempDir()
	if err := os.WriteFile(filepath.Join(dist, "index.html"), []byte("<html></html>"), 0o600); err != nil {
		t.Fatalf("write fixture page: %v", err)
	}
	if _, err := sqlDB.Exec(`DROP TABLE evaluations`); err != nil {
		t.Fatalf("drop evaluations: %v", err)
	}
	if _, err := Run(context.Background(), queries, siteID, "build_delete_fail", dist); err == nil {
		t.Fatal("Run reported success although the idempotency delete failed")
	}
}

// --- 7. Focus indicators --------------------------------------------------

func TestFocusIndicatorRejectsOutlineNoneAndClassStrings(t *testing.T) {
	cases := []struct {
		name string
		css  string
		want bool
	}{
		{"outline none only", `a:focus { outline: none; }`, false},
		{"outline zero only", `a:focus-visible { outline: 0 }`, false},
		{"real outline", `a:focus-visible { outline: 2px solid var(--color-primary); outline-offset: 2px; }`, true},
		{"box shadow ring", `.btn:focus { box-shadow: 0 0 0 3px #09f; }`, true},
		{"inside media query", `@media (min-width: 40rem) { .btn:focus { outline: 2px solid red; } }`, true},
		{"suppressed then restored", `input:focus:not(:focus-visible){outline:none}input:focus-visible{outline:2px solid blue}`, true},
		{"no focus rule at all", `.btn { color: red }`, false},
	}
	for _, c := range cases {
		if got := hasFocusIndicator(c.css); got != c.want {
			t.Errorf("%s: hasFocusIndicator(%q) = %v, want %v", c.name, c.css, got, c.want)
		}
	}
}

func TestFocusIndicatorsFailOnTailwindClassStringAlone(t *testing.T) {
	raw := `<!DOCTYPE html><html><body><button class="md:focus:ring-2 focus:outline-none">Go</button></body></html>`
	site := &SiteContext{Pages: []PageContext{page(t, "/", raw)}}
	c := mustCheck(t, RunAccessibilityChecks(site), "Focus Indicators")
	assertFailed(t, c, "a class attribute is not CSS and no stylesheet declares a focus rule")
}

func TestFocusIndicatorsPassOnRealInlineCSS(t *testing.T) {
	raw := `<!DOCTYPE html><html><head><style>a:focus-visible{outline:2px solid #09f;outline-offset:2px}</style></head><body><a href="/x">x</a></body></html>`
	site := &SiteContext{Pages: []PageContext{page(t, "/", raw)}}
	assertPassed(t, mustCheck(t, RunAccessibilityChecks(site), "Focus Indicators"), "a real focus ring is declared inline")
}

// --- 8. Header values in deploy artifacts ---------------------------------

func TestHeadersArtifactParityComparesValuesNotJustNames(t *testing.T) {
	site := &SiteContext{
		Pages:       []PageContext{page(t, "/", "<html></html>")},
		Headers:     map[string]string{"X-Frame-Options": "DENY"},
		HeadersFile: "/*\n  X-Frame-Options: ALLOWALL\n",
	}
	c := checkHeadersArtifactParity(site)
	assertFailed(t, c, `the artifact emits X-Frame-Options "ALLOWALL" while the build computed DENY`)
	if !strings.Contains(c.Detail, "X-Frame-Options") {
		t.Errorf("detail should name the header, got %q", c.Detail)
	}
}

func TestHeadersArtifactParityPassesOnMatchingNginxArtifact(t *testing.T) {
	site := &SiteContext{
		Pages:     []PageContext{page(t, "/", "<html></html>")},
		Headers:   map[string]string{"X-Frame-Options": "DENY", "Content-Security-Policy": "default-src 'self'"},
		NginxConf: "add_header X-Frame-Options \"DENY\" always;\nadd_header Content-Security-Policy \"default-src 'self'\" always;\n",
	}
	assertPassed(t, checkHeadersArtifactParity(site), "both values match the computed headers")
}

// --- 9. Mixed content -----------------------------------------------------

func TestNoMixedContentIgnoresPlainAnchorLinks(t *testing.T) {
	raw := `<!DOCTYPE html><html><body><a href="http://old-partner.example/report">Partner report</a></body></html>`
	site := &SiteContext{Pages: []PageContext{page(t, "/", raw)}}
	assertPassed(t, mustCheck(t, RunSecurityChecks(site), "No Mixed Content"),
		"an <a href=http://> link is navigation, not a subresource")
}

func TestNoMixedContentStillCatchesInsecureSubresources(t *testing.T) {
	for _, raw := range []string{
		`<html><body><img src="http://cdn.example/a.png"></body></html>`,
		`<html><head><script src="http://cdn.example/a.js"></script></head><body></body></html>`,
		`<html><head><link rel="stylesheet" href="http://cdn.example/a.css"></head><body></body></html>`,
		`<html><body><iframe src="http://cdn.example/frame"></iframe></body></html>`,
	} {
		site := &SiteContext{Pages: []PageContext{page(t, "/", raw)}}
		assertFailed(t, mustCheck(t, RunSecurityChecks(site), "No Mixed Content"), raw)
	}
}

// --- 10. robots.txt wildcard ----------------------------------------------

func TestAIBotsBlockedByWildcardRecord(t *testing.T) {
	site := &SiteContext{
		Pages:     []PageContext{page(t, "/", "<html></html>")},
		RobotsTxt: "User-agent: *\nDisallow: /\n",
	}
	c := mustCheck(t, RunPrivacyChecks(site), "AI Training Bots Blocked")
	assertPassed(t, c, "a wildcard Disallow: / blocks every crawler, AI bots included")
	if !strings.Contains(c.Detail, "wildcard") {
		t.Errorf("detail should say the block comes from the wildcard record, got %q", c.Detail)
	}
}

func TestAIBotWithItsOwnAllowRecordOverridesWildcard(t *testing.T) {
	robots := "User-agent: *\nDisallow: /\n\n" +
		"User-agent: GPTBot\nAllow: /\n\n" +
		"User-agent: ClaudeBot\nAllow: /\n\n" +
		"User-agent: CCBot\nAllow: /\n\n" +
		"User-agent: PerplexityBot\nAllow: /\n\n" +
		"User-agent: Google-Extended\nAllow: /\n\n" +
		"User-agent: Bytespider\nAllow: /\n\n" +
		"User-agent: Amazonbot\nAllow: /\n\n" +
		"User-agent: YouBot\nAllow: /\n\n" +
		"User-agent: anthropic-ai\nAllow: /\n\n" +
		"User-agent: Claude-Web\nAllow: /\n\n" +
		"User-agent: ChatGPT-User\nAllow: /\n\n" +
		"User-agent: OAI-SearchBot\nAllow: /\n\n" +
		"User-agent: Applebot-Extended\nAllow: /\n\n" +
		"User-agent: FacebookBot\nAllow: /\n\n" +
		"User-agent: DuckAssistBot\nAllow: /\n"
	site := &SiteContext{
		Pages:     []PageContext{page(t, "/", "<html></html>")},
		RobotsTxt: robots,
	}
	c := mustCheck(t, RunPrivacyChecks(site), "AI Training Bots Blocked")
	assertFailed(t, c, "every AI bot has its own record that does NOT disallow, so the wildcard does not apply to them")
}

// --- 11. Form labels ------------------------------------------------------

func TestFormLabelsAcceptImplicitLabel(t *testing.T) {
	raw := `<!DOCTYPE html><html><body><form><label>Email <input type="email" name="email"></label></form></body></html>`
	site := &SiteContext{Pages: []PageContext{page(t, "/", raw)}}
	assertPassed(t, mustCheck(t, RunAccessibilityChecks(site), "Form Inputs Have Labels"),
		"a wrapping <label> with text is a valid accessible name")
}

func TestFormLabelsExamineTextareaAndSelect(t *testing.T) {
	cases := map[string]string{
		"textarea": `<html><body><form><textarea name="msg"></textarea></form></body></html>`,
		"select":   `<html><body><form><select name="topic"><option>a</option></select></form></body></html>`,
	}
	for name, raw := range cases {
		site := &SiteContext{Pages: []PageContext{page(t, "/", raw)}}
		c := mustCheck(t, RunAccessibilityChecks(site), "Form Inputs Have Labels")
		assertFailed(t, c, "an unlabelled <"+name+"> is as inaccessible as an unlabelled <input>")
		if !strings.Contains(c.Detail, name) {
			t.Errorf("%s: detail should name the element, got %q", name, c.Detail)
		}
	}
}

func TestFormLabelsRejectEmptyWrappingLabel(t *testing.T) {
	raw := `<html><body><form><label><input type="email" name="email"></label></form></body></html>`
	site := &SiteContext{Pages: []PageContext{page(t, "/", raw)}}
	assertFailed(t, mustCheck(t, RunAccessibilityChecks(site), "Form Inputs Have Labels"),
		"a wrapping label with no text names nothing")
}

// --- 12. Site-wide checks on an empty dist/ -------------------------------

func TestSiteWideChecksAreInfoOnEmptySite(t *testing.T) {
	site := &SiteContext{Headers: map[string]string{}}
	all := append(RunSecurityChecks(site), RunPrivacyChecks(site)...)
	all = append(all, RunAccessibilityChecks(site)...)
	for _, name := range []string{
		"No Mixed Content",
		"Tracker Count",
		"Pre-Consent Tracking",
		"Cookie Count",
		"Consent Banner",
		"Privacy Policy Link",
		"Terms / Cookie Policy Link",
		"Focus Indicators",
	} {
		assertInfo(t, mustCheck(t, all, name), "a build with zero pages has no markup to grade")
	}
}

// Tokenizer-level regression coverage lives in jslex_test.go and the
// adversarial corpus in privacy_corpus_test.go now that stripScriptComments
// has been replaced by tokenizeJS: a hand-rolled comment stripper cannot be
// fixed by stripping more shapes of comment, only a real lexer can, and
// jslex_test.go pins that lexer's token boundaries directly.
