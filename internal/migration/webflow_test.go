package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// ----- Constructor -----

func TestWebflow_RequiresSiteIDAndToken(t *testing.T) {
	if _, err := NewWebflowClient(WebflowOptions{AuthToken: "x"}); err == nil {
		t.Errorf("missing siteID must error")
	}
	if _, err := NewWebflowClient(WebflowOptions{SiteID: "x"}); err == nil {
		t.Errorf("missing authToken must error")
	}
	if _, err := NewWebflowClient(WebflowOptions{SiteID: "x", AuthToken: "y"}); err != nil {
		t.Errorf("happy path errored: %v", err)
	}
}

// ----- Schema mapping -----

func TestWebflow_SchemaTypeMapping(t *testing.T) {
	cases := map[string]string{
		"PlainText": "text", "RichText": "richtext", "Image": "image",
		"MultiImage": "gallery", "Reference": "reference", "MultiReference": "multiselect",
		"DateTime": "datetime", "Date": "date", "Number": "number",
		"Switch": "boolean", "Color": "color", "Email": "email",
		"Phone": "text", "Link": "url", "Option": "select", "Set": "multiselect",
		"File": "url", "VideoLink": "url", "UnknownType": "",
	}
	for in, want := range cases {
		if got := webflowFieldType(in); got != want {
			t.Errorf("webflowFieldType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWebflow_SchemaToFieldsSkipsUnknownTypes(t *testing.T) {
	in := []webflowField{
		{Slug: "name", DisplayName: "Name", Type: "PlainText", IsRequired: true},
		{Slug: "weird", DisplayName: "Weird", Type: "QuantumFlux"},
		{Slug: "img", DisplayName: "Hero", Type: "Image"},
	}
	out := webflowSchemaToFields(in)
	if len(out) != 2 {
		t.Fatalf("want 2 mapped fields (unknown skipped), got %d: %+v", len(out), out)
	}
	if out[0].Required != true || out[0].Type != "text" {
		t.Errorf("name field wrong: %+v", out[0])
	}
}

// ----- Image flattening -----

func TestWebflow_ImageFieldFlattenedToURL(t *testing.T) {
	raw := map[string]interface{}{
		"hero": map[string]interface{}{
			"url":    "https://uploads-ssl.webflow.com/abc.jpg",
			"alt":    "alt text",
			"fileId": "id1",
		},
		"gallery": []interface{}{
			map[string]interface{}{"url": "https://x/1.jpg"},
			map[string]interface{}{"url": "https://x/2.jpg"},
		},
		"plain": "value",
	}
	schema := []webflowField{
		{Slug: "hero", Type: "Image"},
		{Slug: "gallery", Type: "MultiImage"},
		{Slug: "plain", Type: "PlainText"},
	}
	cleaned := cleanWebflowFieldData(raw, schema)
	if cleaned["hero"] != "https://uploads-ssl.webflow.com/abc.jpg" {
		t.Errorf("hero not flattened: %v", cleaned["hero"])
	}
	g, ok := cleaned["gallery"].([]string)
	if !ok || len(g) != 2 {
		t.Errorf("gallery not flattened to []string: %v", cleaned["gallery"])
	}
	if cleaned["plain"] != "value" {
		t.Errorf("plain text mangled: %v", cleaned["plain"])
	}
}

func TestWebflow_ExtractAssetURLsHandlesNestedShapes(t *testing.T) {
	got := webflowExtractAssetURLs(map[string]interface{}{
		"hero":    map[string]interface{}{"url": "https://x/h.jpg"},
		"gallery": []interface{}{map[string]interface{}{"url": "https://x/1.jpg"}},
		"name":    "Acme",
		"empty":   nil,
	})
	if len(got) != 2 {
		t.Errorf("want 2 URLs extracted, got %d: %v", len(got), got)
	}
}

// ----- Mock Webflow server -----

type fakeWebflow struct {
	t           *testing.T
	srv         *httptest.Server
	hits        atomic.Int64
	expectAuth  string
	site        webflowSite
	pages       []webflowPage
	collections []webflowCollection
	itemsByColl map[string][]webflowItem
}

func newFakeWebflow(t *testing.T) *fakeWebflow {
	t.Helper()
	fw := &fakeWebflow{t: t, itemsByColl: map[string][]webflowItem{}}
	mux := http.NewServeMux()

	// Site
	mux.HandleFunc(fmt.Sprintf("/v2/sites/%s", "site_123"), func(w http.ResponseWriter, r *http.Request) {
		if !fw.checkAuth(w, r) {
			return
		}
		fw.hits.Add(1)
		_ = json.NewEncoder(w).Encode(fw.site)
	})

	// List collections
	mux.HandleFunc(fmt.Sprintf("/v2/sites/%s/collections", "site_123"), func(w http.ResponseWriter, r *http.Request) {
		if !fw.checkAuth(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"collections": fw.collections})
	})

	// Pages list
	mux.HandleFunc(fmt.Sprintf("/v2/sites/%s/pages", "site_123"), func(w http.ResponseWriter, r *http.Request) {
		if !fw.checkAuth(w, r) {
			return
		}
		offset := atoiOrZero(r.URL.Query().Get("offset"))
		limit := atoiOrZero(r.URL.Query().Get("limit"))
		if limit == 0 {
			limit = 100
		}
		end := offset + limit
		if end > len(fw.pages) {
			end = len(fw.pages)
		}
		batch := []webflowPage{}
		if offset < len(fw.pages) {
			batch = fw.pages[offset:end]
		}
		_ = json.NewEncoder(w).Encode(webflowPagesResponse{
			Pages: batch, Pagination: webflowPagination{
				Offset: offset, Limit: limit, Total: len(fw.pages),
			},
		})
	})

	// Per-collection: schema + items
	for _, coll := range fw.collections {
		_ = coll
	}

	// Generic catch for /v2/collections/{id}* paths
	mux.HandleFunc("/v2/collections/", func(w http.ResponseWriter, r *http.Request) {
		if !fw.checkAuth(w, r) {
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/v2/collections/")
		parts := strings.Split(path, "/")
		if len(parts) >= 2 && parts[1] == "items" {
			collID := parts[0]
			items := fw.itemsByColl[collID]
			offset := atoiOrZero(r.URL.Query().Get("offset"))
			limit := atoiOrZero(r.URL.Query().Get("limit"))
			if limit == 0 {
				limit = 100
			}
			end := offset + limit
			if end > len(items) {
				end = len(items)
			}
			batch := []webflowItem{}
			if offset < len(items) {
				batch = items[offset:end]
			}
			_ = json.NewEncoder(w).Encode(webflowItemResponse{
				Items: batch,
				Pagination: webflowPagination{
					Offset: offset, Limit: limit, Total: len(items),
				},
			})
			return
		}
		// Get collection by ID -> return matching schema
		collID := parts[0]
		for _, c := range fw.collections {
			if c.ID == collID {
				_ = json.NewEncoder(w).Encode(c)
				return
			}
		}
		http.NotFound(w, r)
	})

	fw.srv = httptest.NewServer(mux)
	t.Cleanup(fw.srv.Close)
	return fw
}

func (fw *fakeWebflow) checkAuth(w http.ResponseWriter, r *http.Request) bool {
	if fw.expectAuth == "" {
		return true
	}
	if r.Header.Get("Authorization") == fw.expectAuth {
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"code":"unauthorized","message":"Invalid token"}`))
	return false
}

func atoiOrZero(s string) int {
	n := 0
	_, _ = fmt.Sscanf(s, "%d", &n)
	return n
}

// ----- Tests using the fake -----

func TestWebflow_DiscoverHappyPath(t *testing.T) {
	fw := newFakeWebflow(t)
	fw.site = webflowSite{
		ID: "site_123", DisplayName: "Acme", ShortName: "acme",
		Domains: []struct {
			ID  string `json:"id"`
			URL string `json:"url"`
		}{{ID: "d1", URL: "https://acme.example"}},
	}
	c, err := NewWebflowClient(WebflowOptions{
		SiteID: "site_123", AuthToken: "tok", BaseURL: fw.srv.URL,
		FetchOpts: FetchOptions{AllowPrivate: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got.DisplayName != "Acme" {
		t.Errorf("name: %q", got.DisplayName)
	}
}

func TestWebflow_AuthHeaderPassedThrough(t *testing.T) {
	fw := newFakeWebflow(t)
	fw.expectAuth = "Bearer my-token"
	fw.site = webflowSite{ID: "site_123", DisplayName: "Acme", ShortName: "acme"}

	good, _ := NewWebflowClient(WebflowOptions{
		SiteID: "site_123", AuthToken: "my-token", BaseURL: fw.srv.URL,
		FetchOpts: FetchOptions{AllowPrivate: true},
	})
	if _, err := good.Discover(context.Background()); err != nil {
		t.Errorf("auth-header should pass: %v", err)
	}

	bad, _ := NewWebflowClient(WebflowOptions{
		SiteID: "site_123", AuthToken: "wrong", BaseURL: fw.srv.URL,
		FetchOpts: FetchOptions{AllowPrivate: true},
	})
	_, err := bad.Discover(context.Background())
	if err == nil {
		t.Fatalf("wrong auth should fail")
	}
	if !strings.Contains(err.Error(), "unauthorized") {
		t.Errorf("error should expose webflow code, got %v", err)
	}
}

func TestWebflow_SSRFGuardInherited(t *testing.T) {
	c, _ := NewWebflowClient(WebflowOptions{
		SiteID: "x", AuthToken: "y", BaseURL: "http://127.0.0.1:1",
		// AllowPrivate intentionally false
	})
	if _, err := c.Discover(context.Background()); err == nil {
		t.Errorf("Webflow client must inherit SSRF guard")
	}
}

func TestWebflow_CrawlEndToEnd(t *testing.T) {
	fw := newFakeWebflow(t)
	fw.site = webflowSite{
		ID: "site_123", DisplayName: "Acme", ShortName: "acme",
		Domains: []struct {
			ID  string `json:"id"`
			URL string `json:"url"`
		}{{ID: "d1", URL: "https://acme.example"}},
	}
	fw.pages = []webflowPage{
		{ID: "p1", Title: "About", Slug: "about", SeoTitle: "About Acme",
			SeoDescription: "About text", OpenGraphImage: "https://acme.example/og.jpg",
			LastUpdated: "2024-05-01T12:00:00Z"},
		{ID: "p2", Title: "Hidden", Slug: "hidden", IsDraft: true},
	}
	fw.collections = []webflowCollection{
		{
			ID: "coll_blog", Slug: "blog", DisplayName: "Blog",
			Fields: []webflowField{
				{Slug: "name", DisplayName: "Name", Type: "PlainText", IsRequired: true},
				{Slug: "slug", DisplayName: "Slug", Type: "PlainText", IsRequired: true},
				{Slug: "body", DisplayName: "Body", Type: "RichText"},
				{Slug: "hero", DisplayName: "Hero", Type: "Image"},
				{Slug: "tags", DisplayName: "Tags", Type: "Set"},
			},
		},
	}
	fw.itemsByColl["coll_blog"] = []webflowItem{
		{
			ID: "it1", IsArchived: false, IsDraft: false,
			LastPublished: "2024-06-15T10:00:00Z",
			FieldData: map[string]interface{}{
				"name": "Hello World",
				"slug": "hello-world",
				"body": "<p>Body content with <a href=\"https://acme.example/about\">link</a></p>",
				"hero": map[string]interface{}{"url": "https://uploads.webflow.com/hero.jpg", "alt": "Hero"},
				"tags": []interface{}{"intro", "news"},
			},
		},
		{
			ID: "it2", IsDraft: true, // must be skipped
			FieldData: map[string]interface{}{"name": "Draft", "slug": "draft"},
		},
	}
	// re-register handlers because newFakeWebflow's range over fw.collections
	// happened before we set them.
	fw.srv.Close()
	fw = newFakeWebflow(t)
	fw.site = webflowSite{
		ID: "site_123", DisplayName: "Acme", ShortName: "acme",
		Domains: []struct {
			ID  string `json:"id"`
			URL string `json:"url"`
		}{{ID: "d1", URL: "https://acme.example"}},
	}
	fw.pages = []webflowPage{
		{ID: "p1", Title: "About", Slug: "about", SeoTitle: "About Acme",
			SeoDescription: "About text", OpenGraphImage: "https://acme.example/og.jpg",
			LastUpdated: "2024-05-01T12:00:00Z"},
		{ID: "p2", Title: "Hidden", Slug: "hidden", IsDraft: true},
	}
	fw.collections = []webflowCollection{
		{
			ID: "coll_blog", Slug: "blog", DisplayName: "Blog",
			Fields: []webflowField{
				{Slug: "name", DisplayName: "Name", Type: "PlainText", IsRequired: true},
				{Slug: "slug", DisplayName: "Slug", Type: "PlainText", IsRequired: true},
				{Slug: "body", DisplayName: "Body", Type: "RichText"},
				{Slug: "hero", DisplayName: "Hero", Type: "Image"},
				{Slug: "tags", DisplayName: "Tags", Type: "Set"},
			},
		},
	}
	fw.itemsByColl["coll_blog"] = []webflowItem{
		{
			ID: "it1", IsArchived: false, IsDraft: false,
			LastPublished: "2024-06-15T10:00:00Z",
			FieldData: map[string]interface{}{
				"name": "Hello World", "slug": "hello-world",
				"body": "<p>Body</p>",
				"hero": map[string]interface{}{"url": "https://uploads.webflow.com/hero.jpg", "alt": "Hero"},
				"tags": []interface{}{"intro", "news"},
			},
		},
		{
			ID: "it2", IsDraft: true,
			FieldData: map[string]interface{}{"name": "Draft", "slug": "draft"},
		},
	}

	c, _ := NewWebflowClient(WebflowOptions{
		SiteID: "site_123", AuthToken: "tok", BaseURL: fw.srv.URL,
		FetchOpts: FetchOptions{AllowPrivate: true},
	})
	manifest, err := c.Crawl(context.Background(), WebflowOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}

	// Drafts filtered out from pages
	if len(manifest.Pages) != 1 {
		t.Fatalf("want 1 published page (draft filtered), got %d", len(manifest.Pages))
	}
	page := manifest.Pages[0]
	if page.Title != "About Acme" {
		t.Errorf("page title: %q", page.Title)
	}
	if page.OGImageURL != "https://acme.example/og.jpg" {
		t.Errorf("og image: %q", page.OGImageURL)
	}
	if page.PublishedAt.IsZero() {
		t.Errorf("published_at not parsed")
	}

	// Collection
	if len(manifest.Collections) != 1 {
		t.Fatalf("want 1 collection, got %d", len(manifest.Collections))
	}
	coll := manifest.Collections[0]
	if coll.Slug != "blog" || coll.Name != "Blog" {
		t.Errorf("coll ident: %+v", coll)
	}
	// Schema preserved + types translated
	wantTypes := map[string]string{"name": "text", "slug": "text", "body": "richtext", "hero": "image", "tags": "multiselect"}
	for _, f := range coll.Schema {
		if wantTypes[f.Name] != f.Type {
			t.Errorf("field %q type = %q want %q", f.Name, f.Type, wantTypes[f.Name])
		}
	}
	// Drafts filtered from items
	if len(coll.Items) != 1 {
		t.Fatalf("want 1 published item (draft filtered), got %d", len(coll.Items))
	}
	item := coll.Items[0]
	if item.Slug != "hello-world" || item.Title != "Hello World" {
		t.Errorf("item ident: %+v", item)
	}
	if hero, _ := item.Data["hero"].(string); hero != "https://uploads.webflow.com/hero.jpg" {
		t.Errorf("image not flattened in item.Data: %v", item.Data["hero"])
	}

	// Media: og + hero
	if len(manifest.Media) < 2 {
		t.Errorf("want >=2 media (og + hero), got %d (%v)", len(manifest.Media), manifest.Media)
	}
}

func TestWebflow_PaginationFollowsTotal(t *testing.T) {
	fw := newFakeWebflow(t)
	fw.site = webflowSite{ID: "site_123", DisplayName: "Acme", ShortName: "acme"}
	for i := 0; i < 250; i++ {
		fw.pages = append(fw.pages, webflowPage{
			ID: fmt.Sprintf("p%d", i), Title: fmt.Sprintf("P%d", i),
			Slug: fmt.Sprintf("p%d", i),
		})
	}
	c, _ := NewWebflowClient(WebflowOptions{
		SiteID: "site_123", AuthToken: "tok", BaseURL: fw.srv.URL,
		FetchOpts: FetchOptions{AllowPrivate: true},
	})
	manifest, err := c.Crawl(context.Background(), WebflowOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if len(manifest.Pages) != 250 {
		t.Errorf("pagination did not return all 250 pages, got %d", len(manifest.Pages))
	}
}

func TestWebflow_ItemCapHonoured(t *testing.T) {
	fw := newFakeWebflow(t)
	fw.site = webflowSite{ID: "site_123", ShortName: "acme"}
	fw.collections = []webflowCollection{{ID: "c", Slug: "blog", DisplayName: "Blog",
		Fields: []webflowField{{Slug: "slug", Type: "PlainText"}}}}
	for i := 0; i < 250; i++ {
		fw.itemsByColl["c"] = append(fw.itemsByColl["c"], webflowItem{
			ID: fmt.Sprintf("i%d", i),
			FieldData: map[string]interface{}{
				"slug": fmt.Sprintf("i%d", i),
				"name": fmt.Sprintf("I%d", i),
			},
		})
	}
	// Re-register handlers after mutating fw fields.
	fw.srv.Close()
	fw2 := newFakeWebflow(t)
	fw2.site = webflowSite{ID: "site_123", ShortName: "acme"}
	fw2.collections = fw.collections
	fw2.itemsByColl = fw.itemsByColl

	c, _ := NewWebflowClient(WebflowOptions{
		SiteID: "site_123", AuthToken: "tok", BaseURL: fw2.srv.URL,
		FetchOpts: FetchOptions{AllowPrivate: true},
	})
	manifest, err := c.Crawl(context.Background(), WebflowOptions{MaxItemsPerCollection: 50})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if got := len(manifest.Collections[0].Items); got != 50 {
		t.Errorf("MaxItemsPerCollection=50 not honoured, got %d", got)
	}
}

func TestWebflow_StatusErrorIncludesCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"insufficient_scope","message":"Token missing cms:read"}`))
	}))
	defer srv.Close()
	c, _ := NewWebflowClient(WebflowOptions{
		SiteID: "x", AuthToken: "y", BaseURL: srv.URL,
		FetchOpts: FetchOptions{AllowPrivate: true},
	})
	_, err := c.Discover(context.Background())
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "insufficient_scope") {
		t.Errorf("error should include WF code, got %v", err)
	}
}
