package eval

import (
	"strings"
	"testing"
)

// This is the adversarial corpus for privacy.go's consent/tracker detection,
// built BEFORE the redesign it verifies (see the design note in privacy.go
// and jslex.go). It is a committed regression suite, not a one-off probe:
// every case here is run against the CURRENT implementation by this file,
// and separately (see privacy_corpus_history_test.go) against the three
// prior remediations checked out from git, to show which ones each patch
// got wrong.
//
// Each case's justification is what a GDPR/ePrivacy auditor would say about
// the page shape, not what makes the code pass. The two checks that matter
// are Consent Banner (does a real consent platform load?) and Pre-Consent
// Tracking (does any tracker fire before consent is obtained?), because
// those are the two claims a tenant will act on.

type corpusCase struct {
	name    string
	html    string
	justify string
	verify  func(t *testing.T, checks []CheckResult)
}

func consentBannerDetail(checks []CheckResult) (passed bool, detail, rec string) {
	for _, c := range checks {
		if c.Name == "Consent Banner" {
			return c.Passed != nil && *c.Passed, c.Detail, c.Recommendation
		}
	}
	return false, "", ""
}

func trackerDetected(checks []CheckResult, substr string) bool {
	for _, c := range checks {
		if strings.HasPrefix(c.Name, "Tracker Count") && strings.Contains(strings.ToLower(c.Detail), substr) {
			return true
		}
	}
	return false
}

func noTrackerDetectedAtAll(checks []CheckResult) bool {
	for _, c := range checks {
		if c.Name == "Tracker Count" && c.Passed != nil && *c.Passed {
			return true
		}
	}
	return false
}

func preConsentDetail(checks []CheckResult) (passed *bool, detail string) {
	for _, c := range checks {
		if c.Name == "Pre-Consent Tracking" {
			return c.Passed, c.Detail
		}
	}
	return nil, ""
}

var privacyCorpus = []corpusCase{
	// --- A. CMP named where it cannot be evidence of a load -----------------
	{
		name: "cmp_named_only_in_html_comment",
		html: `<!DOCTYPE html><html><body><!-- consent: we should add cookiebot here --><h1>Hi</h1></body></html>`,
		justify: "An auditor reading the rendered page sees nothing: an HTML comment never reaches the browser's " +
			"parse of executable content. No consent platform is loaded.",
		verify: func(t *testing.T, checks []CheckResult) {
			passed, _, _ := consentBannerDetail(checks)
			if passed {
				t.Fatal("an HTML comment must not credit a consent platform")
			}
		},
	},
	{
		name: "cmp_named_only_in_js_line_comment",
		html: `<!DOCTYPE html><html><head><script>
// TODO before launch: call Cookiebot.init() and window.CookieProof.init() here
var ready = true;
</script></head><body><h1>Hi</h1></body></html>`,
		justify: "A `//` line comment never executes. The developer's note-to-self about a future integration is not " +
			"the integration; crediting it would tell the tenant they are compliant for work they have not done yet.",
		verify: func(t *testing.T, checks []CheckResult) {
			passed, _, _ := consentBannerDetail(checks)
			if passed {
				t.Fatal("a JS line comment must not credit a consent platform")
			}
		},
	},
	{
		name: "cmp_named_only_in_js_block_comment",
		html: `<!DOCTYPE html><html><head><script>
/* consent: cookiebot is on the roadmap, ticket AS-4711
   onetrust was evaluated and rejected */
var ready = true;
</script></head><body><h1>Hi</h1></body></html>`,
		justify: "A `/* */` block comment never executes, same as the line-comment case. A roadmap note is evidence " +
			"of a plan, not evidence of a running consent platform.",
		verify: func(t *testing.T, checks []CheckResult) {
			passed, _, _ := consentBannerDetail(checks)
			if passed {
				t.Fatal("a JS block comment must not credit a consent platform")
			}
		},
	},
	{
		name: "cmp_named_only_in_string_literal",
		html: `<!DOCTYPE html><html><head><script>
var placeholder = "cookiebot.init(";
console.log(placeholder);
</script></head><body><h1>Hi</h1></body></html>`,
		justify: "A string literal is data, not a call. `\"cookiebot.init(\"` is never parsed as an expression; it is " +
			"a sequence of characters logged to the console. Nothing invokes Cookiebot.",
		verify: func(t *testing.T, checks []CheckResult) {
			passed, _, _ := consentBannerDetail(checks)
			if passed {
				t.Fatal("a string literal must not credit a consent platform")
			}
		},
	},
	{
		name: "cmp_named_only_in_variable_name",
		html: `<!DOCTYPE html><html><head><script>
var cookiebotEnabledFlag = false;
if (cookiebotEnabledFlag) { console.log('would enable'); }
</script></head><body><h1>Hi</h1></body></html>`,
		justify: "A variable that happens to share a CMP's name is not the CMP's own object; here it is even false. " +
			"An auditor asked \"is Cookiebot loaded\" would answer no after reading this exact snippet.",
		verify: func(t *testing.T, checks []CheckResult) {
			passed, _, _ := consentBannerDetail(checks)
			if passed {
				t.Fatal("a same-named local variable must not credit a consent platform")
			}
		},
	},
	{
		name: "cmp_named_inside_disabled_block",
		html: `<!DOCTYPE html><html><head><script>
/*
if (false) {
  Cookiebot.init();
  window.CookieProof.init();
}
*/
var ready = true;
</script></head><body><h1>Hi</h1></body></html>`,
		justify: "The entire block is commented out, so even the `if (false)` guard is redundant: none of this text " +
			"is parsed as JavaScript at all. Dead, disabled code proves nothing about what the live page does.",
		verify: func(t *testing.T, checks []CheckResult) {
			passed, _, _ := consentBannerDetail(checks)
			if passed {
				t.Fatal("a commented-out block must not credit a consent platform")
			}
		},
	},

	// --- B. Canonical inline trackers: the original P2 finding --------------
	{
		name: "canonical_inline_gtm_detected_as_tracker",
		html: inlineGTM,
		justify: "This IS the Google Tag Manager install: the inline IIFE constructs a <script> element, sets its " +
			"src to googletagmanager.com, and inserts it. A crawler that only reads static src= attributes never sees " +
			"this network request happen, but a real visitor's browser makes it unconditionally.",
		verify: func(t *testing.T, checks []CheckResult) {
			if !trackerDetected(checks, "googletagmanager") {
				t.Fatal("canonical inline GTM must be detected as a tracker")
			}
		},
	},
	{
		name: "canonical_inline_meta_pixel_detected_as_tracker",
		html: inlineMetaPixel,
		justify: "This IS the Meta Pixel install: fbq() is defined and immediately called, and the loader assigns " +
			"connect.facebook.net to a script's src. The pixel fires on page load regardless of any static src= " +
			"attribute existing anywhere in the HTML.",
		verify: func(t *testing.T, checks []CheckResult) {
			if !trackerDetected(checks, "facebook") {
				t.Fatal("canonical inline Meta Pixel must be detected as a tracker")
			}
		},
	},
	{
		name: "canonical_inline_ga4_detected_as_tracker",
		html: `<!DOCTYPE html><html><head>
<script>window.dataLayer=window.dataLayer||[];function gtag(){dataLayer.push(arguments);}gtag('js',new Date());gtag('config','G-ABCDEFG');</script>
</head><body><h1>Flat</h1></body></html>`,
		justify: "This is GA4's inline half: gtag() is defined and called with a real measurement ID. Even with no " +
			"static <script src=googletagmanager.com> tag anywhere (some installs load gtag.js from a tag-manager " +
			"container instead), the gtag(...) calls are themselves what queues and later fires the hits.",
		verify: func(t *testing.T, checks []CheckResult) {
			if !trackerDetected(checks, "googletagmanager") {
				t.Fatal("canonical inline GA4 gtag() calls must be detected as a tracker")
			}
		},
	},

	// --- C. A real inline-bootstrapped CMP: the false-negative half of the same finding ---
	{
		name: "real_inline_bootstrapped_cmp_detected_as_consent",
		html: `<!DOCTYPE html><html><head><script type="text/javascript">
(function(d, s, id) {
  var js, tjs = d.getElementsByTagName(s)[0];
  if (d.getElementById(id)) return;
  js = d.createElement(s); js.id = id;
  js.src = "https://app.termly.io/embed.min.js";
  js.setAttribute("data-name", "termly-embed-1234");
  tjs.parentNode.insertBefore(js, tjs);
}(document, "script", "termly-jssdk"));
</script></head><body><h1>Hi</h1></body></html>`,
		justify: "This is Termly's own documented embed snippet: it creates a <script> element, sets its .src to " +
			"app.termly.io, and inserts it into the document, with no static src= attribute anywhere in the built " +
			"HTML. Refusing this credit (patch 3's failure mode) tells a tenant who correctly installed their CMP " +
			"that they have not, which erodes trust in every other check on the report.",
		verify: func(t *testing.T, checks []CheckResult) {
			passed, detail, _ := consentBannerDetail(checks)
			if !passed {
				t.Fatalf("a genuinely inline-bootstrapped CMP must be DETECTED, got fail: %s", detail)
			}
		},
	},

	// --- D. Document order and the same-blob collision ----------------------
	{
		name: "tracker_before_cmp_in_document_order",
		html: `<!DOCTYPE html><html><head>
<script src="https://www.googletagmanager.com/gtag/js?id=G-XYZ"></script>
<script src="https://consent.cookiebot.com/uc.js" data-cbid="x"></script>
</head><body><h1>Hi</h1></body></html>`,
		justify: "The tracker's network request fires the instant the parser reaches its <script> tag, which is " +
			"before the CMP's own tag even begins downloading. The visitor's data has already left before any " +
			"consent UI could possibly have been shown.",
		verify: func(t *testing.T, checks []CheckResult) {
			passed, detail := preConsentDetail(checks)
			if passed == nil || *passed {
				t.Fatal("a tracker ordered before the CMP must fail Pre-Consent Tracking")
			}
			if !strings.Contains(detail, "ahead of the consent script") {
				t.Errorf("detail should name the ordering, got %q", detail)
			}
		},
	},
	{
		name: "cmp_before_tracker_but_still_ungated",
		html: `<!DOCTYPE html><html><head>
<script src="https://consent.cookiebot.com/uc.js" data-cbid="x"></script>
<script src="https://www.googletagmanager.com/gtag/js?id=G-XYZ"></script>
</head><body><h1>Hi</h1></body></html>`,
		justify: "Loading a CMP's script FIRST does not stop an ordinary, ungated <script> tag placed right after it " +
			"from executing the moment the parser reaches it. The CMP being present earlier in the document is not " +
			"a technical gate; only type=\"text/plain\" plus a consent-category attribute is. This exact shape (any " +
			"consent evidence earlier in order, ungated tracker after it) is what a verifier used to reproduce the " +
			"six-point fabrication past the previous remediation.",
		verify: func(t *testing.T, checks []CheckResult) {
			passed, detail := preConsentDetail(checks)
			if passed == nil || *passed {
				t.Fatal("a CMP loading earlier must NOT excuse an ungated tracker that follows it")
			}
			if !strings.Contains(detail, "does not gate this tag") {
				t.Errorf("detail should say loading earlier is not the same as gating, got %q", detail)
			}
		},
	},
	{
		name: "both_tracker_and_cmp_in_one_blob",
		html: `<!DOCTYPE html><html><head><script>
Cookiebot.renew();
(function(w,d,s,l,i){w[l]=w[l]||[];w[l].push({'gtm.start':new Date().getTime(),event:'gtm.js'});
var f=d.getElementsByTagName(s)[0],j=d.createElement(s);j.async=true;
j.src='https://www.googletagmanager.com/gtm.js?id='+i;f.parentNode.insertBefore(j,f);
})(window,document,'script','dataLayer','GTM-ABCDEF');
</script></head><body><h1>Hi</h1></body></html>`,
		justify: "One <script> tag runs top to bottom unconditionally; naming a CMP anywhere inside it does not pause " +
			"execution partway through to wait for consent. The GTM loader in the second half of this exact tag " +
			"still fires regardless of the Cookiebot reference in the first half.",
		verify: func(t *testing.T, checks []CheckResult) {
			passed, detail := preConsentDetail(checks)
			if passed == nil || *passed {
				t.Fatal("a tracker sharing a script tag with a CMP reference must still fail")
			}
			if !strings.Contains(detail, "same script tag") {
				t.Errorf("detail should name the shared-tag misconfiguration, got %q", detail)
			}
		},
	},
	{
		name: "tracker_inside_script_that_also_names_cmp_weak_reference",
		html: `<!DOCTYPE html><html><head><script>
// integrates with cookiebot for consent
gtag('config', 'G-ABCDEFG');
</script></head><body><h1>Hi</h1></body></html>`,
		justify: "The only CMP mention is a comment, so Consent Banner gets nothing at all (comments never execute); " +
			"the gtag() call is real code and fires unconditionally on load. There is no consent platform anywhere " +
			"on this page to gate it.",
		verify: func(t *testing.T, checks []CheckResult) {
			passed, _, _ := consentBannerDetail(checks)
			if passed {
				t.Fatal("a CMP named only in a comment beside a real tracker call must not credit consent")
			}
			pcPassed, detail := preConsentDetail(checks)
			if pcPassed == nil || *pcPassed {
				t.Fatal("the gtag() tracker call must still fail Pre-Consent Tracking")
			}
			if !strings.Contains(detail, "no consent platform on the page") {
				t.Errorf("detail should say no consent platform exists, got %q", detail)
			}
		},
	},

	// --- E. Genuinely nothing: must be ABSENT, not INDETERMINATE ------------
	{
		name: "genuinely_nothing_is_absent_not_indeterminate",
		html: `<!DOCTYPE html><html><body><h1>A perfectly ordinary page with no scripts at all.</h1></body></html>`,
		justify: "There is no tracker, no CMP reference of any kind, and nothing ambiguous to verify manually. An " +
			"auditor would call this a clean, trivially-compliant page (no tracking to gate), not a page that needs " +
			"a human to go check something.",
		verify: func(t *testing.T, checks []CheckResult) {
			passed, detail, rec := consentBannerDetail(checks)
			if passed {
				t.Fatal("a page with nothing must not pass Consent Banner")
			}
			if strings.Contains(detail, "verify manually") || strings.Contains(rec, "Verify manually") {
				t.Errorf("a page with NO evidence at all must be ABSENT, not INDETERMINATE: %s / %s", detail, rec)
			}
			pcPassed, pcDetail := preConsentDetail(checks)
			if pcPassed != nil {
				t.Errorf("Pre-Consent Tracking should be Info (nothing to gate), got Passed=%v: %s", *pcPassed, pcDetail)
			}
		},
	},

	// --- F. The INDETERMINATE tier itself ------------------------------------
	{
		name: "weak_reference_with_no_load_evidence_is_indeterminate",
		html: `<!DOCTYPE html><html><head><script>
Cookiebot.renew();
</script></head><body><h1>Hi</h1></body></html>`,
		justify: "A real call in code position (not a comment, not a string) is genuine evidence someone integrated " +
			"with Cookiebot's API, but nothing on this page ever loads cookiebot.com or carries " +
			"data-consent-platform. A careful auditor would flag this for manual verification rather than either " +
			"crediting it or dismissing it as nothing.",
		verify: func(t *testing.T, checks []CheckResult) {
			passed, detail, rec := consentBannerDetail(checks)
			if passed {
				t.Fatal("a bare call reference with no load evidence must not pass Consent Banner")
			}
			if !strings.Contains(detail, "cookiebot") {
				t.Errorf("detail should name which platform was referenced, got %q", detail)
			}
			if !strings.Contains(rec, "Verify manually") {
				t.Errorf("recommendation should tell the tenant to verify manually, got %q", rec)
			}
		},
	},
}

func TestPrivacyDetectionAdversarialCorpus(t *testing.T) {
	for _, tc := range privacyCorpus {
		t.Run(tc.name, func(t *testing.T) {
			if tc.justify == "" {
				t.Fatal("every corpus case must carry a justification")
			}
			site := &SiteContext{Pages: []PageContext{page(t, "/", tc.html)}}
			checks := RunPrivacyChecks(site)
			tc.verify(t, checks)
		})
	}
}

// TestPrivacyCorpusHasNoZeroTrackerFalsePositive is a sanity check on the
// corpus itself: the comment-only/string-only/variable-only cases must not
// accidentally also read as trackers (they don't mention any tracker
// platform), so Tracker Count on those pages should show a clean pass. This
// guards against a corpus case that "passes" only because two different
// checks cancelled each other out.
func TestPrivacyCorpusHasNoZeroTrackerFalsePositive(t *testing.T) {
	for _, name := range []string{
		"cmp_named_only_in_html_comment",
		"cmp_named_only_in_js_line_comment",
		"cmp_named_only_in_js_block_comment",
		"cmp_named_only_in_string_literal",
		"cmp_named_only_in_variable_name",
		"cmp_named_inside_disabled_block",
	} {
		var tc *corpusCase
		for i := range privacyCorpus {
			if privacyCorpus[i].name == name {
				tc = &privacyCorpus[i]
			}
		}
		if tc == nil {
			t.Fatalf("corpus case %q not found", name)
		}
		site := &SiteContext{Pages: []PageContext{page(t, "/", tc.html)}}
		checks := RunPrivacyChecks(site)
		if !noTrackerDetectedAtAll(checks) {
			t.Errorf("%s: expected a clean Tracker Count pass (no tracker mentioned in this fixture)", name)
		}
	}
}
