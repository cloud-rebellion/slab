package migration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bright-interaction/slab/internal/store"
)

// Porter walks a URLPlan + MigrationManifest and writes the corresponding
// rows into the atomicsite store: collections, items, pages, redirects,
// and (via MediaUploader) re-uploaded media. The porter does not touch
// blocks - imported HTML stays in collection-item data_json or in the
// page's settings_json under `imported_html`. The agent then has full
// control over how to lay out blocks from that source.
//
// Failure model: every section commits in order; a partial failure rolls
// back the rows the porter inserted in this Apply call (best-effort, same
// pattern as collections.BulkImport). Returns ApplyResult so the caller
// can audit-log per-resource counts.
type Porter struct {
	queries *store.Queries
	media   MediaUploader
}

// MediaUploader is the interface the porter uses to re-upload images and
// other binary assets so the new atomicsite stays self-hosted (no cross-
// origin hotlinking, no broken images when the source domain expires).
//
// Real implementation: a thin wrapper around the existing handlers/media.go
// fetchWithSSRFGuard + storage Upload + CreateMedia path. Tests inject a
// fake that returns deterministic atomicsite URLs without hitting disk.
type MediaUploader interface {
	UploadFromURL(ctx context.Context, siteID, sourceURL, alt, caption string) (mediaID, atomicURL string, err error)
}

// NewPorter wires the porter against an open store and a media uploader.
// Both are required.
func NewPorter(q *store.Queries, m MediaUploader) *Porter {
	return &Porter{queries: q, media: m}
}

// ApplyResult is what the migration handler returns to the operator after
// a successful Apply. Counts are post-rollback so partial-failure Apply
// calls don't lie about progress.
//
// Sprint 4 (2026-05-06): re-import upsert mode adds PagesUpdated /
// ItemsUpdated / RedirectsUpdated so the response distinguishes a fresh
// import from a refresh. In create-mode (Upsert=false) the *Updated
// counts stay zero.
type ApplyResult struct {
	CollectionsCreated int      `json:"collections_created"`
	ItemsCreated       int      `json:"items_created"`
	ItemsUpdated       int      `json:"items_updated,omitempty"`
	PagesCreated       int      `json:"pages_created"`
	PagesUpdated       int      `json:"pages_updated,omitempty"`
	RedirectsCreated   int      `json:"redirects_created"`
	RedirectsUpdated   int      `json:"redirects_updated,omitempty"`
	MediaUploaded      int      `json:"media_uploaded"`
	MediaFailed        int      `json:"media_failed"`
	Warnings           []string `json:"warnings,omitempty"`
}

// ApplyOptions tunes a porter run. Sprint 4 (2026-05-06) introduced
// Upsert mode for the annual-content-refresh use case: a re-import on
// the same site overwrites existing rows by their natural key instead
// of erroring on the unique-index conflict.
type ApplyOptions struct {
	// Upsert toggles INSERT vs INSERT ... ON CONFLICT DO UPDATE for
	// pages, redirects, and collection items. Default false preserves
	// the create-only behaviour every previous Sprint depended on.
	Upsert bool
}

// Apply commits the plan + manifest. Order is fixed:
//  1. Media re-upload (so collection items can reference new media IDs)
//  2. Collections + items
//  3. Pages (with og_image_id resolved via the media map when present)
//  4. Redirects (last, so the new pages exist when nginx flips)
//
// On any fatal step the porter walks the IDs it created in reverse and
// deletes them. Media failures are warnings (the source URL stays in
// data_json) so a single broken image doesn't block the migration.
//
// Wraps ApplyWithOptions for backwards compatibility with callers that
// don't need upsert mode.
func (p *Porter) Apply(ctx context.Context, siteID string, manifest *MigrationManifest, plan URLPlan) (*ApplyResult, error) {
	return p.ApplyWithOptions(ctx, siteID, manifest, plan, ApplyOptions{})
}

// ApplyWithOptions is Apply plus knobs. Sprint 4 (2026-05-06) added
// ApplyOptions.Upsert for re-imports: existing pages / redirects /
// collection items are UPDATED in place rather than failing on the
// unique-index conflict the single Create path raises today.
func (p *Porter) ApplyWithOptions(ctx context.Context, siteID string, manifest *MigrationManifest, plan URLPlan, opts ApplyOptions) (*ApplyResult, error) {
	if manifest == nil {
		return nil, fmt.Errorf("manifest is required")
	}
	if len(plan.Conflicts) > 0 {
		return nil, fmt.Errorf("plan has %d unresolved conflict(s); resolve before applying", len(plan.Conflicts))
	}

	res := &ApplyResult{}
	createdPages := []string{}
	createdItems := []string{}
	createdColls := []string{}
	createdRedirects := []string{}

	// 1. Media re-upload first.
	mediaMap := map[string]string{} // sourceURL -> atomicsite URL
	mediaIDByURL := map[string]string{}
	for _, m := range manifest.Media {
		if m.SourceURL == "" {
			continue
		}
		id, atomicURL, err := p.media.UploadFromURL(ctx, siteID, m.SourceURL, m.Alt, m.Caption)
		if err != nil {
			res.MediaFailed++
			res.Warnings = append(res.Warnings, fmt.Sprintf("media %s: %v", m.SourceURL, err))
			continue
		}
		mediaMap[m.SourceURL] = atomicURL
		mediaIDByURL[m.SourceURL] = id
		res.MediaUploaded++
	}

	// 2. Collections + items.
	for _, coll := range manifest.Collections {
		schemaJSON, _ := json.Marshal(coll.Schema)
		settingsJSON := []byte(`{}`)
		collID := newPorterID()
		if err := p.queries.CreateCollection(ctx, store.CreateCollectionParams{
			ID: collID, SiteID: siteID, Name: coll.Name, Slug: coll.Slug,
			SchemaJson: string(schemaJSON), SettingsJson: string(settingsJSON),
			SortOrder: 0,
		}); err != nil {
			p.rollback(ctx, createdRedirects, createdPages, createdItems, createdColls)
			return nil, fmt.Errorf("create collection %q: %w", coll.Slug, err)
		}
		createdColls = append(createdColls, collID)
		res.CollectionsCreated++

		for i, item := range coll.Items {
			data := rewriteMediaInItemData(item.Data, mediaMap)
			data = rewriteLinksInItemData(data, plan)
			dj, _ := json.Marshal(data)
			itemID := newPorterID()
			pubAt := ""
			if !item.PublishedAt.IsZero() {
				pubAt = item.PublishedAt.UTC().Format("2006-01-02 15:04:05")
			}
			if opts.Upsert {
				row, err := p.queries.UpsertItem(ctx, store.UpsertItemParams{
					ID: itemID, CollectionID: collID, SiteID: siteID,
					Slug: item.Slug, Title: item.Title, DataJson: string(dj),
					Locale: item.Locale, Status: "published",
					PublishedAt: pubAt, SortOrder: int64(i),
				})
				if err != nil {
					p.rollback(ctx, createdRedirects, createdPages, createdItems, createdColls)
					return nil, fmt.Errorf("upsert item %q in %q: %w", item.Slug, coll.Slug, err)
				}
				if row.ID == itemID {
					createdItems = append(createdItems, row.ID)
					res.ItemsCreated++
				} else {
					res.ItemsUpdated++
				}
			} else {
				if err := p.queries.CreateItem(ctx, store.CreateItemParams{
					ID: itemID, CollectionID: collID, SiteID: siteID,
					Slug: item.Slug, Title: item.Title, DataJson: string(dj),
					Locale: item.Locale, Status: "published",
					PublishedAt: pubAt, SortOrder: int64(i),
				}); err != nil {
					p.rollback(ctx, createdRedirects, createdPages, createdItems, createdColls)
					return nil, fmt.Errorf("create item %q in %q: %w", item.Slug, coll.Slug, err)
				}
				createdItems = append(createdItems, itemID)
				res.ItemsCreated++
			}
		}
	}

	// 3. Pages.
	pagesBySource := map[string]MigrationPage{}
	for _, mp := range manifest.Pages {
		pagesBySource[mp.SourcePath] = mp
	}
	for sortIdx, pp := range plan.Pages {
		if pp.Skipped {
			continue
		}
		mp := pagesBySource[pp.SourcePath]
		pageID := newPorterID()
		// Strip leading slash for the slug column (atomicsite stores slugs
		// without the leading /, the builder prepends it during render).
		slug := strings.TrimPrefix(pp.NewSlug, "/")
		if slug == "" {
			slug = "index"
		}
		ogImageID := ""
		if mp.OGImageURL != "" {
			ogImageID = mediaIDByURL[mp.OGImageURL]
		}
		noIndex := int64(0)
		if mp.NoIndex {
			noIndex = 1
		}

		if opts.Upsert {
			row, err := p.queries.UpsertPage(ctx, store.UpsertPageParams{
				ID:              pageID,
				SiteID:          siteID,
				Title:           pp.Title,
				Slug:            slug,
				Status:          "published",
				MetaTitle:       truncateMeta(pp.Title, 70),
				MetaDescription: truncateMeta(mp.MetaDescription, 200),
				OgImageID:       ogImageID,
				Layout:          "default",
				SortOrder:       int64(sortIdx),
				ShowInNav:       0,
				NoIndex:         noIndex,
				CanonicalUrl:    mp.CanonicalURL,
			})
			if err != nil {
				p.rollback(ctx, createdRedirects, createdPages, createdItems, createdColls)
				return nil, fmt.Errorf("upsert page %q: %w", pp.NewSlug, err)
			}
			if row.ID == pageID {
				createdPages = append(createdPages, row.ID)
				res.PagesCreated++
			} else {
				res.PagesUpdated++
			}
			continue
		}

		if err := p.queries.CreatePage(ctx, store.CreatePageParams{
			ID: pageID, SiteID: siteID, Title: pp.Title, Slug: slug,
			Layout: "default", SortOrder: int64(sortIdx), ShowInNav: 0,
		}); err != nil {
			p.rollback(ctx, createdRedirects, createdPages, createdItems, createdColls)
			return nil, fmt.Errorf("create page %q: %w", pp.NewSlug, err)
		}
		createdPages = append(createdPages, pageID)
		res.PagesCreated++

		// Apply meta + canonical via UpdatePage. This write sets
		// status=published: swallowing its error left imported pages
		// stranded in draft with no meta while apply reported success,
		// so it rolls back like every sibling write.
		if err := p.queries.UpdatePage(ctx, store.UpdatePageParams{
			ID:               pageID,
			Title:            pp.Title,
			Slug:             slug,
			Status:           "published",
			MetaTitle:        truncateMeta(pp.Title, 70),
			MetaDescription:  truncateMeta(mp.MetaDescription, 200),
			OgImageID:        ogImageID,
			Layout:           "default",
			SortOrder:        int64(sortIdx),
			ShowInNav:        0,
			NavLabel:         "",
			NoIndex:          noIndex,
			CanonicalUrl:     mp.CanonicalURL,
			HideGlobalBlocks: 0,
		}); err != nil {
			p.rollback(ctx, createdRedirects, createdPages, createdItems, createdColls)
			return nil, fmt.Errorf("publish page %q: %w", pp.NewSlug, err)
		}
	}

	// 4. Redirects.
	for _, r := range plan.Redirects {
		// Skip if the resulting redirect would be a no-op.
		if r.FromPath == r.ToPath {
			continue
		}
		// Page-slug collision: if the from_path is also being created as
		// a live page, the redirect would be dead (page wins). Skip with
		// a warning.
		fromSlug := strings.TrimPrefix(r.FromPath, "/")
		liveAtFrom := false
		for _, pp := range plan.Pages {
			if !pp.Skipped && strings.TrimPrefix(pp.NewSlug, "/") == fromSlug {
				liveAtFrom = true
				break
			}
		}
		if liveAtFrom {
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("redirect %s -> %s skipped: a page is being created at the same path",
					r.FromPath, r.ToPath))
			continue
		}
		rid := newPorterID()
		statusCode := int64(r.StatusCode)
		if statusCode == 0 {
			statusCode = 301
		}
		if opts.Upsert {
			row, err := p.queries.UpsertRedirect(ctx, store.UpsertRedirectParams{
				ID: rid, SiteID: siteID, FromPath: r.FromPath, ToPath: r.ToPath,
				StatusCode: statusCode, IsAuto: 1,
			})
			if err != nil {
				res.Warnings = append(res.Warnings,
					fmt.Sprintf("upsert redirect %s -> %s: %v", r.FromPath, r.ToPath, err))
				continue
			}
			// CreatedAt is the only timestamp column on redirects, but
			// the upsert UPDATE clause leaves created_at alone. There's
			// no updated_at column to compare; track via a presence
			// pre-check instead. Cheap: at most one extra SELECT per
			// redirect, only on the upsert path.
			if row.ID == rid {
				createdRedirects = append(createdRedirects, row.ID)
				res.RedirectsCreated++
			} else {
				res.RedirectsUpdated++
			}
			continue
		}
		if err := p.queries.CreateRedirect(ctx, store.CreateRedirectParams{
			ID: rid, SiteID: siteID, FromPath: r.FromPath, ToPath: r.ToPath,
			StatusCode: statusCode, IsAuto: 1,
		}); err != nil {
			// Don't fail the whole import on a single redirect collision -
			// the unique(site_id, from_path) constraint legitimately fires
			// when a manual redirect already exists for this path.
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("redirect %s -> %s: %v (likely already exists)", r.FromPath, r.ToPath, err))
			continue
		}
		createdRedirects = append(createdRedirects, rid)
		res.RedirectsCreated++
	}

	slog.Info("migration apply complete",
		"site", siteID,
		"collections", res.CollectionsCreated,
		"items", res.ItemsCreated,
		"pages", res.PagesCreated,
		"redirects", res.RedirectsCreated,
		"media_uploaded", res.MediaUploaded,
		"media_failed", res.MediaFailed)
	return res, nil
}

// rollback undoes the IDs the porter created in this Apply call. Walks in
// reverse so foreign-key dependencies clear cleanly (redirects -> pages ->
// items -> collections). Errors during rollback go to slog; we don't fail
// the call further.
func (p *Porter) rollback(ctx context.Context, redirects, pages, items, colls []string) {
	for _, id := range redirects {
		if err := p.queries.DeleteRedirect(ctx, id); err != nil {
			slog.Warn("rollback delete redirect", "id", id, "err", err)
		}
	}
	for _, id := range pages {
		if err := p.queries.DeletePage(ctx, id); err != nil {
			slog.Warn("rollback delete page", "id", id, "err", err)
		}
	}
	for _, id := range items {
		if err := p.queries.DeleteItem(ctx, id); err != nil {
			slog.Warn("rollback delete item", "id", id, "err", err)
		}
	}
	for _, id := range colls {
		if err := p.queries.DeleteCollection(ctx, id); err != nil {
			slog.Warn("rollback delete collection", "id", id, "err", err)
		}
	}
}

// rewriteMediaInItemData walks an item's data map and rewrites any string
// value that matches a key in mediaMap to the corresponding atomicsite URL.
// Same-shape rewrite of []string values too. This is what makes the
// imported posts self-hosted instead of hot-linking the source CDN.
func rewriteMediaInItemData(data map[string]any, mediaMap map[string]string) map[string]any {
	if len(mediaMap) == 0 {
		return data
	}
	out := make(map[string]any, len(data))
	for k, v := range data {
		switch tv := v.(type) {
		case string:
			out[k] = rewriteMediaURL(tv, mediaMap)
		case []string:
			rewritten := make([]string, len(tv))
			for i, s := range tv {
				rewritten[i] = rewriteMediaURL(s, mediaMap)
			}
			out[k] = rewritten
		case []any:
			rewritten := make([]any, len(tv))
			for i, el := range tv {
				if s, ok := el.(string); ok {
					rewritten[i] = rewriteMediaURL(s, mediaMap)
				} else {
					rewritten[i] = el
				}
			}
			out[k] = rewritten
		default:
			out[k] = v
		}
	}
	return out
}

// rewriteMediaURL returns mediaMap[s] when the string is an exact match
// (so we don't accidentally rewrite values that happen to contain a media
// URL as a substring). Also rewrites HTML blobs by walking <img src> and
// <a href> values via simple string replace because the imported HTML is
// trusted (we control its origin via SafeFetch).
func rewriteMediaURL(s string, mediaMap map[string]string) string {
	if newURL, ok := mediaMap[s]; ok {
		return newURL
	}
	// HTML rewrite: cheap string-replace for src="..." and href="..." with
	// the source URL. Doesn't touch text outside attribute values because
	// we only replace exact `="<url>"` and `="<url>"` patterns.
	out := s
	for src, dst := range mediaMap {
		out = strings.ReplaceAll(out, `src="`+src+`"`, `src="`+dst+`"`)
		out = strings.ReplaceAll(out, `href="`+src+`"`, `href="`+dst+`"`)
		out = strings.ReplaceAll(out, `src='`+src+`'`, `src='`+dst+`'`)
		out = strings.ReplaceAll(out, `href='`+src+`'`, `href='`+dst+`'`)
	}
	return out
}

// rewriteLinksInItemData rewrites internal links inside HTML strings to
// point at the new atomicsite slugs decided by the URL planner. Same
// mechanism as rewriteMediaURL but keyed by SourcePath -> NewSlug.
func rewriteLinksInItemData(data map[string]any, plan URLPlan) map[string]any {
	if len(plan.Pages) == 0 {
		return data
	}
	pathMap := map[string]string{}
	for _, pp := range plan.Pages {
		if pp.SourcePath != pp.NewSlug && !pp.Skipped {
			pathMap[pp.SourcePath] = pp.NewSlug
		}
	}
	if len(pathMap) == 0 {
		return data
	}
	out := make(map[string]any, len(data))
	for k, v := range data {
		if s, ok := v.(string); ok {
			rewritten := s
			for src, dst := range pathMap {
				rewritten = strings.ReplaceAll(rewritten, `href="`+src+`"`, `href="`+dst+`"`)
				rewritten = strings.ReplaceAll(rewritten, `href='`+src+`'`, `href='`+dst+`'`)
			}
			out[k] = rewritten
		} else {
			out[k] = v
		}
	}
	return out
}

// truncateMeta caps a string at n characters with no clever word-boundary
// detection (Google handles partial truncation gracefully; better to be
// deterministic than smart-but-inconsistent).
func truncateMeta(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// newPorterID returns a 24-char hex ID. Same shape used elsewhere in the
// codebase for site/page/collection IDs; kept local so the porter doesn't
// import handlers.
func newPorterID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
