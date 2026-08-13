package db

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// DSN builds the connection string for modernc.org/sqlite.
//
// Audit 2026-08-11 finding 10. The shipped string was:
//
//	<path>?_journal_mode=WAL&_busy_timeout=5000&_pragma=foreign_keys(1)
//
// modernc.org/sqlite parses ONLY the `_pragma=name(value)` form. It does
// not recognise mattn/go-sqlite3's `_journal_mode=` / `_busy_timeout=`
// spellings and does not error on them either, it ignores them. Read
// back from a fresh database on the exact production string:
//
//	as shipped:  foreign_keys=1  busy_timeout=0     journal_mode=delete
//	corrected:   foreign_keys=1  busy_timeout=5000  journal_mode=wal
//
// So every lock collision returned "database is locked" immediately
// instead of waiting out the writer, and the whole codebase's stated
// concurrency design ("SQLite WAL mode handles concurrent readers plus
// the single writer just fine", retention.go) was asserted rather than
// configured. Production files are probably WAL anyway because
// litestream sets it and journal_mode persists in the file header, but
// that is an accident of the deployment: a self-hoster without
// litestream ran in rollback-journal mode.
//
// foreign_keys(1) was the one pragma that already used the right form,
// which is exactly why this hid for so long: the cascade-delete
// behaviour the GDPR erasure path depends on genuinely worked, so
// spot-checking one pragma said everything was fine.
//
// A non-positive busyTimeoutMs falls back to DefaultBusyTimeoutMs.
func DSN(path string, busyTimeoutMs int) string {
	if busyTimeoutMs <= 0 {
		busyTimeoutMs = DefaultBusyTimeoutMs
	}
	return path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(" + strconv.Itoa(busyTimeoutMs) + ")" +
		"&_pragma=foreign_keys(1)"
}

// DefaultBusyTimeoutMs is the busy timeout used by the server and by
// every subcommand that does not need a longer one.
const DefaultBusyTimeoutMs = 5000

// AssertPragmas reads journal_mode, busy_timeout and foreign_keys back
// off a live connection and returns an error describing every mismatch.
// The point is that a DSN typo can no longer be silent: the process
// that opens the database is the process that proves the settings took.
//
// journal_mode is compared case-insensitively because SQLite reports
// "wal" while the pragma is conventionally written "WAL".
func AssertPragmas(sqlDB *sql.DB, wantBusyTimeoutMs int) error {
	if sqlDB == nil {
		return fmt.Errorf("sqlite pragma check: nil database handle")
	}
	if wantBusyTimeoutMs <= 0 {
		wantBusyTimeoutMs = DefaultBusyTimeoutMs
	}

	var journal string
	if err := sqlDB.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		return fmt.Errorf("sqlite pragma check: read journal_mode: %w", err)
	}
	var busy int
	if err := sqlDB.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
		return fmt.Errorf("sqlite pragma check: read busy_timeout: %w", err)
	}
	var fk int
	if err := sqlDB.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		return fmt.Errorf("sqlite pragma check: read foreign_keys: %w", err)
	}

	var bad []string
	if !strings.EqualFold(journal, "wal") {
		bad = append(bad, fmt.Sprintf("journal_mode=%s (want wal)", journal))
	}
	if busy != wantBusyTimeoutMs {
		bad = append(bad, fmt.Sprintf("busy_timeout=%d (want %d)", busy, wantBusyTimeoutMs))
	}
	if fk != 1 {
		bad = append(bad, fmt.Sprintf("foreign_keys=%d (want 1)", fk))
	}
	if len(bad) > 0 {
		return fmt.Errorf("sqlite pragmas not applied: %s; the DSN was accepted but ignored "+
			"(modernc.org/sqlite honours only the _pragma=name(value) form)", strings.Join(bad, ", "))
	}
	return nil
}

// PragmaSnapshot reports the three pragmas as a loggable map. Used by
// the boot log so the applied values are visible in production without
// having to attach a shell to the container.
func PragmaSnapshot(sqlDB *sql.DB) map[string]string {
	out := map[string]string{}
	if sqlDB == nil {
		return out
	}
	for _, p := range []string{"journal_mode", "busy_timeout", "foreign_keys"} {
		var v string
		if err := sqlDB.QueryRow("PRAGMA " + p).Scan(&v); err != nil {
			out[p] = "error: " + err.Error()
			continue
		}
		out[p] = v
	}
	return out
}
