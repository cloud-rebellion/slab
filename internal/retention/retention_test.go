package retention

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	dbpkg "github.com/bright-interaction/slab/internal/db"
	"github.com/bright-interaction/slab/internal/store"
)

// openTestDB spins up a fresh SQLite DB in a temp dir, applies the schema,
// and returns the sqlc Queries handle. Same pattern handlers/sites_seed_test.go
// uses so retention tests stay aligned with the rest of the suite.
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

// seedSite inserts a minimal sites row that satisfies the FK constraint on
// visit_events / visit_sessions / visit_engagement / consent_records. Returns
// the site_id so the caller can seed dependent rows.
func seedSite(t *testing.T, sqlDB *sql.DB, id, slug string) {
	t.Helper()
	_, err := sqlDB.Exec(`INSERT INTO sites (id, name, slug) VALUES (?, ?, ?)`, id, "Test "+id, slug)
	if err != nil {
		t.Fatalf("seed site %s: %v", id, err)
	}
}

func mustExec(t *testing.T, sqlDB *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := sqlDB.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
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

// TestSweepPurgesOldRowsKeepsRecent is the central guarantee: a sweep with
// site retention 30 days drops only rows older than the cutoff and leaves
// recent rows + identified sessions untouched.
func TestSweepPurgesOldRowsKeepsRecent(t *testing.T) {
	sqlDB, queries := openTestDB(t)
	ctx := context.Background()

	siteID := "abcdef0123456789abcdef01"
	seedSite(t, sqlDB, siteID, "alpha")

	// Per-site retention windows: short for the test.
	mustExec(t, sqlDB, `INSERT INTO site_settings (id, site_id, category, key, value)
		VALUES (?, ?, 'general', 'analytics_retention_days', '30')`, "s1", siteID)
	mustExec(t, sqlDB, `INSERT INTO site_settings (id, site_id, category, key, value)
		VALUES (?, ?, 'general', 'consent_retention_days', '30')`, "s2", siteID)
	mustExec(t, sqlDB, `INSERT INTO site_settings (id, site_id, category, key, value)
		VALUES (?, ?, 'general', 'engagement_retention_days', '14')`, "s3", siteID)

	now := time.Now().UTC()
	old := now.AddDate(0, 0, -45).Format(time.RFC3339)           // 45 days back, well past 30/14
	recent := now.AddDate(0, 0, -7).Format(time.RFC3339)         // last week, inside both windows
	oldEngagement := now.AddDate(0, 0, -20).Format(time.RFC3339) // past 14, inside 30
	oldMs := now.AddDate(0, 0, -45).UnixMilli()
	recentMs := now.AddDate(0, 0, -7).UnixMilli()

	// visit_events: one old, one recent.
	mustExec(t, sqlDB, `INSERT INTO visit_events (id, site_id, fingerprint, session_id, path, referer, status, ms, ts)
		VALUES (?, ?, ?, '', '/', '', 200, 0, ?)`, "ev_old", siteID, "fp1", old)
	mustExec(t, sqlDB, `INSERT INTO visit_events (id, site_id, fingerprint, session_id, path, referer, status, ms, ts)
		VALUES (?, ?, ?, '', '/', '', 200, 0, ?)`, "ev_new", siteID, "fp1", recent)

	// visit_engagement: past the 14-day engagement window vs inside it.
	mustExec(t, sqlDB, `INSERT INTO visit_engagement (id, site_id, fingerprint, path, ts)
		VALUES (?, ?, ?, '/', ?)`, "eng_old", siteID, "fp1", oldEngagement)
	mustExec(t, sqlDB, `INSERT INTO visit_engagement (id, site_id, fingerprint, path, ts)
		VALUES (?, ?, ?, '/', ?)`, "eng_new", siteID, "fp1", recent)

	// visit_sessions: anonymous-old (should drop), anonymous-recent (keep),
	// identified-old (KEEP because identified_at != '').
	mustExec(t, sqlDB, `INSERT INTO visit_sessions (id, site_id, fingerprint, started_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?)`, "sess_anon_old", siteID, "fp_anon_old", old, old)
	mustExec(t, sqlDB, `INSERT INTO visit_sessions (id, site_id, fingerprint, started_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?)`, "sess_anon_new", siteID, "fp_anon_new", recent, recent)
	mustExec(t, sqlDB, `INSERT INTO visit_sessions (id, site_id, fingerprint, started_at, last_seen_at, identified_at)
		VALUES (?, ?, ?, ?, ?, ?)`, "sess_id_old", siteID, "fp_id_old", old, old, old)

	// consent_records: one old, one recent.
	mustExec(t, sqlDB, `INSERT INTO consent_records (id, site_id, domain, ip_hash, consent_method, created_at)
		VALUES (?, ?, ?, ?, 'accept-all', ?)`, "cr_old", siteID, "example.com", "h1", oldMs)
	mustExec(t, sqlDB, `INSERT INTO consent_records (id, site_id, domain, ip_hash, consent_method, created_at)
		VALUES (?, ?, ?, ?, 'reject-all', ?)`, "cr_new", siteID, "example.com", "h2", recentMs)

	// consent_salts: 60 days back so it's past the 30-day salt retention.
	mustExec(t, sqlDB, `INSERT INTO consent_salts (day_utc, salt) VALUES (?, ?)`,
		now.AddDate(0, 0, -60).Format("2006-01-02"), "old_salt")
	mustExec(t, sqlDB, `INSERT INTO consent_salts (day_utc, salt) VALUES (?, ?)`,
		now.Format("2006-01-02"), "today_salt")

	mgr := NewManager(queries, sqlDB)
	res, err := mgr.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("expected no errors, got %v", res.Errors)
	}
	if res.SitesScanned != 1 {
		t.Errorf("SitesScanned = %d, want 1", res.SitesScanned)
	}

	// visit_events: only the old row should be gone.
	if got := mustCount(t, sqlDB, "SELECT COUNT(*) FROM visit_events"); got != 1 {
		t.Errorf("visit_events count = %d, want 1 (only recent kept)", got)
	}
	if got := mustCount(t, sqlDB, "SELECT COUNT(*) FROM visit_events WHERE id = 'ev_new'"); got != 1 {
		t.Error("recent visit_event was wrongly purged")
	}
	if res.VisitEvents != 1 {
		t.Errorf("res.VisitEvents = %d, want 1", res.VisitEvents)
	}

	// visit_engagement: the 20-day-old row exceeds the 14-day window, so it
	// should be gone.
	if got := mustCount(t, sqlDB, "SELECT COUNT(*) FROM visit_engagement WHERE id = 'eng_old'"); got != 0 {
		t.Error("old engagement was not purged")
	}
	if got := mustCount(t, sqlDB, "SELECT COUNT(*) FROM visit_engagement WHERE id = 'eng_new'"); got != 1 {
		t.Error("recent engagement was wrongly purged")
	}

	// visit_sessions: anonymous-old purged, anonymous-recent kept,
	// identified-old kept (identified survives regardless of age).
	if got := mustCount(t, sqlDB, "SELECT COUNT(*) FROM visit_sessions WHERE id = 'sess_anon_old'"); got != 0 {
		t.Error("old anonymous session was not purged")
	}
	if got := mustCount(t, sqlDB, "SELECT COUNT(*) FROM visit_sessions WHERE id = 'sess_anon_new'"); got != 1 {
		t.Error("recent anonymous session was wrongly purged")
	}
	if got := mustCount(t, sqlDB, "SELECT COUNT(*) FROM visit_sessions WHERE id = 'sess_id_old'"); got != 1 {
		t.Error("identified session was wrongly purged (must survive any age)")
	}

	// consent_records: old purged, recent kept.
	if got := mustCount(t, sqlDB, "SELECT COUNT(*) FROM consent_records WHERE id = 'cr_old'"); got != 0 {
		t.Error("old consent record was not purged")
	}
	if got := mustCount(t, sqlDB, "SELECT COUNT(*) FROM consent_records WHERE id = 'cr_new'"); got != 1 {
		t.Error("recent consent record was wrongly purged")
	}

	// consent_salts: 60-day-old salt purged, today's kept.
	if got := mustCount(t, sqlDB, "SELECT COUNT(*) FROM consent_salts WHERE salt = 'old_salt'"); got != 0 {
		t.Error("old salt was not purged")
	}
	if got := mustCount(t, sqlDB, "SELECT COUNT(*) FROM consent_salts WHERE salt = 'today_salt'"); got != 1 {
		t.Error("today's salt was wrongly purged")
	}

	// Per-site result reflects the 1 visit_event + 1 engagement + 1 session +
	// 1 consent that were dropped on this single site.
	if len(res.PerSite) != 1 {
		t.Fatalf("PerSite len = %d, want 1", len(res.PerSite))
	}
	ps := res.PerSite[0]
	if ps.SiteID != siteID {
		t.Errorf("PerSite[0].SiteID = %q, want %q", ps.SiteID, siteID)
	}
	if ps.AnalyticsDays != 30 || ps.ConsentDays != 30 || ps.EngagementDays != 14 {
		t.Errorf("retention windows wrong: analytics=%d consent=%d engagement=%d",
			ps.AnalyticsDays, ps.ConsentDays, ps.EngagementDays)
	}
}

// TestSweepFallsBackToDefaults confirms that a site without retention
// settings rows uses DefaultAnalyticsDays / DefaultConsentDays / DefaultEngagementDays.
func TestSweepFallsBackToDefaults(t *testing.T) {
	sqlDB, queries := openTestDB(t)
	ctx := context.Background()
	siteID := "fffffffffffffffffffffff0"
	seedSite(t, sqlDB, siteID, "defaults")

	mgr := NewManager(queries, sqlDB)
	res, err := mgr.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(res.PerSite) != 1 {
		t.Fatalf("PerSite len = %d", len(res.PerSite))
	}
	ps := res.PerSite[0]
	if ps.AnalyticsDays != DefaultAnalyticsDays {
		t.Errorf("analytics days = %d, want %d", ps.AnalyticsDays, DefaultAnalyticsDays)
	}
	if ps.ConsentDays != DefaultConsentDays {
		t.Errorf("consent days = %d, want %d", ps.ConsentDays, DefaultConsentDays)
	}
	if ps.EngagementDays != DefaultEngagementDays {
		t.Errorf("engagement days = %d, want %d", ps.EngagementDays, DefaultEngagementDays)
	}
}

// TestSweepClampsBadRetentionValues confirms that out-of-range or non-numeric
// settings values fall back to defaults instead of producing nonsense cutoffs.
func TestSweepClampsBadRetentionValues(t *testing.T) {
	sqlDB, queries := openTestDB(t)
	ctx := context.Background()
	siteID := "fffffffffffffffffffffff1"
	seedSite(t, sqlDB, siteID, "clamp")

	// Three pathological values: 0 (below min), 99999 (above max), "abc"
	// (unparseable). All three must fall through to the defaults.
	mustExec(t, sqlDB, `INSERT INTO site_settings (id, site_id, category, key, value)
		VALUES (?, ?, 'general', 'analytics_retention_days', '0')`, "x1", siteID)
	mustExec(t, sqlDB, `INSERT INTO site_settings (id, site_id, category, key, value)
		VALUES (?, ?, 'general', 'consent_retention_days', '99999')`, "x2", siteID)
	mustExec(t, sqlDB, `INSERT INTO site_settings (id, site_id, category, key, value)
		VALUES (?, ?, 'general', 'engagement_retention_days', 'abc')`, "x3", siteID)

	mgr := NewManager(queries, sqlDB)
	res, err := mgr.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	ps := res.PerSite[0]
	if ps.AnalyticsDays != DefaultAnalyticsDays {
		t.Errorf("clamped 0 -> got %d, want default %d", ps.AnalyticsDays, DefaultAnalyticsDays)
	}
	if ps.ConsentDays != DefaultConsentDays {
		t.Errorf("clamped 99999 -> got %d, want default %d", ps.ConsentDays, DefaultConsentDays)
	}
	if ps.EngagementDays != DefaultEngagementDays {
		t.Errorf("clamped abc -> got %d, want default %d", ps.EngagementDays, DefaultEngagementDays)
	}
}

// TestSweepIsolatesSites confirms a sweep across two sites with different
// retention configs purges each independently. Site A keeps 30 days, site B
// keeps 365; a row that's 60 days old belongs only to A's purge.
func TestSweepIsolatesSites(t *testing.T) {
	sqlDB, queries := openTestDB(t)
	ctx := context.Background()
	siteA := "aaaaaaaaaaaaaaaaaaaaaaaa"
	siteB := "bbbbbbbbbbbbbbbbbbbbbbbb"
	seedSite(t, sqlDB, siteA, "alpha")
	seedSite(t, sqlDB, siteB, "beta")

	mustExec(t, sqlDB, `INSERT INTO site_settings (id, site_id, category, key, value)
		VALUES (?, ?, 'general', 'analytics_retention_days', '30')`, "sa", siteA)
	mustExec(t, sqlDB, `INSERT INTO site_settings (id, site_id, category, key, value)
		VALUES (?, ?, 'general', 'analytics_retention_days', '365')`, "sb", siteB)

	old := time.Now().UTC().AddDate(0, 0, -60).Format(time.RFC3339)
	mustExec(t, sqlDB, `INSERT INTO visit_events (id, site_id, fingerprint, session_id, path, referer, status, ms, ts)
		VALUES (?, ?, 'fp', '', '/', '', 200, 0, ?)`, "ev_a", siteA, old)
	mustExec(t, sqlDB, `INSERT INTO visit_events (id, site_id, fingerprint, session_id, path, referer, status, ms, ts)
		VALUES (?, ?, 'fp', '', '/', '', 200, 0, ?)`, "ev_b", siteB, old)

	mgr := NewManager(queries, sqlDB)
	if _, err := mgr.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if got := mustCount(t, sqlDB, "SELECT COUNT(*) FROM visit_events WHERE id = 'ev_a'"); got != 0 {
		t.Error("site A's old event was not purged (retention 30, age 60)")
	}
	if got := mustCount(t, sqlDB, "SELECT COUNT(*) FROM visit_events WHERE id = 'ev_b'"); got != 1 {
		t.Error("site B's event was wrongly purged (retention 365, age 60 -- should survive)")
	}
}

// TestStartAndStop confirms the long-running goroutine exits within the
// shutdown deadline. Uses a short PauseAfterStartup substitute by calling
// Start then Stop immediately so the goroutine is still in the initial
// select{} when cancellation arrives.
func TestStartAndStop(t *testing.T) {
	sqlDB, queries := openTestDB(t)
	mgr := NewManager(queries, sqlDB)
	mgr.Start(context.Background())

	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	mgr.Stop(stopCtx)
	if stopCtx.Err() != nil {
		t.Errorf("Stop did not return before deadline: %v", stopCtx.Err())
	}
}

// TestOrphanWorkspaceSweep is the audit H8 regression guard: a workspace
// directory whose siteID is no longer in the sites table must be removed
// by the daily sweep, but a directory belonging to a live site must
// survive untouched.
func TestOrphanWorkspaceSweep(t *testing.T) {
	sqlDB, queries := openTestDB(t)
	ctx := context.Background()

	dataDir := t.TempDir()
	wsRoot := filepath.Join(dataDir, "workspaces")
	if err := os.MkdirAll(wsRoot, 0o755); err != nil {
		t.Fatalf("mkdir workspaces: %v", err)
	}

	// Two siteID-shaped dirs: one with a live row, one orphan.
	live := "abcdef0123456789abcdef01"
	orphan := "ffffffffffffffffffffffff"
	if _, err := sqlDB.Exec(`INSERT INTO sites (id, name, slug) VALUES (?, ?, ?)`, live, "Live", "live"); err != nil {
		t.Fatalf("seed live site: %v", err)
	}
	for _, dir := range []string{live, orphan} {
		if err := os.MkdirAll(filepath.Join(wsRoot, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	// Plus a non-siteID-shaped dir that must be left alone (defensive).
	if err := os.MkdirAll(filepath.Join(wsRoot, "scratch"), 0o755); err != nil {
		t.Fatalf("mkdir scratch: %v", err)
	}

	mgr := NewManager(queries, sqlDB)
	mgr.SetDataDir(dataDir)

	removed, err := mgr.sweepOrphanWorkspaces(ctx, dataDir)
	if err != nil {
		t.Fatalf("sweepOrphanWorkspaces: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1 (only the orphan)", removed)
	}

	// Live dir survives.
	if _, err := os.Stat(filepath.Join(wsRoot, live)); err != nil {
		t.Errorf("live workspace must survive sweep: %v", err)
	}
	// Orphan is gone.
	if _, err := os.Stat(filepath.Join(wsRoot, orphan)); !os.IsNotExist(err) {
		t.Errorf("orphan workspace must be removed; stat err = %v", err)
	}
	// Scratch dir untouched (not siteID-shaped).
	if _, err := os.Stat(filepath.Join(wsRoot, "scratch")); err != nil {
		t.Errorf("non-siteID 'scratch' dir must NOT be touched: %v", err)
	}
}

func TestOrphanWorkspaceSweep_NoWorkspacesDir(t *testing.T) {
	sqlDB, queries := openTestDB(t)
	mgr := NewManager(queries, sqlDB)
	dataDir := t.TempDir() // empty dir; no workspaces/ subdir
	if n, err := mgr.sweepOrphanWorkspaces(context.Background(), dataDir); err != nil || n != 0 {
		t.Errorf("missing workspaces dir should be a no-op; got n=%d err=%v", n, err)
	}
}
