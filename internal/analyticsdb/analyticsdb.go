// Package analyticsdb is a read-only DuckDB layer that ATTACHes the live
// SQLite database (atomicsite.db) and serves stitched analytical queries.
// SQLite stays the system of record for every write (visit_events from the
// nginx tail, /t/consent receiver, /t/engagement beacon, all admin CRUD);
// DuckDB is a second connection on the same file with a columnar query
// engine on top.
//
// Why this shape:
//   - Single source of truth: one .db file. Backups stay simple.
//   - No sync logic: DuckDB reads what SQLite just wrote (read-only ATTACH).
//   - Columnar reads: DuckDB scans visit_events 5-50x faster than the
//     SQLite query planner for the GROUP BY + COUNT(DISTINCT) shapes the
//     cookie analytics dashboard needs.
//   - No rollup table: DuckDB's speed on raw data lets us skip the
//     materialized-aggregate layer that would otherwise be the next
//     performance step. Every report is a live join.
//
// The DuckDB connection is a long-lived *sql.DB held by Manager; the
// sqlite_scanner extension is loaded once at boot and the SQLite file is
// ATTACHed. Connection lifetime is the process lifetime.
//
// All queries here are read-only by design. Writes always go through the
// SQLite connection in cmd/server/main.go to avoid double-writer contention
// against the same file.
package analyticsdb

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	_ "github.com/marcboeker/go-duckdb/v2"
)

// Manager owns the DuckDB connection and its ATTACH to the SQLite file.
// One per process. Safe for concurrent use across handlers.
type Manager struct {
	db *sql.DB

	mu     sync.RWMutex
	closed bool
}

// Config is the only thing analyticsdb needs to connect: the absolute
// path to atomicsite.db. The DuckDB instance itself is in-memory; only
// the ATTACHed SQLite file lands on disk, which is the same file the
// rest of the server already uses.
type Config struct {
	// SQLitePath is the absolute path to the SQLite database file
	// (typically cfg.DBPath). Must exist before Open is called.
	SQLitePath string
}

// Open creates a new DuckDB connection, loads the sqlite extension, and
// ATTACHes the live SQLite file as schema "oltp". The returned Manager
// is ready for queries; call Close on shutdown.
//
// Errors here are fatal at boot time: if DuckDB can't load or ATTACH
// fails, the analytics tab won't work anyway, so the server should
// log + continue without analytics rather than crash. Callers decide.
func Open(ctx context.Context, cfg Config) (*Manager, error) {
	if strings.TrimSpace(cfg.SQLitePath) == "" {
		return nil, fmt.Errorf("analyticsdb: SQLitePath is required")
	}

	// In-memory DuckDB; storage lives in the ATTACHed SQLite file.
	//
	// GDPR NOTE, so this does not get re-raised every audit: because the DSN is
	// empty this engine holds NO data of its own, and the ATTACH is read-only,
	// so there is nothing here for an erasure to miss. The SQLite cascade in
	// retention.sweepGDPRDeletions IS the erasure, and DuckDB reads through to
	// the same file afterwards. The 2026-07-29 audit flagged "cascade cannot
	// reach the DuckDB store" as a possible compliance gap and it is not one.
	//
	// This stops being true the moment anyone gives this DSN a path, or
	// materialises a DuckDB table instead of querying the ATTACHed schema. If
	// you do either, add an explicit erasure hook to sweepGDPRDeletions first.
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, fmt.Errorf("analyticsdb: open duckdb: %w", err)
	}
	// Pool tuned for a small read-only workload: enough concurrency for a
	// handful of dashboard tabs without flooding DuckDB's planner.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)

	// Load + install the sqlite extension. INSTALL is a no-op when the
	// extension is already cached; LOAD is the real activator. DuckDB
	// downloads the extension on first install if not present in its
	// cache (~/.duckdb/extensions/...), so the first boot in a fresh
	// container makes one outbound HTTP call. Production images bundle
	// the extension to avoid that.
	if _, err := db.ExecContext(ctx, "INSTALL sqlite"); err != nil {
		// Some DuckDB builds bundle the extension; INSTALL is a no-op
		// then. Tolerate failure here as long as LOAD succeeds.
		slog.Debug("analyticsdb: install sqlite (treating as preloaded)", "err", err)
	}
	if _, err := db.ExecContext(ctx, "LOAD sqlite"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("analyticsdb: load sqlite extension: %w", err)
	}

	// READ_ONLY is critical: every write to atomicsite.db goes through
	// the modernc.org/sqlite connection in main.go. If DuckDB writes to
	// the same file we'd violate WAL's single-writer assumption and
	// risk corruption.
	attachSQL := fmt.Sprintf("ATTACH '%s' AS oltp (TYPE sqlite, READ_ONLY)",
		strings.ReplaceAll(cfg.SQLitePath, "'", "''"))
	if _, err := db.ExecContext(ctx, attachSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("analyticsdb: attach sqlite: %w", err)
	}

	slog.Info("analyticsdb: ready", "sqlite_path", cfg.SQLitePath)
	return &Manager{db: db}, nil
}

// Close detaches the SQLite file and closes the DuckDB connection. Safe
// to call multiple times.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	// DETACH is best-effort; closing the underlying conn cleans up either way.
	if _, err := m.db.Exec("DETACH oltp"); err != nil {
		slog.Debug("analyticsdb: detach oltp", "err", err)
	}
	return m.db.Close()
}

// DB exposes the underlying *sql.DB for query implementations that live
// in this package. Not for general handler use; handlers should call
// the typed Query* methods on Manager instead so the SQL stays in one
// place.
func (m *Manager) DB() *sql.DB {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.db
}

// Healthy returns true when DuckDB responds to a trivial query against
// the ATTACHed SQLite. Used by the /api/health endpoint extension and
// the admin debug page; cheap to call.
func (m *Manager) Healthy(ctx context.Context) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return false
	}
	row := m.db.QueryRowContext(ctx, "SELECT 1 FROM oltp.sites LIMIT 1")
	var n int
	if err := row.Scan(&n); err != nil {
		// "no rows" is fine -- the connection works, the table is just
		// empty (e.g. fresh deployment).
		if err == sql.ErrNoRows {
			return true
		}
		slog.Warn("analyticsdb: healthcheck failed", "err", err)
		return false
	}
	return true
}
