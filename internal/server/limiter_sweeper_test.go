package server

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/bright-interaction/slab/internal/config"
	dbpkg "github.com/bright-interaction/slab/internal/db"
	"github.com/bright-interaction/slab/internal/storage"
	"github.com/bright-interaction/slab/internal/store"
)

// Audit 2026-08-11 finding 9, second half.
//
// A unit test of the sweeper passes whether or not anything is wired
// into it, which is exactly the failure mode this estate keeps
// shipping: a guard that is green while the defect it was built for is
// still live. So this test builds the REAL router off the REAL route
// table and asserts the registration happened there. If someone adds a
// sixth /t/* limiter, or a second login limiter, and forgets to
// register it, the name list here stops matching and this fails.
func newSweeperTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	sqlDB, err := sql.Open("sqlite", dbpkg.DSN(filepath.Join(dir, "test.sqlite"), dbpkg.DefaultBusyTimeoutMs))
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

	cfg := &config.Config{
		Env:           config.EnvDevelopment,
		BaseURL:       "http://localhost:8080",
		JWTSecret:     "test-jwt-secret-that-is-long-enough-for-hs256",
		DataDir:       dir,
		MaxUploadSize: 20 << 20,
		// Non-zero so newAdminWriteLimiter builds a real limiter rather
		// than the pass-through nil.
		AdminWriteRateCapacity: 20,
		AdminWriteRateRefill:   1,
	}
	return New(cfg, sqlDB, store.New(sqlDB), st)
}

func TestRouterRegistersEveryLiveRateLimiter(t *testing.T) {
	s := newSweeperTestServer(t)

	if n := s.LimiterSweeper().Len(); n != 0 {
		t.Fatalf("sweeper had %d registrations before Router()", n)
	}

	_ = s.Router()

	got := s.LimiterSweeper().Names()
	want := map[string]bool{
		"track.consent":    false,
		"track.pageview":   false,
		"track.engagement": false,
		"track.event":      false,
		"track.identify":   false,
		"admin.writes":     false,
		"auth.login":       false,
		"forms.submit":     false,
	}
	for _, n := range got {
		if _, ok := want[n]; !ok {
			t.Fatalf("unexpected limiter registration %q (registered: %v)", n, got)
		}
		want[n] = true
	}
	for n, seen := range want {
		if !seen {
			t.Fatalf("limiter %q was never registered with the sweeper; its map will grow "+
				"for the life of the process (registered: %v)", n, got)
		}
	}
}

// StartLimiterSweeper is what cmd/server calls. Assert it actually
// launches: before the fix nothing in the tree called any sweep()
// outside a test, and the whole point is that the schedule now exists.
// P1 verification 2026-08-13, blocker 2, item 3.
//
// shield.SweepExpired is the ninth sweeper of the same shape: a doc
// comment offering itself to "a janitor goroutine" that no non-test
// caller ever wrote. The 2026-08-11 pass wired the eight rate limiters
// and missed this one. The only expiry pass that ran was the
// opportunistic branch inside OpenSession, which fires only when a
// session with a known id is touched AFTER it expired: an agent
// connection that never comes back never expires anything, so its row
// and every encrypted PII token hanging off it live forever.
//
// The registration is conditional on a 32-byte shield key (no key, no
// shield, nothing to sweep), so this test sets one and asserts the name
// appears. TestRouterRegistersEveryLiveRateLimiter builds without a key
// and would fail on an unexpected name, which pins the other half.
func TestRouterRegistersShieldSessionSweep(t *testing.T) {
	s := newSweeperTestServer(t)
	s.cfg.ShieldKey = "0123456789abcdef0123456789abcdef"

	_ = s.Router()

	for _, n := range s.LimiterSweeper().Names() {
		if n == "shield.sessions" {
			// It must also survive a sweep without panicking on the
			// real store; SweepNow runs every callback synchronously.
			s.LimiterSweeper().SweepNow()
			return
		}
	}
	t.Fatalf("shield.sessions was never registered with the sweeper; expired shield sessions and their "+
		"encrypted PII tokens are never collected (registered: %v)", s.LimiterSweeper().Names())
}

func TestStartLimiterSweeperStartsAfterRouter(t *testing.T) {
	s := newSweeperTestServer(t)
	_ = s.Router()

	if s.LimiterSweeper().Started() {
		t.Fatal("sweeper reported started before StartLimiterSweeper")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartLimiterSweeper(ctx)

	if !s.LimiterSweeper().Started() {
		t.Fatal("StartLimiterSweeper did not start the sweeper")
	}
}
