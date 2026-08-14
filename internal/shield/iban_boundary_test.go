package shield

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// corruptingRun is the exact substring, verbatim from the diffed
// production payload, that the old IBAN pattern tokenized inside a
// base64 blob:
//
//	IN   ...ISBDXW/SM36ZXUGY4Q2NA9/0EKx3Qm...
//	OUT  ...ISBDXW/[shield:iban:tok_7c96b09a0a8d:country=SM]/0EKx3Qm...
//
// It is two letters, two digits and eleven more uppercase-or-digit
// characters between two '/' characters. '/' is legal base64 and is not
// a word character, so \b holds on both sides and RE2 reads the run as a
// delimited token. Per the estate's own gotcha on this bug class, the
// fixture is CRAFTED rather than harvested from random bytes: dense
// base64 does not reliably contain the triggering sequence, so a test
// built from random data alone stays green through the ablation and
// proves nothing.
const corruptingRun = "/SM36ZXUGY4Q2NA9/"

// measuredBase64FalsePositives are the runs the old pattern actually
// fired on across 133 MB of random standard-base64 and base64url, with
// the surrounding context that made each one a \b-delimited match. The
// base64url entries sit between '-' characters, which is why a boundary
// denylist would have had to name '-' as well as '+' and '/'.
var measuredBase64FalsePositives = []string{
	"dX2cXQP/QO41RR0SDH4XGD+aMRWPXM",
	"zNtOzIe+IM876E0U2EQKCL2RE9P+tY9hBUN",
	"8Dl-v9Q-ZH06L48RRHCUL2-4uuqQVq",
	"Oy5mLq--FF92102IY8F5FV5-l2R017W",
	"yV8sVCq-YN80J9R2JEI6K6BJN--boPp9S",
}

// realIBANs carry genuine ISO 7064 MOD 97-10 check digits, verified
// against an independent implementation rather than against validIBAN
// itself. Both the compact form and the ISO 13616 print format are
// listed because a human pastes the spaced one and a database stores the
// compact one, and the pattern must tokenize both.
var realIBANs = []struct {
	name    string
	value   string
	country string
}{
	{"SE compact", "SE4550000000058398257466", "SE"},
	{"SE spaced", "SE45 5000 0000 0583 9825 7466", "SE"},
	{"DE compact", "DE89370400440532013000", "DE"},
	{"DE spaced", "DE89 3704 0044 0532 0130 00", "DE"},
	{"GB compact", "GB29NWBK60161331926819", "GB"},
	{"GB spaced", "GB29 NWBK 6016 1331 9268 19", "GB"},
	{"FR compact", "FR1420041010050500013M02606", "FR"},
	{"FR spaced", "FR14 2004 1010 0505 0001 3M02 606", "FR"},
	{"NL compact", "NL91ABNA0417164300", "NL"},
	{"NL spaced", "NL91 ABNA 0417 1643 00", "NL"},
	{"NO shortest registered", "NO9386011117947", "NO"},
	{"MT longest common", "MT84MALT011000012345MTLCAST001S", "MT"},
	{"ES compact", "ES9121000418450200051332", "ES"},
	{"CH compact", "CH9300762011623852957", "CH"},
	{"IT with letter in BBAN", "IT60X0542811101000000123456", "IT"},
}

// TestRealIBANsStillTokenized is the test that stops this fix being a
// silent detection loss. Every value here is a real IBAN and must come
// back as a marker, in the middle of a sentence and standing alone.
func TestRealIBANsStillTokenized(t *testing.T) {
	s := newTestSession(t)
	for _, tc := range realIBANs {
		t.Run(tc.name, func(t *testing.T) {
			for _, tmpl := range []string{"%s", "Betala till %s tack.", "\"iban\":\"%s\""} {
				in := strings.Replace(tmpl, "%s", tc.value, 1)
				out, err := s.RedactString(context.Background(), in)
				if err != nil {
					t.Fatalf("RedactString(%q): %v", in, err)
				}
				if strings.Contains(out, tc.value) {
					t.Fatalf("IBAN %q leaked verbatim through RedactString: %q", tc.value, out)
				}
				if !strings.Contains(out, "[shield:iban:tok_") {
					t.Fatalf("in=%q: no iban marker, got %q", in, out)
				}
				if !strings.Contains(out, "country="+tc.country) {
					t.Fatalf("in=%q: country hint lost, got %q", in, out)
				}
			}
		})
	}
}

// TestIBANBase64RunNotTokenized is the regression test for the reported
// defect: the exact corrupting payload must round-trip byte-identical
// through RedactString.
func TestIBANBase64RunNotTokenized(t *testing.T) {
	s := newTestSession(t)
	cases := append([]string{
		corruptingRun,
		"ISBDXW" + corruptingRun + "0EKx3Qm",
	}, measuredBase64FalsePositives...)
	for _, in := range cases {
		out, err := s.RedactString(context.Background(), in)
		if err != nil {
			t.Fatalf("RedactString(%q): %v", in, err)
		}
		if out != in {
			t.Fatalf("base64 run corrupted:\n in:  %q\n out: %q", in, out)
		}
	}
}

// TestShieldJSONImageBase64SurvivesCorruptingRun is the end-to-end guard
// at the surface that actually corrupts in production: preview_screenshot
// returns image_base64, and ShieldJSON walks every string node of every
// MCP tool result. The blob is real random base64 with the crafted
// trigger overwritten into it, so it stays decodable and the test can
// assert both byte identity and that the agent still gets a decodable
// image.
func TestShieldJSONImageBase64SurvivesCorruptingRun(t *testing.T) {
	raw := make([]byte, 250000)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	b64 := []byte(base64.StdEncoding.EncodeToString(raw))
	// Overwrite in place rather than splice: every character of the
	// trigger is in the base64 alphabet, so the result is still valid
	// base64 of the same length, it just decodes to different bytes.
	for _, at := range []int{1000, len(b64) / 2, len(b64) - 4096} {
		copy(b64[at:], corruptingRun)
	}
	img := string(b64)
	if !strings.Contains(img, corruptingRun) {
		t.Fatalf("fixture did not carry the trigger")
	}

	body, err := json.Marshal(map[string]any{
		"image_base64": img,
		"format":       "jpeg",
		"width":        1280,
		"height":       800,
	})
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
	if got.ImageBase64 != img {
		t.Fatalf("image_base64 corrupted: %d bytes in, %d out", len(img), len(got.ImageBase64))
	}
	decoded, err := base64.StdEncoding.DecodeString(got.ImageBase64)
	if err != nil {
		t.Fatalf("agent received an undecodable image: %v", err)
	}
	if len(decoded) != len(raw) {
		t.Fatalf("decoded length changed: %d -> %d", len(raw), len(decoded))
	}
}

// TestIBANKeywordFloorCatchesWhatThePredicateRejects proves the floor
// carries the detections validIBAN cannot. A mistyped check digit and an
// unregistered country code are both still personal data, and both are
// tokenized when a word nearby says the value is an account number.
func TestIBANKeywordFloorCatchesWhatThePredicateRejects(t *testing.T) {
	s := newTestSession(t)
	cases := []struct {
		name string
		in   string
		leak string
	}{
		{"mistyped check digit", "IBAN: SE4650000000058398257466", "SE4650000000058398257466"},
		{"mistyped, spaced", "iban SE46 5000 0000 0583 9825 7466", "SE46 5000 0000 0583 9825 7466"},
		{"unregistered country", "kontonummer: ZZ8250000000058398257466", "ZZ8250000000058398257466"},
		{"swedish keyword", "Bankkonto=SE4650000000058398257466", "SE4650000000058398257466"},
		{"english keyword", "bank account: SE4650000000058398257466", "SE4650000000058398257466"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := s.RedactString(context.Background(), tc.in)
			if err != nil {
				t.Fatalf("RedactString: %v", err)
			}
			if strings.Contains(out, tc.leak) {
				t.Fatalf("account number leaked: in=%q out=%q", tc.in, out)
			}
			if !strings.Contains(out, "[shield:iban:tok_") {
				t.Fatalf("in=%q: no iban marker, got %q", tc.in, out)
			}
		})
	}
}

// TestIBANKeywordFloorCannotFireInsideBase64 holds down the claim made in
// the floor pattern's comment: its separator class is what anchors it, so
// even a blob that happens to spell the keyword cannot trigger it.
func TestIBANKeywordFloorCannotFireInsideBase64(t *testing.T) {
	s := newTestSession(t)
	cases := []string{
		"QUJD/IBAN" + corruptingRun + "0EKx3Qm",
		"QUJD+iban+SM36ZXUGY4Q2NA9+0EKx3Qm",
		"QUJD-IBAN-SM36ZXUGY4Q2NA9-0EKx3Qm",
	}
	for _, in := range cases {
		out, err := s.RedactString(context.Background(), in)
		if err != nil {
			t.Fatalf("RedactString(%q): %v", in, err)
		}
		if out != in {
			t.Fatalf("keyword floor fired inside a blob:\n in:  %q\n out: %q", in, out)
		}
	}
}

// TestValidIBAN exercises the predicate directly, including the two ways
// it narrows (unregistered country, wrong length) and the arithmetic.
func TestValidIBAN(t *testing.T) {
	for _, tc := range realIBANs {
		if !validIBAN(tc.value) {
			t.Fatalf("validIBAN(%q) = false, want true", tc.value)
		}
	}
	rejects := []struct {
		name  string
		value string
	}{
		{"reported false positive", "SM36ZXUGY4Q2NA9"},
		{"wrong check digit", "SE4650000000058398257466"},
		{"unregistered country", "ZZ4550000000058398257466"},
		{"right country wrong length", "SE455000000005839825746"},
		{"empty", ""},
		{"too short", "SE45"},
		{"lowercase body", "se4550000000058398257466"},
	}
	for _, tc := range rejects {
		if validIBAN(tc.value) {
			t.Fatalf("%s: validIBAN(%q) = true, want false", tc.name, tc.value)
		}
	}
	// Every registered length must be in the range the regex can
	// propose, or the entry is unreachable dead data.
	for cc, n := range ibanLengths {
		if n < 15 || n > 34 {
			t.Fatalf("ibanLengths[%q] = %d, outside the ISO 13616 range 15..34", cc, n)
		}
		if len(cc) != 2 {
			t.Fatalf("ibanLengths key %q is not a two-letter country code", cc)
		}
	}
}
