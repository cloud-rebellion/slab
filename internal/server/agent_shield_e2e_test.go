package server

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/bright-interaction/slab/internal/config"
	dbpkg "github.com/bright-interaction/slab/internal/db"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/storage"
	"github.com/bright-interaction/slab/internal/store"
)

// Audit 2026-08-11 finding 12: Shield covered the MCP transport and not the
// REST agent surface, though both authenticate with the same X-Agent-Key and
// serve the same tenant. GET /api/agent/visitor-metadata returned a visitor's
// raw email and the entire CRM metadata map to the model, and
// GET /api/agent/context was the one ContextBuilder.Build caller of ten with
// no ShieldJSON anywhere in front of it.
//
// These tests drive the REAL router with a REAL 32-byte shield key. Nothing
// here is a stand-in: the whole point of the finding is that the boundary was
// a property of one transport's handler code, so only the real route table
// can prove the other transport is now covered.

const (
	testShieldKey  = "0123456789abcdef0123456789abcdef" // exactly 32 bytes
	testAgentKey   = "test-agent-key-for-shield"
	testSiteID     = "site_shield_test"
	visitorEmail   = "anna.andersson@andersson-law.se"
	crmMetaEmail   = "crm.contact@klientbolaget.se"
	profileEmail   = "kontakt@testbutiken.se"
	profilePhone   = "+46 70 123 45 67"
	blockEmail     = "saljteamet@testbutiken.se"
	testFingerprnt = "fp-shield-test"
)

// newShieldedTestRouter builds the real router with Shield enabled and seeds
// one site, one agent key and one identified visitor carrying PII.
func newShieldedTestRouter(t *testing.T) (http.Handler, *sql.DB) {
	t.Helper()
	dir := scratchDir(t)
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

	if _, err := sqlDB.Exec(
		`INSERT INTO sites (id, workspace_id, name, slug, domain, status) VALUES (?, 'ws_test', 'Shield Test', 'shield-test', 'shield.test', 'draft')`,
		testSiteID); err != nil {
		t.Fatalf("seed site: %v", err)
	}
	if _, err := sqlDB.Exec(
		`INSERT INTO agent_keys (id, site_id, name, key_hash, capabilities, is_active) VALUES ('ak_test', ?, 'test', ?, '["read","write"]', 1)`,
		testSiteID, authmw.HashAgentKey(testAgentKey)); err != nil {
		t.Fatalf("seed agent key: %v", err)
	}
	if _, err := sqlDB.Exec(
		`INSERT INTO site_profiles (id, site_id, business_name, contact_email, contact_phone) VALUES ('sp_test', ?, 'Testbutiken AB', ?, ?)`,
		testSiteID, profileEmail, profilePhone); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	// A page block carrying a contact address. This is the tenant content
	// ContextBuilder.Build returns verbatim in PageInfo.Blocks[].DataJSON,
	// and it is the PII the nine internal/mcp callers of Build already
	// tokenize before the model sees it.
	if _, err := sqlDB.Exec(
		`INSERT INTO pages (id, site_id, title, slug, status) VALUES ('pg_test', ?, 'Kontakt', '/kontakt', 'published')`,
		testSiteID); err != nil {
		t.Fatalf("seed page: %v", err)
	}
	if _, err := sqlDB.Exec(
		`INSERT INTO blocks (id, page_id, block_type, data_json) VALUES ('blk_test', 'pg_test', 'text', ?)`,
		`{"text":"Hor av dig till `+blockEmail+` sa aterkommer vi."}`); err != nil {
		t.Fatalf("seed block: %v", err)
	}
	if _, err := sqlDB.Exec(
		`INSERT INTO visit_sessions (id, site_id, fingerprint, visitor_id, email, identified_at, metadata_json, identity_confirmed_at)
		 VALUES ('vs_test', ?, ?, 'vid-1', ?, '2026-08-11T10:00:00Z', ?, '2026-08-11T10:00:00Z')`,
		testSiteID, testFingerprnt, visitorEmail,
		`{"lifecycle_stage":"customer","crm_email":"`+crmMetaEmail+`"}`); err != nil {
		t.Fatalf("seed visitor: %v", err)
	}

	cfg := &config.Config{
		Env:           config.EnvDevelopment,
		BaseURL:       "http://localhost:8080",
		JWTSecret:     "test-jwt-secret-that-is-long-enough-for-hs256",
		DataDir:       dir,
		MaxUploadSize: 20 << 20,
		ShieldKey:     testShieldKey,
	}
	return New(cfg, sqlDB, store.New(sqlDB), st).Router(), sqlDB
}

func agentGET(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Agent-Key", testAgentKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestRESTAgentVisitorMetadataIsShielded is the repro for the headline half of
// finding 12. Before the fix this endpoint answered 200 with the visitor's raw
// email and the raw CRM metadata map.
func TestRESTAgentVisitorMetadataIsShielded(t *testing.T) {
	h, _ := newShieldedTestRouter(t)

	rec := agentGET(t, h, "/api/agent/visitor-metadata?fingerprint="+testFingerprnt)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if strings.Contains(body, visitorEmail) {
		t.Errorf("PII LEAK: visitor email %q reached the agent verbatim.\nbody=%s", visitorEmail, body)
	}
	if strings.Contains(body, crmMetaEmail) {
		t.Errorf("PII LEAK: CRM metadata email %q reached the agent verbatim.\nbody=%s", crmMetaEmail, body)
	}
	if !strings.Contains(body, "[shield:email:") {
		t.Errorf("no shield marker in the response; the redact pass did not run.\nbody=%s", body)
	}
}

// TestRESTAgentContextIsShielded covers the tenth ContextBuilder.Build caller,
// the one the audit found with no ShieldJSON anywhere in front of it. The
// block text is tenant content Build returns verbatim, and the nine
// internal/mcp callers of the same builder already tokenize it.
func TestRESTAgentContextIsShielded(t *testing.T) {
	h, _ := newShieldedTestRouter(t)

	rec := agentGET(t, h, "/api/agent/context")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if strings.Contains(body, blockEmail) {
		t.Errorf("PII LEAK: block contact email %q reached the agent verbatim from /api/agent/context", blockEmail)
	}
	if !strings.Contains(body, "[shield:email:") {
		t.Errorf("no shield marker anywhere in the context response; the redact pass did not run")
	}
}

// TestRESTAgentProfileIsShielded pins the other read surface that returns the
// site_profiles row. get_profile over MCP was already covered; its REST twin
// is /api/agent/profile.
func TestRESTAgentProfileIsShielded(t *testing.T) {
	h, _ := newShieldedTestRouter(t)

	rec := agentGET(t, h, "/api/agent/profile")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, profileEmail) {
		t.Errorf("PII LEAK: profile contact_email %q reached the agent verbatim.\nbody=%s", profileEmail, body)
	}
	if !strings.Contains(body, "[shield:") {
		t.Errorf("no shield marker in the profile response.\nbody=%s", body)
	}
}

// TestRESTAgentWriteResolvesMarkers proves the request half of the chokepoint.
// Without it, response shielding would make the agent echo literal
// "[shield:...]" strings back into tenant content on every write, which is a
// recorded prior failure of this boundary.
func TestRESTAgentWriteResolvesMarkers(t *testing.T) {
	h, db := newShieldedTestRouter(t)

	// Read the profile so a marker for profileEmail exists in this session.
	rec := agentGET(t, h, "/api/agent/profile")
	if rec.Code != http.StatusOK {
		t.Fatalf("read profile: status %d body %s", rec.Code, rec.Body.String())
	}
	marker := extractMarker(t, rec.Body.String())

	// Echo that marker back into a write, exactly as an agent would.
	req := httptest.NewRequest(http.MethodPatch, "/api/agent/profile",
		strings.NewReader(`{"business_name":"Testbutiken AB","contact_email":"`+marker+`"}`))
	req.Header.Set("X-Agent-Key", testAgentKey)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("patch profile: status %d body %s", w.Code, w.Body.String())
	}

	var stored string
	if err := db.QueryRow(`SELECT contact_email FROM site_profiles WHERE site_id = ?`, testSiteID).Scan(&stored); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if strings.Contains(stored, "[shield:") {
		t.Fatalf("stored a literal shield marker instead of plaintext: %q", stored)
	}
	if stored != profileEmail {
		t.Fatalf("stored = %q, want the resolved plaintext %q", stored, profileEmail)
	}
}

// TestRESTAgentSurfaceShieldIsOffWithoutAKey pins that an install with no
// ATOMICSITE_SHIELD_KEY is unchanged: the middleware must be a pure no-op, not
// a source of new failures for OSS/dev deployments.
func TestRESTAgentSurfaceShieldIsOffWithoutAKey(t *testing.T) {
	dir := scratchDir(t)
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
	if _, err := sqlDB.Exec(
		`INSERT INTO sites (id, workspace_id, name, slug, status) VALUES (?, 'ws_test', 'No Shield', 'no-shield', 'draft')`,
		testSiteID); err != nil {
		t.Fatalf("seed site: %v", err)
	}
	if _, err := sqlDB.Exec(
		`INSERT INTO agent_keys (id, site_id, name, key_hash, capabilities, is_active) VALUES ('ak_test', ?, 'test', ?, '["read"]', 1)`,
		testSiteID, authmw.HashAgentKey(testAgentKey)); err != nil {
		t.Fatalf("seed agent key: %v", err)
	}
	if _, err := sqlDB.Exec(
		`INSERT INTO site_profiles (id, site_id, contact_email) VALUES ('sp_test', ?, ?)`,
		testSiteID, profileEmail); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	cfg := &config.Config{
		Env:           config.EnvDevelopment,
		BaseURL:       "http://localhost:8080",
		JWTSecret:     "test-jwt-secret-that-is-long-enough-for-hs256",
		DataDir:       dir,
		MaxUploadSize: 20 << 20,
	}
	h := New(cfg, sqlDB, store.New(sqlDB), st).Router()

	rec := agentGET(t, h, "/api/agent/profile")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), profileEmail) {
		t.Errorf("shield-disabled install redacted anyway; body=%s", rec.Body.String())
	}
}

// extractMarker pulls the first [shield:...] marker out of a response body.
func extractMarker(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, "[shield:")
	if i < 0 {
		t.Fatalf("no shield marker in body: %s", body)
	}
	j := strings.IndexByte(body[i:], ']')
	if j < 0 {
		t.Fatalf("unterminated shield marker in body: %s", body)
	}
	return body[i : i+j+1]
}

// scratchDir is t.TempDir with a best-effort cleanup. AgentAuthMiddleware
// updates last_used_at in a background goroutine, so the SQLite WAL/SHM files
// can outlive the test body by microseconds. t.TempDir treats a non-empty
// directory at cleanup as a test FAILURE, which turned a passing assertion
// into a flake under a loaded `go test ./...`.
func scratchDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "atomicsite-shield-test-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
