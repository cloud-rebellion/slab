package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"
	_ "modernc.org/sqlite"

	"github.com/bright-interaction/slab/internal/config"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"
)

func setupDeployTestDB(t *testing.T) (*sql.DB, *store.Queries) {
	t.Helper()
	sqlDB, _ := newSchemaDB(t, "test.sqlite")
	return sqlDB, store.New(sqlDB)
}

func seedSite(t *testing.T, q *store.Queries, id string) {
	t.Helper()
	if err := q.CreateSite(context.Background(), store.CreateSiteParams{
		ID: id, Name: "Test", Slug: id, Domain: "",
		PrimaryColor: "#000000", SecondaryColor: "#000000",
		BgColor: "#FFFFFF", TextColor: "#000000",
		FontHeading: "Inter", FontBody: "Inter", Lang: "en",
	}); err != nil {
		t.Fatalf("create site: %v", err)
	}
}

func deployRouter(h *DeployHandler) chi.Router {
	r := chi.NewRouter()
	// In production these routes sit behind AuthMiddleware + SiteAccessMiddleware.
	// The CRUD handlers now also call RequireOwnerOrAdmin, so inject an
	// authenticated admin (admin bypasses the per-site role check) to exercise
	// the handler logic in isolation.
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := context.WithValue(req.Context(), authmw.UserContextKey, &authmw.AuthUser{ID: "u-admin", Role: "admin"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Get("/api/sites/{siteID}/deploy-targets", h.ListTargets)
	r.Post("/api/sites/{siteID}/deploy-targets", h.CreateTarget)
	r.Get("/api/sites/{siteID}/deploy-targets/{targetID}", h.GetTarget)
	r.Patch("/api/sites/{siteID}/deploy-targets/{targetID}", h.UpdateTarget)
	r.Delete("/api/sites/{siteID}/deploy-targets/{targetID}", h.DeleteTarget)
	r.Post("/api/sites/{siteID}/deploy-targets/{targetID}/default", h.SetDefault)
	return r
}

func doJSON(t *testing.T, r chi.Router, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	resp := w.Result()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) == 0 {
		return resp.StatusCode, nil
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		// list endpoint wraps in {targets: [...]}, but unmarshal-into-map fails for top-level array.
		// Wrap the body manually.
		out = map[string]any{"raw": string(raw)}
	}
	return resp.StatusCode, out
}

func TestDeployHandler_E2E_LocalTargetFlow(t *testing.T) {
	sqlDB, q := setupDeployTestDB(t)
	seedSite(t, q, "site1")
	parent := t.TempDir()
	// Local target paths must be scoped to the site (include the site id as a
	// path segment) per the cross-tenant isolation guard in LocalDeployer.Validate.
	dest := filepath.Join(parent, "site1", "site")

	h := NewDeployHandler(&config.Config{}, q, sqlDB)
	r := deployRouter(h)

	// 1. POST a local target.
	createBody := map[string]any{
		"name": "primary",
		"kind": "local",
		"config": map[string]any{
			"path":       dest,
			"public_url": "https://example.com",
		},
	}
	code, resp := doJSON(t, r, http.MethodPost, "/api/sites/site1/deploy-targets", createBody)
	if code != http.StatusCreated {
		t.Fatalf("create: got %d body %+v", code, resp)
	}
	id, _ := resp["id"].(string)
	if id == "" {
		t.Fatalf("create: missing id; body=%+v", resp)
	}
	if got, _ := resp["kind"].(string); got != "local" {
		t.Fatalf("create: kind=%q", got)
	}

	// 2. GET list returns 1 row.
	code, resp = doJSON(t, r, http.MethodGet, "/api/sites/site1/deploy-targets", nil)
	if code != http.StatusOK {
		t.Fatalf("list: got %d", code)
	}
	targets, _ := resp["targets"].([]any)
	if len(targets) != 1 {
		t.Fatalf("list: got %d targets", len(targets))
	}

	// 3. POST default.
	code, resp = doJSON(t, r, http.MethodPost, "/api/sites/site1/deploy-targets/"+id+"/default", nil)
	if code != http.StatusOK {
		t.Fatalf("set default: got %d body %+v", code, resp)
	}
	if got, _ := resp["is_default"].(bool); !got {
		t.Fatalf("set default: is_default=%v", resp["is_default"])
	}

	// 4. DB row reflects default.
	row := sqlDB.QueryRowContext(context.Background(),
		"SELECT name, kind, is_default FROM deploy_targets WHERE id = ?", id)
	var name, kind string
	var isDefault int64
	if err := row.Scan(&name, &kind, &isDefault); err != nil {
		t.Fatalf("db scan: %v", err)
	}
	if name != "primary" || kind != "local" || isDefault != 1 {
		t.Fatalf("db row: name=%q kind=%q default=%d", name, kind, isDefault)
	}

	// 5. Add a second target as default; first must lose default.
	createBody2 := map[string]any{
		"name":       "backup",
		"kind":       "local",
		"is_default": true,
		"config": map[string]any{
			"path": filepath.Join(parent, "site1", "backup-site"),
		},
	}
	code, resp = doJSON(t, r, http.MethodPost, "/api/sites/site1/deploy-targets", createBody2)
	if code != http.StatusCreated {
		t.Fatalf("create2: got %d body %+v", code, resp)
	}
	var n int64
	if err := sqlDB.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM deploy_targets WHERE site_id = ? AND is_default = 1", "site1").Scan(&n); err != nil {
		t.Fatalf("count default: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 default target, got %d", n)
	}
}

func TestDeployHandler_E2E_ValidationRejects(t *testing.T) {
	sqlDB, q := setupDeployTestDB(t)
	seedSite(t, q, "site1")
	h := NewDeployHandler(&config.Config{}, q, sqlDB)
	r := deployRouter(h)

	// Missing name.
	code, _ := doJSON(t, r, http.MethodPost, "/api/sites/site1/deploy-targets", map[string]any{
		"kind": "local", "config": map[string]any{"path": "/tmp/x"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("missing name: got %d", code)
	}

	// Bad kind.
	code, _ = doJSON(t, r, http.MethodPost, "/api/sites/site1/deploy-targets", map[string]any{
		"name": "x", "kind": "ftp", "config": map[string]any{"path": "/tmp/x"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("bad kind: got %d", code)
	}

	// Local with relative path.
	code, _ = doJSON(t, r, http.MethodPost, "/api/sites/site1/deploy-targets", map[string]any{
		"name": "x", "kind": "local", "config": map[string]any{"path": "relative/path"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("relative path: got %d", code)
	}
}
