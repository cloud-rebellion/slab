package server

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/bright-interaction/slab/internal/config"
	dbpkg "github.com/bright-interaction/slab/internal/db"
	"github.com/bright-interaction/slab/internal/storage"
	"github.com/bright-interaction/slab/internal/store"
)

// newTestRouter builds the REAL router off the real route table. Nothing
// here is a stand-in: the point of this test is that a route's auth
// posture is decided by where it is registered, and only the real
// registration can be trusted to tell us.
func newTestRouter(t *testing.T) http.Handler {
	t.Helper()
	dir := t.TempDir()
	sqlDB, err := sql.Open("sqlite", filepath.Join(dir, "test.sqlite")+"?_journal_mode=WAL&_busy_timeout=5000&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if _, err := sqlDB.Exec(string(dbpkg.Schema)); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	st, err := storage.NewLocalStore(filepath.Join(dir, "media"))
	if err != nil {
		t.Fatalf("storage: %v", err)
	}

	cfg := &config.Config{
		Env:           config.EnvDevelopment,
		BaseURL:       "http://localhost:8080",
		JWTSecret:     "test-jwt-secret-that-is-long-enough-for-hs256",
		DataDir:       dir,
		MaxUploadSize: 20 << 20,
	}
	return New(cfg, sqlDB, store.New(sqlDB), st).Router()
}

// postNoCookie sends a cookie-less POST, which is exactly what Mollie and
// an anonymous visitor's browser send.
func postNoCookie(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestRoutes_PublicRoutesAreReachableWithoutCookie is the regression guard
// for the two routes that were registered inside the authenticated group
// while their comments claimed they were public.
//
// The 404 control matters as much as the assertions. Without it a 401 and
// "the route does not exist" are indistinguishable, which is precisely how
// the original defect survived: the e2e specs posted with an authenticated
// fixture, got a 201, and proved nothing about the anonymous case.
func TestRoutes_PublicRoutesAreReachableWithoutCookie(t *testing.T) {
	h := newTestRouter(t)

	// Control 1: an unregistered path. Proves a 404 is available as an
	// answer, so a 401 elsewhere is a real auth rejection and not chi's
	// catch-all.
	if got := postNoCookie(t, h, "/api/starter-kits-DOES-NOT-EXIST", "{}").Code; got != http.StatusNotFound {
		t.Fatalf("unregistered path control: got %d, want 404; the controls in this test are unreliable", got)
	}

	// Control 2: a route that IS meant to sit behind the session cookie.
	// Proves the authenticated group still rejects, so a non-401 on the
	// public routes below means they left the group rather than that the
	// middleware stopped working.
	if got := postNoCookie(t, h, "/api/sites", `{"name":"x","slug":"x"}`).Code; got != http.StatusUnauthorized {
		t.Fatalf("authenticated-route control /api/sites: got %d, want 401; the auth group is not guarding anything", got)
	}

	public := []struct {
		name string
		path string
		body string
	}{
		{
			// Mollie posts this URL (internal/cloud/mollie/checkout.go
			// builds it from BaseURL). A 401 here means subscription
			// payments never confirm.
			name: "mollie billing callback",
			path: "/api/billing/webhook",
			body: "id=tr_test",
		},
		{
			// Anonymous visitor submitting the waitlist form.
			name: "public waitlist submit",
			path: "/api/waitlist",
			body: `{"email":"visitor@example.com"}`,
		},
	}
	for _, tc := range public {
		t.Run(tc.name, func(t *testing.T) {
			rec := postNoCookie(t, h, tc.path, tc.body)
			if rec.Code == http.StatusUnauthorized {
				t.Fatalf("%s (%s) returned 401 without a cookie; it is registered inside the authenticated group", tc.name, tc.path)
			}
			if rec.Code == http.StatusNotFound {
				t.Fatalf("%s (%s) returned 404; the route is not registered at all", tc.name, tc.path)
			}
		})
	}
}

// TestRoutes_StorefrontMollieWebhookStaysPublic pins the other Mollie
// callback, the per-site storefront one. It was already correct and this
// asserts a later edit does not sweep it into the authenticated group the
// way the billing one was.
func TestRoutes_StorefrontMollieWebhookStaysPublic(t *testing.T) {
	h := newTestRouter(t)
	// s.ordersH is wired by cmd/server, so this route may be absent in a
	// bare Router(). Absent is fine; behind auth is not.
	if got := postNoCookie(t, h, "/api/sites/some-site/payments/mollie/webhook", "id=tr_test").Code; got == http.StatusUnauthorized {
		t.Fatalf("storefront Mollie webhook returned 401 without a cookie; Mollie has no cookie to send")
	}
}
