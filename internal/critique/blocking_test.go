package critique

import (
	"testing"

	"github.com/bright-interaction/slab/internal/agent"
)

// TestHasBlockingFindings covers the curated allowlist of finding names that
// are tripwires in strict mode. Soft warnings stay non-blocking.
func TestHasBlockingFindings(t *testing.T) {
	cases := []struct {
		name     string
		findings []LintFinding
		want     bool
	}{
		{"empty", nil, false},
		{"only soft warning", []LintFinding{
			{Name: "hero_quality", Severity: "warning"},
			{Name: "headline_length", Severity: "warning"},
		}, false},
		{"slop term blocks", []LintFinding{
			{Name: "hero_quality", Severity: "warning"},
			{Name: "slop_term", Severity: "warning"},
		}, true},
		{"slop name blocks", []LintFinding{{Name: "slop_name"}}, true},
		{"slop company blocks", []LintFinding{{Name: "slop_company"}}, true},
		{"slop number blocks", []LintFinding{{Name: "slop_number"}}, true},
		{"archetype drift blocks", []LintFinding{{Name: "archetype_drift"}}, true},
		{"unknown name does not block", []LintFinding{{Name: "future_rule_we_did_not_curate"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := HasBlockingFindings(c.findings); got != c.want {
				t.Errorf("HasBlockingFindings(%+v) = %v, want %v", c.findings, got, c.want)
			}
		})
	}
}

func TestFilterBlocking(t *testing.T) {
	input := []LintFinding{
		{Name: "hero_quality"},
		{Name: "slop_term", Field: "headline"},
		{Name: "headline_length"},
		{Name: "archetype_drift", Field: "hero_graphic"},
	}
	got := FilterBlocking(input)
	if len(got) != 2 {
		t.Fatalf("expected 2 blocking findings, got %d", len(got))
	}
	if got[0].Name != "slop_term" || got[1].Name != "archetype_drift" {
		t.Errorf("blocking order changed: %+v", got)
	}
}

// TestFilterBlockingFor_ShowcaseDropsArchetypeDriftKeepsSlop pins the
// fidelity contract on the write gate: showcase demotes archetype_drift
// (an off-archetype bespoke hero is the point of showcase) but slop_*
// blocks in every fidelity.
func TestFilterBlockingFor_ShowcaseDropsArchetypeDriftKeepsSlop(t *testing.T) {
	input := []LintFinding{
		{Name: "slop_term", Field: "headline"},
		{Name: "archetype_drift", Field: "hero_graphic"},
		{Name: "slop_number", Field: "stats"},
	}
	got := FilterBlockingFor(input, agent.FidelityShowcase)
	if len(got) != 2 {
		t.Fatalf("showcase should block exactly the 2 slop findings, got %d: %+v", len(got), got)
	}
	for _, f := range got {
		if f.Name == "archetype_drift" {
			t.Fatal("archetype_drift must not block in showcase")
		}
	}
	// Balanced + performance keep all three.
	for _, fid := range []agent.DesignFidelity{agent.FidelityBalanced, agent.FidelityPerformance} {
		if got := FilterBlockingFor(input, fid); len(got) != 3 {
			t.Fatalf("%s should block all 3 findings, got %d", fid, len(got))
		}
	}
}
