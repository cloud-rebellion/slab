package settingspolicy

import "testing"

// TestAgentWritableSetting_RefusesSecretKeyInWritableCategory is the
// behavioural half of the fix.
//
// The three settings write surfaces shared an envelope shape but not the
// authorization predicate. The human REST path gated on
// PrivilegedCategory || IsSecretValue, while the agent REST path and the
// MCP tool gated on the category alone. analytics is agent-writable and
// analytics.crm_webhook_secret is a secret, so both agent paths MASKED the
// key on read and ACCEPTED it on write: an agent could overwrite a
// credential it was not allowed to see.
func TestAgentWritableSetting_RefusesSecretKeyInWritableCategory(t *testing.T) {
	cases := []struct {
		name     string
		category string
		key      string
		want     bool
		why      string
	}{
		{
			name:     "secret key inside a writable category is refused",
			category: "analytics", key: "crm_webhook_secret", want: false,
			why: "bearer-equivalent; masked on read by both agent surfaces, so accepting the write let an agent destroy or hijack a credential it could not read",
		},
		{
			name:     "ordinary key in the same category still allowed",
			category: "analytics", key: "umami_url", want: true,
			why: "wiring up tracking is exactly what the agent surface is for; the fix must not turn into a blanket category ban",
		},
		{
			name:     "non-secret key in another writable category still allowed",
			category: "seo", key: "meta_description", want: true,
			why: "regression guard: the key half must not leak into unrelated categories",
		},
		{
			name:     "privileged category refused regardless of key",
			category: "security", key: "csp_policy", want: false,
			why: "security was never agent-writable and must stay that way",
		},
		{
			name:     "payments refused regardless of key",
			category: "payments", key: "mollie_test_mode", want: false,
			why: "the whole category moves the tenant's money; not agent-writable even for its non-secret key",
		},
		{
			name:     "unknown category refused",
			category: "nonsense", key: "whatever", want: false,
			why: "fail closed on anything not explicitly enumerated",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AgentWritableSetting(tc.category, tc.key); got != tc.want {
				t.Errorf("AgentWritableSetting(%q, %q) = %v, want %v\nwhy this case exists: %s",
					tc.category, tc.key, got, tc.want, tc.why)
			}
		})
	}
}

// TestAgentWritableSetting_IsStrictlyNarrowerThanCategory asserts the new
// predicate never ALLOWS something the category rule refused. The fix is
// meant to be a narrowing only; if this ever fails, the two rules have
// diverged in the dangerous direction.
func TestAgentWritableSetting_IsStrictlyNarrowerThanCategory(t *testing.T) {
	keys := []string{
		"crm_webhook_secret", "crm_webhook_url", "umami_url", "meta_description",
		"api_key", "token", "secret", "password", "mollie_api_key", "",
	}
	for _, cat := range AgentWritableCategories() {
		for _, k := range keys {
			if AgentWritableSetting(cat, k) && !AgentWritableCategory(cat) {
				t.Errorf("AgentWritableSetting(%q, %q) allowed a pair the category rule refused", cat, k)
			}
		}
	}
	// And the refusal is real for at least one pair, so this test cannot
	// pass vacuously by the predicate simply mirroring the category rule.
	if AgentWritableSetting("analytics", "crm_webhook_secret") {
		t.Fatal("expected analytics.crm_webhook_secret to be refused; the narrowing is not in effect")
	}
	if !AgentWritableCategory("analytics") {
		t.Fatal("expected analytics to be an agent-writable CATEGORY; without that the previous assertion proves nothing")
	}
}
