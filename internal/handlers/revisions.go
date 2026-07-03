// Package handlers: entity revisions endpoint.
//
// Sprint 1 of the WP/Webflow replacement roadmap (see
// atomicsite/docs/ROADMAP.md). Lists per-entity history, fetches a
// specific snapshot, and restores it onto the live row. Restore is
// non-destructive: applying snapshot v3 onto a page at v7 creates
// v8 (the current state) plus v9 (the restored state) so history
// stays append-only and the operator can undo the restore itself.
//
// Versioned entities (v1): pages, blocks. Future sprints extend to
// global_blocks, collection_items, branding via the same recorder.
//
// Auth: SiteAccessMiddleware gates the URL siteID; cross-tenant
// defence-in-depth via the existing pageInSite / blockInSite helpers.

package handlers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/bright-interaction/slab/internal/config"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/revisions"
	"github.com/bright-interaction/slab/internal/store"
)

const revisionsListDefaultLimit = 50
const revisionsListMaxLimit = 200

// RevisionsHandler powers the per-entity history surface. It depends
// on the same *store.Queries the page + block handlers use plus a
// recorder so Restore can snapshot the current state before applying
// the prior snapshot.
type RevisionsHandler struct {
	cfg      *config.Config
	queries  *store.Queries
	recorder *revisions.Recorder
}

func NewRevisionsHandler(cfg *config.Config, queries *store.Queries, recorder *revisions.Recorder) *RevisionsHandler {
	return &RevisionsHandler{cfg: cfg, queries: queries, recorder: recorder}
}

// recordRevision writes a pre-mutation snapshot and logs loudly when it
// fails. The mutation proceeds either way (undo must never block an
// edit), but a silently skipped snapshot before a destructive op
// violates the history contract the restore UI advertises, so every
// failure is visible in the logs. Shared by every handler that records.
func recordRevision(rec *revisions.Recorder, ctx context.Context, p revisions.RecordParams) {
	if rec == nil {
		return
	}
	if err := rec.Record(ctx, p); err != nil {
		slog.Warn("revisions: snapshot failed; proceeding without an undo point",
			"entity_type", p.EntityType, "entity_id", p.EntityID, "site_id", p.SiteID, "err", err)
	}
}

// RevisionOut is the wire shape every endpoint returns. Mirrors
// store.EntityRevision but flattens timestamps to plain strings the
// frontend renders directly without re-parsing.
type RevisionOut struct {
	ID            string `json:"id"`
	SiteID        string `json:"site_id"`
	EntityType    string `json:"entity_type"`
	EntityID      string `json:"entity_id"`
	VersionNumber int64  `json:"version_number"`
	SnapshotJSON  string `json:"snapshot_json"`
	ChangeSummary string `json:"change_summary"`
	CreatedBy     string `json:"created_by"`
	CreatedAt     string `json:"created_at"`
}

func toRevisionOut(row store.EntityRevision) RevisionOut {
	return RevisionOut{
		ID:            row.ID,
		SiteID:        row.SiteID,
		EntityType:    row.EntityType,
		EntityID:      row.EntityID,
		VersionNumber: row.VersionNumber,
		SnapshotJSON:  row.SnapshotJson,
		ChangeSummary: row.ChangeSummary,
		CreatedBy:     row.CreatedBy,
		CreatedAt:     row.CreatedAt,
	}
}

// validEntityType matches the schema CHECK constraint AND the
// revisions package constants. Centralised here so the handler
// returns a friendly 400 instead of bubbling a SQLite constraint
// error.
func validEntityType(t string) bool {
	return t == revisions.EntityTypePage || t == revisions.EntityTypeBlock
}

// verifyEntityBelongsToSite is the cross-tenant defence: a user with
// access to site A cannot list/get/restore revisions for an entity
// that belongs to site B even if they know the entity ID. Returns
// true if entity exists in the given site; false otherwise (no
// distinguishing between "missing" and "wrong tenant" by design).
func (h *RevisionsHandler) verifyEntityBelongsToSite(ctx context.Context, siteID, entityType, entityID string) bool {
	switch entityType {
	case revisions.EntityTypePage:
		page, err := h.queries.GetPageByID(ctx, entityID)
		if err != nil {
			return false
		}
		return page.SiteID == siteID
	case revisions.EntityTypeBlock:
		block, err := h.queries.GetBlockByID(ctx, entityID)
		if err != nil {
			return false
		}
		page, err := h.queries.GetPageByID(ctx, block.PageID)
		if err != nil {
			return false
		}
		return page.SiteID == siteID
	default:
		return false
	}
}

// List returns up to ?limit revisions for the entity, newest first.
// GET /api/sites/{siteID}/revisions/{entityType}/{entityID}
func (h *RevisionsHandler) List(w http.ResponseWriter, r *http.Request) {
	if authmw.GetUser(r) == nil {
		writeError(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	siteID := urlParam(r, "siteID")
	entityType := urlParam(r, "entityType")
	entityID := urlParam(r, "entityID")
	if !validEntityType(entityType) {
		writeError(w, http.StatusBadRequest, "entity_type must be page or block")
		return
	}
	if !h.verifyEntityBelongsToSite(r.Context(), siteID, entityType, entityID) {
		writeError(w, http.StatusNotFound, "entity not found in site")
		return
	}

	limit := revisionsListDefaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 && v <= revisionsListMaxLimit {
			limit = v
		}
	}

	rows, err := h.queries.ListEntityRevisions(r.Context(), store.ListEntityRevisionsParams{
		SiteID:     siteID,
		EntityType: entityType,
		EntityID:   entityID,
		Limit:      int64(limit),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]RevisionOut, 0, len(rows))
	for _, row := range rows {
		out = append(out, toRevisionOut(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"revisions": out,
		"count":     len(out),
	})
}

// Get returns a single revision by version number.
// GET /api/sites/{siteID}/revisions/{entityType}/{entityID}/{version}
func (h *RevisionsHandler) Get(w http.ResponseWriter, r *http.Request) {
	if authmw.GetUser(r) == nil {
		writeError(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	siteID := urlParam(r, "siteID")
	entityType := urlParam(r, "entityType")
	entityID := urlParam(r, "entityID")
	versionStr := urlParam(r, "version")
	if !validEntityType(entityType) {
		writeError(w, http.StatusBadRequest, "entity_type must be page or block")
		return
	}
	version, err := strconv.ParseInt(versionStr, 10, 64)
	if err != nil || version <= 0 {
		writeError(w, http.StatusBadRequest, "version must be a positive integer")
		return
	}
	if !h.verifyEntityBelongsToSite(r.Context(), siteID, entityType, entityID) {
		writeError(w, http.StatusNotFound, "entity not found in site")
		return
	}
	row, err := h.queries.GetEntityRevisionByVersion(r.Context(), store.GetEntityRevisionByVersionParams{
		SiteID:        siteID,
		EntityType:    entityType,
		EntityID:      entityID,
		VersionNumber: version,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "revision not found")
		return
	}
	writeJSON(w, http.StatusOK, toRevisionOut(row))
}

// Restore applies the snapshot of the given version onto the current
// entity. The current state is first snapshotted (so the operator
// can undo the restore itself), then the prior snapshot is applied.
// POST /api/sites/{siteID}/revisions/{entityType}/{entityID}/{version}/restore
func (h *RevisionsHandler) Restore(w http.ResponseWriter, r *http.Request) {
	u := authmw.GetUser(r)
	if u == nil {
		writeError(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	siteID := urlParam(r, "siteID")
	entityType := urlParam(r, "entityType")
	entityID := urlParam(r, "entityID")
	versionStr := urlParam(r, "version")
	if !validEntityType(entityType) {
		writeError(w, http.StatusBadRequest, "entity_type must be page or block")
		return
	}
	version, err := strconv.ParseInt(versionStr, 10, 64)
	if err != nil || version <= 0 {
		writeError(w, http.StatusBadRequest, "version must be a positive integer")
		return
	}
	if !h.verifyEntityBelongsToSite(r.Context(), siteID, entityType, entityID) {
		writeError(w, http.StatusNotFound, "entity not found in site")
		return
	}

	target, err := h.queries.GetEntityRevisionByVersion(r.Context(), store.GetEntityRevisionByVersionParams{
		SiteID:        siteID,
		EntityType:    entityType,
		EntityID:      entityID,
		VersionNumber: version,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "revision not found")
		return
	}

	createdBy := "user:" + u.ID
	summary := "restored to v" + strconv.FormatInt(version, 10)

	switch entityType {
	case revisions.EntityTypePage:
		h.restorePage(w, r, siteID, entityID, target.SnapshotJson, summary, createdBy)
	case revisions.EntityTypeBlock:
		h.restoreBlock(w, r, siteID, entityID, target.SnapshotJson, summary, createdBy)
	}
}

// restorePage snapshots current page, then applies the prior snapshot
// via UpdatePage and returns the updated row.
func (h *RevisionsHandler) restorePage(w http.ResponseWriter, r *http.Request, siteID, pageID, snapshotJSON, summary, createdBy string) {
	current, err := h.queries.GetPageByID(r.Context(), pageID)
	if err != nil {
		writeError(w, http.StatusNotFound, "Page not found")
		return
	}

	if h.recorder != nil {
		recordRevision(h.recorder, r.Context(), revisions.RecordParams{
			SiteID:        siteID,
			EntityType:    revisions.EntityTypePage,
			EntityID:      pageID,
			Snapshot:      current,
			ChangeSummary: "pre-restore snapshot",
			CreatedBy:     createdBy,
		})
	}

	var snap store.Page
	if err := json.Unmarshal([]byte(snapshotJSON), &snap); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "snapshot not parseable as page")
		return
	}
	if err := h.queries.UpdatePage(r.Context(), store.UpdatePageParams{
		Title:            snap.Title,
		Slug:             snap.Slug,
		Status:           snap.Status,
		MetaTitle:        snap.MetaTitle,
		MetaDescription:  snap.MetaDescription,
		OgImageID:        snap.OgImageID,
		Layout:           snap.Layout,
		SortOrder:        snap.SortOrder,
		ShowInNav:        snap.ShowInNav,
		NavLabel:         snap.NavLabel,
		NoIndex:          snap.NoIndex,
		CanonicalUrl:     snap.CanonicalUrl,
		HideGlobalBlocks: snap.HideGlobalBlocks,
		ID:               pageID,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to apply snapshot")
		return
	}

	// UpdatePage does not carry the archetype column (it has its own setter), so
	// restore it separately or the vibe-archetype lock desyncs after a restore.
	// Best-effort: the page content is already restored; a rare archetype-set
	// error should not fail the whole restore.
	if err := h.queries.SetPageArchetype(r.Context(), store.SetPageArchetypeParams{
		Archetype: snap.Archetype,
		ID:        pageID,
	}); err != nil {
		slog.Warn("restore: failed to restore page archetype", "page_id", pageID, "err", err)
	}

	updated, _ := h.queries.GetPageByID(r.Context(), pageID)
	writeJSON(w, http.StatusOK, map[string]any{
		"entity_type":    revisions.EntityTypePage,
		"entity_id":      pageID,
		"restored_from":  snap,
		"change_summary": summary,
		"entity":         updated,
	})
}

// restoreBlock snapshots current block, then applies the prior snapshot
// via UpdateBlock and returns the updated row.
func (h *RevisionsHandler) restoreBlock(w http.ResponseWriter, r *http.Request, siteID, blockID, snapshotJSON, summary, createdBy string) {
	current, err := h.queries.GetBlockByID(r.Context(), blockID)
	if err != nil {
		writeError(w, http.StatusNotFound, "Block not found")
		return
	}

	if h.recorder != nil {
		recordRevision(h.recorder, r.Context(), revisions.RecordParams{
			SiteID:        siteID,
			EntityType:    revisions.EntityTypeBlock,
			EntityID:      blockID,
			Snapshot:      current,
			ChangeSummary: "pre-restore snapshot",
			CreatedBy:     createdBy,
		})
	}

	var snap store.Block
	if err := json.Unmarshal([]byte(snapshotJSON), &snap); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "snapshot not parseable as block")
		return
	}
	if err := h.queries.UpdateBlock(r.Context(), store.UpdateBlockParams{
		BlockType: snap.BlockType,
		Name:      snap.Name,
		SortOrder: snap.SortOrder,
		DataJson:  snap.DataJson,
		StyleJson: snap.StyleJson,
		IsVisible: snap.IsVisible,
		ID:        blockID,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to apply snapshot")
		return
	}

	updated, _ := h.queries.GetBlockByID(r.Context(), blockID)
	writeJSON(w, http.StatusOK, map[string]any{
		"entity_type":    revisions.EntityTypeBlock,
		"entity_id":      blockID,
		"restored_from":  snap,
		"change_summary": summary,
		"entity":         updated,
	})
}

// listLatestForEntity is an exported helper used by the MCP tools so
// the agent can read revisions without going through the HTTP layer.
// Sanitises inputs and routes to the same query the REST List uses.
func (h *RevisionsHandler) ListLatestForEntity(ctx context.Context, siteID, entityType, entityID string, limit int) ([]RevisionOut, error) {
	entityType = strings.ToLower(strings.TrimSpace(entityType))
	if !validEntityType(entityType) {
		return nil, errInvalidEntityType
	}
	if !h.verifyEntityBelongsToSite(ctx, siteID, entityType, entityID) {
		return nil, errEntityNotFound
	}
	if limit <= 0 || limit > revisionsListMaxLimit {
		limit = revisionsListDefaultLimit
	}
	rows, err := h.queries.ListEntityRevisions(ctx, store.ListEntityRevisionsParams{
		SiteID:     siteID,
		EntityType: entityType,
		EntityID:   entityID,
		Limit:      int64(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]RevisionOut, 0, len(rows))
	for _, row := range rows {
		out = append(out, toRevisionOut(row))
	}
	return out, nil
}

// GetByVersion exposes the single-revision read so MCP tools can
// share the same cross-tenant defence as the REST endpoint.
func (h *RevisionsHandler) GetByVersion(ctx context.Context, siteID, entityType, entityID string, version int64) (RevisionOut, error) {
	entityType = strings.ToLower(strings.TrimSpace(entityType))
	if !validEntityType(entityType) {
		return RevisionOut{}, errInvalidEntityType
	}
	if !h.verifyEntityBelongsToSite(ctx, siteID, entityType, entityID) {
		return RevisionOut{}, errEntityNotFound
	}
	row, err := h.queries.GetEntityRevisionByVersion(ctx, store.GetEntityRevisionByVersionParams{
		SiteID:        siteID,
		EntityType:    entityType,
		EntityID:      entityID,
		VersionNumber: version,
	})
	if err != nil {
		return RevisionOut{}, errRevisionNotFound
	}
	return toRevisionOut(row), nil
}

// RestoreForAgent is the MCP-side path. Mirrors the REST Restore but
// stamps created_by with the agent identity and returns the updated
// entity. Errors are typed so the MCP tool can map them to the right
// JSON-RPC error.
func (h *RevisionsHandler) RestoreForAgent(ctx context.Context, siteID, entityType, entityID string, version int64, agentLabel string) (map[string]any, error) {
	entityType = strings.ToLower(strings.TrimSpace(entityType))
	if !validEntityType(entityType) {
		return nil, errInvalidEntityType
	}
	if !h.verifyEntityBelongsToSite(ctx, siteID, entityType, entityID) {
		return nil, errEntityNotFound
	}
	target, err := h.queries.GetEntityRevisionByVersion(ctx, store.GetEntityRevisionByVersionParams{
		SiteID:        siteID,
		EntityType:    entityType,
		EntityID:      entityID,
		VersionNumber: version,
	})
	if err != nil {
		return nil, errRevisionNotFound
	}
	summary := "restored to v" + strconv.FormatInt(version, 10)
	createdBy := agentLabel
	if createdBy == "" {
		createdBy = "agent:unknown"
	}
	switch entityType {
	case revisions.EntityTypePage:
		current, err := h.queries.GetPageByID(ctx, entityID)
		if err != nil {
			return nil, errEntityNotFound
		}
		if h.recorder != nil {
			recordRevision(h.recorder, ctx, revisions.RecordParams{
				SiteID:        siteID,
				EntityType:    revisions.EntityTypePage,
				EntityID:      entityID,
				Snapshot:      current,
				ChangeSummary: "pre-restore snapshot",
				CreatedBy:     createdBy,
			})
		}
		var snap store.Page
		if err := json.Unmarshal([]byte(target.SnapshotJson), &snap); err != nil {
			return nil, errSnapshotUnparseable
		}
		if err := h.queries.UpdatePage(ctx, store.UpdatePageParams{
			Title:            snap.Title,
			Slug:             snap.Slug,
			Status:           snap.Status,
			MetaTitle:        snap.MetaTitle,
			MetaDescription:  snap.MetaDescription,
			OgImageID:        snap.OgImageID,
			Layout:           snap.Layout,
			SortOrder:        snap.SortOrder,
			ShowInNav:        snap.ShowInNav,
			NavLabel:         snap.NavLabel,
			NoIndex:          snap.NoIndex,
			CanonicalUrl:     snap.CanonicalUrl,
			HideGlobalBlocks: snap.HideGlobalBlocks,
			ID:               entityID,
		}); err != nil {
			return nil, err
		}
		// UpdatePage does not carry the archetype column (separate
		// setter); restore it too or the vibe-archetype lock desyncs
		// after an MCP restore (the REST restore path already does this).
		if err := h.queries.SetPageArchetype(ctx, store.SetPageArchetypeParams{
			Archetype: snap.Archetype,
			ID:        entityID,
		}); err != nil {
			slog.Warn("restore: failed to restore page archetype", "page_id", entityID, "err", err)
		}
		updated, _ := h.queries.GetPageByID(ctx, entityID)
		return map[string]any{
			"entity_type":    revisions.EntityTypePage,
			"entity_id":      entityID,
			"change_summary": summary,
			"entity":         updated,
		}, nil
	case revisions.EntityTypeBlock:
		current, err := h.queries.GetBlockByID(ctx, entityID)
		if err != nil {
			return nil, errEntityNotFound
		}
		if h.recorder != nil {
			recordRevision(h.recorder, ctx, revisions.RecordParams{
				SiteID:        siteID,
				EntityType:    revisions.EntityTypeBlock,
				EntityID:      entityID,
				Snapshot:      current,
				ChangeSummary: "pre-restore snapshot",
				CreatedBy:     createdBy,
			})
		}
		var snap store.Block
		if err := json.Unmarshal([]byte(target.SnapshotJson), &snap); err != nil {
			return nil, errSnapshotUnparseable
		}
		if err := h.queries.UpdateBlock(ctx, store.UpdateBlockParams{
			BlockType: snap.BlockType,
			Name:      snap.Name,
			SortOrder: snap.SortOrder,
			DataJson:  snap.DataJson,
			StyleJson: snap.StyleJson,
			IsVisible: snap.IsVisible,
			ID:        entityID,
		}); err != nil {
			return nil, err
		}
		updated, _ := h.queries.GetBlockByID(ctx, entityID)
		return map[string]any{
			"entity_type":    revisions.EntityTypeBlock,
			"entity_id":      entityID,
			"change_summary": summary,
			"entity":         updated,
		}, nil
	}
	return nil, errInvalidEntityType
}
