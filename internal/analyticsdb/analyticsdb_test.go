package analyticsdb

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	dbpkg "github.com/bright-interaction/slab/internal/db"
)

// seedTestDB writes a complete fixture mirroring the production schema:
// one site with 3 visitors who all hit / and /about, two of whom consent
// (one accepts, one rejects), one has GPC, plus engagement rows. The
// stitched analytics queries are then validated against known answers.
func seedTestDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.sqlite")
	sqlDB, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if _, err := sqlDB.Exec(string(dbpkg.Schema)); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })

	siteID := "abcdef0123456789abcdef01"
	if _, err := sqlDB.Exec(`INSERT INTO sites (id, name, slug) VALUES (?, ?, ?)`,
		siteID, "Test", "test-site"); err != nil {
		t.Fatalf("seed site: %v", err)
	}

	now := time.Now().UTC()
	t0 := now.Add(-2 * time.Hour)
	t1 := now.Add(-90 * time.Minute)
	t2 := now.Add(-60 * time.Minute)
	t3 := now.Add(-30 * time.Minute)

	rfc := func(tt time.Time) string { return tt.Format(time.RFC3339) }

	// Three visitors with different consent paths:
	//   fp_a: visits /, /about, accepts after 30 min
	//   fp_b: visits /, /pricing, rejects after 60 min
	//   fp_c: visits /, never consents (banner abandoner)
	for _, ev := range []struct {
		id, fp, path, ts string
	}{
		{"e1", "fp_a", "/", rfc(t0)},
		{"e2", "fp_a", "/about", rfc(t1)},
		{"e3", "fp_b", "/", rfc(t0)},
		{"e4", "fp_b", "/pricing", rfc(t1)},
		{"e5", "fp_c", "/", rfc(t1)},
	} {
		if _, err := sqlDB.Exec(`INSERT INTO visit_events (id, site_id, fingerprint, session_id, path, referer, status, ms, ts)
			VALUES (?, ?, ?, '', ?, '', 200, 0, ?)`, ev.id, siteID, ev.fp, ev.path, ev.ts); err != nil {
			t.Fatalf("seed visit_event %s: %v", ev.id, err)
		}
	}

	// Engagement: fp_a is engaged (high scroll), fp_b is not, fp_c is not
	for _, eng := range []struct {
		id, fp, path string
		scrollPct    int
		timeOnPageMs int
		ts           string
	}{
		{"eng1", "fp_a", "/about", 80, 45000, rfc(t1)},
		{"eng2", "fp_b", "/pricing", 20, 5000, rfc(t1)},
		{"eng3", "fp_c", "/", 10, 2000, rfc(t1)},
	} {
		if _, err := sqlDB.Exec(`INSERT INTO visit_engagement (id, site_id, fingerprint, path, ts, max_scroll_pct, time_on_page_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, eng.id, siteID, eng.fp, eng.path, eng.ts, eng.scrollPct, eng.timeOnPageMs); err != nil {
			t.Fatalf("seed engagement %s: %v", eng.id, err)
		}
	}

	// Consent records: fp_a accept-all at t2, fp_b reject-all at t3.
	// fp_c never consents.
	for _, cr := range []struct {
		id, fp, method string
		gpc            int
		ts             time.Time
	}{
		{"c1", "fp_a", "accept-all", 0, t2},
		{"c2", "fp_b", "reject-all", 0, t3},
	} {
		if _, err := sqlDB.Exec(`INSERT INTO consent_records (id, site_id, session_id, fingerprint, domain, ip_hash, consent_method, consent_version, categories_json, gpc_active, created_at)
			VALUES (?, ?, '', ?, 'test.example', 'h', ?, 1, '{}', ?, ?)`,
			cr.id, siteID, cr.fp, cr.method, cr.gpc, cr.ts.UnixMilli()); err != nil {
			t.Fatalf("seed consent_record %s: %v", cr.id, err)
		}
	}

	// One identified visit_session (fp_a) so the IdentifiedVisitors
	// counter has something to count.
	if _, err := sqlDB.Exec(`INSERT INTO visit_sessions (id, site_id, fingerprint, started_at, last_seen_at, identified_at)
		VALUES (?, ?, ?, ?, ?, ?)`, "vs1", siteID, "fp_a", rfc(t0), rfc(t3), rfc(t3)); err != nil {
		t.Fatalf("seed identified session: %v", err)
	}
	// Anonymous session for fp_b + fp_c so the visitor count works
	if _, err := sqlDB.Exec(`INSERT INTO visit_sessions (id, site_id, fingerprint, started_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?)`, "vs2", siteID, "fp_b", rfc(t0), rfc(t3)); err != nil {
		t.Fatalf("seed anon session: %v", err)
	}

	// Important: close the SQLite handle before DuckDB ATTACHes it
	// READ_ONLY. SQLite's WAL mode is happy with concurrent readers but
	// some shared-cache configs trip on overlapping process opens; the
	// safer pattern in tests is to write fixtures, close, then ATTACH.
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close sqlite: %v", err)
	}
	return dbPath, sqlDB
}

// openManager opens a DuckDB ATTACH against the seeded SQLite file and
// registers the cleanup hook.
func openManager(t *testing.T, dbPath string) *Manager {
	t.Helper()
	mgr, err := Open(context.Background(), Config{SQLitePath: dbPath})
	if err != nil {
		t.Fatalf("Open analyticsdb: %v", err)
	}
	t.Cleanup(func() { mgr.Close() })
	return mgr
}

func widePastWindow() TimeRange {
	now := time.Now().UTC()
	return NewTimeRange(now.Add(-7*24*time.Hour), now.Add(time.Hour))
}

func TestFunnelKPIs_StitchedFromAllSources(t *testing.T) {
	dbPath, _ := seedTestDB(t)
	mgr := openManager(t, dbPath)

	k, err := mgr.FunnelKPIs(context.Background(), "abcdef0123456789abcdef01", widePastWindow())
	if err != nil {
		t.Fatalf("FunnelKPIs: %v", err)
	}

	if k.UniqueVisitors != 3 {
		t.Errorf("UniqueVisitors = %d, want 3", k.UniqueVisitors)
	}
	if k.ConsentingVisitors != 2 {
		t.Errorf("ConsentingVisitors = %d, want 2", k.ConsentingVisitors)
	}
	if k.AcceptAllVisitors != 1 {
		t.Errorf("AcceptAllVisitors = %d, want 1", k.AcceptAllVisitors)
	}
	if k.RejectAllVisitors != 1 {
		t.Errorf("RejectAllVisitors = %d, want 1", k.RejectAllVisitors)
	}
	if k.IdentifiedVisitors != 1 {
		t.Errorf("IdentifiedVisitors = %d, want 1", k.IdentifiedVisitors)
	}
	if k.AbandonedBannerVisitors != 1 {
		t.Errorf("AbandonedBannerVisitors = %d, want 1 (fp_c never consented)", k.AbandonedBannerVisitors)
	}
}

func TestPreConsentTrafficShare(t *testing.T) {
	dbPath, _ := seedTestDB(t)
	mgr := openManager(t, dbPath)

	share, err := mgr.PreConsentTrafficShare(context.Background(), "abcdef0123456789abcdef01", widePastWindow())
	if err != nil {
		t.Fatalf("PreConsentTrafficShare: %v", err)
	}

	// Total pageviews = 5 (e1..e5).
	if share.Total != 5 {
		t.Errorf("Total = %d, want 5", share.Total)
	}
	// fp_a's two pageviews (t0, t1) both happened BEFORE accept at t2,
	// so they count as before_or_never. fp_b's two pageviews (t0, t1)
	// both happened BEFORE reject at t3, also before_or_never.
	// fp_c's one pageview never consented = before_or_never.
	// All 5 pageviews are before-or-never in this fixture; that's by
	// design and tells the tenant 100% of their tracking is in the gap.
	if share.BeforeOrNever != 5 {
		t.Errorf("BeforeOrNever = %d, want 5 (all pageviews were before consent in fixture)", share.BeforeOrNever)
	}
	if share.AfterConsented != 0 {
		t.Errorf("AfterConsented = %d, want 0", share.AfterConsented)
	}
}

func TestTimeToConsent(t *testing.T) {
	dbPath, _ := seedTestDB(t)
	mgr := openManager(t, dbPath)

	t2c, err := mgr.TimeToConsent(context.Background(), "abcdef0123456789abcdef01", widePastWindow())
	if err != nil {
		t.Fatalf("TimeToConsent: %v", err)
	}

	// Only fp_a + fp_b have both events and consent. So sample size = 2.
	if t2c.SampleSize != 2 {
		t.Errorf("SampleSize = %d, want 2", t2c.SampleSize)
	}
	// fp_a delta = t2 - t0 = ~60 min = 3600s
	// fp_b delta = t3 - t0 = ~90 min = 5400s
	// Mean ≈ 4500s. Median between two values is the average ~4500s
	// (DuckDB's quantile_cont interpolates).
	if t2c.MeanSeconds < 3500 || t2c.MeanSeconds > 5500 {
		t.Errorf("MeanSeconds = %f, want roughly 4500", t2c.MeanSeconds)
	}
	if t2c.P50Seconds <= 0 {
		t.Errorf("P50Seconds = %f, expected > 0", t2c.P50Seconds)
	}
}

func TestEngagedAcceptRate_HigherForEngagedCohort(t *testing.T) {
	dbPath, _ := seedTestDB(t)
	mgr := openManager(t, dbPath)

	er, err := mgr.EngagedAcceptRate(context.Background(), "abcdef0123456789abcdef01", widePastWindow())
	if err != nil {
		t.Fatalf("EngagedAcceptRate: %v", err)
	}

	// fp_a is engaged (80% scroll, 45s time-on-page) AND accepted.
	// fp_b is NOT engaged (20% scroll, 5s) and rejected.
	// fp_c is NOT engaged (10%, 2s) and didn't consent at all.
	if er.EngagedVisitors != 1 {
		t.Errorf("EngagedVisitors = %d, want 1 (fp_a)", er.EngagedVisitors)
	}
	if er.EngagedAcceptedVisitors != 1 {
		t.Errorf("EngagedAccepted = %d, want 1 (fp_a accepted)", er.EngagedAcceptedVisitors)
	}
	if er.EngagedAcceptRatePct != 100.0 {
		t.Errorf("EngagedAcceptRatePct = %f, want 100.0", er.EngagedAcceptRatePct)
	}
	if er.NotEngagedVisitors != 2 {
		t.Errorf("NotEngagedVisitors = %d, want 2", er.NotEngagedVisitors)
	}
	if er.NotEngagedAcceptedCount != 0 {
		t.Errorf("NotEngagedAccepted = %d, want 0", er.NotEngagedAcceptedCount)
	}
}

func TestConsentDailySplit(t *testing.T) {
	dbPath, _ := seedTestDB(t)
	mgr := openManager(t, dbPath)

	rows, err := mgr.ConsentDailySplit(context.Background(), "abcdef0123456789abcdef01", widePastWindow())
	if err != nil {
		t.Fatalf("ConsentDailySplit: %v", err)
	}
	if len(rows) == 0 {
		t.Fatalf("expected at least 1 row, got 0")
	}
	// Total accepts + rejects across all days = 1 + 1 = 2
	var totalAccept, totalReject int64
	for _, r := range rows {
		totalAccept += r.AcceptAll
		totalReject += r.RejectAll
	}
	if totalAccept != 1 {
		t.Errorf("total accept-all = %d, want 1", totalAccept)
	}
	if totalReject != 1 {
		t.Errorf("total reject-all = %d, want 1", totalReject)
	}
}

func TestVisitorJourney_StitchesAllStreamsForOneFingerprint(t *testing.T) {
	dbPath, _ := seedTestDB(t)
	mgr := openManager(t, dbPath)

	steps, err := mgr.VisitorJourney(context.Background(), "abcdef0123456789abcdef01", "fp_a", widePastWindow())
	if err != nil {
		t.Fatalf("VisitorJourney: %v", err)
	}
	// fp_a has 2 pageviews + 1 engagement + 1 consent = 4 steps.
	if len(steps) != 4 {
		t.Errorf("step count = %d, want 4", len(steps))
		for i, s := range steps {
			t.Logf("step[%d]: kind=%s path=%s method=%s ts_ms=%d", i, s.Kind, s.Path, s.Method, s.TimestampMs)
		}
	}

	// Steps must be ordered by ts ascending.
	for i := 1; i < len(steps); i++ {
		if steps[i].TimestampMs < steps[i-1].TimestampMs {
			t.Errorf("step[%d] is older than step[%d]; ordering violated", i, i-1)
		}
	}

	// Last step should be the consent.
	if len(steps) > 0 && steps[len(steps)-1].Kind != "consent" {
		t.Errorf("last step kind = %q, want consent", steps[len(steps)-1].Kind)
	}
}

func TestTopAcceptingPages_RespectsMinVisitorThreshold(t *testing.T) {
	dbPath, _ := seedTestDB(t)
	mgr := openManager(t, dbPath)

	// Each entry path in the fixture has fewer than 10 visitors, so
	// the HAVING COUNT(*) >= 10 filter should produce zero rows.
	rows, err := mgr.TopAcceptingPages(context.Background(), "abcdef0123456789abcdef01", widePastWindow(), 10)
	if err != nil {
		t.Fatalf("TopAcceptingPages: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows, want 0 (fixture has < 10 visitors per path)", len(rows))
	}
}

func TestHealthy_AfterOpen(t *testing.T) {
	dbPath, _ := seedTestDB(t)
	mgr := openManager(t, dbPath)

	if !mgr.Healthy(context.Background()) {
		t.Error("Healthy() returned false on freshly-opened manager")
	}
}

func TestClose_Idempotent(t *testing.T) {
	dbPath, _ := seedTestDB(t)
	mgr, err := Open(context.Background(), Config{SQLitePath: dbPath})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := mgr.Close(); err != nil {
		t.Errorf("first Close failed: %v", err)
	}
	if err := mgr.Close(); err != nil {
		t.Errorf("second Close failed (should be no-op): %v", err)
	}
	if mgr.Healthy(context.Background()) {
		t.Error("Healthy() returned true after Close")
	}
}
