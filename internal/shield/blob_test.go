package shield

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestShieldJSONLeavesEncodedBlobsIntact: the regex bank's phone pattern
// matches '+' followed by digits, and base64 emits that constantly. Before
// blob.go, running a base64 PNG through ShieldJSON replaced random slices of
// the image with [shield:phone:...] markers. Measured on a 200 KB PNG:
// 266668 bytes in, 266743 out, undecodable.
//
// This is the shared JSON walker, so the guard covers both the MCP screenshot
// tool and the REST agent surface's response chokepoint.
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

	sess := newBlobTestSession(t)
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

// TestShieldJSONStillRedactsLongText: the guard must not become a bypass. Long
// human text is still redacted, because it contains characters outside the
// base64/hex alphabet.
func TestShieldJSONStillRedactsLongText(t *testing.T) {
	long := strings.Repeat("Vi hade ett bra mote i veckan och foljer upp senare. ", 40) +
		"Kontakta anna@andersson-law.se for detaljer."
	body, err := json.Marshal(map[string]any{"note": long})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	sess := newBlobTestSession(t)
	out, err := sess.ShieldJSON(context.Background(), body)
	if err != nil {
		t.Fatalf("ShieldJSON: %v", err)
	}
	if strings.Contains(string(out), "anna@andersson-law.se") {
		t.Fatalf("long text bypassed the redact pass: %s", out)
	}
}

func TestLooksLikeEncodedBlob(t *testing.T) {
	long := strings.Repeat("QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVowMTIzNDU2Nzg5Kw==", 20)
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"short base64", "QUJDREVG", false},
		{"long base64", long, true},
		{"long base64url", strings.ReplaceAll(strings.ReplaceAll(long, "+", "-"), "/", "_"), true},
		{"data uri", "data:image/png;base64," + long, true},
		{"data uri without base64 marker", "data:text/plain," + long, false},
		{"long email-bearing text", strings.Repeat("a", 600) + " anna@andersson-law.se", false},
		{"long text with spaces", strings.Repeat("word ", 200), false},
		{"long text with dots", strings.Repeat("a.b", 300), false},
		{"iban", "SE4550000000058398257466", false},
		{"personnummer", "19850101-1234", false},
	}
	for _, c := range cases {
		if got := looksLikeEncodedBlob(c.in); got != c.want {
			t.Errorf("%s: looksLikeEncodedBlob = %v, want %v", c.name, got, c.want)
		}
	}
}

func newBlobTestSession(t *testing.T) *Session {
	t.Helper()
	sess, err := NewSession(context.Background(), NewMemoryStore(), "s_blob",
		[]byte("0123456789abcdef0123456789abcdef"), time.Hour, HintFull)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return sess
}
