package mcp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	dbpkg "github.com/bright-interaction/slab/internal/db"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"
)

// The literal secrets seeded into site_settings for this test. If either of
// these strings appears anywhere in an MCP response body, a bearer-equivalent
// tenant secret has been handed to the model provider.
const (
	leakTestSiteID     = "site_secretleak"
	leakTestMollieKey  = "live_ZzQq7RRkPnvv44WWxxTTuu9988ffGGhh"
	leakTestCRMSecret  = "whsec_do_not_send_this_to_an_llm_9988"
	leakTestRedactMask = "***redacted***"
)

// openSecretLeakTestDB builds a schema-loaded scratch sqlite and a real
// *store.Queries, so the MCP server under test resolves its context from a
// genuine database rather than a stub.
func openSecretLeakTestDB(t *testing.T) (*sql.DB, *store.Queries) {
	t.Helper()
	dir := t.TempDir()
	sqlDB, err := sql.Open("sqlite", filepath.Join(dir, "t.sqlite")+"?_journal_mode=WAL&_busy_timeout=5000&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if _, err := sqlDB.Exec(string(dbpkg.Schema)); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return sqlDB, store.New(sqlDB)
}

// readOnlyAgentRoundtrip is jsonRPCRoundtrip with a READ-ONLY identity, which
// is the whole point: this leak needs no write capability. Returns the raw
// response body so the assertions can scan the entire wire payload rather than
// a field the test author remembered to look at.
func readOnlyAgentRoundtrip(t *testing.T, s *Server, method string, params any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), authmw.AgentContextKey, &authmw.AgentIdentity{
		KeyID:        "ro-key",
		SiteID:       leakTestSiteID,
		Capabilities: []string{"read"},
	}))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("%s: expected 200, got %d (body=%s)", method, rr.Code, rr.Body.String())
	}
	var resp Response
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("%s: decode response: %v (body=%s)", method, err, rr.Body.String())
	}
	if resp.Error != nil {
		t.Fatalf("%s: JSON-RPC error: code=%d msg=%s", method, resp.Error.Code, resp.Error.Message)
	}
	return rr.Body.Bytes()
}

// TestSecretSettingsNeverReachTheLLMOnAnySurface is the regression for the
// 2026-08-11 audit finding 2: every tenant's live Mollie API key was returned
// in plaintext on four MCP surfaces because buildSettingsCatalog assigned
// CurrentValue with no secret check, while the existing redactor was wired to
// list_settings only.
//
// All five surfaces are driven through the real JSON-RPC handler with a
// read-only agent key. list_settings is included as a POSITIVE CONTROL: it was
// already masked before the fix, so if the control ever stops showing a mask
// the harness itself has broken and the four negative assertions mean nothing.
func TestSecretSettingsNeverReachTheLLMOnAnySurface(t *testing.T) {
	sqlDB, queries := openSecretLeakTestDB(t)
	if _, err := sqlDB.Exec(`INSERT INTO sites (id, name, slug) VALUES (?, ?, ?)`,
		leakTestSiteID, "Leak Test", leakTestSiteID); err != nil {
		t.Fatalf("seed site: %v", err)
	}

	ctx := context.Background()
	for _, row := range []struct{ id, category, key, value string }{
		{"set_mollie", "payments", "mollie_api_key", leakTestMollieKey},
		{"set_crm", "analytics", "crm_webhook_secret", leakTestCRMSecret},
	} {
		if err := queries.UpsertSetting(ctx, store.UpsertSettingParams{
			ID: row.id, SiteID: leakTestSiteID, Category: row.category, Key: row.key, Value: row.value,
		}); err != nil {
			t.Fatalf("seed setting %s.%s: %v", row.category, row.key, err)
		}
	}

	s := NewServer(queries, nil)

	surfaces := []struct {
		label   string
		method  string
		params  any
		control bool // true = expected to have been masked before the fix
	}{
		{
			label:  "tool get_settings_catalog",
			method: "tools/call",
			params: map[string]any{"name": "get_settings_catalog", "arguments": map[string]any{}},
		},
		{
			label:  "tool get_site_context",
			method: "tools/call",
			params: map[string]any{"name": "get_site_context", "arguments": map[string]any{}},
		},
		{
			label:  "resource settings_catalog",
			method: "resources/read",
			params: map[string]any{"uri": "atomicsite://site/settings_catalog"},
		},
		{
			label:  "resource site/context",
			method: "resources/read",
			params: map[string]any{"uri": "atomicsite://site/context"},
		},
		{
			label:   "CONTROL tool list_settings",
			method:  "tools/call",
			params:  map[string]any{"name": "list_settings", "arguments": map[string]any{}},
			control: true,
		},
	}

	for _, surface := range surfaces {
		raw := string(readOnlyAgentRoundtrip(t, s, surface.method, surface.params))

		// The response is JSON-encoded twice (tool text is a string field
		// holding JSON), so scan the raw wire bytes for the secret in both
		// plain and escaped form rather than trusting one decode path.
		if strings.Contains(raw, leakTestMollieKey) {
			t.Errorf("%s: LEAKED the plaintext Mollie API key to the LLM surface", surface.label)
		}
		if strings.Contains(raw, leakTestCRMSecret) {
			t.Errorf("%s: LEAKED the plaintext CRM webhook secret to the LLM surface", surface.label)
		}
		if !strings.Contains(raw, leakTestRedactMask) {
			t.Errorf("%s: no %q mask present; the secret is neither redacted nor reported as configured",
				surface.label, leakTestRedactMask)
		}
	}
}

// TestSecretCatalogEntriesStayMarkedConfigured guards the half of the fix that
// is easy to lose: masking must not make a configured secret look unset.
// IsDefault has to stay false so the agent can still answer "is Mollie
// configured?" without ever seeing the key.
func TestSecretCatalogEntriesStayMarkedConfigured(t *testing.T) {
	sqlDB, queries := openSecretLeakTestDB(t)
	if _, err := sqlDB.Exec(`INSERT INTO sites (id, name, slug) VALUES (?, ?, ?)`,
		leakTestSiteID, "Leak Test", leakTestSiteID); err != nil {
		t.Fatalf("seed site: %v", err)
	}
	if err := queries.UpsertSetting(context.Background(), store.UpsertSettingParams{
		ID: "set_mollie", SiteID: leakTestSiteID, Category: "payments", Key: "mollie_api_key", Value: leakTestMollieKey,
	}); err != nil {
		t.Fatalf("seed setting: %v", err)
	}

	s := NewServer(queries, nil)
	raw := readOnlyAgentRoundtrip(t, s, "tools/call", map[string]any{
		"name": "get_settings_catalog", "arguments": map[string]any{},
	})

	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if len(resp.Result.Content) == 0 {
		t.Fatalf("empty tool content: %s", raw)
	}
	var catalog struct {
		Items []struct {
			Category     string `json:"category"`
			Key          string `json:"key"`
			CurrentValue string `json:"current_value"`
			IsDefault    bool   `json:"is_default"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(resp.Result.Content[0].Text), &catalog); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}

	var found bool
	for _, item := range catalog.Items {
		if item.Category != "payments" || item.Key != "mollie_api_key" {
			continue
		}
		found = true
		if item.CurrentValue != leakTestRedactMask {
			t.Errorf("payments.mollie_api_key current_value = %q, want %q", item.CurrentValue, leakTestRedactMask)
		}
		if item.IsDefault {
			t.Error("payments.mollie_api_key is_default = true; a configured secret must still read as configured")
		}
	}
	if !found {
		t.Fatal("payments.mollie_api_key descriptor missing from the catalog")
	}
}

// TestUnconfiguredSecretStaysDefault pins the other boundary: an absent secret
// row must NOT be dressed up as a redacted value, or the agent would think a
// key is configured when the tenant has not set one.
func TestUnconfiguredSecretStaysDefault(t *testing.T) {
	sqlDB, queries := openSecretLeakTestDB(t)
	if _, err := sqlDB.Exec(`INSERT INTO sites (id, name, slug) VALUES (?, ?, ?)`,
		leakTestSiteID, "Leak Test", leakTestSiteID); err != nil {
		t.Fatalf("seed site: %v", err)
	}

	s := NewServer(queries, nil)
	raw := readOnlyAgentRoundtrip(t, s, "tools/call", map[string]any{
		"name": "get_settings_catalog", "arguments": map[string]any{},
	})
	if strings.Contains(string(raw), leakTestRedactMask) {
		t.Error("an unconfigured secret was reported as redacted; the agent cannot tell configured from unset")
	}
}
