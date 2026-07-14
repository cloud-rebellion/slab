package mcp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	agentpkg "github.com/bright-interaction/slab/internal/agent"
	"github.com/bright-interaction/slab/internal/handlers"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"
)

// registerExtraTools wires the operator-mutable surfaces beyond pages
// and blocks: components, css_classes, knowledgebase, guardrails,
// allowed_scripts, fonts (read+delete; upload still goes through the
// REST endpoint because it is multipart), design_references, plus
// get_capabilities for self-introspection.
//
// Each tool is a thin pass-through to the existing sqlc query the REST
// handler also calls. Same validation, same constraints, same audit
// shape. The only thing MCP adds is the auth + write-cap gating that
// every tool already runs through.
func (s *Server) registerExtraTools() {
	register := func(t Tool) { s.tools[t.Name] = t }

	// --- components ------------------------------------------------------

	register(Tool{
		Name:        "list_components",
		Description: "Lists every reusable component registered for the site (Astro snippets, Svelte fragments, etc.). Components are template blocks the agent can compose into pages without re-authoring CSS.",
		InputSchema: schema(`{"type":"object","properties":{}}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, _ json.RawMessage) (string, error) {
			rows, err := s.queries.ListComponentsBySite(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			return mustJSON(rows), nil
		},
	})

	register(Tool{
		Name:        "create_component",
		Description: "Registers a new reusable component. Required: name, template (Astro/Svelte source). Optional: category, props_schema (JSON), css_classes (space-separated names), usage_note.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"name":{"type":"string"},
				"template":{"type":"string"},
				"category":{"type":"string"},
				"props_schema":{"type":"string"},
				"css_classes":{"type":"string"},
				"usage_note":{"type":"string"}
			},
			"required":["name","template"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				Name        string `json:"name"`
				Template    string `json:"template"`
				Category    string `json:"category"`
				PropsSchema string `json:"props_schema"`
				CssClasses  string `json:"css_classes"`
				UsageNote   string `json:"usage_note"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			if strings.TrimSpace(args.Name) == "" || strings.TrimSpace(args.Template) == "" {
				return "", errors.New("name and template are required")
			}
			// Path-traversal guard: the name becomes a filename under src/components/.
			if err := handlers.ValidateComponentName(args.Name); err != nil {
				return "", err
			}
			// Per-site component cap (runaway-loop / DoS backstop on the MCP path).
			if rows, err := s.queries.ListComponentsBySite(ctx, agent.SiteID); err == nil && len(rows) >= maxComponentsPerSite {
				return "", fmt.Errorf("component cap reached (%d per site). Remove unused components before adding more", maxComponentsPerSite)
			}
			id := newID()
			if err := s.queries.CreateComponent(ctx, store.CreateComponentParams{
				ID:          id,
				SiteID:      agent.SiteID,
				Name:        args.Name,
				Category:    args.Category,
				Template:    args.Template,
				PropsSchema: args.PropsSchema,
				CssClasses:  args.CssClasses,
				UsageNote:   args.UsageNote,
			}); err != nil {
				return "", err
			}
			return mustJSON(map[string]string{"id": id, "status": "created"}), nil
		},
	})

	register(Tool{
		Name:        "update_component",
		Description: "Updates a registered component. Required: id. All other fields are optional but the API replaces the whole row, so pass the existing values for fields you do not want to change.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"id":{"type":"string"},
				"name":{"type":"string"},
				"template":{"type":"string"},
				"category":{"type":"string"},
				"props_schema":{"type":"string"},
				"css_classes":{"type":"string"},
				"usage_note":{"type":"string"}
			},
			"required":["id","name","template"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				ID          string `json:"id"`
				Name        string `json:"name"`
				Template    string `json:"template"`
				Category    string `json:"category"`
				PropsSchema string `json:"props_schema"`
				CssClasses  string `json:"css_classes"`
				UsageNote   string `json:"usage_note"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			existing, err := s.queries.GetComponentByID(ctx, args.ID)
			if err != nil {
				return "", fmt.Errorf("component %s not found", args.ID)
			}
			if existing.SiteID != agent.SiteID {
				return "", errors.New("component belongs to another site")
			}
			if err := s.queries.UpdateComponent(ctx, store.UpdateComponentParams{
				ID:          args.ID,
				Name:        args.Name,
				Category:    args.Category,
				Template:    args.Template,
				PropsSchema: args.PropsSchema,
				CssClasses:  args.CssClasses,
				UsageNote:   args.UsageNote,
			}); err != nil {
				return "", err
			}
			return mustJSON(map[string]string{"id": args.ID, "status": "updated"}), nil
		},
	})

	register(Tool{
		Name:          "delete_component",
		Description:   "Deletes a registered component by id. Cross-tenant guarded.",
		InputSchema:   schema(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			existing, err := s.queries.GetComponentByID(ctx, args.ID)
			if err != nil {
				return "", fmt.Errorf("component %s not found", args.ID)
			}
			if existing.SiteID != agent.SiteID {
				return "", errors.New("component belongs to another site")
			}
			if err := s.queries.DeleteComponent(ctx, args.ID); err != nil {
				return "", err
			}
			return mustJSON(map[string]string{"id": args.ID, "status": "deleted"}), nil
		},
	})

	// --- css_classes -----------------------------------------------------

	register(Tool{
		Name:        "list_css_classes",
		Description: "Lists every CSS class registered for the site, ordered by category and sort_order. The agent reads this before composing pages so it can reuse existing classes instead of pasting one-off styles.",
		InputSchema: schema(`{"type":"object","properties":{}}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, _ json.RawMessage) (string, error) {
			rows, err := s.queries.ListCSSClassesBySite(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			return mustJSON(rows), nil
		},
	})

	register(Tool{
		Name:        "upsert_css_class",
		Description: "Creates or updates a CSS class. If id is provided, updates that class. Otherwise creates a new class with the given name (must be unique per site). Required: name, css. Optional: id (for update), category, usage_note, sort_order.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"id":{"type":"string"},
				"name":{"type":"string"},
				"css":{"type":"string"},
				"category":{"type":"string"},
				"usage_note":{"type":"string"},
				"sort_order":{"type":"number"}
			},
			"required":["name","css"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				ID        string `json:"id"`
				Name      string `json:"name"`
				Css       string `json:"css"`
				Category  string `json:"category"`
				UsageNote string `json:"usage_note"`
				SortOrder int64  `json:"sort_order"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			if strings.TrimSpace(args.Name) == "" || strings.TrimSpace(args.Css) == "" {
				return "", errors.New("name and css are required")
			}
			if args.ID != "" {
				existing, err := s.queries.GetCSSClassByID(ctx, args.ID)
				if err != nil {
					return "", fmt.Errorf("css class %s not found", args.ID)
				}
				if existing.SiteID != agent.SiteID {
					return "", errors.New("css class belongs to another site")
				}
				if err := s.queries.UpdateCSSClass(ctx, store.UpdateCSSClassParams{
					ID:        args.ID,
					Name:      args.Name,
					Category:  args.Category,
					Css:       args.Css,
					UsageNote: args.UsageNote,
					SortOrder: args.SortOrder,
				}); err != nil {
					return "", err
				}
				return mustJSON(map[string]string{"id": args.ID, "status": "updated"}), nil
			}
			id := newID()
			if err := s.queries.CreateCSSClass(ctx, store.CreateCSSClassParams{
				ID:        id,
				SiteID:    agent.SiteID,
				Name:      args.Name,
				Category:  args.Category,
				Css:       args.Css,
				UsageNote: args.UsageNote,
				SortOrder: args.SortOrder,
			}); err != nil {
				return "", err
			}
			return mustJSON(map[string]string{"id": id, "status": "created"}), nil
		},
	})

	register(Tool{
		Name:          "delete_css_class",
		Description:   "Deletes a CSS class by id. Cross-tenant guarded.",
		InputSchema:   schema(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			existing, err := s.queries.GetCSSClassByID(ctx, args.ID)
			if err != nil {
				return "", fmt.Errorf("css class %s not found", args.ID)
			}
			if existing.SiteID != agent.SiteID {
				return "", errors.New("css class belongs to another site")
			}
			if err := s.queries.DeleteCSSClass(ctx, args.ID); err != nil {
				return "", err
			}
			return mustJSON(map[string]string{"id": args.ID, "status": "deleted"}), nil
		},
	})

	// --- knowledgebase ---------------------------------------------------

	register(Tool{
		Name:        "list_knowledgebase",
		Description: "Lists every knowledgebase entry for the site (brand voice, product positioning, FAQ source, anything operators want the agent to remember). The agent reads this before authoring copy so the voice matches.",
		InputSchema: schema(`{"type":"object","properties":{}}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, _ json.RawMessage) (string, error) {
			rows, err := s.queries.ListKnowledgebaseBySite(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			return mustJSON(rows), nil
		},
	})

	register(Tool{
		Name:        "create_knowledgebase_entry",
		Description: "Adds a new knowledgebase entry. Required: title, content. Optional: category (e.g. brand-voice, product, faq), sort_order.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"title":{"type":"string"},
				"content":{"type":"string"},
				"category":{"type":"string"},
				"sort_order":{"type":"number"}
			},
			"required":["title","content"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				Title     string `json:"title"`
				Content   string `json:"content"`
				Category  string `json:"category"`
				SortOrder int64  `json:"sort_order"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			if strings.TrimSpace(args.Title) == "" || strings.TrimSpace(args.Content) == "" {
				return "", errors.New("title and content are required")
			}
			id := newID()
			if err := s.queries.CreateKnowledgebaseEntry(ctx, store.CreateKnowledgebaseEntryParams{
				ID:        id,
				SiteID:    agent.SiteID,
				Category:  args.Category,
				Title:     args.Title,
				Content:   args.Content,
				SortOrder: args.SortOrder,
			}); err != nil {
				return "", err
			}
			return mustJSON(map[string]string{"id": id, "status": "created"}), nil
		},
	})

	register(Tool{
		Name:        "update_knowledgebase_entry",
		Description: "Updates an existing knowledgebase entry. Required: id, title, content. Optional: category, is_active (0 or 1), sort_order.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"id":{"type":"string"},
				"title":{"type":"string"},
				"content":{"type":"string"},
				"category":{"type":"string"},
				"is_active":{"type":"number"},
				"sort_order":{"type":"number"}
			},
			"required":["id","title","content"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				ID        string `json:"id"`
				Title     string `json:"title"`
				Content   string `json:"content"`
				Category  string `json:"category"`
				IsActive  int64  `json:"is_active"`
				SortOrder int64  `json:"sort_order"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			existing, err := s.queries.GetKnowledgebaseByID(ctx, args.ID)
			if err != nil {
				return "", fmt.Errorf("knowledgebase entry %s not found", args.ID)
			}
			if existing.SiteID != agent.SiteID {
				return "", errors.New("knowledgebase entry belongs to another site")
			}
			if err := s.queries.UpdateKnowledgebaseEntry(ctx, store.UpdateKnowledgebaseEntryParams{
				ID:        args.ID,
				Category:  args.Category,
				Title:     args.Title,
				Content:   args.Content,
				IsActive:  args.IsActive,
				SortOrder: args.SortOrder,
			}); err != nil {
				return "", err
			}
			return mustJSON(map[string]string{"id": args.ID, "status": "updated"}), nil
		},
	})

	register(Tool{
		Name:          "delete_knowledgebase_entry",
		Description:   "Deletes a knowledgebase entry by id. Cross-tenant guarded.",
		InputSchema:   schema(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			existing, err := s.queries.GetKnowledgebaseByID(ctx, args.ID)
			if err != nil {
				return "", fmt.Errorf("knowledgebase entry %s not found", args.ID)
			}
			if existing.SiteID != agent.SiteID {
				return "", errors.New("knowledgebase entry belongs to another site")
			}
			if err := s.queries.DeleteKnowledgebaseEntry(ctx, args.ID); err != nil {
				return "", err
			}
			return mustJSON(map[string]string{"id": args.ID, "status": "deleted"}), nil
		},
	})

	// --- guardrails ------------------------------------------------------

	register(Tool{
		Name:        "list_guardrails",
		Description: "Lists every guardrail rule for the site. Guardrails are content-validation rules the build pipeline enforces (e.g. minimum H1 length, banned phrases, required CTA per page).",
		InputSchema: schema(`{"type":"object","properties":{}}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, _ json.RawMessage) (string, error) {
			rows, err := s.queries.ListGuardrailsBySite(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			return mustJSON(rows), nil
		},
	})

	register(Tool{
		Name:        "create_guardrail",
		Description: "Adds a new guardrail rule. Required: rule_type, target, value. Optional: severity (warn|error, defaults to warn).",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"rule_type":{"type":"string"},
				"target":{"type":"string"},
				"value":{"type":"string"},
				"severity":{"type":"string","enum":["warn","error"]}
			},
			"required":["rule_type","target","value"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				RuleType string `json:"rule_type"`
				Target   string `json:"target"`
				Value    string `json:"value"`
				Severity string `json:"severity"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			if args.Severity == "" {
				args.Severity = "warn"
			}
			if args.Severity != "warn" && args.Severity != "error" {
				return "", errors.New("severity must be warn or error")
			}
			id := newID()
			if err := s.queries.CreateGuardrail(ctx, store.CreateGuardrailParams{
				ID:       id,
				SiteID:   agent.SiteID,
				RuleType: args.RuleType,
				Target:   args.Target,
				Value:    args.Value,
				Severity: args.Severity,
			}); err != nil {
				return "", err
			}
			return mustJSON(map[string]string{"id": id, "status": "created"}), nil
		},
	})

	register(Tool{
		Name:          "delete_guardrail",
		Description:   "Deletes a guardrail rule by id. Cross-tenant guarded.",
		InputSchema:   schema(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			existing, err := s.queries.GetGuardrailByID(ctx, args.ID)
			if err != nil {
				return "", fmt.Errorf("guardrail %s not found", args.ID)
			}
			if existing.SiteID != agent.SiteID {
				return "", errors.New("guardrail belongs to another site")
			}
			if err := s.queries.DeleteGuardrail(ctx, args.ID); err != nil {
				return "", err
			}
			return mustJSON(map[string]string{"id": args.ID, "status": "deleted"}), nil
		},
	})

	// --- allowed_scripts -------------------------------------------------

	register(Tool{
		Name:        "list_allowed_scripts",
		Description: "Lists every whitelisted external origin for the site. Each entry has kind (script/frame/image/media/connect/all) which routes it to the corresponding CSP directive when the builder regenerates security headers.",
		InputSchema: schema(`{"type":"object","properties":{}}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, _ json.RawMessage) (string, error) {
			rows, err := s.queries.ListAllowedScriptsBySite(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			return mustJSON(rows), nil
		},
	})

	register(Tool{
		Name:        "register_allowed_script",
		Description: "Whitelists an external origin so the builder includes it in the CSP. Required: domain (hostname only, no scheme), kind (script|frame|image|media|connect|all), purpose (one-line description). The next build picks up the change.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"domain":{"type":"string"},
				"kind":{"type":"string","enum":["script","frame","image","media","connect","all"]},
				"purpose":{"type":"string"}
			},
			"required":["domain","kind","purpose"]
		}`),
		RequiresWrite:      true,
		RequiresCapability: "security", // CSP widening is admin-only on REST; a plain write key must not reach it
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				Domain  string `json:"domain"`
				Kind    string `json:"kind"`
				Purpose string `json:"purpose"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			args.Domain = strings.TrimSpace(args.Domain)
			if args.Domain == "" {
				return "", errors.New("domain is required")
			}
			if strings.Contains(args.Domain, "://") || strings.Contains(args.Domain, "/") {
				return "", errors.New("domain must be a hostname only, no scheme or path")
			}
			switch args.Kind {
			case "script", "frame", "image", "media", "connect", "all":
			default:
				return "", errors.New("kind must be one of: script, frame, image, media, connect, all")
			}
			id := newID()
			if err := s.queries.CreateAllowedScript(ctx, store.CreateAllowedScriptParams{
				ID:      id,
				SiteID:  agent.SiteID,
				Domain:  args.Domain,
				Purpose: args.Purpose,
				Kind:    args.Kind,
			}); err != nil {
				return "", err
			}
			return mustJSON(map[string]string{"id": id, "status": "registered"}), nil
		},
	})

	register(Tool{
		Name:               "revoke_allowed_script",
		Description:        "Revokes a whitelisted origin by id. The next build regenerates CSP directives without it.",
		InputSchema:        schema(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
		RequiresWrite:      true,
		RequiresCapability: "security", // CSP allowlist mutation is admin-only on REST; a plain write key must not reach it
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			scripts, err := s.queries.ListAllowedScriptsBySite(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			found := false
			for _, sc := range scripts {
				if sc.ID == args.ID {
					found = true
					break
				}
			}
			if !found {
				return "", fmt.Errorf("allowed script %s not found in this site", args.ID)
			}
			if err := s.queries.DeleteAllowedScript(ctx, args.ID); err != nil {
				return "", err
			}
			return mustJSON(map[string]string{"id": args.ID, "status": "revoked"}), nil
		},
	})

	// --- fonts (read + delete; upload stays on REST due to multipart) ----

	register(Tool{
		Name:        "list_fonts",
		Description: "Lists every uploaded font (woff2) for the site. Each row carries family_name, weight, style, file_path. Read this before authoring typography to know which faces and weights are available.",
		InputSchema: schema(`{"type":"object","properties":{}}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, _ json.RawMessage) (string, error) {
			rows, err := s.queries.ListSiteFonts(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			return mustJSON(rows), nil
		},
	})

	register(Tool{
		Name:          "delete_font",
		Description:   "Removes an uploaded font by id. The id comes from list_fonts. Cross-tenant guarded.",
		InputSchema:   schema(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			if err := s.queries.DeleteSiteFont(ctx, store.DeleteSiteFontParams{
				ID:     args.ID,
				SiteID: agent.SiteID,
			}); err != nil {
				return "", err
			}
			return mustJSON(map[string]string{"id": args.ID, "status": "deleted"}), nil
		},
	})

	// --- design references ----------------------------------------------

	register(Tool{
		Name:        "list_design_references",
		Description: "Lists every external GitHub repo registered as a design reference for this site (taste-skill, high-end-visual-design, custom). Each row carries the fetched bundle (README, tailwind config, sample components) under fetched_json.",
		InputSchema: schema(`{"type":"object","properties":{}}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, _ json.RawMessage) (string, error) {
			rows, err := s.queries.ListDesignReferences(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			return mustJSON(rows), nil
		},
	})

	register(Tool{
		Name:        "add_design_reference",
		Description: "Registers a public GitHub repo as a design reference. Required: url (https://github.com/owner/repo). Optional: label (display name), ref_type (design-system|component-library|reference-site, defaults to design-system). The handler fetches a representative bundle (package.json, README, tailwind config, sample components) and stores it for the agent to read.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"url":{"type":"string"},
				"label":{"type":"string"},
				"ref_type":{"type":"string","enum":["design-system","component-library","reference-site"]}
			},
			"required":["url"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				URL     string `json:"url"`
				Label   string `json:"label"`
				RefType string `json:"ref_type"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			parsed, err := url.Parse(strings.TrimSpace(args.URL))
			if err != nil || parsed.Host != "github.com" {
				return "", errors.New("url must be a public github.com repo URL")
			}
			if args.RefType == "" {
				args.RefType = "design-system"
			}
			switch args.RefType {
			case "design-system", "component-library", "reference-site":
			default:
				return "", errors.New("ref_type must be design-system, component-library, or reference-site")
			}
			// The MCP tool does not run the fetch synchronously (the
			// handler does that with a 10s timeout). Instead, we record the
			// reference with empty fetched_json and tell the caller to
			// invoke refresh_design_reference to populate the bundle.
			// This keeps the MCP request fast and avoids blocking the
			// JSON-RPC connection on a slow GitHub fetch.
			id := newID()
			if err := s.queries.CreateDesignReference(ctx, store.CreateDesignReferenceParams{
				ID:          id,
				SiteID:      agent.SiteID,
				Url:         args.URL,
				Label:       strings.TrimSpace(args.Label),
				RefType:     args.RefType,
				FetchedJson: "",
				FetchedAt:   "",
			}); err != nil {
				if strings.Contains(err.Error(), "UNIQUE") {
					return "", errors.New("this repo is already registered as a design reference")
				}
				return "", err
			}
			return mustJSON(map[string]string{
				"id":     id,
				"status": "registered",
				"hint":   "Call refresh_design_reference with this id to fetch the bundle from GitHub.",
			}), nil
		},
	})

	register(Tool{
		Name:          "refresh_design_reference",
		Description:   "Fetches (or re-fetches) the GitHub bundle for a registered design reference by id and stores it under fetched_json: README, tailwind config when present, sample components. Call this after add_design_reference to populate the bundle, or when the upstream repo changed. Cross-tenant guarded by site.",
		InputSchema:   schema(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			if strings.TrimSpace(args.ID) == "" {
				return "", errors.New("id is required")
			}
			if err := handlers.RefreshDesignReferenceBundle(ctx, s.queries, agent.SiteID, args.ID); err != nil {
				return "", err
			}
			return mustJSON(map[string]string{"id": args.ID, "status": "refreshed"}), nil
		},
	})

	register(Tool{
		Name:          "delete_design_reference",
		Description:   "Removes a design reference by id. Cross-tenant guarded.",
		InputSchema:   schema(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			if err := s.queries.DeleteDesignReference(ctx, store.DeleteDesignReferenceParams{
				ID:     args.ID,
				SiteID: agent.SiteID,
			}); err != nil {
				return "", err
			}
			return mustJSON(map[string]string{"id": args.ID, "status": "deleted"}), nil
		},
	})

	// --- self-introspection ---------------------------------------------

	register(Tool{
		Name:        "get_capabilities",
		Description: "Returns the same self-describing snapshot as atomicsite://meta/capabilities resource: agent identity, write capability, full tools/resources/prompts surface, eval thresholds, privacy invariants. Use this when a tools/call surface is more convenient than resources/read.",
		InputSchema: schema(`{"type":"object","properties":{}}`),
		Handler: func(_ context.Context, agent *authmw.AgentIdentity, _ json.RawMessage) (string, error) {
			return mustJSON(s.capabilitiesSnapshot(agent)), nil
		},
	})

	register(Tool{
		Name:        "get_design_playbook",
		Description: "Returns THIS site's design playbook (same payload as atomicsite://meta/design-playbook resource), adapted to its design.fidelity dial (performance / balanced / showcase): fidelity contract first (what is unlocked, how grading changes, what is never relaxed), then principles, page archetypes, anti-patterns, vibe archetypes, materiality, content authenticity (banned slop terms), motion budget, copy voice, font system, hero graphics catalog, one-shot recipe. This is the rubric the Inspector grades against. Read it BEFORE authoring blocks, and re-read after changing design.fidelity. Free to call, agents should consult it on session start.",
		InputSchema: schema(`{"type":"object","properties":{}}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, _ json.RawMessage) (string, error) {
			fidelity := agentpkg.FidelityForSite(ctx, s.queries, agent.SiteID)
			return mustJSON(agentpkg.DesignPlaybookFor(fidelity)), nil
		},
	})

	register(Tool{
		Name:        "preview_screenshot",
		Description: "Captures a headless Chromium screenshot of the current DRAFT page (current DB state, no build needed). Fixes the loop where the agent had to trigger_build + wait + screenshot the deploy URL to see its own work. Server-side path: mints a one-shot loopback token, drives chromedp at /_preview/{token}, the loopback handler swaps the token for the rendered draft HTML (same renderer as the admin draft-preview, including the inline-rendered component blocks). Fonts load from the same loopback origin. Args: page_id (preferred) OR slug (the agent's known slug for the page). Optional: viewport_width (320-3840, default 1440), viewport_height (240-2160, default 900), full_page (default true), wait_ms (default 1200, lets the canvas globe + cost-calculator islands paint). Returns base64 PNG; vision-capable agents decode it and reason about pixels in the same turn. Use this BEFORE trigger_build to iterate fast.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"page_id":{"type":"string"},
				"slug":{"type":"string"},
				"viewport_width":{"type":"integer","minimum":320,"maximum":3840},
				"viewport_height":{"type":"integer","minimum":240,"maximum":2160},
				"full_page":{"type":"boolean"},
				"wait_ms":{"type":"integer","minimum":0,"maximum":10000}
			}
		}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			if s.previewTokens == nil {
				return "", errors.New("preview_screenshot not configured on this server (preview tokens unset)")
			}
			var args struct {
				PageID         string `json:"page_id"`
				Slug           string `json:"slug"`
				ViewportWidth  int    `json:"viewport_width"`
				ViewportHeight int    `json:"viewport_height"`
				FullPage       *bool  `json:"full_page"`
				WaitMs         int    `json:"wait_ms"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}

			pageID := strings.TrimSpace(args.PageID)
			slug := strings.TrimSpace(args.Slug)
			if pageID == "" && slug == "" {
				return "", errors.New("page_id or slug required")
			}
			if pageID == "" {
				page, err := s.queries.GetPageBySiteAndSlug(ctx, store.GetPageBySiteAndSlugParams{
					SiteID: agent.SiteID,
					Slug:   slug,
				})
				if err != nil {
					return "", fmt.Errorf("page not found for slug %q: %w", slug, err)
				}
				pageID = page.ID
			} else {
				page, err := s.queries.GetPageByID(ctx, pageID)
				if err != nil {
					return "", fmt.Errorf("page not found: %w", err)
				}
				if page.SiteID != agent.SiteID {
					return "", errors.New("page does not belong to this site")
				}
			}

			tok, err := s.previewTokens.Mint(agent.SiteID, pageID)
			if err != nil {
				return "", fmt.Errorf("mint preview token: %w", err)
			}

			port := s.loopbackPort
			if port == 0 {
				port = 8080
			}
			previewURL := fmt.Sprintf("http://127.0.0.1:%d/_preview/%s", port, tok)

			waitMs := args.WaitMs
			if waitMs == 0 {
				waitMs = 1200
			}

			res, err := handlers.CaptureLoopbackScreenshot(ctx, handlers.ScreenshotRequest{
				URL:            previewURL,
				ViewportWidth:  args.ViewportWidth,
				ViewportHeight: args.ViewportHeight,
				FullPage:       args.FullPage,
				WaitMs:         waitMs,
			})
			if err != nil {
				return "", fmt.Errorf("preview screenshot: %w", err)
			}

			out := map[string]any{
				"page_id":      pageID,
				"site_id":      agent.SiteID,
				"preview_url":  previewURL,
				"image_base64": res.ImageBase64,
				"format":       res.Format,
				"full_page":    res.FullPage,
				"size_bytes":   res.SizeBytes,
				"viewport":     map[string]int{"width": res.Viewport.Width, "height": res.Viewport.Height},
				"hint":         "image_base64 is a base64-encoded PNG. Vision-capable agents decode it and reason about pixels. Capture is of the CURRENT DRAFT (no build needed), so you can iterate before trigger_build.",
			}
			return mustJSON(out), nil
		},
	})

	// --- font upload (base64 JSON-RPC variant of the multipart REST handler) ---

	register(Tool{
		Name:        "upload_font",
		Description: "Uploads a woff2 font as base64 data via JSON-RPC. The REST handler at POST /api/sites/{siteID}/fonts accepts multipart; this tool is the agent-friendly equivalent. Required: family_name (1-64 chars, letters/digits/spaces/hyphen/underscore), file_base64 (woff2 binary, max 2MB after decode). Optional: weight (100-900, default 400), style (normal|italic, default normal), original_name (display filename), subset (unicode-range: latin|latin-ext|cyrillic|greek|vietnamese|arabic|hebrew|all, default latin). Subset drives the @font-face unicode-range descriptor so the browser only fetches the woff2 when the page contains characters in that range. Pick 'all' for a fully unsubsetted font, otherwise stay on 'latin' for EU/UK sites. Server validates the woff2 signature ('wOF2' magic at offset 0) before writing. UNIQUE per (site, family, weight, style); duplicate uploads return a clear error.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"family_name":{"type":"string"},
				"file_base64":{"type":"string"},
				"weight":{"type":"number"},
				"style":{"type":"string","enum":["normal","italic"]},
				"original_name":{"type":"string"},
				"subset":{"type":"string","enum":["latin","latin-ext","cyrillic","greek","vietnamese","arabic","hebrew","all"]}
			},
			"required":["family_name","file_base64"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			if s.fontsDir == "" {
				return "", errors.New("upload_font is not configured on this server (FontsDir unset); upload via the REST endpoint POST /api/sites/{siteID}/fonts instead")
			}
			var args struct {
				FamilyName   string `json:"family_name"`
				FileBase64   string `json:"file_base64"`
				Weight       int64  `json:"weight"`
				Style        string `json:"style"`
				OriginalName string `json:"original_name"`
				Subset       string `json:"subset"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			args.FamilyName = strings.TrimSpace(args.FamilyName)
			if !validFontFamilyName.MatchString(args.FamilyName) {
				return "", errors.New("family_name must be 1 to 64 chars: letters, digits, spaces, hyphen, underscore")
			}
			if args.Weight == 0 {
				args.Weight = 400
			}
			if args.Weight < 100 || args.Weight > 900 {
				return "", errors.New("weight must be between 100 and 900")
			}
			if args.Style == "" {
				args.Style = "normal"
			}
			if args.Style != "normal" && args.Style != "italic" {
				return "", errors.New("style must be normal or italic")
			}
			args.Subset = normalizeMCPSubset(args.Subset)
			body, err := base64.StdEncoding.DecodeString(args.FileBase64)
			if err != nil {
				return "", fmt.Errorf("file_base64 is not valid base64: %v", err)
			}
			if len(body) > fontUploadMaxBytes {
				return "", fmt.Errorf("font exceeds %d byte cap (got %d)", fontUploadMaxBytes, len(body))
			}
			if len(body) < 4 || string(body[:4]) != "wOF2" {
				return "", errors.New("only woff2 fonts are accepted (signature 'wOF2' at offset 0)")
			}
			id := newFontIDForMCP()
			siteFontsDir := filepath.Join(s.fontsDir, agent.SiteID)
			if err := os.MkdirAll(siteFontsDir, 0o755); err != nil {
				return "", fmt.Errorf("failed to prepare fonts directory: %v", err)
			}
			outPath := filepath.Join(siteFontsDir, id+".woff2")
			if err := os.WriteFile(outPath, body, 0o644); err != nil {
				return "", fmt.Errorf("failed to write font file: %v", err)
			}
			if err := s.queries.CreateSiteFont(ctx, store.CreateSiteFontParams{
				ID:           id,
				SiteID:       agent.SiteID,
				FamilyName:   args.FamilyName,
				Weight:       args.Weight,
				Style:        args.Style,
				FilePath:     outPath,
				FileSize:     int64(len(body)),
				OriginalName: cleanFontFilename(args.OriginalName),
				Subset:       args.Subset,
			}); err != nil {
				_ = os.Remove(outPath)
				if strings.Contains(err.Error(), "UNIQUE") {
					return "", errors.New("a font with this family / weight / style already exists for this site")
				}
				return "", fmt.Errorf("failed to record font: %v", err)
			}
			return mustJSON(map[string]any{
				"id":          id,
				"family_name": args.FamilyName,
				"weight":      args.Weight,
				"style":       args.Style,
				"subset":      args.Subset,
				"size":        len(body),
				"status":      "uploaded",
			}), nil
		},
	})

	// --- figma import: tool returns admin URL + instructions ---

	register(Tool{
		Name:        "get_figma_import_url",
		Description: "Returns the admin URL where the user pastes a Figma file URL + personal access token to import design tokens. The agent does NOT receive the access token through MCP (security: tokens stay between user and server). Use this in the build_from_figma flow: call this tool, present the URL + instructions to the user, wait for them to confirm import completed, then read atomicsite://site/context to pick up the new branding tokens and atomicsite://figma/imports for the import record.",
		InputSchema: schema(`{"type":"object","properties":{}}`),
		Handler: func(_ context.Context, agent *authmw.AgentIdentity, _ json.RawMessage) (string, error) {
			base := strings.TrimRight(s.baseURL, "/")
			if base == "" {
				base = "(BASE_URL not configured; use admin host)"
			}
			adminPath := fmt.Sprintf("/sites/%s/settings/design", agent.SiteID)
			return mustJSON(map[string]any{
				"admin_url": base + adminPath,
				"instructions": []string{
					"1. Open the admin URL above in your browser.",
					"2. Find the Figma section under Design Settings.",
					"3. Paste your Figma file URL (https://www.figma.com/design/<key>/...).",
					"4. Paste a Figma personal access token (figd_... or figpat_...). The token is used once to fetch the file and is NEVER persisted server-side.",
					"5. Click Import. The handler seeds CSS classes per published color + text style, updates branding's primary color, and updates font_heading + font_body to the most-likely brand fonts.",
					"6. After confirmation, call get_site_context to pick up the new tokens, list_css_classes to see the seeded classes, and list_fonts to see the new families.",
				},
				"why_not_a_direct_tool": "Pasting the Figma access token through MCP would route it through the AI host's transcript, which is logged by some hosts and visible in conversation context. Keeping the token in the user's browser session is the safer pattern.",
			}), nil
		},
	})
}

// validFontFamilyName mirrors handlers.validFamilyName: 1 to 64 chars,
// letters / digits / spaces / hyphen / underscore. Duplicated here
// (tiny regex; not worth a shared package) so MCP stays decoupled from
// handlers internals.
var validFontFamilyName = regexp.MustCompile(`^[A-Za-z0-9 _-]{1,64}$`)

// fontUploadMaxBytes mirrors handlers.fontUploadMaxBytes (2 MB).
const fontUploadMaxBytes = 2 << 20

// newFontIDForMCP mirrors handlers.newFontID. Distinct name to avoid a
// rename conflict if both files import.
func newFontIDForMCP() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// cleanFontFilename trims and strips path separators from
// agent-supplied filenames before they hit the database. Mirrors the
// behaviour of handlers.cleanFilename without taking a dependency.
func cleanFontFilename(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "/", "")
	name = strings.ReplaceAll(name, "\\", "")
	if len(name) > 128 {
		name = name[:128]
	}
	return name
}
