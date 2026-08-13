package middleware

import (
	"testing"

	"github.com/getsentry/sentry-go"
)

// TestFlareScrubRemovesThisAppsCredentials is the guard the file did not have.
//
// The whole authHeaders list could be emptied and every test in this package
// still passed, which is how X-Agent-Key came to be missing from it: nothing
// asserted that any credential is removed from a Flare event. X-Agent-Key is
// the bearer credential for the entire agent REST and MCP surface, and
// sentry-go's own sensitiveHeaders does not carry it, so with
// SendDefaultPII=false the SDK copied it into every event. A panic under
// /api/agent/* shipped a live tenant key to the shared Flare store, which is a
// different trust boundary from the tenant's own site.
func TestFlareScrubRemovesThisAppsCredentials(t *testing.T) {
	const secret = "ak_live_TENANT_SECRET_0123456789"
	ev := &sentry.Event{Request: &sentry.Request{
		Headers: map[string]string{
			"X-Agent-Key":         secret,
			"X-Agent-Key-Id":      "kid_1",
			"Authorization":       "Bearer " + secret,
			"Proxy-Authorization": "Basic " + secret,
			"Cookie":              "session=" + secret,
			"X-Flare-Key":         "flare-secret",
			"Mcp-Session-Id":      "mcp-secret",
			// Non-canonical spelling: the map is the SDK's, and an exact-key
			// delete would miss this while reporting success.
			"x-api-key": secret,
			// Must survive; a scrub that removes everything proves nothing.
			"Host":       "example.com",
			"User-Agent": "curl/8",
		},
		QueryString: "token=" + secret,
		Cookies:     "session=" + secret,
		URL:         "https://example.com/api/agent/pages?token=" + secret,
	}}

	out := scrubRequestEvent(ev)

	for name, val := range out.Request.Headers {
		if val == secret || val == "Bearer "+secret || val == "Basic "+secret || val == "session="+secret {
			t.Errorf("credential survived the scrub in header %q", name)
		}
	}
	for _, gone := range []string{"X-Agent-Key", "X-Agent-Key-Id", "Authorization", "Proxy-Authorization", "Cookie", "x-api-key"} {
		if _, ok := out.Request.Headers[gone]; ok {
			t.Errorf("header %q was not removed", gone)
		}
	}
	if out.Request.QueryString != "" || out.Request.Cookies != "" {
		t.Error("QueryString and Cookies must be cleared")
	}
	if got := out.Request.URL; got != "https://example.com/api/agent/pages" {
		t.Errorf("URL query not truncated: %q", got)
	}
	// Anti-vacuity.
	if len(out.Request.Headers) != 2 {
		t.Errorf("expected exactly Host and User-Agent to survive, got %v", out.Request.Headers)
	}
}
