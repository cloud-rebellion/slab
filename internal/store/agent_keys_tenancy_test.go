package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	dbpkg "github.com/bright-interaction/slab/internal/db"
	"github.com/bright-interaction/slab/internal/store"
)

func openTenancyTestDB(t *testing.T) (*sql.DB, *store.Queries) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.sqlite")
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

// RevokeAgentKey is reachable on a site-scoped route, but the route guard only
// proves the caller may touch THAT site. Before the site predicate existed the
// UPDATE matched on key id alone, so a tenant authorised for their own site
// could revoke any other tenant's agent key by putting its id in the path and
// take down that tenant's agent integrations. This asserts the query itself
// refuses, not just the handler, because the query is the layer a future
// handler cannot get wrong.
func TestDeactivateAgentKeyIsSiteScoped(t *testing.T) {
	sqlDB, queries := openTenancyTestDB(t)
	ctx := context.Background()

	for _, s := range []string{"site-victim", "site-attacker"} {
		if _, err := sqlDB.Exec(`INSERT INTO sites (id, name, slug) VALUES (?, ?, ?)`, s, s, s); err != nil {
			t.Fatalf("seed site %s: %v", s, err)
		}
	}
	if err := queries.CreateAgentKey(ctx, store.CreateAgentKeyParams{
		ID:           "key-victim",
		SiteID:       "site-victim",
		Name:         "victim integration",
		KeyHash:      "hash-victim",
		Capabilities: "[]",
	}); err != nil {
		t.Fatalf("seed victim key: %v", err)
	}

	// The attack: authorised for site-attacker, aiming at site-victim's key.
	n, err := queries.DeactivateAgentKey(ctx, store.DeactivateAgentKeyParams{
		ID:     "key-victim",
		SiteID: "site-attacker",
	})
	if err != nil {
		t.Fatalf("cross-tenant revoke returned an error instead of zero rows: %v", err)
	}
	if n != 0 {
		t.Errorf("cross-tenant revoke affected %d rows, want 0: a tenant can disable another tenant's agent key", n)
	}

	key, err := queries.GetAgentKeyByID(ctx, "key-victim")
	if err != nil {
		t.Fatalf("re-read victim key: %v", err)
	}
	if key.IsActive != 1 {
		t.Errorf("victim key is_active = %d, want 1: the cross-tenant revoke went through", key.IsActive)
	}

	// The owner must still be able to revoke their own key.
	n, err = queries.DeactivateAgentKey(ctx, store.DeactivateAgentKeyParams{
		ID:     "key-victim",
		SiteID: "site-victim",
	})
	if err != nil {
		t.Fatalf("same-tenant revoke: %v", err)
	}
	if n != 1 {
		t.Errorf("same-tenant revoke affected %d rows, want 1: the fix broke the legitimate path", n)
	}
	key, err = queries.GetAgentKeyByID(ctx, "key-victim")
	if err != nil {
		t.Fatalf("re-read after owner revoke: %v", err)
	}
	if key.IsActive != 0 {
		t.Errorf("after owner revoke is_active = %d, want 0", key.IsActive)
	}
}
