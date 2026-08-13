package eval

import "testing"

// This file pins the lexer's token boundaries directly, independent of the
// privacy checks that consume it. It exists because the specific bug class
// this package's history is about -- a hand-rolled comment/string tracker
// that a verifier could bypass three different ways past stripScriptComments
// -- is a lexing bug, and lexing bugs are cheapest to pin at the token
// level, not only through the higher-level detection tests.

// codeChain is a small test helper: does chain appear in CODE position
// (comments excluded) as a call or assignment, per codeChainUsed.
func codeChainUsed_(t *testing.T, body string, chain ...string) bool {
	t.Helper()
	return codeChainUsed(codeTokens(tokenizeJS(body)), chain)
}

func TestTokenizeJSHTMLCommentOpenerHidesFollowingCode(t *testing.T) {
	// The first bypass a verifier found past stripScriptComments: `<!--` is
	// still a real single-line comment starter in ECMAScript (Annex B.1.3),
	// wherever it appears, not only at the start of a line. A naive
	// stripper that only recognises `//` and `/* */` leaves the text after
	// `<!--` completely unstripped, which is exactly what let a decoy
	// `cookiebot.init(` earn credit while never executing in a real
	// browser.
	body := `<!-- cookiebot.init( -->`
	if codeChainUsed_(t, body, "cookiebot", "*") {
		t.Fatal("text after <!-- must be treated as a comment, not code")
	}
}

func TestTokenizeJSRegexQuoteInteractionDoesNotLeakAComment(t *testing.T) {
	// The second bypass: a hand-rolled stripper that tracks quotes but not
	// regex literals misreads the '"' inside /["]/ as the START of a string,
	// then treats everything up to the next '"' -- including a REAL
	// following comment -- as "inside a string" and never strips it. A real
	// lexer has to recognise /["]/ as one regex token so the quote inside it
	// never enters quote-tracking state at all.
	body := "var re = /[\"]/;  // cookiebot.init(\nvar x = 1;"
	toks := tokenizeJS(body)
	var sawRegex, sawComment bool
	for _, tok := range toks {
		if tok.kind == jsRegex {
			sawRegex = true
		}
		if tok.kind == jsComment {
			sawComment = true
		}
	}
	if !sawRegex {
		t.Fatal("expected /[\"]/ to be tokenized as a single regex literal")
	}
	if !sawComment {
		t.Fatal("expected the // comment following the regex to be recognised as a comment")
	}
	if codeChainUsed_(t, body, "cookiebot", "*") {
		t.Fatal("cookiebot.init( only ever appears inside a comment; it must not read as code")
	}
}

func TestTokenizeJSInitCallInsideStringLiteralIsNotCode(t *testing.T) {
	// The third bypass: stripScriptComments deliberately preserved string
	// literals (so the '//' in an https:// URL string is not eaten as a
	// comment), but that means text INSIDE a string that happens to look
	// like a call was never distinguished from the call itself. A string
	// token's content must never contribute identifiers to the code stream.
	body := `var x = "cookiebot.init(";`
	if codeChainUsed_(t, body, "cookiebot", "*") {
		t.Fatal("cookiebot.init( inside a string literal must not read as a call")
	}
}

func TestTokenizeJSRealCallIsDetected(t *testing.T) {
	if !codeChainUsed_(t, "Cookiebot.renew();", "cookiebot", "*") {
		t.Fatal("a genuine call in code position must be detected")
	}
}

func TestTokenizeJSAssignmentToGlobalIsDetected(t *testing.T) {
	if !codeChainUsed_(t, "window.cookieproof = api;", "window", "cookieproof") {
		t.Fatal("assignment to a known global in code position must be detected")
	}
	if !codeChainUsed_(t, "window.termly = api;", "window", "termly") {
		t.Fatal("assignment to a known global in code position must be detected")
	}
}

func TestTokenizeJSEqualityCheckIsNotAnAssignment(t *testing.T) {
	// `===` must not be split into `==` `=` (or `=` `=` `=`), which would
	// misread a comparison as an assignment and fabricate weak evidence
	// from `if (window.cookieproof === undefined)`.
	if codeChainUsed_(t, "if (window.cookieproof === undefined) {}", "window", "cookieproof") {
		t.Fatal("a strict-equality comparison must not be read as an assignment")
	}
}

func TestTokenizeJSLocalDeclarationIsNotAssignmentToGlobal(t *testing.T) {
	// `var __tcfapi = null;` declares a LOCAL that shadows the name; it is
	// not an assignment to the real global IAB TCF entry point.
	if codeChainUsed_(t, "var __tcfapi = null;", "__tcfapi") {
		t.Fatal("a var declaration must not be read as an assignment to the global")
	}
	// But a bare assignment (no var/let/const) to the same name IS the real
	// shape a loaded TCF stub takes.
	if !codeChainUsed_(t, "window.__tcfapi = function(){};", "__tcfapi") {
		t.Fatal("expected the wildcard-free single-token chain to match a direct assignment")
	}
}

func TestTokenizeJSBlockCommentDoesNotHideTheCodeAfterIt(t *testing.T) {
	body := "/* cookiebot.init( */ gtag('config','G-X');"
	if codeChainUsed_(t, body, "cookiebot", "*") {
		t.Fatal("text inside a block comment must not read as code")
	}
	toks := codeTokens(tokenizeJS(body))
	if !strContains(codeHaystack(toks), "gtag(") {
		t.Fatal("real code after a block comment must still be tokenized")
	}
}

func strContains(hay, needle string) bool {
	return len(hay) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(hay); i++ {
			if hay[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
