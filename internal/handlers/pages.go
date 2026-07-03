package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/bright-interaction/slab/internal/agent"
	"github.com/bright-interaction/slab/internal/builder"
	"github.com/bright-interaction/slab/internal/config"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/revisions"
	"github.com/bright-interaction/slab/internal/store"
	"github.com/bright-interaction/slab/internal/webhook"
)

func encodeWarnings(vs []agent.Violation) string {
	b, _ := json.Marshal(vs)
	return string(b)
}

// pageSlugSegment matches one URL segment of a page slug. Pages can be
// nested ("blog/post"), so the full slug is segments joined by "/". Each
// segment must be lowercase alnum + dashes/underscores; ".." is rejected
// outright. Empty slug ("" -> index page) is allowed and validated as a
// special case before the regex runs.
var pageSlugSegment = regexp.MustCompile(`^[a-z0-9][-a-z0-9_]{0,79}$`)

// ValidatePageSlug is the exported entry point so the MCP and locale
// write paths validate page slugs against the exact same rules as the
// REST handler, with no second implementation to drift. Empty is allowed
// (slug auto-derived downstream). See validatePageSlug for the rules.
func ValidatePageSlug(s string) error { return validatePageSlug(s) }

// componentNameRE accepts a safe component identifier. The name becomes a
// filename under the generated src/components/ dir, so it must never contain a
// path separator or "..". Mirrors builder.ValidComponentName (the last-line
// guard at file write); this is the write-time rejection with a clear error.
var componentNameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

// ValidateComponentName rejects component names that are not safe filename
// identifiers (path traversal, separators, leading non-letter).
func ValidateComponentName(name string) error {
	if !componentNameRE.MatchString(name) {
		return errors.New("component name must start with a letter and contain only letters, digits, hyphen or underscore (max 64)")
	}
	return nil
}

// validatePageSlug rejects path traversal and chars that would corrupt the
// builder's slugToFilePath. Allowed: empty (root index), "blog/post-name",
// "level-1/level-2/level-3". Rejected: "..", "../etc/passwd", trailing
// slashes, double slashes, control chars, anything not [a-z0-9-_].
func validatePageSlug(s string) error {
	if s == "" {
		return nil
	}
	if len(s) > 250 {
		return fmt.Errorf("page slug too long (max 250 chars)")
	}
	if strings.ContainsAny(s, "\r\n\t") {
		return fmt.Errorf("page slug contains control characters")
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == "" {
			return fmt.Errorf("page slug has empty segment (leading/trailing/double slash)")
		}
		if seg == "." || seg == ".." {
			return fmt.Errorf("page slug segment %q is not allowed", seg)
		}
		if !pageSlugSegment.MatchString(seg) {
			return fmt.Errorf("page slug segment %q must match ^[a-z0-9][-a-z0-9_]{0,79}$", seg)
		}
	}
	return nil
}

type PageHandler struct {
	cfg      *config.Config
	queries  *store.Queries
	recorder *revisions.Recorder
}

func NewPageHandler(cfg *config.Config, queries *store.Queries) *PageHandler {
	return &PageHandler{cfg: cfg, queries: queries}
}

// SetRecorder wires the revisions recorder. Nil-safe: when unset the
// Update path skips revision capture (used by older test fixtures
// that don't care about history).
func (h *PageHandler) SetRecorder(r *revisions.Recorder) {
	h.recorder = r
}

// snapshotPageForRevision is a small helper so the recorder call is
// uniform across REST update + future MCP update paths. Returns the
// "user:{id}" or "agent:{keyID}" label for created_by.
func snapshotCreatedBy(r *http.Request) string {
	if u := authmw.GetUser(r); u != nil {
		return "user:" + u.ID
	}
	if a := authmw.GetAgent(r); a != nil {
		return "agent:" + a.KeyID
	}
	return ""
}

func (h *PageHandler) List(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	pages, err := h.queries.ListPagesBySite(r.Context(), siteID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list pages")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pages": pages})
}

func (h *PageHandler) Get(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	page, ok := pageInSite(r.Context(), h.queries, w, urlParam(r, "pageID"), siteID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *PageHandler) Create(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")

	// Verify site exists
	if _, err := h.queries.GetSiteByID(r.Context(), siteID); err != nil {
		writeError(w, http.StatusNotFound, "Site not found")
		return
	}

	var req struct {
		Title     string `json:"title"`
		Slug      string `json:"slug"`
		Layout    string `json:"layout"`
		ShowInNav *bool  `json:"show_in_nav"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.Title == "" {
		writeError(w, http.StatusBadRequest, "Title is required")
		return
	}
	if req.Slug == "" {
		req.Slug = strings.ToLower(strings.ReplaceAll(req.Title, " ", "-"))
	}
	req.Slug = strings.Trim(strings.ToLower(req.Slug), "/")
	if err := validatePageSlug(req.Slug); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Layout == "" {
		req.Layout = "default"
	}

	showInNav := int64(1)
	if req.ShowInNav != nil && !*req.ShowInNav {
		showInNav = 0
	}

	// Get next sort order
	count, _ := h.queries.CountPagesBySite(r.Context(), siteID)

	id := newID()
	if err := h.queries.CreatePage(r.Context(), store.CreatePageParams{
		ID:        id,
		SiteID:    siteID,
		Title:     req.Title,
		Slug:      req.Slug,
		Layout:    req.Layout,
		SortOrder: count,
		ShowInNav: showInNav,
	}); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeError(w, http.StatusConflict, "A page with this slug already exists in this site")
			return
		}
		writeError(w, http.StatusInternalServerError, "Failed to create page")
		return
	}

	page, _ := h.queries.GetPageByID(r.Context(), id)
	emitWebhook(r.Context(), siteID, webhook.EventPageCreated, page)
	writeJSON(w, http.StatusCreated, page)
}

func (h *PageHandler) Update(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	pageID := urlParam(r, "pageID")
	existing, ok := pageInSite(r.Context(), h.queries, w, pageID, siteID)
	if !ok {
		return
	}

	var req struct {
		Title            *string `json:"title"`
		Slug             *string `json:"slug"`
		Status           *string `json:"status"`
		MetaTitle        *string `json:"meta_title"`
		MetaDescription  *string `json:"meta_description"`
		OgImageID        *string `json:"og_image_id"`
		Layout           *string `json:"layout"`
		SortOrder        *int64  `json:"sort_order"`
		ShowInNav        *bool   `json:"show_in_nav"`
		NavLabel         *string `json:"nav_label"`
		NoIndex          *bool   `json:"no_index"`
		CanonicalURL     *string `json:"canonical_url"`
		HideGlobalBlocks *bool   `json:"hide_global_blocks"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	params := store.UpdatePageParams{
		Title:            existing.Title,
		Slug:             existing.Slug,
		Status:           existing.Status,
		MetaTitle:        existing.MetaTitle,
		MetaDescription:  existing.MetaDescription,
		OgImageID:        existing.OgImageID,
		Layout:           existing.Layout,
		SortOrder:        existing.SortOrder,
		ShowInNav:        existing.ShowInNav,
		NavLabel:         existing.NavLabel,
		NoIndex:          existing.NoIndex,
		CanonicalUrl:     existing.CanonicalUrl,
		HideGlobalBlocks: existing.HideGlobalBlocks,
		ID:               pageID,
	}

	if req.Title != nil {
		params.Title = *req.Title
	}
	if req.Slug != nil {
		slug := strings.Trim(strings.ToLower(*req.Slug), "/")
		if err := validatePageSlug(slug); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		params.Slug = slug
	}
	if req.Status != nil {
		params.Status = *req.Status
	}
	if req.MetaTitle != nil {
		params.MetaTitle = *req.MetaTitle
	}
	if req.MetaDescription != nil {
		params.MetaDescription = *req.MetaDescription
	}
	if req.OgImageID != nil {
		params.OgImageID = *req.OgImageID
	}
	if req.Layout != nil {
		params.Layout = *req.Layout
	}
	if req.SortOrder != nil {
		params.SortOrder = *req.SortOrder
	}
	if req.ShowInNav != nil {
		if *req.ShowInNav {
			params.ShowInNav = 1
		} else {
			params.ShowInNav = 0
		}
	}
	if req.NavLabel != nil {
		params.NavLabel = *req.NavLabel
	}
	if req.NoIndex != nil {
		if *req.NoIndex {
			params.NoIndex = 1
		} else {
			params.NoIndex = 0
		}
	}
	if req.CanonicalURL != nil {
		params.CanonicalUrl = *req.CanonicalURL
	}
	if req.HideGlobalBlocks != nil {
		if *req.HideGlobalBlocks {
			params.HideGlobalBlocks = 1
		} else {
			params.HideGlobalBlocks = 0
		}
	}

	// Input-time SEO guardrails (mirrors Site Inspector eval engine).
	violations := agent.ValidatePageMeta(agent.PageMetaInput{
		Title:           params.Title,
		MetaTitle:       params.MetaTitle,
		MetaDescription: params.MetaDescription,
	})
	if agent.HasErrors(violations) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":      "Page meta failed guardrail validation",
			"violations": violations,
		})
		return
	}

	if h.recorder != nil {
		recordRevision(h.recorder, r.Context(), revisions.RecordParams{
			SiteID:        siteID,
			EntityType:    revisions.EntityTypePage,
			EntityID:      pageID,
			Snapshot:      existing,
			ChangeSummary: pageChangeSummary(existing, params),
			CreatedBy:     snapshotCreatedBy(r),
		})
	}

	if err := h.queries.UpdatePage(r.Context(), params); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to update page")
		return
	}

	page, _ := h.queries.GetPageByID(r.Context(), pageID)
	if len(violations) > 0 {
		w.Header().Set("X-Atomicsite-Warnings", encodeWarnings(violations))
	}
	emitWebhook(r.Context(), siteID, webhook.EventPageUpdated, page)
	writeJSON(w, http.StatusOK, page)
}

// pageChangeSummary returns a short human-readable label of what
// changed between the existing page and the incoming update params.
// Used as the change_summary on the revision row so the history
// drawer can show "title edit" rather than "(none)".
func pageChangeSummary(existing store.Page, p store.UpdatePageParams) string {
	parts := []string{}
	if existing.Title != p.Title {
		parts = append(parts, "title")
	}
	if existing.Slug != p.Slug {
		parts = append(parts, "slug")
	}
	if existing.Status != p.Status {
		parts = append(parts, "status")
	}
	if existing.MetaTitle != p.MetaTitle || existing.MetaDescription != p.MetaDescription {
		parts = append(parts, "seo")
	}
	if existing.Layout != p.Layout {
		parts = append(parts, "layout")
	}
	if existing.ShowInNav != p.ShowInNav || existing.NavLabel != p.NavLabel {
		parts = append(parts, "nav")
	}
	if existing.NoIndex != p.NoIndex || existing.CanonicalUrl != p.CanonicalUrl {
		parts = append(parts, "indexing")
	}
	if len(parts) == 0 {
		return "no-op edit"
	}
	return strings.Join(parts, ", ") + " edit"
}

func (h *PageHandler) Delete(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	pageID := urlParam(r, "pageID")
	if _, ok := pageInSite(r.Context(), h.queries, w, pageID, siteID); !ok {
		return
	}

	if err := h.queries.DeletePage(r.Context(), pageID); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete page")
		return
	}
	emitWebhook(r.Context(), siteID, webhook.EventPageDeleted, map[string]string{"id": pageID, "site_id": siteID})
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// bulkDeleteMaxIDs caps the number of IDs accepted in a single
// bulk-delete call. 200 keeps the per-call transaction bounded (one
// SQLite connection is the bottleneck) while covering "select every
// page in this folder + delete" workflows.
const bulkDeleteMaxIDs = 200

// BulkDelete deletes multiple pages in one call. Cross-tenant guard:
// each page must belong to siteID; mismatches return 403 and abort
// the entire batch (no partial deletes when an attacker is trying
// to delete other tenants' rows). Unknown IDs are silently skipped
// because operators may post a stale list and we don't want to fail
// the whole batch over one missing row.
//
// POST /api/sites/{siteID}/pages/bulk-delete   body: {"ids":[...]}
func (h *PageHandler) BulkDelete(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if len(req.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "ids is required")
		return
	}
	if len(req.IDs) > bulkDeleteMaxIDs {
		writeError(w, http.StatusBadRequest, "too many ids (max 200 per call)")
		return
	}
	// Two-pass: validate every id first so a single cross-tenant id
	// aborts the batch BEFORE any row is deleted. Without this, an
	// attacker could mix one of their own ids with one of another
	// tenant's and the first delete would land before we reject.
	toDelete := make([]string, 0, len(req.IDs))
	for _, id := range req.IDs {
		page, err := h.queries.GetPageByID(r.Context(), id)
		if err != nil {
			continue
		}
		if page.SiteID != siteID {
			writeError(w, http.StatusForbidden, "id "+id+" belongs to another site")
			return
		}
		toDelete = append(toDelete, id)
	}
	deleted := 0
	for _, id := range toDelete {
		if err := h.queries.DeletePage(r.Context(), id); err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to delete page "+id+": "+err.Error())
			return
		}
		emitWebhook(r.Context(), siteID, webhook.EventPageDeleted, map[string]string{"id": id, "site_id": siteID})
		deleted++
	}
	AuditLog(r.Context(), h.queries, r,
		siteID, AuditActionDelete, "page_bulk_delete", siteID,
		map[string]any{"ids": req.IDs, "deleted": deleted})
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "deleted": deleted})
}

// PageDraftPreview renders the page's current DB state to a complete HTML
// document the admin app embeds via iframe srcdoc for live in-app preview.
// NOT a build: skips bun + astro, writes nothing to disk. Reuses the same
// renderBlock + global-block + CSS pipeline production uses, so layout
// fidelity is high; component blocks render as placeholders since they
// need Astro compilation. The response sets noindex + same-origin headers
// because the URL is admin-only and not meant to be linkable externally.
func (h *PageHandler) PageDraftPreview(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	pageID := urlParam(r, "pageID")

	page, err := h.queries.GetPageByID(r.Context(), pageID)
	if err != nil || page.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Page not found")
		return
	}

	html, err := builder.RenderPageDraft(r.Context(), h.queries, siteID, pageID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to render preview")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	_, _ = w.Write([]byte(html))
}

// PreviewSource returns the assembled .astro source for a single page so the
// admin can show "View source" without triggering a full build. Uses the
// same renderPage routine the build pipeline writes to disk; output is the
// canonical source-of-truth for what bun build will see.
func (h *PageHandler) PreviewSource(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	pageID := urlParam(r, "pageID")

	page, err := h.queries.GetPageByID(r.Context(), pageID)
	if err != nil || page.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Page not found")
		return
	}

	src, err := builder.RenderPagePreview(r.Context(), h.queries, siteID, pageID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to render page")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"page_id": pageID,
		"slug":    page.Slug,
		"title":   page.Title,
		"astro":   src,
	})
}

func (h *PageHandler) Reorder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Order []struct {
			ID        string `json:"id"`
			SortOrder int64  `json:"sort_order"`
		} `json:"order"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	for _, item := range req.Order {
		_ = h.queries.UpdatePageOrder(r.Context(), store.UpdatePageOrderParams{
			SortOrder: item.SortOrder,
			ID:        item.ID,
		})
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
