package handlers

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"
)

// newGDPRTestStack spins up a fresh DB with a seeded user. Returns
// the handler + the *sql.DB so tests can inspect deletion state.
func newGDPRTestStack(t *testing.T) (*AccountGDPRHandler, *sql.DB, *store.Queries, string) {
	t.Helper()
	sqlDB, _ := newSchemaDB(t, "gdpr.db")
	q := store.New(sqlDB)
	if err := q.CreateUser(context.Background(), store.CreateUserParams{
		ID: "user-gdpr", Email: "alice@example.com", Name: "Alice",
		PasswordHash: "x", Role: "editor",
	}); err != nil {
		t.Fatal(err)
	}
	return NewAccountGDPRHandler(q), sqlDB, q, "user-gdpr"
}

func gdprAuthedReq(method, path, userID, email, role string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	user := &authmw.AuthUser{ID: userID, Email: email, Name: "X", Role: role}
	return r.WithContext(context.WithValue(r.Context(), authmw.UserContextKey, user))
}

func TestGDPRExport_HappyPathProducesZipWithUserJson(t *testing.T) {
	h, _, _, userID := newGDPRTestStack(t)
	r := gdprAuthedReq("GET", "/api/account/export", userID, "alice@example.com", "editor")
	w := httptest.NewRecorder()
	h.Export(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "application/zip" {
		t.Errorf("content-type = %q, want application/zip", got)
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatalf("zip parse: %v", err)
	}
	saw := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(rc)
		_ = rc.Close()
		saw[f.Name] = string(body)
	}
	for _, want := range []string{"user.json", "workspaces.json", "sites.json", "pages.json", "collections.json", "collection_items.json", "README.md"} {
		if _, ok := saw[want]; !ok {
			t.Errorf("zip missing %q", want)
		}
	}
	// user.json must redact the secrets
	if strings.Contains(saw["user.json"], "password_hash") && !strings.Contains(saw["user.json"], "_redactions") {
		t.Error("user.json must redact password_hash; saw raw value")
	}
	var userBlob map[string]any
	if err := json.Unmarshal([]byte(saw["user.json"]), &userBlob); err != nil {
		t.Fatalf("user.json invalid JSON: %v", err)
	}
	if userBlob["email"] != "alice@example.com" {
		t.Errorf("user.json email = %v, want alice@example.com", userBlob["email"])
	}
	if _, leaked := userBlob["password_hash"]; leaked {
		t.Error("password_hash leaked into export")
	}
	if _, leaked := userBlob["totp_secret"]; leaked {
		t.Error("totp_secret leaked into export")
	}
}

func TestGDPRExport_AnonymousReturns401(t *testing.T) {
	h, _, _, _ := newGDPRTestStack(t)
	r := httptest.NewRequest("GET", "/api/account/export", nil)
	w := httptest.NewRecorder()
	h.Export(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestGDPRDelete_FlagsDeletionRequestedAt(t *testing.T) {
	h, sqlDB, _, userID := newGDPRTestStack(t)
	r := gdprAuthedReq("POST", "/api/account/delete", userID, "alice@example.com", "editor")
	w := httptest.NewRecorder()
	h.Delete(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["status"] != "deletion_requested" {
		t.Errorf("status = %v, want deletion_requested", body["status"])
	}
	if body["scheduled_delete_at"] == "" || body["scheduled_delete_at"] == nil {
		t.Error("scheduled_delete_at must be present")
	}

	// Verify the row landed.
	var got string
	if err := sqlDB.QueryRow(`SELECT deletion_requested_at FROM users WHERE id = ?`, userID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got == "" {
		t.Error("deletion_requested_at should be non-empty after delete")
	}
}

func TestGDPRCancelDelete_ClearsTimestamp(t *testing.T) {
	h, sqlDB, _, userID := newGDPRTestStack(t)
	r := gdprAuthedReq("POST", "/api/account/delete", userID, "a@b.c", "editor")
	h.Delete(httptest.NewRecorder(), r)

	r2 := gdprAuthedReq("POST", "/api/account/delete/cancel", userID, "a@b.c", "editor")
	w := httptest.NewRecorder()
	h.CancelDelete(w, r2)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var got string
	if err := sqlDB.QueryRow(`SELECT deletion_requested_at FROM users WHERE id = ?`, userID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("deletion_requested_at = %q, want empty after cancel", got)
	}
}

func TestGDPRDelete_TokenVersionBumpedSoOldSessionsInvalidate(t *testing.T) {
	h, sqlDB, _, userID := newGDPRTestStack(t)
	var tvBefore int64
	if err := sqlDB.QueryRow(`SELECT token_version FROM users WHERE id = ?`, userID).Scan(&tvBefore); err != nil {
		t.Fatal(err)
	}

	r := gdprAuthedReq("POST", "/api/account/delete", userID, "a@b.c", "editor")
	h.Delete(httptest.NewRecorder(), r)

	var tvAfter int64
	if err := sqlDB.QueryRow(`SELECT token_version FROM users WHERE id = ?`, userID).Scan(&tvAfter); err != nil {
		t.Fatal(err)
	}
	if tvAfter <= tvBefore {
		t.Errorf("token_version not bumped: before=%d after=%d", tvBefore, tvAfter)
	}
}
