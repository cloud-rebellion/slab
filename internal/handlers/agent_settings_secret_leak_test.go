package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/bright-interaction/slab/internal/agent"
	"github.com/bright-interaction/slab/internal/config"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/settingspolicy"
	"github.com/bright-interaction/slab/internal/store"
)

// The REST half of the 2026-08-11 finding-2 regression. The MCP half lives in
// internal/mcp/settings_catalog_secret_leak_test.go and covers the four
// catalog surfaces; these are the three agent REST endpoints that returned raw
// store.SiteSetting rows with no masking at all.
//
// Seven surfaces total now route through one masker. The point of having a
// test on BOTH sides of the package boundary is that the shared helper cannot
// be silently unwired from one side: dropping the call in handlers leaves the
// mcp test green and vice versa, so neither test alone would have caught it.
const (
	restLeakSiteID    = "site_rest_leak"
	restLeakMollieKey = "live_RestKKmmNN7788QQwwEErrTTyyUUiiOO"
	restLeakCRMSecret = "whsec_rest_do_not_send_this_to_an_llm"
	// A NON-secret value seeded alongside, used as the positive control: it
	// must survive unmasked. Without it a masker that blanked every value
	// would pass every negative assertion in this file.
	restControlKey   = "meta_title_template"
	restControlValue = "CONTROL-VALUE-must-survive-unmasked"
)

// agentSettingsRouter mirrors the production route table at
// internal/server/server.go:1213-1215 and injects the given capabilities.
func agentSettingsRouter(h *AgentHandler, capabilities []string) *chi.Mux {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := context.WithValue(req.Context(), authmw.AgentContextKey, &authmw.AgentIdentity{
				KeyID:        "rest-leak-key",
				SiteID:       restLeakSiteID,
				Capabilities: capabilities,
			})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Get("/api/agent/settings", h.ListSettings)
	r.Get("/api/agent/settings/{category}", h.ListSettingsByCategory)
	r.Patch("/api/agent/settings", h.BulkUpsertSettings)
	return r
}

func seedRestLeakSettings(t *testing.T, q *store.Queries) {
	t.Helper()
	for _, row := range []struct{ id, category, key, value string }{
		{"set_rest_mollie", "payments", "mollie_api_key", restLeakMollieKey},
		{"set_rest_crm", "analytics", "crm_webhook_secret", restLeakCRMSecret},
		{"set_rest_control", "seo", restControlKey, restControlValue},
	} {
		if err := q.UpsertSetting(context.Background(), store.UpsertSettingParams{
			ID: row.id, SiteID: restLeakSiteID, Category: row.category, Key: row.key, Value: row.value,
		}); err != nil {
			t.Fatalf("seed setting %s.%s: %v", row.category, row.key, err)
		}
	}
}

// TestAgentSettingsRESTNeverLeaksSecretsToTheLLM drives all three agent REST
// settings endpoints and asserts no bearer-equivalent value crosses the LLM
// boundary, while a non-secret value still does.
func TestAgentSettingsRESTNeverLeaksSecretsToTheLLM(t *testing.T) {
	sqlDB, q := setupDeployTestDB(t)
	seedSite(t, q, restLeakSiteID)
	seedRestLeakSettings(t, q)
	h := NewAgentHandler(&config.Config{}, q, sqlDB)

	surfaces := []struct {
		label        string
		method       string
		path         string
		body         string
		capabilities []string
		// wantControl is false where the response is scoped to a category
		// that does not contain the control row.
		wantControl bool
	}{
		{
			label:        "GET /api/agent/settings",
			method:       http.MethodGet,
			path:         "/api/agent/settings",
			capabilities: []string{"read"},
			wantControl:  true,
		},
		{
			label:        "GET /api/agent/settings/payments",
			method:       http.MethodGet,
			path:         "/api/agent/settings/payments",
			capabilities: []string{"read"},
		},
		{
			label:        "GET /api/agent/settings/analytics",
			method:       http.MethodGet,
			path:         "/api/agent/settings/analytics",
			capabilities: []string{"read"},
		},
		{
			// The PATCH response echoes the full settings list back, which is
			// how the write path leaked the same secrets as the read path.
			label:        "PATCH /api/agent/settings",
			method:       http.MethodPatch,
			path:         "/api/agent/settings",
			body:         `[{"category":"seo","key":"canonical_base","value":"https://example.com"}]`,
			capabilities: []string{"read", "write"},
			wantControl:  true,
		},
	}

	for _, surface := range surfaces {
		var body *bytes.Reader
		if surface.body != "" {
			body = bytes.NewReader([]byte(surface.body))
		} else {
			body = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(surface.method, surface.path, body)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		agentSettingsRouter(h, surface.capabilities).ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("%s: expected 200, got %d (body=%s)", surface.label, w.Code, w.Body.String())
			continue
		}
		raw := w.Body.String()

		if strings.Contains(raw, restLeakMollieKey) {
			t.Errorf("%s: LEAKED the plaintext Mollie API key over the agent REST API", surface.label)
		}
		if strings.Contains(raw, restLeakCRMSecret) {
			t.Errorf("%s: LEAKED the plaintext CRM webhook secret over the agent REST API", surface.label)
		}
		if !strings.Contains(raw, settingspolicy.RedactedValue) {
			t.Errorf("%s: no %q mask present; the configured secret is invisible to the agent entirely",
				surface.label, settingspolicy.RedactedValue)
		}
		// Positive control: a non-secret value must pass through untouched, so
		// this test cannot be satisfied by a masker that blanks everything.
		if surface.wantControl && !strings.Contains(raw, restControlValue) {
			t.Errorf("%s: CONTROL value %q was masked; the redactor is over-broad",
				surface.label, restControlValue)
		}
	}
}

// TestMaskSettingRowsIsTheOnlyPolicy pins the shared helper's contract
// directly, so a regression in the extraction is attributable without having
// to bisect seven HTTP surfaces.
func TestMaskSettingRowsIsTheOnlyPolicy(t *testing.T) {
	cases := []struct {
		name              string
		category, key, in string
		want              string
	}{
		{"explicit mollie key", "payments", "mollie_api_key", restLeakMollieKey, settingspolicy.RedactedValue},
		{"explicit crm secret", "analytics", "crm_webhook_secret", restLeakCRMSecret, settingspolicy.RedactedValue},
		{"heuristic future token", "integrations", "slack_token", "xoxb-live", settingspolicy.RedactedValue},
		{"heuristic future password", "smtp", "smtp_password", "hunter2", settingspolicy.RedactedValue},
		{"non-secret passes through", "seo", restControlKey, restControlValue, restControlValue},
		{"public ga4 id passes through", "analytics", "ga4_id", "G-ABC123", "G-ABC123"},
		{"unset secret stays empty", "payments", "mollie_api_key", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows := []store.SiteSetting{{Category: tc.category, Key: tc.key, Value: tc.in}}
			if got := agent.MaskSettingRows(rows)[0].Value; got != tc.want {
				t.Errorf("mask(%s.%s, %q) = %q, want %q", tc.category, tc.key, tc.in, got, tc.want)
			}
		})
	}
}
