package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bright-interaction/slab/internal/agent"
	"github.com/bright-interaction/slab/internal/billing"
	"github.com/bright-interaction/slab/internal/config"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/perfectfoundation"
	"github.com/bright-interaction/slab/internal/starterkits"
	"github.com/bright-interaction/slab/internal/store"
)

type SiteHandler struct {
	cfg        *config.Config
	queries    *store.Queries
	db         *sql.DB
	guardrails *agent.GuardrailEngine
}

func NewSiteHandler(cfg *config.Config, queries *store.Queries, db *sql.DB) *SiteHandler {
	return &SiteHandler{
		cfg:        cfg,
		queries:    queries,
		db:         db,
		guardrails: agent.NewGuardrailEngine(queries),
	}
}

var (
	slugPattern  = regexp.MustCompile(`^[a-z0-9-]+$`)
	colorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

// applyDefaultSiteSettings inserts the security and server best-practice settings
// for a freshly created site using the supplied Queries handle.
func applyDefaultSiteSettings(ctx context.Context, q *store.Queries, siteID string) error {
	defaults := []struct{ category, key, value string }{
		// Security headers
		{"security", "csp_enabled", "true"},
		{"security", "csp_policy", "auto"},
		{"security", "hsts_enabled", "true"},
		{"security", "hsts_max_age", "31536000"},
		{"security", "hsts_preload", "false"},
		{"security", "x_frame_options", "DENY"},
		{"security", "x_content_type", "nosniff"},
		{"security", "referrer_policy", "strict-origin-when-cross-origin"},
		{"security", "permissions_policy", "camera=(), microphone=(), geolocation=(), interest-cohort=()"},
		{"security", "coop", "same-origin"},
		{"security", "corp", "same-origin"},
		// Nginx/server
		{"server", "gzip_enabled", "true"},
		{"server", "brotli_enabled", "false"},
		{"server", "cache_static_max_age", "31536000"},
		{"server", "cache_html_max_age", "3600"},
		{"server", "rate_limit_enabled", "false"},
		{"server", "rate_limit_rps", "10"},
	}
	for _, d := range defaults {
		if err := q.UpsertSetting(ctx, store.UpsertSettingParams{
			ID:       newID(),
			SiteID:   siteID,
			Category: d.category,
			Key:      d.key,
			Value:    d.value,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (h *SiteHandler) List(w http.ResponseWriter, r *http.Request) {
	sites, err := h.queries.ListSites(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list sites")
		return
	}
	// Audit C1: non-admin users only see sites they're a member of.
	// Admins see everything (legacy single-workspace behaviour). The
	// filtering happens after ListSites returns the full set so we can
	// reuse the existing query without adding a JOIN-by-membership
	// variant — at any realistic scale this is faster than a JOIN.
	user := authmw.GetUser(r)
	if user != nil && user.Role != "admin" {
		ids, err := h.queries.ListSiteIDsForUser(r.Context(), user.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to load memberships")
			return
		}
		idSet := make(map[string]bool, len(ids))
		for _, id := range ids {
			idSet[id] = true
		}
		filtered := sites[:0]
		for _, s := range sites {
			if idSet[s.ID] {
				filtered = append(filtered, s)
			}
		}
		sites = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{"sites": sites})
}

func (h *SiteHandler) Get(w http.ResponseWriter, r *http.Request) {
	site, err := h.queries.GetSiteByID(r.Context(), urlParam(r, "siteID"))
	if err != nil {
		writeError(w, http.StatusNotFound, "Site not found")
		return
	}
	writeJSON(w, http.StatusOK, site)
}

// ListSilos returns the site's declared silos with their type. Used by the
// admin pages list to badge silo hubs as soft / hard / inherit.
func (h *SiteHandler) ListSilos(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	if _, err := h.queries.GetSiteByID(r.Context(), siteID); err != nil {
		writeError(w, http.StatusNotFound, "Site not found")
		return
	}
	silos, err := h.queries.ListSilosBySite(r.Context(), siteID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list silos")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"silos": silos})
}

type createSiteRequest struct {
	WorkspaceID    string `json:"workspace_id"`
	Name           string `json:"name"`
	Slug           string `json:"slug"`
	Domain         string `json:"domain"`
	PrimaryColor   string `json:"primary_color"`
	SecondaryColor string `json:"secondary_color"`
	BgColor        string `json:"bg_color"`
	TextColor      string `json:"text_color"`
	FontHeading    string `json:"font_heading"`
	FontBody       string `json:"font_body"`
	Lang           string `json:"lang"`
}

func (h *SiteHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req createSiteRequest
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.Name == "" || req.Slug == "" {
		writeError(w, http.StatusBadRequest, "Name and slug are required")
		return
	}
	// Phase 30.2: resolve the workspace this site belongs to and gate
	// against the workspace's plan. The OSS path resolves to the
	// auto-bootstrap workspace and PlanLimits returns -1 (unlimited)
	// from the OSS Provider; cloud installs check Solo/Studio/Agency
	// caps and refuse with 402 when the customer is over the cap.
	workspaceID, err := h.resolveWorkspaceForCreate(r, req.WorkspaceID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.checkSiteQuota(r, workspaceID); err != nil {
		writeError(w, http.StatusPaymentRequired, err.Error())
		return
	}

	// Defaults
	if req.PrimaryColor == "" {
		req.PrimaryColor = "#D4AF37"
	}
	if req.SecondaryColor == "" {
		req.SecondaryColor = "#935FA7"
	}
	if req.BgColor == "" {
		req.BgColor = "#FFFFFF"
	}
	if req.TextColor == "" {
		req.TextColor = "#1A1A1A"
	}
	if req.FontHeading == "" {
		req.FontHeading = "Inter"
	}
	if req.FontBody == "" {
		req.FontBody = "Inter"
	}
	if req.Lang == "" {
		req.Lang = "en"
	}

	id := newID()
	slug := strings.ToLower(strings.ReplaceAll(req.Slug, " ", "-"))

	if err := h.queries.CreateSite(r.Context(), store.CreateSiteParams{
		ID:             id,
		WorkspaceID:    workspaceID,
		Name:           req.Name,
		Slug:           slug,
		Domain:         req.Domain,
		PrimaryColor:   req.PrimaryColor,
		SecondaryColor: req.SecondaryColor,
		BgColor:        req.BgColor,
		TextColor:      req.TextColor,
		FontHeading:    req.FontHeading,
		FontBody:       req.FontBody,
		Lang:           req.Lang,
	}); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeError(w, http.StatusConflict, "A site with this slug already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "Failed to create site")
		return
	}

	// Audit C1: auto-grant the creating user ownership of the new site so
	// they can access it (RequireSiteAccess middleware would otherwise 403
	// them from their own freshly-created site). Admins also get a row
	// even though they technically bypass the middleware — keeps the
	// member list complete for the dashboard's "who has access" view.
	if user := authmw.GetUser(r); user != nil {
		if err := h.queries.AddSiteMember(r.Context(), store.AddSiteMemberParams{
			SiteID: id,
			UserID: user.ID,
			Role:   "owner",
		}); err != nil {
			slog.Warn("create site: auto-grant owner membership failed",
				"site_id", id, "user_id", user.ID, "err", err)
		}
	}

	// Seed defaults for the new site. Best-effort by design (the site is
	// usable without them), but each failure is logged: a site silently
	// missing its guardrails or security defaults looks identical to a
	// healthy one until an audit finds it.
	if err := agent.SeedDefaultGuardrailsWith(r.Context(), h.queries, id); err != nil {
		slog.Warn("create site: guardrail seeding failed", "site_id", id, "err", err)
	}
	if err := agent.SeedDefaultKnowledgebaseWith(r.Context(), h.queries, id); err != nil {
		slog.Warn("create site: knowledgebase seeding failed", "site_id", id, "err", err)
	}
	if err := h.queries.EnsureMediaFolder(r.Context(), store.EnsureMediaFolderParams{
		SiteID:   id,
		Name:     "brand",
		IsSystem: 1,
	}); err != nil {
		slog.Warn("create site: brand media folder seeding failed", "site_id", id, "err", err)
	}

	// Create default architecture
	if err := h.queries.UpsertSiteArchitecture(r.Context(), store.UpsertSiteArchitectureParams{
		ID:            newID(),
		SiteID:        id,
		StructureType: "soft-silo",
		MaxDepth:      3,
	}); err != nil {
		slog.Warn("create site: architecture seeding failed", "site_id", id, "err", err)
	}

	// Seed default security + server settings (best-practice defaults).
	// All toggles can be flipped via PATCH /api/sites/{id}/settings.
	if err := applyDefaultSiteSettings(r.Context(), h.queries, id); err != nil {
		slog.Warn("create site: default settings seeding failed", "site_id", id, "err", err)
	}

	site, _ := h.queries.GetSiteByID(r.Context(), id)
	writeJSON(w, http.StatusCreated, site)
}

// seedSiteRequest is the wizard payload for POST /api/sites/seed.
type seedSiteRequest struct {
	Type string `json:"type"`
	Info struct {
		Name         string `json:"name"`
		Slug         string `json:"slug"`
		Domain       string `json:"domain"`
		Lang         string `json:"lang"`
		BusinessName string `json:"business_name"`
		ContactEmail string `json:"contact_email"`
		Country      string `json:"country"`
	} `json:"info"`
	Structure struct {
		StructureType string `json:"structure_type"`
		MaxDepth      int64  `json:"max_depth"`
	} `json:"structure"`
	Silos []struct {
		Name       string `json:"name"`
		SlugPrefix string `json:"slug_prefix"`
		SiloType   string `json:"silo_type,omitempty"`
	} `json:"silos"`
	Branding struct {
		PrimaryColor   string `json:"primary_color"`
		SecondaryColor string `json:"secondary_color"`
		BgColor        string `json:"bg_color"`
		TextColor      string `json:"text_color"`
		FontHeading    string `json:"font_heading"`
		FontBody       string `json:"font_body"`
	} `json:"branding"`
	StarterKit string `json:"starter_kit,omitempty"`
}

// validateSeedRequest checks the wizard payload and applies defaults.
// It returns a user-facing error message if validation fails.
func (req *seedSiteRequest) normalize() error {
	// Type
	allowedTypes := map[string]bool{
		"b2b": true, "b2c": true, "personal": true,
		"blog": true, "agency": true, "one-pager": true,
	}
	if req.Type == "" {
		req.Type = "b2b"
	}
	if !allowedTypes[req.Type] {
		return errors.New("invalid type")
	}

	// Info
	req.Info.Name = strings.TrimSpace(req.Info.Name)
	req.Info.Slug = strings.ToLower(strings.TrimSpace(req.Info.Slug))
	if req.Info.Name == "" {
		return errors.New("info.name is required")
	}
	if req.Info.Slug == "" || len(req.Info.Slug) > 50 || !slugPattern.MatchString(req.Info.Slug) {
		return errors.New("info.slug must match ^[a-z0-9-]+$ (1-50 chars)")
	}
	if req.Info.Lang == "" {
		req.Info.Lang = "en"
	}

	// Structure
	allowedStructures := map[string]bool{
		"one-pager": true, "soft-silo": true, "hard-silo": true,
	}
	if req.Structure.StructureType == "" {
		req.Structure.StructureType = "soft-silo"
	}
	if !allowedStructures[req.Structure.StructureType] {
		return errors.New("structure.structure_type invalid")
	}
	if req.Structure.MaxDepth == 0 {
		req.Structure.MaxDepth = 3
	}
	if req.Structure.MaxDepth < 1 || req.Structure.MaxDepth > 3 {
		return errors.New("structure.max_depth must be 1, 2, or 3")
	}

	// Silos
	allowedSiloTypes := map[string]bool{"inherit": true, "soft": true, "hard": true}
	for i, s := range req.Silos {
		s.Name = strings.TrimSpace(s.Name)
		s.SlugPrefix = strings.TrimSpace(s.SlugPrefix)
		s.SiloType = strings.TrimSpace(s.SiloType)
		if s.Name == "" || s.SlugPrefix == "" {
			return errors.New("each silo requires name and slug_prefix")
		}
		if s.SiloType == "" {
			s.SiloType = "inherit"
		}
		if !allowedSiloTypes[s.SiloType] {
			return errors.New("silo silo_type must be 'inherit', 'soft', or 'hard'")
		}
		req.Silos[i] = s
	}

	// Branding defaults + validation
	if req.Branding.PrimaryColor == "" {
		req.Branding.PrimaryColor = "#D4AF37"
	}
	if req.Branding.SecondaryColor == "" {
		req.Branding.SecondaryColor = "#935FA7"
	}
	if req.Branding.BgColor == "" {
		req.Branding.BgColor = "#FFFFFF"
	}
	if req.Branding.TextColor == "" {
		req.Branding.TextColor = "#1A1A1A"
	}
	if req.Branding.FontHeading == "" {
		req.Branding.FontHeading = "Inter"
	}
	if req.Branding.FontBody == "" {
		req.Branding.FontBody = "Inter"
	}
	for _, c := range []string{
		req.Branding.PrimaryColor, req.Branding.SecondaryColor,
		req.Branding.BgColor, req.Branding.TextColor,
	} {
		if !colorPattern.MatchString(c) {
			return errors.New("branding colors must match ^#[0-9a-fA-F]{6}$")
		}
	}

	// Starter kit (optional). If set, must reference a registered kit ID.
	req.StarterKit = strings.TrimSpace(req.StarterKit)
	if req.StarterKit != "" {
		if _, ok := starterkits.Default.Get(req.StarterKit); !ok {
			return errors.New("starter_kit references an unknown kit")
		}
	}

	return nil
}

// Seed creates a fully provisioned site (sites + profile + architecture + silos +
// guardrails + knowledgebase + global blocks + essential pages + settings) inside a
// single transaction. Used by the Phase 7 onboarding wizard.
func (h *SiteHandler) Seed(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)

	var req seedSiteRequest
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if err := req.normalize(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()

	// Phase 30.2: Seed needs the same workspace gate Create has.
	// Resolves the user's workspace, refuses with 402 over plan caps.
	workspaceID, err := h.resolveWorkspaceForCreate(r, "")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.checkSiteQuota(r, workspaceID); err != nil {
		writeError(w, http.StatusPaymentRequired, err.Error())
		return
	}

	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		slog.Error("seed: begin tx", "error", err)
		writeError(w, http.StatusInternalServerError, "Failed to start transaction")
		return
	}
	defer tx.Rollback()

	qtx := h.queries.WithTx(tx)
	siteID := newID()

	// 1. sites row
	if err := qtx.CreateSite(ctx, store.CreateSiteParams{
		ID:             siteID,
		WorkspaceID:    workspaceID,
		Name:           req.Info.Name,
		Slug:           req.Info.Slug,
		Domain:         req.Info.Domain,
		PrimaryColor:   req.Branding.PrimaryColor,
		SecondaryColor: req.Branding.SecondaryColor,
		BgColor:        req.Branding.BgColor,
		TextColor:      req.Branding.TextColor,
		FontHeading:    req.Branding.FontHeading,
		FontBody:       req.Branding.FontBody,
		Lang:           req.Info.Lang,
	}); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeError(w, http.StatusConflict, "A site with this slug already exists")
			return
		}
		slog.Error("seed: create site", "error", err, "slug", req.Info.Slug)
		writeError(w, http.StatusInternalServerError, "Failed to create site")
		return
	}

	// 1b. system 'brand' media folder so the Media library has its
	// always-on Brand Assets section from the very first page load.
	if err := qtx.EnsureMediaFolder(ctx, store.EnsureMediaFolderParams{
		SiteID:   siteID,
		Name:     "brand",
		IsSystem: 1,
	}); err != nil {
		slog.Error("seed: brand folder", "error", err, "site_id", siteID)
		writeError(w, http.StatusInternalServerError, "Failed to seed brand folder")
		return
	}

	// 2. site_profiles row
	if err := qtx.UpsertSiteProfile(ctx, store.UpsertSiteProfileParams{
		ID:           newID(),
		SiteID:       siteID,
		BusinessName: sanitizeLine(req.Info.BusinessName),
		Country:      sanitizeLine(req.Info.Country),
		ContactEmail: strings.TrimSpace(req.Info.ContactEmail),
	}); err != nil {
		slog.Error("seed: upsert profile", "error", err, "site_id", siteID)
		writeError(w, http.StatusInternalServerError, "Failed to create profile")
		return
	}

	// 3. site_architecture row
	if err := qtx.UpsertSiteArchitecture(ctx, store.UpsertSiteArchitectureParams{
		ID:            newID(),
		SiteID:        siteID,
		StructureType: req.Structure.StructureType,
		MaxDepth:      req.Structure.MaxDepth,
	}); err != nil {
		slog.Error("seed: upsert architecture", "error", err, "site_id", siteID)
		writeError(w, http.StatusInternalServerError, "Failed to create architecture")
		return
	}

	// 4. site_silos rows
	for i, s := range req.Silos {
		if err := qtx.CreateSilo(ctx, store.CreateSiloParams{
			ID:         newID(),
			SiteID:     siteID,
			Name:       s.Name,
			SlugPrefix: s.SlugPrefix,
			SiloType:   s.SiloType,
			SortOrder:  int64(i),
		}); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				writeError(w, http.StatusBadRequest, "Duplicate silo slug_prefix")
				return
			}
			slog.Error("seed: create silo", "error", err, "site_id", siteID)
			writeError(w, http.StatusInternalServerError, "Failed to create silo")
			return
		}
	}

	// 5. Phase 2 auto-seeding (guardrails, knowledgebase, global blocks, essential pages, settings)
	if err := agent.SeedDefaultGuardrailsWith(ctx, qtx, siteID); err != nil {
		slog.Error("seed: guardrails", "error", err, "site_id", siteID)
		writeError(w, http.StatusInternalServerError, "Failed to seed guardrails")
		return
	}
	if err := perfectfoundation.Apply(ctx, qtx, siteID); err != nil {
		slog.Error("seed: perfect-foundation", "error", err, "site_id", siteID)
		writeError(w, http.StatusInternalServerError, "Failed to seed reference knowledgebase")
		return
	}
	if err := agent.SeedDefaultGlobalBlocksWith(ctx, qtx, siteID); err != nil {
		slog.Error("seed: global blocks", "error", err, "site_id", siteID)
		writeError(w, http.StatusInternalServerError, "Failed to seed global blocks")
		return
	}
	if err := agent.SeedEssentialPagesWith(ctx, qtx, siteID); err != nil {
		slog.Error("seed: essential pages", "error", err, "site_id", siteID)
		writeError(w, http.StatusInternalServerError, "Failed to seed essential pages")
		return
	}
	if err := applyDefaultSiteSettings(ctx, qtx, siteID); err != nil {
		slog.Error("seed: settings", "error", err, "site_id", siteID)
		writeError(w, http.StatusInternalServerError, "Failed to seed settings")
		return
	}

	// 6. Optional starter kit. If set, apply inside the same tx so a failure
	// rolls back everything we just created.
	if req.StarterKit != "" {
		kit, ok := starterkits.Default.Get(req.StarterKit)
		if !ok {
			// Should never happen because normalize() validated this, but
			// guard against a registry mutation between validate and apply.
			slog.Error("seed: starter kit disappeared", "kit", req.StarterKit, "site_id", siteID)
			writeError(w, http.StatusInternalServerError, "Failed to apply starter kit")
			return
		}
		if err := kit.Apply(ctx, qtx, siteID); err != nil {
			slog.Error("seed: apply starter kit", "error", err, "kit", req.StarterKit, "site_id", siteID)
			writeError(w, http.StatusInternalServerError, "Failed to apply starter kit")
			return
		}
	}

	if err := tx.Commit(); err != nil {
		slog.Error("seed: commit", "error", err, "site_id", siteID)
		writeError(w, http.StatusInternalServerError, "Failed to commit transaction")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"site_id": siteID})
}

func (h *SiteHandler) Update(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	existing, err := h.queries.GetSiteByID(r.Context(), siteID)
	if err != nil {
		writeError(w, http.StatusNotFound, "Site not found")
		return
	}

	var req struct {
		Name              *string `json:"name"`
		Slug              *string `json:"slug"`
		Domain            *string `json:"domain"`
		Status            *string `json:"status"`
		PrimaryColor      *string `json:"primary_color"`
		SecondaryColor    *string `json:"secondary_color"`
		BgColor           *string `json:"bg_color"`
		TextColor         *string `json:"text_color"`
		SurfaceColor      *string `json:"surface_color"`
		BorderColor       *string `json:"border_color"`
		MutedColor        *string `json:"muted_color"`
		AccentColor       *string `json:"accent_color"`
		OnPrimaryColor    *string `json:"on_primary_color"`
		FontHeading       *string `json:"font_heading"`
		FontBody          *string `json:"font_body"`
		MetaTitle         *string `json:"meta_title"`
		MetaDescription   *string `json:"meta_description"`
		OgImageID         *string `json:"og_image_id"`
		FaviconID         *string `json:"favicon_id"`
		Ga4ID             *string `json:"ga4_id"`
		UmamiID           *string `json:"umami_id"`
		UmamiURL          *string `json:"umami_url"`
		CookieproofDomain *string `json:"cookieproof_domain"`
		Lang              *string `json:"lang"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	params := store.UpdateSiteParams{
		Name:              existing.Name,
		Slug:              existing.Slug,
		Domain:            existing.Domain,
		Status:            existing.Status,
		PrimaryColor:      existing.PrimaryColor,
		SecondaryColor:    existing.SecondaryColor,
		BgColor:           existing.BgColor,
		TextColor:         existing.TextColor,
		SurfaceColor:      existing.SurfaceColor,
		BorderColor:       existing.BorderColor,
		MutedColor:        existing.MutedColor,
		AccentColor:       existing.AccentColor,
		OnPrimaryColor:    existing.OnPrimaryColor,
		FontHeading:       existing.FontHeading,
		FontBody:          existing.FontBody,
		MetaTitle:         existing.MetaTitle,
		MetaDescription:   existing.MetaDescription,
		OgImageID:         existing.OgImageID,
		FaviconID:         existing.FaviconID,
		Ga4ID:             existing.Ga4ID,
		UmamiID:           existing.UmamiID,
		UmamiUrl:          existing.UmamiUrl,
		CookieproofDomain: existing.CookieproofDomain,
		Lang:              existing.Lang,
		ID:                siteID,
	}

	if req.Name != nil {
		params.Name = *req.Name
	}
	if req.Slug != nil {
		newSlug := strings.ToLower(strings.TrimSpace(*req.Slug))
		if newSlug == "" || len(newSlug) > 50 || !slugPattern.MatchString(newSlug) {
			writeError(w, http.StatusBadRequest, "slug must match ^[a-z0-9-]+$ (1-50 chars)")
			return
		}
		params.Slug = newSlug
	}
	if req.Domain != nil {
		params.Domain = *req.Domain
	}
	if req.Status != nil {
		params.Status = *req.Status
	}
	if req.PrimaryColor != nil {
		params.PrimaryColor = *req.PrimaryColor
	}
	if req.SecondaryColor != nil {
		params.SecondaryColor = *req.SecondaryColor
	}
	if req.BgColor != nil {
		params.BgColor = *req.BgColor
	}
	if req.TextColor != nil {
		params.TextColor = *req.TextColor
	}
	if req.SurfaceColor != nil {
		params.SurfaceColor = *req.SurfaceColor
	}
	if req.BorderColor != nil {
		params.BorderColor = *req.BorderColor
	}
	if req.MutedColor != nil {
		params.MutedColor = *req.MutedColor
	}
	if req.AccentColor != nil {
		params.AccentColor = *req.AccentColor
	}
	if req.OnPrimaryColor != nil {
		params.OnPrimaryColor = *req.OnPrimaryColor
	}
	if req.FontHeading != nil {
		params.FontHeading = *req.FontHeading
	}
	if req.FontBody != nil {
		params.FontBody = *req.FontBody
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
	if req.FaviconID != nil {
		params.FaviconID = *req.FaviconID
	}
	if req.Ga4ID != nil {
		params.Ga4ID = *req.Ga4ID
	}
	if req.UmamiID != nil {
		params.UmamiID = *req.UmamiID
	}
	if req.UmamiURL != nil {
		params.UmamiUrl = *req.UmamiURL
	}
	if req.CookieproofDomain != nil {
		params.CookieproofDomain = *req.CookieproofDomain
	}
	if req.Lang != nil {
		params.Lang = *req.Lang
	}

	if err := h.queries.UpdateSite(r.Context(), params); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeError(w, http.StatusConflict, "A site with this slug already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "Failed to update site")
		return
	}

	// Local deploy targets freeze the slug inside their config path at
	// creation. After a rename, LocalDeployer.Validate rejects the
	// old-slug path and every subsequent build's auto-deploy fails, so
	// rewrite the slug path segment in step with the rename.
	if params.Slug != existing.Slug {
		h.syncLocalDeployTargetSlugs(r.Context(), siteID, existing.Slug, params.Slug)
	}

	site, _ := h.queries.GetSiteByID(r.Context(), siteID)
	writeJSON(w, http.StatusOK, site)
}

// syncLocalDeployTargetSlugs rewrites the /<oldSlug>/ path segment in
// every local deploy target's config to the new slug. Best-effort with
// loud logging: a stale target fails closed at deploy time (Validate),
// never cross-tenant.
func (h *SiteHandler) syncLocalDeployTargetSlugs(ctx context.Context, siteID, oldSlug, newSlug string) {
	if oldSlug == "" || newSlug == "" || oldSlug == newSlug {
		return
	}
	targets, err := h.queries.ListDeployTargetsBySite(ctx, siteID)
	if err != nil {
		slog.Warn("site rename: listing deploy targets for slug sync failed", "site_id", siteID, "err", err)
		return
	}
	for _, t := range targets {
		if t.Kind != "local" {
			continue
		}
		cfg := map[string]any{}
		if strings.TrimSpace(t.ConfigJson) != "" {
			if err := json.Unmarshal([]byte(t.ConfigJson), &cfg); err != nil {
				continue
			}
		}
		path, _ := cfg["path"].(string)
		if path == "" || !strings.Contains(path, "/"+oldSlug+"/") && !strings.HasSuffix(path, "/"+oldSlug) {
			continue
		}
		updated := strings.ReplaceAll(path, "/"+oldSlug+"/", "/"+newSlug+"/")
		if strings.HasSuffix(updated, "/"+oldSlug) {
			updated = strings.TrimSuffix(updated, "/"+oldSlug) + "/" + newSlug
		}
		if updated == path {
			continue
		}
		cfg["path"] = updated
		configJSON, err := json.Marshal(cfg)
		if err != nil {
			continue
		}
		if err := h.queries.UpdateDeployTarget(ctx, store.UpdateDeployTargetParams{
			ID:         t.ID,
			Name:       t.Name,
			Kind:       t.Kind,
			ConfigJson: string(configJSON),
		}); err != nil {
			slog.Warn("site rename: deploy target slug sync failed", "target_id", t.ID, "err", err)
			continue
		}
		slog.Info("site rename: local deploy target path updated", "target_id", t.ID, "old", path, "new", updated)
	}
}

// ListStarterKits returns the catalog of registered starter kits for the
// onboarding wizard. Public endpoint: only kit metadata is exposed and the
// wizard fetches it before any site exists / before login. Concrete kit
// content lives in code.
func (h *SiteHandler) ListStarterKits(w http.ResponseWriter, r *http.Request) {
	includeHidden := r.URL.Query().Get("include") == "hidden"
	kits := starterkits.Default.List()
	out := make([]map[string]any, 0, len(kits))
	for _, k := range kits {
		if !includeHidden && starterkits.IsHidden(k) {
			continue
		}
		targets := k.TargetSiteTypes()
		if targets == nil {
			targets = []string{}
		}
		out = append(out, map[string]any{
			"id":                k.ID(),
			"name":              k.Name(),
			"description":       k.Description(),
			"target_site_types": targets,
			"hidden":            starterkits.IsHidden(k),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *SiteHandler) Delete(w http.ResponseWriter, r *http.Request) {
	// Owner-or-admin gate. SiteAccessMiddleware already verified the user
	// has membership of this site, but a non-owner editor must not be able
	// to nuke a site they were merely invited to. Workspace admins still
	// pass through (legacy single-tenant behaviour).
	if !authmw.RequireOwnerOrAdmin(w, r, h.queries) {
		return
	}

	siteID := urlParam(r, "siteID")
	site, err := h.queries.GetSiteByID(r.Context(), siteID)
	if err != nil {
		writeError(w, http.StatusNotFound, "Site not found")
		return
	}

	if err := h.queries.DeleteSite(r.Context(), siteID); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete site")
		return
	}

	AuditLog(r.Context(), h.queries, r, siteID, AuditActionDelete, "site", siteID, map[string]any{
		"name":   site.Name,
		"slug":   site.Slug,
		"domain": site.Domain,
	})

	// Audit H8: clean up the workspace dir on disk after the row is gone.
	// The DB cascades all child tables, but `{DataDir}/workspaces/{siteID}/`
	// would otherwise live on as orphan disk usage. siteID went through
	// isSafeSiteID at the route level, so RemoveAll can't escape the
	// workspaces dir.
	if isSafeSiteID(siteID) {
		wsDir := filepath.Join(h.cfg.DataDir, "workspaces", siteID)
		if err := os.RemoveAll(wsDir); err != nil {
			slog.Warn("delete site: workspace cleanup failed",
				"site_id", siteID, "ws_dir", wsDir, "err", err)
			// Non-fatal: the DB delete succeeded so the site is gone
			// from the user's POV; orphaned disk will be reaped by the
			// nightly orphan sweep.
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// resolveWorkspaceForCreate maps a Create-site request to the workspace
// the site will belong to. Phase 30.2.
//
//   - explicit body.workspace_id wins (cloud signup picks the right ws)
//   - else the user's first workspace via ListWorkspaceIDsForUser
//   - else 400 (caller should never get here in OSS because the boot
//     bootstrap auto-creates a default workspace + memberships)
func (h *SiteHandler) resolveWorkspaceForCreate(r *http.Request, requested string) (string, error) {
	user := authmw.GetUser(r)
	if user == nil {
		return "", errors.New("not authenticated")
	}
	if requested != "" {
		// Verify membership before trusting the body. Admins (the
		// seeded OSS admin) bypass; cloud non-admins must be members.
		if user.Role == "admin" {
			return requested, nil
		}
		_, err := h.queries.GetWorkspaceMembership(r.Context(), store.GetWorkspaceMembershipParams{
			WorkspaceID: requested,
			UserID:      user.ID,
		})
		if err != nil {
			return "", errors.New("not a member of the requested workspace")
		}
		return requested, nil
	}
	ids, err := h.queries.ListWorkspaceIDsForUser(r.Context(), user.ID)
	if err != nil || len(ids) == 0 {
		// Admin path falls back to the first existing workspace
		// (the OSS auto-bootstrap one).
		if user.Role == "admin" {
			all, err := h.queries.ListAllWorkspaces(r.Context())
			if err == nil && len(all) > 0 {
				return all[0].ID, nil
			}
		}
		return "", errors.New("user has no workspace; create one first")
	}
	return ids[0], nil
}

// checkSiteQuota enforces the workspace's plan cap on site count.
// Reads directly from billing.Limit so even an OSS deployment that
// configures a paid plan value on a workspace gets the cap enforced
// (the EE Provider's OSS implementation returns -1 for everything by
// design, but this gate must fire whenever the workspace plan
// resolves to a numeric cap). Mirrors the Sprint 3 migration plan
// quota pattern in plan_quota.go.
func (h *SiteHandler) checkSiteQuota(r *http.Request, workspaceID string) error {
	ws, err := h.queries.GetWorkspaceByID(r.Context(), workspaceID)
	if err != nil {
		return errors.New("workspace not found")
	}
	limit := billing.Limit(ws.Plan, "max_sites")
	if limit < 0 {
		return nil
	}
	sites, err := h.queries.ListSitesByWorkspace(r.Context(), workspaceID)
	if err != nil {
		return errors.New("failed to count sites")
	}
	if int64(len(sites)) >= limit {
		return errors.New("plan max sites reached; upgrade plan to add more")
	}
	return nil
}
