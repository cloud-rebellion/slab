package agent

import (
	"strings"
	"testing"
)

// TestParseFidelity_FailSafe pins the fail-safe posture: unknown, empty,
// or garbage input must resolve to balanced, never silently relax
// (showcase) and never silently regrade (performance).
func TestParseFidelity_FailSafe(t *testing.T) {
	cases := map[string]DesignFidelity{
		"":              FidelityBalanced,
		"balanced":      FidelityBalanced,
		"performance":   FidelityPerformance,
		"showcase":      FidelityShowcase,
		" Showcase ":    FidelityShowcase,
		"PERFORMANCE":   FidelityPerformance,
		"turbo":         FidelityBalanced,
		"showcase-plus": FidelityBalanced,
		"0":             FidelityBalanced,
	}
	for in, want := range cases {
		if got := ParseFidelity(in); got != want {
			t.Fatalf("ParseFidelity(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDesignPlaybookFor_OverlaysApply proves the overlays land: showcase
// must announce its unlocks and relax the motion cap; performance must
// demand A+; balanced must be the canonical playbook plus the fidelity
// header.
func TestDesignPlaybookFor_OverlaysApply(t *testing.T) {
	balanced := DesignPlaybookFor(FidelityBalanced)
	if balanced.Fidelity.Active != "balanced" {
		t.Fatalf("balanced playbook fidelity header = %q", balanced.Fidelity.Active)
	}
	if len(balanced.Fidelity.StillEnforced) == 0 {
		t.Fatal("fidelity invariants missing from balanced playbook")
	}

	show := DesignPlaybookFor(FidelityShowcase)
	if show.Fidelity.Active != "showcase" || len(show.Fidelity.Unlocks) == 0 {
		t.Fatalf("showcase playbook missing fidelity contract: %+v", show.Fidelity)
	}
	// Motion cap relaxed from ONE to FOUR.
	foundFour := false
	for _, d := range show.Motion.DontUse {
		if strings.Contains(d, "More than ONE perpetual animation") {
			t.Fatal("showcase playbook still carries the balanced one-animation cap")
		}
		if strings.Contains(d, "More than FOUR perpetual animations") {
			foundFour = true
		}
	}
	if !foundFour {
		t.Fatal("showcase playbook missing the four-animation budget entry")
	}
	// Cinematic Aurora vibe + aurora hero graphic appended.
	if show.VibeArchetypes[len(show.VibeArchetypes)-1].Name != "Cinematic Aurora" {
		t.Fatal("showcase playbook missing the Cinematic Aurora vibe")
	}
	if show.HeroGraphics[len(show.HeroGraphics)-1].Name != "aurora" {
		t.Fatal("showcase playbook missing the aurora hero graphic")
	}
	// stitch-design leads the workflow.
	if show.DesignWorkflow.SkillsByVibe[0].Skill != "stitch-design" {
		t.Fatal("showcase workflow must lead with stitch-design")
	}
	// Authenticity slop lists identical across fidelities (never relaxed).
	if len(show.ContentAuthenticity.BannedPhrases) != len(balanced.ContentAuthenticity.BannedPhrases) {
		t.Fatal("showcase must not touch the slop lists")
	}

	perf := DesignPlaybookFor(FidelityPerformance)
	if perf.Fidelity.Active != "performance" {
		t.Fatalf("performance playbook fidelity header = %q", perf.Fidelity.Active)
	}
	foundAPlus := false
	for _, item := range perf.AuditChecklist {
		if strings.Contains(item.Check, "A+ on every category") {
			foundAPlus = true
		}
	}
	if !foundAPlus {
		t.Fatal("performance playbook must demand A+ on every category")
	}
}

// TestEvalPlaybookFor_TargetsFollowDial pins that the eval playbook's
// goal names the right scoreboard per fidelity.
func TestEvalPlaybookFor_TargetsFollowDial(t *testing.T) {
	if g := EvalPlaybookFor("s1", FidelityShowcase).Goal; !strings.Contains(g, "SHOWCASE FIDELITY") {
		t.Fatalf("showcase eval playbook goal not adapted: %s", g[:120])
	}
	if g := EvalPlaybookFor("s1", FidelityPerformance).Goal; !strings.Contains(g, "PERFORMANCE FIDELITY") {
		t.Fatalf("performance eval playbook goal not adapted: %s", g[:120])
	}
	if g := EvalPlaybookFor("s1", FidelityBalanced).Goal; !strings.Contains(g, "95%+") {
		t.Fatalf("balanced eval playbook goal changed unexpectedly: %s", g[:120])
	}
}
