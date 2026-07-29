package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bright-interaction/slab/internal/store"
)

// seedAgentKey inserts an agent_keys row and returns the raw key
// string so the caller can pass it as X-Agent-Key. CreateAgentKey
// always inserts is_active=1; the deactivated flag in tests is
// applied via DeactivateAgentKey.
func seedAgentKey(t *testing.T, queries *store.Queries, siteID, raw string, active int64) string {
	t.Helper()
	hash := HashAgentKey(raw)
	id := "k_" + raw[:8]
	if err := queries.CreateAgentKey(t.Context(), store.CreateAgentKeyParams{
		ID:           id,
		SiteID:       siteID,
		Name:         "test-key",
		KeyHash:      hash,
		Capabilities: `["read","write"]`,
	}); err != nil {
		t.Fatalf("create agent key: %v", err)
	}
	if active == 0 {
		if _, err := queries.DeactivateAgentKey(t.Context(), store.DeactivateAgentKeyParams{
			ID:     id,
			SiteID: siteID,
		}); err != nil {
			t.Fatalf("deactivate: %v", err)
		}
	}
	return raw
}

// TestAgentAuth_Revoked confirms that flipping is_active to 0 stops
// the key from authenticating. The SQL-level filter inside
// GetAgentKeyByHash carries the enforcement — this test pins the
// behaviour so a future query change can't quietly drop it.
func TestAgentAuth_Revoked(t *testing.T) {
	t.Parallel()
	sqlDB, queries := openTestDB(t)
	_ = sqlDB
	siteID := "0123456789abcdef01234567"
	if err := queries.CreateSite(t.Context(), store.CreateSiteParams{
		ID: siteID, Name: "t", Slug: "t", Domain: "t.test",
		PrimaryColor: "#000", SecondaryColor: "#000",
		BgColor: "#FFF", TextColor: "#000",
		FontHeading: "Inter", FontBody: "Inter", Lang: "en",
	}); err != nil {
		t.Fatal(err)
	}
	rawKey := seedAgentKey(t, queries, siteID, "raw-secret-1234567890abcdef", 1)

	mw := NewAgentAuthMiddleware(queries)

	called := false
	handler := mw.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	// Active key: should pass through.
	req := httptest.NewRequest(http.MethodGet, "/whatever", nil)
	req.Header.Set("X-Agent-Key", rawKey)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !called {
		t.Fatalf("active key blocked: status=%d called=%v", rr.Code, called)
	}

	// Revoke and replay.
	if _, err := queries.DeactivateAgentKey(t.Context(), store.DeactivateAgentKeyParams{
		ID:     "k_" + rawKey[:8],
		SiteID: siteID,
	}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	called = false
	req = httptest.NewRequest(http.MethodGet, "/whatever", nil)
	req.Header.Set("X-Agent-Key", rawKey)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("revoked key passed: status=%d", rr.Code)
	}
	if called {
		t.Error("revoked key reached the wrapped handler")
	}
}

// TestAgentAuth_NoKey rejects requests without the header.
func TestAgentAuth_NoKey(t *testing.T) {
	t.Parallel()
	_, queries := openTestDB(t)
	mw := NewAgentAuthMiddleware(queries)
	handler := mw.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler reached without auth")
	}))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/whatever", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", rr.Code)
	}
}

// TestAgentAuth_BearerHeader accepts Authorization: Bearer <key>.
func TestAgentAuth_BearerHeader(t *testing.T) {
	t.Parallel()
	_, queries := openTestDB(t)
	siteID := "0123456789abcdef01234567"
	if err := queries.CreateSite(t.Context(), store.CreateSiteParams{
		ID: siteID, Name: "t", Slug: "t", Domain: "t.test",
		PrimaryColor: "#000", SecondaryColor: "#000",
		BgColor: "#FFF", TextColor: "#000",
		FontHeading: "Inter", FontBody: "Inter", Lang: "en",
	}); err != nil {
		t.Fatal(err)
	}
	rawKey := seedAgentKey(t, queries, siteID, "raw-secret-2222222222222222", 1)
	mw := NewAgentAuthMiddleware(queries)
	handler := mw.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/whatever", nil)
	req.Header.Set("Authorization", "Bearer "+rawKey)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("Bearer auth rejected: status=%d", rr.Code)
	}
}
