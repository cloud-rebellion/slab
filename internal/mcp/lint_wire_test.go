package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestWireToolCallLintBlock_FlagsPlainHero confirms the lint_block
// tool returns the synchronous design-warnings array. Belts the
// end-to-end wiring: critique.LintBlockData -> runBlockLint helper ->
// tool handler -> JSON-RPC response.
func TestWireToolCallLintBlock_FlagsPlainHero(t *testing.T) {
	s := freshTestServer(t)
	resp, _ := jsonRPCRoundtrip(t, s, "tools/call", map[string]any{
		"name": "lint_block",
		"arguments": map[string]any{
			"block_type": "hero",
			"data": map[string]any{
				"headline":   "We make great products",
				"subheading": "Try us today",
			},
		},
	})
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("tools/call shape: %#v", resp.Result)
	}
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("lint_block returned isError=true: %v", result["content"])
	}
	contentArr, _ := result["content"].([]any)
	if len(contentArr) == 0 {
		t.Fatalf("empty content")
	}
	body, _ := contentArr[0].(map[string]any)["text"].(string)
	if !strings.Contains(body, `"hero_quality"`) {
		t.Errorf("expected hero_quality warning in body; got %s", body[:min(500, len(body))])
	}
	if !strings.Contains(body, `"design_warnings"`) {
		t.Errorf("expected design_warnings key in body; got %s", body[:min(500, len(body))])
	}
}

// TestWireToolCallLintBlock_CleanHeroNoWarnings confirms zero findings
// when the block clears the rules.
func TestWireToolCallLintBlock_CleanHeroNoWarnings(t *testing.T) {
	s := freshTestServer(t)
	resp, _ := jsonRPCRoundtrip(t, s, "tools/call", map[string]any{
		"name": "lint_block",
		"arguments": map[string]any{
			"block_type": "hero",
			"data": map[string]any{
				"hero_graphic": "mesh",
				"headline":     "Stop renting your business.",
				"subheading":   "Own it",
				"cta_text":     "Calculate",
				"cta_url":      "/calc",
			},
		},
	})
	result := resp.Result.(map[string]any)
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("lint_block returned isError=true: %v", result["content"])
	}
	body := result["content"].([]any)[0].(map[string]any)["text"].(string)
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	count, _ := payload["count"].(float64)
	if int(count) != 0 {
		t.Errorf("expected zero design_warnings for clean hero; got %d (%s)", int(count), body[:min(500, len(body))])
	}
}
