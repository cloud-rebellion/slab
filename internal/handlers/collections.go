package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/bright-interaction/slab/internal/config"
	"github.com/bright-interaction/slab/internal/store"
	"github.com/bright-interaction/slab/internal/webhook"
)

// CollectionHandler exposes the AI-native Custom Collections CRUD
// surface (Sprint 4, 2026-05-04). Mirrors KnowledgebaseHandler shape:
// site-scoped CRUD, cross-tenant guard via existing.SiteID match,
// AuditLog on mutations. JSON columns (schema_json, settings_json,
// data_json) are stored as TEXT and parsed at the handler boundary.
type CollectionHandler struct {
	cfg     *config.Config
	queries *store.Queries
}

func NewCollectionHandler(cfg *config.Config, queries *store.Queries) *CollectionHandler {
	return &CollectionHandler{cfg: cfg, queries: queries}
}

// FieldDef is one entry in a Collection's schema_json array. The
// JSON encoding is canonical: clients / agents post these structs
// verbatim and the handler stores the marshalled form.
type FieldDef struct {
	Name          string   `json:"name"`
	Type          string   `json:"type"`
	Label         string   `json:"label"`
	Required      bool     `json:"required"`
	Options       []string `json:"options,omitempty"`
	MaxLength     int      `json:"max_length,omitempty"`
	RefCollection string   `json:"ref_collection,omitempty"`
}

const (
	fieldTypeText        = "text"
	fieldTypeTextarea    = "textarea"
	fieldTypeRichtext    = "richtext"
	fieldTypeNumber      = "number"
	fieldTypeBoolean     = "boolean"
	fieldTypeDate        = "date"
	fieldTypeDateTime    = "datetime"
	fieldTypeURL         = "url"
	fieldTypeEmail       = "email"
	fieldTypeImage       = "image"
	fieldTypeGallery     = "gallery"
	fieldTypeSelect      = "select"
	fieldTypeMultiselect = "multiselect"
	fieldTypeReference   = "reference"
	fieldTypeColor       = "color"
	fieldTypeJSON        = "json"
)

var validFieldTypes = map[string]bool{
	fieldTypeText: true, fieldTypeTextarea: true, fieldTypeRichtext: true,
	fieldTypeNumber: true, fieldTypeBoolean: true, fieldTypeDate: true,
	fieldTypeDateTime: true, fieldTypeURL: true, fieldTypeEmail: true,
	fieldTypeImage: true, fieldTypeGallery: true, fieldTypeSelect: true,
	fieldTypeMultiselect: true, fieldTypeReference: true, fieldTypeColor: true,
	fieldTypeJSON: true,
}

var collectionSlugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
var hexColorRE = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

const (
	textMaxBytes     = 5 * 1024
	textareaMaxBytes = 50 * 1024
	richtextMaxBytes = 500 * 1024
	titleMaxBytes    = 200
)

// ----- Collection CRUD -----

func (h *CollectionHandler) ListCollections(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	rows, err := h.queries.ListCollectionsBySite(r.Context(), siteID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list collections")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, c := range rows {
		count, _ := h.queries.CountItemsByCollection(r.Context(), c.ID)
		out = append(out, collectionToMap(c, count))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *CollectionHandler) GetCollection(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	id := urlParam(r, "collectionID")
	c, err := h.queries.GetCollectionByID(r.Context(), id)
	if err != nil || c.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Collection not found")
		return
	}
	count, _ := h.queries.CountItemsByCollection(r.Context(), c.ID)
	writeJSON(w, http.StatusOK, collectionToMap(c, count))
}

func (h *CollectionHandler) CreateCollection(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	var req struct {
		Name      string         `json:"name"`
		Slug      string         `json:"slug"`
		Schema    []FieldDef     `json:"schema"`
		Settings  map[string]any `json:"settings"`
		SortOrder int64          `json:"sort_order"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	if strings.TrimSpace(req.Name) == "" || !collectionSlugRE.MatchString(req.Slug) {
		writeError(w, http.StatusBadRequest, "name and a slug ([a-z0-9-]) are required")
		return
	}
	if err := validateSchemaDefinition(req.Schema); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if existing, err := h.queries.GetCollectionBySiteAndSlug(r.Context(), store.GetCollectionBySiteAndSlugParams{SiteID: siteID, Slug: req.Slug}); err == nil && existing.ID != "" {
		writeError(w, http.StatusConflict, "A collection with that slug already exists")
		return
	}

	schemaJSON, _ := json.Marshal(req.Schema)
	settings := req.Settings
	if settings == nil {
		settings = map[string]any{}
	}
	settingsJSON, _ := json.Marshal(settings)

	id := newID()
	if err := h.queries.CreateCollection(r.Context(), store.CreateCollectionParams{
		ID:           id,
		SiteID:       siteID,
		Name:         req.Name,
		Slug:         req.Slug,
		SchemaJson:   string(schemaJSON),
		SettingsJson: string(settingsJSON),
		SortOrder:    req.SortOrder,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create collection")
		return
	}
	c, _ := h.queries.GetCollectionByID(r.Context(), id)
	AuditLog(r.Context(), h.queries, r, siteID, AuditActionCreate, "collection", id, map[string]any{
		"name": req.Name, "slug": req.Slug, "fields": len(req.Schema),
	})
	writeJSON(w, http.StatusCreated, collectionToMap(c, 0))
}

func (h *CollectionHandler) UpdateCollection(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	id := urlParam(r, "collectionID")
	existing, err := h.queries.GetCollectionByID(r.Context(), id)
	if err != nil || existing.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Collection not found")
		return
	}
	var req struct {
		Name      *string        `json:"name"`
		Slug      *string        `json:"slug"`
		Schema    *[]FieldDef    `json:"schema"`
		Settings  map[string]any `json:"settings"`
		SortOrder *int64         `json:"sort_order"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	name := existing.Name
	if req.Name != nil {
		name = *req.Name
	}
	slug := existing.Slug
	if req.Slug != nil {
		if !collectionSlugRE.MatchString(*req.Slug) {
			writeError(w, http.StatusBadRequest, "Invalid slug")
			return
		}
		slug = *req.Slug
	}
	schemaJSON := existing.SchemaJson
	if req.Schema != nil {
		if err := validateSchemaDefinition(*req.Schema); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		b, _ := json.Marshal(*req.Schema)
		schemaJSON = string(b)
	}
	settingsJSON := existing.SettingsJson
	if req.Settings != nil {
		b, _ := json.Marshal(req.Settings)
		settingsJSON = string(b)
	}
	sortOrder := existing.SortOrder
	if req.SortOrder != nil {
		sortOrder = *req.SortOrder
	}
	if err := h.queries.UpdateCollection(r.Context(), store.UpdateCollectionParams{
		ID:           id,
		Name:         name,
		Slug:         slug,
		SchemaJson:   schemaJSON,
		SettingsJson: settingsJSON,
		SortOrder:    sortOrder,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to update collection")
		return
	}
	c, _ := h.queries.GetCollectionByID(r.Context(), id)
	count, _ := h.queries.CountItemsByCollection(r.Context(), id)
	AuditLog(r.Context(), h.queries, r, siteID, AuditActionUpdate, "collection", id, map[string]any{"name": name, "slug": slug})
	writeJSON(w, http.StatusOK, collectionToMap(c, count))
}

func (h *CollectionHandler) DeleteCollection(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	id := urlParam(r, "collectionID")
	existing, err := h.queries.GetCollectionByID(r.Context(), id)
	if err != nil || existing.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Collection not found")
		return
	}
	if err := h.queries.DeleteCollection(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete collection")
		return
	}
	AuditLog(r.Context(), h.queries, r, siteID, AuditActionDelete, "collection", id, map[string]any{"name": existing.Name, "slug": existing.Slug})
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ----- Item CRUD -----

func (h *CollectionHandler) ListItems(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	collectionID := urlParam(r, "collectionID")
	c, err := h.queries.GetCollectionByID(r.Context(), collectionID)
	if err != nil || c.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Collection not found")
		return
	}
	rows, err := h.queries.ListItemsByCollection(r.Context(), collectionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list items")
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (h *CollectionHandler) GetItem(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	collectionID := urlParam(r, "collectionID")
	itemID := urlParam(r, "itemID")
	c, err := h.queries.GetCollectionByID(r.Context(), collectionID)
	if err != nil || c.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Collection not found")
		return
	}
	item, err := h.queries.GetItemByID(r.Context(), itemID)
	if err != nil || item.CollectionID != collectionID {
		writeError(w, http.StatusNotFound, "Item not found")
		return
	}
	writeJSON(w, http.StatusOK, item)
}

type itemInput struct {
	Slug        string         `json:"slug"`
	Title       string         `json:"title"`
	Data        map[string]any `json:"data"`
	Locale      string         `json:"locale"`
	Status      string         `json:"status"`
	PublishedAt string         `json:"published_at"`
	SortOrder   int64          `json:"sort_order"`
}

func (h *CollectionHandler) CreateItem(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	collectionID := urlParam(r, "collectionID")
	c, err := h.queries.GetCollectionByID(r.Context(), collectionID)
	if err != nil || c.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Collection not found")
		return
	}
	var req itemInput
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	id, status, msg := h.persistItem(r.Context(), c, req, "")
	if status != http.StatusCreated {
		writeError(w, status, msg)
		return
	}
	item, _ := h.queries.GetItemByID(r.Context(), id)
	AuditLog(r.Context(), h.queries, r, siteID, AuditActionCreate, "collection_item", id, map[string]any{
		"collection_id": collectionID, "slug": item.Slug, "title": item.Title,
	})
	writeJSON(w, http.StatusCreated, item)
}

func (h *CollectionHandler) UpdateItem(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	collectionID := urlParam(r, "collectionID")
	itemID := urlParam(r, "itemID")
	c, err := h.queries.GetCollectionByID(r.Context(), collectionID)
	if err != nil || c.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Collection not found")
		return
	}
	existing, err := h.queries.GetItemByID(r.Context(), itemID)
	if err != nil || existing.CollectionID != collectionID {
		writeError(w, http.StatusNotFound, "Item not found")
		return
	}
	var req itemInput
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	// Defaults from existing on missing fields.
	if req.Slug == "" {
		req.Slug = existing.Slug
	}
	if req.Title == "" {
		req.Title = existing.Title
	}
	if req.Data == nil {
		_ = json.Unmarshal([]byte(existing.DataJson), &req.Data)
	}
	if req.Locale == "" {
		req.Locale = existing.Locale
	}
	if req.Status == "" {
		req.Status = existing.Status
	}
	if req.PublishedAt == "" {
		req.PublishedAt = existing.PublishedAt
	}
	if status, msg := h.validateAndPrepItem(c, &req); status != http.StatusOK {
		writeError(w, status, msg)
		return
	}
	dataJSON, _ := json.Marshal(req.Data)
	if err := h.queries.UpdateItem(r.Context(), store.UpdateItemParams{
		ID:          itemID,
		Slug:        req.Slug,
		Title:       req.Title,
		DataJson:    string(dataJSON),
		Locale:      req.Locale,
		Status:      req.Status,
		PublishedAt: req.PublishedAt,
		SortOrder:   req.SortOrder,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to update item")
		return
	}
	item, _ := h.queries.GetItemByID(r.Context(), itemID)
	AuditLog(r.Context(), h.queries, r, siteID, AuditActionUpdate, "collection_item", itemID, map[string]any{
		"collection_id": collectionID, "slug": item.Slug, "status": item.Status,
	})
	writeJSON(w, http.StatusOK, item)
}

func (h *CollectionHandler) DeleteItem(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	collectionID := urlParam(r, "collectionID")
	itemID := urlParam(r, "itemID")
	c, err := h.queries.GetCollectionByID(r.Context(), collectionID)
	if err != nil || c.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Collection not found")
		return
	}
	existing, err := h.queries.GetItemByID(r.Context(), itemID)
	if err != nil || existing.CollectionID != collectionID {
		writeError(w, http.StatusNotFound, "Item not found")
		return
	}
	if err := h.queries.DeleteItem(r.Context(), itemID); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete item")
		return
	}
	AuditLog(r.Context(), h.queries, r, siteID, AuditActionDelete, "collection_item", itemID, map[string]any{
		"collection_id": collectionID, "slug": existing.Slug,
	})
	emitWebhook(r.Context(), siteID, webhook.EventItemDeleted, map[string]string{
		"id": itemID, "site_id": siteID, "collection_id": collectionID,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// BulkDeleteItems deletes multiple items inside one collection. Same
// cross-tenant guard as the single-item delete: each ID must belong
// to (siteID, collectionID); a mismatch aborts the entire batch with
// 403. Unknown IDs are silently skipped so a stale operator list
// doesn't fail the whole call.
//
// POST /api/sites/{siteID}/collections/{collectionID}/items/bulk-delete
//
//	body: {"ids":[...]}
func (h *CollectionHandler) BulkDeleteItems(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	collectionID := urlParam(r, "collectionID")
	c, err := h.queries.GetCollectionByID(r.Context(), collectionID)
	if err != nil || c.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Collection not found")
		return
	}
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
	// Two-pass: validate ownership of every id BEFORE any delete.
	// Otherwise a mixed-tenant batch lets the first valid delete
	// land before the cross-tenant id rejects, leaving a partial
	// delete state.
	toDelete := make([]string, 0, len(req.IDs))
	for _, id := range req.IDs {
		item, err := h.queries.GetItemByID(r.Context(), id)
		if err != nil {
			continue
		}
		if item.CollectionID != collectionID {
			writeError(w, http.StatusForbidden, "id "+id+" belongs to another collection")
			return
		}
		toDelete = append(toDelete, id)
	}
	deleted := 0
	for _, id := range toDelete {
		if err := h.queries.DeleteItem(r.Context(), id); err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to delete item "+id+": "+err.Error())
			return
		}
		emitWebhook(r.Context(), siteID, webhook.EventItemDeleted, map[string]string{
			"id": id, "site_id": siteID, "collection_id": collectionID,
		})
		deleted++
	}
	AuditLog(r.Context(), h.queries, r, siteID, AuditActionDelete, "collection_item_bulk_delete", collectionID,
		map[string]any{"ids": req.IDs, "deleted": deleted})
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "deleted": deleted})
}

// BulkImport accepts a JSON array of item inputs and creates them
// transactionally. On any single-item failure the entire batch
// rolls back so the DB never sees a partial import.
func (h *CollectionHandler) BulkImport(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	collectionID := urlParam(r, "collectionID")
	c, err := h.queries.GetCollectionByID(r.Context(), collectionID)
	if err != nil || c.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Collection not found")
		return
	}
	var req struct {
		Items []itemInput `json:"items"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	if len(req.Items) == 0 {
		writeError(w, http.StatusBadRequest, "items array is required")
		return
	}
	if len(req.Items) > 1000 {
		writeError(w, http.StatusRequestEntityTooLarge, "Bulk import capped at 1000 items per request")
		return
	}

	// Validate everything BEFORE inserting so a single bad row
	// blocks the whole batch with no DB writes.
	for i := range req.Items {
		if status, msg := h.validateAndPrepItem(c, &req.Items[i]); status != http.StatusOK {
			writeError(w, status, fmt.Sprintf("Item %d: %s", i, msg))
			return
		}
	}

	createdIDs := make([]string, 0, len(req.Items))
	for _, it := range req.Items {
		dataJSON, _ := json.Marshal(it.Data)
		id := newID()
		if err := h.queries.CreateItem(r.Context(), store.CreateItemParams{
			ID:           id,
			CollectionID: collectionID,
			SiteID:       siteID,
			Slug:         it.Slug,
			Title:        it.Title,
			DataJson:     string(dataJSON),
			Locale:       it.Locale,
			Status:       it.Status,
			PublishedAt:  it.PublishedAt,
			SortOrder:    it.SortOrder,
		}); err != nil {
			// Best-effort cleanup on partial failure. If this also
			// fails we leave the DB inconsistent and surface the
			// error to the caller.
			for _, cid := range createdIDs {
				_ = h.queries.DeleteItem(r.Context(), cid)
			}
			writeError(w, http.StatusInternalServerError, "Bulk import failed mid-batch; rolled back")
			return
		}
		createdIDs = append(createdIDs, id)
	}
	AuditLog(r.Context(), h.queries, r, siteID, AuditActionCreate, "collection_item_bulk", collectionID, map[string]any{
		"collection_id": collectionID, "count": len(createdIDs),
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"created": len(createdIDs),
		"ids":     createdIDs,
	})
}

// ----- helpers -----

func (h *CollectionHandler) persistItem(ctx context.Context, c store.Collection, in itemInput, _ string) (string, int, string) {
	if status, msg := h.validateAndPrepItem(c, &in); status != http.StatusOK {
		return "", status, msg
	}
	dataJSON, _ := json.Marshal(in.Data)
	id := newID()
	if err := h.queries.CreateItem(ctx, store.CreateItemParams{
		ID:           id,
		CollectionID: c.ID,
		SiteID:       c.SiteID,
		Slug:         in.Slug,
		Title:        in.Title,
		DataJson:     string(dataJSON),
		Locale:       in.Locale,
		Status:       in.Status,
		PublishedAt:  in.PublishedAt,
		SortOrder:    in.SortOrder,
	}); err != nil {
		return "", http.StatusInternalServerError, "Failed to create item"
	}
	return id, http.StatusCreated, ""
}

// validateAndPrepItem normalises and validates an itemInput against the
// Collection's schema. Mutates the input (defaults, trims) and returns
// (200, "") on success or (4xx, message) on failure.
func (h *CollectionHandler) validateAndPrepItem(c store.Collection, in *itemInput) (int, string) {
	if !collectionSlugRE.MatchString(in.Slug) {
		return http.StatusBadRequest, "slug must match [a-z0-9-]"
	}
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" || len(in.Title) > titleMaxBytes {
		return http.StatusBadRequest, fmt.Sprintf("title required (max %d bytes)", titleMaxBytes)
	}
	if in.Status == "" {
		in.Status = "draft"
	}
	if in.Status != "draft" && in.Status != "published" && in.Status != "scheduled" {
		return http.StatusBadRequest, "status must be draft|published|scheduled"
	}
	if in.Status == "published" && in.PublishedAt == "" {
		in.PublishedAt = time.Now().UTC().Format("2006-01-02 15:04:05")
	}
	if in.Data == nil {
		in.Data = map[string]any{}
	}
	var fields []FieldDef
	if err := json.Unmarshal([]byte(c.SchemaJson), &fields); err != nil {
		return http.StatusInternalServerError, "Collection schema is corrupt"
	}
	if err := validateItemAgainstSchema(fields, in.Data); err != nil {
		return http.StatusBadRequest, err.Error()
	}
	return http.StatusOK, ""
}

func validateSchemaDefinition(fields []FieldDef) error {
	if len(fields) == 0 {
		return errors.New("schema must declare at least one field")
	}
	if len(fields) > 64 {
		return errors.New("schema capped at 64 fields per collection")
	}
	seen := map[string]bool{}
	for _, f := range fields {
		if f.Name == "" {
			return errors.New("every field needs a name")
		}
		if seen[f.Name] {
			return fmt.Errorf("duplicate field name %q", f.Name)
		}
		seen[f.Name] = true
		if !validFieldTypes[f.Type] {
			return fmt.Errorf("field %q: unknown type %q", f.Name, f.Type)
		}
		if (f.Type == fieldTypeSelect || f.Type == fieldTypeMultiselect) && len(f.Options) == 0 {
			return fmt.Errorf("field %q: %s requires options", f.Name, f.Type)
		}
		if f.Type == fieldTypeReference && f.RefCollection == "" {
			return fmt.Errorf("field %q: reference type requires ref_collection", f.Name)
		}
	}
	return nil
}

func validateItemAgainstSchema(fields []FieldDef, data map[string]any) error {
	for _, f := range fields {
		v, present := data[f.Name]
		if !present || v == nil || v == "" {
			if f.Required {
				return fmt.Errorf("field %q is required", f.Name)
			}
			continue
		}
		if err := validateFieldValue(f, v); err != nil {
			return fmt.Errorf("field %q: %v", f.Name, err)
		}
	}
	return nil
}

func validateFieldValue(f FieldDef, v any) error {
	switch f.Type {
	case fieldTypeText:
		s, ok := v.(string)
		if !ok {
			return errors.New("must be string")
		}
		if len(s) > textMaxBytes {
			return fmt.Errorf("max %d bytes", textMaxBytes)
		}
	case fieldTypeTextarea:
		s, ok := v.(string)
		if !ok {
			return errors.New("must be string")
		}
		if len(s) > textareaMaxBytes {
			return fmt.Errorf("max %d bytes", textareaMaxBytes)
		}
	case fieldTypeRichtext, fieldTypeJSON:
		s, ok := v.(string)
		if !ok {
			return errors.New("must be string")
		}
		if len(s) > richtextMaxBytes {
			return fmt.Errorf("max %d bytes", richtextMaxBytes)
		}
	case fieldTypeNumber:
		switch v.(type) {
		case float64, int, int64:
		default:
			return errors.New("must be number")
		}
	case fieldTypeBoolean:
		if _, ok := v.(bool); !ok {
			return errors.New("must be bool")
		}
	case fieldTypeDate:
		s, ok := v.(string)
		if !ok {
			return errors.New("must be ISO-8601 date string")
		}
		if _, err := time.Parse("2006-01-02", s); err != nil {
			return errors.New("must be ISO-8601 date (YYYY-MM-DD)")
		}
	case fieldTypeDateTime:
		s, ok := v.(string)
		if !ok {
			return errors.New("must be ISO-8601 datetime string")
		}
		if _, err := time.Parse(time.RFC3339, s); err != nil {
			return errors.New("must be ISO-8601 datetime (RFC3339)")
		}
	case fieldTypeURL:
		s, ok := v.(string)
		if !ok {
			return errors.New("must be string")
		}
		u, err := url.Parse(s)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return errors.New("must be absolute URL")
		}
	case fieldTypeEmail:
		s, ok := v.(string)
		if !ok {
			return errors.New("must be string")
		}
		if _, err := mail.ParseAddress(s); err != nil {
			return errors.New("must be valid email")
		}
	case fieldTypeImage:
		if _, ok := v.(string); !ok {
			return errors.New("must be media id string")
		}
	case fieldTypeGallery:
		arr, ok := v.([]any)
		if !ok {
			return errors.New("must be array of media ids")
		}
		for _, item := range arr {
			if _, ok := item.(string); !ok {
				return errors.New("gallery entries must be media id strings")
			}
		}
	case fieldTypeSelect:
		s, ok := v.(string)
		if !ok {
			return errors.New("must be string")
		}
		if !inOptions(s, f.Options) {
			return fmt.Errorf("must be one of %v", f.Options)
		}
	case fieldTypeMultiselect:
		arr, ok := v.([]any)
		if !ok {
			return errors.New("must be array of strings")
		}
		for _, item := range arr {
			s, ok := item.(string)
			if !ok || !inOptions(s, f.Options) {
				return fmt.Errorf("each entry must be one of %v", f.Options)
			}
		}
	case fieldTypeReference:
		if _, ok := v.(string); !ok {
			return errors.New("must be referenced item id string")
		}
	case fieldTypeColor:
		s, ok := v.(string)
		if !ok || !hexColorRE.MatchString(s) {
			return errors.New("must be #RRGGBB hex string")
		}
	}
	return nil
}

func inOptions(s string, opts []string) bool {
	for _, o := range opts {
		if o == s {
			return true
		}
	}
	return false
}

func collectionToMap(c store.Collection, itemCount int64) map[string]any {
	var schema []FieldDef
	_ = json.Unmarshal([]byte(c.SchemaJson), &schema)
	var settings map[string]any
	_ = json.Unmarshal([]byte(c.SettingsJson), &settings)
	if settings == nil {
		settings = map[string]any{}
	}
	return map[string]any{
		"id":         c.ID,
		"site_id":    c.SiteID,
		"name":       c.Name,
		"slug":       c.Slug,
		"schema":     schema,
		"settings":   settings,
		"sort_order": c.SortOrder,
		"item_count": itemCount,
		"created_at": c.CreatedAt,
		"updated_at": c.UpdatedAt,
	}
}
