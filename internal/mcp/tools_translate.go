package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/bright-interaction/slab/internal/handlers"
	authmw "github.com/bright-interaction/slab/internal/middleware"
)

// registerTranslateTools wires the Sprint 3 slice B translate_entity
// MCP tool. The tool is BYOAI-shaped: it never calls a model itself.
// Two modes determined by the presence of the `translations` field:
//
//   - GET mode (translations omitted): returns the source page +
//     every block + any existing overlay rows for the target locale.
//     The agent uses this context to author a translation in its own
//     model session, then calls translate_entity again with the
//     translations parameter to write everything atomically.
//
//   - PUT mode (translations provided): upserts the page_locale row
//
//   - each named block_locale row in one logical operation. JSON
//     in block data_json is pre-validated; an invalid payload aborts
//     the whole call so partial writes never leak through.
//
// Marked RequiresWrite=true because PUT mode mutates state. GET-mode
// callers without write capability get rejected at the capability
// gate; that's fine, the read can also be assembled from
// list_blocks + list_page_locales + list_block_locales for read-only
// keys.
//
// Sprint 3 of the WP/Webflow replacement roadmap (see
// atomicsite/docs/ROADMAP.md).
func (s *Server) registerTranslateTools() {
	register := func(t Tool) { s.tools[t.Name] = t }

	register(Tool{
		Name:        "translate_entity",
		Description: "Translation surface for the multilingual page + block system. ONE call covers the round trip: read source content + existing overlays, then write the translated page_locale + block_locale rows atomically. Mode is determined by the `translations` parameter. Required: entity_type ('page'), entity_id (the page ID), target_locale (BCP-47 short form: 'sv', 'en-gb'). Optional: translations object (when omitted, the tool returns source content for translation; when present, the tool upserts the locale rows). translations.page is a PageTranslationOverlay {slug_override, title, meta_title, meta_description, status='draft'|'published'|'archived'}; translations.blocks is an array of {block_id, data_json, is_visible?}. data_json must be valid JSON matching the base block's shape. Returns the translation context on read; returns {success:true, page_id, target_locale, page_status} on write. Currently only entity_type='page' is supported; block-only translation is reachable via this same tool by passing the page that owns the block.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"entity_type":{"type":"string","enum":["page"]},
				"entity_id":{"type":"string"},
				"target_locale":{"type":"string"},
				"translations":{
					"type":"object",
					"properties":{
						"page":{
							"type":"object",
							"properties":{
								"slug_override":{"type":"string"},
								"title":{"type":"string"},
								"meta_title":{"type":"string"},
								"meta_description":{"type":"string"},
								"status":{"type":"string","enum":["draft","published","archived"]}
							}
						},
						"blocks":{
							"type":"array",
							"items":{
								"type":"object",
								"required":["block_id","data_json"],
								"properties":{
									"block_id":{"type":"string"},
									"data_json":{"type":"string"},
									"is_visible":{"type":"boolean"}
								}
							}
						}
					}
				}
			},
			"required":["entity_type","entity_id","target_locale"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			if s.locales == nil {
				return "", errors.New("translate_entity not configured on this server (locales handler unset)")
			}
			var args struct {
				EntityType   string `json:"entity_type"`
				EntityID     string `json:"entity_id"`
				TargetLocale string `json:"target_locale"`
				Translations *struct {
					Page   *handlers.PageTranslationOverlay   `json:"page"`
					Blocks []handlers.BlockTranslationOverlay `json:"blocks"`
				} `json:"translations"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			args.EntityType = strings.ToLower(strings.TrimSpace(args.EntityType))
			if args.EntityType != "page" {
				return "", errors.New("entity_type must be 'page' in v1 (block-only translation reachable by translating the owning page)")
			}
			if args.EntityID == "" {
				return "", errors.New("entity_id required")
			}
			if args.Translations == nil {
				// GET mode: return source content + any existing overlays.
				ctxOut, err := s.locales.GetTranslationContextForAgent(ctx, agent.SiteID, args.EntityID, args.TargetLocale)
				if err != nil {
					return "", err
				}
				return mustJSON(map[string]any{
					"mode":    "read",
					"context": ctxOut,
				}), nil
			}
			// PUT mode: apply the supplied translations atomically.
			pageOverlay := handlers.PageTranslationOverlay{}
			if args.Translations.Page != nil {
				pageOverlay = *args.Translations.Page
			}
			if err := s.locales.ApplyTranslationForAgent(ctx, agent.SiteID, args.EntityID, args.TargetLocale, pageOverlay, args.Translations.Blocks); err != nil {
				return "", err
			}
			return mustJSON(map[string]any{
				"success":       true,
				"page_id":       args.EntityID,
				"target_locale": strings.ToLower(strings.TrimSpace(args.TargetLocale)),
				"page_status":   strings.ToLower(strings.TrimSpace(pageOverlay.Status)),
				"block_count":   len(args.Translations.Blocks),
			}), nil
		},
	})
}
