package builder

import (
	"strings"
	"testing"

	"github.com/bright-interaction/slab/internal/store"
)

// TestRenderBlock_DataBlock confirms the default data-driven branch emits a
// section with heading, subheading, body, and CTA when the matching keys
// are present in data_json. This is the source-of-truth output the new
// admin "</>" code panel surfaces back to developers.
func TestRenderBlock_DataBlock(t *testing.T) {
	bl := store.Block{
		ID:        "blk-1",
		BlockType: "hero",
		DataJson: `{
			"eyebrow": "Calm",
			"headline": "A calm site",
			"subheading": "that earns the click",
			"cta_text": "Book a discovery call",
			"cta_url": "/book",
			"secondary_label": "Read more",
			"secondary_url": "/about"
		}`,
	}
	out := renderBlock(bl, map[string]string{}, nil, "")

	wantSubstrings := []string{
		`class="block block--hero"`,
		`id="a-calm-site"`,
		`<p class="eyebrow">Calm</p>`,
		`<h1>A calm site</h1>`,
		`<p class="subheading">that earns the click</p>`,
		`href="/book"`,
		`>Book a discovery call</a>`,
		`href="/about"`,
		`>Read more</a>`,
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(out, s) {
			t.Errorf("renderBlock output missing %q\nfull output:\n%s", s, out)
		}
	}
}

// TestRenderBlock_HTMLPassthrough confirms a block carrying a `html` key in
// its data_json renders that HTML verbatim instead of the data-driven
// section markup. Used by raw-HTML escape hatch blocks.
func TestRenderBlock_HTMLPassthrough(t *testing.T) {
	bl := store.Block{
		ID:        "blk-2",
		BlockType: "raw",
		DataJson:  `{"html": "<aside class=\"banner\">hello</aside>"}`,
	}
	out := renderBlock(bl, map[string]string{}, nil, "")
	if !strings.Contains(out, `<aside class="banner">hello</aside>`) {
		t.Errorf("expected raw HTML passthrough, got: %q", out)
	}
}

// TestRenderBlock_ComponentBlock confirms that when data_json declares a
// component name AND that component exists for the site, renderBlock emits
// a self-closing PascalCase tag with props rather than a data-driven section.
func TestRenderBlock_ComponentBlock(t *testing.T) {
	bl := store.Block{
		ID:        "blk-3",
		BlockType: "component",
		DataJson: `{
			"component": "feature-card",
			"props": {"title": "Fast", "icon": "bolt", "highlight": true}
		}`,
	}
	out := renderBlock(bl, map[string]string{"feature-card": ".astro"}, nil, "")
	if !strings.Contains(out, "<FeatureCard") {
		t.Errorf("expected PascalCase component tag, got: %q", out)
	}
	if !strings.Contains(out, `title="Fast"`) {
		t.Errorf("expected title prop in output, got: %q", out)
	}
	if !strings.Contains(out, "highlight") {
		t.Errorf("expected boolean highlight prop in output, got: %q", out)
	}
}

// TestRenderBlock_PersonalizationCondition wraps blocks whose data carries
// a non-empty `condition` string in the data-asp-when div hidden by default;
// the visitor-hydration script removes the hidden attribute when the DSL
// matches at render time. Empty condition → no wrapper.
func TestRenderBlock_PersonalizationCondition(t *testing.T) {
	wrapped := store.Block{
		ID:        "blk-4",
		BlockType: "hero",
		DataJson:  `{"heading": "Welcome back", "condition": "lifecycle == \"customer\""}`,
	}
	out := renderBlock(wrapped, map[string]string{}, nil, "")
	if !strings.Contains(out, `data-asp-when=`) {
		t.Errorf("expected condition wrapper when data.condition is set, got: %q", out)
	}
	if !strings.Contains(out, ` hidden>`) {
		t.Errorf("expected hidden attr on conditional wrapper, got: %q", out)
	}

	plain := store.Block{
		ID:        "blk-5",
		BlockType: "hero",
		DataJson:  `{"heading": "Welcome", "condition": ""}`,
	}
	out2 := renderBlock(plain, map[string]string{}, nil, "")
	if strings.Contains(out2, `data-asp-when=`) {
		t.Errorf("expected no wrapper when condition is empty, got: %q", out2)
	}
}

// TestRenderBlock_HTMLEscape confirms the data-driven branch escapes HTML
// metacharacters in heading and body so a stored "<script>" in data_json
// can never escape into the rendered Astro source.
func TestRenderBlock_HTMLEscape(t *testing.T) {
	bl := store.Block{
		ID:        "blk-6",
		BlockType: "text",
		DataJson:  `{"heading": "<script>alert(1)</script>", "text": "AT&T"}`,
	}
	out := renderBlock(bl, map[string]string{}, nil, "")
	if strings.Contains(out, "<script>") {
		t.Errorf("renderBlock leaked unescaped <script> tag: %q", out)
	}
	if !strings.Contains(out, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("expected escaped <script> in heading, got: %q", out)
	}
	if !strings.Contains(out, "AT&amp;T") {
		t.Errorf("expected ampersand-escaped text, got: %q", out)
	}
}

// TestRenderBlock_RejectsDangerousScheme confirms cta_url containing
// javascript:/data:/vbscript: gets rewritten to "#" so a malicious agent
// payload can't slip a clickable XSS into the rendered output.
func TestRenderBlock_RejectsDangerousScheme(t *testing.T) {
	bl := store.Block{
		ID:        "blk-7",
		BlockType: "cta",
		DataJson:  `{"cta_text": "Click", "cta_url": "javascript:alert(1)"}`,
	}
	out := renderBlock(bl, map[string]string{}, nil, "")
	if strings.Contains(out, "javascript:") {
		t.Errorf("dangerous javascript: URL leaked: %q", out)
	}
	if !strings.Contains(out, `href="#"`) {
		t.Errorf("expected fallback href=\"#\" for blocked scheme: %q", out)
	}
}

// TestRenderPage_Wrapping confirms the page wrapper assembles the Base
// import, title/description props, and renders each visible block in order.
// This is the same output the admin page-level "View source" dialog
// fetches from /api/sites/{id}/pages/{id}/preview.
func TestRenderPage_Wrapping(t *testing.T) {
	page := store.Page{
		ID:              "pg-1",
		Slug:            "about",
		Title:           "About",
		MetaTitle:       "About | Acme",
		MetaDescription: "Who we are.",
	}
	blocks := []store.Block{
		{
			ID:        "b1",
			BlockType: "text",
			SortOrder: 0,
			IsVisible: 1,
			DataJson:  `{"heading": "First"}`,
		},
		{
			ID:        "b2",
			BlockType: "text",
			SortOrder: 1,
			IsVisible: 0,
			DataJson:  `{"heading": "Hidden"}`,
		},
		{
			ID:        "b3",
			BlockType: "text",
			SortOrder: 2,
			IsVisible: 1,
			DataJson:  `{"heading": "Second"}`,
		},
	}
	out := renderPage(page, blocks, map[string]string{})

	// Frontmatter + Base wrapper + props
	if !strings.Contains(out, "import Base from '../layouts/Base.astro'") {
		t.Errorf("missing Base import: %q", out)
	}
	if !strings.Contains(out, `<Base title="About | Acme"`) {
		t.Errorf("missing title prop: %q", out)
	}
	if !strings.Contains(out, ` description="Who we are."`) {
		t.Errorf("missing description prop: %q", out)
	}

	// Visible blocks rendered in sort order; hidden block skipped.
	firstIdx := strings.Index(out, "<h2>First</h2>")
	secondIdx := strings.Index(out, "<h2>Second</h2>")
	if firstIdx == -1 || secondIdx == -1 {
		t.Fatalf("expected both visible block headings in output, got: %q", out)
	}
	if firstIdx >= secondIdx {
		t.Errorf("expected First to render before Second, got First@%d, Second@%d", firstIdx, secondIdx)
	}
	if strings.Contains(out, "<h2>Hidden</h2>") {
		t.Errorf("hidden block leaked into rendered page: %q", out)
	}
}

// TestRenderPage_NoIndexEmitsRobots confirms a page flagged no_index=1 gets
// the robots="noindex, nofollow" prop on the Base wrapper, so atomicsite's
// auto-noindex flow surfaces in the rendered Astro.
func TestRenderPage_NoIndexEmitsRobots(t *testing.T) {
	page := store.Page{
		ID:      "pg-2",
		Slug:    "draft",
		Title:   "Draft",
		NoIndex: 1,
	}
	out := renderPage(page, []store.Block{}, map[string]string{})
	if !strings.Contains(out, `robots="noindex, nofollow"`) {
		t.Errorf("expected robots prop on noindex page: %q", out)
	}
}

// TestRenderPage_ImportPathDepth confirms nested slugs use the right number
// of `../` segments to reach src/. /tjanster/avtal must resolve
// `../../layouts/Base.astro`. A regression here breaks astro build.
func TestRenderPage_ImportPathDepth(t *testing.T) {
	page := store.Page{ID: "pg-3", Slug: "tjanster/avtal", Title: "Avtal"}
	out := renderPage(page, []store.Block{}, map[string]string{})
	if !strings.Contains(out, "import Base from '../../layouts/Base.astro'") {
		t.Errorf("expected ../../layouts/Base.astro for depth-1 slug, got: %q", out)
	}
}
