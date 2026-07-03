package handlers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bright-interaction/slab/internal/store"
)

// TestClarifications_CreateGetResolveLifecycle covers the happy path
// the dashboard inbox + agent polling loop relies on. Create a pending
// row, fetch it, resolve with an option index + free-text, confirm the
// resolved row carries the picked option + text + status=resolved.
func TestClarifications_CreateGetResolveLifecycle(t *testing.T) {
	_, q := setupDeployTestDB(t)
	siteID := "claritysite00000aaaa0001"
	seedSite(t, q, siteID)
	h := NewClarificationsHandler(nil, q)
	ctx := context.Background()

	row, err := h.CreateClarification(ctx, siteID, "agent:key-1", CreateClarificationParams{
		Question: "Which hero variant fits the law-firm vibe?",
		Context:  "/index, block_id=hero-001, brand archetype=audit-receipt",
		Options:  []string{"mesh", "audit-receipt", "monogram"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if row.Status != "pending" {
		t.Errorf("new row status = %q; want pending", row.Status)
	}
	if row.OptionsJson == "[]" {
		t.Errorf("expected options serialized; got %q", row.OptionsJson)
	}

	view, err := h.GetClarification(ctx, siteID, row.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got, want := len(view.Options), 3; got != want {
		t.Errorf("options length = %d; want %d", got, want)
	}
	if view.Options[1] != "audit-receipt" {
		t.Errorf("options[1] = %q; want audit-receipt", view.Options[1])
	}

	// Resolve: pick option index 1, add elaboration.
	if err := q.ResolveClarification(ctx, store.ResolveClarificationParams{
		ResolutionOption: 1,
		ResolutionText:   "Go with audit-receipt; the precedent receipts already on the page reinforce the same metaphor.",
		ResolvedBy:       "user:tom",
		ID:               row.ID,
		SiteID:           siteID,
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	view2, err := h.GetClarification(ctx, siteID, row.ID)
	if err != nil {
		t.Fatalf("get-after-resolve: %v", err)
	}
	if view2.Status != "resolved" {
		t.Errorf("status after resolve = %q; want resolved", view2.Status)
	}
	if view2.ResolutionOption != 1 {
		t.Errorf("resolution_option = %d; want 1", view2.ResolutionOption)
	}
	if view2.ResolutionText == "" {
		t.Errorf("expected non-empty resolution_text")
	}
	if view2.ResolvedBy != "user:tom" {
		t.Errorf("resolved_by = %q; want user:tom", view2.ResolvedBy)
	}
}

// TestClarifications_ValidationRejectsEmptyQuestion locks the contract
// that an empty / whitespace-only question is a hard error so the
// dashboard inbox never surfaces ghost rows.
func TestClarifications_ValidationRejectsEmptyQuestion(t *testing.T) {
	_, q := setupDeployTestDB(t)
	siteID := "claritysite00000bbbb0002"
	seedSite(t, q, siteID)
	h := NewClarificationsHandler(nil, q)

	if _, err := h.CreateClarification(context.Background(), siteID, "agent:k", CreateClarificationParams{
		Question: "   ",
	}); err == nil {
		t.Fatalf("expected error for whitespace-only question")
	}
}

// TestClarifications_ListPendingByAgentScopesToCaller proves the
// list_pending_clarifications MCP tool can only see the calling agent's
// own asks. Cross-agent leaks would let one agent steal another's
// pending answers; the test belts that boundary.
func TestClarifications_ListPendingByAgentScopesToCaller(t *testing.T) {
	_, q := setupDeployTestDB(t)
	siteID := "claritysite00000cccc0003"
	seedSite(t, q, siteID)
	h := NewClarificationsHandler(nil, q)
	ctx := context.Background()

	if _, err := h.CreateClarification(ctx, siteID, "agent:a", CreateClarificationParams{Question: "A?"}); err != nil {
		t.Fatalf("create A: %v", err)
	}
	if _, err := h.CreateClarification(ctx, siteID, "agent:b", CreateClarificationParams{Question: "B?"}); err != nil {
		t.Fatalf("create B: %v", err)
	}

	rowsA, err := h.ListPendingByAgent(ctx, siteID, "agent:a", 0)
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	if len(rowsA) != 1 {
		t.Errorf("agent A should see 1 row; got %d", len(rowsA))
	}
	if len(rowsA) > 0 && rowsA[0].Question != "A?" {
		t.Errorf("agent A should see its own question; got %q", rowsA[0].Question)
	}

	rowsB, err := h.ListPendingByAgent(ctx, siteID, "agent:b", 0)
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	if len(rowsB) != 1 || rowsB[0].Question != "B?" {
		t.Errorf("agent B isolation failed; got %v", rowsB)
	}
}

// TestClarifications_OptionsLimit confirms the validator rejects more
// than 12 options. Belts the dashboard from having to render a 50-button
// inbox card.
func TestClarifications_OptionsLimit(t *testing.T) {
	_, q := setupDeployTestDB(t)
	siteID := "claritysite00000dddd0004"
	seedSite(t, q, siteID)
	h := NewClarificationsHandler(nil, q)

	tooMany := make([]string, 13)
	for i := range tooMany {
		tooMany[i] = "opt"
	}
	if _, err := h.CreateClarification(context.Background(), siteID, "agent:x", CreateClarificationParams{
		Question: "Pick one",
		Options:  tooMany,
	}); err == nil {
		t.Fatalf("expected error for >12 options")
	}
}

// TestClarifications_OptionsSerialisedAsJSONArray confirms the options
// round-trip through the JSON column as an array (not a stringified
// blob). Catches any future regression where options leaks the wrong
// shape to the dashboard.
func TestClarifications_OptionsSerialisedAsJSONArray(t *testing.T) {
	_, q := setupDeployTestDB(t)
	siteID := "claritysite00000eeee0005"
	seedSite(t, q, siteID)
	h := NewClarificationsHandler(nil, q)
	row, err := h.CreateClarification(context.Background(), siteID, "agent:y", CreateClarificationParams{
		Question: "Pick a wedge",
		Options:  []string{"compliance", "cost", "speed"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var arr []string
	if err := json.Unmarshal([]byte(row.OptionsJson), &arr); err != nil {
		t.Fatalf("options_json not a JSON array: %v (raw=%q)", err, row.OptionsJson)
	}
	if len(arr) != 3 {
		t.Errorf("expected 3 options; got %d (%v)", len(arr), arr)
	}
}
