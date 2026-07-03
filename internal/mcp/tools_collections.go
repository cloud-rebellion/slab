package mcp

// MCP tools for the AI-native Custom Collections feature
// (Sprint 4, 2026-05-04). 9 tools mirror the REST endpoints under
// /api/sites/{siteID}/collections (handlers/collections.go) but let
// the agent CRUD without a session cookie. Cross-tenant guard via
// agent.SiteID. Bulk import accepts a JSON array.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bright-interaction/slab/internal/handlers"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"
)

// FieldDef mirrors handlers.FieldDef. Duplicated to avoid a circular
// import; the JSON contract is the source of truth.
type collectionFieldDef struct {
	Name          string   `json:"name"`
	Type          string   `json:"type"`
	Label         string   `json:"label"`
	Required      bool     `json:"required"`
	Options       []string `json:"options,omitempty"`
	MaxLength     int      `json:"max_length,omitempty"`
	RefCollection string   `json:"ref_collection,omitempty"`
}

func (s *Server) registerCollectionTools() {
	register := func(t Tool) { s.tools[t.Name] = t }

	register(Tool{
		Name:        "list_collections",
		Description: "Lists every Collection (custom content type, e.g. case studies, products, team members) defined for this site. Returns the schema and item count per collection.",
		InputSchema: schema(`{"type":"object","properties":{}}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, _ json.RawMessage) (string, error) {
			rows, err := s.queries.ListCollectionsBySite(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			out := make([]map[string]any, 0, len(rows))
			for _, c := range rows {
				count, _ := s.queries.CountItemsByCollection(ctx, c.ID)
				out = append(out, collectionToMap(c, count))
			}
			return mustJSON(out), nil
		},
	})

	register(Tool{
		Name:        "create_collection",
		Description: "Defines a new Collection (custom content type). schema is an array of field definitions [{name,type,label,required,options?,max_length?,ref_collection?}]. types: text, textarea, richtext, number, boolean, date, datetime, url, email, image, gallery, select, multiselect, reference, color, json. settings can include {render_as_pages:bool, schema_org_type:string} -- when render_as_pages is true the platform emits standalone pages per item with auto JSON-LD.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"name":{"type":"string"},
				"slug":{"type":"string","description":"[a-z0-9-] kebab"},
				"schema":{"type":"array","items":{"type":"object"}},
				"settings":{"type":"object"},
				"sort_order":{"type":"number"}
			},
			"required":["name","slug","schema"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				Name      string               `json:"name"`
				Slug      string               `json:"slug"`
				Schema    []collectionFieldDef `json:"schema"`
				Settings  map[string]any       `json:"settings"`
				SortOrder int64                `json:"sort_order"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			if strings.TrimSpace(args.Name) == "" || !isCollectionSlug(args.Slug) {
				return "", errors.New("name and a valid slug ([a-z0-9-]) are required")
			}
			if err := validateCollectionSchema(args.Schema); err != nil {
				return "", err
			}
			if existing, err := s.queries.GetCollectionBySiteAndSlug(ctx, store.GetCollectionBySiteAndSlugParams{SiteID: agent.SiteID, Slug: args.Slug}); err == nil && existing.ID != "" {
				return "", errors.New("a collection with that slug already exists")
			}
			schemaJSON, _ := json.Marshal(args.Schema)
			settings := args.Settings
			if settings == nil {
				settings = map[string]any{}
			}
			settingsJSON, _ := json.Marshal(settings)
			id := newID()
			if err := s.queries.CreateCollection(ctx, store.CreateCollectionParams{
				ID:           id,
				SiteID:       agent.SiteID,
				Name:         args.Name,
				Slug:         args.Slug,
				SchemaJson:   string(schemaJSON),
				SettingsJson: string(settingsJSON),
				SortOrder:    args.SortOrder,
			}); err != nil {
				return "", err
			}
			return mustJSON(map[string]string{"id": id, "status": "created"}), nil
		},
	})

	register(Tool{
		Name:        "update_collection",
		Description: "Patches a Collection's name, slug, schema, settings, or sort_order. Pass only the fields you want to change.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"id":{"type":"string"},
				"name":{"type":"string"},
				"slug":{"type":"string"},
				"schema":{"type":"array","items":{"type":"object"}},
				"settings":{"type":"object"},
				"sort_order":{"type":"number"}
			},
			"required":["id"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				ID        string                `json:"id"`
				Name      *string               `json:"name"`
				Slug      *string               `json:"slug"`
				Schema    *[]collectionFieldDef `json:"schema"`
				Settings  map[string]any        `json:"settings"`
				SortOrder *int64                `json:"sort_order"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			existing, err := s.queries.GetCollectionByID(ctx, args.ID)
			if err != nil || existing.SiteID != agent.SiteID {
				return "", errors.New("collection not found")
			}
			name := existing.Name
			if args.Name != nil {
				name = *args.Name
			}
			slug := existing.Slug
			if args.Slug != nil {
				if !isCollectionSlug(*args.Slug) {
					return "", errors.New("invalid slug")
				}
				slug = *args.Slug
			}
			schemaJSON := existing.SchemaJson
			if args.Schema != nil {
				if err := validateCollectionSchema(*args.Schema); err != nil {
					return "", err
				}
				b, _ := json.Marshal(*args.Schema)
				schemaJSON = string(b)
			}
			settingsJSON := existing.SettingsJson
			if args.Settings != nil {
				b, _ := json.Marshal(args.Settings)
				settingsJSON = string(b)
			}
			sortOrder := existing.SortOrder
			if args.SortOrder != nil {
				sortOrder = *args.SortOrder
			}
			if err := s.queries.UpdateCollection(ctx, store.UpdateCollectionParams{
				ID:           args.ID,
				Name:         name,
				Slug:         slug,
				SchemaJson:   schemaJSON,
				SettingsJson: settingsJSON,
				SortOrder:    sortOrder,
			}); err != nil {
				return "", err
			}
			return mustJSON(map[string]string{"id": args.ID, "status": "updated"}), nil
		},
	})

	register(Tool{
		Name:        "delete_collection",
		Description: "Deletes a Collection and cascades all of its items. Irreversible.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{"id":{"type":"string"}},
			"required":["id"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			existing, err := s.queries.GetCollectionByID(ctx, args.ID)
			if err != nil || existing.SiteID != agent.SiteID {
				return "", errors.New("collection not found")
			}
			if err := s.queries.DeleteCollection(ctx, args.ID); err != nil {
				return "", err
			}
			return mustJSON(map[string]string{"id": args.ID, "status": "deleted"}), nil
		},
	})

	register(Tool{
		Name:        "list_collection_items",
		Description: "Lists items in a Collection. Optional filter by status (draft|published|scheduled).",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"collection_id":{"type":"string"},
				"published_only":{"type":"boolean"}
			},
			"required":["collection_id"]
		}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				CollectionID  string `json:"collection_id"`
				PublishedOnly bool   `json:"published_only"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			c, err := s.queries.GetCollectionByID(ctx, args.CollectionID)
			if err != nil || c.SiteID != agent.SiteID {
				return "", errors.New("collection not found")
			}
			var rows []store.CollectionItem
			if args.PublishedOnly {
				rows, err = s.queries.ListPublishedItemsByCollection(ctx, args.CollectionID)
			} else {
				rows, err = s.queries.ListItemsByCollection(ctx, args.CollectionID)
			}
			if err != nil {
				return "", err
			}
			return mustJSON(rows), nil
		},
	})

	register(Tool{
		Name:        "create_collection_item",
		Description: "Creates one item in a Collection. data must conform to the Collection's schema (validated server-side). status defaults to 'draft'.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"collection_id":{"type":"string"},
				"slug":{"type":"string","description":"[a-z0-9-] kebab"},
				"title":{"type":"string"},
				"data":{"type":"object"},
				"locale":{"type":"string"},
				"status":{"type":"string","enum":["draft","published","scheduled"]},
				"published_at":{"type":"string"},
				"sort_order":{"type":"number"}
			},
			"required":["collection_id","slug","title"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				CollectionID string         `json:"collection_id"`
				Slug         string         `json:"slug"`
				Title        string         `json:"title"`
				Data         map[string]any `json:"data"`
				Locale       string         `json:"locale"`
				Status       string         `json:"status"`
				PublishedAt  string         `json:"published_at"`
				SortOrder    int64          `json:"sort_order"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			c, err := s.queries.GetCollectionByID(ctx, args.CollectionID)
			if err != nil || c.SiteID != agent.SiteID {
				return "", errors.New("collection not found")
			}
			if !isCollectionSlug(args.Slug) {
				return "", errors.New("slug must be [a-z0-9-]")
			}
			args.Title = strings.TrimSpace(args.Title)
			if args.Title == "" {
				return "", errors.New("title is required")
			}
			if args.Status == "" {
				args.Status = "draft"
			}
			if args.Status == "published" && args.PublishedAt == "" {
				args.PublishedAt = time.Now().UTC().Format("2006-01-02 15:04:05")
			}
			if args.Data == nil {
				args.Data = map[string]any{}
			}
			var fields []collectionFieldDef
			_ = json.Unmarshal([]byte(c.SchemaJson), &fields)
			if err := validateItemDataAgainstSchema(fields, args.Data); err != nil {
				return "", err
			}
			dataJSON, _ := json.Marshal(args.Data)
			id := newID()
			if err := s.queries.CreateItem(ctx, store.CreateItemParams{
				ID:           id,
				CollectionID: args.CollectionID,
				SiteID:       agent.SiteID,
				Slug:         args.Slug,
				Title:        args.Title,
				DataJson:     string(dataJSON),
				Locale:       args.Locale,
				Status:       args.Status,
				PublishedAt:  args.PublishedAt,
				SortOrder:    args.SortOrder,
			}); err != nil {
				return "", err
			}
			return mustJSON(map[string]string{"id": id, "status": "created"}), nil
		},
	})

	register(Tool{
		Name:        "update_collection_item",
		Description: "Patches one Collection item.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"id":{"type":"string"},
				"slug":{"type":"string"},
				"title":{"type":"string"},
				"data":{"type":"object"},
				"locale":{"type":"string"},
				"status":{"type":"string","enum":["draft","published","scheduled"]},
				"published_at":{"type":"string"},
				"sort_order":{"type":"number"}
			},
			"required":["id"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				ID          string         `json:"id"`
				Slug        *string        `json:"slug"`
				Title       *string        `json:"title"`
				Data        map[string]any `json:"data"`
				Locale      *string        `json:"locale"`
				Status      *string        `json:"status"`
				PublishedAt *string        `json:"published_at"`
				SortOrder   *int64         `json:"sort_order"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			existing, err := s.queries.GetItemByID(ctx, args.ID)
			if err != nil || existing.SiteID != agent.SiteID {
				return "", errors.New("item not found")
			}
			c, err := s.queries.GetCollectionByID(ctx, existing.CollectionID)
			if err != nil {
				return "", errors.New("collection not found")
			}
			slug := existing.Slug
			if args.Slug != nil {
				if !isCollectionSlug(*args.Slug) {
					return "", errors.New("invalid slug")
				}
				slug = *args.Slug
			}
			title := existing.Title
			if args.Title != nil {
				title = *args.Title
			}
			dataJSON := existing.DataJson
			if args.Data != nil {
				var fields []collectionFieldDef
				_ = json.Unmarshal([]byte(c.SchemaJson), &fields)
				if err := validateItemDataAgainstSchema(fields, args.Data); err != nil {
					return "", err
				}
				b, _ := json.Marshal(args.Data)
				dataJSON = string(b)
			}
			locale := existing.Locale
			if args.Locale != nil {
				locale = *args.Locale
			}
			status := existing.Status
			if args.Status != nil {
				status = *args.Status
			}
			publishedAt := existing.PublishedAt
			if args.PublishedAt != nil {
				publishedAt = *args.PublishedAt
			}
			sortOrder := existing.SortOrder
			if args.SortOrder != nil {
				sortOrder = *args.SortOrder
			}
			if err := s.queries.UpdateItem(ctx, store.UpdateItemParams{
				ID:          args.ID,
				Slug:        slug,
				Title:       title,
				DataJson:    dataJSON,
				Locale:      locale,
				Status:      status,
				PublishedAt: publishedAt,
				SortOrder:   sortOrder,
			}); err != nil {
				return "", err
			}
			return mustJSON(map[string]string{"id": args.ID, "status": "updated"}), nil
		},
	})

	register(Tool{
		Name:        "delete_collection_item",
		Description: "Deletes one Collection item by id.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{"id":{"type":"string"}},
			"required":["id"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			existing, err := s.queries.GetItemByID(ctx, args.ID)
			if err != nil || existing.SiteID != agent.SiteID {
				return "", errors.New("item not found")
			}
			if err := s.queries.DeleteItem(ctx, args.ID); err != nil {
				return "", err
			}
			handlers.EmitWebhook(ctx, agent.SiteID, "collection_item.deleted", map[string]string{
				"id": args.ID, "site_id": agent.SiteID, "collection_id": existing.CollectionID,
			})
			return mustJSON(map[string]string{"id": args.ID, "status": "deleted"}), nil
		},
	})

	register(Tool{
		Name:        "bulk_delete_collection_items",
		Description: "Deletes up to 200 Collection items by id in one call, all inside the same collection. Cross-collection or cross-tenant ids abort the entire batch (no partial deletes). Unknown ids are silently skipped. Irreversible. Confirm with the user before calling.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"collection_id":{"type":"string"},
				"ids":{"type":"array","items":{"type":"string"}}
			},
			"required":["collection_id","ids"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				CollectionID string   `json:"collection_id"`
				IDs          []string `json:"ids"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			if len(args.IDs) == 0 {
				return "", errors.New("ids is required")
			}
			if len(args.IDs) > 200 {
				return "", errors.New("too many ids (max 200 per call)")
			}
			c, err := s.queries.GetCollectionByID(ctx, args.CollectionID)
			if err != nil || c.SiteID != agent.SiteID {
				return "", errors.New("collection not found")
			}
			toDelete := make([]string, 0, len(args.IDs))
			for _, id := range args.IDs {
				item, err := s.queries.GetItemByID(ctx, id)
				if err != nil {
					continue
				}
				if item.CollectionID != args.CollectionID {
					return "", fmt.Errorf("id %s belongs to another collection", id)
				}
				toDelete = append(toDelete, id)
			}
			deleted := 0
			for _, id := range toDelete {
				if err := s.queries.DeleteItem(ctx, id); err != nil {
					return "", err
				}
				handlers.EmitWebhook(ctx, agent.SiteID, "collection_item.deleted", map[string]string{
					"id": id, "site_id": agent.SiteID, "collection_id": args.CollectionID,
				})
				deleted++
			}
			return mustJSON(map[string]any{"status": "ok", "deleted": deleted}), nil
		},
	})

	register(Tool{
		Name:        "bulk_import_collection_items",
		Description: "Creates many Collection items in one call. items is an array of {slug,title,data,locale?,status?,published_at?,sort_order?} objects. Validates every row against the Collection schema BEFORE writing; on partial failure rolls back created rows.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"collection_id":{"type":"string"},
				"items":{"type":"array","items":{"type":"object"}}
			},
			"required":["collection_id","items"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				CollectionID string `json:"collection_id"`
				Items        []struct {
					Slug        string         `json:"slug"`
					Title       string         `json:"title"`
					Data        map[string]any `json:"data"`
					Locale      string         `json:"locale"`
					Status      string         `json:"status"`
					PublishedAt string         `json:"published_at"`
					SortOrder   int64          `json:"sort_order"`
				} `json:"items"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			if len(args.Items) == 0 {
				return "", errors.New("items array is required")
			}
			if len(args.Items) > 1000 {
				return "", errors.New("bulk import capped at 1000 items per call")
			}
			c, err := s.queries.GetCollectionByID(ctx, args.CollectionID)
			if err != nil || c.SiteID != agent.SiteID {
				return "", errors.New("collection not found")
			}
			var fields []collectionFieldDef
			_ = json.Unmarshal([]byte(c.SchemaJson), &fields)
			// Validate everything first.
			for i, it := range args.Items {
				if !isCollectionSlug(it.Slug) {
					return "", fmt.Errorf("item %d: slug must be [a-z0-9-]", i)
				}
				if strings.TrimSpace(it.Title) == "" {
					return "", fmt.Errorf("item %d: title required", i)
				}
				data := it.Data
				if data == nil {
					data = map[string]any{}
				}
				if err := validateItemDataAgainstSchema(fields, data); err != nil {
					return "", fmt.Errorf("item %d: %v", i, err)
				}
			}
			created := make([]string, 0, len(args.Items))
			for _, it := range args.Items {
				data := it.Data
				if data == nil {
					data = map[string]any{}
				}
				dataJSON, _ := json.Marshal(data)
				status := it.Status
				if status == "" {
					status = "draft"
				}
				publishedAt := it.PublishedAt
				if status == "published" && publishedAt == "" {
					publishedAt = time.Now().UTC().Format("2006-01-02 15:04:05")
				}
				id := newID()
				if err := s.queries.CreateItem(ctx, store.CreateItemParams{
					ID:           id,
					CollectionID: args.CollectionID,
					SiteID:       agent.SiteID,
					Slug:         it.Slug,
					Title:        it.Title,
					DataJson:     string(dataJSON),
					Locale:       it.Locale,
					Status:       status,
					PublishedAt:  publishedAt,
					SortOrder:    it.SortOrder,
				}); err != nil {
					// Rollback best-effort.
					for _, cid := range created {
						_ = s.queries.DeleteItem(ctx, cid)
					}
					return "", fmt.Errorf("write failed mid-batch (rolled back): %v", err)
				}
				created = append(created, id)
			}
			return mustJSON(map[string]any{
				"created": len(created),
				"ids":     created,
			}), nil
		},
	})
}

// --- helpers (kept private to the mcp package; no cross-import with handlers) ---

func isCollectionSlug(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i, c := range s {
		if i == 0 {
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
				return false
			}
			continue
		}
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return false
		}
	}
	return true
}

var allowedFieldTypes = map[string]bool{
	"text": true, "textarea": true, "richtext": true, "number": true,
	"boolean": true, "date": true, "datetime": true, "url": true, "email": true,
	"image": true, "gallery": true, "select": true, "multiselect": true,
	"reference": true, "color": true, "json": true,
}

func validateCollectionSchema(fields []collectionFieldDef) error {
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
		if !allowedFieldTypes[f.Type] {
			return fmt.Errorf("field %q: unknown type %q", f.Name, f.Type)
		}
	}
	return nil
}

func validateItemDataAgainstSchema(fields []collectionFieldDef, data map[string]any) error {
	for _, f := range fields {
		v, present := data[f.Name]
		if !present || v == nil || v == "" {
			if f.Required {
				return fmt.Errorf("field %q is required", f.Name)
			}
			continue
		}
	}
	return nil
}

func collectionToMap(c store.Collection, itemCount int64) map[string]any {
	var schema []collectionFieldDef
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
