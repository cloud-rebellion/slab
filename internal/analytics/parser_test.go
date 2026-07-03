package analytics

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	dbpkg "github.com/bright-interaction/slab/internal/db"
	"github.com/bright-interaction/slab/internal/store"
)

func TestFingerprintDeterministic(t *testing.T) {
	a := fingerprint("1.2.3.4", "Mozilla/5.0", "en-US", "salt-1")
	b := fingerprint("1.2.3.4", "Mozilla/5.0", "en-US", "salt-1")
	if a != b {
		t.Fatalf("expected determinism, got %q vs %q", a, b)
	}
}

func TestFingerprintDifferentSalts(t *testing.T) {
	a := fingerprint("1.2.3.4", "Mozilla/5.0", "en-US", "salt-1")
	b := fingerprint("1.2.3.4", "Mozilla/5.0", "en-US", "salt-2")
	if a == b {
		t.Fatalf("expected different fingerprints for different salts, got %q", a)
	}
}

func TestFingerprintFormat(t *testing.T) {
	fp := fingerprint("1.2.3.4", "ua", "lang", "salt")
	if len(fp) != 16 {
		t.Fatalf("expected 16 chars, got %d (%q)", len(fp), fp)
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(fp) {
		t.Fatalf("expected lowercase hex, got %q", fp)
	}
}

func TestFingerprintInputSeparation(t *testing.T) {
	// "ab" + "" should differ from "a" + "b": newline separators prevent collision.
	a := fingerprint("ab", "", "", "salt")
	b := fingerprint("a", "b", "", "salt")
	if a == b {
		t.Fatalf("inputs should not collide")
	}
}

// --- end-to-end parser tests ---

func newTestDB(t *testing.T) (*store.Queries, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	sqlDB, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if _, err := sqlDB.Exec(string(dbpkg.Schema)); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	if _, err := sqlDB.Exec(`INSERT INTO sites (id, name, slug) VALUES ('site-1','Test','test')`); err != nil {
		t.Fatalf("seed site: %v", err)
	}
	return store.New(sqlDB), sqlDB
}

func waitForRows(t *testing.T, db *sql.DB, query string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := db.QueryRow(query).Scan(&n); err == nil && n >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %d rows from %q", want, query)
}

func TestParserRecordsValidLine(t *testing.T) {
	queries, db := newTestDB(t)
	defer db.Close()

	logPath := filepath.Join(t.TempDir(), "site.json.log")
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatalf("create log: %v", err)
	}

	p := NewParser("site-1", logPath, "test-salt", queries)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)
	defer p.Stop()

	// Parser opens at end-of-file, so wait briefly for it to attach.
	time.Sleep(50 * time.Millisecond)

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	line := `{"ts":"2026-04-25T10:00:00+00:00","site_id":"site-1","ip":"1.2.3.4","ua":"ua","al":"en","path":"/","status":200,"ms":0.012,"ref":""}` + "\n"
	if _, err := f.WriteString(line); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = f.Close()

	waitForRows(t, db, `SELECT COUNT(*) FROM visit_events WHERE site_id='site-1'`, 1)
	waitForRows(t, db, `SELECT COUNT(*) FROM visit_sessions WHERE site_id='site-1'`, 1)

	var fp string
	if err := db.QueryRow(`SELECT fingerprint FROM visit_events LIMIT 1`).Scan(&fp); err != nil {
		t.Fatalf("query fp: %v", err)
	}
	want := fingerprint("1.2.3.4", "ua", "en", "test-salt")
	if fp != want {
		t.Fatalf("fingerprint mismatch: got %q want %q", fp, want)
	}
}

func TestParserSkipsMalformed(t *testing.T) {
	queries, db := newTestDB(t)
	defer db.Close()

	logPath := filepath.Join(t.TempDir(), "site.json.log")
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatalf("create log: %v", err)
	}

	p := NewParser("site-1", logPath, "salt", queries)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)
	defer p.Stop()

	time.Sleep(50 * time.Millisecond)

	f, _ := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString("this is not json\n")
	_, _ = f.WriteString(`{"path":"/ok","ip":"1.1.1.1","ua":"u","al":"a","status":200,"ms":0.001}` + "\n")
	_ = f.Close()

	waitForRows(t, db, `SELECT COUNT(*) FROM visit_events`, 1)
}

func TestParserHandlesRotation(t *testing.T) {
	queries, db := newTestDB(t)
	defer db.Close()

	logPath := filepath.Join(t.TempDir(), "site.json.log")
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatalf("create log: %v", err)
	}

	p := NewParser("site-1", logPath, "salt", queries)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)
	defer p.Stop()

	time.Sleep(50 * time.Millisecond)

	// Write a line into the original file.
	first := `{"path":"/before","ip":"1.1.1.1","ua":"u","al":"a","status":200,"ms":0.001}` + "\n"
	if err := appendLine(logPath, first); err != nil {
		t.Fatalf("append: %v", err)
	}
	waitForRows(t, db, `SELECT COUNT(*) FROM visit_events`, 1)

	// Simulate logrotate: remove + recreate (new inode).
	if err := os.Remove(logPath); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	// Give the parser time to detect rotation and reopen.
	time.Sleep(2 * pollInterval)

	second := `{"path":"/after","ip":"2.2.2.2","ua":"u","al":"a","status":200,"ms":0.001}` + "\n"
	if err := appendLine(logPath, second); err != nil {
		t.Fatalf("append after rotate: %v", err)
	}
	waitForRows(t, db, `SELECT COUNT(*) FROM visit_events`, 2)
}

func appendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line)
	return err
}

// TestParser404IsCapturedToMissingURLs is the Sprint 2 wiring test: a 404
// log line should land in BOTH visit_events (every request is recorded) and
// missing_urls (404-only aggregation for the migration redirect-suggestion
// inbox). Multiple hits on the same path must aggregate into one row with
// hit_count incremented; query strings must be stripped so /old?ref=foo
// and /old?ref=bar coalesce.
func TestParser404IsCapturedToMissingURLs(t *testing.T) {
	queries, db := newTestDB(t)
	defer db.Close()

	logPath := filepath.Join(t.TempDir(), "site.json.log")
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatalf("create log: %v", err)
	}
	p := NewParser("site-1", logPath, "salt", queries)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)
	defer p.Stop()
	time.Sleep(50 * time.Millisecond)

	// Three 404 hits: two on /old (one with query string, must coalesce),
	// one on /gone. Plus one 200 on /home (must NOT land in missing_urls).
	lines := []string{
		`{"site_id":"site-1","ip":"1.1.1.1","ua":"u","al":"en","path":"/old","status":404,"ms":0.001,"ref":"https://google.com/"}`,
		`{"site_id":"site-1","ip":"2.2.2.2","ua":"u","al":"en","path":"/old?ref=foo","status":404,"ms":0.001,"ref":""}`,
		`{"site_id":"site-1","ip":"3.3.3.3","ua":"u","al":"en","path":"/gone","status":404,"ms":0.001,"ref":""}`,
		`{"site_id":"site-1","ip":"4.4.4.4","ua":"u","al":"en","path":"/home","status":200,"ms":0.001,"ref":""}`,
	}
	for _, line := range lines {
		if err := appendLine(logPath, line+"\n"); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	// All four visit_events rows must land regardless of status code.
	waitForRows(t, db, `SELECT COUNT(*) FROM visit_events WHERE site_id='site-1'`, 4)
	// Two missing_urls rows: /old (hit_count=2 after coalescing) + /gone (1).
	waitForRows(t, db, `SELECT COUNT(*) FROM missing_urls WHERE site_id='site-1'`, 2)

	// Verify the /old row coalesced with hit_count=2 and the referer
	// from the first hit (Google) is preserved when the second hit
	// has an empty referer.
	var path string
	var hits int
	var referer string
	if err := db.QueryRow(
		`SELECT path, hit_count, referer FROM missing_urls WHERE site_id='site-1' AND path='/old'`,
	).Scan(&path, &hits, &referer); err != nil {
		t.Fatalf("query /old: %v", err)
	}
	if path != "/old" {
		t.Errorf("path: %q (query string should be stripped)", path)
	}
	if hits != 2 {
		t.Errorf("/old hit_count: want 2, got %d", hits)
	}
	if referer != "https://google.com/" {
		t.Errorf("referer should be preserved from first hit, got %q", referer)
	}

	// 200 path must NOT show up in missing_urls.
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM missing_urls WHERE site_id='site-1' AND path='/home'`,
	).Scan(&n); err != nil {
		t.Fatalf("query /home: %v", err)
	}
	if n != 0 {
		t.Errorf("200 must not land in missing_urls")
	}
}

func TestStripQueryFragment(t *testing.T) {
	cases := map[string]string{
		"/about":           "/about",
		"/about?ref=email": "/about",
		"/about#section":   "/about",
		"/about?x=1#sec":   "/about",
		"":                 "",
	}
	for in, want := range cases {
		if got := stripQueryFragment(in); got != want {
			t.Errorf("stripQueryFragment(%q) = %q, want %q", in, got, want)
		}
	}
}
