package db

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// Audit 2026-08-11 finding 10 regression guard.
//
// The production DSN was
// "<path>?_journal_mode=WAL&_busy_timeout=5000&_pragma=foreign_keys(1)".
// modernc.org/sqlite honours only the _pragma=name(value) form, so the
// first two options were parsed as nothing and dropped in silence:
// busy_timeout stayed 0 and journal_mode stayed delete. foreign_keys
// applied because it happened to be written correctly, which is what
// made the bug invisible to a spot check.

// shippedDSN is the exact broken string, kept verbatim so the test is
// evidence about the real defect and not a paraphrase of it.
func shippedDSN(path string) string {
	return path + "?_journal_mode=WAL&_busy_timeout=5000&_pragma=foreign_keys(1)"
}

func openAt(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	d.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func pragmas(t *testing.T, d *sql.DB) (journal string, busy, fk int) {
	t.Helper()
	if err := d.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if err := d.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
		t.Fatalf("busy_timeout: %v", err)
	}
	if err := d.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("foreign_keys: %v", err)
	}
	return
}

// TestShippedDSNSilentlyDroppedItsOptions pins the defect itself. If a
// future driver upgrade starts honouring the mattn spellings this test
// fails and tells the next reader the finding's premise changed. It is
// documentation with an assertion attached, not a guard on our code.
func TestShippedDSNSilentlyDroppedItsOptions(t *testing.T) {
	d := openAt(t, shippedDSN(filepath.Join(t.TempDir(), "shipped.sqlite")))
	journal, busy, fk := pragmas(t, d)
	t.Logf("as shipped:  foreign_keys=%d busy_timeout=%d journal_mode=%s", fk, busy, journal)

	if strings.EqualFold(journal, "wal") {
		t.Fatal("driver now honours _journal_mode=; finding 10's premise has changed, re-read it")
	}
	if busy != 0 {
		t.Fatal("driver now honours _busy_timeout=; finding 10's premise has changed, re-read it")
	}
	if fk != 1 {
		t.Fatal("_pragma=foreign_keys(1) was the one form that always worked; it must still work")
	}
}

// TestDSNAppliesAllThreePragmas is the fix.
func TestDSNAppliesAllThreePragmas(t *testing.T) {
	d := openAt(t, DSN(filepath.Join(t.TempDir(), "fixed.sqlite"), DefaultBusyTimeoutMs))
	journal, busy, fk := pragmas(t, d)
	t.Logf("corrected:   foreign_keys=%d busy_timeout=%d journal_mode=%s", fk, busy, journal)

	if !strings.EqualFold(journal, "wal") {
		t.Fatalf("journal_mode = %s, want wal", journal)
	}
	if busy != DefaultBusyTimeoutMs {
		t.Fatalf("busy_timeout = %d, want %d", busy, DefaultBusyTimeoutMs)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys = %d, want 1", fk)
	}
}

func TestDSNHonoursCustomBusyTimeout(t *testing.T) {
	d := openAt(t, DSN(filepath.Join(t.TempDir(), "custom.sqlite"), 10000))
	_, busy, _ := pragmas(t, d)
	if busy != 10000 {
		t.Fatalf("busy_timeout = %d, want 10000", busy)
	}
}

func TestDSNFallsBackOnNonPositiveTimeout(t *testing.T) {
	d := openAt(t, DSN(filepath.Join(t.TempDir(), "zero.sqlite"), 0))
	_, busy, _ := pragmas(t, d)
	if busy != DefaultBusyTimeoutMs {
		t.Fatalf("busy_timeout = %d, want fallback %d", busy, DefaultBusyTimeoutMs)
	}
}

// The boot assertion is the part that makes a future typo survivable,
// so it has to actually reject the old string rather than merely
// accepting the new one.
func TestAssertPragmasRejectsTheShippedDSN(t *testing.T) {
	d := openAt(t, shippedDSN(filepath.Join(t.TempDir(), "reject.sqlite")))
	err := AssertPragmas(d, DefaultBusyTimeoutMs)
	if err == nil {
		t.Fatal("AssertPragmas accepted the DSN that produced busy_timeout=0 journal_mode=delete")
	}
	for _, want := range []string{"journal_mode=delete", "busy_timeout=0"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
	t.Logf("boot assertion refuses: %v", err)
}

func TestAssertPragmasAcceptsCorrectDSN(t *testing.T) {
	d := openAt(t, DSN(filepath.Join(t.TempDir(), "accept.sqlite"), DefaultBusyTimeoutMs))
	if err := AssertPragmas(d, DefaultBusyTimeoutMs); err != nil {
		t.Fatalf("AssertPragmas rejected a correct DSN: %v", err)
	}
}

// A wrong-but-parsed busy timeout must also be caught, not just the
// unparsed case.
func TestAssertPragmasCatchesTimeoutMismatch(t *testing.T) {
	d := openAt(t, DSN(filepath.Join(t.TempDir(), "mismatch.sqlite"), 1234))
	err := AssertPragmas(d, DefaultBusyTimeoutMs)
	if err == nil {
		t.Fatal("AssertPragmas accepted busy_timeout=1234 when 5000 was required")
	}
	if !strings.Contains(err.Error(), "busy_timeout=1234") {
		t.Fatalf("error %q does not name the actual value", err)
	}
}

func TestAssertPragmasRejectsNilHandle(t *testing.T) {
	if err := AssertPragmas(nil, DefaultBusyTimeoutMs); err == nil {
		t.Fatal("AssertPragmas accepted a nil handle")
	}
}

func TestPragmaSnapshotReportsAppliedValues(t *testing.T) {
	d := openAt(t, DSN(filepath.Join(t.TempDir(), "snap.sqlite"), DefaultBusyTimeoutMs))
	snap := PragmaSnapshot(d)
	if !strings.EqualFold(snap["journal_mode"], "wal") || snap["busy_timeout"] != "5000" || snap["foreign_keys"] != "1" {
		t.Fatalf("snapshot = %v", snap)
	}
}
