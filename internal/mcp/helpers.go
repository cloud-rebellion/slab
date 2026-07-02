package mcp

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// newID mirrors handlers.newID() so the MCP package doesn't import the
// handlers package (which would create a cycle: handlers imports the mcp
// server in server.go via the route wiring).
func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// agentWritableCategories mirrors handlers.agentWritableSettingsCategories.
// Duplicated intentionally rather than imported: settings policy is small
// + stable, and a one-source-of-truth helper would have to live in a
// shared package neither handlers nor mcp reaches into today.
// Keep in sync with the REST mirror AND the bulk_upsert_settings schema
// enum in tools.go.
var agentWritableCategories = map[string]bool{
	"general":   true,
	"seo":       true,
	"analytics": true,
	"search":    true,
	"design":    true,
}

func isAgentWritableCategory(category string) bool {
	return agentWritableCategories[category]
}

// validateSettingValue mirrors handlers.validateSetting. Same boundaries:
// enum guards, range checks, format checks. Empty string passes (clears
// the row, matches admin UI behaviour).
//
// Kept in sync with internal/handlers/settings_validate.go. When you add
// a validator there, mirror it here so MCP writes are gated identically.
func validateSettingValue(category, key, value string) error {
	if value == "" {
		return nil
	}
	switch category {
	case "general":
		return validateGeneral(key, value)
	case "seo":
		return validateSEO(key, value)
	case "analytics":
		return validateAnalytics(key, value)
	case "design":
		return validateDesign(key, value)
	}
	return nil
}

func validateDesign(key, value string) error {
	switch key {
	case "fidelity":
		return enumIn("design.fidelity", value, "performance", "balanced", "showcase")
	case "strict_lint":
		return boolStr("design.strict_lint", value)
	}
	return nil
}

func validateGeneral(key, value string) error {
	switch key {
	case "additional_langs", "domain_aliases":
		for _, raw := range strings.Split(value, ",") {
			if strings.TrimSpace(raw) == "" {
				return fmt.Errorf("general.%s contains an empty entry; check the comma list", key)
			}
		}
	}
	return nil
}

func validateSEO(key, value string) error {
	switch key {
	case "hreflang_strategy":
		return enumIn("seo.hreflang_strategy", value, "path", "subdomain", "off")
	case "canonical_base":
		return absURL("seo.canonical_base", value)
	case "sitemap_enabled":
		return boolStr("seo.sitemap_enabled", value)
	}
	return nil
}

func validateAnalytics(key, value string) error {
	switch key {
	case "cookieproof_enabled",
		"atomicsite_tracking_enabled",
		"ga4_enabled",
		"umami_enabled",
		"personalization_enabled":
		return boolStr("analytics."+key, value)
	case "ga4_id":
		if !strings.HasPrefix(value, "G-") || len(value) < 4 {
			return fmt.Errorf("analytics.ga4_id must look like G-XXXXXXX")
		}
	case "umami_url", "crm_webhook_url":
		return absURL("analytics."+key, value)
	case "identity_max_age_days":
		return intRange("analytics.identity_max_age_days", value, 1, 3650)
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

func boolStr(field, value string) error {
	switch value {
	case "0", "1", "true", "false", "yes", "no", "on", "off":
		return nil
	}
	return fmt.Errorf("%s must be 0 or 1", field)
}

func intRange(field, value string, min, max int64) error {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fmt.Errorf("%s must be an integer", field)
	}
	if n < min || n > max {
		return fmt.Errorf("%s must be between %d and %d (got %d)", field, min, max, n)
	}
	return nil
}

func absURL(field, value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("%s must be an absolute URL with scheme and host", field)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s must use http or https", field)
	}
	return nil
}
