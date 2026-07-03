package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// WebflowClient consumes the Webflow CMS API v2 (api.webflow.com/v2). Webflow
// is unique among the importers we ship: its CMS schema is already typed
// (PlainText, RichText, Image, Reference, etc.) so the schema -> atomicsite
// FieldDef translation is one-to-one. Sitemap-crawl can never recover that
// fidelity from the rendered HTML.
//
// Auth: site-scoped tokens are generated under "Apps & integrations" in the
// Webflow project. The crawler needs `cms:read` and `sites:read`. Tokens
// expire after 1 year by default; the operator regenerates and re-runs.
type WebflowClient struct {
	siteID    string
	authToken string
	baseURL   string
	fetchOpts FetchOptions
}

// WebflowOptions wraps the per-site config the operator supplies.
type WebflowOptions struct {
	// SiteID is the Webflow site ID (24-char hex, found under Site Settings
	// -> Integrations -> API Access). Required.
	SiteID string

	// AuthToken is the Bearer token. Required.
	AuthToken string

	// BaseURL overrides api.webflow.com. Tests use httptest.Server here.
	BaseURL string

	// FetchOpts overrides SSRF / size / timeout defaults.
	FetchOpts FetchOptions

	// MaxItemsPerCollection caps the item walk per collection (default 5000).
	// Webflow's hard limit is 10k items per collection; our default leaves
	// headroom and stops early if a collection is malformed.
	MaxItemsPerCollection int
}

const (
	webflowDefaultBaseURL               = "https://api.webflow.com"
	webflowDefaultMaxItemsPerCollection = 5000
	webflowItemsPerPage                 = 100 // Webflow CMS API v2 max
)

// ----- Webflow REST shapes -----

type webflowSite struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	ShortName   string `json:"shortName"`
	Domains     []struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	} `json:"customDomains"`
}

type webflowCollection struct {
	ID           string         `json:"id"`
	DisplayName  string         `json:"displayName"`
	SingularName string         `json:"singularName"`
	Slug         string         `json:"slug"`
	Fields       []webflowField `json:"fields"`
	CreatedOn    string         `json:"createdOn"`
	LastUpdated  string         `json:"lastUpdated"`
}

type webflowField struct {
	ID          string                 `json:"id"`
	DisplayName string                 `json:"displayName"`
	Slug        string                 `json:"slug"`
	Type        string                 `json:"type"`
	IsRequired  bool                   `json:"isRequired"`
	IsEditable  bool                   `json:"isEditable"`
	HelpText    string                 `json:"helpText"`
	Validations map[string]interface{} `json:"validations"`
}

type webflowItemResponse struct {
	Items      []webflowItem     `json:"items"`
	Pagination webflowPagination `json:"pagination"`
}

type webflowPagination struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
	Total  int `json:"total"`
}

type webflowItem struct {
	ID            string                 `json:"id"`
	CMSLocaleID   string                 `json:"cmsLocaleId"`
	IsArchived    bool                   `json:"isArchived"`
	IsDraft       bool                   `json:"isDraft"`
	LastUpdated   string                 `json:"lastUpdated"`
	CreatedOn     string                 `json:"createdOn"`
	LastPublished string                 `json:"lastPublished"`
	FieldData     map[string]interface{} `json:"fieldData"`
}

type webflowPagesResponse struct {
	Pages      []webflowPage     `json:"pages"`
	Pagination webflowPagination `json:"pagination"`
}

type webflowPage struct {
	ID                   string `json:"id"`
	Title                string `json:"title"`
	Slug                 string `json:"slug"`
	URL                  string `json:"url"` // not always populated; the static rendered page URL
	SeoTitle             string `json:"seoTitle"`
	SeoDescription       string `json:"seoDescription"`
	OpenGraphTitle       string `json:"openGraphTitle"`
	OpenGraphDescription string `json:"openGraphDescription"`
	OpenGraphImage       string `json:"openGraphImage"`
	CreatedOn            string `json:"createdOn"`
	LastUpdated          string `json:"lastUpdated"`
	IsDraft              bool   `json:"isDraft"`
	IsArchived           bool   `json:"isArchived"`
}

type webflowAPIError struct {
	Code    string                   `json:"code"`
	Message string                   `json:"message"`
	Details []map[string]interface{} `json:"details"`
}

// ----- Public surface -----

// NewWebflowClient constructs the client. Both SiteID and AuthToken are
// required - returns an error if either is empty so the caller doesn't
// silently make unauthenticated requests.
func NewWebflowClient(opts WebflowOptions) (*WebflowClient, error) {
	if strings.TrimSpace(opts.SiteID) == "" {
		return nil, fmt.Errorf("webflow site id is required")
	}
	if strings.TrimSpace(opts.AuthToken) == "" {
		return nil, fmt.Errorf("webflow auth token is required")
	}
	base := opts.BaseURL
	if base == "" {
		base = webflowDefaultBaseURL
	}
	base = strings.TrimRight(base, "/")
	c := &WebflowClient{
		siteID:    opts.SiteID,
		authToken: opts.AuthToken,
		baseURL:   base,
		fetchOpts: opts.FetchOpts,
	}
	c.fetchOpts.AcceptHeader = "application/json"
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+opts.AuthToken)
	c.fetchOpts.ExtraHeaders = mergeHeaders(c.fetchOpts.ExtraHeaders, hdr)
	return c, nil
}

// Discover hits /v2/sites/{site_id} to confirm the token is valid and the
// site exists. Returns the site display name + custom domain (when set) so
// the migration UI can show "About to import 'Acme Corp' from acme.example".
func (c *WebflowClient) Discover(ctx context.Context) (*webflowSite, error) {
	body, err := c.getJSON(ctx, fmt.Sprintf("%s/v2/sites/%s", c.baseURL, c.siteID))
	if err != nil {
		return nil, fmt.Errorf("discover webflow site: %w", err)
	}
	var site webflowSite
	if err := json.Unmarshal(body, &site); err != nil {
		return nil, fmt.Errorf("decode webflow site: %w (body starts %q)", err, peek(body, 80))
	}
	if site.ID == "" {
		return &site, fmt.Errorf("webflow site response missing id (got %s)", peek(body, 200))
	}
	return &site, nil
}

// Crawl pulls Webflow pages, every CMS collection's schema + published items,
// and references all images / file URLs in collection items so the porter
// (Layer 3d) can re-upload them. Drafts and archived items are filtered out.
func (c *WebflowClient) Crawl(ctx context.Context, opts WebflowOptions) (*MigrationManifest, error) {
	if opts.MaxItemsPerCollection == 0 {
		opts.MaxItemsPerCollection = webflowDefaultMaxItemsPerCollection
	}

	manifest := &MigrationManifest{
		SourceURL:  c.baseURL + "/v2/sites/" + c.siteID,
		SourceType: SourceWebflow,
		FetchedAt:  time.Now().UTC(),
	}

	site, err := c.Discover(ctx)
	if err != nil {
		return nil, err
	}
	manifest.SourceDomain = webflowSourceDomain(site)

	// ---- Pages
	wfPages, pagesWarn := c.fetchPages(ctx)
	manifest.Warnings = append(manifest.Warnings, pagesWarn...)
	for _, p := range wfPages {
		if p.IsDraft || p.IsArchived {
			continue
		}
		manifest.Pages = append(manifest.Pages, webflowToMigrationPage(p, manifest.SourceDomain))
	}
	manifest.Stats.URLsFetched += len(manifest.Pages)

	// ---- Collections (schema + items)
	collections, collWarn := c.fetchCollections(ctx)
	manifest.Warnings = append(manifest.Warnings, collWarn...)

	mediaSet := map[string]MigrationMediaRef{}
	for _, p := range manifest.Pages {
		registerMedia(mediaSet, p.OGImageURL, p.SourcePath, "")
	}
	for _, coll := range collections {
		mc := MigrationCollection{
			Slug:   coll.Slug,
			Name:   coll.DisplayName,
			Schema: webflowSchemaToFields(coll.Fields),
		}
		items, itemWarn := c.fetchCollectionItems(ctx, coll.ID, opts.MaxItemsPerCollection)
		manifest.Warnings = append(manifest.Warnings, itemWarn...)
		for _, it := range items {
			if it.IsArchived || it.IsDraft {
				continue
			}
			migItem := webflowItemToMigration(it, coll)
			mc.Items = append(mc.Items, migItem)
			for _, src := range webflowExtractAssetURLs(it.FieldData) {
				registerMedia(mediaSet, src, "", migItem.Slug)
			}
		}
		if len(mc.Items) > 0 || len(mc.Schema) > 0 {
			manifest.Collections = append(manifest.Collections, mc)
		}
	}

	for _, m := range mediaSet {
		manifest.Media = append(manifest.Media, m)
	}

	manifest.Stats.PagesFound = len(manifest.Pages)
	manifest.Stats.CollectionsFound = len(manifest.Collections)
	manifest.Stats.MediaFound = len(manifest.Media)
	manifest.Stats.URLsRequested = manifest.Stats.URLsFetched

	slog.Info("webflow crawl complete",
		"site", site.DisplayName,
		"pages", len(manifest.Pages),
		"collections", len(manifest.Collections),
		"media", len(manifest.Media),
		"warnings", len(manifest.Warnings))

	return manifest, nil
}

// ----- Pagination helpers -----

func (c *WebflowClient) fetchPages(ctx context.Context) ([]webflowPage, []string) {
	var all []webflowPage
	var warnings []string
	offset := 0
	for {
		u := fmt.Sprintf("%s/v2/sites/%s/pages?limit=%d&offset=%d",
			c.baseURL, c.siteID, webflowItemsPerPage, offset)
		body, err := c.getJSON(ctx, u)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("fetch pages offset=%d: %v", offset, err))
			return all, warnings
		}
		var resp webflowPagesResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			warnings = append(warnings, fmt.Sprintf("decode pages offset=%d: %v", offset, err))
			return all, warnings
		}
		all = append(all, resp.Pages...)
		// Webflow pagination: stop when offset+returned >= total OR returned 0.
		if len(resp.Pages) == 0 {
			break
		}
		offset += len(resp.Pages)
		if resp.Pagination.Total > 0 && offset >= resp.Pagination.Total {
			break
		}
		if offset > 50_000 { // hard safety cap
			warnings = append(warnings, "pages walk hit safety cap (>50k); truncating")
			break
		}
	}
	return all, warnings
}

func (c *WebflowClient) fetchCollections(ctx context.Context) ([]webflowCollection, []string) {
	var warnings []string
	body, err := c.getJSON(ctx, fmt.Sprintf("%s/v2/sites/%s/collections", c.baseURL, c.siteID))
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("list collections: %v", err))
		return nil, warnings
	}
	var resp struct {
		Collections []webflowCollection `json:"collections"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		warnings = append(warnings, fmt.Sprintf("decode collections: %v", err))
		return nil, warnings
	}

	// The list response has the IDs but only summary fields; fetch each
	// collection by ID for the full schema.
	full := make([]webflowCollection, 0, len(resp.Collections))
	for _, summary := range resp.Collections {
		body, err := c.getJSON(ctx, fmt.Sprintf("%s/v2/collections/%s", c.baseURL, summary.ID))
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("fetch collection %s: %v", summary.ID, err))
			continue
		}
		var coll webflowCollection
		if err := json.Unmarshal(body, &coll); err != nil {
			warnings = append(warnings, fmt.Sprintf("decode collection %s: %v", summary.ID, err))
			continue
		}
		full = append(full, coll)
	}
	return full, warnings
}

func (c *WebflowClient) fetchCollectionItems(ctx context.Context, collectionID string, max int) ([]webflowItem, []string) {
	var all []webflowItem
	var warnings []string
	offset := 0
	for {
		u := fmt.Sprintf("%s/v2/collections/%s/items?limit=%d&offset=%d",
			c.baseURL, collectionID, webflowItemsPerPage, offset)
		body, err := c.getJSON(ctx, u)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("items %s offset=%d: %v", collectionID, offset, err))
			return all, warnings
		}
		var resp webflowItemResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			warnings = append(warnings, fmt.Sprintf("decode items %s offset=%d: %v", collectionID, offset, err))
			return all, warnings
		}
		all = append(all, resp.Items...)
		if len(resp.Items) == 0 {
			break
		}
		offset += len(resp.Items)
		if resp.Pagination.Total > 0 && offset >= resp.Pagination.Total {
			break
		}
		if len(all) >= max {
			break
		}
	}
	if len(all) > max {
		all = all[:max]
	}
	return all, warnings
}

// ----- HTTP plumbing -----

func (c *WebflowClient) getJSON(ctx context.Context, rawURL string) ([]byte, error) {
	resp, err := SafeFetch(ctx, rawURL, c.fetchOpts)
	if err != nil {
		return nil, err
	}
	if resp.Status >= 200 && resp.Status < 300 {
		return resp.Body, nil
	}
	// Try to surface Webflow's structured error message.
	var werr webflowAPIError
	if json.Unmarshal(resp.Body, &werr) == nil && werr.Code != "" {
		return nil, fmt.Errorf("webflow %s: %d %s (%s)", rawURL, resp.Status, werr.Code, werr.Message)
	}
	return nil, fmt.Errorf("webflow %s: status %d", rawURL, resp.Status)
}

// ----- Mapping -----

// webflowSourceDomain prefers the first custom domain when set, falls back
// to the shortName (which webflow.io subdomains map to).
func webflowSourceDomain(site *webflowSite) string {
	if site == nil {
		return ""
	}
	if len(site.Domains) > 0 {
		raw := site.Domains[0].URL
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			return u.Host
		}
		// Some payloads return host-only strings without a scheme.
		if !strings.Contains(raw, "://") && raw != "" {
			return raw
		}
	}
	if site.ShortName != "" {
		return site.ShortName + ".webflow.io"
	}
	return ""
}

// webflowSchemaToFields maps Webflow CMS field types to atomicsite field
// type names. The atomicsite type strings are the same ones FieldDef.Type
// uses in handlers/collections.go - kept aligned so the porter can write
// the Webflow schema verbatim into a Collection.
func webflowSchemaToFields(fields []webflowField) []MigrationCollectionField {
	out := make([]MigrationCollectionField, 0, len(fields))
	for _, f := range fields {
		t := webflowFieldType(f.Type)
		if t == "" {
			continue
		}
		out = append(out, MigrationCollectionField{
			Name:     f.Slug,
			Type:     t,
			Label:    f.DisplayName,
			Required: f.IsRequired,
		})
	}
	return out
}

func webflowFieldType(wf string) string {
	switch wf {
	case "PlainText":
		return "text"
	case "RichText":
		return "richtext"
	case "Image":
		return "image"
	case "MultiImage":
		return "gallery"
	case "Reference":
		return "reference"
	case "MultiReference":
		return "multiselect" // atomicsite has no multi-reference yet; stored as slug list
	case "DateTime":
		return "datetime"
	case "Date":
		return "date"
	case "Number":
		return "number"
	case "Switch":
		return "boolean"
	case "Color":
		return "color"
	case "Email":
		return "email"
	case "Phone":
		return "text"
	case "Link":
		return "url"
	case "Option":
		return "select"
	case "Set":
		return "multiselect"
	case "File":
		return "url"
	case "VideoLink":
		return "url"
	default:
		return ""
	}
}

func webflowItemToMigration(it webflowItem, coll webflowCollection) MigrationCollectionItem {
	slug, _ := it.FieldData["slug"].(string)
	name, _ := it.FieldData["name"].(string)
	out := MigrationCollectionItem{
		Slug:  slug,
		Title: name,
		Data:  cleanWebflowFieldData(it.FieldData, coll.Fields),
	}
	if t := parseWebflowTime(it.LastPublished); !t.IsZero() {
		out.PublishedAt = t
	} else if t := parseWebflowTime(it.CreatedOn); !t.IsZero() {
		out.PublishedAt = t
	}
	return out
}

// cleanWebflowFieldData maps the raw fieldData blob into the same shape
// atomicsite stores. Image fields come back as `{url, alt, ...}` maps; we
// flatten to the `url` for the porter to re-upload. Reference fields come
// back as item IDs which we'd resolve in 3d once the referenced collections
// are imported.
func cleanWebflowFieldData(raw map[string]interface{}, schema []webflowField) map[string]any {
	typeBySlug := map[string]string{}
	for _, f := range schema {
		typeBySlug[f.Slug] = f.Type
	}
	out := make(map[string]any, len(raw))
	for k, v := range raw {
		switch typeBySlug[k] {
		case "Image":
			out[k] = webflowImageURL(v)
		case "MultiImage":
			out[k] = webflowMultiImageURLs(v)
		default:
			out[k] = v
		}
	}
	return out
}

func webflowImageURL(v interface{}) string {
	if v == nil {
		return ""
	}
	if m, ok := v.(map[string]interface{}); ok {
		if u, ok := m["url"].(string); ok {
			return u
		}
	}
	return ""
}

func webflowMultiImageURLs(v interface{}) []string {
	out := []string{}
	if list, ok := v.([]interface{}); ok {
		for _, el := range list {
			if u := webflowImageURL(el); u != "" {
				out = append(out, u)
			}
		}
	}
	return out
}

// webflowExtractAssetURLs walks a fieldData map and returns every image / file
// URL we should re-upload. Handles nested {url} maps, []map shapes, and
// strings that look like absolute URLs in known asset-bearing field slugs.
func webflowExtractAssetURLs(raw map[string]interface{}) []string {
	out := []string{}
	for _, v := range raw {
		switch tv := v.(type) {
		case map[string]interface{}:
			if u, ok := tv["url"].(string); ok && strings.HasPrefix(u, "http") {
				out = append(out, u)
			}
		case []interface{}:
			for _, el := range tv {
				if m, ok := el.(map[string]interface{}); ok {
					if u, ok := m["url"].(string); ok && strings.HasPrefix(u, "http") {
						out = append(out, u)
					}
				}
			}
		}
	}
	return out
}

func webflowToMigrationPage(p webflowPage, sourceDomain string) MigrationPage {
	mp := MigrationPage{
		Title:           coalesceString(p.SeoTitle, p.OpenGraphTitle, p.Title),
		MetaDescription: coalesceString(p.SeoDescription, p.OpenGraphDescription),
		OGImageURL:      p.OpenGraphImage,
	}
	if p.Slug != "" {
		mp.SourcePath = "/" + strings.TrimPrefix(p.Slug, "/")
	} else {
		mp.SourcePath = "/" + p.ID
	}
	if sourceDomain != "" {
		mp.SourceURL = "https://" + sourceDomain + mp.SourcePath
	}
	if t := parseWebflowTime(p.LastUpdated); !t.IsZero() {
		mp.PublishedAt = t
	} else if t := parseWebflowTime(p.CreatedOn); !t.IsZero() {
		mp.PublishedAt = t
	}
	return mp
}

func coalesceString(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func parseWebflowTime(s string) time.Time {
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
