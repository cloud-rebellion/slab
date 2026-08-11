package middleware

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

// TestLogShipScrubsCredentialsOutOfAttrs pins the leak the KEY-based redactor
// cannot see.
//
// "error" is not a sensitive key and must not become one, but the value under it
// is routinely a *url.Error whose exported URL field carries the endpoint and its
// query string. slog.Error("...", "error", err) is the single most common line in
// the estate, and it shipped webhook credentials into the shared Flare logs
// store, which is a different trust boundary from the process that logged them.
func TestLogShipScrubsCredentialsOutOfAttrs(t *testing.T) {
	u := "https://hooks.partner.example.com/deliver?token=s3cr3t-webhook-token"
	err := &url.Error{Op: "Post", URL: u, Err: errStub{}}

	raw, merr := json.Marshal(map[string]any{"error": err})
	if merr != nil {
		t.Fatalf("marshal: %v", merr)
	}
	if !strings.Contains(string(raw), "s3cr3t-webhook-token") {
		t.Fatal("precondition: the marshalled attr must carry the secret, else this proves nothing")
	}

	out := string(scrubLogJSON(raw))
	if strings.Contains(out, "s3cr3t-webhook-token") {
		t.Errorf("the token still ships: %s", out)
	}
	// Redaction, not destruction: an operator must still see which call failed.
	if !strings.Contains(out, "hooks.partner.example.com") {
		t.Errorf("the endpoint host was destroyed, leaving the log useless: %s", out)
	}
	if !json.Valid([]byte(out)) {
		t.Errorf("scrubbed attrs are not valid JSON, so the whole record is unshippable: %s", out)
	}
}

// TestLogShipScrubsTheMessageBody covers the other half. The record message is
// free text an author formats by hand, so it carries the same URLs.
func TestLogShipScrubsTheMessageBody(t *testing.T) {
	body := scrubLogText("POST https://api.example.com/v1/send?api_key=live_EXAMPLE_NOT_A_REAL_KEY failed")
	if strings.Contains(body, "live_EXAMPLE_NOT_A_REAL_KEY") {
		t.Errorf("the api key still ships in the body: %s", body)
	}
	if !strings.Contains(body, "api.example.com") {
		t.Errorf("the host was destroyed: %s", body)
	}
}

// TestLogShipScrubDoesNotCorruptNumbers is the trap this codebase already fell
// into once: running a text scrub over serialized JSON rewrites UNQUOTED numeric
// values, and a large integer decoded through float64 comes back rounded.
func TestLogShipScrubDoesNotCorruptNumbers(t *testing.T) {
	raw := []byte(`{"request_id":1712345678901234567,"count":42,"ratio":0.5}`)
	out := scrubLogJSON(raw)
	if !json.Valid(out) {
		t.Fatalf("not valid JSON after scrub: %s", out)
	}
	for _, want := range []string{"1712345678901234567", "42", "0.5"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("numeric value %s was rewritten or rounded: %s", want, out)
		}
	}
}

// TestLogShipScrubKeepsOrdinaryValues keeps the scrub honest. Over-redaction on
// an observability path is its own outage: the operator loses the log.
func TestLogShipScrubKeepsOrdinaryValues(t *testing.T) {
	raw := []byte(`{"path":"/api/v1/users","status":"ok","duration":"1.2s","user":"alice@example.com"}`)
	out := string(scrubLogJSON(raw))
	for _, want := range []string{"/api/v1/users", "ok", "1.2s", "alice@example.com"} {
		if !strings.Contains(out, want) {
			t.Errorf("ordinary value %q was redacted: %s", want, out)
		}
	}
}

type errStub struct{}

func (errStub) Error() string { return "connection refused" }
