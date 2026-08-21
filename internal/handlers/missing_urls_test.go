package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	_ "modernc.org/sqlite"

	"github.com/bright-interaction/slab/internal/config"
	"github.com/bright-interaction/slab/internal/store"
)

func newMissingURLsHandlerForTest(t *testing.T) (*MissingURLsHandler, *store.Queries, string) {
	t.Helper()
	sqlDB, _ := newSchemaDB(t, "miss.db")
	q := store.New(sqlDB)
	siteID := "abcdef0123456789abcdefms"
	if err := q.CreateSite(context.Background(), store.CreateSiteParams{
		ID: siteID, Name: "T", Slug: "t-miss", PrimaryColor: "#000",
		SecondaryColor: "#000", BgColor: "#FFF", TextColor: "#111",
		FontHeading: "Inter", FontBody: "Inter", Lang: "en",
	}); err != nil {
		t.Fatalf("create site: %v", err)
	}
	cfg := &config.Config{BaseURL: "http://localhost:8080"}
	return NewMissingURLsHandler(cfg, q), q, siteID
}

func withMissingParams(siteID, missID string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("siteID", siteID)
		if missID != "" {
			rctx.URLParams.Add("missingID", missID)
		}
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
		h(w, r)
	}
}

func seedMissingURL(t *testing.T, q *store.Queries, siteID, id, path, referer string, hits int) {
	t.Helper()
	for i := 0; i < hits; i++ {
		if err := q.UpsertMissingURL(context.Background(), store.UpsertMissingURLParams{
			ID: id, SiteID: siteID, Path: path, Referer: referer,
		}); err != nil {
			t.Fatalf("upsert missing: %v", err)
		}
	}
}

// ----- List -----

func TestMissingURLs_ListOrdersByHitCount(t *testing.T) {
	h, q, siteID := newMissingURLsHandlerForTest(t)
	seedMissingURL(t, q, siteID, "m1", "/old-popular", "", 10)
	seedMissingURL(t, q, siteID, "m2", "/old-rare", "", 1)
	seedMissingURL(t, q, siteID, "m3", "/old-medium", "", 5)

	req := httptest.NewRequest(http.MethodGet, "/?limit=10", nil)
	rr := httptest.NewRecorder()
	withMissingParams(siteID, "", h.List)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200 got %d", rr.Code)
	}
	var resp struct {
		Items []map[string]any `json:"items"`
		Total int64            `json:"total"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 3 {
		t.Errorf("total: %d", resp.Total)
	}
	if len(resp.Items) != 3 {
		t.Fatalf("items: %d", len(resp.Items))
	}
	if resp.Items[0]["path"] != "/old-popular" {
		t.Errorf("top hit should be /old-popular, got %q", resp.Items[0]["path"])
	}
}

func TestMissingURLs_ListLimitClamped(t *testing.T) {
	h, q, siteID := newMissingURLsHandlerForTest(t)
	for i := 0; i < 10; i++ {
		seedMissingURL(t, q, siteID, "id"+strings.Repeat("0", 19)+string(rune('0'+i)), "/p"+string(rune('0'+i)), "", 1)
	}
	req := httptest.NewRequest(http.MethodGet, "/?limit=5", nil)
	rr := httptest.NewRecorder()
	withMissingParams(siteID, "", h.List)(rr, req)
	var resp struct {
		Items []map[string]any `json:"items"`
		Total int64            `json:"total"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if len(resp.Items) != 5 {
		t.Errorf("limit=5 must return 5 items, got %d", len(resp.Items))
	}
	if resp.Total != 10 {
		t.Errorf("total should reflect full count, got %d", resp.Total)
	}
}

// ----- CreateRedirect -----

func TestMissingURLs_CreateRedirectHappyPath(t *testing.T) {
	h, q, siteID := newMissingURLsHandlerForTest(t)
	seedMissingURL(t, q, siteID, "mhappy", "/old-blog", "https://google.com/", 5)

	body, _ := json.Marshal(map[string]any{"to_path": "/blog", "status_code": 301})
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	withMissingParams(siteID, "mhappy", h.CreateRedirect)(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201 got %d body=%s", rr.Code, rr.Body.String())
	}

	// Redirect row landed.
	rows, _ := q.ListRedirectsBySite(context.Background(), siteID)
	if len(rows) != 1 {
		t.Fatalf("want 1 redirect, got %d", len(rows))
	}
	if rows[0].FromPath != "/old-blog" || rows[0].ToPath != "/blog" || rows[0].StatusCode != 301 {
		t.Errorf("redirect wrong: %+v", rows[0])
	}
	if rows[0].IsAuto != 0 {
		t.Errorf("operator-initiated redirect must have IsAuto=0, got %d", rows[0].IsAuto)
	}

	// Missing-URL row cleared.
	missing, _ := q.ListMissingURLsBySite(context.Background(), store.ListMissingURLsBySiteParams{SiteID: siteID, Limit: 100})
	if len(missing) != 0 {
		t.Errorf("missing-url row must be cleared after redirect creation, got %d", len(missing))
	}
}

func TestMissingURLs_CreateRedirect_410GoneSupported(t *testing.T) {
	h, q, siteID := newMissingURLsHandlerForTest(t)
	seedMissingURL(t, q, siteID, "m410", "/dead-link", "", 3)
	body, _ := json.Marshal(map[string]any{"to_path": "/", "status_code": 410})
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	withMissingParams(siteID, "m410", h.CreateRedirect)(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201 got %d body=%s", rr.Code, rr.Body.String())
	}
	rows, _ := q.ListRedirectsBySite(context.Background(), siteID)
	if len(rows) != 1 || rows[0].StatusCode != 410 {
		t.Errorf("410 not stored: %+v", rows)
	}
}

func TestMissingURLs_CreateRedirectRejectsHostileToPath(t *testing.T) {
	h, q, siteID := newMissingURLsHandlerForTest(t)
	seedMissingURL(t, q, siteID, "mhost", "/x", "", 1)

	cases := []struct {
		body string
		want int
	}{
		{`{"to_path":""}`, http.StatusBadRequest},
		{`{"to_path":"/x"}`, http.StatusUnprocessableEntity}, // no-op
		{`{"to_path":"//evil.com/"}`, http.StatusUnprocessableEntity},
		{`{"to_path":"https://evil.com/"}`, http.StatusUnprocessableEntity},
		{`{"to_path":"/{}injection"}`, http.StatusUnprocessableEntity},
		{`{"to_path":"/x", "status_code":999}`, http.StatusUnprocessableEntity},
	}
	for _, c := range cases {
		// Re-seed since each successful (or rejected) call leaves state.
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		withMissingParams(siteID, "mhost", h.CreateRedirect)(rr, req)
		if rr.Code != c.want {
			t.Errorf("body=%q want %d got %d (%s)", c.body, c.want, rr.Code, rr.Body.String())
		}
	}
	// No redirect rows should exist after all rejections.
	if rows, _ := q.ListRedirectsBySite(context.Background(), siteID); len(rows) != 0 {
		t.Errorf("rejected requests must not create redirects, got %d", len(rows))
	}
}

func TestMissingURLs_CreateRedirectIdempotentWhenRedirectAlreadyExists(t *testing.T) {
	h, q, siteID := newMissingURLsHandlerForTest(t)
	seedMissingURL(t, q, siteID, "mexist", "/x", "", 1)
	// Pre-seed an existing redirect at /x.
	if err := q.CreateRedirect(context.Background(), store.CreateRedirectParams{
		ID: "rexisting", SiteID: siteID, FromPath: "/x", ToPath: "/y",
		StatusCode: 301, IsAuto: 0,
	}); err != nil {
		t.Fatalf("seed redirect: %v", err)
	}

	body, _ := json.Marshal(map[string]any{"to_path": "/z"})
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	withMissingParams(siteID, "mexist", h.CreateRedirect)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200 (already exists), got %d body=%s", rr.Code, rr.Body.String())
	}
	// The pre-existing redirect must NOT have been overwritten.
	rows, _ := q.ListRedirectsBySite(context.Background(), siteID)
	if len(rows) != 1 || rows[0].ToPath != "/y" {
		t.Errorf("pre-existing redirect overwritten: %+v", rows)
	}
	// Missing-URL row should still be cleared since the redirect is in place.
	missing, _ := q.ListMissingURLsBySite(context.Background(), store.ListMissingURLsBySiteParams{SiteID: siteID, Limit: 10})
	if len(missing) != 0 {
		t.Errorf("missing-url row should clear when a redirect already covers the path, got %d", len(missing))
	}
}

// ----- Cross-tenant -----

func TestMissingURLs_CrossTenantBlocked(t *testing.T) {
	h, q, siteA := newMissingURLsHandlerForTest(t)
	siteB := "abcdef0123456789abcdefxx"
	_ = q.CreateSite(context.Background(), store.CreateSiteParams{
		ID: siteB, Name: "B", Slug: "b-miss", PrimaryColor: "#000",
		SecondaryColor: "#000", BgColor: "#FFF", TextColor: "#111",
		FontHeading: "Inter", FontBody: "Inter", Lang: "en",
	})
	seedMissingURL(t, q, siteA, "mtenA", "/x", "", 1)

	// Site B tries to convert site A's missing URL.
	body, _ := json.Marshal(map[string]any{"to_path": "/y"})
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	withMissingParams(siteB, "mtenA", h.CreateRedirect)(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("cross-tenant must 404, got %d", rr.Code)
	}
	// Site A's row must still exist.
	if rows, _ := q.ListMissingURLsBySite(context.Background(), store.ListMissingURLsBySiteParams{SiteID: siteA, Limit: 10}); len(rows) != 1 {
		t.Errorf("cross-tenant attempt must not affect site A, got %d rows", len(rows))
	}
}

// ----- Dismiss -----

func TestMissingURLs_DismissRemovesRow(t *testing.T) {
	h, q, siteID := newMissingURLsHandlerForTest(t)
	seedMissingURL(t, q, siteID, "mdis", "/junk", "", 1)
	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	rr := httptest.NewRecorder()
	withMissingParams(siteID, "mdis", h.Dismiss)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200 got %d", rr.Code)
	}
	if rows, _ := q.ListMissingURLsBySite(context.Background(), store.ListMissingURLsBySiteParams{SiteID: siteID, Limit: 10}); len(rows) != 0 {
		t.Errorf("dismiss left rows: %+v", rows)
	}
	// Dismiss should NOT have created a redirect.
	if rows, _ := q.ListRedirectsBySite(context.Background(), siteID); len(rows) != 0 {
		t.Errorf("dismiss must not create redirect, got %d", len(rows))
	}
}
