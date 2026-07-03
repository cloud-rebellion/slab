package builder

import (
	"strings"
	"testing"
)

// Sprint 5 quick-win (2026-05-24): search_box renderer smoke tests.
// The runtime relies on dist/pagefind/ produced by the post-build
// pagefind pass; the renderer itself just emits the mount + loader.

func TestRenderSearchBoxBlock_EmitsMountAndLoader(t *testing.T) {
	out := renderSearchBoxBlock(map[string]any{"placeholder": "Find docs"})
	for _, want := range []string{
		`id="search"`,
		`/pagefind/pagefind-ui.js`,
		`/pagefind/pagefind-ui.css`,
		`new PagefindUI`,
		`Find docs`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output; got:\n%s", want, out)
		}
	}
}

func TestRenderSearchBoxBlock_DefaultPlaceholder(t *testing.T) {
	out := renderSearchBoxBlock(map[string]any{})
	if !strings.Contains(out, "Search the site") {
		t.Errorf("expected default placeholder; got:\n%s", out)
	}
}

func TestRenderSearchBoxBlock_ShowEmptyToggle(t *testing.T) {
	// show_empty_state=false should drop the zero-results copy entirely.
	out := renderSearchBoxBlock(map[string]any{"show_empty_state": false})
	if strings.Contains(out, "No results") {
		t.Errorf("zero-results copy should not render when show_empty_state=false; got:\n%s", out)
	}
}

// TestRenderSearchBoxBlock_ViewTransitionSafeInit pins the script
// lifecycle contract: init must be idempotent (mount marker guard) and
// re-run on astro:page-load, or search dies after the first soft
// navigation on showcase (ClientRouter) sites.
func TestRenderSearchBoxBlock_ViewTransitionSafeInit(t *testing.T) {
	out := renderSearchBoxBlock(map[string]any{})
	for _, want := range []string{
		`astro:page-load`,
		`data-pagefind-init`,
		`document.readyState`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in init script (view-transition safety); got:\n%s", want, out)
		}
	}
}
