package handlers

import (
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"

	dbpkg "github.com/bright-interaction/slab/internal/db"
)

// newSchemaDB opens a fresh, schema-applied SQLite database for one test and
// returns it together with the temp directory it lives in.
//
// It replaces eighteen hand-copied versions of the same six lines, and it
// exists for the COST, not the duplication. Applying internal/db.Schema to an
// empty database measures 441ms under -race on a quiet laptop --
// modernc.org/sqlite is SQLite transpiled to Go, so the race detector
// instruments every access inside the engine itself -- and this package paid
// that once per test across 338 tests. That is ~149s of a 161s suite: the
// suite was ~93% schema application and ~7% testing.
//
// It read as a hang. On the 4-vCPU CI runner the same suite ran past ci-go's
// `go test -timeout=600s` and panicked with 100+ goroutines parked in
// testing.(*T).Parallel for nine minutes, which looks exactly like a deadlock
// and was not one: the serial pass was simply still running. The honest fix is
// to stop paying the cost, not to raise the ceiling until the symptom clears.
//
// The schema is applied ONCE per test binary, into a template file, and each
// call hands back a byte copy of that template: 38ms instead of 441ms,
// measured the same way. Isolation is unchanged, because a copy is a separate
// database -- its own file, in the test's own t.TempDir(), closed on cleanup.
// No handle, page cache or transaction is shared between tests.
func newSchemaDB(t *testing.T, file string) (*sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, file)

	blank, err := os.ReadFile(schemaTemplatePath(t))
	if err != nil {
		t.Fatalf("read schema template: %v", err)
	}
	if err := os.WriteFile(dbPath, blank, 0o600); err != nil {
		t.Fatalf("write test db: %v", err)
	}

	// One DSN for every fixture. Two of the eighteen carried
	// _busy_timeout=5000 and sixteen did not, which was drift rather than a
	// decision: under SetMaxOpenConns(1) a single process cannot contend with
	// itself, so the timeout only covers an external writer no test has.
	// Keeping it is the strictly more forgiving of the two, so no fixture that
	// passed without it can start failing with it.
	sqlDB, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })
	return sqlDB, dir
}

var (
	schemaTemplateOnce sync.Once
	schemaTemplate     string
	schemaTemplateErr  error
)

// schemaTemplatePath applies the schema once per test binary and returns the
// path of the resulting database file.
//
// The template deliberately does NOT use WAL. A WAL database's committed
// content can be sitting in a separate -wal file that a single-file copy would
// leave behind, and the copy would then open as an empty schema with no error
// anywhere -- the exact silent-vacuity shape this package's guard tests exist
// to prevent. Applying the schema under the default rollback journal keeps
// everything in the one file; each COPY is then opened with WAL, which is what
// the fixtures always did and what production uses.
//
// os.MkdirTemp rather than t.TempDir: the template outlives the first test
// that happens to ask for it. Go removes the process's temp files on exit and
// the CI container is discarded outright, so nothing here leaks past the run.
func schemaTemplatePath(t *testing.T) string {
	t.Helper()
	schemaTemplateOnce.Do(func() {
		dir, err := os.MkdirTemp("", "atomicsite-schema-template-")
		if err != nil {
			schemaTemplateErr = err
			return
		}
		path := filepath.Join(dir, "template.sqlite")
		db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
		if err != nil {
			schemaTemplateErr = err
			return
		}
		defer db.Close()
		if _, err := db.Exec(string(dbpkg.Schema)); err != nil {
			schemaTemplateErr = err
			return
		}
		schemaTemplate = path
	})
	if schemaTemplateErr != nil {
		t.Fatalf("build schema template: %v", schemaTemplateErr)
	}
	return schemaTemplate
}
