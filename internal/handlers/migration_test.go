package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	_ "modernc.org/sqlite"

	"github.com/bright-interaction/slab/internal/config"
	"github.com/bright-interaction/slab/internal/migration"
	"github.com/bright-interaction/slab/internal/store"
)

// fakeUploaderForHandlers mirrors fakeUploader in the migration package
// (kept local so we don't reach across packages for a test type).
type fakeUploaderForHandlers struct {
	uploads atomic.Int32
}

func (f *fakeUploaderForHandlers) UploadFromURL(ctx context.Context, siteID, sourceURL, alt, caption string) (string, string, error) {
	if sourceURL == "" {
		return "", "", errors.New("empty source URL")
	}
	n := f.uploads.Add(1)
	return fmt.Sprintf("media_%020d", n), fmt.Sprintf("https://atomicsite.local/m/%d.jpg", n), nil
}

func newMigrationHandlerForTest(t *testing.T) (*MigrationHandler, *store.Queries, string) {
	t.Helper()
	sqlDB, _ := newSchemaDB(t, "migr.db")
	q := store.New(sqlDB)
	siteID := "abcdef0123456789abcdefm1"
	if err := q.CreateSite(context.Background(), store.CreateSiteParams{
		ID: siteID, Name: "T", Slug: "t-migr", PrimaryColor: "#000",
		SecondaryColor: "#000", BgColor: "#FFF", TextColor: "#111",
		FontHeading: "Inter", FontBody: "Inter", Lang: "en",
	}); err != nil {
		t.Fatalf("create site: %v", err)
	}
	// Sprint 3 (2026-05-06): Apply is plan-gated. Seed an OSS workspace
	// so the gate returns -1 (unlimited) and tests pass without hitting
	// caps. Cap-blocking is exercised in dedicated quota tests below.
	if err := q.CreateWorkspace(context.Background(), store.CreateWorkspaceParams{
		ID: "ws-migr-test", Name: "Test", Slug: "test-migr-ws",
		Plan: "oss", Region: "eu", BillingEmail: "ops@test.local",
		Status: "active", TrialEndsAt: "",
	}); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	cfg := &config.Config{BaseURL: "http://localhost:8080"}
	h := NewMigrationHandler(cfg, q, &fakeUploaderForHandlers{})
	// Sprint 4 (2026-05-06): wire an async verify-job manager so the
	// VerifyLive tests can exercise the queued/running/done lifecycle.
	// Started against a t-scoped context so workers tear down with the
	// test. SetVerifyJobManager + SetVerifyOptionsForTest cooperate so
	// the manager picks up AllowPrivate via the same call.
	jobMgr := migration.NewJobManager(q)
	jobCtx, jobCancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		jobCancel()
		jobMgr.Stop(context.Background())
	})
	jobMgr.Start(jobCtx)
	h.SetVerifyJobManager(jobMgr)
	return h, q, siteID
}

func withMigrationParams(siteID, migrationID string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("siteID", siteID)
		if migrationID != "" {
			rctx.URLParams.Add("migrationID", migrationID)
		}
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
		// Sprint 3 (2026-05-06): Apply requires auth context for the
		// plan-quota gate. Inject admin so resolveUserWorkspace finds
		// the seeded workspace. Non-gated handlers ignore the extra
		// context, so this is a safe no-op for them.
		r = seedAuthCtx(r)
		h(w, r)
	}
}

func postJSONMigr(handler http.HandlerFunc, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler(rr, req)
	return rr
}

// ----- Start: real sitemap crawl against a httptest server -----

func TestMigration_StartSitemapStoresManifest(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		base := "http://" + r.Host
		_, _ = fmt.Fprintf(w, `<?xml version="1.0"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
<url><loc>%s/about</loc></url><url><loc>%s/contact</loc></url>
</urlset>`, base, base)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprintf(w, `<html><head><title>%s</title></head><body><h1>%s</h1></body></html>`,
			r.URL.Path, r.URL.Path)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	h, q, siteID := newMigrationHandlerForTest(t)
	// We need allow_private fetches in the importer when targeting httptest.
	// The handler goes through migration.CrawlSitemap with default options
	// which has SafeFetch.AllowPrivate=false. To work around for this test
	// we POST a test-only flag... but the public Start API doesn't expose
	// AllowPrivate (rightly so). So bypass Start and seed the DB directly
	// with a manifest produced by an in-test crawl.
	manifest, err := migration.CrawlSitemap(context.Background(), srv.URL+"/sitemap.xml",
		migration.CrawlOptions{FetchOpts: migration.FetchOptions{AllowPrivate: true}})
	if err != nil {
		t.Fatalf("crawl: %v", err)
	}
	manifestJSON, _ := json.Marshal(manifest)
	migID := "test-mig-1"
	if err := q.CreateMigration(context.Background(), store.CreateMigrationParams{
		ID: migID, SiteID: siteID, SourceUrl: srv.URL + "/sitemap.xml",
		SourceType: "sitemap", ManifestJson: string(manifestJSON), Status: "ready",
	}); err != nil {
		t.Fatalf("seed migration: %v", err)
	}

	// Get returns the manifest.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	withMigrationParams(siteID, migID, h.Get)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("Get: want 200 got %d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got["status"] != "ready" {
		t.Errorf("status: %v", got["status"])
	}
}

// ----- Plan: returns conflicts when collisions exist -----

func TestMigration_PlanReturnsConflictsAgainstExistingPages(t *testing.T) {
	h, q, siteID := newMigrationHandlerForTest(t)
	// Seed a live page at slug "about".
	_ = q.CreatePage(context.Background(), store.CreatePageParams{
		ID: "p1", SiteID: siteID, Title: "About", Slug: "about", Layout: "default",
	})
	// Seed a manifest whose page collides at /about.
	manifest := migration.MigrationManifest{
		Pages: []migration.MigrationPage{{SourcePath: "/about", Title: "About"}},
	}
	mj, _ := json.Marshal(manifest)
	migID := "test-mig-conflict"
	_ = q.CreateMigration(context.Background(), store.CreateMigrationParams{
		ID: migID, SiteID: siteID, SourceUrl: "x", SourceType: "sitemap",
		ManifestJson: string(mj), Status: "ready",
	})

	rr := postJSONMigr(withMigrationParams(siteID, migID, h.Plan), map[string]any{})
	if rr.Code != http.StatusOK {
		t.Fatalf("Plan: %d body=%s", rr.Code, rr.Body.String())
	}
	var plan migration.URLPlan
	_ = json.Unmarshal(rr.Body.Bytes(), &plan)
	if len(plan.Conflicts) != 1 {
		t.Fatalf("want 1 conflict, got %d (%+v)", len(plan.Conflicts), plan.Conflicts)
	}
}

// ----- Apply end-to-end: manifest -> pages + redirects in DB -----

func TestMigration_ApplyCommitsManifest(t *testing.T) {
	h, q, siteID := newMigrationHandlerForTest(t)
	manifest := migration.MigrationManifest{
		Pages: []migration.MigrationPage{
			{SourcePath: "/about-us.html", Title: "About",
				MetaDescription: "About text"},
			{SourcePath: "/contact", Title: "Contact"},
		},
	}
	mj, _ := json.Marshal(manifest)
	migID := "test-mig-apply"
	_ = q.CreateMigration(context.Background(), store.CreateMigrationParams{
		ID: migID, SiteID: siteID, SourceUrl: "x", SourceType: "sitemap",
		ManifestJson: string(mj), Status: "ready",
	})

	rr := postJSONMigr(withMigrationParams(siteID, migID, h.Apply), map[string]any{})
	if rr.Code != http.StatusOK {
		t.Fatalf("Apply: %d body=%s", rr.Code, rr.Body.String())
	}

	// 2 pages should land.
	pages, _ := q.ListPagesBySite(context.Background(), siteID)
	if len(pages) != 2 {
		t.Errorf("DB pages: %d", len(pages))
	}
	// 1 redirect (.html stripped).
	redirects, _ := q.ListRedirectsBySite(context.Background(), siteID)
	if len(redirects) != 1 {
		t.Errorf("DB redirects: %d", len(redirects))
	}
	if redirects[0].FromPath != "/about-us.html" || redirects[0].ToPath != "/about-us" {
		t.Errorf("redirect mapping: %+v", redirects[0])
	}
	// Migration row status should flip to applied.
	row, _ := q.GetMigrationByID(context.Background(), migID)
	if row.Status != "applied" {
		t.Errorf("status not flipped: %q", row.Status)
	}
}

// ----- Apply: refuses second apply -----

func TestMigration_ApplyTwiceConflicts(t *testing.T) {
	h, q, siteID := newMigrationHandlerForTest(t)
	manifest := migration.MigrationManifest{
		Pages: []migration.MigrationPage{{SourcePath: "/x", Title: "X"}},
	}
	mj, _ := json.Marshal(manifest)
	migID := "test-mig-twice"
	_ = q.CreateMigration(context.Background(), store.CreateMigrationParams{
		ID: migID, SiteID: siteID, SourceUrl: "x", SourceType: "sitemap",
		ManifestJson: string(mj), Status: "ready",
	})

	first := postJSONMigr(withMigrationParams(siteID, migID, h.Apply), map[string]any{})
	if first.Code != http.StatusOK {
		t.Fatalf("first Apply: %d body=%s", first.Code, first.Body.String())
	}
	second := postJSONMigr(withMigrationParams(siteID, migID, h.Apply), map[string]any{})
	if second.Code != http.StatusConflict {
		t.Errorf("second Apply must 409, got %d", second.Code)
	}
}

// ----- Cross-tenant: site B can't see site A's migration -----

func TestMigration_CrossTenantBlocked(t *testing.T) {
	h, q, siteA := newMigrationHandlerForTest(t)
	siteB := "abcdef0123456789abcdefm2"
	_ = q.CreateSite(context.Background(), store.CreateSiteParams{
		ID: siteB, Name: "B", Slug: "b-migr", PrimaryColor: "#000",
		SecondaryColor: "#000", BgColor: "#FFF", TextColor: "#111",
		FontHeading: "Inter", FontBody: "Inter", Lang: "en",
	})
	migID := "test-mig-tenantA"
	_ = q.CreateMigration(context.Background(), store.CreateMigrationParams{
		ID: migID, SiteID: siteA, SourceUrl: "x", SourceType: "sitemap",
		ManifestJson: "{}", Status: "ready",
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	withMigrationParams(siteB, migID, h.Get)(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("cross-tenant Get must 404, got %d", rr.Code)
	}
}

// ----- Delete -----

func TestMigration_DeleteRemovesRow(t *testing.T) {
	h, q, siteID := newMigrationHandlerForTest(t)
	migID := "test-mig-del"
	_ = q.CreateMigration(context.Background(), store.CreateMigrationParams{
		ID: migID, SiteID: siteID, SourceUrl: "x", SourceType: "sitemap",
		ManifestJson: "{}", Status: "ready",
	})
	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	rr := httptest.NewRecorder()
	withMigrationParams(siteID, migID, h.Delete)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("Delete: %d", rr.Code)
	}
	rows, _ := q.ListMigrationsBySite(context.Background(), siteID)
	if len(rows) != 0 {
		t.Errorf("delete left rows: %+v", rows)
	}
}

// ----- List shape -----

func TestMigration_ListReturnsSummaryNotFullManifest(t *testing.T) {
	h, q, siteID := newMigrationHandlerForTest(t)
	manifest := migration.MigrationManifest{
		Pages: []migration.MigrationPage{
			{SourcePath: "/x", Title: "X", HTML: strings.Repeat("a", 10000)},
		},
		Stats: migration.MigrationStats{PagesFound: 1},
	}
	mj, _ := json.Marshal(manifest)
	_ = q.CreateMigration(context.Background(), store.CreateMigrationParams{
		ID: "id-list-1", SiteID: siteID, SourceUrl: "x", SourceType: "sitemap",
		ManifestJson: string(mj), Status: "ready",
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	withMigrationParams(siteID, "", h.List)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("List: %d", rr.Code)
	}
	// Body should NOT contain the 10kB HTML blob.
	if strings.Count(rr.Body.String(), "a") > 1000 {
		t.Errorf("List leaked full manifest body; expected only summary stats")
	}
}

// ----- Sprint 2: VerifyCoverage (pre-launch dry-run) -----

func TestMigration_VerifyCoverageBucketsCorrectly(t *testing.T) {
	h, q, siteID := newMigrationHandlerForTest(t)
	// Seed: a live page at /existing and a manual redirect /old -> /new.
	_ = q.CreatePage(context.Background(), store.CreatePageParams{
		ID: "p1", SiteID: siteID, Title: "Existing", Slug: "existing", Layout: "default",
	})
	_ = q.CreateRedirect(context.Background(), store.CreateRedirectParams{
		ID: "r1", SiteID: siteID, FromPath: "/old", ToPath: "/new",
		StatusCode: 301, IsAuto: 0,
	})
	// Manifest contributes a NEW page /from-manifest plus a redirect
	// /legacy -> /modern (auto from a slug-strip).
	manifest := migration.MigrationManifest{
		Pages: []migration.MigrationPage{
			{SourcePath: "/from-manifest", Title: "From manifest"},
			{SourcePath: "/legacy.html", Title: "Legacy"},
		},
	}
	mj, _ := json.Marshal(manifest)
	migID := "test-mig-vc"
	_ = q.CreateMigration(context.Background(), store.CreateMigrationParams{
		ID: migID, SiteID: siteID, SourceUrl: "x", SourceType: "sitemap",
		ManifestJson: string(mj), Status: "ready",
	})

	rr := postJSONMigr(withMigrationParams(siteID, migID, h.VerifyCoverage), map[string]any{
		"urls": []string{
			"https://old/existing",      // live page -> 200
			"https://old/from-manifest", // planned page -> 200 (post-apply)
			"https://old/old",           // existing redirect -> 301
			"https://old/legacy.html",   // planned redirect -> 301 (post-apply)
			"https://old/orphan",        // unmapped -> 404
			"",                          // malformed
		},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("VerifyCoverage: %d body=%s", rr.Code, rr.Body.String())
	}
	var resp VerifyResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if len(resp.Will200) != 2 {
		t.Errorf("Will200: want 2, got %d (%v)", len(resp.Will200), resp.Will200)
	}
	if len(resp.Will301) != 2 {
		t.Errorf("Will301: want 2, got %d (%v)", len(resp.Will301), resp.Will301)
	}
	if len(resp.Will404) != 1 {
		t.Errorf("Will404: want 1, got %d (%v)", len(resp.Will404), resp.Will404)
	}
	if len(resp.Malformed) != 1 {
		t.Errorf("Malformed: want 1, got %d", len(resp.Malformed))
	}
}

func TestMigration_VerifyCoverageRejectsEmpty(t *testing.T) {
	h, q, siteID := newMigrationHandlerForTest(t)
	migID := "test-mig-vc-empty"
	_ = q.CreateMigration(context.Background(), store.CreateMigrationParams{
		ID: migID, SiteID: siteID, SourceUrl: "x", SourceType: "sitemap",
		ManifestJson: "{}", Status: "ready",
	})
	rr := postJSONMigr(withMigrationParams(siteID, migID, h.VerifyCoverage), map[string]any{
		"urls": []string{},
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("empty urls must 400, got %d", rr.Code)
	}
}

func TestMigration_VerifyCoverageCrossTenantBlocked(t *testing.T) {
	h, q, siteA := newMigrationHandlerForTest(t)
	siteB := "abcdef0123456789abcdefvc"
	_ = q.CreateSite(context.Background(), store.CreateSiteParams{
		ID: siteB, Name: "B", Slug: "b-vc", PrimaryColor: "#000",
		SecondaryColor: "#000", BgColor: "#FFF", TextColor: "#111",
		FontHeading: "Inter", FontBody: "Inter", Lang: "en",
	})
	migID := "test-mig-vc-tenA"
	_ = q.CreateMigration(context.Background(), store.CreateMigrationParams{
		ID: migID, SiteID: siteA, SourceUrl: "x", SourceType: "sitemap",
		ManifestJson: "{}", Status: "ready",
	})
	rr := postJSONMigr(withMigrationParams(siteB, migID, h.VerifyCoverage), map[string]any{
		"urls": []string{"/x"},
	})
	if rr.Code != http.StatusNotFound {
		t.Errorf("cross-tenant must 404, got %d", rr.Code)
	}
}

// ----- Sprint 2 + Sprint 4: VerifyLive (async post-launch crawl) -----
//
// Sprint 4 made VerifyLive async: returns 202 with a job_id, the worker
// drives the crawl + writes per-URL rows + flushes verify_jobs counters.
// Tests poll the job row until terminal before asserting.

// pollVerifyJobUntilDone polls GetVerifyJob until a terminal state with
// a soft deadline. A unit test on a 3-URL crawl typically settles in a
// few hundred milliseconds; a generous 5s ceiling absorbs CI jitter
// without risking false stalls.
func pollVerifyJobUntilDone(t *testing.T, h *MigrationHandler, siteID, migID, jobID string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("siteID", siteID)
		rctx.URLParams.Add("migrationID", migID)
		rctx.URLParams.Add("jobID", jobID)
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		req = seedAuthCtx(req)
		rr := httptest.NewRecorder()
		h.GetVerifyJob(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GetVerifyJob: %d body=%s", rr.Code, rr.Body.String())
		}
		var out map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		switch out["status"] {
		case "done", "failed", "cancelled":
			return out
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("verify job never reached terminal state")
	return nil
}

func TestMigration_VerifyLiveAsyncStoresResults(t *testing.T) {
	h, q, siteID := newMigrationHandlerForTest(t)
	// Synthetic deployed atomicsite: /old 301 -> /new (200), /broken 404.
	mux := http.NewServeMux()
	mux.HandleFunc("/old", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/new", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/new", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/broken", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	migID := "test-mig-vl"
	_ = q.CreateMigration(context.Background(), store.CreateMigrationParams{
		ID: migID, SiteID: siteID, SourceUrl: "x", SourceType: "sitemap",
		ManifestJson: "{}", Status: "applied",
	})

	// Test seam: allow loopback so httptest.Server is reachable. The
	// production code path (server.go) never sets AllowPrivate=true.
	h.SetVerifyOptionsForTest(migration.VerifyOptions{
		AllowPrivate: true, PolitenessDelay: 1 * time.Millisecond,
	})

	rr := postJSONMigr(withMigrationParams(siteID, migID, h.VerifyLive), map[string]any{
		"urls":            []string{"/old", "/new", "/broken"},
		"deployed_domain": srv.URL,
	})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("VerifyLive: want 202, got %d body=%s", rr.Code, rr.Body.String())
	}
	var enq struct {
		JobID     string `json:"job_id"`
		Status    string `json:"status"`
		TotalURLs int    `json:"total_urls"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &enq)
	if enq.JobID == "" || enq.Status != "queued" || enq.TotalURLs != 3 {
		t.Fatalf("enqueue response unexpected: %+v body=%s", enq, rr.Body.String())
	}

	final := pollVerifyJobUntilDone(t, h, siteID, migID, enq.JobID)
	if final["status"] != "done" {
		t.Errorf("terminal status: want done got %v err=%v", final["status"], final["error"])
	}
	if int(final["processed_urls"].(float64)) != 3 ||
		int(final["ok_count"].(float64)) != 2 ||
		int(final["fail_count"].(float64)) != 1 {
		t.Errorf("counts: %+v", final)
	}

	stored, _ := q.ListMigrationVerifications(context.Background(), migID)
	if len(stored) != 3 {
		t.Errorf("stored verifications: want 3, got %d", len(stored))
	}
}

func TestMigration_VerifyLiveRequiresDeployedDomain(t *testing.T) {
	h, q, siteID := newMigrationHandlerForTest(t)
	migID := "test-mig-vl-nodomain"
	_ = q.CreateMigration(context.Background(), store.CreateMigrationParams{
		ID: migID, SiteID: siteID, SourceUrl: "x", SourceType: "sitemap",
		ManifestJson: "{}", Status: "applied",
	})
	rr := postJSONMigr(withMigrationParams(siteID, migID, h.VerifyLive), map[string]any{
		"urls": []string{"/x"},
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("missing deployed_domain must 400, got %d", rr.Code)
	}
}

func TestMigration_VerifyLiveCapsAt25kURLs(t *testing.T) {
	h, q, siteID := newMigrationHandlerForTest(t)
	migID := "test-mig-vl-cap"
	_ = q.CreateMigration(context.Background(), store.CreateMigrationParams{
		ID: migID, SiteID: siteID, SourceUrl: "x", SourceType: "sitemap",
		ManifestJson: "{}", Status: "applied",
	})
	urls := make([]string, 25001) // verifyLiveMaxURLs is 25000
	for i := range urls {
		urls[i] = "/p" + strings.Repeat("0", 6)
	}
	rr := postJSONMigr(withMigrationParams(siteID, migID, h.VerifyLive), map[string]any{
		"urls":            urls,
		"deployed_domain": "deploy.example",
	})
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("over-cap must 413, got %d", rr.Code)
	}
}

func TestMigration_VerifyLiveAnonymousReturns401(t *testing.T) {
	h, q, siteID := newMigrationHandlerForTest(t)
	migID := "test-mig-vl-anon"
	_ = q.CreateMigration(context.Background(), store.CreateMigrationParams{
		ID: migID, SiteID: siteID, SourceUrl: "x", SourceType: "sitemap",
		ManifestJson: "{}", Status: "applied",
	})
	// Bypass withMigrationParams (which injects auth context) so this
	// hits the anonymous path. Mirrors the Apply 401 test contract.
	body, _ := json.Marshal(map[string]any{
		"urls":            []string{"/x"},
		"deployed_domain": "deploy.example",
	})
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("siteID", siteID)
	rctx.URLParams.Add("migrationID", migID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()
	h.VerifyLive(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("anonymous verify-live must 401, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestMigration_GetVerifyJobCrossTenantBlocked(t *testing.T) {
	h, q, siteID := newMigrationHandlerForTest(t)
	migID := "test-mig-vj-tenant"
	_ = q.CreateMigration(context.Background(), store.CreateMigrationParams{
		ID: migID, SiteID: siteID, SourceUrl: "x", SourceType: "sitemap",
		ManifestJson: "{}", Status: "applied",
	})
	// Seed a job manually so the test doesn't need a worker.
	jobID := "test-job-tenant"
	_ = q.CreateVerifyJob(context.Background(), store.CreateVerifyJobParams{
		ID: jobID, SiteID: siteID, MigrationID: migID, TotalUrls: 5,
		DeployedDomain: "deploy.example",
	})

	// Wrong site_id in URL -> 404.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("siteID", "abcdef0123456789abcdefxx")
	rctx.URLParams.Add("migrationID", migID)
	rctx.URLParams.Add("jobID", jobID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	req = seedAuthCtx(req)
	rr := httptest.NewRecorder()
	h.GetVerifyJob(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("cross-tenant GetVerifyJob must 404, got %d", rr.Code)
	}
}

func TestMigration_CancelVerifyJobOnTerminalReturns409(t *testing.T) {
	h, q, siteID := newMigrationHandlerForTest(t)
	migID := "test-mig-vj-409"
	_ = q.CreateMigration(context.Background(), store.CreateMigrationParams{
		ID: migID, SiteID: siteID, SourceUrl: "x", SourceType: "sitemap",
		ManifestJson: "{}", Status: "applied",
	})
	jobID := "test-job-done"
	_ = q.CreateVerifyJob(context.Background(), store.CreateVerifyJobParams{
		ID: jobID, SiteID: siteID, MigrationID: migID, TotalUrls: 1,
		DeployedDomain: "deploy.example",
	})
	_ = q.FinishVerifyJob(context.Background(), store.FinishVerifyJobParams{
		ID: jobID, Status: "done", ProcessedUrls: 1, OkCount: 1, FailCount: 0,
	})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("siteID", siteID)
	rctx.URLParams.Add("migrationID", migID)
	rctx.URLParams.Add("jobID", jobID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	req = seedAuthCtx(req)
	rr := httptest.NewRecorder()
	h.CancelVerifyJob(rr, req)
	if rr.Code != http.StatusConflict {
		t.Errorf("cancel on done job must 409, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestMigration_ListVerifyJobsOrdersByCreatedAtDesc(t *testing.T) {
	h, q, siteID := newMigrationHandlerForTest(t)
	migID := "test-mig-listvj"
	_ = q.CreateMigration(context.Background(), store.CreateMigrationParams{
		ID: migID, SiteID: siteID, SourceUrl: "x", SourceType: "sitemap",
		ManifestJson: "{}", Status: "applied",
	})
	// Seed 2 jobs with deterministic IDs; the second is created later
	// so it should sort first under created_at DESC. SQLite's
	// datetime('now') resolution is 1s so the sleep is sufficient.
	_ = q.CreateVerifyJob(context.Background(), store.CreateVerifyJobParams{
		ID: "job-old", SiteID: siteID, MigrationID: migID, TotalUrls: 10,
		DeployedDomain: "deploy.example",
	})
	time.Sleep(1100 * time.Millisecond)
	_ = q.CreateVerifyJob(context.Background(), store.CreateVerifyJobParams{
		ID: "job-new", SiteID: siteID, MigrationID: migID, TotalUrls: 20,
		DeployedDomain: "deploy.example",
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	withMigrationParams(siteID, migID, h.ListVerifyJobs)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("ListVerifyJobs: %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if len(resp.Items) != 2 {
		t.Fatalf("items: want 2, got %d", len(resp.Items))
	}
	if resp.Items[0]["id"] != "job-new" || resp.Items[1]["id"] != "job-old" {
		t.Errorf("order: %v %v", resp.Items[0]["id"], resp.Items[1]["id"])
	}
}

// ----- Sprint 2: ListVerifications -----

func TestMigration_ListVerificationsReturnsStoredRows(t *testing.T) {
	h, q, siteID := newMigrationHandlerForTest(t)
	migID := "test-mig-listv"
	_ = q.CreateMigration(context.Background(), store.CreateMigrationParams{
		ID: migID, SiteID: siteID, SourceUrl: "x", SourceType: "sitemap",
		ManifestJson: "{}", Status: "applied",
	})
	// Seed two stored verifications directly.
	_ = q.CreateMigrationVerification(context.Background(), store.CreateMigrationVerificationParams{
		ID: "v1", SiteID: siteID, MigrationID: migID,
		SourceUrl: "https://old/a", StatusCode: 200, FinalUrl: "https://new/a",
		Hops: 0, Ok: 1, Error: "",
	})
	_ = q.CreateMigrationVerification(context.Background(), store.CreateMigrationVerificationParams{
		ID: "v2", SiteID: siteID, MigrationID: migID,
		SourceUrl: "https://old/b", StatusCode: 404, FinalUrl: "https://new/b",
		Hops: 1, Ok: 0, Error: "terminal status 404",
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	withMigrationParams(siteID, migID, h.ListVerifications)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("ListVerifications: %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Items  []map[string]any `json:"items"`
		OK     int64            `json:"ok"`
		Failed int64            `json:"failed"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if len(resp.Items) != 2 {
		t.Errorf("items: %d", len(resp.Items))
	}
	if resp.OK != 1 || resp.Failed != 1 {
		t.Errorf("counts: ok=%d fail=%d", resp.OK, resp.Failed)
	}
}

// ----- Sprint 3: workspace plan-quota gate on Apply -----

// newPaidPlanMigrationStack returns a migration handler whose seeded
// workspace is on a specific plan. Used by the quota tests.
func newPaidPlanMigrationStack(t *testing.T, plan string) (*MigrationHandler, *store.Queries, string) {
	t.Helper()
	sqlDB, _ := newSchemaDB(t, "migr-quota.db")
	q := store.New(sqlDB)
	siteID := "abcdef0123456789abcdefqq"
	_ = q.CreateSite(context.Background(), store.CreateSiteParams{
		ID: siteID, Name: "T", Slug: "t-migr-q", PrimaryColor: "#000",
		SecondaryColor: "#000", BgColor: "#FFF", TextColor: "#111",
		FontHeading: "Inter", FontBody: "Inter", Lang: "en",
	})
	_ = q.CreateWorkspace(context.Background(), store.CreateWorkspaceParams{
		ID: "ws-quota", Name: "Quota Test", Slug: "quota-test-ws",
		Plan: plan, Region: "eu", BillingEmail: "ops@test.local",
		Status: "active", TrialEndsAt: "",
	})
	cfg := &config.Config{BaseURL: "http://localhost:8080"}
	return NewMigrationHandler(cfg, q, &fakeUploaderForHandlers{}), q, siteID
}

// makeManifestWithNPages helper - generates a manifest the URL planner
// turns into N new pages (no skips, no collisions).
func makeManifestWithNPages(n int) migration.MigrationManifest {
	pages := make([]migration.MigrationPage, n)
	for i := 0; i < n; i++ {
		pages[i] = migration.MigrationPage{
			SourcePath: fmt.Sprintf("/quota-page-%d", i),
			Title:      fmt.Sprintf("Page %d", i),
		}
	}
	return migration.MigrationManifest{Pages: pages}
}

// TestMigration_ApplyBlockedBySoloPlanCap verifies that a 51-page
// manifest applied against a Solo workspace (50-page cap) returns 402
// without inserting any pages and leaves the migration row at status=ready.
func TestMigration_ApplyBlockedBySoloPlanCap(t *testing.T) {
	h, q, siteID := newPaidPlanMigrationStack(t, "solo")
	manifest := makeManifestWithNPages(51) // exceeds solo cap of 50
	mj, _ := json.Marshal(manifest)
	migID := "test-mig-quota-block"
	_ = q.CreateMigration(context.Background(), store.CreateMigrationParams{
		ID: migID, SiteID: siteID, SourceUrl: "x", SourceType: "sitemap",
		ManifestJson: string(mj), Status: "ready",
	})

	rr := postJSONMigr(withMigrationParams(siteID, migID, h.Apply), map[string]any{})
	if rr.Code != http.StatusPaymentRequired {
		t.Fatalf("want 402, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "max_pages_per_site") &&
		!strings.Contains(rr.Body.String(), "plan cap") {
		t.Errorf("error should mention plan cap or dimension: %s", rr.Body.String())
	}

	// Migration row must stay at "ready" so the operator can adjust skips
	// and retry without re-crawling.
	row, _ := q.GetMigrationByID(context.Background(), migID)
	if row.Status != "ready" {
		t.Errorf("status flipped on quota block (must stay ready), got %q", row.Status)
	}
	// No pages should have been inserted.
	pages, _ := q.ListPagesBySite(context.Background(), siteID)
	if len(pages) != 0 {
		t.Errorf("quota-blocked apply must insert zero pages, got %d", len(pages))
	}
}

// TestMigration_ApplyAllowedAtSoloCap verifies the boundary condition:
// exactly 50 pages on a Solo plan succeeds.
func TestMigration_ApplyAllowedAtSoloCap(t *testing.T) {
	h, q, siteID := newPaidPlanMigrationStack(t, "solo")
	manifest := makeManifestWithNPages(50) // exactly at cap
	mj, _ := json.Marshal(manifest)
	migID := "test-mig-quota-edge"
	_ = q.CreateMigration(context.Background(), store.CreateMigrationParams{
		ID: migID, SiteID: siteID, SourceUrl: "x", SourceType: "sitemap",
		ManifestJson: string(mj), Status: "ready",
	})
	rr := postJSONMigr(withMigrationParams(siteID, migID, h.Apply), map[string]any{})
	if rr.Code != http.StatusOK {
		t.Fatalf("at-cap apply must succeed, got %d body=%s", rr.Code, rr.Body.String())
	}
	pages, _ := q.ListPagesBySite(context.Background(), siteID)
	if len(pages) != 50 {
		t.Errorf("want 50 pages, got %d", len(pages))
	}
}

// TestMigration_ApplyAllowedOnOSSPlan verifies that the OSS plan (-1
// unlimited) lets a large import through without complaint.
func TestMigration_ApplyAllowedOnOSSPlan(t *testing.T) {
	h, q, siteID := newPaidPlanMigrationStack(t, "oss")
	manifest := makeManifestWithNPages(120) // far above any paid cap
	mj, _ := json.Marshal(manifest)
	migID := "test-mig-oss-unlimited"
	_ = q.CreateMigration(context.Background(), store.CreateMigrationParams{
		ID: migID, SiteID: siteID, SourceUrl: "x", SourceType: "sitemap",
		ManifestJson: string(mj), Status: "ready",
	})
	rr := postJSONMigr(withMigrationParams(siteID, migID, h.Apply), map[string]any{})
	if rr.Code != http.StatusOK {
		t.Errorf("OSS plan must allow unlimited; got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestMigration_ApplyAccountsForExistingPages confirms that the gate adds
// current site pages to the projected count - if the site already has
// 49 pages on a Solo plan and the manifest adds 2 more, the second one
// pushes past 50 and the apply must be refused.
func TestMigration_ApplyAccountsForExistingPages(t *testing.T) {
	h, q, siteID := newPaidPlanMigrationStack(t, "solo")
	// Pre-seed 49 pages directly.
	for i := 0; i < 49; i++ {
		_ = q.CreatePage(context.Background(), store.CreatePageParams{
			ID: fmt.Sprintf("pre-%03d", i), SiteID: siteID,
			Title: fmt.Sprintf("Pre %d", i), Slug: fmt.Sprintf("pre-%d", i),
			Layout: "default",
		})
	}
	manifest := makeManifestWithNPages(2) // 49 + 2 = 51 > 50
	mj, _ := json.Marshal(manifest)
	migID := "test-mig-quota-existing"
	_ = q.CreateMigration(context.Background(), store.CreateMigrationParams{
		ID: migID, SiteID: siteID, SourceUrl: "x", SourceType: "sitemap",
		ManifestJson: string(mj), Status: "ready",
	})
	rr := postJSONMigr(withMigrationParams(siteID, migID, h.Apply), map[string]any{})
	if rr.Code != http.StatusPaymentRequired {
		t.Errorf("want 402 (existing+projected over cap), got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestMigration_ApplyRequiresAuth proves the gate refuses anonymous
// callers (matches the existing SiteHandler.Seed contract).
func TestMigration_ApplyRequiresAuth(t *testing.T) {
	h, q, siteID := newPaidPlanMigrationStack(t, "solo")
	manifest := makeManifestWithNPages(1)
	mj, _ := json.Marshal(manifest)
	migID := "test-mig-anon"
	_ = q.CreateMigration(context.Background(), store.CreateMigrationParams{
		ID: migID, SiteID: siteID, SourceUrl: "x", SourceType: "sitemap",
		ManifestJson: string(mj), Status: "ready",
	})
	// Bypass the seedAuthCtx-injecting helper to send a truly unauth'd
	// request straight at h.Apply.
	body, _ := json.Marshal(map[string]any{})
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("siteID", siteID)
	rctx.URLParams.Add("migrationID", migID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()
	h.Apply(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("anonymous Apply must 401, got %d", rr.Code)
	}
}

// ----- NoopMediaUploader returns error so porter records warning -----

func TestNoopMediaUploader_ErrorsCleanly(t *testing.T) {
	u := NoopMediaUploader()
	_, _, err := u.UploadFromURL(context.Background(), "site", "https://x/y.jpg", "", "")
	if err == nil {
		t.Errorf("noop uploader must error so porter records warning")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("error message should be clear: %v", err)
	}
}

// ----- Sprint 4: re-import upsert mode -----

// TestMigration_ApplyUpsertOnFreshSite asserts that upsert=true on a
// site with zero existing pages behaves identically to upsert=false:
// every manifest page becomes a created row, none are updates.
func TestMigration_ApplyUpsertOnFreshSite(t *testing.T) {
	h, q, siteID := newMigrationHandlerForTest(t)
	manifest := makeManifestWithNPages(5)
	mj, _ := json.Marshal(manifest)
	migID := "mig-up-fresh"
	_ = q.CreateMigration(context.Background(), store.CreateMigrationParams{
		ID: migID, SiteID: siteID, SourceUrl: "x", SourceType: "sitemap",
		ManifestJson: string(mj), Status: "ready",
	})
	rr := postJSONMigr(withMigrationParams(siteID, migID, h.Apply), map[string]any{
		"upsert": true,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("Apply upsert: %d body=%s", rr.Code, rr.Body.String())
	}
	var out migration.ApplyResult
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out.PagesCreated != 5 || out.PagesUpdated != 0 {
		t.Errorf("fresh upsert: created=%d updated=%d (want 5/0)", out.PagesCreated, out.PagesUpdated)
	}
	pages, _ := q.ListPagesBySite(context.Background(), siteID)
	if len(pages) != 5 {
		t.Errorf("rows in DB: %d", len(pages))
	}
}

// TestMigration_ApplyUpsertOverwritesOverlap asserts that re-applying a
// manifest with upsert=true on a site that already has 3 of the 5
// candidate slugs results in 2 created + 3 updated rows, with the
// existing rows' content visibly replaced.
func TestMigration_ApplyUpsertOverwritesOverlap(t *testing.T) {
	h, q, siteID := newMigrationHandlerForTest(t)
	// Pre-seed 3 pages with stale content under the same slugs the
	// manifest will hit (quota-page-0, quota-page-1, quota-page-2).
	preExisting := []string{"quota-page-0", "quota-page-1", "quota-page-2"}
	for _, slug := range preExisting {
		_ = q.CreatePage(context.Background(), store.CreatePageParams{
			ID: "p-" + slug, SiteID: siteID, Title: "STALE " + slug, Slug: slug,
			Layout: "default", SortOrder: 0, ShowInNav: 0,
		})
	}

	manifest := makeManifestWithNPages(5) // emits slugs quota-page-0..4
	mj, _ := json.Marshal(manifest)
	migID := "mig-up-overlap"
	_ = q.CreateMigration(context.Background(), store.CreateMigrationParams{
		ID: migID, SiteID: siteID, SourceUrl: "x", SourceType: "sitemap",
		ManifestJson: string(mj), Status: "ready",
	})
	rr := postJSONMigr(withMigrationParams(siteID, migID, h.Apply), map[string]any{
		"upsert": true,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("Apply upsert: %d body=%s", rr.Code, rr.Body.String())
	}
	var out migration.ApplyResult
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out.PagesCreated != 2 || out.PagesUpdated != 3 {
		t.Errorf("overlap counts: created=%d updated=%d (want 2/3)",
			out.PagesCreated, out.PagesUpdated)
	}
	// Content overwritten: title used to be "STALE quota-page-0" and is
	// now "Page 0" per makeManifestWithNPages.
	overwritten, err := q.GetPageBySiteAndSlug(context.Background(), store.GetPageBySiteAndSlugParams{
		SiteID: siteID, Slug: "quota-page-0",
	})
	if err != nil {
		t.Fatalf("re-fetch overwritten page: %v", err)
	}
	if strings.HasPrefix(overwritten.Title, "STALE") {
		t.Errorf("upsert did not overwrite title: %q", overwritten.Title)
	}
	// Total row count: 5, not 8 (no duplication).
	pages, _ := q.ListPagesBySite(context.Background(), siteID)
	if len(pages) != 5 {
		t.Errorf("row count post-upsert: want 5, got %d", len(pages))
	}
}

// TestMigration_ApplyCreateModeStillConflicts asserts that the default
// (Upsert=false) behaviour still rejects a re-import: the porter calls
// CreatePage, the unique index fires, the porter rolls back, and the
// migration row flips to "failed". This locks the existing contract so
// upsert is purely opt-in.
func TestMigration_ApplyCreateModeStillConflicts(t *testing.T) {
	h, q, siteID := newMigrationHandlerForTest(t)
	_ = q.CreatePage(context.Background(), store.CreatePageParams{
		ID: "p-clash", SiteID: siteID, Title: "Existing", Slug: "quota-page-0",
		Layout: "default", SortOrder: 0, ShowInNav: 0,
	})
	manifest := makeManifestWithNPages(2) // emits quota-page-0 & 1
	mj, _ := json.Marshal(manifest)
	migID := "mig-up-conflict"
	_ = q.CreateMigration(context.Background(), store.CreateMigrationParams{
		ID: migID, SiteID: siteID, SourceUrl: "x", SourceType: "sitemap",
		ManifestJson: string(mj), Status: "ready",
	})
	rr := postJSONMigr(withMigrationParams(siteID, migID, h.Apply), map[string]any{})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create-mode collision must 422, got %d body=%s", rr.Code, rr.Body.String())
	}
	row, _ := q.GetMigrationByID(context.Background(), migID)
	if row.Status != "failed" {
		t.Errorf("status after collision: %q (want failed)", row.Status)
	}
}

// TestMigration_ApplyUpsertQuotaSubtractsExisting confirms the Sprint 3
// plan-quota gate carries through: 49 existing pages, manifest with 50
// pages where 40 overlap, projected addition is 10 -> 49 + 10 = 59 over
// the solo cap of 50 -> 402 blocked.
func TestMigration_ApplyUpsertQuotaSubtractsExisting(t *testing.T) {
	h, q, siteID := newPaidPlanMigrationStack(t, "solo")
	// Pre-seed 49 pages, 40 of which overlap the manifest's quota-page-0..49.
	for i := 0; i < 49; i++ {
		_ = q.CreatePage(context.Background(), store.CreatePageParams{
			ID:     fmt.Sprintf("p-old-%02d", i),
			SiteID: siteID,
			Title:  fmt.Sprintf("Old %d", i),
			Slug:   fmt.Sprintf("quota-page-%d", i),
			Layout: "default", SortOrder: 0, ShowInNav: 0,
		})
	}

	manifest := makeManifestWithNPages(50) // slugs quota-page-0..49
	mj, _ := json.Marshal(manifest)
	migID := "mig-up-quota-block"
	_ = q.CreateMigration(context.Background(), store.CreateMigrationParams{
		ID: migID, SiteID: siteID, SourceUrl: "x", SourceType: "sitemap",
		ManifestJson: string(mj), Status: "ready",
	})
	// 40 overlap (slugs 0..39 already exist), 10 net new -> 49 + 10 = 59 > 50.
	// Wait: pre-seeded 49 (slugs 0..48), so overlap = 49, projected = 1 -> 49 + 1 = 50, allowed.
	// Adjust: pre-seed slugs 1..49 (skipping 0), so overlap = 49, projected = 1 -> 50 cap, allowed.
	// To force a block we need projected > limit - current. Current = 49,
	// limit = 50, so projected must be >= 2 to fail. Manifest has slugs
	// 0..49 (50 total). Pre-seed overlap of 48 leaves 2 net new -> 49 + 2 = 51 > 50.
	// Re-seeding: keep 49 pre-seeded but with 48 overlapping slugs.
	// (slugs 0..47 + a non-overlapping slug). Already seeded slugs 0..48
	// above which overlaps 49 of manifest. So projected = 1, total = 50, allowed.
	// Recreate the 49th seed with a non-overlapping slug instead.
	_ = q.DeletePage(context.Background(), "p-old-48")
	_ = q.CreatePage(context.Background(), store.CreatePageParams{
		ID: "p-other", SiteID: siteID, Title: "Other", Slug: "other-not-in-manifest",
		Layout: "default", SortOrder: 0, ShowInNav: 0,
	})

	rr := postJSONMigr(withMigrationParams(siteID, migID, h.Apply), map[string]any{
		"upsert": true,
	})
	if rr.Code != http.StatusPaymentRequired {
		t.Fatalf("upsert quota block: want 402, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestMigration_ApplyUpsertQuotaFullOverlapAllowed: 50 existing pages
// at the solo cap, manifest with 50 fully-overlapping slugs -> projected
// addition is 0, allowed. The annual-content-refresh case.
func TestMigration_ApplyUpsertQuotaFullOverlapAllowed(t *testing.T) {
	h, q, siteID := newPaidPlanMigrationStack(t, "solo")
	for i := 0; i < 50; i++ {
		_ = q.CreatePage(context.Background(), store.CreatePageParams{
			ID:     fmt.Sprintf("p-pre-%02d", i),
			SiteID: siteID,
			Title:  fmt.Sprintf("Pre %d", i),
			Slug:   fmt.Sprintf("quota-page-%d", i),
			Layout: "default", SortOrder: 0, ShowInNav: 0,
		})
	}
	manifest := makeManifestWithNPages(50)
	mj, _ := json.Marshal(manifest)
	migID := "mig-up-quota-allow"
	_ = q.CreateMigration(context.Background(), store.CreateMigrationParams{
		ID: migID, SiteID: siteID, SourceUrl: "x", SourceType: "sitemap",
		ManifestJson: string(mj), Status: "ready",
	})
	rr := postJSONMigr(withMigrationParams(siteID, migID, h.Apply), map[string]any{
		"upsert": true,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("at-cap full-overlap upsert must succeed, got %d body=%s", rr.Code, rr.Body.String())
	}
	var out migration.ApplyResult
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out.PagesUpdated != 50 || out.PagesCreated != 0 {
		t.Errorf("full overlap counts: created=%d updated=%d (want 0/50)",
			out.PagesCreated, out.PagesUpdated)
	}
	// Row count stays at 50 - no duplication.
	pages, _ := q.ListPagesBySite(context.Background(), siteID)
	if len(pages) != 50 {
		t.Errorf("row count after refresh: %d (want 50)", len(pages))
	}
}

// TestMigration_ApplyUpsertEmitsDistinctAuditAction asserts upsert mode
// writes audit_log with action="migration_apply_upsert" so BI can tell
// refresh runs apart from initial imports.
func TestMigration_ApplyUpsertEmitsDistinctAuditAction(t *testing.T) {
	h, q, siteID := newMigrationHandlerForTest(t)
	manifest := makeManifestWithNPages(2)
	mj, _ := json.Marshal(manifest)
	migID := "mig-up-audit"
	_ = q.CreateMigration(context.Background(), store.CreateMigrationParams{
		ID: migID, SiteID: siteID, SourceUrl: "x", SourceType: "sitemap",
		ManifestJson: string(mj), Status: "ready",
	})
	rr := postJSONMigr(withMigrationParams(siteID, migID, h.Apply), map[string]any{
		"upsert": true,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("Apply: %d", rr.Code)
	}
	rows, _ := q.ListAuditLogBySite(context.Background(), store.ListAuditLogBySiteParams{
		SiteID: siteID, Limit: 50,
	})
	// audit_log convention in this codebase puts the action label in
	// the resource_type column; the action column carries the verb
	// ("update"). See the existing migration_apply_blocked emission
	// pattern in checkPagesQuotaForApply.
	found := false
	for _, e := range rows {
		if e.ResourceType == "migration_apply_upsert" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("audit_log missing migration_apply_upsert entry")
	}
}

// ----- Slice B: low-level upsert query roundtrips -----

func TestUpsertPage_InsertThenUpdate(t *testing.T) {
	_, q, siteID := newMigrationHandlerForTest(t)
	first, err := q.UpsertPage(context.Background(), store.UpsertPageParams{
		ID: "page-1", SiteID: siteID, Title: "v1", Slug: "post",
		Status: "published", MetaTitle: "v1", MetaDescription: "",
		OgImageID: "", Layout: "default", SortOrder: 0, ShowInNav: 0,
		NoIndex: 0, CanonicalUrl: "",
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if first.CreatedAt != first.UpdatedAt {
		t.Errorf("first upsert: created_at != updated_at indicates an UPDATE")
	}
	// SQLite datetime('now') has 1s resolution; sleep so updated_at
	// can deviate from created_at.
	time.Sleep(1100 * time.Millisecond)
	second, err := q.UpsertPage(context.Background(), store.UpsertPageParams{
		ID: "page-DIFFERENT", SiteID: siteID, Title: "v2", Slug: "post",
		Status: "published", MetaTitle: "v2", MetaDescription: "x",
		OgImageID: "", Layout: "default", SortOrder: 0, ShowInNav: 0,
		NoIndex: 0, CanonicalUrl: "",
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("second upsert returned different id: %q (want %q)", second.ID, first.ID)
	}
	if second.Title != "v2" {
		t.Errorf("title not overwritten: %q", second.Title)
	}
	if second.CreatedAt == second.UpdatedAt {
		t.Errorf("updated row should have updated_at > created_at: created=%q updated=%q",
			second.CreatedAt, second.UpdatedAt)
	}
}

func TestUpsertRedirect_InsertThenUpdate(t *testing.T) {
	_, q, siteID := newMigrationHandlerForTest(t)
	first, err := q.UpsertRedirect(context.Background(), store.UpsertRedirectParams{
		ID: "r-1", SiteID: siteID, FromPath: "/old", ToPath: "/new", StatusCode: 301, IsAuto: 1,
	})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := q.UpsertRedirect(context.Background(), store.UpsertRedirectParams{
		ID: "r-DIFFERENT", SiteID: siteID, FromPath: "/old", ToPath: "/even-newer", StatusCode: 302, IsAuto: 0,
	})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("second upsert returned different id: %q (want %q)", second.ID, first.ID)
	}
	if second.ToPath != "/even-newer" || second.StatusCode != 302 {
		t.Errorf("upsert did not overwrite: to=%q status=%d", second.ToPath, second.StatusCode)
	}
	all, _ := q.ListRedirectsBySite(context.Background(), siteID)
	if len(all) != 1 {
		t.Errorf("upsert duplicated row: %d", len(all))
	}
}

func TestUpsertItem_InsertThenUpdate(t *testing.T) {
	_, q, siteID := newMigrationHandlerForTest(t)
	collID := "coll-up"
	_ = q.CreateCollection(context.Background(), store.CreateCollectionParams{
		ID: collID, SiteID: siteID, Name: "Posts", Slug: "posts",
		SchemaJson: "[]", SettingsJson: "{}", SortOrder: 0,
	})
	first, err := q.UpsertItem(context.Background(), store.UpsertItemParams{
		ID: "i-1", CollectionID: collID, SiteID: siteID,
		Slug: "hello", Title: "Hello v1", DataJson: `{"x":1}`,
		Locale: "", Status: "published", PublishedAt: "", SortOrder: 0,
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	second, err := q.UpsertItem(context.Background(), store.UpsertItemParams{
		ID: "i-DIFFERENT", CollectionID: collID, SiteID: siteID,
		Slug: "hello", Title: "Hello v2", DataJson: `{"x":2}`,
		Locale: "", Status: "published", PublishedAt: "", SortOrder: 0,
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("upsert produced new row, want stable id")
	}
	if second.Title != "Hello v2" || second.DataJson != `{"x":2}` {
		t.Errorf("data not overwritten: title=%q data=%q", second.Title, second.DataJson)
	}
	if second.CreatedAt == second.UpdatedAt {
		t.Errorf("updated_at not advanced")
	}
	count, _ := q.CountItemsByCollection(context.Background(), collID)
	if count != 1 {
		t.Errorf("row count post-upsert: %d", count)
	}
}

// TestGetPageBySiteAndSlug_ToleratesBothSlugForms locks in the dual-form
// slug lookup. The migration porter stores slugs without a leading slash
// (e.g. "index", "privacy") while the MCP layer normalizes slugs to a
// leading-slash form before lookup ("/index", "/privacy"). Without the
// LTRIM fallback in the SELECT, every porter-created page is unreachable
// through the MCP and agent surfaces despite existing in the DB.
func TestGetPageBySiteAndSlug_ToleratesBothSlugForms(t *testing.T) {
	_, q, siteID := newMigrationHandlerForTest(t)

	if err := q.CreatePage(context.Background(), store.CreatePageParams{
		ID:        "page-noslash-0001",
		SiteID:    siteID,
		Title:     "Privacy",
		Slug:      "privacy",
		Layout:    "default",
		SortOrder: 0,
		ShowInNav: 0,
	}); err != nil {
		t.Fatalf("seed no-slash page: %v", err)
	}

	for _, lookup := range []string{"privacy", "/privacy"} {
		got, err := q.GetPageBySiteAndSlug(context.Background(), store.GetPageBySiteAndSlugParams{
			SiteID: siteID, Slug: lookup,
		})
		if err != nil {
			t.Fatalf("lookup with slug %q: %v", lookup, err)
		}
		if got.ID != "page-noslash-0001" {
			t.Errorf("lookup with slug %q: got id=%q want page-noslash-0001", lookup, got.ID)
		}
	}

	if err := q.CreatePage(context.Background(), store.CreatePageParams{
		ID:        "page-slash-0001",
		SiteID:    siteID,
		Title:     "EN Privacy",
		Slug:      "/en/privacy",
		Layout:    "default",
		SortOrder: 1,
		ShowInNav: 0,
	}); err != nil {
		t.Fatalf("seed slash page: %v", err)
	}

	got, err := q.GetPageBySiteAndSlug(context.Background(), store.GetPageBySiteAndSlugParams{
		SiteID: siteID, Slug: "/en/privacy",
	})
	if err != nil {
		t.Fatalf("lookup canonical slash slug: %v", err)
	}
	if got.ID != "page-slash-0001" {
		t.Errorf("canonical lookup matched wrong row: %q", got.ID)
	}
}
