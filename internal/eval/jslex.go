package eval

import "strings"

// This file is a minimal, pragmatic JavaScript lexer. It exists because
// privacy.go's consent/tracker detection cannot be done correctly with
// string.Contains over raw text or over a hand-rolled comment stripper: see
// the package comment on collectScriptRefs for why. A lexer that actually
// tracks comments, string literals and regex literals as their own token
// kinds makes three questions structurally answerable instead of
// heuristic:
//
//  1. Is this text a comment (never executes)?
//  2. Is this text inside a string or regex literal (never executes as an
//     identifier, even though the characters are visually identical to
//     code)?
//  3. Is a recognised identifier chain immediately followed by a call `(`
//     or a plain assignment `=` (evidence of use), or is it just sitting
//     next to other punctuation (evidence of nothing)?
//
// It is NOT a full ECMAScript parser: there is no AST, no operator
// precedence, and template-literal interpolations (`${...}`) are treated as
// opaque string content rather than re-entering code mode. Both are
// deliberate scope cuts -- see the design note in privacy.go -- because the
// only thing this package needs is "was this identifier chain used in code
// position", not a correct parse of arbitrary JS.

type jsTokenKind int

const (
	jsIdent   jsTokenKind = iota // identifier/keyword, text lowercased
	jsPunct                      // operator/punctuator, exact text
	jsNumber                     // numeric literal, exact text
	jsString                     // '...' "..." or `...`; text is the UNQUOTED, lowercased body
	jsRegex                      // /.../flags; text is the raw literal including slashes
	jsComment                    // // ... , /* ... */, <!-- ... , or a line-leading --> ...
)

type jsToken struct {
	kind jsTokenKind
	text string
}

// regexAllowedAfter reports whether a `/` immediately following this
// identifier/keyword text (already lowercased) should be read as the start
// of a regex literal rather than a division operator. JS keywords that
// themselves behave like operators (they precede an operand, never follow
// one) allow a regex; an ordinary identifier is a value, so a following `/`
// divides it.
var regexAllowedAfterKeyword = map[string]bool{
	"return": true, "typeof": true, "instanceof": true, "in": true, "of": true,
	"new": true, "delete": true, "void": true, "yield": true, "throw": true,
	"do": true, "else": true, "case": true, "await": true, "extends": true,
	"default": true, "if": true, "while": true, "for": true,
}

// multiCharPunct is every multi-character punctuator this lexer needs to
// recognise as one token, longest first so the scan is a simple linear
// probe. Getting `===` split into `==` `=` would misclassify an equality
// check as an assignment and falsely credit an inline CMP.
var multiCharPunct = []string{
	">>>=", "...", "===", "!==", "**=", "<<=", ">>=", ">>>",
	"=>", "==", "!=", "<=", ">=", "&&", "||", "??", "?.",
	"++", "--", "+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "**", "<<", ">>",
}

// tokenizeJS lexes an inline <script> body. It never errors: malformed or
// adversarial input (an unterminated string, a stray backtick) is recovered
// from at the nearest line boundary rather than aborting, because eval runs
// against arbitrary tenant-authored markup and a panic or an infinite loop
// here would take an entire site evaluation down with it.
func tokenizeJS(body string) []jsToken {
	var out []jsToken
	n := len(body)
	i := 0
	regexAllowed := true // true at start of input: `/foo/.test(x)` as a bare statement is legal
	atLineStart := true  // tracks the `-->` HTML-close-comment rule: only valid as the first token on a line

	for i < n {
		c := body[i]

		// Whitespace: track line boundaries for the `-->` rule, otherwise skip.
		if c == ' ' || c == '\t' || c == '\r' {
			i++
			continue
		}
		if c == '\n' {
			i++
			atLineStart = true
			continue
		}

		// HTML-style comment open `<!--` (ECMAScript Annex B.1.3): a legacy
		// but still-live piece of the JS grammar. It is a single-line
		// comment starter WHEREVER it appears, not only at line start. A
		// naive comment stripper that only knows `//` and `/* */` leaves
		// text after `<!--` completely unstripped, which is exactly the
		// bypass class this lexer closes: a fabricated CMP reference placed
		// after `<!--` never executes in a real browser, but survived every
		// prior remediation's text matching.
		if c == '<' && i+3 < n && body[i+1] == '!' && body[i+2] == '-' && body[i+3] == '-' {
			start := i
			i += 4
			for i < n && body[i] != '\n' {
				i++
			}
			out = append(out, jsToken{jsComment, body[start:i]})
			regexAllowed = true
			continue
		}

		// HTML-style comment close `-->`: only a comment starter when it is
		// the first real token on its line (Annex B.1.3's early-error
		// carve-out). Anywhere else `-->` is the decrement operator `--`
		// followed by `>`, both ordinary punctuators.
		if c == '-' && i+2 < n && body[i+1] == '-' && body[i+2] == '>' && atLineStart {
			start := i
			i += 3
			for i < n && body[i] != '\n' {
				i++
			}
			out = append(out, jsToken{jsComment, body[start:i]})
			regexAllowed = true
			continue
		}

		// Line comment.
		if c == '/' && i+1 < n && body[i+1] == '/' {
			start := i
			i += 2
			for i < n && body[i] != '\n' {
				i++
			}
			out = append(out, jsToken{jsComment, body[start:i]})
			regexAllowed = true
			continue
		}

		// Block comment.
		if c == '/' && i+1 < n && body[i+1] == '*' {
			start := i
			i += 2
			for i < n && !(body[i] == '*' && i+1 < n && body[i+1] == '/') {
				i++
			}
			if i < n {
				i += 2
			} else {
				i = n // unterminated: recover at EOF
			}
			out = append(out, jsToken{jsComment, body[start:i]})
			regexAllowed = true
			atLineStart = false
			continue
		}

		// String literals: '...' "..." `...`. Contents are never scanned
		// for identifiers -- see the package comment. Regular strings
		// cannot span a real newline (recovered at the newline); template
		// literals can, and their `${...}` interpolations are deliberately
		// swallowed as opaque content (see the scope-cut note above).
		if c == '\'' || c == '"' || c == '`' {
			quote := c
			start := i
			i++
			closed := false
			for i < n {
				if body[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				if body[i] == quote {
					i++
					closed = true
					break
				}
				if body[i] == '\n' && quote != '`' {
					break // unterminated single/double-quoted string: recover at the newline, don't consume it
				}
				i++
			}
			var inner string
			if closed {
				inner = body[start+1 : i-1]
			} else {
				inner = body[start+1 : i]
			}
			out = append(out, jsToken{jsString, strings.ToLower(inner)})
			regexAllowed = false
			atLineStart = false
			continue
		}

		// `/`: comment forms are already handled above, so at this point it
		// is either a regex literal or division/`/=`.
		if c == '/' {
			if regexAllowed {
				if lit, end, ok := scanRegex(body, i); ok {
					out = append(out, jsToken{jsRegex, lit})
					i = end
					regexAllowed = false
					atLineStart = false
					continue
				}
				// Not a well-formed regex (e.g. an unescaped newline before
				// the closing '/'): fall through and treat '/' as a lone
				// punctuator so the scan makes forward progress.
			}
			if i+1 < n && body[i+1] == '=' {
				out = append(out, jsToken{jsPunct, "/="})
				i += 2
			} else {
				out = append(out, jsToken{jsPunct, "/"})
				i++
			}
			regexAllowed = true
			atLineStart = false
			continue
		}

		// Numbers.
		if isDigit(c) || (c == '.' && i+1 < n && isDigit(body[i+1])) {
			start := i
			for i < n && (isAlnum(body[i]) || body[i] == '.') {
				i++
			}
			out = append(out, jsToken{jsNumber, body[start:i]})
			regexAllowed = false
			atLineStart = false
			continue
		}

		// Identifiers / keywords.
		if isIdentStart(c) {
			start := i
			for i < n && isIdentPart(body[i]) {
				i++
			}
			text := strings.ToLower(body[start:i])
			out = append(out, jsToken{jsIdent, text})
			regexAllowed = regexAllowedAfterKeyword[text]
			atLineStart = false
			continue
		}

		// Multi-character punctuators (longest match wins).
		matched := false
		for _, p := range multiCharPunct {
			if strings.HasPrefix(body[i:], p) {
				out = append(out, jsToken{jsPunct, p})
				i += len(p)
				regexAllowed = punctAllowsRegexAfter(p)
				matched = true
				break
			}
		}
		if matched {
			atLineStart = false
			continue
		}

		// Single-character punctuator (catch-all).
		out = append(out, jsToken{jsPunct, string(c)})
		regexAllowed = punctAllowsRegexAfter(string(c))
		atLineStart = false
		i++
	}
	return out
}

// scanRegex attempts to lex a regex literal starting at body[start] == '/'.
// It tracks character-class brackets (where an unescaped '/' does not
// terminate the literal) and backslash escapes. Returns the literal text,
// the index just past it (including flags), and whether a well-formed
// literal was found.
func scanRegex(body string, start int) (string, int, bool) {
	n := len(body)
	i := start + 1
	inClass := false
	for i < n {
		c := body[i]
		if c == '\\' && i+1 < n {
			i += 2
			continue
		}
		if c == '\n' {
			return "", 0, false
		}
		if c == '[' {
			inClass = true
		} else if c == ']' {
			inClass = false
		} else if c == '/' && !inClass {
			i++
			for i < n && isIdentPart(body[i]) { // flags
				i++
			}
			return body[start:i], i, true
		}
		i++
	}
	return "", 0, false
}

func punctAllowsRegexAfter(p string) bool {
	switch p {
	case ")", "]", "}":
		return true // imprecise (block-end vs object-literal-end) but conservative: allows more regexes than divisions in practice
	case "++", "--":
		return false
	default:
		return true
	}
}

func isDigit(c byte) bool     { return c >= '0' && c <= '9' }
func isAlnum(c byte) bool     { return isDigit(c) || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
func isIdentPart(c byte) bool { return isIdentStart(c) || isDigit(c) }

// codeTokens drops comments, leaving code, string and regex tokens in
// document order. Callers that need to distinguish "in code position" from
// "inside a string/regex" still see both kinds here on purpose: strings are
// what srcAssignedDomains and tracker domain-fragment matching search, and
// leaving them in lets a single pass answer "does this string sit right
// after a .src= assignment" without a second walk.
func codeTokens(toks []jsToken) []jsToken {
	out := make([]jsToken, 0, len(toks))
	for _, t := range toks {
		if t.kind == jsComment {
			continue
		}
		out = append(out, t)
	}
	return out
}

// chainAt reports whether ct[i:] begins with the dotted identifier chain
// (each segment compared case-insensitively; ct's Ident text is already
// lowercased by the tokenizer). A "*" segment matches any single identifier,
// used for CMP namespace objects where the method name varies
// (Cookiebot.renew(), Cookiebot.show(), Cookiebot.consent = ...).
func chainAt(ct []jsToken, i int, chain []string) (end int, ok bool) {
	for si, seg := range chain {
		if si > 0 {
			if i >= len(ct) || ct[i].kind != jsPunct || ct[i].text != "." {
				return 0, false
			}
			i++
		}
		if i >= len(ct) || ct[i].kind != jsIdent {
			return 0, false
		}
		if seg != "*" && ct[i].text != seg {
			return 0, false
		}
		i++
	}
	return i, true
}

// codeChainUsed reports whether the dotted chain appears in CODE position
// (comments already excluded by codeTokens) immediately followed by a call
// `(` or by a plain assignment `=` that is not part of `==`/`===`/`!=` and
// is not a local `var`/`let`/`const` declaration shadowing the name. Either
// shape is evidence the page's own code references the object as callable
// or assigns to it; a bare property READ (`if (Cookiebot.consent.x)`) does
// not match, on purpose -- see consentSignatures' doc comment for why that
// distinction matters.
func codeChainUsed(ct []jsToken, chain []string) bool {
	for i := range ct {
		end, ok := chainAt(ct, i, chain)
		if !ok {
			continue
		}
		if end < len(ct) && ct[end].kind == jsPunct && ct[end].text == "(" {
			return true
		}
		if end < len(ct) && ct[end].kind == jsPunct && ct[end].text == "=" {
			if i > 0 && ct[i-1].kind == jsIdent {
				switch ct[i-1].text {
				case "var", "let", "const":
					continue // a local declaration shadowing the name, not an assignment to the real global
				}
			}
			return true
		}
	}
	return false
}

// identPrefixDeclared reports whether an identifier starting with prefix is
// used as a function name in a `function <name>(` declaration. This covers
// CMP-mandated callback naming conventions with a developer-chosen suffix,
// e.g. Cookiebot's `function CookiebotCallback_OnAccept() {...}`, which a
// dotted-chain signature cannot express.
func identPrefixDeclared(ct []jsToken, prefix string) bool {
	for i, t := range ct {
		if t.kind != jsIdent || !strings.HasPrefix(t.text, prefix) {
			continue
		}
		if i > 0 && ct[i-1].kind == jsIdent && ct[i-1].text == "function" {
			return true
		}
	}
	return false
}

// stringHaystack concatenates the contents of every String token
// (lowercased, comments and regex/code already excluded by construction)
// separated by newlines, for substring matching against known domain
// fragments -- the inline counterpart of a <script src> attribute, since a
// self-inserting bootstrap snippet's URL constant lives in a string
// literal, not a bare identifier.
func stringHaystack(ct []jsToken) string {
	var b strings.Builder
	for _, t := range ct {
		if t.kind == jsString {
			b.WriteString(t.text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// codeHaystack concatenates Ident/Punct/Number token text with no
// separators, comments AND string/regex contents excluded. It exists for
// the low-stakes tracker-alias substring matching (trackerInlineAliases),
// where a cheap flattened-text search is an acceptable trade for the
// simplicity it buys: a false positive here costs a tenant an investigation,
// not a fabricated compliance pass, so it does not need the same rigor as
// consent detection. Idents are already lowercased by the tokenizer.
func codeHaystack(ct []jsToken) string {
	var b strings.Builder
	for _, t := range ct {
		switch t.kind {
		case jsIdent, jsPunct, jsNumber:
			b.WriteString(t.text)
		}
	}
	return b.String()
}

// srcAssignedDomains returns the lowercased content of every string literal
// assigned to a property named "src" in code position: `x.src = "..."` or a
// bare `src = "..."`. This is the inline counterpart of a static
// `<script src=...>` attribute -- code telling the browser to fetch an
// exact URL, just constructed at runtime instead of written in markup.
// Several CMPs (Termly's official embed snippet among them) and the
// canonical GTM/Meta Pixel/LinkedIn bootstrap loaders all ship this way:
// create a <script> element, set its .src, insert it, with no static src=
// attribute anywhere in the page's HTML.
func srcAssignedDomains(ct []jsToken) []string {
	var out []string
	for i, t := range ct {
		if t.kind != jsIdent || t.text != "src" {
			continue
		}
		j := i + 1
		if j >= len(ct) || ct[j].kind != jsPunct || ct[j].text != "=" {
			continue
		}
		k := j + 1
		if k >= len(ct) || ct[k].kind != jsString {
			continue
		}
		out = append(out, ct[k].text)
	}
	return out
}
