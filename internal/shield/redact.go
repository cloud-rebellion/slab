package shield

import (
	"context"
	"regexp"
	"strings"
)

// redactPattern is one regex + the kind it produces. Order in
// redactPatterns matters: more specific patterns must come before more
// general ones (e.g. personnummer before generic digit runs).
//
// group selects which submatch gets replaced (0 = the whole match, the
// default and correct choice for every pattern that is naturally
// self-delimiting). A pattern that needs an explicit non-capturing
// boundary consumed ahead of the PII itself (see the international phone
// pattern below) sets group to the index of the submatch that should
// actually be tokenized, so the boundary character is matched to anchor
// the pattern but is never touched by the replacement.
// accept is an optional second stage: the regex proposes, accept disposes.
// A nil accept means every regex match is taken.
//
// RE2 has no lookaround and no counting across alternations, so a pattern
// whose real definition includes an arithmetic or calendar constraint
// ("a phone number carries at least eight digits", "a personnummer starts
// with a date") cannot express that constraint in the regex itself. Before
// this hook those constraints lived only in the comment above each pattern,
// and both had drifted away from what the regex actually matched, which is
// how a SQLite timestamp came to be tokenized as a phone number in every
// agent API response. Encoding them as code keeps the doc and the behaviour
// from disagreeing again.
//
// A validator may only ever NARROW a match, and only on grounds that no
// genuine value of that kind can fail. Widening belongs in the regex, where
// it is visible.
type redactPattern struct {
	kind   Kind
	re     *regexp.Regexp
	group  int
	hint   []string // hint keys to extract on match
	accept func(match string) bool
}

// countDigits returns how many ASCII digits appear in s.
func countDigits(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			n++
		}
	}
	return n
}

// looksLikeDatePrefix reports whether the first six digits of s form a
// plausible YYMMDD (or the last six of a YYYYMMDD), which every Swedish
// personnummer does by construction: it is a birth date followed by a
// four-digit birth number.
//
// This is a calendar check, not a validity check. It deliberately does NOT
// verify the Luhn control digit or the real length of the month, because a
// mistyped-but-real personnummer is still personal data and must still be
// tokenized. It only rejects a digit run whose date field could not be a
// date at all, which no real personnummer can fail.
func looksLikeDatePrefix(s string) bool {
	digits := make([]byte, 0, 12)
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			digits = append(digits, s[i])
		}
	}
	// A 12-digit form carries the century; the date starts after it.
	if len(digits) >= 12 {
		digits = digits[len(digits)-10:]
	}
	if len(digits) < 6 {
		return false
	}
	month := int(digits[2]-'0')*10 + int(digits[3]-'0')
	day := int(digits[4]-'0')*10 + int(digits[5]-'0')
	// Day 61..91 is the coordination-number (samordningsnummer) form: the
	// real day plus 60. It is issued to people without a personnummer and
	// is exactly as sensitive, so it is accepted here.
	if month < 1 || month > 12 {
		return false
	}
	if day < 1 || (day > 31 && (day < 61 || day > 91)) {
		return false
	}
	return true
}

// ibanLengths is the ISO 13616 registry: IBAN country code -> the total
// length of an IBAN issued under it, spaces removed. Every country that
// issues IBANs publishes exactly one length, so a candidate whose length
// disagrees with its own country code is not an IBAN.
//
// Codes that are NOT keys here deliberately include every two-letter
// sequence that is not an IBAN issuer at all, which is what the base64
// false positives were made of (measured: QO41..., ZH06..., FF92...,
// YN80..., IM876... over 133 MB of random base64, none of them an issuer).
//
// Territories that issue under another country's code are absent on
// purpose: Jersey, Guernsey and the Isle of Man use GB, and the French
// overseas departments use FR, so JE/GG/IM/GP/MQ/RE never lead a real
// IBAN. Entries below the horizontal break are national IBAN schemes in
// live use that the ISO registry has not adopted; they are included
// because omitting a real issuer is a PII leak, which is the worse error.
//
// Adding a country is one line. If a new issuer is missing here its
// IBANs fall to the keyword-anchored floor pattern below, so the failure
// mode is "only caught next to the word IBAN", not "never caught".
var ibanLengths = map[string]int{
	"AD": 24, "AE": 23, "AL": 28, "AT": 20, "AZ": 28, "BA": 20, "BE": 16,
	"BG": 22, "BH": 22, "BI": 27, "BR": 29, "BY": 28, "CH": 21, "CR": 22,
	"CY": 28, "CZ": 24, "DE": 22, "DJ": 27, "DK": 18, "DO": 28, "EE": 20,
	"EG": 29, "ES": 24, "FI": 18, "FK": 18, "FO": 18, "FR": 27, "GB": 22,
	"GE": 22, "GI": 23, "GL": 18, "GR": 27, "GT": 28, "HN": 28, "HR": 21,
	"HU": 28, "IE": 22, "IL": 23, "IQ": 23, "IS": 26, "IT": 27, "JO": 30,
	"KW": 30, "KZ": 20, "LB": 28, "LC": 32, "LI": 21, "LT": 20, "LU": 20,
	"LV": 21, "LY": 25, "MC": 27, "MD": 24, "ME": 22, "MK": 19, "MN": 20,
	"MR": 27, "MT": 31, "MU": 30, "NI": 28, "NL": 18, "NO": 15, "OM": 23,
	"PK": 24, "PL": 28, "PS": 29, "PT": 25, "QA": 29, "RO": 24, "RS": 22,
	"RU": 33, "SA": 24, "SC": 31, "SD": 18, "SE": 24, "SI": 19, "SK": 24,
	"SM": 27, "SO": 23, "ST": 25, "SV": 28, "TL": 23, "TN": 24, "TR": 26,
	"UA": 29, "VA": 22, "VG": 24, "XK": 20, "YE": 30,

	"AO": 25, "BF": 28, "BJ": 28, "CF": 27, "CG": 27, "CI": 28, "CM": 27,
	"CV": 25, "DZ": 26, "GA": 27, "GQ": 27, "GW": 25, "IR": 26, "KM": 27,
	"MA": 28, "MG": 27, "ML": 28, "MZ": 25, "NE": 28, "SN": 28, "SS": 21,
	"TD": 27, "TG": 28,
}

// validIBAN reports whether s is structurally an IBAN: a registered
// country code, that country's exact length, and the ISO 7064 MOD 97-10
// check digits ISO 13616 mandates. Spaces are ignored, so the print
// format ("SE45 5000 0000 0583 9825 7466") and the compact form validate
// identically.
//
// Unlike looksLikeDatePrefix above, this predicate CAN cost a real
// detection, and the cases are named rather than waved away: an IBAN
// whose check digits are mistyped fails here, and so does one whose
// issuer is missing from the table. Both are covered by the
// keyword-anchored floor pattern below, which carries no accept hook, so
// the loss is confined to a malformed IBAN written with no nearby word
// identifying it as one. That is the price of a predicate over a
// denylist, and it buys the property a boundary tweak cannot: a random
// base64 run is rejected on arithmetic rather than on the punctuation
// that happens to surround it.
func validIBAN(s string) bool {
	var buf [34]byte
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' {
			continue
		}
		if n == len(buf) {
			return false
		}
		buf[n] = c
		n++
	}
	if n < 4 {
		return false
	}
	if want, ok := ibanLengths[string(buf[:2])]; !ok || want != n {
		return false
	}
	// ISO 7064 MOD 97-10 over the value rotated four characters left,
	// with A..Z read as 10..35, carried as a running remainder so no
	// bignum is needed. A genuine IBAN leaves remainder 1 by definition.
	rem := 0
	for i := 0; i < n; i++ {
		switch c := buf[(i+4)%n]; {
		case c >= '0' && c <= '9':
			rem = rem*10 + int(c-'0')
		case c >= 'A' && c <= 'Z':
			rem = rem*100 + int(c-'A') + 10
		default:
			return false
		}
		rem %= 97
	}
	return rem == 1
}

var redactPatterns = []redactPattern{
	{
		kind: KindPersonnummer,
		// Swedish personnummer: YYYYMMDD-XXXX or YYMMDD-XXXX, optional
		// dash. 10 or 12 digits with optional dash separator.
		//
		// '+' is in the separator class because Swedish law REQUIRES it in
		// place of the hyphen once the holder turns 100 (850315+1234). It was
		// absent, so every personnummer belonging to a person aged 100 or over
		// passed through unredacted, which is precisely the population an
		// elder-care or estate-administration tenant handles.
		re:   regexp.MustCompile(`\b(?:19|20)?\d{6}[-+\s]?\d{4}\b`),
		hint: []string{"century"},
		// A personnummer BEGINS with a birth date, so a bare digit run
		// whose date field is not a date is not one. Without this, every
		// unbroken ten-digit run matched, which is every unix timestamp
		// for the next two centuries and most generated ids: the tenant
		// slug demo-1700000000 was tokenized as a personnummer in the
		// build log, so the operator could not read where their own site
		// had published.
		//
		// This cannot cost a real detection. The check reads only the
		// month and day fields, which every personnummer and every
		// samordningsnummer populates by construction, and it does not
		// look at the control digit, so a mistyped or fabricated number
		// of the right shape is still tokenized. An id that does happen
		// to carry a valid date field still matches, deliberately: shape
		// alone cannot separate it from a real number, and tokenizing is
		// the safe side of that ambiguity.
		//
		// This validator is only safe because of the bare-domestic pattern
		// directly below. Read that one before changing either.
		accept: looksLikeDatePrefix,
	},
	{
		kind: KindPhone,
		// BARE Swedish domestic number: a leading 0 and 7..9 more digits,
		// with NO separator anywhere.
		//
		// This is the floor under the personnummer validator, and it exists
		// because adding that validator removed one that was load-bearing
		// without anyone noticing.
		//
		// The national pattern below REQUIRES a separator, deliberately, and
		// its comment records the reasoning: a bare ten-digit run is ambiguous
		// between a mobile (0701234567) and a personnummer (9001011234), "the
		// personnummer pattern above runs first and wins ... both get tokenized
		// either way, so the only cost is a mislabelled kind on the marker."
		// That was true, and it made the personnummer pattern the only thing
		// catching a bare phone number. Teaching it to reject non-dates voided
		// the contract from the other side: 072/073/076/079 mobiles read as
		// month 21/31/61/91, get rejected, meet no separator here, and reach
		// the model verbatim. Measured before this pattern existed: 1000/1000
		// bare 073 numbers leaked.
		//
		// The leading 0 is what makes this safe against the false positives the
		// validator was written for. A unix timestamp begins with 1 for the next
		// two centuries, so it cannot match; a hex id has no word boundary before
		// its digits; and a YYYY-MM-DD HH:MM:SS stamp has no run this long. Those
		// three are exactly the cases in machine_text_test.go, and they stay green.
		//
		// 7..9 more digits covers the real range, from a short geographic number
		// (08123456) up to a ten-digit mobile, without reaching the 13-digit
		// 0046-prefixed international form, which has its own gap.
		// Deliberately NO accept hook. The obvious one, "reject anything that
		// looks like a personnummer", is worse than useless twice over: the
		// personnummer pattern runs first and has already tokenized every
		// date-prefixed 10- and 12-digit run, so on those it is dead code; and
		// on the 7..9 digit runs personnummer never matches, it would start
		// REJECTING real landlines that happen to read as a date. 08010112 is
		// month 01 day 01, and would leak for that reason alone. That is the
		// same mistake as the one this pattern exists to repair, one length
		// down, so the floor stays unconditional.
		re:   regexp.MustCompile(`\b0\d{7,9}\b`),
		hint: []string{"country", "len"},
	},
	{
		kind: KindIBAN,
		// IBAN, compact or in the ISO 13616 print format (groups of four
		// separated by single spaces). The regex proposes and validIBAN
		// disposes; "detail validation happens at the application layer"
		// was the old comment here and it was not true of any caller.
		//
		// Why this needs a predicate at all, the same lesson as the
		// international phone anchor directly below: base64's alphabet
		// contains '+' and '/', both of which are regex word boundaries,
		// so a random base64 run reads as \b-delimited to RE2. The old
		// pattern `\b[A-Z]{2}\d{2}[A-Z0-9]{10,30}\b` needs only two
		// uppercase letters, two digits and ten more alnum between two
		// such boundaries, which random base64 produces constantly:
		// measured at 5 hits over 133 MB of it, e.g.
		// "...ISBDXW/SM36ZXUGY4Q2NA9/0EKx3Qm..." came back as
		// "...ISBDXW/[shield:iban:tok_7c96b09a0a8d:country=SM]/0EKx3Qm...".
		// Every ShieldJSON caller was exposed, including the base64 JPEG
		// in preview_screenshot's result: about 0.4% of screenshots
		// reached the agent as an undecodable image with no error, which
		// is indistinguishable from a failed capture.
		//
		// A boundary tweak was the other option and was rejected. It is a
		// denylist: it would have to name '+', '/', '-', '=' and '_' (the
		// base64url false positives measured here sat between '-'
		// characters), and it would cost a real detection on "IBAN=SE45..."
		// while still admitting any run the denylist did not anticipate.
		// The check digits are a predicate instead, and they reject on
		// arithmetic rather than on surrounding punctuation.
		//
		// The regex is a deliberate SUPERSET of what validIBAN accepts,
		// which is the safe direction: a match the predicate rejects is
		// copied through verbatim by replacePatternGroup, so a greedy
		// over-match can only fail to redact, never eat text.
		re:     regexp.MustCompile(`\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]{4}){2,7}(?: ?[A-Z0-9]{1,3})?\b`),
		hint:   []string{"country"},
		accept: validIBAN,
	},
	{
		kind: KindIBAN,
		// The floor under validIBAN, exactly as the bare-domestic phone
		// pattern is the floor under looksLikeDatePrefix. Read that one
		// before changing either.
		//
		// A predicate that rejects random base64 also rejects a genuine
		// IBAN with a mistyped check digit, and one issued by a country
		// missing from ibanLengths. Both are still personal data. So an
		// IBAN-shaped value introduced by a word that says it is an
		// account number is tokenized with NO accept hook, on the same
		// reasoning the personnummer pattern uses for mistyped numbers:
		// the safe side of the ambiguity is to tokenize.
		//
		// This cannot reintroduce the corruption it sits beside. The
		// separator class [\s:=] is the anchor: no base64 or base64url
		// encoder emits whitespace or a colon, and '=' appears only as
		// terminal padding with nothing after it, so the keyword and the
		// separator together cannot occur inside an encoded blob.
		// Measured at zero hits over the same 133 MB.
		//
		// group 1 is the account number; the keyword and separator are
		// matched only to anchor the pattern and are copied through.
		re:    regexp.MustCompile(`(?i:\b(?:iban|bankkonto|kontonummer|bank ?account)\b)[\s:=]{1,4}([A-Z]{2}\d{2}(?: ?[A-Z0-9]{4}){2,7}(?: ?[A-Z0-9]{1,3})?)\b`),
		group: 1,
		hint:  []string{"country"},
	},
	{
		kind: KindEmail,
		re:   regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`),
		hint: []string{"domain", "len"},
	},
	{
		kind: KindPhone,
		// International: +CC followed by 7..14 digits with optional
		// separators, ANCHORED so it cannot fire on a "+" that is glued
		// to another alphanumeric/base64 character (group 1 is what gets
		// tokenized; the boundary character in the non-capturing
		// alternative is matched only to anchor the pattern and is
		// copied through untouched).
		//
		// Why the anchor exists: base64 output contains "+" followed by
		// digits constantly (every "+" is legal base64 alphabet and is
		// very often immediately followed by digit characters, which are
		// also legal base64 alphabet). Before this anchor, this
		// alternative matched a "+<digits>" run anywhere, including
		// inside an encoded blob, and replacing that match corrupted the
		// blob (measured on a 200 KB PNG: 266668 bytes in, 266743 out,
		// undecodable). A real phone number's leading "+" is always
		// preceded by whitespace, punctuation, a JSON quote, or the start
		// of the string, never by another base64/hex character, so
		// requiring that the preceding character (if any) falls outside
		// the base64/base64url alphabet loses no legitimate match.
		re:    regexp.MustCompile(`(?:^|[^A-Za-z0-9+/=_])(\+\d{1,3}[\s\-().]*\d{1,4}[\s\-().]*\d{1,4}[\s\-().]*\d{1,9})`),
		group: 1,
		hint:  []string{"country", "len"},
	},
	{
		kind: KindPhone,
		// National: a leading 0 then 8..10 more digits with separators.
		// Requires at least one separator, deliberately. A BARE ten-digit
		// string is ambiguous between a Swedish mobile (0701234567) and a
		// personnummer (9001011234) with no way to tell them apart by shape,
		// and the personnummer pattern above runs first and wins. Both are
		// PII and both get tokenized either way, so the only cost is a
		// mislabelled kind on the marker, which is the right trade against
		// loosening the personnummer match. Already self-anchored via \b,
		// so it does not need the boundary-group treatment above: a bare
		// "0" preceded by another base64 alphanumeric character is not a
		// \b boundary, so this alternative cannot fire inside a blob either.
		re:   regexp.MustCompile(`\b0\d{1,3}[\s\-().]+\d{2,4}[\s\-().]*\d{2,4}[\s\-().]*\d{0,4}\b`),
		hint: []string{"country", "len"},
		// The regex above accepts as few as FIVE digits, while the line
		// that documents it says "a leading 0 then 8..10 more digits".
		// That gap is not cosmetic: a SQLite timestamp is "2026-08-13
		// 18:41:42", the separator class contains '-' and ' ' but not
		// ':', and the run "08-13 18" is a clean six-digit match. Every
		// created_at and updated_at crossing the agent boundary came back
		// as "2026-[shield:phone:...]:41:42", observed in production on
		// the first agent call after the per-leaf scan shipped.
		//
		// Restoring the documented count closes the whole date class
		// rather than special-casing dates: a YYYY-MM-DD HH:MM:SS stamp
		// can never reach past the ':', so it tops out at six digits, and
		// nothing shorter than eight is a dialable Swedish number in
		// national form anyway. Real numbers are unaffected: 08-594 118
		// 50 and 070-123 45 67 both carry ten.
		accept: func(m string) bool { return countDigits(m) >= 8 },
	},
}

// RedactString runs the regex bank over in and replaces every match
// with a marker. Used for free-form text fields (note bodies, message
// content, comments) where struct-tag-based shielding does not apply
// because the PII is embedded inside narrative text.
//
// There is deliberately no length- or alphabet-based short-circuit here:
// an earlier version skipped whole string leaves that "looked like" an
// encoded blob (512+ chars, base64-only alphabet) to avoid the phone
// pattern's blob false positives, but that check ran on CONTENT rather
// than on where the value came from (see the estate's secure-build
// pattern: never branch on the shape of client input) and a payload
// engineered to satisfy the blob alphabet while still carrying readable
// PII sailed through completely untouched, and longer payloads were
// LESS protected than shorter ones. The blob false-positive problem is
// fixed at its source above (the phone pattern anchor); every leaf is
// now always scanned, so protection no longer depends on length.
func (s *Session) RedactString(ctx context.Context, in string) (string, error) {
	if in == "" {
		return in, nil
	}
	out := in
	for _, p := range redactPatterns {
		var firstErr error
		out = replacePatternGroup(p, out, func(match string) (string, bool) {
			// Skip if the match is already inside an existing marker.
			if strings.HasPrefix(match, "[shield:") {
				return match, false
			}
			hint := buildHint(p.kind, match, p.hint)
			marker, err := s.Tokenize(ctx, p.kind, match, hint)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return match, false
			}
			return marker, true
		})
		if firstErr != nil {
			return out, firstErr
		}
	}
	return out, nil
}

// replacePatternGroup replaces submatch p.group (0 = the whole match) of
// every non-overlapping match of p.re in in, leaving every other part of
// the string, including any anchor characters captured only outside
// p.group, untouched. replace returns the replacement text and whether a
// replacement actually happened (false leaves the original text in
// place, used when a match is skipped, e.g. it is already a marker).
func replacePatternGroup(p redactPattern, in string, replace func(match string) (string, bool)) string {
	idxs := p.re.FindAllStringSubmatchIndex(in, -1)
	if idxs == nil {
		return in
	}
	gi := p.group * 2
	var b strings.Builder
	b.Grow(len(in))
	last := 0
	for _, m := range idxs {
		start, end := m[gi], m[gi+1]
		if start < 0 {
			// The target group did not participate in this match; every
			// pattern above always populates its target group when the
			// overall pattern matches, so this should not happen, but
			// skip defensively rather than panic on a slice index.
			continue
		}
		b.WriteString(in[last:start])
		target := in[start:end]
		// The second stage. A rejected match is copied through verbatim,
		// exactly like a match the caller declines, so a validator can
		// never drop text.
		if p.accept != nil && !p.accept(target) {
			b.WriteString(target)
			last = end
			continue
		}
		repl, ok := replace(target)
		if ok {
			b.WriteString(repl)
		} else {
			b.WriteString(target)
		}
		last = end
	}
	b.WriteString(in[last:])
	return b.String()
}
