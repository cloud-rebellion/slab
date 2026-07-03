package critique

import (
	"strings"
	"testing"

	"github.com/bright-interaction/slab/internal/agent"
)

// TestBlockLint_HeroQualityFires asserts the headline write-time check:
// a hero with no graphic-bearing field flags before the next build.
func TestBlockLint_HeroQualityFires(t *testing.T) {
	playbook := agent.DefaultDesignPlaybook()
	findings := LintBlockData("hero", map[string]any{
		"headline":   "Stop renting your business",
		"subheading": "Own it",
		"cta_text":   "Calculate",
		"cta_url":    "/calc",
	}, playbook)
	if !hasFinding(findings, "hero_quality") {
		t.Fatalf("expected hero_quality finding; got %s", debugLint(findings))
	}
}

// TestBlockLint_HeroQualityClearsWithGraphic confirms picking any of
// hero_graphic / bg=circuit / image_id clears the check.
func TestBlockLint_HeroQualityClearsWithGraphic(t *testing.T) {
	playbook := agent.DefaultDesignPlaybook()
	cases := []map[string]any{
		{"headline": "Own it", "hero_graphic": "mesh"},
		{"headline": "Own it", "bg": "circuit"},
		{"headline": "Own it", "image_id": "media-abc"},
	}
	for i, data := range cases {
		findings := LintBlockData("hero", data, playbook)
		if hasFinding(findings, "hero_quality") {
			t.Errorf("case %d: hero_quality fired when it should have been cleared: %s", i, debugLint(findings))
		}
	}
}

// TestBlockLint_HeadlineLengthFlagsOnOverlong locks the 12-word cap.
func TestBlockLint_HeadlineLengthFlagsOnOverlong(t *testing.T) {
	playbook := agent.DefaultDesignPlaybook()
	findings := LintBlockData("hero", map[string]any{
		"hero_graphic": "mesh",
		"headline":     "We make really great products that help businesses scale their teams without paying for unused seats every month",
	}, playbook)
	if !hasFinding(findings, "headline_length") {
		t.Fatalf("expected headline_length finding for 19-word hero; got %s", debugLint(findings))
	}
}

// TestBlockLint_HeadlineLengthClearsAtCap confirms the boundary: 12
// words is fine, 13 trips. Belts the off-by-one.
func TestBlockLint_HeadlineLengthClearsAtCap(t *testing.T) {
	playbook := agent.DefaultDesignPlaybook()
	twelve := "one two three four five six seven eight nine ten eleven twelve"
	findings := LintBlockData("hero", map[string]any{
		"hero_graphic": "mesh",
		"headline":     twelve,
	}, playbook)
	if hasFinding(findings, "headline_length") {
		t.Errorf("12-word headline should pass; got %s", debugLint(findings))
	}
}

// TestBlockLint_CustomBlockDuplicateEyebrow catches MCP gotcha #1
// before the renderer emits the duplicate.
func TestBlockLint_CustomBlockDuplicateEyebrow(t *testing.T) {
	playbook := agent.DefaultDesignPlaybook()
	findings := LintBlockData("custom", map[string]any{
		"name":    "trust-block",
		"eyebrow": "EU JURISDICTION",
		"markup":  `<div class="bi-trust"><p class="eyebrow">EU JURISDICTION</p><h2>...</h2></div>`,
	}, playbook)
	if !hasFinding(findings, "custom_block_duplicate_eyebrow") {
		t.Fatalf("expected custom_block_duplicate_eyebrow; got %s", debugLint(findings))
	}
}

// TestBlockLint_SlopTermInCopy catches the playbook's banned-phrase
// terms across any string-valued field.
func TestBlockLint_SlopTermInCopy(t *testing.T) {
	playbook := agent.DefaultDesignPlaybook()
	// "Elevate" is on the banned list.
	findings := LintBlockData("hero", map[string]any{
		"hero_graphic": "mesh",
		"headline":     "Elevate your team",
		"subheading":   "Own it",
	}, playbook)
	if !hasFindingForField(findings, "slop_term", "headline") {
		t.Fatalf("expected slop_term on headline; got %s", debugLint(findings))
	}
}

// TestBlockLint_NestedSlopScan confirms the walk visits items[] inside
// stat_grid / feature_grid / etc, not just top-level fields.
func TestBlockLint_NestedSlopScan(t *testing.T) {
	playbook := agent.DefaultDesignPlaybook()
	findings := LintBlockData("stat_grid", map[string]any{
		"heading": "Our numbers",
		"items": []any{
			map[string]any{"value": "99.99%", "label": "Uptime"},
		},
	}, playbook)
	// "99.99%" is on the banned-numbers list.
	if !hasFinding(findings, "slop_number") {
		t.Fatalf("expected slop_number for 99.99%%; got %s", debugLint(findings))
	}
}

// TestBlockLint_NonHeroDoesNotFireHeroChecks confirms checks scoped to
// hero / split_hero do NOT fire on other block types.
func TestBlockLint_NonHeroDoesNotFireHeroChecks(t *testing.T) {
	playbook := agent.DefaultDesignPlaybook()
	findings := LintBlockData("text", map[string]any{
		"heading": "Why open source",
		"text":    "Receipts not promises.",
	}, playbook)
	if hasFinding(findings, "hero_quality") || hasFinding(findings, "headline_length") {
		t.Errorf("hero checks should not fire on text block; got %s", debugLint(findings))
	}
}

// TestBlockLint_ArchetypeDriftFires confirms the gap-6 archetype-lock
// check fires when a hero's hero_graphic does not fit the page's
// archetype.
func TestBlockLint_ArchetypeDriftFires(t *testing.T) {
	playbook := agent.DefaultDesignPlaybook()
	findings := LintBlockDataWithArchetype("hero", map[string]any{
		"hero_graphic": "mesh", // mesh-style graphic on an audit-receipt page
		"headline":     "Own it",
	}, "audit-receipt", playbook)
	if !hasFinding(findings, "archetype_drift") {
		t.Fatalf("expected archetype_drift; got %s", debugLint(findings))
	}
}

// TestBlockLint_ArchetypeDriftClearsOnMatch confirms an in-archetype
// hero_graphic clears the lint.
func TestBlockLint_ArchetypeDriftClearsOnMatch(t *testing.T) {
	playbook := agent.DefaultDesignPlaybook()
	findings := LintBlockDataWithArchetype("hero", map[string]any{
		"hero_graphic": "audit-receipt",
		"headline":     "Own it",
	}, "audit-receipt", playbook)
	if hasFinding(findings, "archetype_drift") {
		t.Errorf("archetype_drift should clear on match; got %s", debugLint(findings))
	}
}

// TestBlockLint_ArchetypeEmptyDisablesCheck confirms that passing an
// empty archetype (no lock) skips the drift check entirely.
func TestBlockLint_ArchetypeEmptyDisablesCheck(t *testing.T) {
	playbook := agent.DefaultDesignPlaybook()
	findings := LintBlockDataWithArchetype("hero", map[string]any{
		"hero_graphic": "anything-weird",
		"headline":     "Own it",
	}, "", playbook)
	if hasFinding(findings, "archetype_drift") {
		t.Errorf("archetype_drift should not fire when no lock; got %s", debugLint(findings))
	}
}

// TestBlockLint_ArchetypeUnknownIsIgnored confirms an unknown
// archetype falls through quietly (no false positives). New
// archetypes should be added to archetypeHeroGraphics.
func TestBlockLint_ArchetypeUnknownIsIgnored(t *testing.T) {
	playbook := agent.DefaultDesignPlaybook()
	findings := LintBlockDataWithArchetype("hero", map[string]any{
		"hero_graphic": "mesh",
		"headline":     "Own it",
	}, "future-archetype-not-in-map", playbook)
	if hasFinding(findings, "archetype_drift") {
		t.Errorf("unknown archetype should not fire drift; got %s", debugLint(findings))
	}
}

// TestBlockLint_ProductDetailMissingSlug locks the Slice C check:
// a product_detail block with an empty product_slug field must flag.
func TestBlockLint_ProductDetailMissingSlug(t *testing.T) {
	playbook := agent.DefaultDesignPlaybook()
	findings := LintBlockData("product_detail", map[string]any{
		"show_variants": true,
	}, playbook)
	if !hasFinding(findings, "product_detail_slug_missing") {
		t.Fatalf("expected product_detail_slug_missing; got %s", debugLint(findings))
	}
}

func TestBlockLint_ProductDetailSlugClears(t *testing.T) {
	playbook := agent.DefaultDesignPlaybook()
	findings := LintBlockData("product_detail", map[string]any{
		"product_slug": "blue-tee",
	}, playbook)
	if hasFinding(findings, "product_detail_slug_missing") {
		t.Errorf("product_detail_slug_missing should clear when slug is set; got %s", debugLint(findings))
	}
}

// TestBlockLint_CheckoutFormReturnURL locks the missing return_url
// warning. Mollie cannot redirect without one.
func TestBlockLint_CheckoutFormReturnURL(t *testing.T) {
	playbook := agent.DefaultDesignPlaybook()
	findings := LintBlockData("checkout_form", map[string]any{
		"heading": "Checkout",
	}, playbook)
	if !hasFinding(findings, "checkout_return_url_missing") {
		t.Fatalf("expected checkout_return_url_missing; got %s", debugLint(findings))
	}
}

func TestBlockLint_CheckoutFormReturnURLClears(t *testing.T) {
	playbook := agent.DefaultDesignPlaybook()
	findings := LintBlockData("checkout_form", map[string]any{
		"heading":    "Checkout",
		"return_url": "/thank-you",
	}, playbook)
	if hasFinding(findings, "checkout_return_url_missing") {
		t.Errorf("checkout_return_url_missing should clear when return_url is set; got %s", debugLint(findings))
	}
}

// TestBlockLint_ProductGridLimitHigh asserts the info-level warning when
// the author sets a high limit that the renderer would cap at 48.
func TestBlockLint_ProductGridLimitHigh(t *testing.T) {
	playbook := agent.DefaultDesignPlaybook()
	findings := LintBlockData("product_grid", map[string]any{
		"heading": "All products",
		"limit":   float64(60),
	}, playbook)
	if !hasFinding(findings, "product_grid_limit_high") {
		t.Fatalf("expected product_grid_limit_high for limit=60; got %s", debugLint(findings))
	}
}

func TestBlockLint_ProductGridLimitNormal(t *testing.T) {
	playbook := agent.DefaultDesignPlaybook()
	findings := LintBlockData("product_grid", map[string]any{
		"heading": "Featured",
		"limit":   float64(12),
	}, playbook)
	if hasFinding(findings, "product_grid_limit_high") {
		t.Errorf("product_grid_limit_high should not fire at limit=12; got %s", debugLint(findings))
	}
}

func hasFinding(findings []LintFinding, name string) bool {
	for _, f := range findings {
		if f.Name == name {
			return true
		}
	}
	return false
}

func hasFindingForField(findings []LintFinding, name, field string) bool {
	for _, f := range findings {
		if f.Name == name && f.Field == field {
			return true
		}
	}
	return false
}

func debugLint(findings []LintFinding) string {
	if len(findings) == 0 {
		return "[]"
	}
	parts := make([]string, len(findings))
	for i, f := range findings {
		parts[i] = f.Name + "@" + f.Field
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
