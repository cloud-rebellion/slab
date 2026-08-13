package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/bright-interaction/slab/internal/agent"
	"github.com/bright-interaction/slab/internal/builder"
	"github.com/bright-interaction/slab/internal/config"
	"github.com/bright-interaction/slab/internal/store"
)

// GlobalBlockHandler exposes admin CRUD for the site-wide blocks that the
// builder slots into <header> and <footer>. The agent already has its own
// agent-side endpoint at PUT /api/agent/global/{slot} for upsert; this
// handler is for the human admin UI.
type GlobalBlockHandler struct {
	cfg     *config.Config
	queries *store.Queries
	// guardrails runs the same per-site block validation the ordinary block
	// handlers run. It was missing here entirely, and a global block renders in
	// the header and footer of EVERY page, so a <script> payload refused by
	// POST /api/sites/{id}/pages/{id}/blocks was accepted here and shipped
	// site-wide. Blast radius larger than an ordinary block, not smaller.
	guardrails *agent.GuardrailEngine
}

func NewGlobalBlockHandler(cfg *config.Config, queries *store.Queries) *GlobalBlockHandler {
	return &GlobalBlockHandler{
		cfg:        cfg,
		queries:    queries,
		guardrails: agent.NewGuardrailEngine(queries),
	}
}

// allowedSlots gates create/update so a careless caller can't seed a junk
// slot string the builder doesn't read. Mirrors the existing layout.go
// switch statement (header / footer).
var allowedGlobalSlots = map[string]bool{
	"header": true,
	"footer": true,
}

// List returns every global block on a site, ordered by slot then sort_order.
func (h *GlobalBlockHandler) List(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	rows, err := h.queries.ListGlobalBlocksBySite(r.Context(), siteID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list global blocks")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"global_blocks": rows})
}

// Get returns one global block, scoped to the site to prevent cross-site reads.
func (h *GlobalBlockHandler) Get(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	id := urlParam(r, "blockID")
	gb, err := h.queries.GetGlobalBlockByID(r.Context(), id)
	if err != nil || gb.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Global block not found")
		return
	}
	writeJSON(w, http.StatusOK, gb)
}

// Create inserts a new global block on a site. Slot must be header or footer.
func (h *GlobalBlockHandler) Create(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")

	var req struct {
		Name      string          `json:"name"`
		Slot      string          `json:"slot"`
		BlockType string          `json:"block_type"`
		Data      json.RawMessage `json:"data"`
		Style     json.RawMessage `json:"style"`
		SortOrder *int64          `json:"sort_order"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if !allowedGlobalSlots[req.Slot] {
		writeError(w, http.StatusBadRequest, "slot must be 'header' or 'footer'")
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
	if v, bad := validateBlockWrite(r.Context(), h.guardrails, siteID, req.BlockType, dataJSON); bad {
		writeGuardrailViolations(w, v)
		return
	}
	// Auto-assign sort_order to max(slot)+1 unless the caller explicitly
	// supplies one. The schema has UNIQUE(site_id, slot, sort_order); the
	// site-create seed reserves 0 in both header and footer slots, so
	// every subsequent block needs a higher number to avoid a 500.
	var sortOrder int64
	if req.SortOrder != nil {
		sortOrder = *req.SortOrder
	} else {
		existing, _ := h.queries.ListGlobalBlocksBySite(r.Context(), siteID)
		next := int64(0)
		for _, gb := range existing {
			if gb.Slot == req.Slot && gb.SortOrder >= next {
				next = gb.SortOrder + 1
			}
		}
		sortOrder = next
	}

	id := newID()
	if err := h.queries.CreateGlobalBlock(r.Context(), store.CreateGlobalBlockParams{
		ID:        id,
		SiteID:    siteID,
		Name:      req.Name,
		Slot:      req.Slot,
		BlockType: req.BlockType,
		DataJson:  dataJSON,
		StyleJson: styleJSON,
		SortOrder: sortOrder,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create global block")
		return
	}
	gb, _ := h.queries.GetGlobalBlockByID(r.Context(), id)
	writeJSON(w, http.StatusCreated, gb)
}

// Update edits the name, slot, block_type, data, style, sort_order, or
// is_active of a global block. Slot is validated against allowed values.
func (h *GlobalBlockHandler) Update(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	id := urlParam(r, "blockID")
	existing, err := h.queries.GetGlobalBlockByID(r.Context(), id)
	if err != nil || existing.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Global block not found")
		return
	}

	var req struct {
		Name      *string          `json:"name"`
		Slot      *string          `json:"slot"`
		BlockType *string          `json:"block_type"`
		Data      *json.RawMessage `json:"data"`
		Style     *json.RawMessage `json:"style"`
		SortOrder *int64           `json:"sort_order"`
		IsActive  *bool            `json:"is_active"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	params := store.UpdateGlobalBlockParams{
		ID:        id,
		Name:      existing.Name,
		Slot:      existing.Slot,
		BlockType: existing.BlockType,
		DataJson:  existing.DataJson,
		StyleJson: existing.StyleJson,
		IsActive:  existing.IsActive,
		SortOrder: existing.SortOrder,
	}

	if req.Name != nil {
		params.Name = *req.Name
	}
	if req.Slot != nil {
		if !allowedGlobalSlots[*req.Slot] {
			writeError(w, http.StatusBadRequest, "slot must be 'header' or 'footer'")
			return
		}
		params.Slot = *req.Slot
	}
	if req.BlockType != nil {
		params.BlockType = *req.BlockType
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
	if req.IsActive != nil {
		if *req.IsActive {
			params.IsActive = 1
		} else {
			params.IsActive = 0
		}
	}

	// Validate the RESOLVED payload, not just the delta: a partial update can
	// turn an innocuous global block into a violating one by changing data
	// alone. Same contract as BlockHandler.Update.
	if v, bad := validateBlockWrite(r.Context(), h.guardrails, siteID, params.BlockType, params.DataJson); bad {
		writeGuardrailViolations(w, v)
		return
	}

	if err := h.queries.UpdateGlobalBlock(r.Context(), params); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to update global block")
		return
	}
	gb, _ := h.queries.GetGlobalBlockByID(r.Context(), id)
	writeJSON(w, http.StatusOK, gb)
}

// Delete removes a global block. Returns 404 cross-site to avoid leaking
// the existence of blocks on other sites.
func (h *GlobalBlockHandler) Delete(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	id := urlParam(r, "blockID")
	existing, err := h.queries.GetGlobalBlockByID(r.Context(), id)
	if err != nil || existing.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Global block not found")
		return
	}
	if err := h.queries.DeleteGlobalBlock(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete global block")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// Render returns the HTML the builder would emit for a block given the
// provided (block_type, slot, data_json). Used by the admin UI's
// "Rendered" tab so editors see the actual output without saving first.
// Output is byte-for-byte the same as what lands in the deployed site
// because we share the renderer with internal/builder.
//
// Accepts an unsaved payload (not a block_id lookup) so the preview
// reflects in-progress edits in the JSON textarea.
func (h *GlobalBlockHandler) Render(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BlockType string `json:"block_type"`
		Slot      string `json:"slot"`
		DataJSON  string `json:"data_json"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if !allowedGlobalSlots[req.Slot] {
		writeError(w, http.StatusBadRequest, "Invalid slot")
		return
	}
	gb := store.GlobalBlock{
		BlockType: req.BlockType,
		Slot:      req.Slot,
		DataJson:  req.DataJSON,
	}
	html := builder.ExtractHTMLFromGlobalBlock(gb)
	writeJSON(w, http.StatusOK, map[string]string{
		"html":       html,
		"block_type": req.BlockType,
		"slot":       req.Slot,
	})
}
