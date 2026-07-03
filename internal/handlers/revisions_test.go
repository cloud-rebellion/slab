package handlers

import (
	"context"
	"testing"

	"github.com/bright-interaction/slab/internal/revisions"
	"github.com/bright-interaction/slab/internal/store"
)

// TestRevisions_RecorderWritesSnapshotOnPageUpdate exercises the
// happy path: a page is created, the recorder writes a snapshot when
// UpdatePage runs, and ListEntityRevisions returns the row newest-first
// with the pre-update values preserved.
//
// Sprint 1 of the WP/Webflow replacement roadmap.
func TestRevisions_RecorderWritesSnapshotOnPageUpdate(t *testing.T) {
	_, q := setupDeployTestDB(t)
	siteID := "revsite0000000000aaaa01"
	seedSite(t, q, siteID)
	rec := revisions.New(q)
	ctx := context.Background()

	pageID := "page-v1-aaaa00000001"
	if err := q.CreatePage(ctx, store.CreatePageParams{
		ID:     pageID,
		SiteID: siteID,
		Title:  "Original Title",
		Slug:   "original",
		Layout: "default",
	}); err != nil {
		t.Fatalf("create page: %v", err)
	}

	// Simulate the handler's update path: snapshot current row first.
	cur, err := q.GetPageByID(ctx, pageID)
	if err != nil {
		t.Fatalf("get page: %v", err)
	}
	if err := rec.Record(ctx, revisions.RecordParams{
		SiteID:        siteID,
		EntityType:    revisions.EntityTypePage,
		EntityID:      pageID,
		Snapshot:      cur,
		ChangeSummary: "title edit",
		CreatedBy:     "user:tester",
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := q.UpdatePage(ctx, store.UpdatePageParams{
		Title:            "Edited Title",
		Slug:             cur.Slug,
		Status:           cur.Status,
		Layout:           cur.Layout,
		ID:               pageID,
		ShowInNav:        cur.ShowInNav,
		MetaTitle:        cur.MetaTitle,
		MetaDescription:  cur.MetaDescription,
		OgImageID:        cur.OgImageID,
		SortOrder:        cur.SortOrder,
		NavLabel:         cur.NavLabel,
		NoIndex:          cur.NoIndex,
		CanonicalUrl:     cur.CanonicalUrl,
		HideGlobalBlocks: cur.HideGlobalBlocks,
	}); err != nil {
		t.Fatalf("update page: %v", err)
	}

	rows, err := q.ListEntityRevisions(ctx, store.ListEntityRevisionsParams{
		SiteID:     siteID,
		EntityType: revisions.EntityTypePage,
		EntityID:   pageID,
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("revisions count = %d; want 1", len(rows))
	}
	if rows[0].VersionNumber != 1 {
		t.Errorf("version_number = %d; want 1", rows[0].VersionNumber)
	}
	if rows[0].CreatedBy != "user:tester" {
		t.Errorf("created_by = %q; want user:tester", rows[0].CreatedBy)
	}
	if rows[0].ChangeSummary != "title edit" {
		t.Errorf("change_summary = %q; want title edit", rows[0].ChangeSummary)
	}
	// Snapshot must contain the pre-update title.
	if want := "Original Title"; !contains(rows[0].SnapshotJson, want) {
		t.Errorf("snapshot_json missing %q; got %s", want, rows[0].SnapshotJson)
	}
}

// TestRevisions_RetentionPrunesOverFifty exercises the prune-on-insert
// path. After 60 inserts, only 50 most-recent revisions remain.
func TestRevisions_RetentionPrunesOverFifty(t *testing.T) {
	_, q := setupDeployTestDB(t)
	siteID := "revsite0000000000aaaa02"
	seedSite(t, q, siteID)
	rec := revisions.New(q)
	ctx := context.Background()

	pageID := "page-prune-aaaa0001"
	if err := q.CreatePage(ctx, store.CreatePageParams{
		ID: pageID, SiteID: siteID, Title: "p", Slug: "p", Layout: "default",
	}); err != nil {
		t.Fatalf("create page: %v", err)
	}

	for i := 0; i < 60; i++ {
		if err := rec.Record(ctx, revisions.RecordParams{
			SiteID:     siteID,
			EntityType: revisions.EntityTypePage,
			EntityID:   pageID,
			Snapshot:   map[string]any{"i": i},
			CreatedBy:  "user:loop",
		}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}

	count, err := q.CountEntityRevisions(ctx, store.CountEntityRevisionsParams{
		SiteID: siteID, EntityType: revisions.EntityTypePage, EntityID: pageID,
	})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != revisions.MaxPerEntity {
		t.Errorf("retained count = %d; want %d", count, revisions.MaxPerEntity)
	}
}

// TestRevisions_RestorePathSnapshotsCurrentThenApplies covers the
// handler's restore semantics: current state is snapshotted (so
// restoring is itself undoable), then the prior snapshot is applied
// via UpdatePage, producing the expected final row state.
func TestRevisions_RestorePathSnapshotsCurrentThenApplies(t *testing.T) {
	_, q := setupDeployTestDB(t)
	siteID := "revsite0000000000aaaa03"
	seedSite(t, q, siteID)
	rec := revisions.New(q)
	ctx := context.Background()
	h := NewRevisionsHandler(nil, q, rec)

	pageID := "page-rest-aaaa00000001"
	if err := q.CreatePage(ctx, store.CreatePageParams{
		ID: pageID, SiteID: siteID, Title: "Original", Slug: "p", Layout: "default",
	}); err != nil {
		t.Fatalf("create page: %v", err)
	}
	// Record v1 (Original)
	cur, _ := q.GetPageByID(ctx, pageID)
	_ = rec.Record(ctx, revisions.RecordParams{
		SiteID: siteID, EntityType: revisions.EntityTypePage, EntityID: pageID,
		Snapshot: cur, CreatedBy: "user:tester",
	})
	// Mutate to Edited
	_ = q.UpdatePage(ctx, store.UpdatePageParams{
		Title: "Edited", Slug: "p", Status: "draft", Layout: "default", ID: pageID, ShowInNav: 1,
	})

	// Restore v1 via handler
	result, err := h.RestoreForAgent(ctx, siteID, revisions.EntityTypePage, pageID, 1, "agent:test-key")
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if result["entity_type"] != revisions.EntityTypePage {
		t.Errorf("entity_type in result = %v", result["entity_type"])
	}

	got, _ := q.GetPageByID(ctx, pageID)
	if got.Title != "Original" {
		t.Errorf("title after restore = %q; want Original", got.Title)
	}

	// Two revisions should exist now: v1 (Original) + v2 (pre-restore Edited
	// snapshot the handler captured before applying v1's content).
	count, _ := q.CountEntityRevisions(ctx, store.CountEntityRevisionsParams{
		SiteID: siteID, EntityType: revisions.EntityTypePage, EntityID: pageID,
	})
	if count != 2 {
		t.Errorf("revisions count after restore = %d; want 2 (v1 original + v2 pre-restore)", count)
	}
}

// TestRevisions_CrossTenantBlocked locks the cross-tenant guard: a
// user with access to site A cannot list revisions belonging to a
// page in site B even if they know the page ID.
func TestRevisions_CrossTenantBlocked(t *testing.T) {
	_, q := setupDeployTestDB(t)
	siteA := "revsite0000000000aaaa04"
	siteB := "revsite0000000000aaaa05"
	seedSite(t, q, siteA)
	seedSite(t, q, siteB)
	rec := revisions.New(q)
	ctx := context.Background()
	h := NewRevisionsHandler(nil, q, rec)

	pageID := "page-cross-aaaa00000001"
	if err := q.CreatePage(ctx, store.CreatePageParams{
		ID: pageID, SiteID: siteB, Title: "B Page", Slug: "p", Layout: "default",
	}); err != nil {
		t.Fatalf("create page in B: %v", err)
	}

	_, err := h.ListLatestForEntity(ctx, siteA, revisions.EntityTypePage, pageID, 50)
	if err == nil {
		t.Errorf("expected cross-tenant error; got nil")
	}
}

// TestRevisions_InvalidEntityTypeRejected locks the contract that the
// entity_type CHECK constraint is mirrored at the handler boundary so
// the agent gets a friendly error rather than a SQLite constraint dump.
func TestRevisions_InvalidEntityTypeRejected(t *testing.T) {
	_, q := setupDeployTestDB(t)
	siteID := "revsite0000000000aaaa06"
	seedSite(t, q, siteID)
	rec := revisions.New(q)
	ctx := context.Background()
	h := NewRevisionsHandler(nil, q, rec)

	_, err := h.ListLatestForEntity(ctx, siteID, "site_branding", "anything", 50)
	if err == nil {
		t.Errorf("expected invalid entity_type error; got nil")
	}
}

// contains is a tiny helper avoiding a bytes/strings dependency. The
// snapshot_json column is a single JSON string so substring matching
// is a sufficient signal for the field-was-preserved check.
func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
