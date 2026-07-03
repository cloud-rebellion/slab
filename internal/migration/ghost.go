package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"
)

// GhostClient consumes the Ghost Content API. We use the public Content API
// (key passed as `?key=` query string) rather than the Admin API so we never
// need session cookies or staff tokens. The Content API exposes posts,
// pages, tags, and authors - everything an editorial site has.
//
// Ghost's response shape is unusual among CMSs: lists arrive under a
// top-level key matching the resource ("posts", "pages", "tags") and
// `meta.pagination.pages` carries the total page count. Authors and tags
// are embedded into posts via `?include=tags,authors`.
type GhostClient struct {
	baseURL    string
	contentKey string
	fetchOpts  FetchOptions
}

// GhostOptions wraps the per-site config the operator supplies.
type GhostOptions struct {
	// BaseURL is the Ghost site root (`https://example.com`). The
	// Content API mounts at `/ghost/api/content/`. Required.
	BaseURL string

	// ContentKey is the Ghost Content API key. Generated under
	// Ghost admin -> Integrations -> Add custom integration. Required.
	ContentKey string

	// FetchOpts overrides SSRF / size / timeout defaults.
	FetchOpts FetchOptions

	// MaxPagesPerEndpoint caps the pagination walk per resource (posts,
	// pages, tags). Default 100 (= 10,000 rows at limit=100).
	MaxPagesPerEndpoint int
}

const (
	ghostDefaultMaxPagesPerEndpoint = 100
	ghostItemsPerPage               = 100 // Ghost API max
)

// ----- Ghost shapes -----

type ghostPost struct {
	ID              string        `json:"id"`
	UUID            string        `json:"uuid"`
	Slug            string        `json:"slug"`
	Title           string        `json:"title"`
	HTML            string        `json:"html"`
	FeatureImage    string        `json:"feature_image"`
	FeatureImageAlt string        `json:"feature_image_alt"`
	Excerpt         string        `json:"excerpt"`
	CustomExcerpt   string        `json:"custom_excerpt"`
	URL             string        `json:"url"`
	CreatedAt       string        `json:"created_at"`
	UpdatedAt       string        `json:"updated_at"`
	PublishedAt     string        `json:"published_at"`
	MetaTitle       string        `json:"meta_title"`
	MetaDescription string        `json:"meta_description"`
	OGTitle         string        `json:"og_title"`
	OGDescription   string        `json:"og_description"`
	OGImage         string        `json:"og_image"`
	Visibility      string        `json:"visibility"`
	Tags            []ghostTag    `json:"tags"`
	Authors         []ghostAuthor `json:"authors"`
}

type ghostTag struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Description string `json:"description"`
	URL         string `json:"url"`
	Visibility  string `json:"visibility"`
}

type ghostAuthor struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Slug         string `json:"slug"`
	ProfileImage string `json:"profile_image"`
	Email        string `json:"email"`
	URL          string `json:"url"`
}

type ghostMeta struct {
	Pagination ghostPagination `json:"pagination"`
}

type ghostPagination struct {
	Page  int `json:"page"`
	Limit int `json:"limit"`
	Pages int `json:"pages"`
	Total int `json:"total"`
	Next  int `json:"next"`
	Prev  int `json:"prev"`
}

type ghostPostsResponse struct {
	Posts []ghostPost `json:"posts"`
	Pages []ghostPost `json:"pages"`
	Meta  ghostMeta   `json:"meta"`
}

type ghostTagsResponse struct {
	Tags []ghostTag `json:"tags"`
	Meta ghostMeta  `json:"meta"`
}

type ghostErrorResponse struct {
	Errors []struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		ErrCode string `json:"code"`
	} `json:"errors"`
}

// ----- Public surface -----

// NewGhostClient constructs the client. Both BaseURL and ContentKey are
// required; the constructor returns an error if either is empty so a
// missing-config bug isn't silently turned into anonymous requests.
func NewGhostClient(opts GhostOptions) (*GhostClient, error) {
	if strings.TrimSpace(opts.BaseURL) == "" {
		return nil, fmt.Errorf("ghost base URL is required")
	}
	if strings.TrimSpace(opts.ContentKey) == "" {
		return nil, fmt.Errorf("ghost content API key is required")
	}
	c := &GhostClient{
		baseURL:    strings.TrimRight(opts.BaseURL, "/"),
		contentKey: opts.ContentKey,
		fetchOpts:  opts.FetchOpts,
	}
	c.fetchOpts.AcceptHeader = "application/json"
	return c, nil
}

// Discover does a 1-row probe of /posts to confirm the key is valid. Returns
// the site's display URL (extracted from the post URLs) so the migration UI
// can show "About to import from acme.example".
func (c *GhostClient) Discover(ctx context.Context) (string, error) {
	u := c.endpoint("/posts/", url.Values{
		"limit": {"1"},
	})
	body, err := c.getJSON(ctx, u)
	if err != nil {
		return "", fmt.Errorf("discover ghost: %w", err)
	}
	var resp ghostPostsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("decode ghost posts probe: %w (body starts %q)", err, peek(body, 80))
	}
	host := ""
	if len(resp.Posts) > 0 {
		if u, err := url.Parse(resp.Posts[0].URL); err == nil {
			host = u.Host
		}
	}
	if host == "" {
		// Fallback: parse base URL.
		if u, err := url.Parse(c.baseURL); err == nil {
			host = u.Host
		}
	}
	return host, nil
}

// Crawl pulls Ghost posts (-> "posts" collection), pages (-> MigrationPages),
// and tags (-> field options on the posts collection). Drafts and members-
// only posts are filtered server-side via the public Content API which only
// returns public/published rows by default.
func (c *GhostClient) Crawl(ctx context.Context, opts GhostOptions) (*MigrationManifest, error) {
	if opts.MaxPagesPerEndpoint == 0 {
		opts.MaxPagesPerEndpoint = ghostDefaultMaxPagesPerEndpoint
	}

	manifest := &MigrationManifest{
		SourceURL:  c.baseURL,
		SourceType: SourceGhost,
		FetchedAt:  time.Now().UTC(),
	}
	host, err := c.Discover(ctx)
	if err != nil {
		return nil, err
	}
	manifest.SourceDomain = host

	// Tags first: feed them into the post collection's tag field options.
	tags, tagsWarn := c.fetchTags(ctx, opts.MaxPagesPerEndpoint)
	manifest.Warnings = append(manifest.Warnings, tagsWarn...)

	// Pages -> MigrationPages
	gPages, pagesWarn := c.fetchResource(ctx, "pages", opts.MaxPagesPerEndpoint, false)
	manifest.Warnings = append(manifest.Warnings, pagesWarn...)
	for _, p := range gPages {
		manifest.Pages = append(manifest.Pages, ghostPostToMigrationPage(p, manifest.SourceDomain))
	}
	manifest.Stats.URLsFetched += len(manifest.Pages)

	// Posts -> "posts" collection with embedded tags + authors
	gPosts, postsWarn := c.fetchResource(ctx, "posts", opts.MaxPagesPerEndpoint, true)
	manifest.Warnings = append(manifest.Warnings, postsWarn...)

	mediaSet := map[string]MigrationMediaRef{}
	for _, p := range manifest.Pages {
		registerMedia(mediaSet, p.OGImageURL, p.SourcePath, "")
		for _, img := range p.ImageURLs {
			registerMedia(mediaSet, img, p.SourcePath, "")
		}
	}

	if len(gPosts) > 0 {
		coll := MigrationCollection{
			Slug: "posts",
			Name: "Posts",
			Schema: []MigrationCollectionField{
				{Name: "title", Type: "text", Label: "Title", Required: true},
				{Name: "excerpt", Type: "textarea", Label: "Excerpt"},
				{Name: "content", Type: "richtext", Label: "Content"},
				{Name: "feature_image", Type: "url", Label: "Feature image URL"},
				{Name: "feature_image_alt", Type: "text", Label: "Feature image alt"},
				{Name: "author", Type: "text", Label: "Author"},
				{Name: "tags", Type: "multiselect", Label: "Tags",
					Required: false},
			},
			Items: make([]MigrationCollectionItem, 0, len(gPosts)),
		}
		_ = tags // reserved for select-options enrichment in 3d
		for _, p := range gPosts {
			item := ghostPostToCollectionItem(p)
			coll.Items = append(coll.Items, item)
			registerMedia(mediaSet, p.FeatureImage, "", item.Slug)
			imgs, internalLinks := extractAssetsFromHTML(p.HTML, c.baseURL)
			for _, img := range imgs {
				registerMedia(mediaSet, img, "", item.Slug)
			}
			postPath := pathOf(p.URL)
			for _, l := range internalLinks {
				manifest.Links = append(manifest.Links, MigrationLink{From: postPath, To: l})
			}
		}
		manifest.Collections = append(manifest.Collections, coll)
		manifest.Stats.URLsFetched += len(gPosts)
	}

	for _, m := range mediaSet {
		manifest.Media = append(manifest.Media, m)
	}

	manifest.Stats.PagesFound = len(manifest.Pages)
	manifest.Stats.CollectionsFound = len(manifest.Collections)
	manifest.Stats.MediaFound = len(manifest.Media)
	manifest.Stats.LinksFound = len(manifest.Links)
	manifest.Stats.URLsRequested = manifest.Stats.URLsFetched

	slog.Info("ghost crawl complete",
		"host", host,
		"pages", len(manifest.Pages),
		"collections", len(manifest.Collections),
		"media", len(manifest.Media),
		"warnings", len(manifest.Warnings))
	return manifest, nil
}

// ----- Pagination -----

// fetchResource walks /posts or /pages with limit=100. Pagination follows
// meta.pagination.pages. include=tags,authors is set when withInclude is
// true so post authoring metadata arrives in one round trip.
func (c *GhostClient) fetchResource(ctx context.Context, resource string, maxPages int, withInclude bool) ([]ghostPost, []string) {
	var all []ghostPost
	var warnings []string
	for page := 1; page <= maxPages; page++ {
		params := url.Values{
			"limit": {fmt.Sprintf("%d", ghostItemsPerPage)},
			"page":  {fmt.Sprintf("%d", page)},
		}
		if withInclude {
			params.Set("include", "tags,authors")
		}
		u := c.endpoint("/"+resource+"/", params)
		body, err := c.getJSON(ctx, u)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("fetch %s page %d: %v", resource, page, err))
			return all, warnings
		}
		var resp ghostPostsResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			warnings = append(warnings, fmt.Sprintf("decode %s page %d: %v", resource, page, err))
			return all, warnings
		}
		var batch []ghostPost
		if resource == "posts" {
			batch = resp.Posts
		} else {
			batch = resp.Pages
		}
		all = append(all, batch...)
		totalPages := resp.Meta.Pagination.Pages
		if page >= totalPages || len(batch) == 0 {
			break
		}
	}
	return all, warnings
}

func (c *GhostClient) fetchTags(ctx context.Context, maxPages int) ([]ghostTag, []string) {
	var all []ghostTag
	var warnings []string
	for page := 1; page <= maxPages; page++ {
		u := c.endpoint("/tags/", url.Values{
			"limit": {fmt.Sprintf("%d", ghostItemsPerPage)},
			"page":  {fmt.Sprintf("%d", page)},
		})
		body, err := c.getJSON(ctx, u)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("fetch tags page %d: %v", page, err))
			return all, warnings
		}
		var resp ghostTagsResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			warnings = append(warnings, fmt.Sprintf("decode tags page %d: %v", page, err))
			return all, warnings
		}
		all = append(all, resp.Tags...)
		if page >= resp.Meta.Pagination.Pages || len(resp.Tags) == 0 {
			break
		}
	}
	return all, warnings
}

// ----- HTTP plumbing -----

// endpoint builds a Ghost Content API URL with the API key + extra params.
// The key MUST go in the query string for the public Content API (the
// Admin API uses JWT Bearer instead, which we deliberately don't support).
func (c *GhostClient) endpoint(path string, extra url.Values) string {
	if extra == nil {
		extra = url.Values{}
	}
	extra.Set("key", c.contentKey)
	return c.baseURL + "/ghost/api/content" + path + "?" + extra.Encode()
}

func (c *GhostClient) getJSON(ctx context.Context, rawURL string) ([]byte, error) {
	resp, err := SafeFetch(ctx, rawURL, c.fetchOpts)
	if err != nil {
		return nil, err
	}
	if resp.Status >= 200 && resp.Status < 300 {
		return resp.Body, nil
	}
	var gerr ghostErrorResponse
	if json.Unmarshal(resp.Body, &gerr) == nil && len(gerr.Errors) > 0 {
		first := gerr.Errors[0]
		return nil, fmt.Errorf("ghost %s: %d %s (%s)", redactGhostKey(rawURL), resp.Status, first.ErrCode, first.Message)
	}
	return nil, fmt.Errorf("ghost %s: status %d", redactGhostKey(rawURL), resp.Status)
}

// redactGhostKey strips the `key=` query param value so error messages
// don't accidentally leak the Content API key into logs.
func redactGhostKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	if q.Get("key") != "" {
		q.Set("key", "REDACTED")
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// ----- Mapping -----

func ghostPostToMigrationPage(p ghostPost, sourceDomain string) MigrationPage {
	mp := MigrationPage{
		SourceURL:       p.URL,
		SourcePath:      pathOf(p.URL),
		Title:           coalesceString(p.MetaTitle, p.OGTitle, p.Title),
		MetaDescription: coalesceString(p.MetaDescription, p.OGDescription, p.CustomExcerpt, stripHTMLTags(p.Excerpt)),
		OGImageURL:      coalesceString(p.OGImage, p.FeatureImage),
		HTML:            p.HTML,
	}
	if mp.SourcePath == "" {
		mp.SourcePath = "/" + p.Slug
	}
	if t := parseGhostTime(p.PublishedAt); !t.IsZero() {
		mp.PublishedAt = t
	}
	imgs, links := extractAssetsFromHTML(p.HTML, p.URL)
	mp.ImageURLs = imgs
	mp.InternalLinks = links
	return mp
}

func ghostPostToCollectionItem(p ghostPost) MigrationCollectionItem {
	tagSlugs := make([]string, 0, len(p.Tags))
	for _, t := range p.Tags {
		tagSlugs = append(tagSlugs, t.Slug)
	}
	authorName := ""
	if len(p.Authors) > 0 {
		authorName = p.Authors[0].Name
	}
	item := MigrationCollectionItem{
		SourceURL: p.URL,
		Slug:      p.Slug,
		Title:     p.Title,
		Data: map[string]any{
			"title":             p.Title,
			"excerpt":           coalesceString(p.CustomExcerpt, stripHTMLTags(p.Excerpt)),
			"content":           p.HTML,
			"feature_image":     p.FeatureImage,
			"feature_image_alt": p.FeatureImageAlt,
			"author":            authorName,
			"tags":              tagSlugs,
		},
	}
	if t := parseGhostTime(p.PublishedAt); !t.IsZero() {
		item.PublishedAt = t
	}
	return item
}

func parseGhostTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339, time.RFC3339Nano, "2006-01-02T15:04:05.000Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
