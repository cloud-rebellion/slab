package shield

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// This file replaces blob_test.go. The old blob.go guard skipped whole
// JSON string leaves that were 512+ chars of base64/base64url/hex
// alphabet, to stop the phone pattern's old unanchored "+<digits>"
// alternative from firing inside encoded binary (see redact.go). That
// guard branched on the CONTENT of the value: a string engineered to
// stay inside the accepted alphabet while still carrying a readable
// personnummer (dashes and digits are both in the accepted alphabet)
// sailed through completely untokenized once it crossed 512 characters,
// and the same content at any length just under 512 was correctly
// redacted. Longer was less protected than shorter, which is
// indefensible for a PII boundary.
//
// The fix is root-cause: the phone pattern's international alternative
// is now anchored so it cannot match a "+" that is glued to another
// base64-alphabet character (see the KindPhone international entry in
// redact.go). With that false-positive source closed, there is no
// remaining reason for any length- or alphabet-based exemption, so
// shieldAny (session.go) now always scans every string leaf. Every test
// in this file exercises that property.

func newTestSession(t *testing.T) *Session {
	t.Helper()
	sess, err := NewSession(context.Background(), NewMemoryStore(), "s_pii",
		[]byte("0123456789abcdef0123456789abcdef"), time.Hour, HintFull)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return sess
}

// blobFiller is a repeating run of pure base64 alphabet (letters, digits,
// '+', '='). It contains none of the characters ('@', '.', ' ') that
// would already disqualify a string from the OLD blob alphabet, so
// padding built from it reproduces the exact conditions the old guard's
// alphabet check accepted.
const blobFillerUnit = "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVowMTIzNDU2Nzg5Kw=="

func blobFiller(n int) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(blobFillerUnit)
	}
	return b.String()[:n]
}

// pnrPayload returns a string of exactly totalLen characters: base64-style
// filler, a dash, a Swedish personnummer, a dash, more filler. At 512+
// chars this is exactly the shape the old encodedBlobMinLen=512 +
// base64/base64url alphabet guard (which included '-') classified as an
// opaque blob and skipped entirely.
func pnrPayload(t *testing.T, totalLen int) string {
	t.Helper()
	const pnr = "19850101-1234"
	core := "-" + pnr + "-"
	if totalLen < len(core) {
		t.Fatalf("totalLen %d too small for core %q (%d chars)", totalLen, core, len(core))
	}
	fillerLen := totalLen - len(core)
	prefix := fillerLen / 2
	suffix := fillerLen - prefix
	payload := blobFiller(prefix) + core + blobFiller(suffix)
	if len(payload) != totalLen {
		t.Fatalf("payload length %d != requested %d", len(payload), totalLen)
	}
	return payload
}

// TestPersonnummerRedactedAtBlobLength is the headline proof: a 584-char
// payload bearing a personnummer, in the exact shape the old blob guard
// (512+ chars, base64/base64url alphabet including '-') swallowed whole.
// Run with -v to see the raw before/after payload.
func TestPersonnummerRedactedAtBlobLength(t *testing.T) {
	s := newTestSession(t)
	payload := pnrPayload(t, 584)
	if len(payload) != 584 {
		t.Fatalf("setup: payload is %d chars, want 584", len(payload))
	}
	t.Logf("BEFORE (584 chars): %s", payload)

	out, err := s.RedactString(context.Background(), payload)
	if err != nil {
		t.Fatalf("RedactString: %v", err)
	}
	t.Logf("AFTER  (%d chars): %s", len(out), out)

	if strings.Contains(out, "19850101-1234") {
		t.Fatalf("personnummer leaked through a 584-char blob-shaped payload:\n%s", out)
	}
	if !strings.Contains(out, "[shield:personnummer:tok_") {
		t.Fatalf("expected a personnummer marker in output, got:\n%s", out)
	}
}

// TestRedactionLengthMonotonic is the committed table test: the same
// PII-bearing content, padded to a spread of lengths straddling the old
// 512-char threshold, must be redacted at every length. No length may be
// less protected than a shorter one (the exact failure mode of the old
// guard, where 511 was safe and 512 was not).
func TestRedactionLengthMonotonic(t *testing.T) {
	lengths := []int{64, 129, 511, 512, 513, 584, 2048, 8192}
	s := newTestSession(t)
	for _, n := range lengths {
		n := n
		t.Run(fmt.Sprintf("len_%d", n), func(t *testing.T) {
			payload := pnrPayload(t, n)
			out, err := s.RedactString(context.Background(), payload)
			if err != nil {
				t.Fatalf("RedactString: %v", err)
			}
			if strings.Contains(out, "19850101-1234") {
				t.Fatalf("length %d: personnummer leaked:\n%s", n, out)
			}
			if !strings.Contains(out, "[shield:personnummer:tok_") {
				t.Fatalf("length %d: no personnummer marker in output:\n%s", n, out)
			}
		})
	}
}

// TestPIICorpusRedactedWhenPaddedLikeBlob is the differential-shape
// proof: personnummer, email, phone and IBAN, each embedded in 600+
// chars of blob-style filler (over the old 512-char threshold), must
// all still be tokenized. There is deliberately no "card" case: the
// regex bank has no credit-card pattern on this branch OR on base
// c6c5a8802, so card numbers were never redacted by RedactString either
// way. That is a real, separate gap (see the report), not something
// this fix regresses or repairs.
func TestPIICorpusRedactedWhenPaddedLikeBlob(t *testing.T) {
	cases := []struct {
		name   string
		needle string
		kind   string
	}{
		{"personnummer", "19850101-1234", "personnummer"},
		{"email", "anna@andersson-law.se", "email"},
		{"phone_dashed", "+46-70-123-4567", "phone"},
		{"iban", "SE3550000000054910000003", "iban"},
	}
	s := newTestSession(t)
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			core := "-" + tc.needle + "-"
			fillerLen := 620 - len(core)
			payload := blobFiller(fillerLen/2) + core + blobFiller(fillerLen-fillerLen/2)
			out, err := s.RedactString(context.Background(), payload)
			if err != nil {
				t.Fatalf("RedactString: %v", err)
			}
			if strings.Contains(out, tc.needle) {
				t.Fatalf("%s leaked through a %d-char blob-shaped payload:\n%s", tc.name, len(payload), out)
			}
			marker := "[shield:" + tc.kind + ":tok_"
			if !strings.Contains(out, marker) {
				t.Fatalf("expected marker %q in output, got:\n%s", marker, out)
			}
		})
	}
}

// TestPhoneInternationalAnchorBoundary pins the root-cause fix directly:
// the international phone alternative must not fire when its leading
// '+' is glued to another base64-alphabet character (the shape that
// corrupted real base64 payloads), but must still fire on every
// realistic real-world placement (string start, after whitespace, after
// punctuation, after a JSON-style quote).
func TestPhoneInternationalAnchorBoundary(t *testing.T) {
	s := newTestSession(t)
	cases := []struct {
		name       string
		in         string
		wantMarker bool
	}{
		{"string start", "+46701234567", true},
		{"after space", "call +46701234567 now", true},
		{"after dash", "tel-+46701234567", true},
		{"after colon", "phone:+46701234567", true},
		{"after quote-like char", "(+46701234567)", true},
		{"glued to letters", "ABCXYZ+46701234567QRSTUV", false},
		{"glued to digits", "12345+46701234567654321", false},
		{"glued to base64 symbol +", "++46701234567", false},
		{"glued to base64 symbol /", "/+46701234567", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := s.RedactString(context.Background(), tc.in)
			if err != nil {
				t.Fatalf("RedactString: %v", err)
			}
			got := strings.Contains(out, "[shield:phone:tok_")
			if got != tc.wantMarker {
				t.Fatalf("in=%q: marker present=%v, want %v (out=%q)", tc.in, got, tc.wantMarker, out)
			}
		})
	}
}

// TestShieldJSONLeavesEncodedBlobsIntact: carried over from blob_test.go
// unchanged. Before the phone-anchor fix, running a base64 PNG through
// ShieldJSON replaced random slices of the image with
// [shield:phone:...] markers (measured on a 200 KB PNG: 266668 bytes in,
// 266743 out, undecodable), because the phone pattern matched '+'
// followed by digits with no boundary requirement, and base64 output
// contains that constantly. This is the shared JSON walker, so the fix
// covers both the MCP screenshot tool and the REST agent surface's
// response chokepoint.
func TestShieldJSONLeavesEncodedBlobsIntact(t *testing.T) {
	raw := make([]byte, 128*1024)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	b64 := base64.StdEncoding.EncodeToString(raw)
	body, err := json.Marshal(map[string]any{"image_base64": b64, "format": "png"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	sess := newTestSession(t)
	out, err := sess.ShieldJSON(context.Background(), body)
	if err != nil {
		t.Fatalf("ShieldJSON: %v", err)
	}
	var got struct {
		ImageBase64 string `json:"image_base64"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ImageBase64 != b64 {
		t.Fatalf("base64 corrupted: %d bytes in, %d out", len(b64), len(got.ImageBase64))
	}
}

// TestShieldJSONStillRedactsLongText: carried over from blob_test.go
// unchanged. Long human text must still be redacted.
func TestShieldJSONStillRedactsLongText(t *testing.T) {
	long := strings.Repeat("Vi hade ett bra mote i veckan och foljer upp senare. ", 40) +
		"Kontakta anna@andersson-law.se for detaljer."
	body, err := json.Marshal(map[string]any{"note": long})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	sess := newTestSession(t)
	out, err := sess.ShieldJSON(context.Background(), body)
	if err != nil {
		t.Fatalf("ShieldJSON: %v", err)
	}
	if strings.Contains(string(out), "anna@andersson-law.se") {
		t.Fatalf("long text bypassed the redact pass: %s", out)
	}
}

// TestEncodedBlobMonteCarlo empirically validates the phone-anchor fix
// rather than trusting a single 128 KB sample: many independently random
// base64 payloads, across a spread of sizes, must all round-trip through
// ShieldJSON byte-for-byte. This is the check that would have caught the
// original bug (the estate's own post-mortem on the same bug class
// warned that a single sample "passed by luck"; see the
// pii-regexes-over-a-base64-attachment Mesh gotcha).
func TestEncodedBlobMonteCarlo(t *testing.T) {
	if testing.Short() {
		t.Skip("monte carlo, skipped in -short")
	}
	sess := newTestSession(t)
	sizes := []int{64, 512, 2048, 20000, 100000}
	trialsPerSize := 25
	total := 0
	for _, n := range sizes {
		for i := 0; i < trialsPerSize; i++ {
			total++
			raw := make([]byte, n)
			if _, err := rand.Read(raw); err != nil {
				t.Fatalf("rand: %v", err)
			}
			b64 := base64.StdEncoding.EncodeToString(raw)
			body, err := json.Marshal(map[string]any{"blob": b64})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			out, err := sess.ShieldJSON(context.Background(), body)
			if err != nil {
				t.Fatalf("size=%d trial=%d: ShieldJSON: %v", n, i, err)
			}
			var got struct {
				Blob string `json:"blob"`
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("size=%d trial=%d: unmarshal: %v", n, i, err)
			}
			if got.Blob != b64 {
				t.Fatalf("size=%d trial=%d: base64 corrupted: %d bytes in, %d out\nin:  %s\nout: %s",
					n, i, len(b64), len(got.Blob), b64, got.Blob)
			}
		}
	}
	t.Logf("monte carlo: %d trials across sizes %v, zero corruption", total, sizes)
}
