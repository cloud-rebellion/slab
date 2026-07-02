package eval

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/bright-interaction/slab/internal/agent"
	dbpkg "github.com/bright-interaction/slab/internal/db"
	"github.com/bright-interaction/slab/internal/store"
)

// openTestDB matches the helper pattern used in internal/retention/retention_test.go
// so the eval suite stays aligned with the rest of the project.
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

func seedSite(t *testing.T, sqlDB *sql.DB, id string) {
	t.Helper()
	if _, err := sqlDB.Exec(`INSERT INTO sites (id, name, slug) VALUES (?, ?, ?)`, id, "Test "+id, id); err != nil {
		t.Fatalf("seed site: %v", err)
	}
}

func seedCWV(t *testing.T, sqlDB *sql.DB, siteID, metric string, values []float64) {
	t.Helper()
	for i, v := range values {
		id := fmt.Sprintf("cwv_%s_%d", metric, i)
		if _, err := sqlDB.Exec(
			`INSERT INTO cwv_events (id, site_id, page_path, metric, value, rating, device) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, siteID, "/", metric, v, "", "",
		); err != nil {
			t.Fatalf("seed cwv: %v", err)
		}
	}
}

// findCheck looks up the first check whose Name contains the substring. Returns
// nil if not found.
func findCheck(checks []CheckResult, contains string) *CheckResult {
	for i := range checks {
		if strings.Contains(checks[i].Name, contains) {
			return &checks[i]
		}
	}
	return nil
}

func TestRunCWVChecks_NoSamples(t *testing.T) {
	_, queries := openTestDB(t)
	ctx := context.Background()
	// No site seeded, no events. Function must not crash, and every metric
	// is Info-only (no scoring impact for a brand-new site).
	checks := RunCWVChecks(ctx, queries, "site_no_data", agent.FidelityBalanced)
	if len(checks) != len(cwvMetricOrder) {
		t.Fatalf("expected %d info checks, got %d", len(cwvMetricOrder), len(checks))
	}
	for _, c := range checks {
		if c.Passed != nil {
			t.Errorf("metric %s: expected Info (Passed=nil), got Passed=%v", c.Name, *c.Passed)
		}
	}
}

func TestRunCWVChecks_NilQueriesIsSafe(t *testing.T) {
	if checks := RunCWVChecks(context.Background(), nil, "any", agent.FidelityBalanced); checks != nil {
		t.Fatalf("expected nil checks for nil queries, got %d", len(checks))
	}
	_, queries := openTestDB(t)
	if checks := RunCWVChecks(context.Background(), queries, "", agent.FidelityBalanced); checks != nil {
		t.Fatalf("expected nil checks for empty siteID, got %d", len(checks))
	}
}

func TestRunCWVChecks_BelowMinSamplesIsInfo(t *testing.T) {
	sqlDB, queries := openTestDB(t)
	siteID := "abcdef0123456789abcdef02"
	seedSite(t, sqlDB, siteID)

	// 5 samples is below the floor; should produce Info, not a graded result.
	seedCWV(t, sqlDB, siteID, "LCP", []float64{1000, 1100, 1200, 1300, 1400})
	checks := RunCWVChecks(context.Background(), queries, siteID, agent.FidelityBalanced)

	lcp := findCheck(checks, "LCP")
	if lcp == nil {
		t.Fatal("LCP check missing")
	}
	if lcp.Passed != nil {
		t.Fatalf("LCP with %d samples should be Info, got Passed=%v", 5, *lcp.Passed)
	}
	if !strings.Contains(lcp.Detail, "need 10") {
		t.Errorf("expected detail to mention sample floor, got %q", lcp.Detail)
	}
}

func TestRunCWVChecks_GradesGoodLCP(t *testing.T) {
	sqlDB, queries := openTestDB(t)
	siteID := "abcdef0123456789abcdef03"
	seedSite(t, sqlDB, siteID)

	// 12 LCP samples, all well under 2500ms. p75 should be ~1800ms => good.
	values := []float64{1000, 1100, 1200, 1300, 1400, 1500, 1600, 1700, 1800, 1900, 2000, 2100}
	seedCWV(t, sqlDB, siteID, "LCP", values)

	checks := RunCWVChecks(context.Background(), queries, siteID, agent.FidelityBalanced)
	lcp := findCheck(checks, "LCP")
	if lcp == nil || lcp.Passed == nil || !*lcp.Passed {
		t.Fatalf("expected LCP to pass, got %+v", lcp)
	}
	if lcp.Weight != cwvMetricThresholds["LCP"].weight {
		t.Errorf("expected weight %d, got %d", cwvMetricThresholds["LCP"].weight, lcp.Weight)
	}
}

func TestRunCWVChecks_GradesPoorINP(t *testing.T) {
	sqlDB, queries := openTestDB(t)
	siteID := "abcdef0123456789abcdef04"
	seedSite(t, sqlDB, siteID)

	// 12 INP samples; p75 sample at index 9 (floor(0.75*12)=9) lands at 800ms,
	// which is well above the 500ms "poor" threshold.
	values := []float64{100, 150, 200, 250, 300, 400, 500, 600, 700, 800, 900, 1000}
	seedCWV(t, sqlDB, siteID, "INP", values)

	checks := RunCWVChecks(context.Background(), queries, siteID, agent.FidelityBalanced)
	inp := findCheck(checks, "INP")
	if inp == nil || inp.Passed == nil || *inp.Passed {
		t.Fatalf("expected INP to fail, got %+v", inp)
	}
	if inp.Severity != SeverityError {
		t.Errorf("expected SeverityError for poor INP, got %q", inp.Severity)
	}
	if !strings.Contains(inp.Recommendation, "main-thread") {
		t.Errorf("expected INP recommendation to mention main-thread, got %q", inp.Recommendation)
	}
}

func TestRunCWVChecks_NeedsImprovementCLS(t *testing.T) {
	sqlDB, queries := openTestDB(t)
	siteID := "abcdef0123456789abcdef05"
	seedSite(t, sqlDB, siteID)

	// 12 CLS samples; p75 sample at index 9 lands at 0.18, between 0.1 and 0.25.
	values := []float64{0.01, 0.02, 0.03, 0.05, 0.07, 0.09, 0.11, 0.13, 0.15, 0.18, 0.20, 0.22}
	seedCWV(t, sqlDB, siteID, "CLS", values)

	checks := RunCWVChecks(context.Background(), queries, siteID, agent.FidelityBalanced)
	cls := findCheck(checks, "CLS")
	if cls == nil || cls.Passed == nil || *cls.Passed {
		t.Fatalf("expected CLS to fail with warning, got %+v", cls)
	}
	if cls.Severity != SeverityWarning {
		t.Errorf("expected SeverityWarning for needs-improvement CLS, got %q", cls.Severity)
	}
}

func TestRunCWVChecks_ComputeScoreFoldsCWV(t *testing.T) {
	sqlDB, queries := openTestDB(t)
	siteID := "abcdef0123456789abcdef06"
	seedSite(t, sqlDB, siteID)

	// Good LCP, poor INP. Verify the resulting checks slice produces a score
	// that reflects both: 4 points won (LCP), 4 points lost (INP). ComputeScore
	// must count both metrics in the denominator (Pass + Fail are both scored).
	good := []float64{1000, 1100, 1200, 1300, 1400, 1500, 1600, 1700, 1800, 1900, 2000, 2100}
	poor := []float64{100, 150, 200, 250, 300, 400, 500, 600, 700, 800, 900, 1000}
	seedCWV(t, sqlDB, siteID, "LCP", good)
	seedCWV(t, sqlDB, siteID, "INP", poor)

	checks := RunCWVChecks(context.Background(), queries, siteID, agent.FidelityBalanced)
	score, max := ComputeScore(checks)
	wantMax := cwvMetricThresholds["LCP"].weight + cwvMetricThresholds["INP"].weight
	if max != wantMax {
		t.Fatalf("expected scoring denominator %d, got %d", wantMax, max)
	}
	wantScore := cwvMetricThresholds["LCP"].weight
	if score != wantScore {
		t.Errorf("expected score %d (LCP pass only), got %d", wantScore, score)
	}
}

func TestFormatCWVValue(t *testing.T) {
	cases := []struct {
		metric string
		v      float64
		want   string
	}{
		{"LCP", 2345.6, "2346ms"},
		{"INP", 199.4, "199ms"},
		{"FCP", 1800, "1800ms"},
		{"TTFB", 750.9, "751ms"},
		{"CLS", 0.0834, "0.083"},
	}
	for _, c := range cases {
		got := formatCWVValue(c.metric, c.v)
		if got != c.want {
			t.Errorf("formatCWVValue(%s, %v) = %q, want %q", c.metric, c.v, got, c.want)
		}
	}
}
