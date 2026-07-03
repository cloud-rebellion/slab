// Package migration parses external CMS sources (sitemap-crawl, WordPress,
// Webflow, Ghost, ...) into a normalised manifest the agent can review and
// then commit into atomicsite pages, collections, media and redirects.
//
// Layer 3a (this file + sitemap.go + safefetch.go) ships only the manifest
// types and the universal sitemap crawler. Per-platform native importers
// (WordPress REST, Webflow API, Ghost) land in 3b/3c. URL planning + content
// porting + media re-upload land in 3d. The redirects table emission
// (Layer 1) and CRUD/verify API (Layer 2) are already in place and consume
// MigrationLink + MigrationPage to wire everything end-to-end.
package migration

import "time"

// SourceType is the discriminator stored on the migrations row so a later
// re-parse knows which importer to invoke.
type SourceType string

const (
	SourceSitemap   SourceType = "sitemap"
	SourceWordPress SourceType = "wordpress"
	SourceWebflow   SourceType = "webflow"
	SourceGhost     SourceType = "ghost"
)

// MigrationManifest is the normalised output of every importer. Stored as
// JSON in migrations.manifest_json. The agent reads this, the operator
// optionally edits, then migration_apply (Layer 3d) walks Pages/Collections/
// Media to commit and emits the corresponding redirect rows.
type MigrationManifest struct {
	SourceURL    string                `json:"source_url"`
	SourceType   SourceType            `json:"source_type"`
	SourceDomain string                `json:"source_domain"`
	FetchedAt    time.Time             `json:"fetched_at"`
	Pages        []MigrationPage       `json:"pages"`
	Collections  []MigrationCollection `json:"collections,omitempty"`
	Media        []MigrationMediaRef   `json:"media,omitempty"`
	Links        []MigrationLink       `json:"links,omitempty"`
	Stats        MigrationStats        `json:"stats"`
	Warnings     []string              `json:"warnings,omitempty"`
}

// MigrationPage is one extracted page from the source. Slug is the source-side
// path (`/about-us`); the URL planner in Layer 3d turns this into the new
// atomicsite slug + decides whether a redirect row is needed.
type MigrationPage struct {
	SourceURL       string    `json:"source_url"`
	SourcePath      string    `json:"source_path"`
	Title           string    `json:"title"`
	MetaDescription string    `json:"meta_description,omitempty"`
	CanonicalURL    string    `json:"canonical_url,omitempty"`
	OGImageURL      string    `json:"og_image_url,omitempty"`
	H1              string    `json:"h1,omitempty"`
	HTML            string    `json:"html,omitempty"`       // raw <body> innerHTML or full doc snippet
	JSONLD          []string  `json:"jsonld,omitempty"`     // raw JSON-LD blocks (preserve as-is)
	ImageURLs       []string  `json:"image_urls,omitempty"` // src URLs found inside HTML
	InternalLinks   []string  `json:"internal_links,omitempty"`
	PublishedAt     time.Time `json:"published_at,omitempty"` // best-effort, only set if found in metadata
	Lang            string    `json:"lang,omitempty"`
	NoIndex         bool      `json:"noindex,omitempty"`
}

// MigrationCollection groups items that share a schema (blog posts, products,
// case studies). Native importers (WP/Webflow/Ghost) populate this; the
// universal sitemap crawler does not - it cannot infer collection structure.
type MigrationCollection struct {
	Slug   string                     `json:"slug"`
	Name   string                     `json:"name"`
	Schema []MigrationCollectionField `json:"schema"`
	Items  []MigrationCollectionItem  `json:"items"`
}

// MigrationCollectionField mirrors handlers.FieldDef but lives here so the
// migration package has no dependency on internal/handlers.
type MigrationCollectionField struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Label    string `json:"label,omitempty"`
	Required bool   `json:"required,omitempty"`
}

// MigrationCollectionItem is one row in a collection. Data is field-name to
// value, matching how collection_items.data_json works in atomicsite.
type MigrationCollectionItem struct {
	SourceURL   string         `json:"source_url,omitempty"`
	Slug        string         `json:"slug"`
	Title       string         `json:"title"`
	Data        map[string]any `json:"data"`
	PublishedAt time.Time      `json:"published_at,omitempty"`
	Locale      string         `json:"locale,omitempty"`
}

// MigrationMediaRef points at a downloadable asset (image, video, doc) the
// porter (Layer 3d) re-uploads through the existing media handler so the
// new site is fully self-hosted (no hotlinking, no broken links when the
// source domain disappears).
type MigrationMediaRef struct {
	SourceURL string `json:"source_url"`
	Alt       string `json:"alt,omitempty"`
	Caption   string `json:"caption,omitempty"`
	OnPage    string `json:"on_page,omitempty"` // SourcePath of the page that referenced it
}

// MigrationLink records an internal-link edge so the porter can rewrite
// `<a href>` and `<img src>` after Pages have been imported and the new
// atomicsite slugs are known. From and To are SourcePath form
// (`/blog/post-slug`).
type MigrationLink struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// MigrationStats is what the migration UI surfaces to the operator before
// they hit "apply".
type MigrationStats struct {
	PagesFound       int `json:"pages_found"`
	CollectionsFound int `json:"collections_found,omitempty"`
	MediaFound       int `json:"media_found"`
	LinksFound       int `json:"links_found"`
	URLsRequested    int `json:"urls_requested"`
	URLsFetched      int `json:"urls_fetched"`
	URLsFailed       int `json:"urls_failed"`
}
