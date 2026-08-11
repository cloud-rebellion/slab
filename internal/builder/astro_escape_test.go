package builder

import (
	"strings"
	"testing"
)

// The escaping engine has four exits because .astro is four output languages,
// and the security half of this is cheap to get right while the preservation
// half is what actually breaks customer sites. This file is mostly the second
// half: the exits must leave ordinary content alone, byte for byte.

func TestAstroEscapeExits(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		// Exit 1, markup text. Braces neutralise, ordinary text untouched.
		{"text ordinary", escapeTextForAstro("Fran 1 200 kr i manaden"), "Fran 1 200 kr i manaden"},
		{"text brace", escapeTextForAstro("{fr. 1 200 kr}"), "&#123;fr. 1 200 kr&#125;"},
		{"text markup", escapeTextForAstro(`<b>&</b>`), "&lt;b&gt;&amp;&lt;/b&gt;"},

		// Exit 2, quoted attribute.
		{"attr ordinary", escapeAttrForAstro("hero-image wide"), "hero-image wide"},
		{"attr quote", escapeAttrForAstro(`" onload="alert(1)`), "&quot; onload=&quot;alert(1)"},
		{"attr brace", escapeAttrForAstro("{x}"), "&#123;x&#125;"},

		// Exit 3, script half. Braces survive because Astro does not evaluate
		// them there and an entity would not be decoded there.
		{"js ordinary", escapeJSStringForAstro("Sok produkter & tjanster"), "Sok produkter & tjanster"},
		{"js brace", escapeJSStringForAstro("{a:1}"), "{a:1}"},
		{"js quote", escapeJSStringForAstro(`it's`), `it\'s`},
		{"js backslash", escapeJSStringForAstro(`C:\x`), `C:\\x`},
		{"js script close", escapeJSStringForAstro(`</script><img src=x onerror=alert(1)>`), `\u003c/script\u003e\u003cimg src=x onerror=alert(1)\u003e`},
		{"js newline", escapeJSStringForAstro("a\nb"), `a\nb`},

		// Exit 3, style half.
		{"css ordinary", escapeCSSStringForAstro("Inter Display"), "Inter Display"},
		{"css brace", escapeCSSStringForAstro("a{b}"), "a{b}"},
		{"css style close", escapeCSSStringForAstro(`</style><img src=x>`), `\3c /style\3e \3c img src=x\3e `},
		{"css quote", escapeCSSStringForAstro(`a"b`), `a\"b`},

		// Exit 4, component prop. Deliberately no brace neutralisation.
		{"prop ordinary", escapeAstroPropString("Om oss"), "Om oss"},
		{"prop brace", escapeAstroPropString("{x}"), "{x}"},
		{"prop quote", escapeAstroPropString(`a"b`), "a&quot;b"},

		// The font-style slot is a keyword, not a string, so it is clamped.
		{"font style italic", cssFontStyleKeyword("Italic"), "italic"},
		{"font style junk", cssFontStyleKeyword("normal; } body { display:none } @x{"), "normal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("want %q\ngot  %q", tt.want, tt.got)
			}
		})
	}
}

// jsScan walks JavaScript source tracking string literals and comments, so an
// escaping bug that terminates a literal early surfaces as an unterminated or
// unbalanced scan instead of as a silent break in a browser. It does not
// understand regex literals, which the scripts this package writes do not use.
func jsScan(src string) (balanced bool, reason string) {
	var depth int
	var i int
	for i < len(src) {
		c := src[i]
		switch {
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return false, "unterminated block comment"
			}
			i += end + 4
		case c == '\'' || c == '"' || c == '`':
			quote := c
			i++
			for {
				if i >= len(src) {
					return false, "unterminated string literal starting with " + string(quote)
				}
				if src[i] == '\\' {
					i += 2
					continue
				}
				if src[i] == quote {
					i++
					break
				}
				if src[i] == '\n' && quote != '`' {
					return false, "newline inside a " + string(quote) + " string literal"
				}
				i++
			}
		case c == '{' || c == '(' || c == '[':
			depth++
			i++
		case c == '}' || c == ')' || c == ']':
			depth--
			if depth < 0 {
				return false, "closed a delimiter that was never opened"
			}
			i++
		default:
			i++
		}
	}
	if depth != 0 {
		return false, "unbalanced delimiters"
	}
	return true, ""
}

func scriptBodies(t *testing.T, out string) []string {
	t.Helper()
	var bodies []string
	for _, m := range rawTextElementRE.FindAllStringSubmatch(out, -1) {
		if strings.EqualFold(m[1], "script") {
			bodies = append(bodies, m[2])
		}
	}
	return bodies
}

// TestSearchBoxInlineScriptStaysValidJS is the preservation test for exit 3's
// script half. The search block writes an inline script and interpolates the
// tenant's placeholder into a JS string literal, so this is the one place where
// applying the markup exit would produce &#123; in running JavaScript.
func TestSearchBoxInlineScriptStaysValidJS(t *testing.T) {
	cases := []struct {
		name        string
		placeholder string
		wantInJS    string
	}{
		{"ordinary swedish copy", "Sok produkter & tjanster", "Sok produkter & tjanster"},
		{"apostrophe", "What's here?", `What\'s here?`},
		{"braces stay literal", "search {anything}", "search {anything}"},
		{"script breakout attempt", `</script><img src=x onerror=alert(document.domain)>`, `\u003c/script\u003e`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := renderDataBlock("search_box", map[string]any{"placeholder": tc.placeholder}, nil, "site-1")
			bodies := scriptBodies(t, out)
			if len(bodies) == 0 {
				t.Fatalf("search_box emitted no <script> body:\n%s", out)
			}
			joined := strings.Join(bodies, "\n")
			if !strings.Contains(joined, tc.wantInJS) {
				t.Errorf("placeholder did not survive into the script as %q:\n%s", tc.wantInJS, joined)
			}
			if strings.Contains(joined, "&#") {
				t.Errorf("an HTML entity reached the script body, which nothing decodes:\n%s", joined)
			}
			for _, body := range bodies {
				if ok, why := jsScan(body); !ok {
					t.Errorf("script body is not valid JavaScript (%s):\n%s", why, body)
				}
			}
			// The inline config object's own braces must still be there, or the
			// script is dead even when it parses.
			if !strings.Contains(joined, "new PagefindUI({") {
				t.Errorf("the PagefindUI call lost its object literal:\n%s", joined)
			}
		})
	}
}

// TestFontFaceStyleStaysValidCSS is the preservation test for exit 3's style
// half, over the real SiteFontsCSS emission.
func TestFontFaceStyleStaysValidCSS(t *testing.T) {
	got := renderFontFaceForTest("Inter Display", "italic", "/atomicsite-fonts/s1/f1.woff2")
	want := `@font-face { font-family: "Inter Display"; font-style: italic; font-weight: 400; font-display: swap; src: url("/atomicsite-fonts/s1/f1.woff2") format('woff2'); }`
	if strings.TrimSpace(got) != want {
		t.Errorf("ordinary font row changed shape\nwant: %s\ngot:  %s", want, strings.TrimSpace(got))
	}

	hostile := renderFontFaceForTest(`</style><img src=x onerror=alert(1)>`, `normal; } html { display: none } @media all {`, "/atomicsite-fonts/s1/f2.woff2")
	if strings.Contains(strings.ToLower(hostile), "</style") {
		t.Errorf("a font family closed the <style> element:\n%s", hostile)
	}
	if strings.Contains(hostile, "display: none") {
		t.Errorf("the font-style slot took arbitrary CSS:\n%s", hostile)
	}
	if !strings.Contains(hostile, "font-style: normal;") {
		t.Errorf("an unrecognised font-style did not clamp to normal:\n%s", hostile)
	}
	if strings.Contains(hostile, "&#") {
		t.Errorf("an HTML entity reached CSS text, which nothing decodes:\n%s", hostile)
	}
}

// TestTypedBlockOrdinaryTextIsUnchanged pins two typed blocks byte for byte on
// ordinary tenant copy. The sweep touched every renderer in the package, and
// this is the assertion that says it changed nothing a customer would see.
func TestTypedBlockOrdinaryTextIsUnchanged(t *testing.T) {
	hero := renderDataBlock("hero", map[string]any{
		"eyebrow":    "Priser",
		"headline":   "Snabb hemsida for smaforetag",
		"subheading": "Fran 1 200 kr i manaden, utan bindningstid.",
		"cta_text":   "Kom igang",
	}, nil, "site-1")
	wantHero := "  <section class=\"block block--hero\">\n" +
		"    <p class=\"eyebrow\">Priser</p>\n" +
		"    <h1>Snabb hemsida for smaforetag</h1>\n" +
		"    <p class=\"subheading\">Fran 1 200 kr i manaden, utan bindningstid.</p>\n" +
		"    <div class=\"hero-actions\">\n" +
		"      <a href=\"#\" class=\"btn-primary\">Kom igang</a>\n" +
		"    </div>\n" +
		"  </section>\n"
	if hero != wantHero {
		t.Errorf("hero block changed shape\nwant:\n%s\ngot:\n%s", wantHero, hero)
	}

	pricing := renderDataBlock("pricing", map[string]any{"heading": "Vad kostar det?"}, nil, "site-1")
	wantPricing := "  <section class=\"block block--pricing\">\n    <h2>Vad kostar det?</h2>\n  </section>\n"
	if pricing != wantPricing {
		t.Errorf("pricing block changed shape\nwant:\n%s\ngot:\n%s", wantPricing, pricing)
	}
}

// TestTypedBlockBraceTextRendersAsText is the non-security half at a typed
// block: a Swedish price label in an ordinary heading used to be a build-time
// expression and failed the tenant's whole build.
func TestTypedBlockBraceTextRendersAsText(t *testing.T) {
	out := renderDataBlock("hero", map[string]any{"headline": "Fran {fr. 1 200 kr} i manaden"}, nil, "site-1")
	if !strings.Contains(out, "<h1>Fran &#123;fr. 1 200 kr&#125; i manaden</h1>") {
		t.Errorf("literal braces in a heading did not render as text:\n%s", out)
	}
}

// renderFontFaceForTest exercises the real SiteFontsCSS emission shape without
// a database, by calling the same formatting the builder uses.
func renderFontFaceForTest(family, style, src string) string {
	return fontFaceDecl(family, style, 400, src, "")
}
