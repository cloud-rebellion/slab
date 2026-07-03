package handlers

import (
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/bright-interaction/slab/internal/blocks"
	"github.com/bright-interaction/slab/internal/builder"
	"github.com/bright-interaction/slab/internal/config"
	"github.com/bright-interaction/slab/internal/revisions"
	"github.com/bright-interaction/slab/internal/store"
)

type BlockHandler struct {
	cfg      *config.Config
	queries  *store.Queries
	db       *sql.DB
	recorder *revisions.Recorder
}

func NewBlockHandler(cfg *config.Config, queries *store.Queries, db *sql.DB) *BlockHandler {
	return &BlockHandler{cfg: cfg, queries: queries, db: db}
}

// SetRecorder wires the revisions recorder. Nil-safe.
func (h *BlockHandler) SetRecorder(r *revisions.Recorder) {
	h.recorder = r
}

func (h *BlockHandler) List(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	pageID := urlParam(r, "pageID")
	if _, ok := pageInSite(r.Context(), h.queries, w, pageID, siteID); !ok {
		return
	}
	blocks, err := h.queries.ListBlocksByPage(r.Context(), pageID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list blocks")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"blocks": blocks})
}

func (h *BlockHandler) Get(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	block, ok := blockInSite(r.Context(), h.queries, w, urlParam(r, "blockID"), siteID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, block)
}

func (h *BlockHandler) Create(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	pageID := urlParam(r, "pageID")
	if _, ok := pageInSite(r.Context(), h.queries, w, pageID, siteID); !ok {
		return
	}

	var req struct {
		BlockType string          `json:"block_type"`
		Name      string          `json:"name"`
		Data      json.RawMessage `json:"data"`
		Style     json.RawMessage `json:"style"`
		SortOrder *int64          `json:"sort_order"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.BlockType == "" {
		writeError(w, http.StatusBadRequest, "block_type is required")
		return
	}

	dataJSON := "{}"
	if len(req.Data) > 0 {
		dataJSON = string(req.Data)
	}
	styleJSON := "{}"
	if len(req.Style) > 0 {
		styleJSON = string(req.Style)
	}

	sortOrder := int64(0)
	if req.SortOrder != nil {
		sortOrder = *req.SortOrder
	}

	id := newID()
	if err := h.queries.CreateBlock(r.Context(), store.CreateBlockParams{
		ID:        id,
		PageID:    pageID,
		BlockType: req.BlockType,
		Name:      req.Name,
		SortOrder: sortOrder,
		DataJson:  dataJSON,
		StyleJson: styleJSON,
		IsVisible: 1,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create block")
		return
	}

	block, _ := h.queries.GetBlockByID(r.Context(), id)
	writeJSON(w, http.StatusCreated, block)
}

func (h *BlockHandler) Update(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	blockID := urlParam(r, "blockID")
	existing, ok := blockInSite(r.Context(), h.queries, w, blockID, siteID)
	if !ok {
		return
	}

	var req struct {
		BlockType *string          `json:"block_type"`
		Name      *string          `json:"name"`
		Data      *json.RawMessage `json:"data"`
		Style     *json.RawMessage `json:"style"`
		SortOrder *int64           `json:"sort_order"`
		IsVisible *bool            `json:"is_visible"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	params := store.UpdateBlockParams{
		BlockType: existing.BlockType,
		Name:      existing.Name,
		SortOrder: existing.SortOrder,
		DataJson:  existing.DataJson,
		StyleJson: existing.StyleJson,
		IsVisible: existing.IsVisible,
		ID:        blockID,
	}

	if req.BlockType != nil {
		params.BlockType = *req.BlockType
	}
	if req.Name != nil {
		params.Name = *req.Name
	}
	if req.Data != nil {
		params.DataJson = string(*req.Data)
	}
	if req.Style != nil {
		params.StyleJson = string(*req.Style)
	}
	if req.SortOrder != nil {
		params.SortOrder = *req.SortOrder
	}
	if req.IsVisible != nil {
		if *req.IsVisible {
			params.IsVisible = 1
		} else {
			params.IsVisible = 0
		}
	}

	if h.recorder != nil {
		recordRevision(h.recorder, r.Context(), revisions.RecordParams{
			SiteID:        siteID,
			EntityType:    revisions.EntityTypeBlock,
			EntityID:      blockID,
			Snapshot:      existing,
			ChangeSummary: blockChangeSummary(existing, params),
			CreatedBy:     snapshotCreatedBy(r),
		})
	}

	if err := h.queries.UpdateBlock(r.Context(), params); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to update block")
		return
	}

	block, _ := h.queries.GetBlockByID(r.Context(), blockID)
	writeJSON(w, http.StatusOK, block)
}

// blockChangeSummary is the human-readable label for the revision
// row created when a block is updated. Returns "data edit" when only
// the data_json changed (the most common edit), more specific labels
// for type/style/visibility/order changes.
func blockChangeSummary(existing store.Block, p store.UpdateBlockParams) string {
	parts := []string{}
	if existing.BlockType != p.BlockType {
		parts = append(parts, "type")
	}
	if existing.Name != p.Name {
		parts = append(parts, "name")
	}
	if existing.DataJson != p.DataJson {
		parts = append(parts, "data")
	}
	if existing.StyleJson != p.StyleJson {
		parts = append(parts, "style")
	}
	if existing.SortOrder != p.SortOrder {
		parts = append(parts, "order")
	}
	if existing.IsVisible != p.IsVisible {
		parts = append(parts, "visibility")
	}
	if len(parts) == 0 {
		return "no-op edit"
	}
	return joinWithComma(parts) + " edit"
}

func joinWithComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

func (h *BlockHandler) Delete(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	blockID := urlParam(r, "blockID")
	existing, ok := blockInSite(r.Context(), h.queries, w, blockID, siteID)
	if !ok {
		return
	}

	if h.recorder != nil {
		recordRevision(h.recorder, r.Context(), revisions.RecordParams{
			SiteID:        siteID,
			EntityType:    revisions.EntityTypeBlock,
			EntityID:      blockID,
			Snapshot:      existing,
			ChangeSummary: "pre-delete snapshot",
			CreatedBy:     snapshotCreatedBy(r),
		})
	}

	if err := h.queries.DeleteBlock(r.Context(), blockID); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete block")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *BlockHandler) Reorder(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	pageID := urlParam(r, "pageID")
	if _, ok := pageInSite(r.Context(), h.queries, w, pageID, siteID); !ok {
		return
	}

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

	// Only reorder blocks that actually belong to this page. Otherwise a
	// member of site A could pass blockIDs from site B and silently
	// scramble their sort_order even though the parent page check above
	// blocks the broader path.
	for _, item := range req.Order {
		blk, err := h.queries.GetBlockByID(r.Context(), item.ID)
		if err != nil || blk.PageID != pageID {
			continue
		}
		_ = h.queries.UpdateBlockOrder(r.Context(), store.UpdateBlockOrderParams{
			SortOrder: item.SortOrder,
			ID:        item.ID,
		})
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Preview returns the rendered Astro source for a single block. Used by
// the per-block "</>" code toggle in the admin so developers can see
// exactly what each block produces without triggering a full build.
// Defends against cross-site fetches by checking the block's page belongs
// to the requested site.
func (h *BlockHandler) Preview(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	blockID := urlParam(r, "blockID")

	block, err := h.queries.GetBlockByID(r.Context(), blockID)
	if err != nil {
		writeError(w, http.StatusNotFound, "Block not found")
		return
	}
	page, err := h.queries.GetPageByID(r.Context(), block.PageID)
	if err != nil || page.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Block not found")
		return
	}

	src, err := builder.RenderSingleBlock(r.Context(), h.queries, siteID, blockID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to render block")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"block_id":   blockID,
		"block_type": block.BlockType,
		"page_id":    block.PageID,
		"astro":      src,
	})
}

// Schemas returns the field schema for every registered block_type. The
// editor reads this once on page load and renders forms generically.
// Cacheable indefinitely client-side; only changes between deploys.
func (h *BlockHandler) Schemas(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"schemas": blocks.All()})
}

// PreviewHTML renders one block to standalone HTML using draft data
// from the request body, plus the site's compiled CSS, so the editor
// can show a live visual preview as the user types. The block must
// belong to the requested site (defends against cross-site fetches).
//
// Body: { "block_type": "stat_grid", "data_json": "{...}" }. Either
// field may be omitted to fall back to the saved values.
func (h *BlockHandler) PreviewHTML(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	blockID := urlParam(r, "blockID")

	block, err := h.queries.GetBlockByID(r.Context(), blockID)
	if err != nil {
		writeError(w, http.StatusNotFound, "Block not found")
		return
	}
	page, err := h.queries.GetPageByID(r.Context(), block.PageID)
	if err != nil || page.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Block not found")
		return
	}

	var req struct {
		BlockType string          `json:"block_type"`
		Data      json.RawMessage `json:"data"`
	}
	_ = parseJSON(r, &req)

	blockType := block.BlockType
	if req.BlockType != "" {
		blockType = req.BlockType
	}
	dataJSON := block.DataJson
	if len(req.Data) > 0 {
		dataJSON = string(req.Data)
	}

	html, err := builder.RenderBlockDraft(r.Context(), h.queries, siteID, blockType, dataJSON)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to render block")
		return
	}
	css, err := builder.BuildCSS(r.Context(), h.queries, siteID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to build CSS")
		return
	}
	fontFace := builder.SiteFontsCSS(r.Context(), h.queries, siteID)

	writeJSON(w, http.StatusOK, map[string]any{
		"block_id":   blockID,
		"block_type": blockType,
		"html":       html,
		"css":        css,
		"font_face":  fontFace,
	})
}

// BulkSave atomically replaces all blocks on a page.
func (h *BlockHandler) BulkSave(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	pageID := urlParam(r, "pageID")
	if _, ok := pageInSite(r.Context(), h.queries, w, pageID, siteID); !ok {
		return
	}

	var req struct {
		Blocks []struct {
			ID        string          `json:"id"`
			BlockType string          `json:"block_type"`
			Name      string          `json:"name"`
			SortOrder int64           `json:"sort_order"`
			Data      json.RawMessage `json:"data"`
			Style     json.RawMessage `json:"style"`
			IsVisible bool            `json:"is_visible"`
		} `json:"blocks"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	tx, err := h.db.Begin()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to begin transaction")
		return
	}
	defer tx.Rollback()

	qtx := h.queries.WithTx(tx)

	// Delete existing blocks
	if err := qtx.DeleteBlocksByPage(r.Context(), pageID); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to clear blocks")
		return
	}

	// Insert new blocks
	for _, b := range req.Blocks {
		id := b.ID
		if id == "" {
			id = newID()
		}

		dataJSON := "{}"
		if len(b.Data) > 0 {
			dataJSON = string(b.Data)
		}
		styleJSON := "{}"
		if len(b.Style) > 0 {
			styleJSON = string(b.Style)
		}

		visible := int64(0)
		if b.IsVisible {
			visible = 1
		}

		if err := qtx.CreateBlock(r.Context(), store.CreateBlockParams{
			ID:        id,
			PageID:    pageID,
			BlockType: b.BlockType,
			Name:      b.Name,
			SortOrder: b.SortOrder,
			DataJson:  dataJSON,
			StyleJson: styleJSON,
			IsVisible: visible,
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to create block")
			return
		}
	}

	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to commit")
		return
	}

	blocks, _ := h.queries.ListBlocksByPage(r.Context(), pageID)
	writeJSON(w, http.StatusOK, map[string]any{"blocks": blocks})
}
