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
var authHeaders = []string{
	"Authorization", "Cookie", "Set-Cookie",
	"X-Api-Key", "X-API-Key", "X-Auth-Token",
	"X-Flare-Key", "X-Sentry-Auth", "Mcp-Session-Id",
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
	for _, h := range authHeaders {
		delete(event.Request.Headers, h)
	}
	if i := strings.IndexByte(event.Request.URL, '?'); i >= 0 {
		event.Request.URL = event.Request.URL[:i]
	}
	return event
}
