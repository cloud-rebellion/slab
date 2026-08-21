package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	_ "modernc.org/sqlite"

	"github.com/bright-interaction/slab/internal/config"
	"github.com/bright-interaction/slab/internal/store"
	"github.com/bright-interaction/slab/internal/webhook"
)

func newCollectionHandlerForTest(t *testing.T) (*CollectionHandler, *store.Queries, string) {
	t.Helper()
	sqlDB, _ := newSchemaDB(t, "collections.db")
	q := store.New(sqlDB)
	siteID := "abcdef0123456789abcdef02"
	if err := q.CreateSite(context.Background(), store.CreateSiteParams{
		ID: siteID, Name: "T", Slug: "t-cols", PrimaryColor: "#000",
		SecondaryColor: "#000", BgColor: "#FFF", TextColor: "#111",
		FontHeading: "Inter", FontBody: "Inter", Lang: "en",
	}); err != nil {
		t.Fatalf("create site: %v", err)
	}
	cfg := &config.Config{BaseURL: "http://localhost:8080"}
	return NewCollectionHandler(cfg, q), q, siteID
}

func withColParams(siteID, colID string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("siteID", siteID)
		if colID != "" {
			rctx.URLParams.Add("collectionID", colID)
		}
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
		h(w, r)
	}
}

func withItemParams(siteID, colID, itemID string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("siteID", siteID)
		rctx.URLParams.Add("collectionID", colID)
		rctx.URLParams.Add("itemID", itemID)
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
		h(w, r)
	}
}

func postJSONCol(handler http.HandlerFunc, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler(rr, req)
	return rr
}

func TestCollections_CRUDHappyPath(t *testing.T) {
	h, q, siteID := newCollectionHandlerForTest(t)

	// Create
	body := map[string]any{
		"name": "Case Studies",
		"slug": "case-studies",
		"schema": []map[string]any{
			{"name": "headline", "type": "text", "label": "Headline", "required": true},
			{"name": "body", "type": "richtext", "label": "Body"},
		},
		"settings": map[string]any{"render_as_pages": true, "schema_org_type": "Article"},
	}
	rr := postJSONCol(withColParams(siteID, "", h.CreateCollection), body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &created)
	colID, _ := created["id"].(string)
	if colID == "" {
		t.Fatal("no id in create response")
	}

	// List
	listReq := httptest.NewRequest(http.MethodGet, "/", nil)
	listRR := httptest.NewRecorder()
	withColParams(siteID, "", h.ListCollections)(listRR, listReq)
	if listRR.Code != http.StatusOK {
		t.Fatalf("list expected 200, got %d", listRR.Code)
	}

	// Get
	getRR := httptest.NewRecorder()
	withColParams(siteID, colID, h.GetCollection)(getRR, httptest.NewRequest(http.MethodGet, "/", nil))
	if getRR.Code != http.StatusOK {
		t.Fatalf("get expected 200, got %d", getRR.Code)
	}

	// Delete
	delRR := httptest.NewRecorder()
	withColParams(siteID, colID, h.DeleteCollection)(delRR, httptest.NewRequest(http.MethodDelete, "/", nil))
	if delRR.Code != http.StatusOK {
		t.Fatalf("delete expected 200, got %d", delRR.Code)
	}
	rows, _ := q.ListCollectionsBySite(context.Background(), siteID)
	if len(rows) != 0 {
		t.Errorf("expected 0 collections after delete, got %d", len(rows))
	}
}

func TestCollections_CrossSiteIsolation(t *testing.T) {
	h, q, siteA := newCollectionHandlerForTest(t)
	siteB := "abcdef0123456789abcdef03"
	if err := q.CreateSite(context.Background(), store.CreateSiteParams{
		ID: siteB, Name: "B", Slug: "b-cols", PrimaryColor: "#000",
		SecondaryColor: "#000", BgColor: "#FFF", TextColor: "#111",
		FontHeading: "Inter", FontBody: "Inter", Lang: "en",
	}); err != nil {
		t.Fatal(err)
	}
	// Create collection in site A.
	rr := postJSONCol(withColParams(siteA, "", h.CreateCollection), map[string]any{
		"name": "A col", "slug": "a-col",
		"schema": []map[string]any{{"name": "x", "type": "text"}},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create A expected 201, got %d", rr.Code)
	}
	var created map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &created)
	colID, _ := created["id"].(string)

	// Access from site B should 404.
	getRR := httptest.NewRecorder()
	withColParams(siteB, colID, h.GetCollection)(getRR, httptest.NewRequest(http.MethodGet, "/", nil))
	if getRR.Code != http.StatusNotFound {
		t.Errorf("cross-site get expected 404, got %d", getRR.Code)
	}
}

func TestCollections_DuplicateSlugReturns409(t *testing.T) {
	h, _, siteID := newCollectionHandlerForTest(t)
	body := map[string]any{
		"name": "First", "slug": "case-studies",
		"schema": []map[string]any{{"name": "x", "type": "text"}},
	}
	rr1 := postJSONCol(withColParams(siteID, "", h.CreateCollection), body)
	if rr1.Code != http.StatusCreated {
		t.Fatalf("first expected 201, got %d", rr1.Code)
	}
	rr2 := postJSONCol(withColParams(siteID, "", h.CreateCollection), body)
	if rr2.Code != http.StatusConflict {
		t.Errorf("duplicate slug expected 409, got %d", rr2.Code)
	}
}

func seedCollection(t *testing.T, h *CollectionHandler, q *store.Queries, siteID string) string {
	t.Helper()
	rr := postJSONCol(withColParams(siteID, "", h.CreateCollection), map[string]any{
		"name": "Articles", "slug": "articles",
		"schema": []map[string]any{
			{"name": "headline", "type": "text", "required": true},
			{"name": "body", "type": "textarea"},
		},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed col expected 201, got %d", rr.Code)
	}
	var created map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &created)
	id, _ := created["id"].(string)
	return id
}

func TestCollectionItems_CRUDHappyPath(t *testing.T) {
	h, q, siteID := newCollectionHandlerForTest(t)
	colID := seedCollection(t, h, q, siteID)
	// Create item.
	rr := postJSONCol(withColParams(siteID, colID, h.CreateItem), map[string]any{
		"slug": "first-post", "title": "First", "data": map[string]any{"headline": "Hi"},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	rows, _ := q.ListItemsByCollection(context.Background(), colID)
	if len(rows) != 1 {
		t.Errorf("expected 1 item, got %d", len(rows))
	}
}

func TestCollectionItems_FieldValidation_RequiredMissing(t *testing.T) {
	h, q, siteID := newCollectionHandlerForTest(t)
	colID := seedCollection(t, h, q, siteID)
	rr := postJSONCol(withColParams(siteID, colID, h.CreateItem), map[string]any{
		"slug": "missing-required", "title": "Missing",
		"data": map[string]any{}, // no headline
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing required field, got %d", rr.Code)
	}
}

func TestCollectionItems_BulkImport_ValidatesAndTransactions(t *testing.T) {
	h, q, siteID := newCollectionHandlerForTest(t)
	colID := seedCollection(t, h, q, siteID)
	// Send 3 items where the 2nd is invalid (missing required headline).
	body := map[string]any{
		"items": []map[string]any{
			{"slug": "a", "title": "A", "data": map[string]any{"headline": "AA"}},
			{"slug": "b", "title": "B", "data": map[string]any{}}, // bad
			{"slug": "c", "title": "C", "data": map[string]any{"headline": "CC"}},
		},
	}
	rr := postJSONCol(withColParams(siteID, colID, h.BulkImport), body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bulk import expected 400 on partial failure, got %d body=%s", rr.Code, rr.Body.String())
	}
	rows, _ := q.ListItemsByCollection(context.Background(), colID)
	if len(rows) != 0 {
		t.Errorf("expected 0 rows after rollback, got %d", len(rows))
	}
}

func TestCollectionItems_BulkImport_HappyPath(t *testing.T) {
	h, q, siteID := newCollectionHandlerForTest(t)
	colID := seedCollection(t, h, q, siteID)
	body := map[string]any{
		"items": []map[string]any{
			{"slug": "a", "title": "A", "data": map[string]any{"headline": "AA"}},
			{"slug": "b", "title": "B", "data": map[string]any{"headline": "BB"}},
		},
	}
	rr := postJSONCol(withColParams(siteID, colID, h.BulkImport), body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("bulk import expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	rows, _ := q.ListItemsByCollection(context.Background(), colID)
	if len(rows) != 2 {
		t.Errorf("expected 2 rows, got %d", len(rows))
	}
}

func TestCollectionItems_LocaleVariantsUniqueness(t *testing.T) {
	h, q, siteID := newCollectionHandlerForTest(t)
	colID := seedCollection(t, h, q, siteID)
	// Same slug, two locales: allowed.
	r1 := postJSONCol(withColParams(siteID, colID, h.CreateItem), map[string]any{
		"slug": "shared", "title": "EN", "locale": "en", "data": map[string]any{"headline": "EN"},
	})
	if r1.Code != http.StatusCreated {
		t.Fatalf("en variant expected 201, got %d", r1.Code)
	}
	r2 := postJSONCol(withColParams(siteID, colID, h.CreateItem), map[string]any{
		"slug": "shared", "title": "SV", "locale": "sv", "data": map[string]any{"headline": "SV"},
	})
	if r2.Code != http.StatusCreated {
		t.Fatalf("sv variant expected 201, got %d body=%s", r2.Code, r2.Body.String())
	}
	rows, _ := q.ListItemsByCollection(context.Background(), colID)
	if len(rows) != 2 {
		t.Errorf("expected 2 rows, got %d", len(rows))
	}
}

func TestCollectionItems_BulkDelete_HappyPath(t *testing.T) {
	h, q, siteID := newCollectionHandlerForTest(t)
	colID := seedCollection(t, h, q, siteID)
	for _, slug := range []string{"a", "b", "c"} {
		postJSONCol(withColParams(siteID, colID, h.CreateItem), map[string]any{
			"slug": slug, "title": slug, "data": map[string]any{"headline": slug},
		})
	}
	rows, _ := q.ListItemsByCollection(context.Background(), colID)
	if len(rows) != 3 {
		t.Fatalf("expected 3 seed rows, got %d", len(rows))
	}
	ids := []string{rows[0].ID, rows[1].ID}
	rr := postJSONCol(withColParams(siteID, colID, h.BulkDeleteItems), map[string]any{"ids": ids})
	if rr.Code != http.StatusOK {
		t.Fatalf("bulk-delete expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	after, _ := q.ListItemsByCollection(context.Background(), colID)
	if len(after) != 1 {
		t.Errorf("expected 1 row after bulk-delete, got %d", len(after))
	}
}

func TestCollectionItems_BulkDelete_EmitsWebhookPerRow(t *testing.T) {
	h, q, siteID := newCollectionHandlerForTest(t)
	colID := seedCollection(t, h, q, siteID)
	for _, slug := range []string{"a", "b"} {
		postJSONCol(withColParams(siteID, colID, h.CreateItem), map[string]any{
			"slug": slug, "title": slug, "data": map[string]any{"headline": slug},
		})
	}
	rows, _ := q.ListItemsByCollection(context.Background(), colID)

	if err := q.CreateWebhookSubscription(context.Background(), store.CreateWebhookSubscriptionParams{
		ID: "sub-bulk-del-1", SiteID: siteID,
		TargetUrl:      "https://example.invalid/hook",
		Secret:         "shh",
		EventTypesJson: `["collection_item.deleted"]`,
		Active:         1,
	}); err != nil {
		t.Fatalf("seed sub: %v", err)
	}

	prev := activeEmitter
	SetWebhookEmitter(webhook.NewEmitter(q))
	t.Cleanup(func() { activeEmitter = prev })

	rr := postJSONCol(withColParams(siteID, colID, h.BulkDeleteItems), map[string]any{
		"ids": []string{rows[0].ID, rows[1].ID},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("bulk-delete expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	deliveries, err := q.ListWebhookDeliveriesBySite(context.Background(), store.ListWebhookDeliveriesBySiteParams{
		SiteID: siteID, Limit: 50,
	})
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	count := 0
	for _, d := range deliveries {
		if d.EventType == "collection_item.deleted" {
			count++
		}
	}
	if count != 2 {
		t.Errorf("expected 2 collection_item.deleted deliveries, got %d", count)
	}
}

func TestCollectionItems_BulkDelete_CrossCollectionForbidden(t *testing.T) {
	h, q, siteID := newCollectionHandlerForTest(t)
	colA := seedCollection(t, h, q, siteID)
	rrCol := postJSONCol(withColParams(siteID, "", h.CreateCollection), map[string]any{
		"name": "Other", "slug": "other",
		"schema": []map[string]any{{"name": "headline", "type": "text", "required": true}},
	})
	if rrCol.Code != http.StatusCreated {
		t.Fatalf("seed colB expected 201, got %d", rrCol.Code)
	}
	var colBObj map[string]any
	_ = json.Unmarshal(rrCol.Body.Bytes(), &colBObj)
	colB, _ := colBObj["id"].(string)

	postJSONCol(withColParams(siteID, colA, h.CreateItem), map[string]any{
		"slug": "in-a", "title": "A", "data": map[string]any{"headline": "A"},
	})
	postJSONCol(withColParams(siteID, colB, h.CreateItem), map[string]any{
		"slug": "in-b", "title": "B", "data": map[string]any{"headline": "B"},
	})
	rowsA, _ := q.ListItemsByCollection(context.Background(), colA)
	rowsB, _ := q.ListItemsByCollection(context.Background(), colB)

	rr := postJSONCol(withColParams(siteID, colA, h.BulkDeleteItems), map[string]any{
		"ids": []string{rowsA[0].ID, rowsB[0].ID},
	})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 cross-collection, got %d body=%s", rr.Code, rr.Body.String())
	}
	stillA, _ := q.ListItemsByCollection(context.Background(), colA)
	stillB, _ := q.ListItemsByCollection(context.Background(), colB)
	if len(stillA) != 1 || len(stillB) != 1 {
		t.Errorf("rows must be untouched on abort, got A=%d B=%d", len(stillA), len(stillB))
	}
}

func TestCollectionItems_StatusGate(t *testing.T) {
	h, q, siteID := newCollectionHandlerForTest(t)
	colID := seedCollection(t, h, q, siteID)
	// One draft, one published.
	postJSONCol(withColParams(siteID, colID, h.CreateItem), map[string]any{
		"slug": "draft-one", "title": "Draft", "status": "draft", "data": map[string]any{"headline": "D"},
	})
	postJSONCol(withColParams(siteID, colID, h.CreateItem), map[string]any{
		"slug": "pub-one", "title": "Pub", "status": "published", "data": map[string]any{"headline": "P"},
	})
	pub, _ := q.ListPublishedItemsByCollection(context.Background(), colID)
	if len(pub) != 1 {
		t.Errorf("expected 1 published item, got %d", len(pub))
	}
	all, _ := q.ListItemsByCollection(context.Background(), colID)
	if len(all) != 2 {
		t.Errorf("expected 2 total items, got %d", len(all))
	}
}
