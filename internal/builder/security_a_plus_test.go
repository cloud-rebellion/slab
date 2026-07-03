package builder

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	dbpkg "github.com/bright-interaction/slab/internal/db"
	"github.com/bright-interaction/slab/internal/store"

	_ "modernc.org/sqlite"
)

// openSecurityTestDB sets up an in-memory SQLite with the canonical
// schema so we can exercise BuildSecurityHeaders and the nginx renderer
// against a real database. Mirrors the helper in
// internal/retention/retention_test.go (kept local to avoid a cross-
// package internal_test import dance).
func openSecurityTestDB(t *testing.T) (*sql.DB, *store.Queries) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sec.sqlite")
	sqlDB, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if _, err := sqlDB.Exec(string(dbpkg.Schema)); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return sqlDB, store.New(sqlDB)
}

func seedSecuritySite(t *testing.T, sqlDB *sql.DB, id string) {
	t.Helper()
	_, err := sqlDB.Exec(`INSERT INTO sites (id, name, slug) VALUES (?, ?, ?)`, id, "Test", id+"-slug")
	if err != nil {
		t.Fatalf("seed site: %v", err)
	}
}

// TestBuildSecurityHeaders_AutoAPlusDefaults locks in the contract: a
// fresh tenant with NO security settings rows produces every header a
// scanner needs for an A+ grade. This is the regression guard for the
// whole "Atomic Site is automatically A+" pitch — break this and a
// new tenant ships with weak defaults.
func TestBuildSecurityHeaders_AutoAPlusDefaults(t *testing.T) {
	sqlDB, q := openSecurityTestDB(t)
	siteID := "abcdef0123456789abcdef01"
	seedSecuritySite(t, sqlDB, siteID)

	h, err := BuildSecurityHeaders(context.Background(), q, siteID)
	if err != nil {
		t.Fatalf("BuildSecurityHeaders: %v", err)
	}

	// Every Site Inspector / securityheaders.com check that needs a
	// header must be non-empty here. The values themselves are tested
	// separately; this asserts presence.
	checks := map[string]string{
		"CSP":                           h.CSP,
		"HSTS":                          h.HSTS,
		"XFrameOptions":                 h.XFrameOptions,
		"XContentTypeOptions":           h.XContentTypeOptions,
		"ReferrerPolicy":                h.ReferrerPolicy,
		"PermissionsPolicy":             h.PermissionsPolicy,
		"COOP":                          h.COOP,
		"CORP":                          h.CORP,
		"XPermittedCrossDomainPolicies": h.XPermittedCrossDomainPolicies,
		"XXSSProtection":                h.XXSSProtection,
	}
	for name, value := range checks {
		if strings.TrimSpace(value) == "" {
			t.Errorf("default %s is empty — A+ regression", name)
		}
	}
}

// TestBuildSecurityHeaders_NewHeaderValues verifies the specific values
// scanners want for the two headers added 2026-05-01.
func TestBuildSecurityHeaders_NewHeaderValues(t *testing.T) {
	sqlDB, q := openSecurityTestDB(t)
	siteID := "abcdef0123456789abcdef02"
	seedSecuritySite(t, sqlDB, siteID)

	h, err := BuildSecurityHeaders(context.Background(), q, siteID)
	if err != nil {
		t.Fatalf("BuildSecurityHeaders: %v", err)
	}
	if h.XPermittedCrossDomainPolicies != "none" {
		t.Errorf("X-Permitted-Cross-Domain-Policies = %q want none", h.XPermittedCrossDomainPolicies)
	}
	if h.XXSSProtection != "1; mode=block" {
		t.Errorf("X-XSS-Protection = %q want 1; mode=block", h.XXSSProtection)
	}
}

// TestBuildSecurityHeaders_PermissionsPolicyTightDefault confirms the
// default Permissions-Policy turns off all the modern powerful APIs
// scanners look for.
func TestBuildSecurityHeaders_PermissionsPolicyTightDefault(t *testing.T) {
	sqlDB, q := openSecurityTestDB(t)
	siteID := "abcdef0123456789abcdef03"
	seedSecuritySite(t, sqlDB, siteID)

	h, _ := BuildSecurityHeaders(context.Background(), q, siteID)
	must := []string{
		"camera=()", "microphone=()", "geolocation=()",
		"payment=()", "usb=()", "interest-cohort=()",
	}
	for _, want := range must {
		if !strings.Contains(h.PermissionsPolicy, want) {
			t.Errorf("Permissions-Policy missing %q: %s", want, h.PermissionsPolicy)
		}
	}
}

// TestRenderNginxConfig_EmitsAllHeaders confirms the nginx writer
// produces add_header lines for every entry in SecurityHeaders, in
// particular the two we just added (auto-emit guard against a future
// refactor that forgets to write one of them).
func TestRenderNginxConfig_EmitsAllHeaders(t *testing.T) {
	sqlDB, q := openSecurityTestDB(t)
	siteID := "abcdef0123456789abcdef04"
	seedSecuritySite(t, sqlDB, siteID)

	dir := t.TempDir()
	if err := RenderNginxConfig(context.Background(), q, siteID, dir); err != nil {
		t.Fatalf("RenderNginxConfig: %v", err)
	}

	body, err := readFileForTest(filepath.Join(dir, "nginx.conf"))
	if err != nil {
		t.Fatalf("read nginx.conf: %v", err)
	}
	must := []string{
		`add_header Content-Security-Policy "`,
		`add_header Strict-Transport-Security "`,
		`add_header X-Frame-Options "`,
		`add_header X-Content-Type-Options "`,
		`add_header Referrer-Policy "`,
		`add_header Permissions-Policy "`,
		`add_header Cross-Origin-Opener-Policy "`,
		`add_header Cross-Origin-Resource-Policy "`,
		`add_header X-Permitted-Cross-Domain-Policies "none" always;`,
		`add_header X-XSS-Protection "1; mode=block" always;`,
	}
	for _, want := range must {
		if !strings.Contains(body, want) {
			t.Errorf("nginx.conf missing %q", want)
		}
	}
}

// TestRenderSecurityHeaders_HeadersFileEmitsAll mirrors the nginx test
// for the _headers file path (Caddy / Cloudflare Pages / Netlify).
func TestRenderSecurityHeaders_HeadersFileEmitsAll(t *testing.T) {
	sqlDB, q := openSecurityTestDB(t)
	siteID := "abcdef0123456789abcdef05"
	seedSecuritySite(t, sqlDB, siteID)

	dir := t.TempDir()
	if err := RenderSecurityHeaders(context.Background(), q, siteID, dir); err != nil {
		t.Fatalf("RenderSecurityHeaders: %v", err)
	}
	body, err := readFileForTest(filepath.Join(dir, "public", "_headers"))
	if err != nil {
		t.Fatalf("read _headers: %v", err)
	}
	must := []string{
		"Content-Security-Policy:",
		"Permissions-Policy:",
		"X-Permitted-Cross-Domain-Policies: none",
		"X-XSS-Protection: 1; mode=block",
	}
	for _, want := range must {
		if !strings.Contains(body, want) {
			t.Errorf("_headers missing %q", want)
		}
	}
}

func readFileForTest(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
