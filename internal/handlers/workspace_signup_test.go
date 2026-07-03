package handlers

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/bright-interaction/slab/internal/config"
	"github.com/bright-interaction/slab/internal/store"
)

func seedWorkspaceInvite(t *testing.T, sqlDB *sql.DB, q *store.Queries, token, email, expiresAt string) string {
	t.Helper()
	if _, err := sqlDB.Exec(`INSERT INTO workspaces (id, name, slug) VALUES ('wsx000000000000000000001', 'Test WS', 'test-ws')
		ON CONFLICT(id) DO NOTHING`); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	inviteID := "wi" + token[:20]
	if err := q.CreateWorkspaceInvite(context.Background(), store.CreateWorkspaceInviteParams{
		ID:          inviteID,
		WorkspaceID: "wsx000000000000000000001",
		Email:       email,
		Role:        "member",
		Token:       token,
		CreatedBy:   "",
		ExpiresAt:   expiresAt,
	}); err != nil {
		t.Fatalf("seed invite: %v", err)
	}
	return inviteID
}

func workspaceSignupRouter(h *WorkspaceHandler) chi.Router {
	r := chi.NewRouter()
	r.Get("/api/cloud/signup/{token}", h.SignupInfo)
	r.Post("/api/cloud/signup/{token}", h.SignupRedeem)
	return r
}

func TestWorkspaceSignup_NewAccountRedeem(t *testing.T) {
	sqlDB, q := setupDeployTestDB(t)
	cfg := &config.Config{JWTSecret: "test-jwt-secret-1234567890-abcdef"}
	h := NewWorkspaceHandler(cfg, q)
	r := workspaceSignupRouter(h)

	token := strings.Repeat("a", 48)
	seedWorkspaceInvite(t, sqlDB, q, token, "new@example.com",
		time.Now().Add(24*time.Hour).UTC().Format(time.RFC3339))

	// Info first: fresh email, no existing account.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/cloud/signup/"+token, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("info status = %d, body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"existing_account":false`) {
		t.Fatalf("info should report no existing account: %s", w.Body.String())
	}

	// Redeem creates the user + membership + session cookie.
	w = httptest.NewRecorder()
	body := strings.NewReader(`{"name":"New User","password":"longenough-password"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/signup/"+token, body)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("redeem status = %d, body %s", w.Code, w.Body.String())
	}
	user, err := q.GetUserByEmail(context.Background(), "new@example.com")
	if err != nil {
		t.Fatalf("user not created: %v", err)
	}
	if _, err := q.GetWorkspaceMembership(context.Background(), store.GetWorkspaceMembershipParams{
		WorkspaceID: "wsx000000000000000000001", UserID: user.ID,
	}); err != nil {
		t.Fatalf("membership not created: %v", err)
	}

	// The invite is spent: a second redeem must 410.
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/cloud/signup/"+token,
		strings.NewReader(`{"name":"x","password":"longenough-password"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusGone {
		t.Fatalf("spent invite should 410, got %d", w.Code)
	}
}

func TestWorkspaceSignup_ExistingAccountGetsMembershipNoSession(t *testing.T) {
	sqlDB, q := setupDeployTestDB(t)
	cfg := &config.Config{JWTSecret: "test-jwt-secret-1234567890-abcdef"}
	h := NewWorkspaceHandler(cfg, q)
	r := workspaceSignupRouter(h)

	if err := q.CreateUser(context.Background(), store.CreateUserParams{
		ID: "usr000000000000000000001", Email: "existing@example.com",
		PasswordHash: "x", Name: "Existing", Role: "editor",
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	token := strings.Repeat("b", 48)
	seedWorkspaceInvite(t, sqlDB, q, token, "existing@example.com",
		time.Now().Add(24*time.Hour).UTC().Format(time.RFC3339))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/signup/"+token, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("redeem status = %d, body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "membership_added") {
		t.Fatalf("expected membership_added: %s", w.Body.String())
	}
	// Possession of the link must never become a login to an existing
	// account: no session cookie on this path.
	for _, c := range w.Result().Cookies() {
		if strings.Contains(strings.ToLower(c.Name), "token") || strings.Contains(strings.ToLower(c.Name), "session") {
			t.Fatalf("existing-account redeem must not set a session cookie, got %q", c.Name)
		}
	}
	if _, err := q.GetWorkspaceMembership(context.Background(), store.GetWorkspaceMembershipParams{
		WorkspaceID: "wsx000000000000000000001", UserID: "usr000000000000000000001",
	}); err != nil {
		t.Fatalf("membership not created: %v", err)
	}
}

func TestWorkspaceSignup_ExpiredAndMalformedExpiryFailClosed(t *testing.T) {
	sqlDB, q := setupDeployTestDB(t)
	h := NewWorkspaceHandler(&config.Config{JWTSecret: "test-jwt-secret-1234567890-abcdef"}, q)
	r := workspaceSignupRouter(h)

	expired := strings.Repeat("c", 48)
	seedWorkspaceInvite(t, sqlDB, q, expired, "late@example.com",
		time.Now().Add(-1*time.Hour).UTC().Format(time.RFC3339))
	malformed := strings.Repeat("d", 48)
	seedWorkspaceInvite(t, sqlDB, q, malformed, "weird@example.com", "not-a-timestamp")

	for _, tok := range []string{expired, malformed} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/cloud/signup/"+tok, nil))
		if w.Code != http.StatusGone {
			t.Fatalf("token %q should 410 (fail closed), got %d", tok[:4], w.Code)
		}
	}
}
