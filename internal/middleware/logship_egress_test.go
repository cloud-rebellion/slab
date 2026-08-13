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
//
// The fixture key says out loud that it is synthetic, and that is load bearing.
// This file ships to a public mirror and the publish gate runs gitleaks over the
// filtered clone, so a fixture that merely looks like a live key refuses the
// publish over the product's own test data. It cost five mirrors exactly that.
// An EXAMPLE marker is skipped by the stopword list gitleaks' generic rules
// honour, which is a content check, so it survives a rename or a move in a way a
// path exclusion does not.
//
// The const NAME is load bearing for a SECOND and completely separate reason,
// and it is the one the previous version of this fixture got wrong.
// mirror-secret-preflight.sh does not look for key-shaped VALUES at all. It
// looks for the assignment SHAPE: (api_key|secret|password|bearer|private_key),
// then =, then a QUOTE, then 16+ key characters. The fixture this replaced was a
// URL, ?api_key=live_..., which has no quote after the = and so never matched
// that shape. Naming the const apiKey and quoting the value next to it built the
// shape by hand. Because the preflight runs BEFORE gitleaks in every split
// script, that did not trade one refusal for another: it moved the refusal
// EARLIER, and five products went from publishing to refusing over a fixture
// that had just been made safer.
//
// So the name must carry no credential keyword. Do not rename it back to apiKey,
// nor to secret, password, bearer or privateKey. The value is free to look like
// a key; the thing to the LEFT of the = is what the gate reads.
//
// It does not weaken the test either. scrubLogQuery keys on the PARAMETER NAME,
// ?api_key=, and never on the shape of the value, so a value that reads as
// obviously fake drives the identical code path. The proof is the ablation: take
// api_key out of scrubLogQuery and scrubLogAssign and this test goes red.
func TestLogShipScrubsTheMessageBody(t *testing.T) {
	// One const for both the fixture and the assertion. Spelled out twice, an
	// edit can change the URL and leave the Contains check hunting a string
	// nothing emits any more, which passes green while proving nothing.
	const plantedValue = "live_EXAMPLE_NOT_A_REAL_KEY"

	body := scrubLogText("POST https://api.example.com/v1/send?api_key=" + plantedValue + " failed")
	if strings.Contains(body, plantedValue) {
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
