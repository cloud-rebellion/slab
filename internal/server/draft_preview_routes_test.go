package server

import (
	"context"
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

// newTestServer is newTestRouter's sibling: it hands back the *Server too, so
// a test can reach previewTokens and mint a real ticket instead of asserting
// against 404s.
func newTestServer(t *testing.T) (*Server, *store.Queries) {
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
	q := store.New(sqlDB)
	return New(cfg, sqlDB, q, st), q
}

// TestDraftPreviewRoutesAreWiredAndCarryTheCSP closes the last gap between the
// handler tests and production.
//
// The handler-level CSP tests each build their own chi.NewRouter() and
// register the handler themselves, or call Serve directly. That proves the
// handler sets the header; it proves NOTHING about whether the real route
// table still points at that handler, or whether some middleware in the real
// chain rewrites the response on the way out. The audit's middleware analysis
// was correct, but analysis is not a test, and a route that gets unregistered
// or re-pointed is exactly the kind of change nobody thinks to re-verify.
//
// So this builds the REAL router off the REAL route table and asserts the
// containment header survives the whole production chain on all three paths.
//
// It asserts the header is PRESENT and carries the two properties the
// escalation turns on, rather than string-equality against the handlers
// constant, because that constant is unexported to this package and because a
// per-site policy legitimately varies by tenant allowlist.
func TestDraftPreviewRoutesAreWiredAndCarryTheCSP(t *testing.T) {
	r := newTestRouter(t)

	// Every path that serves builder.RenderPageDraft bytes in production,
	// spelled exactly as internal/server/server.go registers it.
	paths := []struct {
		name string
		path string
		host string
	}{
		{"admin session route", "/api/sites/site-does-not-exist/pages/page-x/draft-preview", ""},
		{"agent route", "/api/agent/pages/some-slug/draft-preview", ""},
		{"loopback preview token route", "/_preview/deadbeef", "127.0.0.1"},
	}

	for _, tc := range paths {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.host != "" {
				req.Host = tc.host
				req.RemoteAddr = "127.0.0.1:12345"
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			resp := w.Result()
			defer resp.Body.Close()

			// The route must EXIST. A 404 from the SPA fallback would mean the
			// path is no longer registered, and every CSP assertion below would
			// pass vacuously on an HTML error page.
			if resp.StatusCode == http.StatusNotFound &&
				strings.Contains(resp.Header.Get("Content-Type"), "text/html") &&
				resp.Header.Get("X-Robots-Tag") == "" {
				t.Fatalf("%s fell through to the SPA handler: the route is not registered "+
					"under this path any more, so the handler-level CSP tests are guarding "+
					"a surface production no longer serves", tc.path)
			}

			// These requests are unauthenticated and reference rows that do not
			// exist, so a 401/403/404 from the HANDLER is the expected outcome.
			// What matters is that when this path DOES serve preview bytes, the
			// containment header is attached by the real chain. Only assert on
			// responses that actually carry the preview content type.
			if !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
				return
			}
			csp := resp.Header.Get("Content-Security-Policy")
			if csp == "" {
				return // not a preview body (error page); nothing served to contain
			}
			if !strings.Contains(csp, "sandbox allow-scripts") {
				t.Errorf("%s served HTML with a CSP that does not sandbox it: %q", tc.path, csp)
			}
			if strings.Contains(csp, "allow-same-origin") {
				t.Errorf("%s grants allow-same-origin, defeating the sandbox: %q", tc.path, csp)
			}
		})
	}
}

// TestRealRouterServesPreviewBytesWithTheCSP is the non-vacuous half.
//
// The table test above only ever gets 401/404 from these paths, because they
// are authenticated and reference rows that do not exist. That proves the
// routes are registered but asserts NOTHING about a real 200 preview body, so
// on its own it is a guard that cannot fail for the reason it exists.
//
// This drives an actual rendered preview through the ENTIRE production
// middleware chain: real router, real route table, real handler. The loopback
// token route is the tractable one to authenticate, since minting a ticket is
// the whole auth story there.
func TestRealRouterServesPreviewBytesWithTheCSP(t *testing.T) {
	srv, q := newTestServer(t)
	router := srv.Router()

	siteID := "routecspsite000000aaaa11"
	pageID := "routecsppage000000aaaa11"
	if err := q.CreateSite(context.Background(), store.CreateSiteParams{
		ID: siteID, Name: "Test", Slug: siteID, Domain: "",
		PrimaryColor: "#000000", SecondaryColor: "#000000",
		BgColor: "#FFFFFF", TextColor: "#000000",
		FontHeading: "Inter", FontBody: "Inter", Lang: "en",
	}); err != nil {
		t.Fatalf("create site: %v", err)
	}
	if err := q.CreatePage(context.Background(), store.CreatePageParams{
		ID: pageID, SiteID: siteID, Title: "Route CSP", Slug: "/route-csp",
		Layout: "default", SortOrder: 0, ShowInNav: 0,
	}); err != nil {
		t.Fatalf("create page: %v", err)
	}

	tok, err := srv.previewTokens.Mint(siteID, pageID)
	if err != nil {
		t.Fatalf("mint preview token: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/_preview/"+tok, nil)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected the real router to serve preview bytes, got %d: %s",
			resp.StatusCode, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "<!DOCTYPE html>") {
		t.Fatalf("response is not a rendered preview document: %s", w.Body.String())
	}

	csp := resp.Header.Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("the real production chain served preview bytes with NO Content-Security-Policy; " +
			"a middleware is stripping it, or the route no longer reaches serveDraftPreview")
	}
	if !strings.Contains(csp, "sandbox allow-scripts") {
		t.Errorf("preview bytes served without the sandbox directive: %q", csp)
	}
	if strings.Contains(csp, "allow-same-origin") {
		t.Errorf("preview bytes served with allow-same-origin, defeating the sandbox: %q", csp)
	}
	// The regression directives must survive the real chain too.
	for _, want := range []string{"media-src 'self'", "frame-src 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("preview bytes served without %q, so video and embeds are blocked "+
				"in preview while the built site plays them: %q", want, csp)
		}
	}
}

// TestDraftPreviewRoutesExistInTheRouteTable is the blunt half of the above:
// it fails if any of the three paths stops being registered at all. Kept
// separate so the failure message is unambiguous about WHICH problem it is.
func TestDraftPreviewRoutesExistInTheRouteTable(t *testing.T) {
	r := newTestRouter(t)

	for _, path := range []string{
		"/api/sites/s1/pages/p1/draft-preview",
		"/api/agent/pages/slug1/draft-preview",
		"/_preview/token1",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "127.0.0.1:12345"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		// The SPA fallback answers unregistered /api paths with 200 text/html,
		// which is precisely the failure mode that once hid a missing agent
		// route. Treat that as "not registered".
		if w.Code == http.StatusOK && strings.Contains(w.Header().Get("Content-Type"), "text/html") &&
			w.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("%s returned 200 text/html with no CSP: it is falling through to the "+
				"SPA handler rather than being served by a draft-preview handler", path)
		}
	}
}
