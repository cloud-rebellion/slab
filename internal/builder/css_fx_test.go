package builder

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	dbpkg "github.com/bright-interaction/slab/internal/db"
	"github.com/bright-interaction/slab/internal/store"
)

func openFXTestDB(t *testing.T) (*sql.DB, *store.Queries) {
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

// TestBuildCSS_FXLayerGatedByFidelity pins the byte-identity contract:
// the fx utility layer is emitted ONLY when design.fidelity=showcase.
// Balanced (absent row) and performance builds carry no fx rules, so
// existing sites keep building byte-identically.
func TestBuildCSS_FXLayerGatedByFidelity(t *testing.T) {
	sqlDB, queries := openFXTestDB(t)
	ctx := context.Background()
	const siteID = "fxsite000000000000000001"
	if _, err := sqlDB.Exec(`INSERT INTO sites (id, name, slug) VALUES (?, 'FX', 'fx')`, siteID); err != nil {
		t.Fatalf("seed site: %v", err)
	}

	balanced, err := BuildCSS(ctx, queries, siteID)
	if err != nil {
		t.Fatalf("BuildCSS balanced: %v", err)
	}
	if strings.Contains(balanced, ".fx-reveal") || strings.Contains(balanced, "Showcase fx layer") {
		t.Fatal("balanced build must not carry the fx layer")
	}
	// The aurora hero graphic is fidelity-independent (any profile may
	// render it; the playbook just advertises it in showcase).
	if !strings.Contains(balanced, ".hero-graphic--aurora") {
		t.Fatal("aurora hero CSS missing from balanced build")
	}
	if !strings.Contains(balanced, ".hero-graphic--aurora, .hero-graphic--aurora::after { animation: none; }") {
		t.Fatal("aurora reduced-motion gate missing")
	}

	set := func(value string) {
		if _, err := sqlDB.Exec(`INSERT INTO site_settings (id, site_id, category, key, value) VALUES ('fid1', ?, 'design', 'fidelity', ?)
			ON CONFLICT(site_id, category, key) DO UPDATE SET value=excluded.value`, siteID, value); err != nil {
			t.Fatalf("set fidelity: %v", err)
		}
	}

	set("performance")
	perf, err := BuildCSS(ctx, queries, siteID)
	if err != nil {
		t.Fatalf("BuildCSS performance: %v", err)
	}
	if perf != balanced {
		t.Fatal("performance CSS must be byte-identical to balanced")
	}

	set("showcase")
	show, err := BuildCSS(ctx, queries, siteID)
	if err != nil {
		t.Fatalf("BuildCSS showcase: %v", err)
	}
	for _, cls := range []string{".fx-reveal", ".fx-parallax-slow", ".fx-float", ".fx-shimmer", ".fx-gradient-text", ".fx-aurora-bg", ".fx-stagger", ".fx-tilt"} {
		if !strings.Contains(show, cls) {
			t.Fatalf("showcase CSS missing %s", cls)
		}
	}
	// The a11y invariant: the fx layer ships its own reduced-motion kill.
	if !strings.Contains(show, ".fx-reveal, .fx-reveal-scale, .fx-parallax-slow, .fx-parallax-fast, .fx-stagger > *, .fx-float, .fx-shimmer, .fx-aurora-bg::before { animation: none; }") {
		t.Fatal("fx reduced-motion gate missing from showcase CSS")
	}
	// Scroll-driven effects live behind the @supports gate.
	if !strings.Contains(show, "@supports (animation-timeline: view())") {
		t.Fatal("fx scroll-driven block missing @supports gate")
	}
}
