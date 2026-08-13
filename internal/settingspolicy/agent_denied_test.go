package settingspolicy

import "testing"

// TestAnalyticsSaltIsSecret pins the fingerprint pseudonymisation key.
//
// It is not a provider credential, so neither the explicit allowlist nor the
// "_secret/_api_key/_token" suffix heuristic reached it, and it is written into
// site_settings by internal code (analytics/manager.go) rather than declared in
// the agent catalog, so it also had no validator. That combination made it
// readable AND writable at the lowest trust tier in the product while every
// surface reported the settings policy as enforced.
//
// analytics/parser.go computes fingerprint(ip, ua, accept_language, salt), so
// the value is what stands between the visit_sessions fingerprint column and
// offline de-pseudonymisation of every visitor the site has recorded.
func TestAnalyticsSaltIsSecret(t *testing.T) {
	if !IsSecretValue("analytics", "salt") {
		t.Error("analytics.salt must be secret: it is the visitor-fingerprint pseudonymisation key")
	}
	if AgentWritableSetting("analytics", "salt") {
		t.Error("analytics.salt must not be agent-writable: a chosen salt makes the whole future dataset precomputable")
	}
	if MaskValue("analytics", "salt", "9f2c1a7e") != RedactedValue {
		t.Error("analytics.salt must mask on read")
	}
	// The suffix half must stand on its own, so the next key of this shape is
	// covered before anyone remembers the allowlist.
	for _, k := range []string{"fingerprint_salt", "signing_key", "vendor_credential"} {
		if !IsSecretValue("analytics", k) {
			t.Errorf("analytics.%s should be caught by the suffix heuristic", k)
		}
	}
}

// TestAgentDeniedKeysAreRefused covers settings that are not secrets but whose
// value becomes executable or navigable output on every published page.
func TestAgentDeniedKeysAreRefused(t *testing.T) {
	for _, k := range []string{"cookie_banner_snippet", "admin_base_url"} {
		if AgentWritableSetting("analytics", k) {
			t.Errorf("analytics.%s must not be agent-writable: it reaches <head> or the hydration endpoint on every published page", k)
		}
	}
	// Anti-vacuity: the category must still be writable for ordinary keys, or
	// the assertions above would pass because everything is denied.
	if !AgentWritableSetting("analytics", "personalization_enabled") {
		t.Fatal("analytics.personalization_enabled should stay agent-writable; the denial is per-key, not per-category")
	}
}
