package builder

import (
	"strings"
	"testing"

	"github.com/bright-interaction/slab/internal/store"
)

// TestRenderCookieProofSnippet_HappyPath verifies the snippet emits the
// expected single same-origin script tag and that the per-site config
// prefix carries siteId/domain/proofEndpoint/cssVars + GCM default-denied
// stub. After 2026-05-01 the snippet became one tag; the config + GCM
// stub moved into the per-site bundle so CSP `script-src 'self'` doesn't
// have to allow inline.
func TestRenderCookieProofSnippet_HappyPath(t *testing.T) {
	cfg := CookieProofConfig{
		SiteID:    "abcdef0123456789abcdef01",
		Domain:    "example.com",
		Locale:    "sv",
		TrackPath: "/t",
		CssVars: map[string]string{
			"--cc-bg":             "#FAFAFA",
			"--cc-btn-primary-bg": "#0066CC",
		},
	}
	out := RenderCookieProofSnippet(cfg)
	prefix0, err := RenderCookieProofConfigPrefix(cfg)
	if err != nil {
		t.Fatalf("prefix: %v", err)
	}
	expectedAsset := "/" + CookieProofWidgetFilename(prefix0)
	if !strings.Contains(out, expectedAsset) {
		t.Errorf("missing same-origin widget asset reference %q: %s", expectedAsset, out)
	}
	if strings.Contains(out, "consent.example.com") {
		t.Errorf("snippet must not reference consent.example.com: %s", out)
	}
	// 2026-05-19: the widget filename is hashed (_ccb.{hash}.js) so the
	// Inspector's privacy check can't detect the consent banner by
	// matching "cookieproof" in the script src. The snippet must carry
	// a data-consent-platform attribute the eval picks up instead.
	if !strings.Contains(out, `data-consent-platform="cookieproof"`) {
		t.Errorf("snippet missing data-consent-platform attribute (Inspector won't grade Privacy correctly without it): %s", out)
	}
	// Inline scripts moved to the per-site bundle. Validate the bundle
	// prefix carries the config + GCM stub.
	prefix, err := RenderCookieProofConfigPrefix(cfg)
	if err != nil {
		t.Fatalf("config prefix: %v", err)
	}
	prefixStr := string(prefix)
	if !strings.Contains(prefixStr, `"siteId":"abcdef0123456789abcdef01"`) {
		t.Errorf("missing siteId in bundle prefix: %s", prefixStr)
	}
	if !strings.Contains(prefixStr, `"domain":"example.com"`) {
		t.Errorf("missing domain in bundle prefix: %s", prefixStr)
	}
	if !strings.Contains(prefixStr, `"proofEndpoint":"/t/consent"`) {
		t.Errorf("missing proofEndpoint in bundle prefix: %s", prefixStr)
	}
	if !strings.Contains(prefixStr, `"--cc-bg":"#FAFAFA"`) {
		t.Errorf("missing cssVars in bundle prefix: %s", prefixStr)
	}
	if !strings.Contains(prefixStr, "gtag('consent','default'") {
		t.Errorf("missing GCM default-denied stub in bundle prefix: %s", prefixStr)
	}
}

// TestRenderCookieProofSnippet_DefaultsTrackPath confirms an empty trackPath
// falls back to /t in the per-site bundle prefix.
func TestRenderCookieProofSnippet_DefaultsTrackPath(t *testing.T) {
	prefix, err := RenderCookieProofConfigPrefix(CookieProofConfig{
		SiteID: "aaaaaaaaaaaaaaaaaaaaaaaa",
		Domain: "example.com",
	})
	if err != nil {
		t.Fatalf("config prefix: %v", err)
	}
	if !strings.Contains(string(prefix), `"proofEndpoint":"/t/consent"`) {
		t.Errorf("expected default /t/consent: %s", string(prefix))
	}
}

// TestRenderCookieProofSnippet_TrimsTrailingSlash ensures /t/ doesn't produce
// /t//consent.
func TestRenderCookieProofSnippet_TrimsTrailingSlash(t *testing.T) {
	prefix, err := RenderCookieProofConfigPrefix(CookieProofConfig{
		SiteID:    "aaaaaaaaaaaaaaaaaaaaaaaa",
		Domain:    "example.com",
		TrackPath: "/t/",
	})
	if err != nil {
		t.Fatalf("config prefix: %v", err)
	}
	if strings.Contains(string(prefix), `"proofEndpoint":"/t//consent"`) {
		t.Errorf("did not trim trailing slash: %s", string(prefix))
	}
	if !strings.Contains(string(prefix), `"proofEndpoint":"/t/consent"`) {
		t.Errorf("expected /t/consent: %s", string(prefix))
	}
}

// TestRenderCookieProofSnippet_EmptyArgs returns "" when required args are
// missing so the layout doesn't emit a broken snippet.
func TestRenderCookieProofSnippet_EmptyArgs(t *testing.T) {
	if got := RenderCookieProofSnippet(CookieProofConfig{Domain: "example.com"}); got != "" {
		t.Errorf("expected empty for missing siteID, got %q", got)
	}
	if got := RenderCookieProofSnippet(CookieProofConfig{SiteID: "aaaa"}); got != "" {
		t.Errorf("expected empty for missing domain, got %q", got)
	}
}

// TestCookieProofConfigPrefix_CopyOverrides confirms title/description/etc.
// land in the bundle-prefix config.copy object so the widget uses them
// as translation overrides.
func TestCookieProofConfigPrefix_CopyOverrides(t *testing.T) {
	prefix, err := RenderCookieProofConfigPrefix(CookieProofConfig{
		SiteID:        "aaaaaaaaaaaaaaaaaaaaaaaa",
		Domain:        "example.com",
		Title:         "Vi använder kakor",
		AcceptLabel:   "Acceptera",
		RejectLabel:   "Avvisa",
		SettingsLabel: "Anpassa",
	})
	if err != nil {
		t.Fatalf("config prefix: %v", err)
	}
	out := string(prefix)
	if !strings.Contains(out, `"title":"Vi använder kakor"`) {
		t.Errorf("missing title override: %s", out)
	}
	if !strings.Contains(out, `"accept":"Acceptera"`) {
		t.Errorf("missing accept override: %s", out)
	}
	if !strings.Contains(out, `"reject":"Avvisa"`) {
		t.Errorf("missing reject override: %s", out)
	}
	if !strings.Contains(out, `"customize":"Anpassa"`) {
		t.Errorf("missing customize override: %s", out)
	}
}

// TestCookieProofWidgetHash_StableAcrossCalls makes sure the cache-bust hash
// is deterministic for a given embedded bundle, so tenant builds reuse the
// same filename across rebuilds (and CDN caches stay warm).
func TestCookieProofWidgetHash_StableAcrossCalls(t *testing.T) {
	a := CookieProofWidgetHash()
	b := CookieProofWidgetHash()
	if a != b {
		t.Errorf("hash should be stable, got %q vs %q", a, b)
	}
	if len(a) != 8 {
		t.Errorf("expected 8-char hash, got %d", len(a))
	}
}

// TestCookieProofThemeFromBranding_MapsPalette confirms the branding palette
// fills in the cssVars the widget exposes, and skips empty fields.
func TestCookieProofThemeFromBranding_MapsPalette(t *testing.T) {
	site := store.Site{
		BgColor:        "#FAFAFA",
		TextColor:      "#1A1A1A",
		PrimaryColor:   "#0066CC",
		OnPrimaryColor: "#FFFFFF",
		MutedColor:     "#6B7280",
		BorderColor:    "#E5E7EB",
		SurfaceColor:   "#FFFFFF",
		FontBody:       "Source Sans Pro",
	}
	vars := cookieProofThemeFromBranding(site)
	if vars["--cc-bg"] != "#FAFAFA" {
		t.Errorf("--cc-bg = %q want #FAFAFA", vars["--cc-bg"])
	}
	if vars["--cc-btn-primary-bg"] != "#0066CC" {
		t.Errorf("--cc-btn-primary-bg = %q want #0066CC", vars["--cc-btn-primary-bg"])
	}
	if vars["--cc-btn-primary-text"] != "#FFFFFF" {
		t.Errorf("--cc-btn-primary-text = %q want #FFFFFF", vars["--cc-btn-primary-text"])
	}
	font := vars["--cc-font"]
	if !strings.HasPrefix(font, `"Source Sans Pro"`) {
		t.Errorf("multi-word font family should be quoted, got %q", font)
	}
}

func TestCookieProofThemeFromBranding_SkipsEmpty(t *testing.T) {
	site := store.Site{} // every field empty
	vars := cookieProofThemeFromBranding(site)
	if len(vars) != 0 {
		t.Errorf("expected no vars for empty branding, got %d: %v", len(vars), vars)
	}
}

// TestCookieProofThemeFromBranding_RejectMatchesAccept locks in the IMY
// 2026 equal-prominence rule: Reject and Accept must carry the same fill
// color. Regression guard for the previous bug where Reject was mapped to
// surface (white) and disappeared into the modal background.
func TestCookieProofThemeFromBranding_RejectMatchesAccept(t *testing.T) {
	site := store.Site{
		PrimaryColor:   "#0066CC",
		OnPrimaryColor: "#FFFFFF",
		SurfaceColor:   "#FFFFFF",
		TextColor:      "#1A1A1A",
	}
	vars := cookieProofThemeFromBranding(site)
	if vars["--cc-btn-reject-bg"] != "#0066CC" {
		t.Errorf("--cc-btn-reject-bg = %q want #0066CC (must match primary)", vars["--cc-btn-reject-bg"])
	}
	if vars["--cc-btn-reject-text"] != "#FFFFFF" {
		t.Errorf("--cc-btn-reject-text = %q want #FFFFFF (must match on-primary)", vars["--cc-btn-reject-text"])
	}
}

// TestPrimaryAndRejectAlwaysShareColor is the property-style guardrail
// that enforces IMY 2026 equal-prominence across the entire palette
// space — not just one fixed brand color. Whatever site.PrimaryColor /
// OnPrimaryColor combination is fed in, the four corresponding CSS vars
// must come out tied together. Break this and IMY compliance breaks
// silently for every tenant on the next build.
func TestPrimaryAndRejectAlwaysShareColor(t *testing.T) {
	cases := []struct {
		name, primary, onPrimary string
	}{
		{"BI gold", "#D4AF37", "#111111"},
		{"BI teal", "#0E7490", "#FFFFFF"},
		{"safety blue", "#0066CC", "#FFFFFF"},
		{"emerald", "#059669", "#FFFFFF"},
		{"deep purple", "#4C1D95", "#FFFFFF"},
		{"amber", "#F59E0B", "#111111"},
		{"crimson", "#DC2626", "#FFFFFF"},
		{"slate", "#1E293B", "#FFFFFF"},
		{"shorthand hex", "#FA0", "#000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			site := store.Site{
				PrimaryColor:   tc.primary,
				OnPrimaryColor: tc.onPrimary,
				// Branding fields below are deliberately set to weird values
				// so we'd notice if reject ever leaked from one of them.
				SurfaceColor: "#000000",
				BgColor:      "#FF00FF",
				TextColor:    "#00FF00",
				MutedColor:   "#FF8800",
				BorderColor:  "#888888",
			}
			vars := cookieProofThemeFromBranding(site)
			if vars["--cc-btn-primary-bg"] != vars["--cc-btn-reject-bg"] {
				t.Errorf("IMY equal-prominence violated: primary-bg=%q reject-bg=%q",
					vars["--cc-btn-primary-bg"], vars["--cc-btn-reject-bg"])
			}
			if vars["--cc-btn-primary-text"] != vars["--cc-btn-reject-text"] {
				t.Errorf("IMY equal-prominence violated: primary-text=%q reject-text=%q",
					vars["--cc-btn-primary-text"], vars["--cc-btn-reject-text"])
			}
		})
	}
}

// TestPrimaryAndRejectShareColor_EmptyPrimary covers the corner: a site
// with no configured primary color must skip both pairs together. Either
// both vars are present and equal, or neither is present (the widget then
// falls back to its built-in defaults, which already render Accept and
// Reject in the same teal). The bad outcome is one set + the other unset.
func TestPrimaryAndRejectShareColor_EmptyPrimary(t *testing.T) {
	site := store.Site{} // no branding at all
	vars := cookieProofThemeFromBranding(site)
	_, primaryBgSet := vars["--cc-btn-primary-bg"]
	_, rejectBgSet := vars["--cc-btn-reject-bg"]
	if primaryBgSet != rejectBgSet {
		t.Errorf("primary-bg and reject-bg presence diverged: primary=%v reject=%v", primaryBgSet, rejectBgSet)
	}
	_, primaryTextSet := vars["--cc-btn-primary-text"]
	_, rejectTextSet := vars["--cc-btn-reject-text"]
	if primaryTextSet != rejectTextSet {
		t.Errorf("primary-text and reject-text presence diverged: primary=%v reject=%v", primaryTextSet, rejectTextSet)
	}
}

// TestCookieProofThemeFromBranding_SecondaryButton verifies the Save
// Preferences button picks up the site's secondary brand color (not
// surface) and gets a contrasting text color computed from luminance.
func TestCookieProofThemeFromBranding_SecondaryButton(t *testing.T) {
	site := store.Site{
		PrimaryColor:   "#0066CC",
		OnPrimaryColor: "#FFFFFF",
		SecondaryColor: "#935FA7",
		SurfaceColor:   "#FFFFFF",
		TextColor:      "#1A1A1A",
	}
	vars := cookieProofThemeFromBranding(site)
	if vars["--cc-btn-secondary-bg"] != "#935FA7" {
		t.Errorf("--cc-btn-secondary-bg = %q want #935FA7", vars["--cc-btn-secondary-bg"])
	}
	// Purple #935FA7 is dark enough that white text is legible on it.
	if vars["--cc-btn-secondary-text"] != "#FFFFFF" {
		t.Errorf("--cc-btn-secondary-text = %q want #FFFFFF on dark purple", vars["--cc-btn-secondary-text"])
	}
}

// TestCookieProofThemeFromBranding_SecondaryFallsBackToSurface confirms
// tenants without a configured secondary brand color still get a usable
// Save Preferences button (the previous quiet-near-white tone).
func TestCookieProofThemeFromBranding_SecondaryFallsBackToSurface(t *testing.T) {
	site := store.Site{
		PrimaryColor:   "#0066CC",
		OnPrimaryColor: "#FFFFFF",
		SurfaceColor:   "#F5F5F5",
		TextColor:      "#1A1A1A",
	}
	vars := cookieProofThemeFromBranding(site)
	if vars["--cc-btn-secondary-bg"] != "#F5F5F5" {
		t.Errorf("--cc-btn-secondary-bg = %q want #F5F5F5 (surface fallback)", vars["--cc-btn-secondary-bg"])
	}
	// Light surface → contrast helper should pick the dark text.
	if vars["--cc-btn-secondary-text"] != "#1A1A1A" {
		t.Errorf("--cc-btn-secondary-text = %q want #1A1A1A on light fill", vars["--cc-btn-secondary-text"])
	}
}

func TestPresetCookieDeclarations_AlwaysOnBaseline(t *testing.T) {
	site := store.Site{Lang: "sv"}
	got := PresetCookieDeclarations(site, map[string]string{})
	// Expect the Atomic Site consent cookie in necessary + lang in
	// preferences. The cookie name must match what the widget actually
	// writes, so we read from the same constant the embed config uses.
	wantNames := map[string]string{
		AtomicSiteConsentCookieName: "necessary",
		"lang":                      "preferences",
	}
	for name, cat := range wantNames {
		found := false
		for _, d := range got {
			if d.Name == name && d.Category == cat {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing baseline preset %s in %s; got %+v", name, cat, got)
		}
	}
}

// TestEmbedConfig_ComplianceFieldsRoundtrip locks in that the new
// compliance + UX settings (added 2026-05-01) actually flow into the
// embed config that the widget reads. Regression guard: any future
// refactor that drops one of these from the JSON output is caught
// here. The setting names mirror analytics.cookie_expiry_days,
// analytics.cookie_revision, etc.
func TestEmbedConfig_ComplianceFieldsRoundtrip(t *testing.T) {
	cfg := CookieProofConfig{
		SiteID:           "abcdef0123456789abcdef01",
		Domain:           "example.com",
		Locale:           "sv",
		TrackPath:        "/t",
		Theme:            "auto",
		FloatingTrigger:  "right",
		LanguageSelector: true,
		CookieExpiryDays: 180,
		Revision:         3,
		CcpaEnabled:      true,
		CcpaURL:          "/privacy#ccpa",
	}
	out, err := buildCookieProofEmbedConfig(cfg)
	if err != nil {
		t.Fatalf("buildCookieProofEmbedConfig: %v", err)
	}
	js := string(out)
	wantSubstrings := []string{
		`"theme":"auto"`,
		`"floatingTrigger":"right"`,
		`"languageSelector":true`,
		`"cookieExpiry":180`,
		`"revision":3`,
		`"ccpaEnabled":true`,
		`"ccpaUrl":"/privacy#ccpa"`,
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(js, want) {
			t.Errorf("embed config missing %s in: %s", want, js)
		}
	}
}

// TestEmbedConfig_FloatingTriggerOff verifies that the "off" value maps
// to a JSON `false`, which is what the widget interprets as "no
// floating trigger" (booleans + 'left'/'right' strings are the widget's
// supported shape).
func TestEmbedConfig_FloatingTriggerOff(t *testing.T) {
	cfg := CookieProofConfig{
		SiteID:          "abcdef0123456789abcdef01",
		Domain:          "example.com",
		FloatingTrigger: "off",
	}
	out, err := buildCookieProofEmbedConfig(cfg)
	if err != nil {
		t.Fatalf("buildCookieProofEmbedConfig: %v", err)
	}
	if !strings.Contains(string(out), `"floatingTrigger":false`) {
		t.Errorf("expected floatingTrigger=false for 'off', got: %s", string(out))
	}
}

// TestEmbedConfig_ThemeFallsBackToLight verifies that an unrecognized
// theme value falls back to "light" rather than passing through and
// confusing the widget.
func TestEmbedConfig_ThemeFallsBackToLight(t *testing.T) {
	cfg := CookieProofConfig{
		SiteID: "abcdef0123456789abcdef01",
		Domain: "example.com",
		Theme:  "neon",
	}
	out, err := buildCookieProofEmbedConfig(cfg)
	if err != nil {
		t.Fatalf("buildCookieProofEmbedConfig: %v", err)
	}
	if !strings.Contains(string(out), `"theme":"light"`) {
		t.Errorf("unknown theme should fall back to light, got: %s", string(out))
	}
}

// TestBuildCookieProofConfig_ReadsComplianceSettings ensures the
// settings-map-to-config plumbing actually wires every new key. Sets
// each setting to a non-default value and confirms it shows up on the
// CookieProofConfig the builder produces.
func TestBuildCookieProofConfig_ReadsComplianceSettings(t *testing.T) {
	site := store.Site{
		ID:             "siteX",
		Domain:         "example.com",
		Lang:           "sv",
		PrimaryColor:   "#0066CC",
		OnPrimaryColor: "#FFFFFF",
		SecondaryColor: "#935FA7",
		SurfaceColor:   "#FFFFFF",
		TextColor:      "#1A1A1A",
	}
	settings := map[string]string{
		"analytics.cookie_theme":             "dark",
		"analytics.cookie_floating_trigger":  "right",
		"analytics.cookie_language_selector": "1",
		"analytics.cookie_expiry_days":       "120",
		"analytics.cookie_revision":          "7",
		"analytics.ccpa_enabled":             "1",
		"analytics.ccpa_url":                 "/privacy#ccpa",
	}
	cfg := BuildCookieProofConfig(site, settings)
	if cfg.Theme != "dark" {
		t.Errorf("Theme = %q want dark", cfg.Theme)
	}
	if cfg.FloatingTrigger != "right" {
		t.Errorf("FloatingTrigger = %q want right", cfg.FloatingTrigger)
	}
	if !cfg.LanguageSelector {
		t.Error("LanguageSelector should be true")
	}
	if cfg.CookieExpiryDays != 120 {
		t.Errorf("CookieExpiryDays = %d want 120", cfg.CookieExpiryDays)
	}
	if cfg.Revision != 7 {
		t.Errorf("Revision = %d want 7", cfg.Revision)
	}
	if !cfg.CcpaEnabled {
		t.Error("CcpaEnabled should be true")
	}
	if cfg.CcpaURL != "/privacy#ccpa" {
		t.Errorf("CcpaURL = %q want /privacy#ccpa", cfg.CcpaURL)
	}
}

// TestEmbedConfig_AtomicSiteConsentCookieName guards the rename: the
// widget's storage layer reads cookieName from window.__CCB__, so the
// embed config must always carry the Atomic Site name. If anyone ever
// removes the CookieName assignment in buildCookieProofEmbedConfig, the
// widget falls back to the upstream "ce_consent" default and the
// declared cookie row stops matching what the browser actually stores.
func TestEmbedConfig_AtomicSiteConsentCookieName(t *testing.T) {
	cfg := CookieProofConfig{
		SiteID:    "abcdef0123456789abcdef01",
		Domain:    "example.com",
		Locale:    "sv",
		TrackPath: "/t",
	}
	out, err := buildCookieProofEmbedConfig(cfg)
	if err != nil {
		t.Fatalf("buildCookieProofEmbedConfig: %v", err)
	}
	want := `"cookieName":"` + AtomicSiteConsentCookieName + `"`
	if !strings.Contains(string(out), want) {
		t.Errorf("embed config missing %s; got %s", want, string(out))
	}
	if AtomicSiteConsentCookieName == "ce_consent" {
		t.Error("AtomicSiteConsentCookieName regressed to upstream CookieProof default")
	}
}

func TestPresetCookieDeclarations_GA4AddsGoogleCookies(t *testing.T) {
	site := store.Site{Lang: "en"}
	settings := map[string]string{"analytics.ga4_enabled": "1"}
	got := PresetCookieDeclarations(site, settings)
	wantGoogleCookies := []string{"_ga", "_ga_*", "_gid", "_gac_*"}
	for _, name := range wantGoogleCookies {
		found := false
		for _, d := range got {
			if d.Name == name && d.Category == "analytics" && d.Provider == "Google" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("GA4-enabled site missing preset %s in analytics; got %+v", name, got)
		}
	}
}

func TestMergeDeclarations_UserOverridesPreset(t *testing.T) {
	presets := []CookieDeclaration{
		{Category: "analytics", Name: "_ga", Provider: "Google", Purpose: "Distinguishes unique users", Expiry: "2 years"},
	}
	user := []CookieDeclaration{
		{Category: "analytics", Name: "_ga", Provider: "Google (translated)", Purpose: "Skiljer på användare", Expiry: "2 år"},
	}
	merged := mergeDeclarations(presets, user)
	if len(merged) != 1 {
		t.Fatalf("expected 1 merged row, got %d: %+v", len(merged), merged)
	}
	if merged[0].Purpose != "Skiljer på användare" {
		t.Errorf("user purpose should win: got %q", merged[0].Purpose)
	}
}

func TestMergeDeclarations_AppendsNonCollidingUserRows(t *testing.T) {
	presets := []CookieDeclaration{
		{Category: "necessary", Name: "ce_consent", Provider: "This site"},
	}
	user := []CookieDeclaration{
		{Category: "marketing", Name: "fbp", Provider: "Meta", Purpose: "Advertising", Expiry: "3 months"},
	}
	merged := mergeDeclarations(presets, user)
	if len(merged) != 2 {
		t.Fatalf("expected 2 merged rows, got %d", len(merged))
	}
	// Presets first, then user-only entries.
	if merged[0].Name != "ce_consent" || merged[1].Name != "fbp" {
		t.Errorf("order wrong: %+v", merged)
	}
}

func TestParseUserCookieDeclarations_DropsBlankNames(t *testing.T) {
	raw := `[{"category":"analytics","name":"  ","provider":"x"},{"category":"analytics","name":"_ga"}]`
	got := parseUserCookieDeclarations(raw)
	if len(got) != 1 || got[0].Name != "_ga" {
		t.Errorf("expected only _ga, got %+v", got)
	}
}

func TestBuildCookieProofConfig_InjectsDeclarations(t *testing.T) {
	site := store.Site{
		ID:             "siteX",
		Domain:         "example.com",
		Lang:           "sv",
		PrimaryColor:   "#0066CC",
		OnPrimaryColor: "#FFFFFF",
		SecondaryColor: "#935FA7",
		SurfaceColor:   "#FFFFFF",
		TextColor:      "#1A1A1A",
	}
	settings := map[string]string{
		"analytics.ga4_enabled":         "1",
		"analytics.cookie_declarations": `[{"category":"marketing","name":"fbp","provider":"Meta","purpose":"Advertising","expiry":"3 months"}]`,
	}
	cfg := BuildCookieProofConfig(site, settings)

	// Find each category and check declarations are present.
	byID := map[string]CookieCategory{}
	for _, c := range cfg.Categories {
		byID[c.ID] = c
	}
	if got := byID["analytics"]; len(got.Declarations) == 0 {
		t.Errorf("expected GA4 declarations on analytics category")
	}
	if got := byID["marketing"]; len(got.Declarations) != 1 || got.Declarations[0].Name != "fbp" {
		t.Errorf("expected user-added fbp on marketing; got %+v", got.Declarations)
	}
	if got := byID["necessary"]; len(got.Declarations) == 0 || got.Declarations[0].Name != AtomicSiteConsentCookieName {
		t.Errorf("expected %s preset on necessary; got %+v", AtomicSiteConsentCookieName, got.Declarations)
	}
}
