package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/bright-interaction/slab/internal/config"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/starterkits"
	"github.com/bright-interaction/slab/internal/store"
)

// seedAuthCtx wraps the request with a fake admin user + injects the
// admin's UserContextKey so the workspace gate added in Phase 30.2 (cloud
// tier) finds an authenticated caller. Centralised here so any new Seed
// test can reuse it instead of re-deriving the auth wiring.
func seedAuthCtx(req *http.Request) *http.Request {
	user := &authmw.AuthUser{
		ID: "test-admin", Email: "test@admin.local",
		Name: "Test Admin", Role: "admin",
	}
	return req.WithContext(context.WithValue(req.Context(), authmw.UserContextKey, user))
}

type seedFakeKit struct {
	id          string
	calls       int
	lastSiteID  string
	lastQueries *store.Queries
	err         error
}

func (f *seedFakeKit) ID() string                { return f.id }
func (f *seedFakeKit) Name() string              { return "Fake" }
func (f *seedFakeKit) Description() string       { return "fake kit for tests" }
func (f *seedFakeKit) TargetSiteTypes() []string { return []string{"b2b"} }
func (f *seedFakeKit) Apply(_ context.Context, q *store.Queries, siteID string) error {
	f.calls++
	f.lastSiteID = siteID
	f.lastQueries = q
	return f.err
}

func setupSeedTestDB(t *testing.T) (*sql.DB, *store.Queries) {
	t.Helper()
	sqlDB, _ := newSchemaDB(t, "test.sqlite")
	q := store.New(sqlDB)
	// Phase 30.2: Seed/Create resolve a workspace via ListWorkspaceIDsForUser
	// or, for admins with no membership row, ListAllWorkspaces[0]. Tests use
	// the admin path so we just need one workspace to exist.
	if err := q.CreateWorkspace(context.Background(), store.CreateWorkspaceParams{
		ID: "ws-test", Name: "Test", Slug: "test-ws",
		Plan: "oss", Region: "eu", BillingEmail: "ops@test.local",
		Status: "active", TrialEndsAt: "",
	}); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	// Seed grants the creating user an owner row in site_members, and that
	// FK needs a real users row. seedAuthCtx injects "test-admin", so the
	// fixture has to contain it. Before the grant existed the fixture got
	// away without a user, which is exactly why the missing grant went
	// unnoticed: nothing in the test path ever needed the creator to exist.
	if err := q.CreateUser(context.Background(), store.CreateUserParams{
		ID: "test-admin", Email: "test@admin.local", PasswordHash: "x",
		Name: "Test Admin", Role: "admin",
	}); err != nil {
		t.Fatalf("seed creating user: %v", err)
	}
	return sqlDB, q
}

func mustCount(t *testing.T, sqlDB *sql.DB, query string, args ...any) int64 {
	t.Helper()
	row := sqlDB.QueryRowContext(context.Background(), query, args...)
	var n int64
	if err := row.Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func TestSiteHandler_Seed_HappyPath(t *testing.T) {
	sqlDB, q := setupSeedTestDB(t)
	h := NewSiteHandler(&config.Config{}, q, sqlDB)

	body := `{
		"type": "b2b",
		"info": {
			"name": "Acme",
			"slug": "acme",
			"domain": "acme.test",
			"lang": "sv",
			"business_name": "Acme AB",
			"contact_email": "hello@acme.test",
			"country": "SE"
		},
		"structure": {"structure_type": "soft-silo", "max_depth": 3},
		"silos": [
			{"name": "Services", "slug_prefix": "/services"},
			{"name": "Industries", "slug_prefix": "/industries"}
		],
		"branding": {
			"primary_color": "#112233",
			"secondary_color": "#445566",
			"bg_color": "#FFFFFF",
			"text_color": "#1A1A1A",
			"font_heading": "Inter",
			"font_body": "Inter"
		}
	}`
	req := httptest.NewRequest("POST", "/api/sites/seed", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Seed(rr, seedAuthCtx(req))

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	siteID := resp["site_id"]
	if siteID == "" {
		t.Fatalf("expected site_id in response, got %s", rr.Body.String())
	}

	// Verify all rows were created
	if got := mustCount(t, sqlDB, "select count(*) from sites where id=?", siteID); got != 1 {
		t.Errorf("sites: want 1, got %d", got)
	}
	if got := mustCount(t, sqlDB, "select count(*) from site_profiles where site_id=?", siteID); got != 1 {
		t.Errorf("site_profiles: want 1, got %d", got)
	}
	if got := mustCount(t, sqlDB, "select count(*) from site_architecture where site_id=?", siteID); got != 1 {
		t.Errorf("site_architecture: want 1, got %d", got)
	}
	if got := mustCount(t, sqlDB, "select count(*) from site_silos where site_id=?", siteID); got != 2 {
		t.Errorf("site_silos: want 2, got %d", got)
	}
	if got := mustCount(t, sqlDB, "select count(*) from knowledgebase_entries where site_id=?", siteID); got < 3 {
		t.Errorf("knowledgebase_entries: want >=3, got %d", got)
	}
	if got := mustCount(t, sqlDB, "select count(*) from guardrail_rules where site_id=?", siteID); got < 2 {
		t.Errorf("guardrail_rules: want >=2, got %d", got)
	}
	if got := mustCount(t, sqlDB, "select count(*) from global_blocks where site_id=?", siteID); got != 2 {
		t.Errorf("global_blocks: want 2, got %d", got)
	}
	if got := mustCount(t, sqlDB, "select count(*) from pages where site_id=? and slug in ('/privacy','/terms','/cookies','/404')", siteID); got != 4 {
		t.Errorf("essential pages: want 4, got %d", got)
	}
	if got := mustCount(t, sqlDB, "select count(*) from site_settings where site_id=?", siteID); got < 10 {
		t.Errorf("site_settings: want >=10, got %d", got)
	}
}

func TestSiteHandler_Seed_DuplicateSlug_RollsBack(t *testing.T) {
	sqlDB, q := setupSeedTestDB(t)
	h := NewSiteHandler(&config.Config{}, q, sqlDB)

	body := `{
		"type": "b2b",
		"info": {"name": "Acme", "slug": "acme"},
		"structure": {"structure_type": "soft-silo", "max_depth": 3},
		"silos": [{"name": "S", "slug_prefix": "/s"}],
		"branding": {}
	}`

	// First call succeeds
	req1 := httptest.NewRequest("POST", "/api/sites/seed", bytes.NewReader([]byte(body)))
	req1.Header.Set("Content-Type", "application/json")
	rr1 := httptest.NewRecorder()
	h.Seed(rr1, seedAuthCtx(req1))
	if rr1.Code != http.StatusOK {
		t.Fatalf("first seed: expected 200, got %d: %s", rr1.Code, rr1.Body.String())
	}

	siteCountBefore := mustCount(t, sqlDB, "select count(*) from sites")
	siloCountBefore := mustCount(t, sqlDB, "select count(*) from site_silos")
	kbCountBefore := mustCount(t, sqlDB, "select count(*) from knowledgebase_entries")

	// Second call with same slug: must 409 and not add any rows
	req2 := httptest.NewRequest("POST", "/api/sites/seed", bytes.NewReader([]byte(body)))
	req2.Header.Set("Content-Type", "application/json")
	rr2 := httptest.NewRecorder()
	h.Seed(rr2, seedAuthCtx(req2))
	if rr2.Code != http.StatusConflict {
		t.Fatalf("second seed: expected 409, got %d: %s", rr2.Code, rr2.Body.String())
	}

	if got := mustCount(t, sqlDB, "select count(*) from sites"); got != siteCountBefore {
		t.Errorf("rollback failed: sites changed from %d to %d", siteCountBefore, got)
	}
	if got := mustCount(t, sqlDB, "select count(*) from site_silos"); got != siloCountBefore {
		t.Errorf("rollback failed: site_silos changed from %d to %d", siloCountBefore, got)
	}
	if got := mustCount(t, sqlDB, "select count(*) from knowledgebase_entries"); got != kbCountBefore {
		t.Errorf("rollback failed: kb changed from %d to %d", kbCountBefore, got)
	}
}

func TestSiteHandler_Seed_BadSlug(t *testing.T) {
	sqlDB, q := setupSeedTestDB(t)
	h := NewSiteHandler(&config.Config{}, q, sqlDB)

	body := `{"type":"b2b","info":{"name":"X","slug":"BAD SLUG"},"structure":{},"branding":{}}`
	req := httptest.NewRequest("POST", "/api/sites/seed", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Seed(rr, seedAuthCtx(req))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSiteHandler_Seed_BadColor(t *testing.T) {
	sqlDB, q := setupSeedTestDB(t)
	h := NewSiteHandler(&config.Config{}, q, sqlDB)

	body := `{"type":"b2b","info":{"name":"X","slug":"x"},"structure":{},"branding":{"primary_color":"red"}}`
	req := httptest.NewRequest("POST", "/api/sites/seed", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Seed(rr, seedAuthCtx(req))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSiteHandler_Seed_StarterKit_Applied(t *testing.T) {
	sqlDB, q := setupSeedTestDB(t)
	h := NewSiteHandler(&config.Config{}, q, sqlDB)

	// Register a fake kit on the package-level registry. Use a unique id to
	// avoid collisions with kits registered via init() by Worker P9-B.
	fk := &seedFakeKit{id: "fake-applied"}
	starterkits.Default.Register(fk)

	body := `{
		"type": "b2b",
		"info": {"name": "Acme", "slug": "acme-applied"},
		"structure": {"structure_type": "soft-silo", "max_depth": 3},
		"silos": [{"name": "S", "slug_prefix": "/s"}],
		"branding": {},
		"starter_kit": "fake-applied"
	}`
	req := httptest.NewRequest("POST", "/api/sites/seed", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Seed(rr, seedAuthCtx(req))

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if fk.calls != 1 {
		t.Fatalf("expected Apply to be called once, got %d", fk.calls)
	}
	if fk.lastSiteID == "" {
		t.Fatalf("expected Apply to receive a non-empty siteID")
	}
	if fk.lastQueries == nil {
		t.Fatalf("expected Apply to receive a tx-bound *store.Queries")
	}
}

func TestSiteHandler_Seed_StarterKit_Unknown_Rejected(t *testing.T) {
	sqlDB, q := setupSeedTestDB(t)
	h := NewSiteHandler(&config.Config{}, q, sqlDB)

	body := `{
		"type": "b2b",
		"info": {"name": "Acme", "slug": "acme-unknown"},
		"structure": {"structure_type": "soft-silo", "max_depth": 3},
		"silos": [{"name": "S", "slug_prefix": "/s"}],
		"branding": {},
		"starter_kit": "definitely-not-registered"
	}`
	req := httptest.NewRequest("POST", "/api/sites/seed", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Seed(rr, seedAuthCtx(req))

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown kit, got %d: %s", rr.Code, rr.Body.String())
	}
	// Nothing should have been written.
	if got := mustCount(t, sqlDB, "select count(*) from sites where slug=?", "acme-unknown"); got != 0 {
		t.Fatalf("expected no site row for unknown kit, got %d", got)
	}
}

func TestSiteHandler_Seed_StarterKit_ApplyError_RollsBack(t *testing.T) {
	sqlDB, q := setupSeedTestDB(t)
	h := NewSiteHandler(&config.Config{}, q, sqlDB)

	fk := &seedFakeKit{id: "fake-fails", err: errKitBoom}
	starterkits.Default.Register(fk)

	body := `{
		"type": "b2b",
		"info": {"name": "Acme", "slug": "acme-fail"},
		"structure": {"structure_type": "soft-silo", "max_depth": 3},
		"silos": [{"name": "S", "slug_prefix": "/s"}],
		"branding": {},
		"starter_kit": "fake-fails"
	}`
	req := httptest.NewRequest("POST", "/api/sites/seed", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Seed(rr, seedAuthCtx(req))

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when kit Apply fails, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := mustCount(t, sqlDB, "select count(*) from sites where slug=?", "acme-fail"); got != 0 {
		t.Fatalf("expected rollback (no site row), got %d", got)
	}
}

func TestSiteHandler_Seed_NoKit_StillWorks(t *testing.T) {
	sqlDB, q := setupSeedTestDB(t)
	h := NewSiteHandler(&config.Config{}, q, sqlDB)

	body := `{
		"type": "b2b",
		"info": {"name": "Acme", "slug": "acme-nokit"},
		"structure": {"structure_type": "soft-silo", "max_depth": 3},
		"silos": [{"name": "S", "slug_prefix": "/s"}],
		"branding": {}
	}`
	req := httptest.NewRequest("POST", "/api/sites/seed", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Seed(rr, seedAuthCtx(req))

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 without a kit, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSiteHandler_ListStarterKits(t *testing.T) {
	sqlDB, q := setupSeedTestDB(t)
	h := NewSiteHandler(&config.Config{}, q, sqlDB)

	starterkits.Default.Register(&seedFakeKit{id: "fake-listed"})

	req := httptest.NewRequest("GET", "/api/starter-kits", nil)
	rr := httptest.NewRecorder()
	h.ListStarterKits(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var kits []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &kits); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	found := false
	for _, k := range kits {
		if k["id"] == "fake-listed" {
			found = true
			if k["name"] == nil || k["description"] == nil || k["target_site_types"] == nil {
				t.Fatalf("expected all fields populated, got %+v", k)
			}
		}
	}
	if !found {
		t.Fatalf("expected fake-listed in catalog, got %+v", kits)
	}
}

// TestSiteHandler_Seed_GrantsCreatorOwnership drives the REAL wizard path
// and asserts the creator ends up with the site_members row every
// owner-gated surface depends on.
//
// This is the test that was missing. The authz guard tests hand-insert the
// membership row with seedSiteMember(), so they only ever ran against a
// state the wizard could not produce: POST /api/sites/seed created a site
// with ZERO site_members rows, SiteAccessRole reads that table alone and
// returned "", and the tenant's own operator was then refused on payments
// settings and on the site domain with no way to repair it (the only other
// AddSiteMember call is in Create, and there is no site-member route).
// A guard test whose fixture no real code path can produce is not a guard
// test, so this one goes through the handler.
func TestSiteHandler_Seed_GrantsCreatorOwnership(t *testing.T) {
	sqlDB, q := setupSeedTestDB(t)
	h := &SiteHandler{db: sqlDB, queries: q}

	body := `{
		"type": "b2b",
		"info": {
			"name": "Acme", "slug": "acme", "domain": "acme.test", "lang": "sv",
			"business_name": "Acme AB", "contact_email": "hello@acme.test", "country": "SE"
		},
		"structure": {"structure_type": "soft-silo", "max_depth": 3},
		"silos": [{"name": "Services", "slug_prefix": "/services"}],
		"branding": {
			"primary_color": "#112233", "secondary_color": "#445566",
			"bg_color": "#FFFFFF", "text_color": "#1A1A1A",
			"font_heading": "Inter", "font_body": "Inter"
		}
	}`
	req := httptest.NewRequest("POST", "/api/sites/seed", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Seed(rr, seedAuthCtx(req))

	if rr.Code != http.StatusOK {
		t.Fatalf("seed: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	siteID := resp["site_id"]
	if siteID == "" {
		t.Fatalf("expected site_id, got %s", rr.Body.String())
	}

	// The row itself.
	if got := mustCount(t, sqlDB,
		"select count(*) from site_members where site_id=? and user_id=? and role='owner'",
		siteID, "test-admin"); got != 1 {
		t.Errorf("owner site_members row: want 1, got %d", got)
	}
	// No stray extra members.
	if got := mustCount(t, sqlDB, "select count(*) from site_members where site_id=?", siteID); got != 1 {
		t.Errorf("site_members rows for site: want exactly 1, got %d", got)
	}
	// The predicate every owner-gated handler actually calls. This is the
	// one that returned "" and produced the 403s.
	if role := authmw.SiteAccessRole(context.Background(), q, "test-admin", siteID); role != "owner" {
		t.Errorf("SiteAccessRole after wizard seed: want \"owner\", got %q", role)
	}
}

var errKitBoom = errKitBoomT("kit boom")

type errKitBoomT string

func (e errKitBoomT) Error() string { return string(e) }
