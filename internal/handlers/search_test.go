package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	dbpkg "github.com/bright-interaction/slab/internal/db"
	"github.com/bright-interaction/slab/internal/store"
)

// newSearchHandlerForTest builds a SearchHandler against an in-memory
// SQLite DB with the canonical schema applied. Returns the handler,
// the *sql.DB (for direct content insertion), the queries, and a
// pre-seeded site_id so tests can immediately seed content.
func newSearchHandlerForTest(t *testing.T) (*SearchHandler, *sql.DB, *store.Queries, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "search.db")
	sqlDB, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	if _, err := sqlDB.Exec(string(dbpkg.Schema)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	q := store.New(sqlDB)
	siteID := "abcdef0123456789abcdef88"
	if err := q.CreateSite(context.Background(), store.CreateSiteParams{
		ID: siteID, Name: "Search Test Site", Slug: "search-test", Domain: "",
		PrimaryColor: "#000", SecondaryColor: "#000", BgColor: "#FFF", TextColor: "#111",
		FontHeading: "Inter", FontBody: "Inter", Lang: "en",
	}); err != nil {
		t.Fatal(err)
	}
	return NewSearchHandler(sqlDB, q), sqlDB, q, siteID
}

func searchReq(method, siteID, query, kind string) *http.Request {
	u := "/api/sites/" + siteID + "/search?q=" + url.QueryEscape(query)
	if kind != "" {
		u += "&kind=" + url.QueryEscape(kind)
	}
	r := httptest.NewRequest(method, u, nil)
	return withChiSiteParam(r, siteID)
}

func TestSearch_EmptyQueryReturnsEmptyResults(t *testing.T) {
	h, _, _, siteID := newSearchHandlerForTest(t)
	w := httptest.NewRecorder()
	h.Search(w, searchReq("GET", siteID, "", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	results, _ := body["results"].([]any)
	if len(results) != 0 {
		t.Errorf("results = %d, want 0 for empty query", len(results))
	}
}

func TestSearch_ReindexThenQueryFindsPage(t *testing.T) {
	h, _, q, siteID := newSearchHandlerForTest(t)
	if err := q.CreatePage(context.Background(), store.CreatePageParams{
		ID: "p-1", SiteID: siteID, Slug: "about", Title: "About Atomicsite",
		Layout: "default",
	}); err != nil {
		t.Fatal(err)
	}
	count, err := h.ReindexSite(context.Background(), siteID)
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("reindex returned 0; expected at least the page + site shell")
	}

	w := httptest.NewRecorder()
	h.Search(w, searchReq("GET", siteID, "atomicsite", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	results, _ := body["results"].([]any)
	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}
	first := results[0].(map[string]any)
	if !strings.Contains(strings.ToLower(first["title"].(string)), "atomicsite") {
		t.Errorf("first hit title = %v, want to contain 'atomicsite'", first["title"])
	}
}

func TestSearch_KindFilterRestrictsResults(t *testing.T) {
	h, _, q, siteID := newSearchHandlerForTest(t)
	if err := q.CreatePage(context.Background(), store.CreatePageParams{
		ID: "p-1", SiteID: siteID, Slug: "match", Title: "Apple page",
		Layout: "default",
	}); err != nil {
		t.Fatal(err)
	}
	if err := q.CreateKnowledgebaseEntry(context.Background(), store.CreateKnowledgebaseEntryParams{
		ID: "k-1", SiteID: siteID, Category: "fruit", Title: "Apple kb", Content: "An apple a day.",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ReindexSite(context.Background(), siteID); err != nil {
		t.Fatal(err)
	}

	// No filter: both kinds match.
	w := httptest.NewRecorder()
	h.Search(w, searchReq("GET", siteID, "apple", ""))
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	allResults, _ := body["results"].([]any)
	if len(allResults) < 2 {
		t.Errorf("unfiltered results = %d, want >= 2", len(allResults))
	}

	// Filter to page: only the page hit.
	w2 := httptest.NewRecorder()
	h.Search(w2, searchReq("GET", siteID, "apple", "page"))
	var pageBody map[string]any
	_ = json.Unmarshal(w2.Body.Bytes(), &pageBody)
	pageResults, _ := pageBody["results"].([]any)
	for _, r := range pageResults {
		row := r.(map[string]any)
		if row["kind"] != "page" {
			t.Errorf("kind=page filter leaked %v", row["kind"])
		}
	}
}

func TestSearch_FTSReservedSyntaxNeutralized(t *testing.T) {
	// FTS5's grammar reserves NEAR, *, parentheses, etc. Without
	// neutralization, a hostile query like 'OR "x" *' would either
	// throw a syntax error or surface internal data. We quote every
	// token so reserved syntax is treated as literal text.
	h, _, q, siteID := newSearchHandlerForTest(t)
	if err := q.CreatePage(context.Background(), store.CreatePageParams{
		ID: "p-1", SiteID: siteID, Slug: "near-page", Title: "Hello world",
		Layout: "default",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ReindexSite(context.Background(), siteID); err != nil {
		t.Fatal(err)
	}
	for _, hostile := range []string{"NEAR", "OR(", "* x *", `"; DROP TABLE`, `(((`} {
		w := httptest.NewRecorder()
		h.Search(w, searchReq("GET", siteID, hostile, ""))
		if w.Code != http.StatusOK {
			t.Errorf("hostile query %q returned %d, want 200", hostile, w.Code)
		}
	}
}

func TestSearch_QueryTooLongReturns400(t *testing.T) {
	h, _, _, siteID := newSearchHandlerForTest(t)
	long := strings.Repeat("a", 250)
	w := httptest.NewRecorder()
	h.Search(w, searchReq("GET", siteID, long, ""))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (query too long)", w.Code)
	}
}

func TestSearch_InvalidSiteIDReturns400(t *testing.T) {
	h, _, _, _ := newSearchHandlerForTest(t)
	r := httptest.NewRequest("GET", "/api/sites/bad/search?q=x", nil)
	r = withChiSiteParam(r, "bad-site")
	w := httptest.NewRecorder()
	h.Search(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestSearchUpsert_AndDeleteRoundTrip(t *testing.T) {
	h, sqlDB, _, siteID := newSearchHandlerForTest(t)
	ctx := context.Background()

	SearchUpsert(ctx, sqlDB, siteID, SearchKindPage, "p-x", "Pagex", "Some body content")

	w := httptest.NewRecorder()
	h.Search(w, searchReq("GET", siteID, "pagex", ""))
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if rs, _ := body["results"].([]any); len(rs) != 1 {
		t.Fatalf("expected 1 result after upsert, got %d", len(rs))
	}

	// Re-upsert with new title and verify the OLD row is gone (no
	// dupes) and the new row matches.
	SearchUpsert(ctx, sqlDB, siteID, SearchKindPage, "p-x", "Renamed", "New body")
	w2 := httptest.NewRecorder()
	h.Search(w2, searchReq("GET", siteID, "pagex", ""))
	var body2 map[string]any
	_ = json.Unmarshal(w2.Body.Bytes(), &body2)
	if rs, _ := body2["results"].([]any); len(rs) != 0 {
		t.Errorf("re-upsert should remove old row; got %d hits for old title", len(rs))
	}

	// Delete and confirm.
	SearchDelete(ctx, sqlDB, siteID, SearchKindPage, "p-x")
	w3 := httptest.NewRecorder()
	h.Search(w3, searchReq("GET", siteID, "renamed", ""))
	var body3 map[string]any
	_ = json.Unmarshal(w3.Body.Bytes(), &body3)
	if rs, _ := body3["results"].([]any); len(rs) != 0 {
		t.Errorf("expected 0 results after delete; got %d", len(rs))
	}
}

func TestSearch_CrossSiteIsolation(t *testing.T) {
	// Create a second site and index a page there. A search against
	// the first site MUST NOT return the second site's row.
	h, sqlDB, q, siteA := newSearchHandlerForTest(t)
	siteB := "abcdef0123456789abcdef99"
	if err := q.CreateSite(context.Background(), store.CreateSiteParams{
		ID: siteB, Name: "Other", Slug: "other", Domain: "",
		PrimaryColor: "#000", SecondaryColor: "#000", BgColor: "#FFF", TextColor: "#111",
		FontHeading: "Inter", FontBody: "Inter", Lang: "en",
	}); err != nil {
		t.Fatal(err)
	}
	SearchUpsert(context.Background(), sqlDB, siteB, SearchKindPage, "leak", "SecretLeak", "leaked content")

	w := httptest.NewRecorder()
	h.Search(w, searchReq("GET", siteA, "secretleak", ""))
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if rs, _ := body["results"].([]any); len(rs) != 0 {
		t.Errorf("cross-site search leaked %d rows from siteB", len(rs))
	}
}

func TestBuildFTS5Query_SanitizesEverySpecial(t *testing.T) {
	cases := map[string]string{
		"hello world":       `"hello" AND "world"`,
		"OR (foo) NEAR":     `"OR" AND "foo" AND "NEAR"`,
		"":                  "",
		"   ":               "",
		"  hello   world  ": `"hello" AND "world"`,
		`"quoted"`:          `"""quoted"""`,
		"x*y":               `"xy"`,
		"a:b":               `"ab"`,
	}
	for in, want := range cases {
		if got := buildFTS5Query(in); got != want {
			t.Errorf("buildFTS5Query(%q) = %q, want %q", in, got, want)
		}
	}
}
