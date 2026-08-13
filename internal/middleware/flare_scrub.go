package middleware

import (
	"strings"

	sentry "github.com/getsentry/sentry-go"
)

// authHeaders are the request headers whose value is a credential.
//
// X-Flare-Key and X-Sentry-Auth are on the list because they are how Flare's
// own ingest endpoint is authenticated, and sentry-go's built-in
// sensitiveHeaders set contains neither.
// X-Agent-Key is THIS APP'S bearer credential for the entire agent REST and MCP
// surface (middleware/agent_auth.go), and it was missing. sentry-go's own
// sensitiveHeaders does not carry it either, and with SendDefaultPII=false the
// SDK copies every header NOT on its list into the event, so the app's own
// credential passed both filters. Any panic under /api/agent/* shipped a live
// tenant agent key to the Flare event store, which is a different trust
// boundary from the tenant's site: recovering it is full write access to that
// tenant's content and settings.
//
// The lesson generalises: this list must be derived from what THIS app treats
// as a credential, never from what an upstream SDK happens to know about.
var authHeaders = []string{
	"Authorization", "Cookie", "Set-Cookie",
	"X-Api-Key", "X-API-Key", "X-Auth-Token",
	"X-Flare-Key", "X-Sentry-Auth", "Mcp-Session-Id",
	"X-Agent-Key", "X-Agent-Key-Id", "Proxy-Authorization",
}

// scrubRequestEvent strips request context that must not reach the shared Flare
// event store: the query string, the cookies, and auth headers. Method and path
// are kept, so triage still knows what was being called.
//
// SendDefaultPII does NOT cover the query string. sentry-go assigns
// Request.QueryString from r.URL.RawQuery unconditionally, outside that branch,
// so an OAuth code, a password-reset token or an API key in a query parameter
// egressed on every panic and every traced request regardless of the setting.
//
// Registered on BOTH hooks. Transaction envelopes route through
// BeforeSendTransaction, a separate hook, so wiring only BeforeSend leaves every
// traced request unscrubbed.
func scrubRequestEvent(event *sentry.Event) *sentry.Event {
	if event == nil || event.Request == nil {
		return event
	}
	event.Request.QueryString = ""
	event.Request.Cookies = ""
	// Case-insensitive, not delete-by-exact-key. The map is populated by the
	// SDK from http.Header, whose keys are canonical today, so an exact delete
	// happens to work; it makes correctness depend on a detail of an upstream
	// library rather than on this list. One non-canonical key and the credential
	// is shipped, silently, with the guard reporting nothing.
	for name := range event.Request.Headers {
		for _, h := range authHeaders {
			if strings.EqualFold(name, h) {
				delete(event.Request.Headers, name)
				break
			}
		}
	}
	if i := strings.IndexByte(event.Request.URL, '?'); i >= 0 {
		event.Request.URL = event.Request.URL[:i]
	}
	return event
}
