package handlers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoTestAppliesTheSchemaDirectly keeps newSchemaDB the only place this
// package applies internal/db.Schema.
//
// The cost this guards is not stylistic. Eighteen fixtures each ran the full
// DDL per test, at 441ms apiece under -race, and the nineteenth copy would be
// written the same way the first eighteen were: by copying the block above it.
// The suite that resulted ran past ci-go's 600s ceiling on the CI runner and
// panicked with a hundred goroutines parked in testing.(*T).Parallel, which
// reads as a deadlock and was not one. Nothing in the codebase would have
// objected; that is what this test is for.
//
// It reads the test SOURCE rather than counting queries at runtime, because the
// failure mode is a fixture that is never called by the test you happen to run.
func TestNoTestAppliesTheSchemaDirectly(t *testing.T) {
	const fixture = "schema_fixture_test.go"

	// Assembled rather than written literally, so this file does not contain
	// the string it is looking for. Exempting the guard by NAME would be the
	// obvious alternative and is worse: a named exemption is a hole that a
	// future copy could be written into, and it would report itself as passing.
	needle := "dbpkg" + ".Schema"
	applies := "db.Exec(string(" + needle + "))"

	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) < 20 {
		t.Fatalf("expected the handlers test suite to be dozens of files, found %d: the glob is not matching", len(files))
	}

	// Proves the detector is not vacuous: the one legitimate site must be
	// found by the very substring the loop below rejects everywhere else.
	tmpl, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read %s: %v", fixture, err)
	}
	// Exactly ONE site, not "at least one". Skipping the fixture file wholesale
	// was a hole: a second DDL-applying helper appended INTO schema_fixture_test.go
	// -- a sibling newSchemaDBNoWAL is exactly what the WAL tradeoff discussed in
	// that file would motivate -- runs 441ms per test and the guard reports clean.
	if n := strings.Count(string(tmpl), applies); n != 1 {
		t.Fatalf("%s must apply the schema at exactly one site, found %d. Either the one site moved (this guard is now checking for a string that no longer exists, and would pass over any number of new copies) or a second one was added inside the exempt file.", fixture, n)
	}

	for _, f := range files {
		if f == fixture {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if strings.Contains(string(b), needle) {
			t.Errorf("%s applies internal/db.Schema directly. That costs ~441ms per call under -race and it is paid per test; use newSchemaDB(t, %q) instead, which copies a template built once per binary (~38ms).", f, "example.db")
		}
	}
}
