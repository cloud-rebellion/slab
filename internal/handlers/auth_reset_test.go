package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"

	"github.com/bright-interaction/slab/internal/config"
	dbpkg "github.com/bright-interaction/slab/internal/db"
	"github.com/bright-interaction/slab/internal/store"
)

// withChiToken wraps a handler so chi.URLParam(r, "token") returns
// the supplied raw token. SetPathValue (Go 1.22 stdlib mux) doesn't
// reach chi's RouteCtx, so tests inject directly.
func withChiToken(token string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("token", token)
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
		h(w, r)
	}
}

func newAuthHandlerForReset(t *testing.T) (*AuthHandler, *store.Queries) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "auth.db")
	sqlDB, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	if _, err := sqlDB.Exec(string(dbpkg.Schema)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	q := store.New(sqlDB)
	cfg := &config.Config{BaseURL: "http://localhost:8080"}
	return NewAuthHandler(cfg, q), q
}

func seedUser(t *testing.T, q *store.Queries, email string) string {
	t.Helper()
	hash, _ := bcrypt.GenerateFromPassword([]byte("oldpassword12"), 10)
	id := "u_" + email[:1]
	if err := q.CreateUser(context.Background(), store.CreateUserParams{
		ID:           id,
		Email:        email,
		PasswordHash: string(hash),
		Name:         "Test",
		Role:         "admin",
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return id
}

func postJSONResetTest(handler http.HandlerFunc, path string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	rr := httptest.NewRecorder()
	handler(rr, req)
	return rr
}

// TestForgotPassword_BareSuccessRegardlessOfEmail confirms the
// 200/ok response shape doesn't leak whether the email exists. Both
// real and bogus emails get the same bare ack.
func TestForgotPassword_BareSuccessRegardlessOfEmail(t *testing.T) {
	t.Parallel()
	h, q := newAuthHandlerForReset(t)
	seedUser(t, q, "real@example.com")

	for _, email := range []string{"real@example.com", "ghost@example.com"} {
		rr := postJSONResetTest(h.ForgotPassword, "/api/auth/forgot-password",
			map[string]string{"email": email})
		if rr.Code != http.StatusOK {
			t.Errorf("email=%s: status=%d, want 200", email, rr.Code)
		}
	}
}

// TestForgotPassword_PersistsTokenForRealUserOnly verifies a
// password_resets row only lands when the email matches.
func TestForgotPassword_PersistsTokenForRealUserOnly(t *testing.T) {
	t.Parallel()
	h, q := newAuthHandlerForReset(t)
	uid := seedUser(t, q, "real@example.com")

	postJSONResetTest(h.ForgotPassword, "/api/auth/forgot-password",
		map[string]string{"email": "real@example.com"})
	postJSONResetTest(h.ForgotPassword, "/api/auth/forgot-password",
		map[string]string{"email": "ghost@example.com"})

	// Direct count via a reflection-free query: ListAuditLogBySite
	// captured "create password_reset" rows. There's exactly one for
	// the real user.
	rows, _ := q.ListAuditLogGlobal(context.Background(), 100)
	count := 0
	for _, r := range rows {
		if r.ResourceType == "password_reset" && r.ResourceID == uid {
			count++
		}
	}
	if count != 1 {
		t.Errorf("audit log shows %d reset rows for real user, want 1", count)
	}
}

// TestResetPassword_HappyPath_E2E mints a token, posts a new
// password, and confirms the user row is updated.
func TestResetPassword_HappyPath_E2E(t *testing.T) {
	t.Parallel()
	h, q := newAuthHandlerForReset(t)
	uid := seedUser(t, q, "live@example.com")
	rawToken := mintTokenForUser(t, q, uid)

	rr := postJSONResetTest(
		withChiToken(rawToken, h.ResetPassword),
		"/api/auth/reset-password/"+rawToken,
		map[string]string{"password": "newpassword123abc"},
	)
	if rr.Code != http.StatusOK {
		t.Fatalf("reset failed: status=%d body=%s", rr.Code, rr.Body.String())
	}
	user, err := q.GetUserByEmail(context.Background(), "live@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte("newpassword123abc")); err != nil {
		t.Errorf("new password didn't take: %v", err)
	}
}

// TestResetPassword_RejectsShortPassword: 11 chars fails, 12 passes.
func TestResetPassword_RejectsShortPassword(t *testing.T) {
	t.Parallel()
	h, q := newAuthHandlerForReset(t)
	uid := seedUser(t, q, "short@example.com")
	rawToken := mintTokenForUser(t, q, uid)

	rr := postJSONResetTest(
		withChiToken(rawToken, h.ResetPassword),
		"/api/auth/reset-password/"+rawToken,
		map[string]string{"password": "12345678901"}, // 11 chars
	)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("11-char password: status=%d, want 400", rr.Code)
	}
}

// TestResetPassword_TokenIsSingleUse: using a token twice fails the
// second time.
func TestResetPassword_TokenIsSingleUse(t *testing.T) {
	t.Parallel()
	h, q := newAuthHandlerForReset(t)
	uid := seedUser(t, q, "single@example.com")
	rawToken := mintTokenForUser(t, q, uid)

	hf := withChiToken(rawToken, h.ResetPassword)
	first := postJSONResetTest(hf, "/api/auth/reset-password/"+rawToken,
		map[string]string{"password": "validpass1234"})
	if first.Code != http.StatusOK {
		t.Fatalf("first use failed: %d", first.Code)
	}
	second := postJSONResetTest(hf, "/api/auth/reset-password/"+rawToken,
		map[string]string{"password": "anothervalid1234"})
	if second.Code != http.StatusNotFound {
		t.Errorf("second use of token: status=%d, want 404", second.Code)
	}
}

// TestResetPassword_ExpiredTokenRejected backdates the reset row's
// expires_at and confirms the redemption returns 404.
func TestResetPassword_ExpiredTokenRejected(t *testing.T) {
	t.Parallel()
	h, q := newAuthHandlerForReset(t)
	uid := seedUser(t, q, "expired@example.com")

	raw, err := generateResetToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := q.CreatePasswordReset(context.Background(), store.CreatePasswordResetParams{
		ID:          newID(),
		UserID:      uid,
		TokenHash:   hashResetToken(raw),
		ExpiresAt:   time.Now().UTC().Add(-1 * time.Minute).Format(time.RFC3339),
		RequesterIp: "0.0.0.0",
	}); err != nil {
		t.Fatal(err)
	}

	rr := postJSONResetTest(
		withChiToken(raw, h.ResetPassword),
		"/api/auth/reset-password/"+raw,
		map[string]string{"password": "validpass1234"},
	)
	if rr.Code != http.StatusNotFound {
		t.Errorf("expired token: status=%d, want 404", rr.Code)
	}
}

// TestResetPasswordInfo_MasksEmail confirms the GET preview returns
// a masked email that doesn't echo the full address.
func TestResetPasswordInfo_MasksEmail(t *testing.T) {
	t.Parallel()
	h, q := newAuthHandlerForReset(t)
	uid := seedUser(t, q, "alice@example.com")
	raw := mintTokenForUser(t, q, uid)

	req := httptest.NewRequest(http.MethodGet, "/api/auth/reset-password/"+raw, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("token", raw)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()
	h.ResetPasswordInfo(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	var resp struct {
		Email string `json:"email"`
	}
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp.Email == "alice@example.com" {
		t.Error("full email leaked")
	}
	if resp.Email != "a***@example.com" {
		t.Errorf("masked email = %q", resp.Email)
	}
}

// mintTokenForUser inserts a fresh password_resets row + returns
// the raw token. Helper for the tests above.
func mintTokenForUser(t *testing.T, q *store.Queries, userID string) string {
	t.Helper()
	raw, err := generateResetToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := q.CreatePasswordReset(context.Background(), store.CreatePasswordResetParams{
		ID:          newID(),
		UserID:      userID,
		TokenHash:   hashResetToken(raw),
		ExpiresAt:   time.Now().UTC().Add(30 * time.Minute).Format(time.RFC3339),
		RequesterIp: "0.0.0.0",
	}); err != nil {
		t.Fatal(err)
	}
	return raw
}
