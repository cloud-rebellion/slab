package handlers

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/bright-interaction/slab/internal/config"
	dbpkg "github.com/bright-interaction/slab/internal/db"
	"github.com/bright-interaction/slab/internal/store"
	"github.com/go-chi/chi/v5"
)

// newExportTestStack builds a BuildHandler wired to a tempdir
// DataDir + a fresh SQLite DB. Returns helpers the test cases use
// to plant a dist tree and a deployment row.
func newExportTestStack(t *testing.T) (*BuildHandler, *sql.DB, *store.Queries, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "build.sqlite")
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
	cfg := &config.Config{DataDir: dir}
	h := NewBuildHandler(cfg, q, nil)
	return h, sqlDB, q, dir
}

// plantDist writes a few files under {dataDir}/workspaces/{siteID}/dist
// so ExportDistZip has something to walk.
func plantDist(t *testing.T, dataDir, siteID string, files map[string]string) {
	t.Helper()
	root := filepath.Join(dataDir, "workspaces", siteID, "dist")
	for rel, content := range files {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// seedSiteAndDeploy creates the site row + a deployment row in the
// given status. Returns the deployment ID.
func seedSiteAndDeploy(t *testing.T, sqlDB *sql.DB, q *store.Queries, siteID, slug, status string) string {
	t.Helper()
	if _, err := sqlDB.Exec(`INSERT INTO sites (id, name, slug) VALUES (?, ?, ?)`, siteID, "Test", slug); err != nil {
		t.Fatal(err)
	}
	depID := "dep_" + siteID
	if err := q.CreateDeployment(context.Background(), store.CreateDeploymentParams{
		ID: depID, SiteID: siteID, DeployTarget: "static", DeployConfig: "{}",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`UPDATE deployments SET status = ? WHERE id = ?`, status, depID); err != nil {
		t.Fatal(err)
	}
	return depID
}

// withChiSiteParam wraps a request with chi route params populated
// (siteID) so urlParam works.
func withChiSiteParam(r *http.Request, siteID string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("siteID", siteID)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

func TestExportDistZip_HappyPath(t *testing.T) {
	h, sqlDB, q, dataDir := newExportTestStack(t)
	siteID := "abcdef0123456789abcdef00"
	seedSiteAndDeploy(t, sqlDB, q, siteID, "demo", "succeeded")
	plantDist(t, dataDir, siteID, map[string]string{
		"index.html":        "<!doctype html><h1>Hi</h1>",
		"about/index.html":  "<!doctype html><h1>About</h1>",
		"_astro/styles.css": "body{margin:0}",
		"images/logo.png":   "fake-png-bytes",
	})
	_ = q

	r := httptest.NewRequest("GET", "/api/sites/"+siteID+"/dist.zip", nil)
	r = withChiSiteParam(r, siteID)
	w := httptest.NewRecorder()
	h.ExportDistZip(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "application/zip" {
		t.Errorf("content-type = %q, want application/zip", got)
	}
	if !strings.HasPrefix(w.Header().Get("Content-Disposition"), `attachment; filename="demo-`) {
		t.Errorf("content-disposition = %q, want attachment with demo- prefix", w.Header().Get("Content-Disposition"))
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatalf("zip parse: %v", err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(rc)
		_ = rc.Close()
		got[f.Name] = string(body)
	}
	for _, want := range []string{
		"index.html",
		"about/index.html",
		"_astro/styles.css",
		"images/logo.png",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("zip missing %q; got keys=%v", want, zipKeys(got))
		}
	}
	if got["index.html"] != "<!doctype html><h1>Hi</h1>" {
		t.Errorf("body roundtrip mismatch; got %q", got["index.html"])
	}
}

func TestExportDistZip_NoSuccessfulDeploymentReturns404(t *testing.T) {
	h, sqlDB, q, dataDir := newExportTestStack(t)
	siteID := "abcdef0123456789abcdef01"
	// Deployment exists but in 'failed' status.
	seedSiteAndDeploy(t, sqlDB, q, siteID, "broken", "failed")
	plantDist(t, dataDir, siteID, map[string]string{"index.html": "x"})

	r := httptest.NewRequest("GET", "/api/sites/"+siteID+"/dist.zip", nil)
	r = withChiSiteParam(r, siteID)
	w := httptest.NewRecorder()
	h.ExportDistZip(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no successful deployment)", w.Code)
	}
}

func TestExportDistZip_DistIsRegularFileReturns404(t *testing.T) {
	// If something has placed a regular file at the dist path
	// instead of a directory, the handler must NOT try to walk it
	// (filepath.Walk would call the func once and exit, but the
	// `info.IsDir()` check at the top of ExportDistZip must fire
	// first). Defense against a misconfigured deploy that landed a
	// tarball at the dist location.
	h, sqlDB, q, dataDir := newExportTestStack(t)
	siteID := "abcdef0123456789abcdef10"
	seedSiteAndDeploy(t, sqlDB, q, siteID, "filey", "succeeded")
	wsRoot := filepath.Join(dataDir, "workspaces", siteID)
	if err := os.MkdirAll(wsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// dist is a file, not a directory.
	if err := os.WriteFile(filepath.Join(wsRoot, "dist"), []byte("not-a-dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/api/sites/"+siteID+"/dist.zip", nil)
	r = withChiSiteParam(r, siteID)
	w := httptest.NewRecorder()
	h.ExportDistZip(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("dist-as-file should 404, got %d", w.Code)
	}
}

func TestExportDistZip_SymlinkChainEscapeRejected(t *testing.T) {
	// The walk must reject any symlink whose EvalSymlinks-resolved
	// target lives outside the dist tree. We plant: dist/legit.txt
	// (real file) + dist/escape -> /tmp/<random>/secret. The ZIP
	// must contain legit.txt and NOT secret.
	h, sqlDB, q, dataDir := newExportTestStack(t)
	siteID := "abcdef0123456789abcdef11"
	seedSiteAndDeploy(t, sqlDB, q, siteID, "links", "succeeded")
	distRoot := filepath.Join(dataDir, "workspaces", siteID, "dist")
	if err := os.MkdirAll(distRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(distRoot, "legit.txt"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Outside-of-dist target.
	outsideDir := t.TempDir()
	secret := filepath.Join(outsideDir, "secret")
	if err := os.WriteFile(secret, []byte("PWNED"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(distRoot, "escape")); err != nil {
		t.Skipf("symlink not supported on this filesystem: %v", err)
	}

	r := httptest.NewRequest("GET", "/api/sites/"+siteID+"/dist.zip", nil)
	r = withChiSiteParam(r, siteID)
	w := httptest.NewRecorder()
	h.ExportDistZip(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if f.Name == "escape" || strings.Contains(f.Name, "secret") {
			t.Errorf("escape symlink leaked into archive: %s", f.Name)
		}
		if !f.FileInfo().IsDir() {
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(rc)
			_ = rc.Close()
			if strings.Contains(string(body), "PWNED") {
				t.Errorf("escape target content leaked into %s", f.Name)
			}
		}
	}
}

func TestExportDistZip_SymlinkInsideDistAllowed(t *testing.T) {
	// Mirror of the escape test: a symlink to ANOTHER FILE inside
	// dist/ (e.g. /index.html -> /home.html) should land in the
	// archive correctly. Otherwise the symlink hardening kills
	// legitimate use.
	h, sqlDB, q, dataDir := newExportTestStack(t)
	siteID := "abcdef0123456789abcdef12"
	seedSiteAndDeploy(t, sqlDB, q, siteID, "innerlinks", "succeeded")
	distRoot := filepath.Join(dataDir, "workspaces", siteID, "dist")
	if err := os.MkdirAll(distRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(distRoot, "home.html")
	if err := os.WriteFile(target, []byte("home"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(distRoot, "index.html")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	r := httptest.NewRequest("GET", "/api/sites/"+siteID+"/dist.zip", nil)
	r = withChiSiteParam(r, siteID)
	w := httptest.NewRecorder()
	h.ExportDistZip(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	saw := map[string]bool{}
	for _, f := range zr.File {
		saw[f.Name] = true
	}
	if !saw["home.html"] {
		t.Error("expected home.html in archive")
	}
	if !saw["index.html"] {
		t.Error("expected index.html (the in-tree symlink) in archive")
	}
}

func TestExportDistZip_DistMissingReturns404(t *testing.T) {
	h, sqlDB, q, _ := newExportTestStack(t)
	siteID := "abcdef0123456789abcdef02"
	seedSiteAndDeploy(t, sqlDB, q, siteID, "no-dist", "succeeded")
	// No plantDist call → dist directory doesn't exist on disk.

	r := httptest.NewRequest("GET", "/api/sites/"+siteID+"/dist.zip", nil)
	r = withChiSiteParam(r, siteID)
	w := httptest.NewRecorder()
	h.ExportDistZip(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (dist missing)", w.Code)
	}
}

func TestExportDistZip_CrossTenantBuildIDBlocked(t *testing.T) {
	h, sqlDB, q, dataDir := newExportTestStack(t)
	siteA := "abcdef0123456789abcdef03"
	siteB := "abcdef0123456789abcdef04"
	depA := seedSiteAndDeploy(t, sqlDB, q, siteA, "alpha", "succeeded")
	seedSiteAndDeploy(t, sqlDB, q, siteB, "beta", "succeeded")
	plantDist(t, dataDir, siteB, map[string]string{"index.html": "B"})

	// Request /api/sites/{siteB}/dist.zip?build_id=<depA>
	// Expect 404 because the build belongs to siteA, not siteB.
	r := httptest.NewRequest("GET", "/api/sites/"+siteB+"/dist.zip?build_id="+depA, nil)
	r = withChiSiteParam(r, siteB)
	w := httptest.NewRecorder()
	h.ExportDistZip(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant build_id should 404, got %d", w.Code)
	}
}

func TestExportDistZip_ExplicitBuildIDPicksThatDeployment(t *testing.T) {
	h, sqlDB, q, dataDir := newExportTestStack(t)
	siteID := "abcdef0123456789abcdef05"
	// Two successful deployments; only one is named via build_id.
	seedSiteAndDeploy(t, sqlDB, q, siteID, "site", "succeeded")
	// Insert a second succeeded deployment manually.
	if err := q.CreateDeployment(context.Background(), store.CreateDeploymentParams{
		ID: "older_dep_001", SiteID: siteID, DeployTarget: "static", DeployConfig: "{}",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`UPDATE deployments SET status = 'succeeded' WHERE id = 'older_dep_001'`); err != nil {
		t.Fatal(err)
	}
	plantDist(t, dataDir, siteID, map[string]string{"index.html": "ok"})

	r := httptest.NewRequest("GET", "/api/sites/"+siteID+"/dist.zip?build_id=older_dep_001", nil)
	r = withChiSiteParam(r, siteID)
	w := httptest.NewRecorder()
	h.ExportDistZip(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Disposition"), "older_dep_00") {
		t.Errorf("filename should embed picked build id, got %q", w.Header().Get("Content-Disposition"))
	}
}

func TestExportDistZip_InvalidSiteID(t *testing.T) {
	h, _, _, _ := newExportTestStack(t)
	r := httptest.NewRequest("GET", "/api/sites/bad-id/dist.zip", nil)
	r = withChiSiteParam(r, "not-hex")
	w := httptest.NewRecorder()
	h.ExportDistZip(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (invalid siteID)", w.Code)
	}
}

func TestBuildExportFilename_Sanitizes(t *testing.T) {
	cases := []struct {
		slug, build, created string
		want                 string
	}{
		{"my-site", "abc123def456789", "2026-05-04T12:00:00Z", "my-site-20260504T120000Z-abc123def456.zip"},
		{"weird ../../!!", "x", "", "weird-"},
	}
	for _, tc := range cases {
		got := buildExportFilename(tc.slug, tc.build, tc.created)
		if !strings.Contains(got, tc.want) {
			t.Errorf("filename %q does not contain %q", got, tc.want)
		}
	}
}

func TestSafeSlugForFilename(t *testing.T) {
	cases := map[string]string{
		"my-site":                "my-site",
		"My Site":                "my-site",
		"a/b/c":                  "a-b-c",
		"-leading-and-trailing-": "leading-and-trailing",
		"a..b..c":                "a-b-c",
		"":                       "",
	}
	for in, want := range cases {
		if got := safeSlugForFilename(in); got != want {
			t.Errorf("safeSlugForFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func zipKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
