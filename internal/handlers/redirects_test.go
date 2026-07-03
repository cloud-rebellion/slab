package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	_ "modernc.org/sqlite"

	"github.com/bright-interaction/slab/internal/config"
	dbpkg "github.com/bright-interaction/slab/internal/db"
	"github.com/bright-interaction/slab/internal/store"
)

func newRedirectHandlerForTest(t *testing.T) (*RedirectHandler, *store.Queries, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "redirects.db")
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
	siteID := "abcdef0123456789abcdefr1"
	if err := q.CreateSite(context.Background(), store.CreateSiteParams{
		ID: siteID, Name: "T", Slug: "t-redir", PrimaryColor: "#000",
		SecondaryColor: "#000", BgColor: "#FFF", TextColor: "#111",
		FontHeading: "Inter", FontBody: "Inter", Lang: "en",
	}); err != nil {
		t.Fatalf("create site: %v", err)
	}
	// Sprint 3 (2026-05-06): bulk import is plan-gated, which requires
	// resolveUserWorkspace to find a workspace. Seed an OSS workspace
	// so the gate's "unlimited" path returns -1 and tests pass without
	// hitting plan caps.
	if err := q.CreateWorkspace(context.Background(), store.CreateWorkspaceParams{
		ID: "ws-redir-test", Name: "Test", Slug: "test-redir-ws",
		Plan: "oss", Region: "eu", BillingEmail: "ops@test.local",
		Status: "active", TrialEndsAt: "",
	}); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	cfg := &config.Config{BaseURL: "http://localhost:8080"}
	return NewRedirectHandler(cfg, q), q, siteID
}

func withRedirectParams(siteID, redirectID string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("siteID", siteID)
		if redirectID != "" {
			rctx.URLParams.Add("redirectID", redirectID)
		}
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
		// Sprint 3 (2026-05-06): bulk import requires auth context.
		// Inject admin so resolveUserWorkspace finds the seeded ws.
		// Other handlers (Create/Update/Delete/Get/List) ignore the
		// extra context, so this is a safe no-op for them.
		r = seedAuthCtx(r)
		h(w, r)
	}
}

func postJSONRedir(handler http.HandlerFunc, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler(rr, req)
	return rr
}

// ------- Create -------

func TestRedirects_CreateHappyPath(t *testing.T) {
	h, q, siteID := newRedirectHandlerForTest(t)
	rr := postJSONRedir(withRedirectParams(siteID, "", h.Create), map[string]any{
		"from_path": "/old", "to_path": "/new", "status_code": 301,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201 got %d body=%s", rr.Code, rr.Body.String())
	}
	rows, _ := q.ListRedirectsBySite(context.Background(), siteID)
	if len(rows) != 1 || rows[0].FromPath != "/old" || rows[0].ToPath != "/new" || rows[0].IsAuto != 0 {
		t.Errorf("unexpected stored row: %+v", rows)
	}
}

func TestRedirects_CreateRejectsHostileFromPath(t *testing.T) {
	h, _, siteID := newRedirectHandlerForTest(t)
	rr := postJSONRedir(withRedirectParams(siteID, "", h.Create), map[string]any{
		"from_path": "/a}return 500;{", "to_path": "/x", "status_code": 301,
	})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422 got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "forbidden characters") {
		t.Errorf("want clear validation error, got %s", rr.Body.String())
	}
}

func TestRedirects_CreateRejectsProtocolRelative(t *testing.T) {
	h, _, siteID := newRedirectHandlerForTest(t)
	rr := postJSONRedir(withRedirectParams(siteID, "", h.Create), map[string]any{
		"from_path": "/safe", "to_path": "//evil.com/", "status_code": 301,
	})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("open-redirect via // must be rejected, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestRedirects_CreateRejectsOffSite(t *testing.T) {
	h, _, siteID := newRedirectHandlerForTest(t)
	rr := postJSONRedir(withRedirectParams(siteID, "", h.Create), map[string]any{
		"from_path": "/safe", "to_path": "https://evil.com/", "status_code": 301,
	})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("off-site to_path must be rejected, got %d", rr.Code)
	}
}

func TestRedirects_CreateRejectsNoOp(t *testing.T) {
	h, _, siteID := newRedirectHandlerForTest(t)
	rr := postJSONRedir(withRedirectParams(siteID, "", h.Create), map[string]any{
		"from_path": "/x", "to_path": "/x", "status_code": 301,
	})
	if rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "no-op") {
		t.Errorf("from==to must be rejected as no-op, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestRedirects_CreateRejectsBadStatusCode(t *testing.T) {
	h, _, siteID := newRedirectHandlerForTest(t)
	rr := postJSONRedir(withRedirectParams(siteID, "", h.Create), map[string]any{
		"from_path": "/x", "to_path": "/y", "status_code": 418,
	})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422 for status_code=418 got %d", rr.Code)
	}
}

func TestRedirects_CreateRejectsCollisionWithExistingPage(t *testing.T) {
	h, q, siteID := newRedirectHandlerForTest(t)
	// Seed a published page at slug "about-us".
	if err := q.CreatePage(context.Background(), store.CreatePageParams{
		ID: "p1", SiteID: siteID, Title: "About", Slug: "about-us",
		Layout: "default", SortOrder: 0,
	}); err != nil {
		t.Fatalf("seed page: %v", err)
	}
	rr := postJSONRedir(withRedirectParams(siteID, "", h.Create), map[string]any{
		"from_path": "/about-us", "to_path": "/about", "status_code": 301,
	})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("page-slug collision must reject, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "already exists") {
		t.Errorf("want collision error, got %s", rr.Body.String())
	}
}

func TestRedirects_CreateRejectsDuplicate(t *testing.T) {
	h, _, siteID := newRedirectHandlerForTest(t)
	first := postJSONRedir(withRedirectParams(siteID, "", h.Create), map[string]any{
		"from_path": "/dup", "to_path": "/a", "status_code": 301,
	})
	if first.Code != http.StatusCreated {
		t.Fatalf("first create failed: %s", first.Body.String())
	}
	second := postJSONRedir(withRedirectParams(siteID, "", h.Create), map[string]any{
		"from_path": "/dup", "to_path": "/b", "status_code": 301,
	})
	if second.Code != http.StatusConflict {
		t.Errorf("duplicate must 409, got %d", second.Code)
	}
}

// ------- Cross-tenant -------

func TestRedirects_GetCrossTenantBlocked(t *testing.T) {
	h, q, siteA := newRedirectHandlerForTest(t)
	siteB := "abcdef0123456789abcdefb2"
	_ = q.CreateSite(context.Background(), store.CreateSiteParams{
		ID: siteB, Name: "B", Slug: "b-redir", PrimaryColor: "#000",
		SecondaryColor: "#000", BgColor: "#FFF", TextColor: "#111",
		FontHeading: "Inter", FontBody: "Inter", Lang: "en",
	})
	// Seed a redirect on siteA, try to fetch via siteB context.
	_ = q.CreateRedirect(context.Background(), store.CreateRedirectParams{
		ID: "rA", SiteID: siteA, FromPath: "/x", ToPath: "/y", StatusCode: 301,
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	withRedirectParams(siteB, "rA", h.Get)(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("cross-tenant Get must 404, got %d", rr.Code)
	}
}

// ------- Bulk import -------

func TestRedirects_BulkImportJSONHappyPath(t *testing.T) {
	h, q, siteID := newRedirectHandlerForTest(t)
	rr := postJSONRedir(withRedirectParams(siteID, "", h.BulkImport), map[string]any{
		"items": []map[string]any{
			{"from_path": "/a", "to_path": "/x", "status_code": 301},
			{"from_path": "/b", "to_path": "/y", "status_code": 302},
		},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201 got %d body=%s", rr.Code, rr.Body.String())
	}
	rows, _ := q.ListRedirectsBySite(context.Background(), siteID)
	if len(rows) != 2 {
		t.Errorf("want 2 rows, got %d", len(rows))
	}
}

func TestRedirects_BulkImportRollsBackOnInvalidRow(t *testing.T) {
	h, q, siteID := newRedirectHandlerForTest(t)
	rr := postJSONRedir(withRedirectParams(siteID, "", h.BulkImport), map[string]any{
		"items": []map[string]any{
			{"from_path": "/good1", "to_path": "/x", "status_code": 301},
			{"from_path": "/bad}return", "to_path": "/y", "status_code": 301},
			{"from_path": "/good2", "to_path": "/z", "status_code": 301},
		},
	})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422 got %d body=%s", rr.Code, rr.Body.String())
	}
	rows, _ := q.ListRedirectsBySite(context.Background(), siteID)
	if len(rows) != 0 {
		t.Errorf("validation failure must roll back; got %d rows", len(rows))
	}
}

func TestRedirects_BulkImportRejectsIntraBatchDuplicate(t *testing.T) {
	h, q, siteID := newRedirectHandlerForTest(t)
	rr := postJSONRedir(withRedirectParams(siteID, "", h.BulkImport), map[string]any{
		"items": []map[string]any{
			{"from_path": "/dup", "to_path": "/a", "status_code": 301},
			{"from_path": "/dup", "to_path": "/b", "status_code": 301},
		},
	})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422 for duplicate from_path in batch, got %d body=%s", rr.Code, rr.Body.String())
	}
	rows, _ := q.ListRedirectsBySite(context.Background(), siteID)
	if len(rows) != 0 {
		t.Errorf("must not insert any rows when batch has duplicate, got %d", len(rows))
	}
}

func TestRedirects_BulkImportCSV(t *testing.T) {
	h, q, siteID := newRedirectHandlerForTest(t)
	body := "from,to,status\n/old,/new,301\n/legacy,/modern,302\n"
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "text/csv")
	rr := httptest.NewRecorder()
	withRedirectParams(siteID, "", h.BulkImport)(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201 got %d body=%s", rr.Code, rr.Body.String())
	}
	rows, _ := q.ListRedirectsBySite(context.Background(), siteID)
	if len(rows) != 2 {
		t.Errorf("want 2 rows, got %d (%+v)", len(rows), rows)
	}
}

func TestRedirects_BulkImportCSVNoHeaderTwoCols(t *testing.T) {
	h, q, siteID := newRedirectHandlerForTest(t)
	// No header, no status column - status defaults to 301.
	body := "/a,/x\n/b,/y\n"
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "text/csv")
	rr := httptest.NewRecorder()
	withRedirectParams(siteID, "", h.BulkImport)(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201 got %d body=%s", rr.Code, rr.Body.String())
	}
	rows, _ := q.ListRedirectsBySite(context.Background(), siteID)
	if len(rows) != 2 || rows[0].StatusCode != 301 {
		t.Errorf("want 2 rows w/ status 301, got %+v", rows)
	}
}

// ------- Verify (pre-launch URL diff) -------

func TestRedirects_VerifyBucketsCorrectly(t *testing.T) {
	h, q, siteID := newRedirectHandlerForTest(t)
	// Live page at /about
	_ = q.CreatePage(context.Background(), store.CreatePageParams{
		ID: "p1", SiteID: siteID, Title: "About", Slug: "about",
		Layout: "default",
	})
	// Exact 301: /old-blog -> /blog
	_ = q.CreateRedirect(context.Background(), store.CreateRedirectParams{
		ID: "r1", SiteID: siteID, FromPath: "/old-blog", ToPath: "/blog", StatusCode: 301,
	})
	// 410 gone: /dead
	_ = q.CreateRedirect(context.Background(), store.CreateRedirectParams{
		ID: "r2", SiteID: siteID, FromPath: "/dead", ToPath: "/whatever", StatusCode: 410,
	})
	// Regex: /archive/* -> /
	_ = q.CreateRedirect(context.Background(), store.CreateRedirectParams{
		ID: "r3", SiteID: siteID, FromPath: "~ ^/archive/.*$", ToPath: "/", StatusCode: 301,
	})

	rr := postJSONRedir(withRedirectParams(siteID, "", h.Verify), map[string]any{
		"urls": []string{
			"https://old-site.com/about",          // -> 200 (page exists)
			"https://old-site.com/about/",         // -> 200 (trailing slash normalized)
			"https://old-site.com/old-blog",       // -> 301 (exact match)
			"https://old-site.com/dead",           // -> 410
			"https://old-site.com/archive/2020/x", // -> 301 (regex)
			"/lonely-orphan",                      // -> 404
			"https://old-site.com/about?ref=x",    // -> 200 (query stripped)
			"",                                    // -> malformed
		},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200 got %d body=%s", rr.Code, rr.Body.String())
	}
	var resp VerifyResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Will200) != 3 {
		t.Errorf("want 3 will_200, got %d: %v", len(resp.Will200), resp.Will200)
	}
	if len(resp.Will301) != 2 {
		t.Errorf("want 2 will_301, got %d: %v", len(resp.Will301), resp.Will301)
	}
	if len(resp.Will410) != 1 {
		t.Errorf("want 1 will_410, got %d: %v", len(resp.Will410), resp.Will410)
	}
	if len(resp.Will404) != 1 {
		t.Errorf("want 1 will_404, got %d: %v", len(resp.Will404), resp.Will404)
	}
	if len(resp.Malformed) != 1 {
		t.Errorf("want 1 malformed, got %d: %v", len(resp.Malformed), resp.Malformed)
	}
}

func TestRedirects_VerifyEmptyInputRejected(t *testing.T) {
	h, _, siteID := newRedirectHandlerForTest(t)
	rr := postJSONRedir(withRedirectParams(siteID, "", h.Verify), map[string]any{
		"urls": []string{},
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("empty urls must 400, got %d", rr.Code)
	}
}

// ------- Update -------

func TestRedirects_UpdateBasic(t *testing.T) {
	h, q, siteID := newRedirectHandlerForTest(t)
	_ = q.CreateRedirect(context.Background(), store.CreateRedirectParams{
		ID: "r1", SiteID: siteID, FromPath: "/old", ToPath: "/v1", StatusCode: 301,
	})
	body, _ := json.Marshal(map[string]any{
		"from_path": "/old", "to_path": "/v2", "status_code": 302,
	})
	req := httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	withRedirectParams(siteID, "r1", h.Update)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200 got %d body=%s", rr.Code, rr.Body.String())
	}
	got, _ := q.GetRedirectByID(context.Background(), "r1")
	if got.ToPath != "/v2" || got.StatusCode != 302 {
		t.Errorf("update did not apply: %+v", got)
	}
}

// ------- Delete -------

func TestRedirects_DeleteRemovesRow(t *testing.T) {
	h, q, siteID := newRedirectHandlerForTest(t)
	_ = q.CreateRedirect(context.Background(), store.CreateRedirectParams{
		ID: "rdel", SiteID: siteID, FromPath: "/x", ToPath: "/y", StatusCode: 301,
	})
	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	rr := httptest.NewRecorder()
	withRedirectParams(siteID, "rdel", h.Delete)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200 got %d body=%s", rr.Code, rr.Body.String())
	}
	if rows, _ := q.ListRedirectsBySite(context.Background(), siteID); len(rows) != 0 {
		t.Errorf("delete left rows: %+v", rows)
	}
}

// ------- Sprint 3: bulk import plan-quota gate -------

// newRedirectStackOnPlan constructs a redirect handler whose seeded
// workspace is on a specific billing plan, so the cap test can target
// any tier in the ladder.
func newRedirectStackOnPlan(t *testing.T, plan string) (*RedirectHandler, *store.Queries, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "redirq.db")
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
	siteID := "abcdef0123456789abcdefrq"
	_ = q.CreateSite(context.Background(), store.CreateSiteParams{
		ID: siteID, Name: "T", Slug: "t-redir-q", PrimaryColor: "#000",
		SecondaryColor: "#000", BgColor: "#FFF", TextColor: "#111",
		FontHeading: "Inter", FontBody: "Inter", Lang: "en",
	})
	_ = q.CreateWorkspace(context.Background(), store.CreateWorkspaceParams{
		ID: "ws-redir-quota", Name: "Quota", Slug: "redir-quota-ws",
		Plan: plan, Region: "eu", BillingEmail: "ops@test.local",
		Status: "active", TrialEndsAt: "",
	})
	cfg := &config.Config{BaseURL: "http://localhost:8080"}
	return NewRedirectHandler(cfg, q), q, siteID
}

// TestRedirects_BulkImportBlockedBySoloPlanCap verifies that 501 redirect
// rows on a Solo plan (cap=500) returns 402 with no rows inserted.
func TestRedirects_BulkImportBlockedBySoloPlanCap(t *testing.T) {
	h, q, siteID := newRedirectStackOnPlan(t, "solo")
	items := make([]map[string]any, 501)
	for i := range items {
		items[i] = map[string]any{
			"from_path":   fmt.Sprintf("/q-from-%d", i),
			"to_path":     fmt.Sprintf("/q-to-%d", i),
			"status_code": 301,
		}
	}
	rr := postJSONRedir(withRedirectParams(siteID, "", h.BulkImport), map[string]any{
		"items": items,
	})
	if rr.Code != http.StatusPaymentRequired {
		t.Fatalf("want 402, got %d body=%s", rr.Code, rr.Body.String())
	}
	rows, _ := q.ListRedirectsBySite(context.Background(), siteID)
	if len(rows) != 0 {
		t.Errorf("quota-blocked import must insert zero rows, got %d", len(rows))
	}
}

// TestRedirects_BulkImportAllowedAtSoloCap verifies the boundary: exactly
// 500 redirects on a Solo plan succeeds.
func TestRedirects_BulkImportAllowedAtSoloCap(t *testing.T) {
	h, q, siteID := newRedirectStackOnPlan(t, "solo")
	items := make([]map[string]any, 500)
	for i := range items {
		items[i] = map[string]any{
			"from_path":   fmt.Sprintf("/cap-from-%d", i),
			"to_path":     fmt.Sprintf("/cap-to-%d", i),
			"status_code": 301,
		}
	}
	rr := postJSONRedir(withRedirectParams(siteID, "", h.BulkImport), map[string]any{
		"items": items,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("at-cap import must succeed, got %d body=%s", rr.Code, rr.Body.String())
	}
	rows, _ := q.ListRedirectsBySite(context.Background(), siteID)
	if len(rows) != 500 {
		t.Errorf("want 500 rows, got %d", len(rows))
	}
}

// TestRedirects_BulkImportAccountsForExistingRedirects: 499 already
// stored on a Solo plan, importing 2 more must 402 because total = 501.
func TestRedirects_BulkImportAccountsForExisting(t *testing.T) {
	h, q, siteID := newRedirectStackOnPlan(t, "solo")
	for i := 0; i < 499; i++ {
		_ = q.CreateRedirect(context.Background(), store.CreateRedirectParams{
			ID: fmt.Sprintf("pre%04d", i), SiteID: siteID,
			FromPath: fmt.Sprintf("/pre-%d", i), ToPath: fmt.Sprintf("/dst-%d", i),
			StatusCode: 301, IsAuto: 0,
		})
	}
	rr := postJSONRedir(withRedirectParams(siteID, "", h.BulkImport), map[string]any{
		"items": []map[string]any{
			{"from_path": "/extra-a", "to_path": "/x", "status_code": 301},
			{"from_path": "/extra-b", "to_path": "/y", "status_code": 301},
		},
	})
	if rr.Code != http.StatusPaymentRequired {
		t.Errorf("want 402 (existing+projected over cap), got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestRedirects_BulkImportAllowedOnOSSPlan: OSS plan returns -1
// (unlimited) so a large import only hits the global 50k ceiling.
func TestRedirects_BulkImportAllowedOnOSSPlan(t *testing.T) {
	h, _, siteID := newRedirectStackOnPlan(t, "oss")
	items := make([]map[string]any, 600) // > solo cap, < global ceiling
	for i := range items {
		items[i] = map[string]any{
			"from_path":   fmt.Sprintf("/oss-%d", i),
			"to_path":     fmt.Sprintf("/d-%d", i),
			"status_code": 301,
		}
	}
	rr := postJSONRedir(withRedirectParams(siteID, "", h.BulkImport), map[string]any{
		"items": items,
	})
	if rr.Code != http.StatusCreated {
		t.Errorf("OSS unlimited tier must allow large bulk import, got %d", rr.Code)
	}
}
