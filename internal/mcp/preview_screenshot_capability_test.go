package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authmw "github.com/bright-interaction/slab/internal/middleware"
)

// preview_screenshot shipped with no RequiresWrite field, so it defaulted to
// false and the gate at server.go handleToolsCall never fired for it. Any
// ACTIVE key, including a read-only one, could drive a headless-browser capture
// and read back a render of the site's draft pages. Its sibling `screenshot`
// has carried RequiresWrite: true since it was written; this one was simply
// missed.
//
// This test asserts the ENFORCEMENT, not just the struct field, so it survives
// a refactor of how the flag is spelled.
//
// ABLATION: delete `RequiresWrite: true` from the preview_screenshot
// registration in tools_extra.go and this test fails with the read-only key
// getting past the capability gate.
func TestPreviewScreenshotRequiresWriteCapability(t *testing.T) {
	s := &Server{
		tools:     map[string]Tool{},
		resources: map[string]Resource{},
		prompts:   map[string]Prompt{},
	}
	s.registerTools()

	for _, name := range []string{"preview_screenshot", "screenshot"} {
		tool, ok := s.tools[name]
		if !ok {
			t.Fatalf("tool %q not registered", name)
		}
		if !tool.RequiresWrite {
			t.Errorf("tool %q must be RequiresWrite: true; it drives a browser and returns a render of site content", name)
		}
	}

	// The attack: an active key with read capability only.
	readOnly := &authmw.AgentIdentity{SiteID: "site-1", KeyID: "key-ro", Capabilities: []string{"read"}}
	r := httptest.NewRequest(http.MethodPost, "/", nil)

	for _, name := range []string{"preview_screenshot", "screenshot"} {
		params, err := json.Marshal(map[string]any{
			"name":      name,
			"arguments": map[string]any{"slug": "index", "url": "https://example.com/"},
		})
		if err != nil {
			t.Fatalf("marshal params: %v", err)
		}
		res, rerr := s.handleToolsCall(r, readOnly, params)
		if rerr != nil {
			t.Fatalf("%s: unexpected JSON-RPC error: %v", name, rerr.Message)
		}
		out, ok := res.(ToolCallResult)
		if !ok {
			t.Fatalf("%s: unexpected result type %T", name, res)
		}
		if !out.IsError {
			t.Errorf("%s: a READ-ONLY key was allowed to run a capture", name)
			continue
		}
		body := ""
		if len(out.Content) > 0 {
			body = out.Content[0].Text
		}
		if !strings.Contains(body, "requires the 'write' capability") {
			t.Errorf("%s: refused for the wrong reason: %q", name, body)
		}
	}
}

// A write key must still get through, so the fix does not silently disable the
// tool for the agents that are supposed to use it. It fails on the missing
// preview-token wiring, which is well past the capability gate.
func TestPreviewScreenshotWriteKeyPassesTheCapabilityGate(t *testing.T) {
	s := &Server{
		tools:     map[string]Tool{},
		resources: map[string]Resource{},
		prompts:   map[string]Prompt{},
	}
	s.registerTools()

	writeKey := &authmw.AgentIdentity{SiteID: "site-1", KeyID: "key-rw", Capabilities: []string{"read", "write"}}
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	params, err := json.Marshal(map[string]any{
		"name":      "preview_screenshot",
		"arguments": map[string]any{"slug": "index"},
	})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	res, rerr := s.handleToolsCall(r, writeKey, params)
	if rerr != nil {
		t.Fatalf("unexpected JSON-RPC error: %v", rerr.Message)
	}
	out, ok := res.(ToolCallResult)
	if !ok {
		t.Fatalf("unexpected result type %T", res)
	}
	body := ""
	if len(out.Content) > 0 {
		body = out.Content[0].Text
	}
	if strings.Contains(body, "requires the 'write' capability") {
		t.Errorf("a write key was refused by the capability gate: %q", body)
	}
}
