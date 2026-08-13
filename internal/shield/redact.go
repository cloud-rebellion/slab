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
type redactPattern struct {
	kind  Kind
	re    *regexp.Regexp
	group int
	hint  []string // hint keys to extract on match
}

var redactPatterns = []redactPattern{
	{
		kind: KindPersonnummer,
		// Swedish personnummer: YYYYMMDD-XXXX or YYMMDD-XXXX, optional
		// dash. 10 or 12 digits with optional dash separator.
		re:   regexp.MustCompile(`\b(?:19|20)?\d{6}[-\s]?\d{4}\b`),
		hint: []string{"century"},
	},
	{
		kind: KindIBAN,
		// IBAN: 2 letters + 2 digits + 11..30 alnum. Loosely matched;
		// detail validation happens at the application layer.
		re:   regexp.MustCompile(`\b[A-Z]{2}\d{2}[A-Z0-9]{10,30}\b`),
		hint: []string{"country"},
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
