package handlers

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// validateSetting checks one {category, key, value} write against the
// constraints documented in the agent SettingsCatalog. Returns nil when
// the value is acceptable, or a user-facing error string when it isn't.
//
// Empty values are always accepted (they clear the row). Validation only
// fires on non-empty input. This matches the admin UI's behaviour: typing
// nothing into a field is the right way to revert to a default, and the
// pre-existing settings system has always treated "" as "unset".
//
// Validators here mirror the catalog in
// internal/agent/settings_catalog.go. When you add a new setting there,
// add the matching validator here so admin + agent writes both gate.
func validateSetting(category, key, value string) error {
	if value == "" {
		return nil
	}
	switch category {
	case "general":
		return validateGeneralSetting(key, value)
	case "seo":
		return validateSEOSetting(key, value)
	case "analytics":
		return validateAnalyticsSetting(key, value)
	case "security":
		return validateSecuritySetting(key, value)
	case "design":
		return validateDesignSetting(key, value)
	}
	// Unknown categories pass through; future-compat for whatever the next
	// phase introduces. The handler-level routing already guards write
	// access (agent_settings.go.agentWritableSettingsCategories).
	return nil
}

func validateDesignSetting(key, value string) error {
	switch key {
	case "fidelity":
		// A typo'd fidelity must never silently select a no-op profile.
		return enumIn("design.fidelity", value, "performance", "balanced", "showcase")
	case "strict_lint":
		return boolValue("design.strict_lint", value)
	}
	return nil
}

func validateGeneralSetting(key, value string) error {
	switch key {
	case "additional_langs", "domain_aliases":
		// CSV; reject empty entries between commas to surface typos.
		for _, raw := range strings.Split(value, ",") {
			if strings.TrimSpace(raw) == "" {
				return fmt.Errorf("general.%s contains an empty entry; check the comma list", key)
			}
		}
	}
	return nil
}

func validateSEOSetting(key, value string) error {
	switch key {
	case "hreflang_strategy":
		return enumIn("seo.hreflang_strategy", value, "path", "subdomain", "off")
	case "canonical_base":
		return mustBeAbsoluteURL("seo.canonical_base", value)
	case "sitemap_enabled":
		return boolValue("seo.sitemap_enabled", value)
	}
	return nil
}

func validateAnalyticsSetting(key, value string) error {
	switch key {
	case "cookieproof_enabled",
		"atomicsite_tracking_enabled",
		"ga4_enabled",
		"umami_enabled",
		"personalization_enabled",
		"cookie_language_selector",
		"ccpa_enabled":
		return boolValue("analytics."+key, value)
	case "ga4_id":
		// G-XXXXXXX. Permissive: accept any G- prefix followed by alnum.
		if !strings.HasPrefix(value, "G-") || len(value) < 4 {
			return fmt.Errorf("analytics.ga4_id must look like G-XXXXXXX")
		}
	case "umami_url", "crm_webhook_url":
		return mustBeAbsoluteURL("analytics."+key, value)
	case "identity_max_age_days":
		return intInRange("analytics.identity_max_age_days", value, 1, 3650)
	case "cookie_banner_position":
		return enumIn("analytics.cookie_banner_position", value, "top", "bottom", "center")
	case "cookie_theme":
		return enumIn("analytics.cookie_theme", value, "light", "dark", "auto")
	case "cookie_floating_trigger":
		return enumIn("analytics.cookie_floating_trigger", value, "left", "right", "off")
	case "cookie_expiry_days":
		// IMY caps consent records at 12 months; 0 means widget default (365).
		return intInRange("analytics.cookie_expiry_days", value, 0, 365)
	case "cookie_revision":
		return intInRange("analytics.cookie_revision", value, 0, 9999)
	case "ccpa_url":
		// Either an absolute https URL or an in-site path starting with /.
		if strings.HasPrefix(value, "/") {
			return nil
		}
		return mustBeAbsoluteURL("analytics.ccpa_url", value)
	case "track_path":
		// Must be a same-origin rooted path (e.g. "/t"). Reject absolute
		// URLs, protocol-relative URLs, and unrooted strings to keep
		// proofs same-origin (the widget POSTs to <currentOrigin>+trackPath).
		if !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") {
			return fmt.Errorf("analytics.track_path must start with a single \"/\" (e.g. /t)")
		}
		if strings.Contains(value, "://") {
			return fmt.Errorf("analytics.track_path must be a relative path, not an absolute URL")
		}
	case "cookie_declarations":
		return validateCookieDeclarationsJSON(value)
	}
	return nil
}

// validateCookieDeclarationsJSON parses analytics.cookie_declarations and
// gates it against the per-row constraints the build pipeline + admin UI
// rely on. Empty/blank passes (clears the row). Malformed JSON, unknown
// categories, empty names, oversized fields, and overflowing row counts
// all fail. Mirrors the shape in builder/cookie_declarations.go.
func validateCookieDeclarationsJSON(value string) error {
	v := strings.TrimSpace(value)
	if v == "" {
		return nil
	}
	var rows []struct {
		Category string `json:"category"`
		Name     string `json:"name"`
		Provider string `json:"provider"`
		Purpose  string `json:"purpose"`
		Expiry   string `json:"expiry"`
	}
	if err := json.Unmarshal([]byte(v), &rows); err != nil {
		return fmt.Errorf("analytics.cookie_declarations: invalid JSON: %v", err)
	}
	if len(rows) > 100 {
		return fmt.Errorf("analytics.cookie_declarations: at most 100 rows (got %d)", len(rows))
	}
	allowedCategory := map[string]bool{
		"necessary": true, "analytics": true, "marketing": true, "preferences": true,
	}
	for i, r := range rows {
		if !allowedCategory[r.Category] {
			return fmt.Errorf("analytics.cookie_declarations[%d]: category %q is not one of necessary/analytics/marketing/preferences", i, r.Category)
		}
		if strings.TrimSpace(r.Name) == "" {
			return fmt.Errorf("analytics.cookie_declarations[%d]: name is required", i)
		}
		if len(r.Name) > 200 || len(r.Provider) > 200 || len(r.Purpose) > 500 || len(r.Expiry) > 80 {
			return fmt.Errorf("analytics.cookie_declarations[%d]: field exceeds size limit (name/provider 200, purpose 500, expiry 80)", i)
		}
	}
	return nil
}

func validateSecuritySetting(key, value string) error {
	// Every security setting eventually lands in an HTTP response header
	// (or nginx add_header line). A CR/LF in any of these values would
	// inject a separate header into the live site, so reject up front
	// before the catalog-specific rules below.
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("security.%s contains CR/LF; control characters are not allowed in security settings", key)
	}
	switch key {
	case "hsts_enabled",
		"hsts_preload",
		"csp_enabled",
		"x_content_type_options",
		"https_redirect":
		return boolValue("security."+key, value)
	case "hsts_max_age":
		return intInRange("security.hsts_max_age", value, 0, 63072000)
	case "x_frame_options":
		return enumIn("security.x_frame_options", value, "DENY", "SAMEORIGIN")
	case "referrer_policy":
		return enumIn("security.referrer_policy", value,
			"no-referrer", "no-referrer-when-downgrade", "origin",
			"origin-when-cross-origin", "same-origin", "strict-origin",
			"strict-origin-when-cross-origin", "unsafe-url")
	case "coop":
		return enumIn("security.coop", value, "same-origin", "same-origin-allow-popups", "unsafe-none")
	case "corp":
		return enumIn("security.corp", value, "same-origin", "same-site", "cross-origin")
	case "coep":
		return enumIn("security.coep", value, "require-corp", "credentialless")
	case "x_permitted_cross_domain_policies":
		return enumIn("security.x_permitted_cross_domain_policies", value,
			"none", "master-only", "by-content-type", "all")
	case "x_xss_protection":
		// Modern browsers ignore this header; we still emit a value for
		// scanner parity. Allow the two values that grade well; reject
		// anything else so admins can't smuggle arbitrary text into the
		// header.
		return enumIn("security.x_xss_protection", value, "0", "1", "1; mode=block")
	}
	return nil
}

func enumIn(field, value string, allowed ...string) error {
	for _, a := range allowed {
		if value == a {
			return nil
		}
	}
	return fmt.Errorf("%s must be one of: %s", field, strings.Join(allowed, ", "))
}

func boolValue(field, value string) error {
	switch value {
	case "0", "1", "true", "false", "yes", "no", "on", "off":
		return nil
	}
	return fmt.Errorf("%s must be 0 or 1", field)
}

func intInRange(field, value string, min, max int64) error {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fmt.Errorf("%s must be an integer", field)
	}
	if n < min || n > max {
		return fmt.Errorf("%s must be between %d and %d (got %d)", field, min, max, n)
	}
	return nil
}

func mustBeAbsoluteURL(field, value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("%s must be an absolute URL with scheme and host", field)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s must use http or https", field)
	}
	return nil
}
