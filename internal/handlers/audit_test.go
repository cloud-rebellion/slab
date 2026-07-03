package handlers

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	dbpkg "github.com/bright-interaction/slab/internal/db"
	"github.com/bright-interaction/slab/internal/store"
)

func openAuditTestDB(t *testing.T) *store.Queries {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "audit.db")
	sqlDB, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	if _, err := sqlDB.Exec(string(dbpkg.Schema)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return store.New(sqlDB)
}

// TestAuditLog_HappyPath confirms a row lands with every expected
// field and JSON-encoded changes. The IP comes from r.RemoteAddr,
// which the trusted-proxy middleware (TrustedProxyRealIP, mounted at
// the top of the request stack in production) has already rewritten
// from XFF when the immediate peer is in
// ATOMICSITE_TRUSTED_PROXIES. auditClientIP intentionally does NOT
// read XFF / CF-Connecting-IP directly; that would let any direct
// peer spoof the audit log.
func TestAuditLog_HappyPath(t *testing.T) {
	t.Parallel()
	queries := openAuditTestDB(t)
	r := httptest.NewRequest("DELETE", "/api/sites/x", nil)
	r.RemoteAddr = "203.0.113.7:55432"
	AuditLog(context.Background(), queries, r,
		"site-id-abc",
		AuditActionDelete,
		"site",
		"site-id-abc",
		map[string]any{"slug": "demo", "domain": "example.com"})

	rows, err := queries.ListAuditLogBySite(context.Background(), store.ListAuditLogBySiteParams{
		SiteID: "site-id-abc",
		Limit:  10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	row := rows[0]
	if row.Action != "delete" {
		t.Errorf("action=%q, want delete", row.Action)
	}
	if row.ResourceType != "site" {
		t.Errorf("resource_type=%q", row.ResourceType)
	}
	if row.ActorIp != "203.0.113.7" {
		t.Errorf("actor_ip=%q, want 203.0.113.7", row.ActorIp)
	}
	if row.ChangesJson == "" || row.ChangesJson == "{}" {
		t.Errorf("changes_json should carry payload, got %q", row.ChangesJson)
	}
}

// TestAuditLog_NilChangesEmitsEmptyObject verifies passing nil
// changes still produces a valid {} JSON value rather than null
// or an unset column.
func TestAuditLog_NilChangesEmitsEmptyObject(t *testing.T) {
	t.Parallel()
	queries := openAuditTestDB(t)
	r := httptest.NewRequest("POST", "/api/sites/x/agent-keys/k/revoke", nil)
	AuditLog(context.Background(), queries, r,
		"site-y",
		AuditActionRevoke,
		"agent_key",
		"k_42",
		nil)
	rows, err := queries.ListAuditLogBySite(context.Background(), store.ListAuditLogBySiteParams{
		SiteID: "site-y",
		Limit:  5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows", len(rows))
	}
	if rows[0].ChangesJson != "{}" {
		t.Errorf("nil changes -> changes_json=%q, want {}", rows[0].ChangesJson)
	}
}

// TestAuditLog_NonSerializableChangesFallsBackEmpty: an unmarshal
// error keeps the row alive at "{}" instead of dropping it.
func TestAuditLog_NonSerializableChangesFallsBackEmpty(t *testing.T) {
	t.Parallel()
	queries := openAuditTestDB(t)
	r := httptest.NewRequest("POST", "/x", nil)
	// channels can't marshal to JSON.
	weird := map[string]any{"ch": make(chan int)}
	AuditLog(context.Background(), queries, r, "site-z",
		AuditActionUpdate, "weird", "id", weird)
	rows, _ := queries.ListAuditLogBySite(context.Background(), store.ListAuditLogBySiteParams{
		SiteID: "site-z",
		Limit:  5,
	})
	if len(rows) != 1 {
		t.Fatalf("row should still be written; got %d", len(rows))
	}
	if rows[0].ChangesJson != "{}" {
		t.Errorf("changes_json on marshal failure = %q, want {}", rows[0].ChangesJson)
	}
}

// TestAuditLog_ListGlobal returns rows across sites in created_at
// DESC order.
func TestAuditLog_ListGlobal(t *testing.T) {
	t.Parallel()
	queries := openAuditTestDB(t)
	r := httptest.NewRequest("POST", "/x", nil)
	for i := 0; i < 3; i++ {
		AuditLog(context.Background(), queries, r,
			"site-multi", AuditActionCreate, "thing", "id", nil)
	}
	rows, err := queries.ListAuditLogGlobal(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Errorf("global feed: %d rows, want 3", len(rows))
	}
}

// TestParseLimit covers the ?limit= clamping behaviour.
func TestParseLimit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		def  int64
		max  int64
		want int64
	}{
		{"", 100, 1000, 100},       // default
		{"50", 100, 1000, 50},      // explicit
		{"99999", 100, 1000, 1000}, // clamped to max
		{"-5", 100, 1000, 100},     // negative -> default
		{"abc", 100, 1000, 100},    // junk -> default
		{"0", 100, 1000, 100},      // zero -> default (we treat as invalid)
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/x?limit="+c.raw, nil)
		got := parseLimit(r, c.def, c.max)
		if got != c.want {
			t.Errorf("parseLimit(raw=%q) = %d, want %d", c.raw, got, c.want)
		}
	}
}
