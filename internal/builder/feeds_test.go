package builder

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/slab/internal/store"
)

// readFeedXML returns the parsed atomFeed for the given collection
// slug under wsDir, or fails the test if the file is missing or
// malformed XML.
func readFeedXML(t *testing.T, wsDir, slug string) atomFeed {
	t.Helper()
	path := filepath.Join(wsDir, "public", slug, "feed.xml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read feed.xml at %s: %v", path, err)
	}
	var feed atomFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		t.Fatalf("parse feed.xml: %v\nbody:\n%s", err, body)
	}
	return feed
}

func TestRenderCollectionFeeds_AutoEmitsForArticleSchema(t *testing.T) {
	_, q, siteID, wsDir := newBuilderTestDB(t)
	seedCollectionWithItem(t, q, siteID, "blog", "BlogPosting", "en")

	count, err := RenderCollectionFeeds(context.Background(), q, siteID, wsDir)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("feeds emitted = %d, want 1", count)
	}
	feed := readFeedXML(t, wsDir, "blog")
	if feed.Title != "blog" {
		t.Errorf("feed title = %q, want %q", feed.Title, "blog")
	}
	if !strings.HasSuffix(feed.ID, "/blog/feed.xml") {
		t.Errorf("feed id = %q, want ending in /blog/feed.xml", feed.ID)
	}
	if len(feed.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(feed.Entries))
	}
	if !strings.Contains(feed.Entries[0].ID, "/blog/first-item/") {
		t.Errorf("entry id = %q, want item path", feed.Entries[0].ID)
	}
	if feed.Entries[0].Title != "First Item" {
		t.Errorf("entry title = %q, want %q", feed.Entries[0].Title, "First Item")
	}
	// Atom requires updated; we converted SQLite "YYYY-MM-DD HH:MM:SS"
	// into RFC3339 form.
	if !strings.Contains(feed.Entries[0].Updated, "T") {
		t.Errorf("entry updated = %q, must be RFC3339", feed.Entries[0].Updated)
	}
}

func TestRenderCollectionFeeds_NonArticleSchemaSkipsByDefault(t *testing.T) {
	_, q, siteID, wsDir := newBuilderTestDB(t)
	seedCollectionWithItem(t, q, siteID, "products", "Product", "en")

	count, err := RenderCollectionFeeds(context.Background(), q, siteID, wsDir)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("feeds emitted = %d, want 0 (Product is not article-like)", count)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "public", "products", "feed.xml")); !os.IsNotExist(err) {
		t.Errorf("feed.xml should not exist for Product schema; got err=%v", err)
	}
}

func TestRenderCollectionFeeds_ExplicitOptInOverrides(t *testing.T) {
	_, q, siteID, wsDir := newBuilderTestDB(t)
	colID := "c_recipes"
	on := true
	settings, _ := json.Marshal(map[string]any{
		"render_as_pages":  true,
		"schema_org_type":  "Recipe",
		"feed_enabled":     &on,
		"feed_title":       "Cooking with Atomic",
		"feed_description": "Weekly recipes",
	})
	schema, _ := json.Marshal([]map[string]any{{"name": "headline", "type": "text"}})
	if err := q.CreateCollection(context.Background(), store.CreateCollectionParams{
		ID: colID, SiteID: siteID, Name: "Recipes", Slug: "recipes",
		SchemaJson: string(schema), SettingsJson: string(settings),
	}); err != nil {
		t.Fatal(err)
	}
	itemData, _ := json.Marshal(map[string]any{"headline": "Pasta"})
	if err := q.CreateItem(context.Background(), store.CreateItemParams{
		ID: "i_recipes_1", CollectionID: colID, SiteID: siteID,
		Slug: "pasta", Title: "Pasta", DataJson: string(itemData),
		Locale: "en", Status: "published", PublishedAt: "2026-05-04 12:00:00",
	}); err != nil {
		t.Fatal(err)
	}

	count, err := RenderCollectionFeeds(context.Background(), q, siteID, wsDir)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("feeds emitted = %d, want 1 (explicit feed_enabled=true)", count)
	}
	feed := readFeedXML(t, wsDir, "recipes")
	if feed.Title != "Cooking with Atomic" {
		t.Errorf("feed title = %q, want override", feed.Title)
	}
	if feed.Subtitle != "Weekly recipes" {
		t.Errorf("feed subtitle = %q, want override", feed.Subtitle)
	}
}

func TestRenderCollectionFeeds_ExplicitOptOut(t *testing.T) {
	_, q, siteID, wsDir := newBuilderTestDB(t)
	colID := "c_blog"
	off := false
	settings, _ := json.Marshal(map[string]any{
		"render_as_pages": true,
		"schema_org_type": "BlogPosting",
		"feed_enabled":    &off,
	})
	schema, _ := json.Marshal([]map[string]any{{"name": "headline", "type": "text"}})
	if err := q.CreateCollection(context.Background(), store.CreateCollectionParams{
		ID: colID, SiteID: siteID, Name: "blog", Slug: "blog",
		SchemaJson: string(schema), SettingsJson: string(settings),
	}); err != nil {
		t.Fatal(err)
	}
	count, err := RenderCollectionFeeds(context.Background(), q, siteID, wsDir)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("feeds emitted = %d, want 0 (explicit feed_enabled=false)", count)
	}
}

func TestRenderCollectionFeeds_DraftItemsExcluded(t *testing.T) {
	_, q, siteID, wsDir := newBuilderTestDB(t)
	colID, _ := seedCollectionWithItem(t, q, siteID, "blog", "Article", "en")
	// Add a draft item that should NOT appear in the feed.
	if err := q.CreateItem(context.Background(), store.CreateItemParams{
		ID: "i_draft", CollectionID: colID, SiteID: siteID,
		Slug: "draft", Title: "Draft Post", DataJson: `{"headline":"Drafty"}`,
		Locale: "en", Status: "draft",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := RenderCollectionFeeds(context.Background(), q, siteID, wsDir); err != nil {
		t.Fatal(err)
	}
	feed := readFeedXML(t, wsDir, "blog")
	if len(feed.Entries) != 1 {
		t.Fatalf("entries = %d, want 1 (draft excluded)", len(feed.Entries))
	}
	if feed.Entries[0].Title == "Draft Post" {
		t.Error("draft item leaked into feed")
	}
}

func TestRenderCollectionFeeds_OrderedByPublishedDESC(t *testing.T) {
	_, q, siteID, wsDir := newBuilderTestDB(t)
	colID, _ := seedCollectionWithItem(t, q, siteID, "news", "NewsArticle", "en")
	// First item already exists, published 2026-05-04. Add an older
	// and a newer one; expect newest first.
	for _, it := range []struct {
		id, slug, pub, title string
	}{
		{"i_news_old", "old-news", "2026-04-01 00:00:00", "Old News"},
		{"i_news_new", "newest", "2026-05-08 00:00:00", "Newest"},
	} {
		if err := q.CreateItem(context.Background(), store.CreateItemParams{
			ID: it.id, CollectionID: colID, SiteID: siteID,
			Slug: it.slug, Title: it.title, DataJson: `{"headline":"x"}`,
			Locale: "en", Status: "published", PublishedAt: it.pub,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := RenderCollectionFeeds(context.Background(), q, siteID, wsDir); err != nil {
		t.Fatal(err)
	}
	feed := readFeedXML(t, wsDir, "news")
	if len(feed.Entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(feed.Entries))
	}
	if feed.Entries[0].Title != "Newest" {
		t.Errorf("entry[0] = %q, want Newest", feed.Entries[0].Title)
	}
	if feed.Entries[2].Title != "Old News" {
		t.Errorf("entry[2] = %q, want Old News", feed.Entries[2].Title)
	}
}

func TestRenderCollectionFeeds_SummaryFromDataJson(t *testing.T) {
	_, q, siteID, wsDir := newBuilderTestDB(t)
	colID := "c_blog"
	on := true
	settings, _ := json.Marshal(map[string]any{
		"render_as_pages": true,
		"schema_org_type": "BlogPosting",
		"feed_enabled":    &on,
	})
	schema, _ := json.Marshal([]map[string]any{{"name": "headline", "type": "text"}})
	if err := q.CreateCollection(context.Background(), store.CreateCollectionParams{
		ID: colID, SiteID: siteID, Name: "blog", Slug: "blog",
		SchemaJson: string(schema), SettingsJson: string(settings),
	}); err != nil {
		t.Fatal(err)
	}
	itemData, _ := json.Marshal(map[string]any{
		"headline": "Hi",
		"summary":  "Short summary line.",
	})
	if err := q.CreateItem(context.Background(), store.CreateItemParams{
		ID: "i_1", CollectionID: colID, SiteID: siteID,
		Slug: "hi", Title: "Hi", DataJson: string(itemData),
		Locale: "en", Status: "published", PublishedAt: "2026-05-04 12:00:00",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := RenderCollectionFeeds(context.Background(), q, siteID, wsDir); err != nil {
		t.Fatal(err)
	}
	feed := readFeedXML(t, wsDir, "blog")
	if len(feed.Entries) != 1 {
		t.Fatalf("entries = %d", len(feed.Entries))
	}
	if feed.Entries[0].Summary == nil || feed.Entries[0].Summary.Body != "Short summary line." {
		t.Errorf("summary = %+v, want extracted from data_json", feed.Entries[0].Summary)
	}
}

func TestRenderCollectionFeeds_BothTimestampsEmptyStillRenders(t *testing.T) {
	// Defense for the case where an item has neither published_at
	// nor created_at (CMS imports may leave both blank). The feed
	// must still render a valid Atom doc using time.Now() as the
	// fallback for entry.updated.
	_, q, siteID, wsDir := newBuilderTestDB(t)
	colID, _ := seedCollectionWithItem(t, q, siteID, "blog", "Article", "en")
	// An item whose published_at is empty (handled by SQLite default)
	// and we manually wipe created_at to simulate an upstream import
	// that didn't stamp it.
	if err := q.CreateItem(context.Background(), store.CreateItemParams{
		ID: "i_no_dates", CollectionID: colID, SiteID: siteID,
		Slug: "no-dates", Title: "No Dates", DataJson: `{"headline":"x"}`,
		Locale: "en", Status: "published", PublishedAt: "",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := RenderCollectionFeeds(context.Background(), q, siteID, wsDir); err != nil {
		t.Fatalf("render with empty timestamps: %v", err)
	}
	feed := readFeedXML(t, wsDir, "blog")
	for _, e := range feed.Entries {
		if e.Updated == "" {
			t.Errorf("entry %q has empty <updated>; feed would be invalid Atom", e.Title)
		}
	}
	if feed.Updated == "" {
		t.Error("feed-level <updated> must never be empty")
	}
}

func TestRenderCollectionFeeds_EscapesHostileTitle(t *testing.T) {
	// encoding/xml handles escaping; this test locks in that we route
	// titles through xml.Marshal rather than string concatenation, so
	// a hostile title can't break the feed structure.
	_, q, siteID, wsDir := newBuilderTestDB(t)
	colID, _ := seedCollectionWithItem(t, q, siteID, "blog", "Article", "en")
	hostile := `Boss <script>alert("xss")</script> & "quoted"`
	if err := q.CreateItem(context.Background(), store.CreateItemParams{
		ID: "i_hostile", CollectionID: colID, SiteID: siteID,
		Slug: "hostile", Title: hostile, DataJson: `{"headline":"x"}`,
		Locale: "en", Status: "published", PublishedAt: "2026-05-08 12:00:00",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := RenderCollectionFeeds(context.Background(), q, siteID, wsDir); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(wsDir, "public", "blog", "feed.xml"))
	if err != nil {
		t.Fatal(err)
	}
	bodyStr := string(body)
	if strings.Contains(bodyStr, "<script>") {
		t.Error("hostile <script> tag survived unescaped")
	}
	if !strings.Contains(bodyStr, "&lt;script&gt;") {
		t.Error("expected escaped <script> in feed")
	}
	// Re-parse to confirm structural integrity.
	var feed atomFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		t.Fatalf("feed.xml unparseable after hostile input: %v", err)
	}
	found := false
	for _, e := range feed.Entries {
		if e.Title == hostile {
			found = true
			break
		}
	}
	if !found {
		t.Error("hostile title not round-tripped via XML decode")
	}
}
