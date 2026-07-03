package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ----- Constructor -----

func TestGhost_RequiresBaseURLAndKey(t *testing.T) {
	if _, err := NewGhostClient(GhostOptions{ContentKey: "x"}); err == nil {
		t.Errorf("missing base URL must error")
	}
	if _, err := NewGhostClient(GhostOptions{BaseURL: "https://x"}); err == nil {
		t.Errorf("missing content key must error")
	}
	if _, err := NewGhostClient(GhostOptions{BaseURL: "https://x", ContentKey: "k"}); err != nil {
		t.Errorf("happy path errored: %v", err)
	}
}

// ----- Endpoint construction + key redaction -----

func TestGhost_EndpointBuildsCorrectURL(t *testing.T) {
	c, _ := NewGhostClient(GhostOptions{BaseURL: "https://acme.example", ContentKey: "k123"})
	got := c.endpoint("/posts/", nil)
	if !strings.Contains(got, "/ghost/api/content/posts/") {
		t.Errorf("path: %q", got)
	}
	if !strings.Contains(got, "key=k123") {
		t.Errorf("api key not in query: %q", got)
	}
}

func TestGhost_RedactGhostKey(t *testing.T) {
	in := "https://acme.example/ghost/api/content/posts/?key=secret123&limit=15"
	got := redactGhostKey(in)
	if strings.Contains(got, "secret123") {
		t.Errorf("key not redacted: %q", got)
	}
	if !strings.Contains(got, "key=REDACTED") {
		t.Errorf("redacted marker missing: %q", got)
	}
	if !strings.Contains(got, "limit=15") {
		t.Errorf("other params dropped: %q", got)
	}
}

// ----- Mock Ghost server -----

type fakeGhost struct {
	t         *testing.T
	srv       *httptest.Server
	expectKey string
	posts     []ghostPost
	pages     []ghostPost
	tags      []ghostTag
}

func newFakeGhost(t *testing.T) *fakeGhost {
	t.Helper()
	fg := &fakeGhost{t: t}
	mux := http.NewServeMux()

	pageOf := func(r *http.Request) (int, int) {
		page := 1
		limit := 100
		_, _ = fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page)
		_, _ = fmt.Sscanf(r.URL.Query().Get("limit"), "%d", &limit)
		return page, limit
	}
	paginate := func(total, page, limit int) (start, end, totalPages int) {
		start = (page - 1) * limit
		end = start + limit
		if end > total {
			end = total
		}
		totalPages = (total + limit - 1) / limit
		if totalPages == 0 {
			totalPages = 1
		}
		return
	}

	check := func(w http.ResponseWriter, r *http.Request) bool {
		if fg.expectKey != "" && r.URL.Query().Get("key") != fg.expectKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"errors":[{"code":"INVALID_API_KEY","type":"UnauthorizedError","message":"Unknown Content API Key"}]}`))
			return false
		}
		return true
	}

	mux.HandleFunc("/ghost/api/content/posts/", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		page, limit := pageOf(r)
		start, end, totalPages := paginate(len(fg.posts), page, limit)
		batch := []ghostPost{}
		if start < len(fg.posts) {
			batch = fg.posts[start:end]
		}
		_ = json.NewEncoder(w).Encode(ghostPostsResponse{
			Posts: batch,
			Meta: ghostMeta{Pagination: ghostPagination{
				Page: page, Limit: limit, Pages: totalPages, Total: len(fg.posts),
			}},
		})
	})
	mux.HandleFunc("/ghost/api/content/pages/", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		page, limit := pageOf(r)
		start, end, totalPages := paginate(len(fg.pages), page, limit)
		batch := []ghostPost{}
		if start < len(fg.pages) {
			batch = fg.pages[start:end]
		}
		_ = json.NewEncoder(w).Encode(ghostPostsResponse{
			Pages: batch,
			Meta: ghostMeta{Pagination: ghostPagination{
				Page: page, Limit: limit, Pages: totalPages, Total: len(fg.pages),
			}},
		})
	})
	mux.HandleFunc("/ghost/api/content/tags/", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		page, limit := pageOf(r)
		start, end, totalPages := paginate(len(fg.tags), page, limit)
		batch := []ghostTag{}
		if start < len(fg.tags) {
			batch = fg.tags[start:end]
		}
		_ = json.NewEncoder(w).Encode(ghostTagsResponse{
			Tags: batch,
			Meta: ghostMeta{Pagination: ghostPagination{
				Page: page, Limit: limit, Pages: totalPages, Total: len(fg.tags),
			}},
		})
	})

	fg.srv = httptest.NewServer(mux)
	t.Cleanup(fg.srv.Close)
	return fg
}

// ----- Discovery -----

func TestGhost_DiscoverReturnsHost(t *testing.T) {
	fg := newFakeGhost(t)
	fg.posts = []ghostPost{{ID: "1", URL: "https://acme.example/welcome/", Slug: "welcome", Title: "Welcome"}}

	c, _ := NewGhostClient(GhostOptions{
		BaseURL: fg.srv.URL, ContentKey: "k",
		FetchOpts: FetchOptions{AllowPrivate: true},
	})
	host, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if host != "acme.example" {
		t.Errorf("host: %q", host)
	}
}

// ----- Auth via key -----

func TestGhost_KeyAuthFailureSurfacesError(t *testing.T) {
	fg := newFakeGhost(t)
	fg.expectKey = "secret"
	c, _ := NewGhostClient(GhostOptions{
		BaseURL: fg.srv.URL, ContentKey: "wrong",
		FetchOpts: FetchOptions{AllowPrivate: true},
	})
	_, err := c.Discover(context.Background())
	if err == nil {
		t.Fatalf("wrong key should error")
	}
	if !strings.Contains(err.Error(), "INVALID_API_KEY") {
		t.Errorf("ghost error code should surface, got %v", err)
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Errorf("plaintext key should not appear in error, got %v", err)
	}
}

// ----- SSRF inheritance -----

func TestGhost_SSRFGuardInherited(t *testing.T) {
	c, _ := NewGhostClient(GhostOptions{BaseURL: "http://127.0.0.1:1", ContentKey: "k"})
	if _, err := c.Discover(context.Background()); err == nil {
		t.Errorf("Ghost client must inherit SSRF guard")
	}
}

// ----- End-to-end crawl -----

func TestGhost_CrawlEndToEnd(t *testing.T) {
	fg := newFakeGhost(t)
	fg.tags = []ghostTag{
		{ID: "t1", Slug: "intro", Name: "Intro"},
		{ID: "t2", Slug: "news", Name: "News"},
	}
	fg.pages = []ghostPost{
		{
			ID: "pg1", Slug: "about", Title: "About",
			URL:       "https://acme.example/about/",
			HTML:      `<p>About body with <a href="https://acme.example/contact">contact</a></p>`,
			MetaTitle: "About Acme", MetaDescription: "About description",
			OGImage:     "https://acme.example/og.jpg",
			PublishedAt: "2024-04-01T00:00:00.000Z",
		},
	}
	fg.posts = []ghostPost{
		{
			ID: "p1", Slug: "hello", Title: "Hello",
			URL:     "https://acme.example/hello/",
			HTML:    `<p>Body <img src="https://acme.example/img.jpg"></p>`,
			Excerpt: "Excerpt text", FeatureImage: "https://acme.example/feat.jpg",
			FeatureImageAlt: "Feat",
			PublishedAt:     "2024-05-15T12:00:00.000Z",
			Tags:            []ghostTag{{Slug: "intro"}, {Slug: "news"}},
			Authors:         []ghostAuthor{{Name: "Alice", Slug: "alice"}},
		},
		{
			ID: "p2", Slug: "second", Title: "Second",
			URL:         "https://acme.example/second/",
			HTML:        "<p>Two</p>",
			PublishedAt: "2024-05-20T00:00:00.000Z",
		},
	}

	c, _ := NewGhostClient(GhostOptions{
		BaseURL: fg.srv.URL, ContentKey: "k",
		FetchOpts: FetchOptions{AllowPrivate: true},
	})
	manifest, err := c.Crawl(context.Background(), GhostOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}

	if len(manifest.Pages) != 1 {
		t.Fatalf("want 1 page, got %d", len(manifest.Pages))
	}
	page := manifest.Pages[0]
	if page.Title != "About Acme" {
		t.Errorf("page title: %q", page.Title)
	}
	if page.OGImageURL != "https://acme.example/og.jpg" {
		t.Errorf("og image: %q", page.OGImageURL)
	}
	if len(page.InternalLinks) != 1 || page.InternalLinks[0] != "/contact" {
		t.Errorf("internal links: %v", page.InternalLinks)
	}

	if len(manifest.Collections) != 1 {
		t.Fatalf("want 1 collection, got %d", len(manifest.Collections))
	}
	posts := manifest.Collections[0]
	if posts.Slug != "posts" || posts.Name != "Posts" {
		t.Errorf("collection ident: %+v", posts)
	}
	if len(posts.Items) != 2 {
		t.Fatalf("want 2 post items, got %d", len(posts.Items))
	}
	first := posts.Items[0]
	if first.Title != "Hello" || first.Slug != "hello" {
		t.Errorf("item ident: %+v", first)
	}
	if first.PublishedAt.IsZero() {
		t.Errorf("post publish date not parsed")
	}
	tags, ok := first.Data["tags"].([]string)
	if !ok || len(tags) != 2 {
		t.Errorf("tags not extracted: %v", first.Data["tags"])
	}
	if a, _ := first.Data["author"].(string); a != "Alice" {
		t.Errorf("author not extracted: %q", a)
	}
	if fi, _ := first.Data["feature_image"].(string); fi != "https://acme.example/feat.jpg" {
		t.Errorf("feature_image: %q", fi)
	}

	// Media: og + feature + inline (deduped)
	mediaURLs := map[string]bool{}
	for _, m := range manifest.Media {
		mediaURLs[m.SourceURL] = true
	}
	if !mediaURLs["https://acme.example/og.jpg"] || !mediaURLs["https://acme.example/feat.jpg"] || !mediaURLs["https://acme.example/img.jpg"] {
		t.Errorf("media missing: %v", manifest.Media)
	}

	// Stats
	if manifest.Stats.PagesFound != 1 || manifest.Stats.CollectionsFound != 1 {
		t.Errorf("stats wrong: %+v", manifest.Stats)
	}
}

// ----- Pagination -----

func TestGhost_PaginationFollowsMetaPages(t *testing.T) {
	fg := newFakeGhost(t)
	for i := 0; i < 250; i++ {
		fg.posts = append(fg.posts, ghostPost{
			ID:   fmt.Sprintf("p%d", i),
			Slug: fmt.Sprintf("p%d", i), Title: fmt.Sprintf("P%d", i),
			URL:         fmt.Sprintf("https://acme.example/p%d/", i),
			PublishedAt: "2024-01-01T00:00:00.000Z",
		})
	}
	c, _ := NewGhostClient(GhostOptions{
		BaseURL: fg.srv.URL, ContentKey: "k",
		FetchOpts: FetchOptions{AllowPrivate: true},
	})
	manifest, err := c.Crawl(context.Background(), GhostOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	got := 0
	if len(manifest.Collections) > 0 {
		got = len(manifest.Collections[0].Items)
	}
	if got != 250 {
		t.Errorf("pagination did not return all 250 posts, got %d", got)
	}
}

// ----- Empty install -----

func TestGhost_EmptySiteCompletesCleanly(t *testing.T) {
	fg := newFakeGhost(t)
	c, _ := NewGhostClient(GhostOptions{
		BaseURL: fg.srv.URL, ContentKey: "k",
		FetchOpts: FetchOptions{AllowPrivate: true},
	})
	manifest, err := c.Crawl(context.Background(), GhostOptions{})
	if err != nil {
		t.Fatalf("empty Ghost should not error: %v", err)
	}
	if len(manifest.Pages) != 0 || len(manifest.Collections) != 0 {
		t.Errorf("expected empty manifest, got %+v", manifest)
	}
}

// ----- Date parsing -----

func TestGhost_DateParsing(t *testing.T) {
	got := parseGhostTime("2024-06-01T12:34:56.000Z")
	if got.IsZero() {
		t.Errorf("ghost millisecond Z date should parse")
	}
	got2 := parseGhostTime("2024-06-01T12:34:56Z")
	if got2.IsZero() {
		t.Errorf("rfc3339 should parse")
	}
}
