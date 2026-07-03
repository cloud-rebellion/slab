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
	"time"
)

// ----- baseURL normalization -----

func TestNormalizeWPBaseURL_AcceptsAllThreeShapes(t *testing.T) {
	cases := map[string]string{
		"https://example.com":                  "https://example.com/wp-json/wp/v2",
		"https://example.com/":                 "https://example.com/wp-json/wp/v2",
		"https://example.com/wp-json":          "https://example.com/wp-json/wp/v2",
		"https://example.com/wp-json/":         "https://example.com/wp-json/wp/v2",
		"https://example.com/wp-json/wp/v2":    "https://example.com/wp-json/wp/v2",
		"  https://example.com/wp-json/wp/v2/": "https://example.com/wp-json/wp/v2",
	}
	for in, want := range cases {
		if got := normalizeWPBaseURL(in); got != want {
			t.Errorf("normalizeWPBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// ----- HTML helper unit tests -----

func TestStripHTMLTags(t *testing.T) {
	cases := map[string]string{
		"<p>Hello <strong>world</strong></p>": "Hello world",
		"Plain text":                          "Plain text",
		"":                                    "",
		"   <em>spaced</em>   ":               "spaced",
		"<p>Multi&nbsp;entity &amp; more</p>": "Multi entity & more",
	}
	for in, want := range cases {
		if got := stripHTMLTags(in); got != want {
			t.Errorf("stripHTMLTags(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExtractAssetsFromHTML(t *testing.T) {
	html := `<p>Read <a href="/another-post">more</a> or <a href="https://example.com/about">about</a>.</p>
<p>External: <a href="https://other.example/x">x</a> and skip <a href="mailto:hi@x.com">mail</a>, <a href="#top">top</a>.</p>
<img src="/images/foo.jpg"><img src="https://example.com/images/bar.jpg"><img src="https://other.example/cdn.jpg">`
	imgs, links := extractAssetsFromHTML(html, "https://example.com/posts/hello")
	if len(imgs) != 3 {
		t.Errorf("want 3 images (cross-origin allowed for media re-upload), got %d: %v", len(imgs), imgs)
	}
	// Internal links: only same-host paths
	if len(links) != 2 {
		t.Errorf("want 2 internal links (/another-post, /about), got %d: %v", len(links), links)
	}
	for _, l := range links {
		if strings.Contains(l, "other.example") || strings.Contains(l, "mailto") || strings.Contains(l, "#") {
			t.Errorf("external/anchor/mailto leaked: %q", l)
		}
	}
}

// ----- Mock WP server fixtures -----

type fakeWP struct {
	t          *testing.T
	srv        *httptest.Server
	hits       atomic.Int64
	authHeader string // if set, server returns 401 unless Authorization matches

	pages      []wpPage
	posts      []wpPage
	categories []wpTerm
	tags       []wpTerm
	media      map[int64]wpMedia // by ID; embedded into pages/posts via _embedded
}

func newFakeWP(t *testing.T) *fakeWP {
	t.Helper()
	fw := &fakeWP{t: t, media: map[int64]wpMedia{}}
	mux := http.NewServeMux()

	// Discovery root
	mux.HandleFunc("/wp-json/", func(w http.ResponseWriter, r *http.Request) {
		if !fw.checkAuth(w, r) {
			return
		}
		fw.hits.Add(1)
		_ = json.NewEncoder(w).Encode(wpRoot{
			Name: "FakeWP", Description: "Test", URL: "http://" + r.Host, Home: "http://" + r.Host,
			Namespaces: []string{"wp/v2", "oembed/1.0"},
		})
	})

	mux.HandleFunc("/wp-json/wp/v2/pages", func(w http.ResponseWriter, r *http.Request) {
		if !fw.checkAuth(w, r) {
			return
		}
		fw.servePaginatedContent(w, r, fw.pages)
	})
	mux.HandleFunc("/wp-json/wp/v2/posts", func(w http.ResponseWriter, r *http.Request) {
		if !fw.checkAuth(w, r) {
			return
		}
		fw.servePaginatedContent(w, r, fw.posts)
	})
	mux.HandleFunc("/wp-json/wp/v2/categories", func(w http.ResponseWriter, r *http.Request) {
		fw.servePaginatedTerms(w, r, fw.categories)
	})
	mux.HandleFunc("/wp-json/wp/v2/tags", func(w http.ResponseWriter, r *http.Request) {
		fw.servePaginatedTerms(w, r, fw.tags)
	})

	fw.srv = httptest.NewServer(mux)
	t.Cleanup(fw.srv.Close)
	return fw
}

func (fw *fakeWP) checkAuth(w http.ResponseWriter, r *http.Request) bool {
	if fw.authHeader == "" {
		return true
	}
	if r.Header.Get("Authorization") == fw.authHeader {
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"code":"rest_not_logged_in","message":"You are not currently logged in.","data":{"status":401}}`))
	return false
}

func (fw *fakeWP) servePaginatedContent(w http.ResponseWriter, r *http.Request, all []wpPage) {
	page := 1
	if v := r.URL.Query().Get("page"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &page)
	}
	perPage := 100
	if v := r.URL.Query().Get("per_page"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &perPage)
	}
	embed := r.URL.Query().Get("_embed") == "1"
	start := (page - 1) * perPage
	end := start + perPage
	if start > len(all) {
		start = len(all)
	}
	if end > len(all) {
		end = len(all)
	}
	batch := append([]wpPage(nil), all[start:end]...)
	if embed {
		for i := range batch {
			fw.embedInto(&batch[i])
		}
	}
	totalPages := (len(all) + perPage - 1) / perPage
	if totalPages == 0 {
		totalPages = 1
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-WP-Total", fmt.Sprintf("%d", len(all)))
	w.Header().Set("X-WP-TotalPages", fmt.Sprintf("%d", totalPages))
	_ = json.NewEncoder(w).Encode(batch)
}

func (fw *fakeWP) servePaginatedTerms(w http.ResponseWriter, r *http.Request, all []wpTerm) {
	page := 1
	if v := r.URL.Query().Get("page"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &page)
	}
	perPage := 100
	start := (page - 1) * perPage
	end := start + perPage
	if start > len(all) {
		start = len(all)
	}
	if end > len(all) {
		end = len(all)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-WP-TotalPages", "1")
	_ = json.NewEncoder(w).Encode(all[start:end])
}

func (fw *fakeWP) embedInto(p *wpPage) {
	emb := &wpEmbedded{}
	if p.FeaturedMedia != 0 {
		if m, ok := fw.media[p.FeaturedMedia]; ok {
			emb.FeaturedMedia = []wpMedia{m}
		}
	}
	emb.Author = []wpAuthor{{ID: 1, Name: "Alice", Slug: "alice"}}
	p.Embedded = emb
}

// ----- Discovery -----

func TestWordPress_DiscoverHappyPath(t *testing.T) {
	fw := newFakeWP(t)
	c := NewWordPressClient(fw.srv.URL, WordPressOptions{
		FetchOpts: FetchOptions{AllowPrivate: true},
	})
	root, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if root.Name != "FakeWP" {
		t.Errorf("name: %q", root.Name)
	}
	if len(root.Namespaces) == 0 {
		t.Errorf("namespaces empty")
	}
}

func TestWordPress_DiscoverFailsWithoutWPV2(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(wpRoot{
			Name: "BrokenWP", Namespaces: []string{"oembed/1.0"}, // no wp/v2
		})
	}))
	defer srv.Close()
	c := NewWordPressClient(srv.URL, WordPressOptions{FetchOpts: FetchOptions{AllowPrivate: true}})
	if _, err := c.Discover(context.Background()); err == nil {
		t.Errorf("Discover should fail when wp/v2 namespace is missing")
	}
}

// ----- Auth pass-through -----

func TestWordPress_AuthHeaderPassedThrough(t *testing.T) {
	fw := newFakeWP(t)
	fw.authHeader = "Basic dXNlcjphcHA="

	c := NewWordPressClient(fw.srv.URL, WordPressOptions{
		AuthHeader: "Basic dXNlcjphcHA=",
		FetchOpts:  FetchOptions{AllowPrivate: true},
	})
	if _, err := c.Discover(context.Background()); err != nil {
		t.Errorf("auth-header request should have succeeded: %v", err)
	}

	// Now without auth - should 401 and surface the WP error code.
	bad := NewWordPressClient(fw.srv.URL, WordPressOptions{FetchOpts: FetchOptions{AllowPrivate: true}})
	_, err := bad.Discover(context.Background())
	if err == nil {
		t.Fatalf("missing auth should fail")
	}
	if !strings.Contains(err.Error(), "rest_not_logged_in") {
		t.Errorf("error should expose WP error code, got %v", err)
	}
}

// ----- SSRF guard inherited from SafeFetch -----

func TestWordPress_SSRFGuardInherited(t *testing.T) {
	c := NewWordPressClient("http://127.0.0.1:1", WordPressOptions{}) // AllowPrivate left false
	if _, err := c.Discover(context.Background()); err == nil {
		t.Errorf("WP client must inherit SSRF guard")
	}
}

// ----- Crawl: end-to-end mapping -----

func TestWordPress_CrawlMapsPagesPostsCategoriesTagsAndMedia(t *testing.T) {
	fw := newFakeWP(t)
	fw.categories = []wpTerm{
		{ID: 10, Slug: "guides", Name: "Guides", Taxonomy: "category"},
		{ID: 11, Slug: "news", Name: "News", Taxonomy: "category"},
	}
	fw.tags = []wpTerm{
		{ID: 20, Slug: "seo", Name: "SEO", Taxonomy: "post_tag"},
		{ID: 21, Slug: "wp", Name: "WP", Taxonomy: "post_tag"},
	}
	fw.media[100] = wpMedia{
		ID: 100, SourceURL: fw.srv.URL + "/wp-content/uploads/featured.jpg",
		AltText: "Featured", MediaType: "image", MimeType: "image/jpeg",
	}
	fw.pages = []wpPage{
		{
			ID: 1, Slug: "about", Status: "publish",
			DateGMT: "2024-01-15T12:00:00",
			Title:   wpRendered{Rendered: "About Us"},
			Content: wpRendered{Rendered: `<p>Hello <a href="/contact">contact</a></p><img src="/wp-content/uploads/abt.png">`},
			Excerpt: wpRendered{Rendered: "<p>Short.</p>"},
			Link:    fw.srv.URL + "/about/",
		},
	}
	fw.posts = []wpPage{
		{
			ID: 50, Slug: "first-post", Status: "publish",
			DateGMT:       "2024-02-20T09:30:00",
			Title:         wpRendered{Rendered: "First &amp; Best Post"},
			Content:       wpRendered{Rendered: `<p>Body</p><img src="/wp-content/uploads/inline.png">`},
			Excerpt:       wpRendered{Rendered: "<p>Excerpt.</p>"},
			Link:          fw.srv.URL + "/posts/first-post/",
			FeaturedMedia: 100,
			Categories:    []int64{10},
			Tags:          []int64{20, 21},
		},
		{
			ID: 51, Slug: "second-post", Status: "publish",
			DateGMT:    "2024-03-01T15:00:00",
			Title:      wpRendered{Rendered: "Second"},
			Content:    wpRendered{Rendered: "<p>Two</p>"},
			Link:       fw.srv.URL + "/posts/second-post/",
			Categories: []int64{11},
		},
	}

	c := NewWordPressClient(fw.srv.URL, WordPressOptions{FetchOpts: FetchOptions{AllowPrivate: true}})
	manifest, err := c.Crawl(context.Background(), WordPressOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v\nwarnings=%v", err, manifest)
	}

	// Pages
	if len(manifest.Pages) != 1 {
		t.Fatalf("want 1 page, got %d", len(manifest.Pages))
	}
	page := manifest.Pages[0]
	if page.Title != "About Us" {
		t.Errorf("page title: %q", page.Title)
	}
	if page.SourcePath != "/about/" {
		t.Errorf("page path: %q", page.SourcePath)
	}
	if page.PublishedAt.IsZero() {
		t.Errorf("publish date not parsed")
	}
	if len(page.InternalLinks) != 1 || page.InternalLinks[0] != "/contact" {
		t.Errorf("page internal links: %v", page.InternalLinks)
	}
	if len(page.ImageURLs) != 1 {
		t.Errorf("page images: %v", page.ImageURLs)
	}

	// Collection: posts
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
	if first.Title != "First & Best Post" {
		t.Errorf("title not entity-decoded: %q", first.Title)
	}
	if first.Slug != "first-post" {
		t.Errorf("slug: %q", first.Slug)
	}
	if first.PublishedAt.IsZero() {
		t.Errorf("post publish date not parsed")
	}
	if cats, ok := first.Data["categories"].([]string); !ok || len(cats) != 1 || cats[0] != "guides" {
		t.Errorf("categories not mapped to slugs: %v", first.Data["categories"])
	}
	if tags, ok := first.Data["tags"].([]string); !ok || len(tags) != 2 {
		t.Errorf("tags not mapped: %v", first.Data["tags"])
	}
	if fi, ok := first.Data["featured_image"].(string); !ok || !strings.HasSuffix(fi, "/featured.jpg") {
		t.Errorf("featured_image missing or wrong: %v", first.Data["featured_image"])
	}
	if author, _ := first.Data["author"].(string); author != "Alice" {
		t.Errorf("author not embedded: %q", author)
	}

	// Media: featured + inline (deduped)
	gotMedia := map[string]bool{}
	for _, m := range manifest.Media {
		gotMedia[m.SourceURL] = true
	}
	if !gotMedia[fw.srv.URL+"/wp-content/uploads/featured.jpg"] {
		t.Errorf("featured media missing from manifest.Media: %v", manifest.Media)
	}
	if !gotMedia[fw.srv.URL+"/wp-content/uploads/inline.png"] {
		t.Errorf("inline image missing from manifest.Media: %v", manifest.Media)
	}
	if !gotMedia[fw.srv.URL+"/wp-content/uploads/abt.png"] {
		t.Errorf("page inline image missing: %v", manifest.Media)
	}

	// Stats sanity
	if manifest.Stats.PagesFound != 1 {
		t.Errorf("stats.PagesFound = %d", manifest.Stats.PagesFound)
	}
	if manifest.Stats.CollectionsFound != 1 {
		t.Errorf("stats.CollectionsFound = %d", manifest.Stats.CollectionsFound)
	}
	if manifest.Stats.MediaFound < 3 {
		t.Errorf("stats.MediaFound: %d", manifest.Stats.MediaFound)
	}
}

// ----- Pagination -----

func TestWordPress_PaginationFollowsXWPTotalPages(t *testing.T) {
	fw := newFakeWP(t)
	// Generate 250 posts so we hit 3 pages at per_page=100.
	for i := 0; i < 250; i++ {
		fw.posts = append(fw.posts, wpPage{
			ID: int64(1000 + i), Slug: fmt.Sprintf("p%d", i), Status: "publish",
			DateGMT: "2024-01-01T00:00:00",
			Title:   wpRendered{Rendered: fmt.Sprintf("Post %d", i)},
			Content: wpRendered{Rendered: ""},
			Link:    fmt.Sprintf("%s/posts/p%d/", fw.srv.URL, i),
		})
	}
	c := NewWordPressClient(fw.srv.URL, WordPressOptions{FetchOpts: FetchOptions{AllowPrivate: true}})
	manifest, err := c.Crawl(context.Background(), WordPressOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if len(manifest.Collections) != 1 || len(manifest.Collections[0].Items) != 250 {
		t.Errorf("pagination didn't return all 250 posts; got %d items in collection", func() int {
			if len(manifest.Collections) == 0 {
				return 0
			}
			return len(manifest.Collections[0].Items)
		}())
	}
}

func TestWordPress_RespectsMaxPagesCap(t *testing.T) {
	fw := newFakeWP(t)
	for i := 0; i < 250; i++ {
		fw.posts = append(fw.posts, wpPage{
			ID: int64(1000 + i), Slug: fmt.Sprintf("p%d", i), Status: "publish",
			DateGMT: "2024-01-01T00:00:00",
			Title:   wpRendered{Rendered: "P"}, Content: wpRendered{Rendered: ""},
			Link: fmt.Sprintf("%s/p%d/", fw.srv.URL, i),
		})
	}
	c := NewWordPressClient(fw.srv.URL, WordPressOptions{FetchOpts: FetchOptions{AllowPrivate: true}})
	manifest, err := c.Crawl(context.Background(), WordPressOptions{MaxPagesPerEndpoint: 2})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	got := len(manifest.Collections[0].Items)
	if got != 200 {
		t.Errorf("MaxPagesPerEndpoint=2 should yield 200 items (2 * per_page=100), got %d", got)
	}
}

// ----- Empty installs -----

func TestWordPress_EmptySiteCompletesCleanly(t *testing.T) {
	fw := newFakeWP(t)
	c := NewWordPressClient(fw.srv.URL, WordPressOptions{FetchOpts: FetchOptions{AllowPrivate: true}})
	manifest, err := c.Crawl(context.Background(), WordPressOptions{})
	if err != nil {
		t.Fatalf("empty WP must not error: %v", err)
	}
	if len(manifest.Pages) != 0 || len(manifest.Collections) != 0 {
		t.Errorf("expected empty manifest, got %+v", manifest)
	}
}

// ----- WP error shape surfaced on 4xx -----

func TestWordPress_StatusErrorIncludesWPCodeAndMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"rest_forbidden","message":"Sorry, you cannot do that.","data":{"status":403}}`))
	}))
	defer srv.Close()
	c := NewWordPressClient(srv.URL, WordPressOptions{FetchOpts: FetchOptions{AllowPrivate: true}})
	_, err := c.Discover(context.Background())
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "rest_forbidden") {
		t.Errorf("WP error code should surface, got %v", err)
	}
}

// ----- Date parsing -----

func TestWordPress_DateParsing_HandlesNonZSuffix(t *testing.T) {
	p := wpPage{
		Slug: "x", Status: "publish",
		DateGMT: "2024-06-01T12:34:56",
		Title:   wpRendered{Rendered: "X"},
		Link:    "https://example.com/x/",
	}
	mp := wpToMigrationPage(p, "example.com")
	if mp.PublishedAt.IsZero() {
		t.Errorf("WP date_gmt without trailing Z should still parse")
	}
	want := time.Date(2024, 6, 1, 12, 34, 56, 0, time.UTC)
	if !mp.PublishedAt.Equal(want) {
		t.Errorf("parsed date = %v, want %v", mp.PublishedAt, want)
	}
}
