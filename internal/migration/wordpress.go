package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// WordPressClient consumes /wp-json/wp/v2 against a target install. The
// shape sticks to the public REST API the WP core exposes (no plugin-
// specific extensions, no admin endpoints) so it works against the long
// tail of WP installs we'll see in the wild without per-host config.
//
// Why a native importer when CrawlSitemap already works: the REST API
// hands us the slug, the GMT publish date, the category/tag taxonomy,
// and the featured media URL as structured data. Sitemap-crawl has to
// guess all of that from the rendered HTML, and gets it wrong on installs
// with custom themes that strip the usual schema markers. Native = same
// page, three times the metadata fidelity, half the HTTP requests.
type WordPressClient struct {
	baseURL    string
	httpHeader http.Header
	fetchOpts  FetchOptions
}

// WordPressOptions carries the bits the operator may need to set per-site.
// Empty fields use the importer defaults.
type WordPressOptions struct {
	// AuthHeader, if set, is appended to every request. Use for
	// WP Application Passwords ("Authorization: Basic base64(user:pass)"
	// where pass is the application password) or custom auth plugins.
	AuthHeader string

	// FetchOpts overrides the SSRF / size / timeout defaults.
	FetchOpts FetchOptions

	// MaxPagesPerEndpoint caps the pagination walk per content type
	// so a malicious or runaway WP install can't make us pull millions
	// of rows. Default 100 (= 10,000 rows at per_page=100).
	MaxPagesPerEndpoint int
}

const wpDefaultMaxPagesPerEndpoint = 100

// NewWordPressClient constructs the client. baseURL can be the site root
// (`https://example.com`), the wp-json root (`https://example.com/wp-json`),
// or the wp/v2 root (`https://example.com/wp-json/wp/v2`) - all three forms
// resolve correctly. Trailing slashes are tolerated.
func NewWordPressClient(baseURL string, opts WordPressOptions) *WordPressClient {
	c := &WordPressClient{
		baseURL:    normalizeWPBaseURL(baseURL),
		httpHeader: http.Header{},
		fetchOpts:  opts.FetchOpts,
	}
	if opts.AuthHeader != "" {
		c.httpHeader.Set("Authorization", opts.AuthHeader)
	}
	c.fetchOpts.ExtraHeaders = mergeHeaders(c.fetchOpts.ExtraHeaders, c.httpHeader)
	return c
}

// normalizeWPBaseURL turns any of the three accepted shapes into the canonical
// `https://example.com/wp-json/wp/v2` form, no trailing slash.
func normalizeWPBaseURL(raw string) string {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	switch {
	case strings.HasSuffix(s, "/wp-json/wp/v2"):
		return s
	case strings.HasSuffix(s, "/wp-json"):
		return s + "/wp/v2"
	default:
		return s + "/wp-json/wp/v2"
	}
}

func mergeHeaders(dst, src http.Header) http.Header {
	if dst == nil {
		dst = http.Header{}
	}
	for k, vs := range src {
		for _, v := range vs {
			dst.Set(k, v)
		}
	}
	return dst
}

// ----- WP REST shapes -----

type wpRendered struct {
	Rendered string `json:"rendered"`
}

// wpPage covers both /pages and /posts; the fields that overlap are all we
// touch. Custom post types from plugins look the same and slot right in.
type wpPage struct {
	ID            int64       `json:"id"`
	Date          string      `json:"date"`
	DateGMT       string      `json:"date_gmt"`
	Modified      string      `json:"modified_gmt"`
	Slug          string      `json:"slug"`
	Status        string      `json:"status"`
	Title         wpRendered  `json:"title"`
	Content       wpRendered  `json:"content"`
	Excerpt       wpRendered  `json:"excerpt"`
	Link          string      `json:"link"`
	FeaturedMedia int64       `json:"featured_media"`
	Categories    []int64     `json:"categories"`
	Tags          []int64     `json:"tags"`
	Embedded      *wpEmbedded `json:"_embedded,omitempty"`
}

type wpEmbedded struct {
	Author        []wpAuthor `json:"author,omitempty"`
	FeaturedMedia []wpMedia  `json:"wp:featuredmedia,omitempty"`
	Term          [][]wpTerm `json:"wp:term,omitempty"`
}

type wpAuthor struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
	Link string `json:"link"`
}

type wpMedia struct {
	ID        int64      `json:"id"`
	SourceURL string     `json:"source_url"`
	AltText   string     `json:"alt_text"`
	MediaType string     `json:"media_type"`
	MimeType  string     `json:"mime_type"`
	Title     wpRendered `json:"title"`
	Caption   wpRendered `json:"caption"`
	Link      string     `json:"link"`
	Date      string     `json:"date_gmt"`
}

type wpTerm struct {
	ID       int64  `json:"id"`
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Taxonomy string `json:"taxonomy"`
}

// wpRoot is what /wp-json/ returns: the discovery doc.
type wpRoot struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	URL         string   `json:"url"`
	Home        string   `json:"home"`
	Namespaces  []string `json:"namespaces"`
}

// ----- Public surface -----

// Discover hits the wp-json root to confirm the REST API is enabled and the
// `wp/v2` namespace is registered. Returns the site name + home URL so the
// migration UI can show "About to import 'Acme Corp' from acme.example".
func (c *WordPressClient) Discover(ctx context.Context) (*wpRoot, error) {
	// The discovery endpoint is at /wp-json/, one level above /wp/v2.
	rootURL := strings.TrimSuffix(c.baseURL, "/wp/v2")
	body, err := c.getBody(ctx, rootURL+"/")
	if err != nil {
		return nil, fmt.Errorf("discover wp-json root: %w", err)
	}
	var root wpRoot
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("decode wp-json root: %w (body starts %q)", err, peek(body, 80))
	}
	hasV2 := false
	for _, ns := range root.Namespaces {
		if ns == "wp/v2" {
			hasV2 = true
			break
		}
	}
	if !hasV2 {
		return &root, fmt.Errorf("wp/v2 namespace not registered (got %v)", root.Namespaces)
	}
	return &root, nil
}

// Crawl pulls pages, posts, categories, tags, and embedded media into a
// MigrationManifest. Returns a partial manifest + error if any single
// endpoint fails fatally; per-row issues become Warnings instead.
func (c *WordPressClient) Crawl(ctx context.Context, opts WordPressOptions) (*MigrationManifest, error) {
	if opts.MaxPagesPerEndpoint == 0 {
		opts.MaxPagesPerEndpoint = wpDefaultMaxPagesPerEndpoint
	}

	manifest := &MigrationManifest{
		SourceURL:  c.baseURL,
		SourceType: SourceWordPress,
		FetchedAt:  time.Now().UTC(),
	}
	if u, err := url.Parse(c.baseURL); err == nil {
		manifest.SourceDomain = u.Host
	}

	root, err := c.Discover(ctx)
	if err != nil {
		return nil, err
	}
	if root.Home != "" {
		if u, err := url.Parse(root.Home); err == nil && u.Host != "" {
			manifest.SourceDomain = u.Host
		}
	}

	// Categories + tags first so post items can reference them by slug.
	categories, catWarn := c.fetchTerms(ctx, "categories", opts.MaxPagesPerEndpoint)
	tags, tagWarn := c.fetchTerms(ctx, "tags", opts.MaxPagesPerEndpoint)
	manifest.Warnings = append(manifest.Warnings, catWarn...)
	manifest.Warnings = append(manifest.Warnings, tagWarn...)

	// Pages: published only.
	wpPages, pagesWarn := c.fetchContent(ctx, "pages", opts.MaxPagesPerEndpoint)
	manifest.Warnings = append(manifest.Warnings, pagesWarn...)

	// Posts: published only. Maps to a "posts" collection.
	wpPosts, postsWarn := c.fetchContent(ctx, "posts", opts.MaxPagesPerEndpoint)
	manifest.Warnings = append(manifest.Warnings, postsWarn...)

	manifest.Stats.URLsRequested = len(wpPages) + len(wpPosts)

	// ---- Pages -> MigrationPages
	mediaSet := map[string]MigrationMediaRef{}
	for _, p := range wpPages {
		mp := wpToMigrationPage(p, manifest.SourceDomain)
		manifest.Pages = append(manifest.Pages, mp)
		registerMedia(mediaSet, mp.OGImageURL, mp.SourcePath, "")
		for _, img := range mp.ImageURLs {
			registerMedia(mediaSet, img, mp.SourcePath, "")
		}
		for _, l := range mp.InternalLinks {
			manifest.Links = append(manifest.Links, MigrationLink{From: mp.SourcePath, To: l})
		}
	}
	manifest.Stats.URLsFetched += len(wpPages)

	// ---- Posts -> MigrationCollection
	if len(wpPosts) > 0 {
		coll := MigrationCollection{
			Slug: "posts",
			Name: "Posts",
			Schema: []MigrationCollectionField{
				{Name: "title", Type: "text", Label: "Title", Required: true},
				{Name: "excerpt", Type: "textarea", Label: "Excerpt"},
				{Name: "content", Type: "richtext", Label: "Content"},
				{Name: "featured_image", Type: "url", Label: "Featured image URL"},
				{Name: "author", Type: "text", Label: "Author"},
				{Name: "categories", Type: "multiselect", Label: "Categories"},
				{Name: "tags", Type: "multiselect", Label: "Tags"},
			},
			Items: make([]MigrationCollectionItem, 0, len(wpPosts)),
		}
		catByID := termsByID(categories)
		tagByID := termsByID(tags)
		for _, p := range wpPosts {
			item := wpToCollectionItem(p, catByID, tagByID, manifest.SourceDomain)
			coll.Items = append(coll.Items, item)

			// Pull image URLs out of the rendered HTML for media re-upload.
			images, internalLinks := extractAssetsFromHTML(p.Content.Rendered, c.baseURL)
			if fm := wpFeaturedMediaURL(p); fm != "" {
				registerMedia(mediaSet, fm, "", item.Slug)
			}
			for _, img := range images {
				registerMedia(mediaSet, img, "", item.Slug)
			}
			postPath := pathOf(p.Link)
			for _, l := range internalLinks {
				manifest.Links = append(manifest.Links, MigrationLink{From: postPath, To: l})
			}
		}
		manifest.Collections = append(manifest.Collections, coll)
	}
	manifest.Stats.URLsFetched += len(wpPosts)

	// ---- Media: dedup + emit
	for _, m := range mediaSet {
		manifest.Media = append(manifest.Media, m)
	}

	manifest.Stats.PagesFound = len(manifest.Pages)
	manifest.Stats.CollectionsFound = len(manifest.Collections)
	manifest.Stats.MediaFound = len(manifest.Media)
	manifest.Stats.LinksFound = len(manifest.Links)

	slog.Info("wordpress crawl complete",
		"source", c.baseURL,
		"pages", len(manifest.Pages),
		"collections", len(manifest.Collections),
		"media", len(manifest.Media),
		"warnings", len(manifest.Warnings))

	return manifest, nil
}

// ----- Mapping helpers -----

func wpToMigrationPage(p wpPage, sourceDomain string) MigrationPage {
	mp := MigrationPage{
		SourceURL:       p.Link,
		SourcePath:      pathOf(p.Link),
		Title:           stripHTMLTags(p.Title.Rendered),
		MetaDescription: stripHTMLTags(p.Excerpt.Rendered),
		HTML:            p.Content.Rendered,
	}
	if mp.SourcePath == "" {
		mp.SourcePath = "/" + p.Slug
	}
	if t, err := time.Parse(time.RFC3339, p.DateGMT+"Z"); err == nil {
		mp.PublishedAt = t
	} else if t, err := time.Parse("2006-01-02T15:04:05", p.DateGMT); err == nil {
		mp.PublishedAt = t.UTC()
	}
	if fm := wpFeaturedMediaURL(p); fm != "" {
		mp.OGImageURL = fm
	}
	images, internalLinks := extractAssetsFromHTML(p.Content.Rendered, p.Link)
	mp.ImageURLs = images
	mp.InternalLinks = internalLinks
	return mp
}

func wpToCollectionItem(p wpPage, catByID, tagByID map[int64]wpTerm, sourceDomain string) MigrationCollectionItem {
	cats := make([]string, 0, len(p.Categories))
	for _, id := range p.Categories {
		if t, ok := catByID[id]; ok {
			cats = append(cats, t.Slug)
		}
	}
	tags := make([]string, 0, len(p.Tags))
	for _, id := range p.Tags {
		if t, ok := tagByID[id]; ok {
			tags = append(tags, t.Slug)
		}
	}
	authorName := ""
	if p.Embedded != nil && len(p.Embedded.Author) > 0 {
		authorName = p.Embedded.Author[0].Name
	}
	item := MigrationCollectionItem{
		SourceURL: p.Link,
		Slug:      p.Slug,
		Title:     stripHTMLTags(p.Title.Rendered),
		Data: map[string]any{
			"title":          stripHTMLTags(p.Title.Rendered),
			"excerpt":        stripHTMLTags(p.Excerpt.Rendered),
			"content":        p.Content.Rendered,
			"featured_image": wpFeaturedMediaURL(p),
			"author":         authorName,
			"categories":     cats,
			"tags":           tags,
		},
	}
	if t, err := time.Parse(time.RFC3339, p.DateGMT+"Z"); err == nil {
		item.PublishedAt = t
	}
	return item
}

func wpFeaturedMediaURL(p wpPage) string {
	if p.Embedded != nil && len(p.Embedded.FeaturedMedia) > 0 {
		return p.Embedded.FeaturedMedia[0].SourceURL
	}
	return ""
}

func termsByID(terms []wpTerm) map[int64]wpTerm {
	out := make(map[int64]wpTerm, len(terms))
	for _, t := range terms {
		out[t.ID] = t
	}
	return out
}

func registerMedia(set map[string]MigrationMediaRef, src, onPage, onItem string) {
	if src == "" {
		return
	}
	if _, ok := set[src]; ok {
		return
	}
	set[src] = MigrationMediaRef{SourceURL: src, OnPage: onPage}
}

func pathOf(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Path == "" {
		return "/"
	}
	return u.Path
}

// extractAssetsFromHTML parses an HTML fragment (the `content.rendered`
// blob from WP) and returns the image src URLs and internal link paths
// found inside. Same heuristics as extractPage in sitemap.go but for
// fragments without the surrounding <html>/<head>/<body>.
func extractAssetsFromHTML(rawHTML, baseURL string) (images []string, internalLinks []string) {
	if strings.TrimSpace(rawHTML) == "" {
		return nil, nil
	}
	doc, err := html.Parse(strings.NewReader(rawHTML))
	if err != nil {
		return nil, nil
	}
	host := hostOf(baseURL)
	imgSet := map[string]bool{}
	linkSet := map[string]bool{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch strings.ToLower(n.Data) {
			case "img":
				if src := absURL(baseURL, attr(n, "src")); src != "" && !imgSet[src] {
					imgSet[src] = true
					images = append(images, src)
				}
			case "a":
				href := strings.TrimSpace(attr(n, "href"))
				if href == "" || strings.HasPrefix(href, "#") ||
					strings.HasPrefix(href, "mailto:") || strings.HasPrefix(href, "tel:") ||
					strings.HasPrefix(href, "javascript:") {
					break
				}
				abs := absURL(baseURL, href)
				if abs == "" {
					break
				}
				if hostOf(abs) == host {
					if u, err := url.Parse(abs); err == nil {
						p := u.Path
						if p == "" {
							p = "/"
						}
						if !linkSet[p] {
							linkSet[p] = true
							internalLinks = append(internalLinks, p)
						}
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return images, internalLinks
}

// stripHTMLTags pulls plain text out of WP's `title.rendered` /
// `excerpt.rendered`, which embed entities and inline markup we don't
// want in our title field.
func stripHTMLTags(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	doc, err := html.Parse(strings.NewReader(raw))
	if err != nil {
		return raw
	}
	return strings.TrimSpace(textContent(doc))
}

// ----- Pagination + fetch -----

// fetchContent walks `/posts` or `/pages` (or any custom post type endpoint
// with the same shape) using `_embed=1` so author/featured-media/term
// links arrive in one request. Pagination follows the X-WP-TotalPages
// header. Only `status=publish` rows are returned (drafts and trash stay
// hidden so the migration doesn't surface unpublished content).
func (c *WordPressClient) fetchContent(ctx context.Context, endpoint string, maxPages int) ([]wpPage, []string) {
	var all []wpPage
	var warnings []string
	for page := 1; page <= maxPages; page++ {
		u := fmt.Sprintf("%s/%s?per_page=100&page=%d&status=publish&_embed=1", c.baseURL, endpoint, page)
		body, totalPages, err := c.getBodyWithPaging(ctx, u)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("fetch %s page %d: %v", endpoint, page, err))
			return all, warnings
		}
		var batch []wpPage
		if err := json.Unmarshal(body, &batch); err != nil {
			warnings = append(warnings, fmt.Sprintf("decode %s page %d: %v", endpoint, page, err))
			return all, warnings
		}
		all = append(all, batch...)
		if page >= totalPages || len(batch) == 0 {
			break
		}
	}
	return all, warnings
}

// fetchTerms walks /categories or /tags. Same pagination contract.
func (c *WordPressClient) fetchTerms(ctx context.Context, endpoint string, maxPages int) ([]wpTerm, []string) {
	var all []wpTerm
	var warnings []string
	for page := 1; page <= maxPages; page++ {
		u := fmt.Sprintf("%s/%s?per_page=100&page=%d", c.baseURL, endpoint, page)
		body, totalPages, err := c.getBodyWithPaging(ctx, u)
		if err != nil {
			// `categories` and `tags` may legitimately be empty (404 with
			// `rest_term_invalid` or just no rows) - record but don't
			// abort the crawl.
			warnings = append(warnings, fmt.Sprintf("fetch %s page %d: %v", endpoint, page, err))
			return all, warnings
		}
		var batch []wpTerm
		if err := json.Unmarshal(body, &batch); err != nil {
			warnings = append(warnings, fmt.Sprintf("decode %s page %d: %v", endpoint, page, err))
			return all, warnings
		}
		all = append(all, batch...)
		if page >= totalPages || len(batch) == 0 {
			break
		}
	}
	return all, warnings
}

// getBody fetches and returns the raw JSON body (or an SSRF/HTTP error).
func (c *WordPressClient) getBody(ctx context.Context, rawURL string) ([]byte, error) {
	resp, err := SafeFetch(ctx, rawURL, c.fetchOpts)
	if err != nil {
		return nil, err
	}
	if err := wpStatusError(rawURL, resp); err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// getBodyWithPaging is getBody plus parsing of the X-WP-TotalPages header,
// which the WP REST API emits on list endpoints. Defaults to 1 when absent
// (single-page response).
func (c *WordPressClient) getBodyWithPaging(ctx context.Context, rawURL string) ([]byte, int, error) {
	resp, err := safeFetchPreservingHeaders(ctx, rawURL, c.fetchOpts)
	if err != nil {
		return nil, 0, err
	}
	if err := wpStatusError(rawURL, resp.result); err != nil {
		return nil, 0, err
	}
	totalPages := 1
	if v := resp.headers.Get("X-WP-TotalPages"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			totalPages = n
		}
	}
	return resp.result.Body, totalPages, nil
}

// wpStatusError is the WP-aware error mapper. WP returns its own error
// JSON shape for 4xx/5xx so we surface that message verbatim instead of
// just "status 401".
func wpStatusError(reqURL string, r *FetchResult) error {
	if r.Status >= 200 && r.Status < 300 {
		return nil
	}
	var werr struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Status int `json:"status"`
		} `json:"data"`
	}
	if json.Unmarshal(r.Body, &werr) == nil && werr.Code != "" {
		return fmt.Errorf("wp-api %s: %d %s (%s)", reqURL, r.Status, werr.Code, werr.Message)
	}
	return fmt.Errorf("wp-api %s: status %d", reqURL, r.Status)
}

// peek returns the first n bytes of b as a string for error messages.
func peek(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n])
}

// ----- header-aware fetch shim -----

// SafeFetch returns FetchResult{Status, ContentType, Body} but no other
// headers. WP's pagination needs X-WP-TotalPages so we wrap a thin
// version that captures the full Header map. Stays in this file because
// it's only useful for WP today; if a future importer needs the same
// it'll graduate to safefetch.go.
type fetchWithHeaders struct {
	result  *FetchResult
	headers http.Header
}

func safeFetchPreservingHeaders(ctx context.Context, rawURL string, opts FetchOptions) (*fetchWithHeaders, error) {
	if opts.MaxBytes == 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.MaxRedirects == 0 {
		opts.MaxRedirects = DefaultMaxRedirects
	}
	if opts.Timeout == 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.AcceptHeader == "" {
		opts.AcceptHeader = "application/json"
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("only http/https URLs allowed (got %q)", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("URL missing host")
	}
	if !opts.AllowPrivate {
		if err := checkHostPublic(u.Hostname()); err != nil {
			return nil, err
		}
	}

	client := &http.Client{
		Timeout:   opts.Timeout,
		Transport: safeTransport(opts.AllowPrivate),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= opts.MaxRedirects {
				return fmt.Errorf("too many redirects (>%d)", opts.MaxRedirects)
			}
			if !opts.AllowPrivate {
				if err := checkHostPublic(req.URL.Hostname()); err != nil {
					return fmt.Errorf("redirect blocked: %w", err)
				}
			}
			return nil
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", opts.AcceptHeader)
	req.Header.Set("Accept-Language", "en;q=0.8")
	for k, vals := range opts.ExtraHeaders {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	body := make([]byte, 0)
	buf := make([]byte, 32*1024)
	var read int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			read += int64(n)
			if read > opts.MaxBytes {
				return nil, fmt.Errorf("response exceeds max size %d bytes", opts.MaxBytes)
			}
			body = append(body, buf[:n]...)
		}
		if rerr != nil {
			break
		}
	}

	return &fetchWithHeaders{
		result: &FetchResult{
			URL:         resp.Request.URL.String(),
			Status:      resp.StatusCode,
			ContentType: resp.Header.Get("Content-Type"),
			Body:        body,
		},
		headers: resp.Header,
	}, nil
}
