package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/bright-interaction/slab/internal/critique"
	"github.com/bright-interaction/slab/internal/handlers"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"
)

// registerTools wires up every MCP tool. The list is curated, NOT
// auto-mirrored from /api/agent/* — adding a route there does not
// auto-expose it via MCP. To expose a tool, add it here.
//
// Privacy note (the headline rule): visitor metadata
// (/api/agent/visitor-metadata) is INTENTIONALLY ABSENT. The endpoint
// returns identified-tier PII (name, email, lifecycle, last_topic) that
// has no business flowing through an AI host's tool surface. The agent
// can SET UP analytics (flip cookieproof_enabled, set ga4_id, configure
// the personalization flag) via bulk_upsert_settings, but it cannot READ
// or WRITE the data those analytics produce. This boundary is enforced
// by the absence of the tool — there's no "skip when production" bypass.
func (s *Server) registerTools() {
	register := func(t Tool) { s.tools[t.Name] = t }

	// --- read-only context ---------------------------------------------
	register(Tool{
		Name:        "get_site_context",
		Description: "Returns the full site context payload (identical to GET /api/agent/context): site info, structure, knowledgebase, components, css_classes, constraints, architecture, design_references, pending_setup, personalization config (NOT visitor data), endpoints, block_schemas, editing_modes, security_posture, i18n, settings_catalog. Read this first on every new session.",
		InputSchema: schema(`{"type":"object","properties":{}}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, _ json.RawMessage) (string, error) {
			c, err := s.context.Build(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			return mustJSON(c), nil
		},
	})

	register(Tool{
		Name:        "get_settings_catalog",
		Description: "Returns just the settings_catalog block from /api/agent/context. Use this when you need to write a setting and want to know the exact key name, value type, valid range, current value, and whether you can write it directly or have to ask the human.",
		InputSchema: schema(`{"type":"object","properties":{}}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, _ json.RawMessage) (string, error) {
			c, err := s.context.Build(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			return mustJSON(c.SettingsCatalog), nil
		},
	})

	// --- pages ----------------------------------------------------------
	register(Tool{
		Name:        "list_pages",
		Description: "Lists every page on the site with id, slug, title, status, layout, sort_order, show_in_nav, hide_global_blocks. No PII; pages are public-facing content.",
		InputSchema: schema(`{"type":"object","properties":{}}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, _ json.RawMessage) (string, error) {
			pages, err := s.queries.ListPagesBySite(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			return mustJSON(pages), nil
		},
	})

	register(Tool{
		Name:        "create_page",
		Description: "Creates a new page. Required: title. Optional: slug (auto-derived if absent), layout (defaults to 'default'), nav_label.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"title":{"type":"string"},
				"slug":{"type":"string"},
				"layout":{"type":"string"},
				"nav_label":{"type":"string"}
			},
			"required":["title"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				Title    string `json:"title"`
				Slug     string `json:"slug"`
				Layout   string `json:"layout"`
				NavLabel string `json:"nav_label"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			if args.Title == "" {
				return "", errors.New("title is required")
			}
			if err := handlers.ValidatePageSlug(args.Slug); err != nil {
				return "", err
			}
			// SECURITY guardrail: per-site forbid_pattern / max-url-depth slug rules,
			// same as the REST handler (syntactic ValidatePageSlug alone is not enough).
			if gv, bad := s.guardrailPageSlug(ctx, agent.SiteID, args.Slug); bad {
				return mustJSON(map[string]any{"error": "guardrail_violations", "violations": gv}), nil
			}
			// Per-site page cap (DoS backstop + plan parity with migration Apply).
			if limit := s.pageQuota(ctx, agent.SiteID); limit >= 0 {
				if pages, err := s.queries.ListPagesBySite(ctx, agent.SiteID); err == nil && int64(len(pages)) >= limit {
					return "", fmt.Errorf("plan cap reached: site already has %d pages (plan caps at %d). Remove pages or upgrade the plan", len(pages), limit)
				}
			}
			if args.Layout == "" {
				args.Layout = "default"
			}
			id := newID()
			if err := s.queries.CreatePage(ctx, store.CreatePageParams{
				ID:     id,
				SiteID: agent.SiteID,
				Title:  args.Title,
				Slug:   args.Slug,
				Layout: args.Layout,
			}); err != nil {
				return "", err
			}
			page, _ := s.queries.GetPageByID(ctx, id)
			return mustJSON(page), nil
		},
	})

	register(Tool{
		Name:        "update_page",
		Description: "Updates a page identified by slug. Pass slug + any of: title, new_slug, status (draft|published), meta_title, meta_description, og_image_id, layout, show_in_nav (0|1), nav_label, no_index (0|1), canonical_url, hide_global_blocks (0|1).",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"slug":{"type":"string"},
				"title":{"type":"string"},
				"new_slug":{"type":"string"},
				"status":{"type":"string","enum":["draft","published"]},
				"meta_title":{"type":"string"},
				"meta_description":{"type":"string"},
				"og_image_id":{"type":"string"},
				"layout":{"type":"string"},
				"show_in_nav":{"type":"integer","enum":[0,1]},
				"nav_label":{"type":"string"},
				"no_index":{"type":"integer","enum":[0,1]},
				"canonical_url":{"type":"string"},
				"hide_global_blocks":{"type":"integer","enum":[0,1]}
			},
			"required":["slug"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				Slug             string  `json:"slug"`
				Title            *string `json:"title"`
				NewSlug          *string `json:"new_slug"`
				Status           *string `json:"status"`
				MetaTitle        *string `json:"meta_title"`
				MetaDescription  *string `json:"meta_description"`
				OgImageID        *string `json:"og_image_id"`
				Layout           *string `json:"layout"`
				ShowInNav        *int64  `json:"show_in_nav"`
				NavLabel         *string `json:"nav_label"`
				NoIndex          *int64  `json:"no_index"`
				CanonicalURL     *string `json:"canonical_url"`
				HideGlobalBlocks *int64  `json:"hide_global_blocks"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			page, err := s.queries.GetPageBySiteAndSlug(ctx, store.GetPageBySiteAndSlugParams{
				SiteID: agent.SiteID,
				Slug:   normalizeSlug(args.Slug),
			})
			if err != nil {
				return "", fmt.Errorf("page not found: %s", args.Slug)
			}
			params := store.UpdatePageParams{
				ID:               page.ID,
				Title:            page.Title,
				Slug:             page.Slug,
				Status:           page.Status,
				MetaTitle:        page.MetaTitle,
				MetaDescription:  page.MetaDescription,
				OgImageID:        page.OgImageID,
				Layout:           page.Layout,
				SortOrder:        page.SortOrder,
				ShowInNav:        page.ShowInNav,
				NavLabel:         page.NavLabel,
				NoIndex:          page.NoIndex,
				CanonicalUrl:     page.CanonicalUrl,
				HideGlobalBlocks: page.HideGlobalBlocks,
			}
			if args.Title != nil {
				params.Title = *args.Title
			}
			if args.NewSlug != nil {
				if err := handlers.ValidatePageSlug(*args.NewSlug); err != nil {
					return "", err
				}
				params.Slug = *args.NewSlug
			}
			if args.Status != nil {
				params.Status = *args.Status
			}
			if args.MetaTitle != nil {
				params.MetaTitle = *args.MetaTitle
			}
			if args.MetaDescription != nil {
				params.MetaDescription = *args.MetaDescription
			}
			if args.OgImageID != nil {
				params.OgImageID = *args.OgImageID
			}
			if args.Layout != nil {
				params.Layout = *args.Layout
			}
			if args.ShowInNav != nil {
				params.ShowInNav = *args.ShowInNav
			}
			if args.NavLabel != nil {
				params.NavLabel = *args.NavLabel
			}
			if args.NoIndex != nil {
				params.NoIndex = *args.NoIndex
			}
			if args.CanonicalURL != nil {
				params.CanonicalUrl = *args.CanonicalURL
			}
			if args.HideGlobalBlocks != nil {
				params.HideGlobalBlocks = *args.HideGlobalBlocks
			}
			if err := s.queries.UpdatePage(ctx, params); err != nil {
				return "", err
			}
			updated, _ := s.queries.GetPageByID(ctx, page.ID)
			return mustJSON(updated), nil
		},
	})

	register(Tool{
		Name:        "delete_page",
		Description: "Deletes a page by slug. Cascades to its blocks. Irreversible. Confirm with the user before calling.",
		InputSchema: schema(`{"type":"object","properties":{"slug":{"type":"string"}},"required":["slug"]}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				Slug string `json:"slug"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			page, err := s.queries.GetPageBySiteAndSlug(ctx, store.GetPageBySiteAndSlugParams{
				SiteID: agent.SiteID,
				Slug:   normalizeSlug(args.Slug),
			})
			if err != nil {
				return "", fmt.Errorf("page not found: %s", args.Slug)
			}
			if err := s.queries.DeletePage(ctx, page.ID); err != nil {
				return "", err
			}
			handlers.EmitWebhook(ctx, agent.SiteID, "page.deleted",
				map[string]string{"id": page.ID, "site_id": agent.SiteID, "slug": page.Slug})
			return `{"status":"deleted"}`, nil
		},
	})

	register(Tool{
		Name:        "bulk_delete_pages",
		Description: "Deletes up to 200 pages by id in one call. Cross-tenant guard: any id that belongs to another site aborts the entire batch (no partial deletes). Unknown ids are silently skipped. Irreversible. Confirm with the user before calling.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{"ids":{"type":"array","items":{"type":"string"}}},
			"required":["ids"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				IDs []string `json:"ids"`
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
			toDelete := make([]string, 0, len(args.IDs))
			for _, id := range args.IDs {
				page, err := s.queries.GetPageByID(ctx, id)
				if err != nil {
					continue
				}
				if page.SiteID != agent.SiteID {
					return "", fmt.Errorf("id %s belongs to another site", id)
				}
				toDelete = append(toDelete, id)
			}
			deleted := 0
			for _, id := range toDelete {
				if err := s.queries.DeletePage(ctx, id); err != nil {
					return "", err
				}
				handlers.EmitWebhook(ctx, agent.SiteID, "page.deleted",
					map[string]string{"id": id, "site_id": agent.SiteID})
				deleted++
			}
			return mustJSON(map[string]any{"status": "ok", "deleted": deleted}), nil
		},
	})

	// --- blocks ---------------------------------------------------------
	register(Tool{
		Name:        "list_blocks",
		Description: "Lists every block on a page identified by slug, in sort order. Returns block_type, data_json, style_json, sort_order, is_visible.",
		InputSchema: schema(`{"type":"object","properties":{"slug":{"type":"string"}},"required":["slug"]}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				Slug string `json:"slug"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			page, err := s.queries.GetPageBySiteAndSlug(ctx, store.GetPageBySiteAndSlugParams{
				SiteID: agent.SiteID,
				Slug:   normalizeSlug(args.Slug),
			})
			if err != nil {
				return "", fmt.Errorf("page not found: %s", args.Slug)
			}
			blocks, err := s.queries.ListBlocksByPage(ctx, page.ID)
			if err != nil {
				return "", err
			}
			return mustJSON(blocks), nil
		},
	})

	register(Tool{
		Name:        "create_block",
		Description: "Creates a new block on a page. Required: page_slug, block_type. Optional: data (block-type-specific JSON, see context.block_schemas), style, sort_order, force (bool: bypass strict-mode lint, default false). Strict mode (default ON) refuses writes that hit slop_term, slop_name, slop_company, slop_number, or archetype_drift findings; pass force=true to override after a human has approved the copy. Use list_blocks first to see what's already there + sort_order conventions.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"page_slug":{"type":"string"},
				"block_type":{"type":"string"},
				"data":{"type":"object"},
				"style":{"type":"object"},
				"sort_order":{"type":"integer"},
				"force":{"type":"boolean"}
			},
			"required":["page_slug","block_type"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				PageSlug  string `json:"page_slug"`
				BlockType string `json:"block_type"`
				Data      any    `json:"data"`
				Style     any    `json:"style"`
				SortOrder *int64 `json:"sort_order"`
				Force     bool   `json:"force"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			page, err := s.queries.GetPageBySiteAndSlug(ctx, store.GetPageBySiteAndSlugParams{
				SiteID: agent.SiteID,
				Slug:   normalizeSlug(args.PageSlug),
			})
			if err != nil {
				return "", fmt.Errorf("page not found: %s", args.PageSlug)
			}

			// SECURITY guardrail (forbid_pattern / allow_block_type / block-count
			// cap): the MCP write path must run the same security checks as the
			// REST handler, not just the design lint below.
			dataJSON := marshalJSON(args.Data)
			if gv, bad := s.guardrailBlockWrite(ctx, agent.SiteID, args.BlockType, dataJSON, page.ID); bad {
				return mustJSON(map[string]any{"error": "guardrail_violations", "violations": gv}), nil
			}

			fidelity := siteFidelity(ctx, s.queries, agent.SiteID)
			findings := runBlockLint(args.BlockType, page.Archetype, args.Data, fidelity)
			if !args.Force && strictDesignLintEnabled(ctx, s.queries, agent.SiteID) {
				if blockers := critique.FilterBlockingFor(findings, fidelity); len(blockers) > 0 {
					return mustJSON(map[string]any{
						"error":              "strict_lint_blocked",
						"blockers":           blockers,
						"design_warnings":    findings,
						"design_inspiration": critique.InspirationsFor(args.BlockType),
						"page_archetype":     page.Archetype,
						"hint":               "Strict design lint refused the write. Each blocker carries a fix hint; revise the data and call create_block again. Override only after a human reviewer has approved the copy by passing force=true, or write design.strict_lint=0 via bulk_upsert_settings to relax hard-blocking for this site. Slop findings block regardless of design.fidelity; if the TASTE rules feel too tight for the brief, the sanctioned relax path is design.fidelity=showcase (see design_playbook.fidelity), not disabling lint.",
					}), nil
				}
			}

			id := newID()
			var so int64
			if args.SortOrder != nil {
				so = *args.SortOrder
			}
			if err := s.queries.CreateBlock(ctx, store.CreateBlockParams{
				ID:        id,
				PageID:    page.ID,
				BlockType: args.BlockType,
				DataJson:  dataJSON,
				StyleJson: marshalJSON(args.Style),
				SortOrder: so,
				IsVisible: 1,
			}); err != nil {
				return "", err
			}
			block, _ := s.queries.GetBlockByID(ctx, id)
			inspirations := critique.InspirationsFor(args.BlockType)
			return mustJSON(map[string]any{
				"block":              block,
				"design_warnings":    findings,
				"design_inspiration": inspirations,
				"page_archetype":     page.Archetype,
				"hint":               "design_warnings = synchronous write-time lint (gap 4 + 6): a hero with no hero_graphic, an overly long headline, slop terms in copy, custom block duplicate eyebrow, archetype drift when the page is locked. Each finding carries a fix hint. design_inspiration = curated 2-3 design-corpus references for this block_type (gap 5); read them via search_design_corpus when designing the next variant. page_archetype = the lock the page carries (set via set_page_archetype); empty = no lock.",
			}), nil
		},
	})

	register(Tool{
		Name:        "update_block",
		Description: "Updates a block by id. Pass block_id + any of: block_type, data, style, is_visible (0|1), sort_order, force (bool: bypass strict-mode lint, default false). Strict mode (default ON) refuses writes that hit slop_term, slop_name, slop_company, slop_number, or archetype_drift findings. Use list_blocks to find the block_id.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"block_id":{"type":"string"},
				"block_type":{"type":"string"},
				"data":{"type":"object"},
				"style":{"type":"object"},
				"is_visible":{"type":"integer","enum":[0,1]},
				"sort_order":{"type":"integer"},
				"force":{"type":"boolean"}
			},
			"required":["block_id"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				BlockID   string  `json:"block_id"`
				BlockType *string `json:"block_type"`
				Data      any     `json:"data"`
				Style     any     `json:"style"`
				IsVisible *int64  `json:"is_visible"`
				SortOrder *int64  `json:"sort_order"`
				Force     bool    `json:"force"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			existing, err := s.queries.GetBlockByID(ctx, args.BlockID)
			if err != nil {
				return "", fmt.Errorf("block not found: %s", args.BlockID)
			}
			page, err := s.queries.GetPageByID(ctx, existing.PageID)
			if err != nil || page.SiteID != agent.SiteID {
				return "", fmt.Errorf("block does not belong to this site")
			}
			bt := existing.BlockType
			if args.BlockType != nil {
				bt = *args.BlockType
			}
			data := existing.DataJson
			if args.Data != nil {
				data = marshalJSON(args.Data)
			}

			// SECURITY guardrail (forbid_pattern / allow_block_type) on the
			// post-update block_type+data, same as the REST handler. pageID
			// empty: an update does not change the block count.
			if gv, bad := s.guardrailBlockWrite(ctx, agent.SiteID, bt, data, ""); bad {
				return mustJSON(map[string]any{"error": "guardrail_violations", "violations": gv}), nil
			}

			// Run lint against the incoming data BEFORE the UpdateBlock call so
			// strict-mode blockers can refuse to overwrite a clean block with
			// slop. Lint runs against the merged post-update state.
			fidelity := siteFidelity(ctx, s.queries, agent.SiteID)
			findings := runBlockLintFromJSON(bt, page.Archetype, data, fidelity)
			if !args.Force && strictDesignLintEnabled(ctx, s.queries, agent.SiteID) {
				if blockers := critique.FilterBlockingFor(findings, fidelity); len(blockers) > 0 {
					return mustJSON(map[string]any{
						"error":              "strict_lint_blocked",
						"blockers":           blockers,
						"design_warnings":    findings,
						"design_inspiration": critique.InspirationsFor(bt),
						"page_archetype":     page.Archetype,
						"hint":               "Strict design lint refused the update. Each blocker carries a fix hint; revise the data and call update_block again. Override only after human approval by passing force=true, or write design.strict_lint=0 via bulk_upsert_settings to relax hard-blocking for this site. Slop findings block regardless of design.fidelity; if the TASTE rules feel too tight for the brief, the sanctioned relax path is design.fidelity=showcase (see design_playbook.fidelity), not disabling lint.",
					}), nil
				}
			}

			style := existing.StyleJson
			if args.Style != nil {
				style = marshalJSON(args.Style)
			}
			vis := existing.IsVisible
			if args.IsVisible != nil {
				vis = *args.IsVisible
			}
			so := existing.SortOrder
			if args.SortOrder != nil {
				so = *args.SortOrder
			}
			if err := s.queries.UpdateBlock(ctx, store.UpdateBlockParams{
				ID: args.BlockID, BlockType: bt, SortOrder: so, DataJson: data, StyleJson: style, IsVisible: vis,
			}); err != nil {
				return "", err
			}
			block, _ := s.queries.GetBlockByID(ctx, args.BlockID)
			return mustJSON(map[string]any{
				"block":              block,
				"design_warnings":    findings,
				"design_inspiration": critique.InspirationsFor(bt),
				"page_archetype":     page.Archetype,
				"hint":               "design_warnings = synchronous design lint (gap 4 + 6); design_inspiration = curated corpus references for this block_type (gap 5); page_archetype = current lock (set via set_page_archetype). See create_block hint for the full schema.",
			}), nil
		},
	})

	register(Tool{
		Name:          "delete_block",
		Description:   "Deletes a block by id. Confirm with user first.",
		InputSchema:   schema(`{"type":"object","properties":{"block_id":{"type":"string"}},"required":["block_id"]}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				BlockID string `json:"block_id"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			existing, err := s.queries.GetBlockByID(ctx, args.BlockID)
			if err != nil {
				return "", fmt.Errorf("block not found: %s", args.BlockID)
			}
			page, err := s.queries.GetPageByID(ctx, existing.PageID)
			if err != nil || page.SiteID != agent.SiteID {
				return "", fmt.Errorf("block does not belong to this site")
			}
			// require_block guardrail: don't let the MCP path delete a page's
			// last required block (REST DeleteBlock already enforces this).
			if gv, bad := s.guardrailRequiredBlocks(ctx, agent.SiteID, page.Slug, page.ID, args.BlockID); bad {
				return mustJSON(map[string]any{"error": "guardrail_violations", "violations": gv}), nil
			}
			if err := s.queries.DeleteBlock(ctx, args.BlockID); err != nil {
				return "", err
			}
			return `{"status":"deleted"}`, nil
		},
	})

	// --- branding -------------------------------------------------------
	register(Tool{
		Name:        "get_branding",
		Description: "Returns the site branding (palette, fonts, hero defaults). Mirrors GET /api/agent/branding.",
		InputSchema: schema(`{"type":"object","properties":{}}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, _ json.RawMessage) (string, error) {
			site, err := s.queries.GetSiteByID(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			return mustJSON(map[string]any{
				"primary_color":   site.PrimaryColor,
				"secondary_color": site.SecondaryColor,
				"accent_color":    site.AccentColor,
				"text_color":      site.TextColor,
				"bg_color":        site.BgColor,
				"font_heading":    site.FontHeading,
				"font_body":       site.FontBody,
			}), nil
		},
	})

	register(Tool{
		Name:          "update_branding",
		Description:   "Partial update of the site branding row. Pass any of: primary_color, secondary_color, bg_color, text_color, surface_color, border_color, muted_color, accent_color, on_primary_color (all #RRGGBB or empty), font_heading, font_body (e.g. 'Space Grotesk', 'Inter'), meta_title, meta_description, og_image_id, favicon_id, lang. Mirrors PATCH /api/agent/branding.",
		RequiresWrite: true,
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"primary_color":{"type":"string"},
				"secondary_color":{"type":"string"},
				"bg_color":{"type":"string"},
				"text_color":{"type":"string"},
				"surface_color":{"type":"string"},
				"border_color":{"type":"string"},
				"muted_color":{"type":"string"},
				"accent_color":{"type":"string"},
				"on_primary_color":{"type":"string"},
				"font_heading":{"type":"string"},
				"font_body":{"type":"string"},
				"meta_title":{"type":"string"},
				"meta_description":{"type":"string"},
				"og_image_id":{"type":"string"},
				"favicon_id":{"type":"string"},
				"lang":{"type":"string"}
			}
		}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				PrimaryColor    *string `json:"primary_color"`
				SecondaryColor  *string `json:"secondary_color"`
				BgColor         *string `json:"bg_color"`
				TextColor       *string `json:"text_color"`
				SurfaceColor    *string `json:"surface_color"`
				BorderColor     *string `json:"border_color"`
				MutedColor      *string `json:"muted_color"`
				AccentColor     *string `json:"accent_color"`
				OnPrimaryColor  *string `json:"on_primary_color"`
				FontHeading     *string `json:"font_heading"`
				FontBody        *string `json:"font_body"`
				MetaTitle       *string `json:"meta_title"`
				MetaDescription *string `json:"meta_description"`
				OgImageID       *string `json:"og_image_id"`
				FaviconID       *string `json:"favicon_id"`
				Lang            *string `json:"lang"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			row, err := s.queries.GetSiteByID(ctx, agent.SiteID)
			if err != nil {
				return "", fmt.Errorf("site not found")
			}
			hexRE := regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
			check := func(name string, v *string) error {
				if v == nil || *v == "" {
					return nil
				}
				if !hexRE.MatchString(*v) {
					return fmt.Errorf("%s must be #RRGGBB hex or empty", name)
				}
				return nil
			}
			for _, c := range []struct {
				name string
				v    *string
			}{
				{"primary_color", args.PrimaryColor}, {"secondary_color", args.SecondaryColor},
				{"bg_color", args.BgColor}, {"text_color", args.TextColor},
				{"surface_color", args.SurfaceColor}, {"border_color", args.BorderColor},
				{"muted_color", args.MutedColor}, {"accent_color", args.AccentColor},
				{"on_primary_color", args.OnPrimaryColor},
			} {
				if err := check(c.name, c.v); err != nil {
					return "", err
				}
			}
			params := store.UpdateSiteParams{
				Name: row.Name, Slug: row.Slug, Domain: row.Domain, Status: row.Status,
				PrimaryColor: row.PrimaryColor, SecondaryColor: row.SecondaryColor,
				BgColor: row.BgColor, TextColor: row.TextColor,
				SurfaceColor: row.SurfaceColor, BorderColor: row.BorderColor,
				MutedColor: row.MutedColor, AccentColor: row.AccentColor,
				OnPrimaryColor: row.OnPrimaryColor,
				FontHeading:    row.FontHeading, FontBody: row.FontBody,
				MetaTitle: row.MetaTitle, MetaDescription: row.MetaDescription,
				OgImageID: row.OgImageID, FaviconID: row.FaviconID,
				Ga4ID: row.Ga4ID, UmamiID: row.UmamiID, UmamiUrl: row.UmamiUrl,
				CookieproofDomain: row.CookieproofDomain, Lang: row.Lang,
				ID: row.ID,
			}
			apply := func(field *string, target *string) {
				if field != nil {
					*target = *field
				}
			}
			apply(args.PrimaryColor, &params.PrimaryColor)
			apply(args.SecondaryColor, &params.SecondaryColor)
			apply(args.BgColor, &params.BgColor)
			apply(args.TextColor, &params.TextColor)
			apply(args.SurfaceColor, &params.SurfaceColor)
			apply(args.BorderColor, &params.BorderColor)
			apply(args.MutedColor, &params.MutedColor)
			apply(args.AccentColor, &params.AccentColor)
			apply(args.OnPrimaryColor, &params.OnPrimaryColor)
			apply(args.FontHeading, &params.FontHeading)
			apply(args.FontBody, &params.FontBody)
			apply(args.MetaTitle, &params.MetaTitle)
			apply(args.MetaDescription, &params.MetaDescription)
			apply(args.OgImageID, &params.OgImageID)
			apply(args.FaviconID, &params.FaviconID)
			apply(args.Lang, &params.Lang)
			if err := s.queries.UpdateSite(ctx, params); err != nil {
				return "", fmt.Errorf("failed to update branding")
			}
			updated, _ := s.queries.GetSiteByID(ctx, agent.SiteID)
			return mustJSON(map[string]any{
				"primary_color": updated.PrimaryColor, "secondary_color": updated.SecondaryColor,
				"accent_color": updated.AccentColor, "text_color": updated.TextColor,
				"bg_color": updated.BgColor, "font_heading": updated.FontHeading,
				"font_body": updated.FontBody,
			}), nil
		},
	})

	// --- profile --------------------------------------------------------
	register(Tool{
		Name:        "get_profile",
		Description: "Returns the site_profiles row (business_name, registration_nr, country, contact_email, contact_phone, privacy_email, security_email, address). Mirrors GET /api/agent/profile.",
		InputSchema: schema(`{"type":"object","properties":{}}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, _ json.RawMessage) (string, error) {
			profile, _ := s.queries.GetSiteProfile(ctx, agent.SiteID)
			return mustJSON(profile), nil
		},
	})

	register(Tool{
		Name:        "update_profile",
		Description: "Updates the site_profiles row. Pass any of: business_name, registration_nr, country, contact_email, contact_phone, privacy_email, security_email, address_line1, address_line2, city, postal_code. Drives security.txt + humans.txt + JSON-LD Organization schema.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"business_name":{"type":"string"},
				"registration_nr":{"type":"string"},
				"country":{"type":"string"},
				"contact_email":{"type":"string"},
				"contact_phone":{"type":"string"},
				"privacy_email":{"type":"string"},
				"security_email":{"type":"string"},
				"address_line1":{"type":"string"},
				"address_line2":{"type":"string"},
				"city":{"type":"string"},
				"postal_code":{"type":"string"}
			}
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			existing, _ := s.queries.GetSiteProfile(ctx, agent.SiteID)
			var args struct {
				BusinessName   *string `json:"business_name"`
				RegistrationNr *string `json:"registration_nr"`
				Country        *string `json:"country"`
				ContactEmail   *string `json:"contact_email"`
				ContactPhone   *string `json:"contact_phone"`
				PrivacyEmail   *string `json:"privacy_email"`
				SecurityEmail  *string `json:"security_email"`
				AddressLine1   *string `json:"address_line1"`
				AddressLine2   *string `json:"address_line2"`
				City           *string `json:"city"`
				PostalCode     *string `json:"postal_code"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			params := store.UpsertSiteProfileParams{
				ID:             existing.ID,
				SiteID:         agent.SiteID,
				BusinessName:   existing.BusinessName,
				RegistrationNr: existing.RegistrationNr,
				Country:        existing.Country,
				ContactEmail:   existing.ContactEmail,
				ContactPhone:   existing.ContactPhone,
				PrivacyEmail:   existing.PrivacyEmail,
				SecurityEmail:  existing.SecurityEmail,
				AddressLine1:   existing.AddressLine1,
				AddressLine2:   existing.AddressLine2,
				City:           existing.City,
				PostalCode:     existing.PostalCode,
			}
			if params.ID == "" {
				params.ID = newID()
			}
			if args.BusinessName != nil {
				params.BusinessName = *args.BusinessName
			}
			if args.RegistrationNr != nil {
				params.RegistrationNr = *args.RegistrationNr
			}
			if args.Country != nil {
				params.Country = *args.Country
			}
			if args.ContactEmail != nil {
				params.ContactEmail = *args.ContactEmail
			}
			if args.ContactPhone != nil {
				params.ContactPhone = *args.ContactPhone
			}
			if args.PrivacyEmail != nil {
				params.PrivacyEmail = *args.PrivacyEmail
			}
			if args.SecurityEmail != nil {
				params.SecurityEmail = *args.SecurityEmail
			}
			if args.AddressLine1 != nil {
				params.AddressLine1 = *args.AddressLine1
			}
			if args.AddressLine2 != nil {
				params.AddressLine2 = *args.AddressLine2
			}
			if args.City != nil {
				params.City = *args.City
			}
			if args.PostalCode != nil {
				params.PostalCode = *args.PostalCode
			}
			if err := s.queries.UpsertSiteProfile(ctx, params); err != nil {
				return "", err
			}
			profile, _ := s.queries.GetSiteProfile(ctx, agent.SiteID)
			return mustJSON(profile), nil
		},
	})

	// --- settings -------------------------------------------------------
	register(Tool{
		Name:        "list_settings",
		Description: "Returns every settings row for the site. Use get_settings_catalog instead when you need to know what each key MEANS — list_settings is just the raw rows.",
		InputSchema: schema(`{"type":"object","properties":{"category":{"type":"string","description":"optional category filter (general|seo|analytics|security|server)"}}}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				Category string `json:"category"`
			}
			_ = json.Unmarshal(raw, &args)
			if args.Category != "" {
				rows, err := s.queries.ListSettingsByCategory(ctx, store.ListSettingsByCategoryParams{
					SiteID: agent.SiteID, Category: args.Category,
				})
				if err != nil {
					return "", err
				}
				return mustJSON(s.maskSecretSettings(rows)), nil
			}
			rows, err := s.queries.ListSettingsBySite(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			return mustJSON(s.maskSecretSettings(rows)), nil
		},
	})

	register(Tool{
		Name: "bulk_upsert_settings",
		Description: "Writes one or more settings rows. Writable categories: general / seo / analytics / search / design (design carries the fidelity dial + strict-lint toggle); security / allowed-scripts stay admin-only (pass them and they're rejected with a clean error). Each item validated against the catalog (enum guards, range checks, format checks). Read get_settings_catalog first so you write the right shape.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"items":{
					"type":"array",
					"items":{
						"type":"object",
						"properties":{
							"category":{"type":"string","enum":["general","seo","analytics","search","design"]},
							"key":{"type":"string"},
							"value":{"type":"string"}
						},
						"required":["category","key","value"]
					}
				}
			},
			"required":["items"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				Items []struct {
					Category string `json:"category"`
					Key      string `json:"key"`
					Value    string `json:"value"`
				} `json:"items"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			if len(args.Items) == 0 {
				return "", errors.New("items must not be empty")
			}
			rejected := []string{}
			// Validate first; bail before any write so the batch is atomic.
			for _, it := range args.Items {
				if !isAgentWritableCategory(it.Category) {
					rejected = append(rejected, it.Category+"."+it.Key)
					continue
				}
				if err := validateSettingValue(it.Category, it.Key, it.Value); err != nil {
					return "", err
				}
			}
			for _, it := range args.Items {
				if !isAgentWritableCategory(it.Category) {
					continue
				}
				_ = s.queries.UpsertSetting(ctx, store.UpsertSettingParams{
					ID: newID(), SiteID: agent.SiteID, Category: it.Category, Key: it.Key, Value: it.Value,
				})
			}
			rows, _ := s.queries.ListSettingsBySite(ctx, agent.SiteID)
			return mustJSON(map[string]any{
				"settings":              s.maskSecretSettings(rows),
				"rejected_admin_only":   rejected,
				"writable_categories":   []string{"analytics", "general", "seo"},
			}), nil
		},
	})

	// --- security posture (READ-ONLY; never expose write here) ----------
	register(Tool{
		Name:        "get_security_posture",
		Description: "Returns resolved security headers (CSP, HSTS, X-Frame-Options, COOP/CORP/COEP, Referrer-Policy, Permissions-Policy) the next build will emit, plus the trusted-domain list grouped by kind (script/frame/image/media/connect) and the auto-generated files inventory. Read-only — security writes stay admin-only because adding a CSP host widens the attack surface.",
		InputSchema: schema(`{"type":"object","properties":{}}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, _ json.RawMessage) (string, error) {
			c, err := s.context.Build(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			return mustJSON(c.SecurityPosture), nil
		},
	})

	// --- media ----------------------------------------------------------
	register(Tool{
		Name:        "list_media",
		Description: "Lists every media row (id, filename, alt_text, mime_type, width, height, folder). Optional folder filter.",
		InputSchema: schema(`{"type":"object","properties":{"folder":{"type":"string"}}}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			rows, err := s.queries.ListMediaBySite(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			var args struct {
				Folder string `json:"folder"`
			}
			_ = json.Unmarshal(raw, &args)
			if args.Folder != "" {
				out := rows[:0]
				for _, m := range rows {
					if m.Folder == args.Folder {
						out = append(out, m)
					}
				}
				rows = out
			}
			return mustJSON(rows), nil
		},
	})

	register(Tool{
		Name:        "list_media_folders",
		Description: "Lists media folders with item counts. The 'brand' folder is system-managed and reserved for favicon / apple-touch-icon / OG image; create your own folders for content.",
		InputSchema: schema(`{"type":"object","properties":{}}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, _ json.RawMessage) (string, error) {
			folders, err := s.queries.ListMediaFoldersBySite(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			return mustJSON(folders), nil
		},
	})

	// --- build + evaluation --------------------------------------------
	register(Tool{
		Name:          "trigger_build",
		Description:   "Triggers an async build of the current site. Returns build_id; poll get_build_status until status==success or failed, then call get_evaluation(build_id) to read the eval results. After success, call screenshot to see the rendered output.",
		InputSchema:   schema(`{"type":"object","properties":{}}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, _ json.RawMessage) (string, error) {
			if s.builds == nil {
				return "", errors.New("build trigger not wired (server constructed without BuildTrigger)")
			}
			id, err := s.builds.StartBuild(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			return mustJSON(map[string]string{"build_id": id, "status": "building"}), nil
		},
	})

	register(Tool{
		Name:          "screenshot",
		Description:   "Captures a headless Chromium screenshot of a deployed page and returns base64-encoded PNG. Use after trigger_build success to visually verify the rendered output. The agent should decode the image and reason about pixels: that's how you tell whether a layout looks right vs. needs another iteration. Defaults: 1440x900 viewport, full_page=true, wait_ms=800. SSRF-locked to the configured public domain(s) for this deployment; loopback hosts always pass.",
		InputSchema:   schema(`{"type":"object","properties":{"url":{"type":"string","description":"Full URL of the deployed page to screenshot (must be on the configured public domain or loopback)."},"viewport_width":{"type":"integer","minimum":320,"maximum":3840},"viewport_height":{"type":"integer","minimum":240,"maximum":2160},"full_page":{"type":"boolean","description":"Capture entire scrollable page; false = only the viewport."},"wait_ms":{"type":"integer","minimum":0,"maximum":10000,"description":"Ms to wait after navigation before capturing (lets fonts + JS islands settle)."}},"required":["url"]}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args handlers.ScreenshotRequest
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			res, err := handlers.CaptureScreenshot(ctx, args)
			if err != nil {
				return "", err
			}
			return mustJSON(res), nil
		},
	})

	register(Tool{
		Name:        "get_build_status",
		Description: "Returns build status (building|success|failed) plus duration_ms, pages_built, and any error message.",
		InputSchema: schema(`{"type":"object","properties":{"build_id":{"type":"string"}},"required":["build_id"]}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			if s.builds == nil {
				return "", errors.New("build trigger not wired")
			}
			var args struct {
				BuildID string `json:"build_id"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			state, err := s.builds.GetBuildState(ctx, args.BuildID, agent.SiteID)
			if err != nil {
				return "", err
			}
			return mustJSON(state), nil
		},
	})

	register(Tool{
		Name:        "get_evaluation",
		Description: "Returns evaluation results for a build (security, seo, a11y, privacy, perf categories, with per-check rows). Use after get_build_status reports success.",
		InputSchema: schema(`{"type":"object","properties":{"build_id":{"type":"string"}},"required":["build_id"]}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				BuildID string `json:"build_id"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			// Ownership check via deployments table; ListEvaluationsByBuild
			// itself doesn't carry siteID. We rely on the build trigger's
			// own ownership check at status-fetch time as the gate; eval
			// rows themselves are inert.
			if s.builds != nil {
				if _, err := s.builds.GetBuildState(ctx, args.BuildID, agent.SiteID); err != nil {
					return "", err
				}
			}
			evals, err := s.queries.ListEvaluationsByBuild(ctx, args.BuildID)
			if err != nil {
				return "", err
			}
			return mustJSON(evals), nil
		},
	})

	// Operator-mutable surfaces (components, css_classes, knowledgebase,
	// guardrails, allowed_scripts, fonts read+delete, design_references)
	// plus get_capabilities live in tools_extra.go to keep this file
	// focused on the original page/block + build/eval flow.
	s.registerExtraTools()

	// AI-native Custom Collections (Sprint 4, 2026-05-04). 9 tools
	// for Collection + item CRUD plus bulk import. Lives in
	// tools_collections.go.
	s.registerCollectionTools()

	// Migration system (Sprint 4.5, 2026-05-06). 13 tools wrapping the
	// crawl + plan + apply (with upsert) + verify-live + missing-urls
	// surface so the AI can drive an end-to-end CMS migration through
	// MCP without dropping to raw HTTP. Lives in tools_migrations.go.
	s.registerMigrationTools()

	// Clarifications (Sprint 6 of the design-quality push, 2026-05-21).
	// 3 tools: request_clarification, get_clarification,
	// list_pending_clarifications. Backs the dashboard inbox loop so the
	// agent can ask the human when a decision would otherwise force a
	// guess. Closes the "agent guesses + ships wrong + corrects after"
	// loop that costs the one-shot wow. Lives in tools_clarifications.go.
	s.registerClarificationTools()

	// 3 tools: list_revisions, get_revision, restore_revision. Sprint 1
	// of the WP/Webflow replacement roadmap. Lets the agent (and the
	// human via dashboard) roll back any page or block to a prior
	// state. Restore is non-destructive; history is append-only. Lives
	// in tools_revisions.go.
	s.registerRevisionTools()

	// 9 catalog tools + 4 discount-code tools: Sprint 2 slice A of
	// the WP/Webflow roadmap. Lets the agent manage the storefront
	// catalog end to end (products, variants, inventory adjustments,
	// promo codes) ahead of slice B's orders + Mollie checkout. Lives
	// in tools_products.go.
	s.registerProductTools()
	s.registerDiscountCodeTools()

	// 4 order tools: Sprint 2 slice B of the WP/Webflow roadmap.
	// Order pipeline management (list, get, update_status, refund)
	// for post-checkout workflows. Lives in tools_orders.go.
	s.registerOrderTools()

	// 1 translation tool: Sprint 3 slice B of the WP/Webflow roadmap.
	// translate_entity is BYOAI-shaped: GET mode returns source +
	// existing overlays, PUT mode upserts a page_locale + N
	// block_locale rows atomically. Lives in tools_translate.go.
	s.registerTranslateTools()

	// 2 apps tools: Sprint 4 slice A. list_apps_marketplace (read
	// the cross-tenant catalogue) + list_installed_apps (read the
	// per-site install ledger with credentials_set metadata, never
	// the values). Slice B adds use_app for upstream MCP proxy
	// calls. Lives in tools_apps.go.
	s.registerAppsTools()

	// Synchronous design lint (gap 4 of 6, 2026-05-21). lint_block runs
	// the same rule set create_block + update_block embed in their
	// responses, but standalone so the agent can vet variants before
	// committing.
	s.registerLintTools()

	// Page archetype lock (gap 6 of 6, 2026-05-21). set_page_archetype
	// + get_page_archetype tools wire the playbook's vibe_archetypes
	// catalog to a per-page lock; every create_block / update_block on
	// a locked page runs the archetype_drift check.
	s.registerArchetypeTools()

	// Conversion goals + events (Phase 31.1.1, 2026-05-06). 6 tools
	// wrapping the goals admin REST surface plus a server-side
	// track_event for back-channel conversions. Lives in
	// tools_goals.go.
	s.registerGoalTools()
}

// --- helpers (shared across tool handlers) -------------------------------

func schema(s string) json.RawMessage { return json.RawMessage(s) }

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func marshalJSON(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func normalizeSlug(s string) string {
	if s == "" || s == "/" {
		return "/"
	}
	if !strings.HasPrefix(s, "/") {
		return "/" + s
	}
	return s
}
