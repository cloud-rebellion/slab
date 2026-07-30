package shield

import (
	"context"
	"regexp"
	"strings"
)

// redactPattern is one regex + the kind it produces. Order in
// redactPatterns matters: more specific patterns must come before more
// general ones (e.g. personnummer before generic digit runs).
type redactPattern struct {
	kind Kind
	re   *regexp.Regexp
	hint []string // hint keys to extract on match
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
		// Two alternatives.
		//
		// 1. International: +CC followed by 7..14 digits with optional
		//    separators. This was the ONLY alternative, so it required a
		//    leading "+" and therefore missed every national-format number:
		//    "070-123 45 67" passed through a get_profile response to the
		//    model completely untokenized.
		// 2. National: a leading 0 then 8..10 more digits with separators.
		//    Requires at least one separator, deliberately. A BARE ten-digit
		//    string is ambiguous between a Swedish mobile (0701234567) and a
		//    personnummer (9001011234) with no way to tell them apart by shape,
		//    and the personnummer pattern above runs first and wins. Both are
		//    PII and both get tokenized either way, so the only cost is a
		//    mislabelled kind on the marker, which is the right trade against
		//    loosening the personnummer match.
		re: regexp.MustCompile(`(?:\+\d{1,3}[\s\-().]*\d{1,4}[\s\-().]*\d{1,4}[\s\-().]*\d{1,9})` +
			`|(?:\b0\d{1,3}[\s\-().]+\d{2,4}[\s\-().]*\d{2,4}[\s\-().]*\d{0,4}\b)`),
		hint: []string{"country", "len"},
	},
}

// RedactString runs the regex bank over in and replaces every match
// with a marker. Used for free-form text fields (note bodies, message
// content, comments) where struct-tag-based shielding does not apply
// because the PII is embedded inside narrative text.
func (s *Session) RedactString(ctx context.Context, in string) (string, error) {
	if in == "" {
		return in, nil
	}
	out := in
	for _, p := range redactPatterns {
		var firstErr error
		out = p.re.ReplaceAllStringFunc(out, func(match string) string {
			// Skip if the match is already inside an existing marker.
			if strings.HasPrefix(match, "[shield:") {
				return match
			}
			hint := buildHint(p.kind, match, p.hint)
			marker, err := s.Tokenize(ctx, p.kind, match, hint)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return match
			}
			return marker
		})
		if firstErr != nil {
			return out, firstErr
		}
	}
	return out, nil
}
