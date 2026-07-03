package revisions

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	dbpkg "github.com/bright-interaction/slab/internal/db"
	"github.com/bright-interaction/slab/internal/store"
)

func openTestDB(t *testing.T) (*sql.DB, *store.Queries) {
	t.Helper()
	dir := t.TempDir()
	sqlDB, err := sql.Open("sqlite", filepath.Join(dir, "t.sqlite")+"?_journal_mode=WAL&_busy_timeout=5000&_pragma=foreign_keys(1)")
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

func seedSite(t *testing.T, sqlDB *sql.DB, siteID string) {
	t.Helper()
	if _, err := sqlDB.Exec(`INSERT INTO sites (id, name, slug) VALUES (?, ?, ?)`, siteID, "Test", siteID); err != nil {
		t.Fatalf("seed site: %v", err)
	}
}

func TestNew_NilStoreReturnsNilRecorder(t *testing.T) {
	if r := New(nil); r != nil {
		t.Errorf("nil store should produce nil recorder; got %+v", r)
	}
}

func TestRecord_NilRecorderReturnsErrorNotPanic(t *testing.T) {
	var r *Recorder
	err := r.Record(context.Background(), RecordParams{})
	if err == nil {
		t.Error("nil recorder Record should return an error, not panic")
	}
}

func TestRecord_ValidatesRequiredFields(t *testing.T) {
	_, q := openTestDB(t)
	r := New(q)
	cases := []struct {
		name   string
		params RecordParams
	}{
		{"empty site_id", RecordParams{EntityType: EntityTypePage, EntityID: "p1", Snapshot: map[string]any{"k": "v"}}},
		{"empty entity_id", RecordParams{SiteID: "s1", EntityType: EntityTypePage, Snapshot: map[string]any{"k": "v"}}},
		{"unknown entity_type", RecordParams{SiteID: "s1", EntityType: "spaceship", EntityID: "e1", Snapshot: map[string]any{"k": "v"}}},
		{"nil snapshot", RecordParams{SiteID: "s1", EntityType: EntityTypePage, EntityID: "e1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := r.Record(context.Background(), c.params); err == nil {
				t.Error("expected validation error")
			}
		})
	}
}

func TestRecord_AcceptsAllSupportedEntityTypes(t *testing.T) {
	sqlDB, q := openTestDB(t)
	siteID := "site_entity_types"
	seedSite(t, sqlDB, siteID)
	r := New(q)
	ctx := context.Background()
	for _, et := range []string{EntityTypePage, EntityTypeBlock, EntityTypePageLocale, EntityTypeBlockLocale} {
		if err := r.Record(ctx, RecordParams{
			SiteID:     siteID,
			EntityType: et,
			EntityID:   "entity-" + et,
			Snapshot:   map[string]any{"name": et},
		}); err != nil {
			t.Errorf("entity_type %s: %v", et, err)
		}
	}
}

func TestRecord_IncrementsVersionNumber(t *testing.T) {
	sqlDB, q := openTestDB(t)
	siteID := "site_versions"
	seedSite(t, sqlDB, siteID)
	r := New(q)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := r.Record(ctx, RecordParams{
			SiteID:        siteID,
			EntityType:    EntityTypePage,
			EntityID:      "page1",
			Snapshot:      map[string]any{"title": "v" + string(rune('0'+i))},
			ChangeSummary: "edit",
		}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	row := sqlDB.QueryRow(`SELECT MAX(version_number) FROM entity_revisions WHERE site_id=? AND entity_type='page' AND entity_id='page1'`, siteID)
	var maxV int64
	if err := row.Scan(&maxV); err != nil {
		t.Fatal(err)
	}
	if maxV != 3 {
		t.Errorf("expected 3 revisions, got max version %d", maxV)
	}
}

func TestRecord_RetentionCapPrunesOldest(t *testing.T) {
	// Insert MaxPerEntity + 5 revisions. After every insert the prune
	// step trims the tail; final count must equal the cap.
	sqlDB, q := openTestDB(t)
	siteID := "site_retention"
	seedSite(t, sqlDB, siteID)
	r := New(q)
	ctx := context.Background()
	for i := 0; i < MaxPerEntity+5; i++ {
		if err := r.Record(ctx, RecordParams{
			SiteID:     siteID,
			EntityType: EntityTypeBlock,
			EntityID:   "blk1",
			Snapshot:   map[string]any{"i": i},
		}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	var count int64
	row := sqlDB.QueryRow(`SELECT COUNT(*) FROM entity_revisions WHERE site_id=? AND entity_type='block' AND entity_id='blk1'`, siteID)
	if err := row.Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != MaxPerEntity {
		t.Errorf("retention cap broke: got %d rows, want %d", count, MaxPerEntity)
	}
}

func TestRecord_MarshalsArbitraryStructs(t *testing.T) {
	sqlDB, q := openTestDB(t)
	siteID := "site_struct"
	seedSite(t, sqlDB, siteID)
	r := New(q)
	type sample struct {
		Title string   `json:"title"`
		Tags  []string `json:"tags"`
	}
	if err := r.Record(context.Background(), RecordParams{
		SiteID:     siteID,
		EntityType: EntityTypePage,
		EntityID:   "p2",
		Snapshot:   sample{Title: "Hello", Tags: []string{"a", "b"}},
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	row := sqlDB.QueryRow(`SELECT snapshot_json FROM entity_revisions WHERE site_id=? AND entity_id='p2'`, siteID)
	var snap string
	if err := row.Scan(&snap); err != nil {
		t.Fatal(err)
	}
	if snap != `{"title":"Hello","tags":["a","b"]}` {
		t.Errorf("snapshot JSON: got %s", snap)
	}
}

func TestNewID_FormatAndUniqueness(t *testing.T) {
	a := newID()
	b := newID()
	if a == b {
		t.Errorf("newID collisions: %q == %q", a, b)
	}
	if len(a) != 24 {
		t.Errorf("newID length: got %d, want 24 hex chars (12 bytes)", len(a))
	}
}
