package middleware

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	_ "modernc.org/sqlite"

	dbpkg "github.com/bright-interaction/slab/internal/db"
	"github.com/bright-interaction/slab/internal/store"
)

// openTestDB sets up a fresh SQLite + applies the embedded schema. Same
// pattern as analyticsdb_test / retention_test so this stays portable.
func openTestDB(t *testing.T) (*sql.DB, *store.Queries) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.sqlite")
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

// seedUserAndSite returns (userID, siteID). The user is created with the
// given role; site_members is NOT auto-populated so each test can decide.
func seedUserAndSite(t *testing.T, sqlDB *sql.DB, role, userID, siteID string) {
	t.Helper()
	if _, err := sqlDB.Exec(
		`INSERT INTO users (id, email, password_hash, name, role) VALUES (?, ?, '', '', ?)`,
		userID, userID+"@example.com", role,
	); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := sqlDB.Exec(
		`INSERT INTO sites (id, name, slug) VALUES (?, ?, ?)`,
		siteID, "Site "+siteID, siteID,
	); err != nil {
		t.Fatalf("seed site: %v", err)
	}
}

func grantMembership(t *testing.T, queries *store.Queries, siteID, userID, role string) {
	t.Helper()
	if err := queries.AddSiteMember(context.Background(), store.AddSiteMemberParams{
		SiteID: siteID,
		UserID: userID,
		Role:   role,
	}); err != nil {
		t.Fatalf("grant membership: %v", err)
	}
}

// runRouted exercises the middleware via a chi router so URLParam
// resolves correctly. Returns the recorded response.
func runRouted(t *testing.T, mw func(http.Handler) http.Handler, user *AuthUser, siteID string) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.With(mw).Get("/api/sites/{siteID}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("granted"))
	})
	req := httptest.NewRequest(http.MethodGet, "/api/sites/"+siteID, nil)
	if user != nil {
		ctx := context.WithValue(req.Context(), UserContextKey, user)
		req = req.WithContext(ctx)
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func TestSiteAccess_UnauthenticatedRejects(t *testing.T) {
	_, q := openTestDB(t)
	rr := runRouted(t, SiteAccessMiddleware(q), nil, "abcdef0123456789abcdef01")
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestSiteAccess_AdminBypassesMembershipCheck(t *testing.T) {
	sqlDB, q := openTestDB(t)
	siteID := "abcdef0123456789abcdef01"
	seedUserAndSite(t, sqlDB, "admin", "admin1", siteID)
	// No membership row, but admin role bypasses.
	user := &AuthUser{ID: "admin1", Role: "admin"}
	rr := runRouted(t, SiteAccessMiddleware(q), user, siteID)
	if rr.Code != http.StatusOK {
		t.Errorf("admin should pass through; status = %d, body=%s", rr.Code, rr.Body.String())
	}
}

func TestSiteAccess_NonMemberRejected(t *testing.T) {
	sqlDB, q := openTestDB(t)
	siteID := "abcdef0123456789abcdef01"
	seedUserAndSite(t, sqlDB, "editor", "alice", siteID)
	// Alice has no site_members row → 403.
	user := &AuthUser{ID: "alice", Role: "editor"}
	rr := runRouted(t, SiteAccessMiddleware(q), user, siteID)
	if rr.Code != http.StatusForbidden {
		t.Errorf("non-member should 403; got %d", rr.Code)
	}
}

func TestSiteAccess_MemberAccepted(t *testing.T) {
	sqlDB, q := openTestDB(t)
	siteID := "abcdef0123456789abcdef01"
	seedUserAndSite(t, sqlDB, "editor", "alice", siteID)
	grantMembership(t, q, siteID, "alice", "editor")
	user := &AuthUser{ID: "alice", Role: "editor"}
	rr := runRouted(t, SiteAccessMiddleware(q), user, siteID)
	if rr.Code != http.StatusOK {
		t.Errorf("member should pass; got %d body=%s", rr.Code, rr.Body.String())
	}
}

// The audit C1 regression guard: a site_members row for site A must NOT
// grant Alice access to site B.
func TestSiteAccess_CrossSiteIsolation(t *testing.T) {
	sqlDB, q := openTestDB(t)
	siteA := "aaaaaaaaaaaaaaaaaaaaaaaa"
	siteB := "bbbbbbbbbbbbbbbbbbbbbbbb"
	seedUserAndSite(t, sqlDB, "editor", "alice", siteA)
	if _, err := sqlDB.Exec(`INSERT INTO sites (id, name, slug) VALUES (?, 'B', 'siteb')`, siteB); err != nil {
		t.Fatalf("seed site B: %v", err)
	}
	grantMembership(t, q, siteA, "alice", "editor")
	user := &AuthUser{ID: "alice", Role: "editor"}

	// Alice → site A: 200
	if rr := runRouted(t, SiteAccessMiddleware(q), user, siteA); rr.Code != http.StatusOK {
		t.Errorf("alice on site A: got %d, want 200", rr.Code)
	}
	// Alice → site B: 403 (no membership row)
	if rr := runRouted(t, SiteAccessMiddleware(q), user, siteB); rr.Code != http.StatusForbidden {
		t.Errorf("alice on site B: got %d, want 403 (cross-site isolation)", rr.Code)
	}
}

func TestSiteAccess_MalformedSiteIDRejects(t *testing.T) {
	_, q := openTestDB(t)
	user := &AuthUser{ID: "alice", Role: "editor"}
	// chi will 404 on empty / slash-bearing strings before middleware
	// runs (no route match), which is fine. We only test cases that
	// chi DOES route through to the middleware: bare strings that
	// match the {siteID} placeholder shape but fail isSafeSiteID.
	for _, bad := range []string{"abc", "abcdef0123456789abcdef01zz", "ZZZZZZZZZZZZZZZZZZZZZZZZ"} {
		rr := runRouted(t, SiteAccessMiddleware(q), user, bad)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("siteID=%q: got %d, want 400", bad, rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "Invalid siteID") {
			t.Errorf("siteID=%q: body should mention Invalid siteID; got %s", bad, rr.Body.String())
		}
	}
}

func TestBackfillSiteMembersForAdmin_Idempotent(t *testing.T) {
	sqlDB, q := openTestDB(t)
	seedUserAndSite(t, sqlDB, "admin", "admin1", "abcdef0123456789abcdef01")
	if _, err := sqlDB.Exec(`INSERT INTO sites (id, name, slug) VALUES ('bbbb000000000000000000bb', 'B', 'b')`); err != nil {
		t.Fatalf("seed site B: %v", err)
	}
	ctx := context.Background()

	// First pass: should grant 2 memberships.
	if err := q.BackfillSiteMembersForAdmin(ctx, "admin1"); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	ids, _ := q.ListSiteIDsForUser(ctx, "admin1")
	if len(ids) != 2 {
		t.Errorf("after backfill: ListSiteIDsForUser = %v, want 2 sites", ids)
	}

	// Second pass: idempotent, count must not double.
	if err := q.BackfillSiteMembersForAdmin(ctx, "admin1"); err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	ids2, _ := q.ListSiteIDsForUser(ctx, "admin1")
	if len(ids2) != 2 {
		t.Errorf("second backfill must be idempotent; got %d rows, want 2", len(ids2))
	}
}

// TestSiteAccess_WorkspaceIsolation is the audit-#5 regression guard for
// the multi-tenant path (sites carrying a real workspace_id). It proves:
// a member of the site's own workspace passes; an outsider with neither a
// site grant nor workspace membership is denied; and a global platform
// admin who is NOT in the site's workspace still passes but is audited
// (matching WorkspaceAccessMiddleware). Pre-fix, any user.Role=="admin"
// silently read/mutated every tenant's site with no membership and no log.
func TestSiteAccess_WorkspaceIsolation(t *testing.T) {
	sqlDB, q := openTestDB(t)
	ctx := context.Background()
	wsA := "aaaa1111aaaa1111aaaa1111"
	wsB := "bbbb2222bbbb2222bbbb2222"
	siteB := "cccc3333cccc3333cccc3333"
	for _, ws := range []string{wsA, wsB} {
		if _, err := sqlDB.Exec(`INSERT INTO workspaces (id, name, slug) VALUES (?, ?, ?)`, ws, ws, ws); err != nil {
			t.Fatalf("seed workspace %s: %v", ws, err)
		}
	}
	if _, err := sqlDB.Exec(`INSERT INTO sites (id, workspace_id, name, slug) VALUES (?, ?, 'B', 'site-b')`, siteB, wsB); err != nil {
		t.Fatalf("seed site B: %v", err)
	}
	mkUser := func(id, role string) {
		if _, err := sqlDB.Exec(`INSERT INTO users (id, email, password_hash, name, role) VALUES (?, ?, '', '', ?)`, id, id+"@e.com", role); err != nil {
			t.Fatalf("seed user %s: %v", id, err)
		}
	}
	mkUser("memberb", "editor")
	mkUser("outsider", "editor")
	mkUser("opsadmin", "admin")
	if err := q.AddWorkspaceMember(ctx, store.AddWorkspaceMemberParams{WorkspaceID: wsB, UserID: "memberb", Role: "member"}); err != nil {
		t.Fatalf("add ws member: %v", err)
	}
	if err := q.AddWorkspaceMember(ctx, store.AddWorkspaceMemberParams{WorkspaceID: wsA, UserID: "opsadmin", Role: "owner"}); err != nil {
		t.Fatalf("add ws admin member: %v", err)
	}

	// (1) Member of the site's own workspace passes.
	if rr := runRouted(t, SiteAccessMiddleware(q), &AuthUser{ID: "memberb", Role: "editor"}, siteB); rr.Code != http.StatusOK {
		t.Errorf("workspace member on own site: got %d, want 200", rr.Code)
	}
	// (2) Outsider (non-admin, no site grant, not a workspace member) denied.
	if rr := runRouted(t, SiteAccessMiddleware(q), &AuthUser{ID: "outsider", Role: "editor"}, siteB); rr.Code != http.StatusForbidden {
		t.Errorf("outsider on foreign site: got %d, want 403 (cross-tenant isolation)", rr.Code)
	}
	if rows, _ := q.ListAuditLogGlobal(ctx, 10); len(rows) != 0 {
		t.Fatalf("no audit row expected before admin cross-access; got %d", len(rows))
	}
	// (3) Platform admin not in the site's workspace passes BUT is audited.
	if rr := runRouted(t, SiteAccessMiddleware(q), &AuthUser{ID: "opsadmin", Role: "admin"}, siteB); rr.Code != http.StatusOK {
		t.Errorf("platform admin cross-workspace: got %d, want 200 (audited)", rr.Code)
	}
	rows, err := q.ListAuditLogGlobal(ctx, 10)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(rows) != 1 || rows[0].Action != "cross_workspace_site_admin" {
		got := ""
		if len(rows) > 0 {
			got = rows[0].Action
		}
		t.Fatalf("want exactly 1 cross_workspace_site_admin audit row; got %d rows, action0=%q", len(rows), got)
	}
	if rows[0].ResourceID != wsB {
		t.Errorf("audit ResourceID=%q, want site's workspace %s", rows[0].ResourceID, wsB)
	}
}

// TestSiteAccess_DeletedWorkspaceRevokesItsSites pins the half of the
// soft-delete invariant that was missing.
//
// WorkspaceAccessMiddleware began refusing a workspace with status='deleted',
// but this middleware's workspace-membership branch did not, so the same user
// at the same moment got 404 on GET /api/workspaces/{id} and 200 with data on
// GET /api/sites/{id}: across roughly a hundred site routes including form
// submissions and other PII, plus the agent-key surface. The admin UI told the
// operator "Everyone loses access to it."
func TestSiteAccess_DeletedWorkspaceRevokesItsSites(t *testing.T) {
	sqlDB, q := openTestDB(t)
	const wsID = "aaaaaaaaaaaaaaaaaaaaaaaa"
	const userID = "u-ws-member"
	const siteID = "bbbbbbbbbbbbbbbbbbbbbbbb"

	seedUserAndSite(t, sqlDB, "editor", userID, siteID)
	if _, err := sqlDB.Exec(
		`INSERT INTO workspaces (id, name, slug, plan, region, status) VALUES (?, 'WS', 'ws-del', 'oss', 'eu', 'active')`,
		wsID,
	); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	if _, err := sqlDB.Exec(`UPDATE sites SET workspace_id = ? WHERE id = ?`, wsID, siteID); err != nil {
		t.Fatalf("attach site to workspace: %v", err)
	}
	if err := q.AddWorkspaceMember(context.Background(), store.AddWorkspaceMemberParams{
		WorkspaceID: wsID, UserID: userID, Role: "member",
	}); err != nil {
		t.Fatalf("seed workspace member: %v", err)
	}
	user := &AuthUser{ID: userID, Role: "editor"}

	// Access is granted purely by workspace membership while the workspace lives.
	if rr := runRouted(t, SiteAccessMiddleware(q), user, siteID); rr.Code != http.StatusOK {
		t.Fatalf("active workspace: status=%d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}

	if _, err := sqlDB.Exec(`UPDATE workspaces SET status = 'deleted' WHERE id = ?`, wsID); err != nil {
		t.Fatalf("soft-delete workspace: %v", err)
	}

	if rr := runRouted(t, SiteAccessMiddleware(q), user, siteID); rr.Code != http.StatusForbidden {
		t.Fatalf("deleted workspace still serves its sites: status=%d, want 403 (body=%s)", rr.Code, rr.Body.String())
	}

	// A platform admin keeps the audited break-glass path, so a mistaken
	// delete stays recoverable by an operator.
	admin := &AuthUser{ID: "u-admin", Role: "admin"}
	if _, err := sqlDB.Exec(
		`INSERT INTO users (id, email, password_hash, name, role) VALUES ('u-admin', 'a@example.com', '', '', 'admin')`,
	); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	if rr := runRouted(t, SiteAccessMiddleware(q), admin, siteID); rr.Code != http.StatusOK {
		t.Fatalf("platform admin lost break-glass access: status=%d, want 200", rr.Code)
	}
}
