package perfectfoundation

import "testing"

// TestEveryCheckHasOwner ensures every entry in CheckOwnership has a non-empty
// Owner. Adding a new eval check without naming an owner fails CI.
func TestEveryCheckHasOwner(t *testing.T) {
	for _, c := range CheckOwnership {
		if c.Name == "" {
			t.Errorf("check entry missing Name in category %q", c.Category)
		}
		if c.Category == "" {
			t.Errorf("check %q missing Category", c.Name)
		}
		if c.Owner == "" {
			t.Errorf("check %q (%s) has no Owner — assign bake/block/teach + file:function", c.Name, c.Category)
		}
		switch c.Strategy {
		case StrategyBake, StrategyBlock, StrategyTeach:
		default:
			t.Errorf("check %q has invalid Strategy %q", c.Name, c.Strategy)
		}
	}
}

// TestCategoryCounts pins the expected total per category so adding a new
// eval check forces a deliberate update to CheckOwnership.
func TestCategoryCounts(t *testing.T) {
	counts := CountByCategory()
	expected := map[string]int{
		"security":      18,
		"seo":           38,
		"geo":           11,
		"performance":   8,
		"accessibility": 18,
		"privacy":       9,
	}
	for cat, want := range expected {
		if got := counts[cat]; got != want {
			t.Errorf("category %q: got %d checks, want %d. Update CheckOwnership and this test together.", cat, got, want)
		}
	}
}

// TestReferenceKBNonEmpty ensures the brightinteraction-derived reference KB
// has substantive content for every entry.
func TestReferenceKBNonEmpty(t *testing.T) {
	entries := ReferenceEntries()
	if len(entries) < 20 {
		t.Errorf("reference KB shrunk to %d entries; expected >=20", len(entries))
	}
	for _, e := range entries {
		if e.Title == "" {
			t.Errorf("KB entry in %q has empty Title", e.Category)
		}
		if len(e.Content) < 100 {
			t.Errorf("KB entry %q (%s) content too short (%d chars); aim for >=100", e.Title, e.Category, len(e.Content))
		}
	}
}
