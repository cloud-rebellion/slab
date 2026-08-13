package handlers

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/config"
	"github.com/bright-interaction/slab/internal/store"
)

// ---------------------------------------------------------------------------
// The structural guard.
//
// The defect this replaces was not "the CSP is wrong", it was "the CSP is on
// one of three routes". A hand-listed route table would have reproduced the
// original mistake exactly, because a hand-listed anything only ever covers
// what somebody remembered. So the guard reads the callers out of the source:
// every file that calls builder.RenderPageDraft must hand its bytes to
// serveDraftPreview, and a fourth route fails this the day it is written
// rather than at the next audit.
// ---------------------------------------------------------------------------

// renderPageDraftCallerFiles returns every non-test file in this package that
// calls builder.RenderPageDraft.
func renderPageDraftCallerFiles(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	callers := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		body := string(src)
		if strings.Contains(body, "builder.RenderPageDraft(") {
			callers[name] = body
		}
	}
	return callers
}

// TestEveryRenderPageDraftCallerServesThroughHelper is the by-construction
// half. It does not care which routes exist; it cares that nobody writes
// draft-preview bytes to a ResponseWriter without going through the one
// function that attaches the containment policy.
func TestEveryRenderPageDraftCallerServesThroughHelper(t *testing.T) {
	callers := renderPageDraftCallerFiles(t)

	// If this drops to zero the guard has gone vacuous: either the renderer
	// was renamed or the callers moved package, and a silent pass here would
	// be worse than a failure.
	if len(callers) < 3 {
		t.Fatalf("expected at least 3 files calling builder.RenderPageDraft, found %d (%v); "+
			"the guard is keyed on that call and has drifted", len(callers), keysOf(callers))
	}

	for name, body := range callers {
		if !strings.Contains(body, "serveDraftPreview(") {
			t.Errorf("%s calls builder.RenderPageDraft but never calls serveDraftPreview; "+
				"draft-preview bytes must be written by that helper or they ship without "+
				"the sandbox CSP that contains agent-authored component templates", name)
		}
		// Writing the body directly is the specific regression: it is how a
		// route ends up serving the bytes with its own ad-hoc header set.
		if writesBodyDirectly(body) {
			t.Errorf("%s calls builder.RenderPageDraft and also writes a response body "+
				"directly near it; route the bytes through serveDraftPreview instead", name)
		}
	}
}

// writesBodyDirectly looks for w.Write of a variable named html, the shape
// all three routes used before they were centralised.
var directWriteRE = regexp.MustCompile(`w\.Write\(\[\]byte\(html\)\)`)

func writesBodyDirectly(body string) bool { return directWriteRE.MatchString(body) }

// TestDraftPreviewCSPIsAnOpaqueOriginPolicy pins the two properties the
// escalation actually turns on, so a future edit cannot quietly weaken them
// while leaving a header present that still looks like a defence.
func TestDraftPreviewCSPIsAnOpaqueOriginPolicy(t *testing.T) {
	// The opaque origin. Without this the document is same-origin with the
	// admin app and a credentialed fetch is fully authorized.
	if !strings.Contains(DraftPreviewCSP, "sandbox allow-scripts") {
		t.Errorf("DraftPreviewCSP lost its sandbox directive: %q", DraftPreviewCSP)
	}
	// allow-same-origin would hand the origin straight back and make the
	// sandbox directive decorative.
	if strings.Contains(DraftPreviewCSP, "allow-same-origin") {
		t.Errorf("DraftPreviewCSP allows same-origin, which defeats the sandbox: %q", DraftPreviewCSP)
	}
	// Reachability. Measured in Chrome: sandbox alone still lets a no-cors
	// POST arrive at the server (unauthenticated, but arriving). default-src
	// 'none' covers connect-src and stops the request being sent.
	if !strings.Contains(DraftPreviewCSP, "default-src 'none'") &&
		!strings.Contains(DraftPreviewCSP, "connect-src 'none'") {
		t.Errorf("DraftPreviewCSP no longer blocks connect-src; a sandboxed document "+
			"can still emit unauthenticated requests at the loopback interface: %q", DraftPreviewCSP)
	}
	// The feature. script-src 'none' would contain the payload but also kill
	// the component animation the preview exists to show.
	if strings.Contains(DraftPreviewCSP, "script-src 'none'") {
		t.Errorf("DraftPreviewCSP blocks all scripts, which breaks the inline component "+
			"animation locked in by builder/draft_preview_inline_test.go: %q", DraftPreviewCSP)
	}
}

// ---------------------------------------------------------------------------
// The behavioural guard: all three routes, over real HTTP.
// ---------------------------------------------------------------------------

func seedPreviewPage(t *testing.T, q *store.Queries, siteID, pageID, slug string) {
	t.Helper()
	seedSite(t, q, siteID)
	if err := q.CreatePage(context.Background(), store.CreatePageParams{
		ID:        pageID,
		SiteID:    siteID,
		Title:     "CSP Preview",
		Slug:      slug,
		Layout:    "default",
		SortOrder: 0,
		ShowInNav: 0,
	}); err != nil {
		t.Fatalf("create page: %v", err)
	}
}

// doAgentDraftPreview drives the agent draft-preview route with an
// authenticated agent identity in context, mirroring the production route
// table at internal/server/server.go.
func doAgentDraftPreview(t *testing.T, sqlDB *sql.DB, q *store.Queries, siteID, slug string) *http.Response {
	t.Helper()
	h := NewAgentHandler(&config.Config{}, q, sqlDB)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := context.WithValue(req.Context(), authmw.AgentContextKey, &authmw.AgentIdentity{
				KeyID:        "csp-preview-key",
				SiteID:       siteID,
				Capabilities: []string{"read", "write"},
			})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Get("/api/agent/preview", h.DraftPreview)

	req := httptest.NewRequest(http.MethodGet, "/api/agent/preview?page="+slug, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Result()
}

// TestAllThreeDraftPreviewRoutesSetTheSandboxCSP drives each of the three
// routes that serve RenderPageDraft output and asserts the containment header
// is on the response. Two of these three shipped with no CSP at all.
func TestAllThreeDraftPreviewRoutesSetTheSandboxCSP(t *testing.T) {
	_, q := setupDeployTestDB(t)
	siteID := "cspprevsite00000000aaaa1"
	pageID := "cspprevpage00000000aaaa1"
	seedPreviewPage(t, q, siteID, pageID, "/csp-preview")

	type routeCase struct {
		name string
		do   func(t *testing.T) *http.Response
	}

	cases := []routeCase{
		{"pages.go PageDraftPreview", func(t *testing.T) *http.Response {
			h := NewPageHandler(&config.Config{}, q)
			r := chi.NewRouter()
			r.Get("/api/sites/{siteID}/pages/{pageID}/draft-preview", h.PageDraftPreview)
			req := httptest.NewRequest(http.MethodGet,
				"/api/sites/"+siteID+"/pages/"+pageID+"/draft-preview", nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			return w.Result()
		}},
		{"preview_token.go Serve", func(t *testing.T) *http.Response {
			m := NewPreviewTokenManager(q)
			tok, err := m.Mint(siteID, pageID)
			if err != nil {
				t.Fatalf("mint: %v", err)
			}
			req := httptest.NewRequest(http.MethodGet, "/_preview/"+tok, nil)
			req.RemoteAddr = "127.0.0.1:12345"
			w := httptest.NewRecorder()
			m.Serve(w, req)
			return w.Result()
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := tc.do(t)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("expected 200, got %d", resp.StatusCode)
			}
			got := resp.Header.Get("Content-Security-Policy")
			if got == "" {
				t.Fatalf("no Content-Security-Policy on the response; this route serves "+
					"agent-authored component templates as a top-level document on the admin origin")
			}
			if got != DraftPreviewCSP {
				t.Errorf("CSP differs from the shared policy.\n got: %q\nwant: %q", got, DraftPreviewCSP)
			}
		})
	}
}

// TestAgentDraftPreviewSetsTheSandboxCSP covers the third route. It is split
// out because the agent route needs an authenticated agent in the request
// context, not just a chi router.
func TestAgentDraftPreviewSetsTheSandboxCSP(t *testing.T) {
	sqlDB, q := setupDeployTestDB(t)
	siteID := "cspagentsite000000aaaa11"
	pageID := "cspagentpage000000aaaa11"
	seedPreviewPage(t, q, siteID, pageID, "/csp-preview")

	resp := doAgentDraftPreview(t, sqlDB, q, siteID, "csp-preview")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	got := resp.Header.Get("Content-Security-Policy")
	if got != DraftPreviewCSP {
		t.Errorf("agent route CSP differs from the shared policy.\n got: %q\nwant: %q", got, DraftPreviewCSP)
	}
}
