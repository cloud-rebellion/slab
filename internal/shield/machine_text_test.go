package shield

import (
	"context"
	"strings"
	"testing"
)

// This file is the counterweight to pii_boundary_test.go.
//
// That file proves the boundary does not UNDER-redact: it deleted the
// length- and alphabet-based blob exemption and asserts every string leaf
// is scanned. Scanning every leaf is only half a boundary, though. The
// other half is that ordinary machine-generated text has to survive the
// scan intact, and nothing measured that, so two false positives shipped
// and were found by reading a live agent API response rather than by the
// suite:
//
//	created_at "2026-08-13 18:41:42" came back "2026-[shield:phone:...]:41:42"
//	the build log's published URL came back https://demo-[shield:personnummer:...].atomicsite...
//
// Both are corruption of data that is not PII and never was. A tokenized
// timestamp is unparseable to the agent that receives it, and the second
// hid from the operator where their own site had just published.
//
// The cases below are all real strings this product emits through the
// agent boundary. They are asserted to pass through byte-for-byte.

// mustRedact runs the full session redaction, which is what the agent
// transport actually calls.
func mustRedact(t *testing.T, s *Session, in string) string {
	t.Helper()
	out, err := s.RedactString(context.Background(), in)
	if err != nil {
		t.Fatalf("RedactString(%q): %v", in, err)
	}
	return out
}

// TestMachineTextPassesThroughUntouched pins the regression directly: each
// of these is a value this product puts in an agent-visible field, and none
// of them is personal data.
func TestMachineTextPassesThroughUntouched(t *testing.T) {
	s := newTestSession(t)

	cases := []struct {
		name string
		in   string
	}{
		// The exact production symptom. SQLite's datetime('now'), which is
		// the schema default for created_at and updated_at on every table.
		{"sqlite datetime", "2026-08-13 18:41:42"},
		// The same stamp with a single-digit month and day, which is where
		// the old six-digit match came from most readily.
		{"sqlite datetime single digits", "2026-01-02 03:04:05"},
		{"rfc3339", "2026-08-13T18:41:42Z"},
		{"rfc3339 offset", "2026-08-13T18:41:42+02:00"},
		{"date only", "2026-08-13"},
		{"json created_at field", `{"created_at":"2026-08-13 18:41:42","pages_built":90}`},

		// The second production symptom: the demo tenant's slug is a unix
		// timestamp, so the whole published URL was mangled.
		{"unix timestamp slug", "demo-1700000000"},
		{"published url", "published to https://demo-1700000000.slab.example.com"},
		{"bare unix seconds", "1777164176"},
		{"unix millis", "1777164176000"},

		// Ordinary machine identifiers that live next to these in the same
		// payloads.
		{"build id", "4ca9e5a9dffd291a44a53903"},
		{"site id", "517e0000000000000000000d"},
		{"duration", "duration_ms=8555 pages_built=90"},
		{"semver", "astro 5.18.2, svelte 5.56.8, bun 1.3.14"},
		{"asset path", "/_assets/cost-calculator.KY8t0Ucs.js"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mustRedact(t, s, tc.in)
			if got != tc.in {
				t.Errorf("machine text was corrupted by the PII boundary:\n  in:  %q\n  out: %q", tc.in, got)
			}
		})
	}
}

// TestRealPIIStillRedacted is the other side, and it is the reason this
// file cannot be "fix it by matching less".
//
// A narrowing that also stops catching personal data is worse than the bug
// it fixes, so every value below has to keep producing a marker. They are
// the same shapes the rest of this package's tests use, restated here so
// this file fails on its own if a future narrowing goes too far, rather
// than relying on a test in another file to notice.
func TestRealPIIStillRedacted(t *testing.T) {
	s := newTestSession(t)

	cases := []struct {
		name string
		in   string
		kind Kind
	}{
		{"personnummer 12 digit", "19850101-1234", KindPersonnummer},
		{"personnummer 10 digit", "900101-1234", KindPersonnummer},
		{"personnummer no dash", "8501011234", KindPersonnummer},
		// Samordningsnummer: the day field carries +60.
		{"samordningsnummer", "850161-1234", KindPersonnummer},
		{"swedish mobile", "070-123 45 67", KindPhone},
		{"stockholm landline", "08-123 456 78", KindPhone},
		{"gothenburg landline", "031-123 45 67", KindPhone},
		{"four digit area code", "0470-123 45", KindPhone},
		{"international", "+46 70 123 45 67", KindPhone},
		{"email", "anna.andersson@example.com", KindEmail},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mustRedact(t, s, tc.in)
			if got == tc.in {
				t.Fatalf("PII passed through unredacted: %q", tc.in)
			}
			want := "[shield:" + string(tc.kind) + ":"
			if !strings.Contains(got, want) {
				t.Errorf("redacted %q to %q, want a %s marker", tc.in, got, tc.kind)
			}
		})
	}
}

// TestPIISurvivesInsideMachineText is the case neither list alone covers: a
// real number embedded in a line that also carries a timestamp. The
// timestamp must survive and the number must not.
func TestPIISurvivesInsideMachineText(t *testing.T) {
	s := newTestSession(t)

	const in = `2026-08-13 18:41:42 order from 070-123 45 67 for site demo-1700000000`
	got := mustRedact(t, s, in)

	if !strings.Contains(got, "2026-08-13 18:41:42") {
		t.Errorf("timestamp was corrupted: %q", got)
	}
	if !strings.Contains(got, "demo-1700000000") {
		t.Errorf("tenant slug was corrupted: %q", got)
	}
	if strings.Contains(got, "070-123 45 67") {
		t.Errorf("phone number was NOT redacted: %q", got)
	}
	if !strings.Contains(got, "[shield:phone:") {
		t.Errorf("expected a phone marker in %q", got)
	}
}

// TestDigitCountFloorMatchesRealNumbers documents where the phone floor
// sits and why, so a future reader does not have to rediscover it from the
// regex. Eight is the smallest digit count any dialable Swedish number
// carries in national form; a YYYY-MM-DD HH:MM:SS stamp tops out at six
// because the separator class excludes ':'.
func TestDigitCountFloorMatchesRealNumbers(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"08-13 18", 6},   // the longest run a SQLite timestamp can offer
		{"01-02 03", 6},   // and with single-digit fields
		{"08-123 456", 8}, // the shortest real number that must still match
		{"070-123 45 67", 10},
		{"08-123 456 78", 10},
	} {
		if got := countDigits(tc.in); got != tc.want {
			t.Errorf("countDigits(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestLooksLikeDatePrefixRejectsOnlyNonDates asserts the personnummer
// validator can only ever reject a digit run whose date field is not a
// date, which is the one thing no real personnummer can fail.
func TestLooksLikeDatePrefixRejectsOnlyNonDates(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
		why  string
	}{
		{"19850101-1234", true, "12-digit personnummer"},
		{"850101-1234", true, "10-digit personnummer"},
		{"8501011234", true, "no separator"},
		{"851231-1234", true, "december 31"},
		{"850161-1234", true, "samordningsnummer, day +60"},
		{"850191-1234", true, "samordningsnummer, day 31+60"},
		{"1777164176", false, "unix seconds: month 77"},
		{"1712345678", false, "unix seconds: day 34"},
		{"850001-1234", false, "month 00"},
		{"851301-1234", false, "month 13"},
		{"850100-1234", false, "day 00"},
		{"850132-1234", false, "day 32, below the coordination range"},
		{"850192-1234", false, "day 92, above the coordination range"},
	} {
		if got := looksLikeDatePrefix(tc.in); got != tc.want {
			t.Errorf("looksLikeDatePrefix(%q) = %v, want %v (%s)", tc.in, got, tc.want, tc.why)
		}
	}
}

// TestRedactStringDoesNotUnderRedact is the guard this file was missing.
//
// TestRealPIIStillRedacted above lists the shapes I thought of when I added the
// accept hook, and every one of them passed while the boundary was leaking
// 4442 of 5000 real Swedish mobiles to the model. The gap was in the direction
// the hook narrows: a bare digit run with NO separator. The national phone
// pattern refuses those by design, so the personnummer pattern was the only
// thing catching them, and teaching it to reject non-dates removed that floor
// without any test noticing. 072/073/076/079 read as month 21/31/61/91.
//
// Every case here is a raw string that must not survive anywhere in the output,
// which is a stricter assertion than "a marker appeared somewhere": the
// international pattern used to tokenize most of a number and leave the last
// group in the clear, and a marker-presence check calls that a pass.
func TestRedactStringDoesNotUnderRedact(t *testing.T) {
	s := newTestSession(t)

	cases := []struct {
		name string
		in   string
		raw  string // must not survive anywhere in the output
	}{
		// Bare-form Swedish mobiles, one per operator prefix. These are the
		// regression: all but 070 leaked completely.
		{"mobile_070_bare", "ring 0701234567 nu", "0701234567"},
		{"mobile_070_zeros", "ring 0700004567 nu", "0700004567"},
		{"mobile_072_bare", "ring 0721234567 nu", "0721234567"},
		{"mobile_073_bare", "ring 0731234567 nu", "0731234567"},
		{"mobile_076_bare", "ring 0761234567 nu", "0761234567"},
		{"mobile_079_bare", "ring 0791234567 nu", "0791234567"},
		{"landline_bare_10", "ring 0812345678 nu", "0812345678"},
		{"landline_bare_8", "ring 08123456 nu", "08123456"},
		// A bare landline whose digits happen to read as a valid date. This is
		// why the floor carries no accept hook of its own.
		{"landline_date_shaped", "ring 08010112 nu", "08010112"},
		// Swedish law requires '+' instead of '-' once the holder turns 100,
		// so this is the exact population an elder-care tenant handles.
		{"pnr_plus_form", "PNR 850315+1234", "850315+1234"},
		{"pnr_plus_form_12", "PNR 19850315+1234", "19850315+1234"},
		{"samordning_plus_form", "SAM 850375+1234", "850375+1234"},
		// Forms that already worked. Kept so a future widening of the date
		// check cannot quietly break them.
		{"pnr_hyphen", "PNR 850315-1234", "850315-1234"},
		{"pnr_bare", "PNR 8503151234", "8503151234"},
		{"samordning_hyphen", "SAM 850375-1234", "850375-1234"},
		{"samordning_bare", "SAM 8503751234", "8503751234"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mustRedact(t, s, tc.in)
			if strings.Contains(got, tc.raw) {
				t.Errorf("raw PII survived redaction:\n  in:  %q\n  out: %q\n  raw: %q", tc.in, got, tc.raw)
			}
		})
	}
}
