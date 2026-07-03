package builder

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/bright-interaction/slab/internal/store"
)

// AtomicSiteConsentCookieName is the first-party cookie + localStorage key
// every Atomic Site banner uses for the consent record. We deliberately
// override the CookieProof default ("ce_consent") so tenants see an
// Atomic-Site-branded cookie in their DevTools / consent table, and so
// downstream tooling can recognize "this banner is on Atomic Site" by the
// cookie name alone.
//
// The widget's storage layer accepts any name matching /^[a-zA-Z0-9_-]+$/
// and rejects anything else — keep this constant in that shape.
const AtomicSiteConsentCookieName = "as_consent"

// CookieProofConfig captures the per-tenant inputs the snippet generator
// needs. Most fields are optional and default to widget defaults if empty.
type CookieProofConfig struct {
	SiteID    string
	Domain    string
	Locale    string
	TrackPath string // POST endpoint for proofs, e.g. "/t/consent"
	CssVars   map[string]string

	// Optional banner copy overrides. Empty values fall back to the widget's
	// built-in i18n strings for the resolved Locale.
	Title         string
	Description   string
	AcceptLabel   string
	RejectLabel   string
	SettingsLabel string

	// Banner position: "bottom" (default), "top", or "center".
	Position string

	// Theme: "light", "dark", or "auto" (follow OS prefers-color-scheme).
	// Empty falls back to "light".
	Theme string

	// FloatingTrigger: "left", "right", or "off". Empty defaults to "left".
	FloatingTrigger string

	// LanguageSelector controls the in-banner language dropdown:
	//   - false:        dropdown hidden (single language)
	//   - true:         dropdown shown with all 14 widget-bundled locales
	//                   (da, de, en, es, fi, fr, it, ja, nl, no, pl, pt, sv)
	//   - non-empty []: dropdown shown limited to the listed ISO codes,
	//                   e.g. ["en", "sv"] for an EN+SV-only site
	LanguageSelector    bool
	LanguageSelectorSet []string

	// CookieExpiryDays bounds how long the visitor's stored consent
	// record stays valid before the banner re-prompts. 0 = widget default.
	CookieExpiryDays int

	// Revision is the consent policy version. Bump when categories or
	// privacy policy materially change to invalidate prior consent.
	Revision int

	// CcpaEnabled toggles the "Do Not Sell" link in the banner footer.
	CcpaEnabled bool
	// CcpaURL is the disclosure target. Required when CcpaEnabled.
	CcpaURL string

	// Categories to enable. Empty = use widget defaults (necessary, analytics,
	// marketing, preferences with necessary required, others off).
	Categories []CookieCategory

	// PrivacyPolicyURL surfaces a link in the banner footer when set.
	PrivacyPolicyURL string

	// CustomTranslations are per-language overrides for banner copy.
	// Outer key = ISO language code (e.g. "sv", "de"). Inner map keys
	// follow the widget's TranslationStrings shape: title, description,
	// accept, reject, customize. Empty inner fields fall back to the
	// widget's built-in translation for that locale, so operators only
	// fill what they want to override. Mirrors CookieProof's
	// customTranslations option (CookieProof/src/i18n/index.ts).
	CustomTranslations map[string]map[string]string
}

// CookieCategory mirrors the relevant subset of the widget's CategoryConfig.
// Atomicsite stores category preferences flat per site; we render them out as
// a list when emitting the inline config.
//
// Declarations is the per-category cookie list the widget renders as a
// table in its preferences modal. The widget shape excludes the category
// field (it's already nested under the category), so we use widgetCookieDecl.
type CookieCategory struct {
	ID           string             `json:"id"`
	Required     bool               `json:"required,omitempty"`
	Enabled      bool               `json:"enabled,omitempty"`
	Declarations []widgetCookieDecl `json:"declarations,omitempty"`
}

// CookieDeclaration is atomicsite's storage + admin-API shape for a single
// cookie row. It carries the routing Category field; we strip it when
// emitting to the widget (see widgetCookieDecl).
//
// Mirrors the widget's CookieDeclaration interface
// (CookieProof/src/core/types.ts:20) field-for-field except for Category.
type CookieDeclaration struct {
	Category string `json:"category,omitempty"`
	Name     string `json:"name"`
	Provider string `json:"provider,omitempty"`
	Purpose  string `json:"purpose,omitempty"`
	Expiry   string `json:"expiry,omitempty"`
}

// widgetCookieDecl is the shape the embed widget actually reads. Identical
// to CookieDeclaration minus the routing Category field.
type widgetCookieDecl struct {
	Name     string `json:"name"`
	Provider string `json:"provider,omitempty"`
	Purpose  string `json:"purpose,omitempty"`
	Expiry   string `json:"expiry,omitempty"`
}

// RenderCookieProofSnippet returns the HTML snippet that the build pipeline
// injects into a tenant site's <head> when CookieProof is enabled.
//
// Three parts, all same-origin and inline-configured (no third-party fetches):
//
//  1. Synchronous Google Consent Mode V2 default-denied stub. Must run before
//     any tracking script so GA/GTM see denied-by-default.
//  2. Inline config blob written to `window.__CCB__` .  the embed widget
//     reads this on load instead of fetching from a remote API.
//  3. <script defer src="/_ccb.{hash}.js"> .  the widget bundle, served from
//     the tenant's own origin. Hash comes from CookieProofWidgetHash() so
//     the file is content-addressed and immutably cacheable.
//
// Replaces the previous snippet that loaded the CookieProof loader as a
// third-party script and POSTed proofs back to a remote origin. Atomic
// Site is now standalone for cookies: tenant pages have zero third-party
// connections for the banner.
func RenderCookieProofSnippet(cfg CookieProofConfig) string {
	if cfg.SiteID == "" || cfg.Domain == "" {
		return ""
	}
	// Single same-origin script tag. The per-site bundle (written by
	// WriteCookieProofWidgetAsset) contains GCM default-denied +
	// window.__CCB__ config + widget code, in that order. CSP
	// script-src 'self' allows it; no inline-script hashes needed;
	// no race between config-set and widget-read because they're in
	// the same JS file. Verified bug 2026-05-01: emitting the config
	// inline forced 'unsafe-inline' or per-build SHA-256 hashes; both
	// were dead-end paths.
	//
	// The filename hash incorporates the prefix bytes (per-site
	// siteID + revision + branding) so a `cookie_revision` bump
	// produces a new filename and visitors with the old `_ccb.*.js`
	// cached pick up the fresh config on next page load.
	prefix, err := RenderCookieProofConfigPrefix(cfg)
	if err != nil {
		// If we can't render the prefix here, fall back to the
		// content-only filename so the script tag still resolves
		// to whatever WriteCookieProofWidgetAsset wrote.
		return fmt.Sprintf(`<script is:inline defer data-consent-platform="cookieproof" src="/%s"></script>`+"\n", CookieProofWidgetFilename(nil))
	}
	return fmt.Sprintf(`<script is:inline defer data-consent-platform="cookieproof" src="/%s"></script>`+"\n", CookieProofWidgetFilename(prefix))
}

// gcmDefaultDeniedStub is the GCM consent default that runs before any
// trackers. Same content for every site (no per-site state). Prepended
// to the per-site bundle by WriteCookieProofWidgetAsset.
const gcmDefaultDeniedStub = `(function(){window.dataLayer=window.dataLayer||[];if(typeof window.gtag!=='function'){window.gtag=function(){window.dataLayer.push(arguments);};}window.gtag('consent','default',{ad_storage:'denied',analytics_storage:'denied',ad_user_data:'denied',ad_personalization:'denied',functionality_storage:'denied',personalization_storage:'denied',security_storage:'granted',wait_for_update:2500});})();
`

// buildCookieProofEmbedConfig assembles the JSON shape the embed widget
// expects on window.__CCB__. Pure data: no scripting concerns. Used
// by both RenderCookieProofConfigPrefix (build-time bundle prefix) and
// any future debug surface that wants to inspect the per-site config.
func buildCookieProofEmbedConfig(cfg CookieProofConfig) ([]byte, error) {
	trackPath := cfg.TrackPath
	if trackPath == "" {
		trackPath = "/t"
	}
	proofEndpoint := strings.TrimRight(trackPath, "/") + "/consent"

	type embedConfig struct {
		SiteID           string           `json:"siteId"`
		Domain           string           `json:"domain"`
		ProofEndpoint    string           `json:"proofEndpoint"`
		Language         string           `json:"language,omitempty"`
		Position         string           `json:"position,omitempty"`
		Categories       []CookieCategory `json:"categories,omitempty"`
		PrivacyPolicyURL string           `json:"privacyPolicyUrl,omitempty"`
		GcmEnabled       bool             `json:"gcmEnabled"`
		RespectGPC       bool             `json:"respectGPC"`
		FloatingTrigger  any              `json:"floatingTrigger,omitempty"`
		Theme            string           `json:"theme,omitempty"`
		CookieName       string           `json:"cookieName,omitempty"`
		CookieExpiry     int              `json:"cookieExpiry,omitempty"`
		Revision         int              `json:"revision,omitempty"`
		// LanguageSelector accepts the same shape the widget does:
		// bool (true = all built-in, false = none) or []string of ISO codes.
		LanguageSelector any               `json:"languageSelector,omitempty"`
		CcpaEnabled      bool              `json:"ccpaEnabled,omitempty"`
		CcpaURL          string            `json:"ccpaUrl,omitempty"`
		CssVars          map[string]string `json:"cssVars,omitempty"`
		Copy             map[string]string `json:"copy,omitempty"`
		// CustomTranslations maps ISO language code -> partial
		// TranslationStrings (title, description, accept, reject,
		// customize). Widget merges over its built-in en/sv/de/etc.
		CustomTranslations map[string]map[string]string `json:"customTranslations,omitempty"`
	}

	theme := strings.ToLower(strings.TrimSpace(cfg.Theme))
	switch theme {
	case "light", "dark", "auto":
		// ok
	default:
		theme = "light"
	}

	// floatingTrigger: widget accepts boolean false or 'left'/'right'.
	// "off" maps to false; empty maps to "left" (legacy default).
	var floatingTrigger any
	switch strings.ToLower(strings.TrimSpace(cfg.FloatingTrigger)) {
	case "off":
		floatingTrigger = false
	case "right":
		floatingTrigger = "right"
	default:
		floatingTrigger = "left"
	}

	// LanguageSelector serialisation: prefer the explicit list when the
	// operator has narrowed the set; otherwise fall through to the on/off
	// boolean. Empty list AND false -> omit entirely so the widget falls
	// back to its built-in default (no dropdown).
	var langSel any
	if cfg.LanguageSelector && len(cfg.LanguageSelectorSet) > 0 {
		langSel = cfg.LanguageSelectorSet
	} else if cfg.LanguageSelector {
		langSel = true
	}

	out := embedConfig{
		SiteID:           cfg.SiteID,
		Domain:           cfg.Domain,
		ProofEndpoint:    proofEndpoint,
		Language:         cfg.Locale,
		Position:         normalisePosition(cfg.Position),
		Categories:       cfg.Categories,
		PrivacyPolicyURL: cfg.PrivacyPolicyURL,
		GcmEnabled:       true,
		RespectGPC:       true,
		FloatingTrigger:  floatingTrigger,
		Theme:            theme,
		CookieName:       AtomicSiteConsentCookieName,
		CookieExpiry:     cfg.CookieExpiryDays,
		Revision:         cfg.Revision,
		LanguageSelector: langSel,
		CcpaEnabled:      cfg.CcpaEnabled,
		CcpaURL:          cfg.CcpaURL,
		CssVars:          cfg.CssVars,
	}
	// Per-language overrides (analytics.cookie_translations). Trimmed of
	// empty strings + empty inner maps so the JSON stays small and the
	// widget's fallback logic doesn't see no-op overrides.
	if len(cfg.CustomTranslations) > 0 {
		clean := map[string]map[string]string{}
		for code, fields := range cfg.CustomTranslations {
			if code == "" || len(fields) == 0 {
				continue
			}
			inner := map[string]string{}
			for k, v := range fields {
				if v == "" {
					continue
				}
				inner[k] = v
			}
			if len(inner) > 0 {
				clean[code] = inner
			}
		}
		if len(clean) > 0 {
			out.CustomTranslations = clean
		}
	}
	copy := map[string]string{}
	if cfg.Title != "" {
		copy["title"] = cfg.Title
	}
	if cfg.Description != "" {
		copy["description"] = cfg.Description
	}
	if cfg.AcceptLabel != "" {
		copy["accept"] = cfg.AcceptLabel
	}
	if cfg.RejectLabel != "" {
		copy["reject"] = cfg.RejectLabel
	}
	if cfg.SettingsLabel != "" {
		copy["customize"] = cfg.SettingsLabel
	}
	if len(copy) > 0 {
		out.Copy = copy
	}
	return json.Marshal(out)
}

// RenderCookieProofConfigPrefix builds the JS prefix that gets prepended
// to the widget bundle: GCM default-denied stub + window.__CCB__ assignment.
// Returns plain JS bytes — caller writes them ahead of the bundle.
//
// Exported so the admin preview handler can stream "config + widget bundle"
// bytes directly into the iframe response without going through the
// per-tenant workspace asset writer.
func RenderCookieProofConfigPrefix(cfg CookieProofConfig) ([]byte, error) {
	cfgBytes, err := buildCookieProofEmbedConfig(cfg)
	if err != nil {
		return nil, err
	}
	var b []byte
	b = append(b, gcmDefaultDeniedStub...)
	b = append(b, []byte("window.__CCB__=")...)
	b = append(b, cfgBytes...)
	b = append(b, ';', '\n')
	return b, nil
}

// WriteCookieProofWidgetAsset writes the embedded widget bundle into the
// tenant workspace's public/ directory under the cache-busted filename
// returned by CookieProofWidgetFilename. Idempotent: overwrites any
// previous version on each build.
func WriteCookieProofWidgetAsset(workspaceDir string, cfg CookieProofConfig) error {
	publicDir := filepath.Join(workspaceDir, "public")
	if err := EnsureDir(publicDir); err != nil {
		return fmt.Errorf("ensure public dir: %w", err)
	}
	prefix, err := RenderCookieProofConfigPrefix(cfg)
	if err != nil {
		return fmt.Errorf("render config prefix: %w", err)
	}
	target := filepath.Join(publicDir, CookieProofWidgetFilename(prefix))
	return WriteFile(target, string(prefix)+string(CookieProofWidget))
}

// BuildCookieProofConfig assembles a CookieProofConfig from a site row +
// settings map + any per-site profile, picking sensible defaults so the
// caller doesn't have to know which keys map where.
func BuildCookieProofConfig(site store.Site, settingsMap map[string]string) CookieProofConfig {
	domain := strings.TrimSpace(site.CookieproofDomain)
	if domain == "" {
		domain = strings.TrimSpace(site.Domain)
	}
	cfg := CookieProofConfig{
		SiteID:    site.ID,
		Domain:    domain,
		Locale:    strings.TrimSpace(site.Lang),
		TrackPath: orDefault(settingsMap["analytics.track_path"], "/t"),
		CssVars:   cookieProofThemeFromBranding(site),
	}

	if v := strings.TrimSpace(settingsMap["analytics.cookie_banner_position"]); v != "" {
		cfg.Position = v
	}
	cfg.Title = settingsMap["analytics.cookie_banner_title"]
	cfg.Description = settingsMap["analytics.cookie_banner_description"]
	cfg.AcceptLabel = settingsMap["analytics.cookie_banner_accept"]
	cfg.RejectLabel = settingsMap["analytics.cookie_banner_reject"]
	cfg.SettingsLabel = settingsMap["analytics.cookie_banner_customize"]

	// Privacy policy URL: explicit setting wins; otherwise auto-derive a
	// locale-prefixed /<lang>/privacy URL. Starter kits seed a /privacy
	// page in every language so this default works out of the box.
	// Operators can still override via seo.privacy_policy_url.
	cfg.PrivacyPolicyURL = strings.TrimSpace(settingsMap["seo.privacy_policy_url"])
	if cfg.PrivacyPolicyURL == "" {
		lang := strings.TrimSpace(site.Lang)
		if lang == "" {
			cfg.PrivacyPolicyURL = "/privacy"
		} else {
			cfg.PrivacyPolicyURL = "/" + lang + "/privacy"
		}
	}

	cfg.Theme = settingsMap["analytics.cookie_theme"]
	cfg.FloatingTrigger = settingsMap["analytics.cookie_floating_trigger"]
	cfg.LanguageSelector = boolSetting(settingsMap["analytics.cookie_language_selector"], false)
	// cookie_languages: comma-separated list of ISO codes (e.g. "en,sv,de").
	// Empty + LanguageSelector=true -> show ALL widget-bundled locales.
	// Non-empty + LanguageSelector=true -> dropdown limited to listed codes.
	// LanguageSelector=false -> dropdown hidden regardless.
	if raw := strings.TrimSpace(settingsMap["analytics.cookie_languages"]); raw != "" {
		// Bound the input so a pathological setting value (e.g.
		// "en," repeated 10k times) can't expand the parser slice
		// indefinitely. 1024 chars covers all 13 supported codes
		// with whitespace and then some.
		if len(raw) > 1024 {
			raw = raw[:1024]
		}
		seen := map[string]bool{}
		for _, code := range strings.Split(raw, ",") {
			c := strings.ToLower(strings.TrimSpace(code))
			if c == "" || seen[c] {
				continue
			}
			seen[c] = true
			cfg.LanguageSelectorSet = append(cfg.LanguageSelectorSet, c)
			// Hard cap at the bundled-locale count. Anything above
			// is a typo or attack; widget would silently drop the
			// excess anyway.
			if len(cfg.LanguageSelectorSet) >= 32 {
				break
			}
		}
	}
	cfg.CookieExpiryDays = intSetting(settingsMap["analytics.cookie_expiry_days"], 365, 0, 365)
	cfg.Revision = intSetting(settingsMap["analytics.cookie_revision"], 0, 0, 9999)
	cfg.CcpaEnabled = boolSetting(settingsMap["analytics.ccpa_enabled"], false)
	cfg.CcpaURL = strings.TrimSpace(settingsMap["analytics.ccpa_url"])

	// cookie_translations: JSON map of {lang: {title, description, accept,
	// reject, customize}}. Per-language banner copy overrides. Widget
	// merges over its built-in translations; empty inner fields fall back
	// to the bundled string. Invalid JSON is ignored (admin UI guarantees
	// well-formed JSON, but the agent API can write anything).
	if raw := strings.TrimSpace(settingsMap["analytics.cookie_translations"]); raw != "" {
		var parsed map[string]map[string]string
		if err := json.Unmarshal([]byte(raw), &parsed); err == nil && len(parsed) > 0 {
			// Whitelist allowed inner keys so a typo or future widget
			// change can't leak unexpected strings into the bundle.
			allowed := map[string]bool{
				"title": true, "description": true,
				"accept": true, "reject": true, "customize": true,
			}
			clean := map[string]map[string]string{}
			for code, fields := range parsed {
				code = strings.ToLower(strings.TrimSpace(code))
				if code == "" {
					continue
				}
				inner := map[string]string{}
				for k, v := range fields {
					if allowed[k] && v != "" {
						inner[k] = v
					}
				}
				if len(inner) > 0 {
					clean[code] = inner
				}
			}
			if len(clean) > 0 {
				cfg.CustomTranslations = clean
			}
		}
	}

	cfg.Categories = []CookieCategory{
		{ID: "necessary", Required: true, Enabled: true},
		{ID: "analytics", Enabled: boolSetting(settingsMap["analytics.cookie_cat_analytics"], true)},
		{ID: "marketing", Enabled: boolSetting(settingsMap["analytics.cookie_cat_marketing"], true)},
		{ID: "preferences", Enabled: boolSetting(settingsMap["analytics.cookie_cat_preferences"], true)},
	}

	// Merge presets (auto-derived from enabled trackers) with the user's
	// edited list (from analytics.cookie_declarations) and assign each row
	// to its category. User entries win on (category, name) collision so
	// tenants can translate strings or override providers.
	merged := mergeDeclarations(
		PresetCookieDeclarations(site, settingsMap),
		parseUserCookieDeclarations(settingsMap["analytics.cookie_declarations"]),
	)
	assignDeclarationsToCategories(cfg.Categories, merged)

	return cfg
}

// assignDeclarationsToCategories groups merged declarations by Category and
// writes them onto the matching CookieCategory.Declarations slice. Rows
// with an unknown category are dropped silently — atomicsite's UI gates
// the input to the four built-in categories, so this is defensive only.
func assignDeclarationsToCategories(cats []CookieCategory, decls []CookieDeclaration) {
	idx := make(map[string]int, len(cats))
	for i, c := range cats {
		idx[c.ID] = i
	}
	for _, d := range decls {
		i, ok := idx[d.Category]
		if !ok {
			continue
		}
		cats[i].Declarations = append(cats[i].Declarations, widgetCookieDecl{
			Name:     d.Name,
			Provider: d.Provider,
			Purpose:  d.Purpose,
			Expiry:   d.Expiry,
		})
	}
	// reflect back via index since Go doesn't write through the range
	// variable; assigning .Declarations above already does the job because
	// CookieCategory is a struct held by index in the slice. Nothing more
	// to do here.
	_ = idx
}

// normalisePosition clamps the position string to the widget's accepted
// values. Returns "" for unknown values so the widget falls back to its own
// default ("bottom").
func normalisePosition(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "top", "bottom", "center":
		return strings.ToLower(strings.TrimSpace(p))
	default:
		return ""
	}
}
