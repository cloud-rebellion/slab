package handlers

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/bright-interaction/slab/internal/builder"
	"github.com/bright-interaction/slab/internal/config"
	"github.com/bright-interaction/slab/internal/store"
)

// SettingsHandler handles per-site settings CRUD.
type SettingsHandler struct {
	cfg     *config.Config
	queries *store.Queries
	// onAnalyticsChange is invoked after any write that touches the analytics
	// category, so the analytics Manager can rescan and (re)spawn parsers for
	// sites that just had tracking flipped on.
	onAnalyticsChange func(context.Context)
}

func NewSettingsHandler(cfg *config.Config, queries *store.Queries) *SettingsHandler {
	return &SettingsHandler{cfg: cfg, queries: queries}
}

// OnAnalyticsChange registers a callback invoked after analytics-category writes.
func (h *SettingsHandler) OnAnalyticsChange(fn func(context.Context)) {
	h.onAnalyticsChange = fn
}

func (h *SettingsHandler) List(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	settings, err := h.queries.ListSettingsBySite(r.Context(), siteID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list settings")
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (h *SettingsHandler) ListByCategory(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	category := urlParam(r, "category")
	settings, err := h.queries.ListSettingsByCategory(r.Context(), store.ListSettingsByCategoryParams{
		SiteID:   siteID,
		Category: category,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list settings")
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (h *SettingsHandler) Upsert(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	var req struct {
		Category string `json:"category"`
		Key      string `json:"key"`
		Value    string `json:"value"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	if req.Category == "" || req.Key == "" {
		writeError(w, http.StatusBadRequest, "category and key are required")
		return
	}
	if err := validateSetting(req.Category, req.Key, req.Value); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	id := newID()
	err := h.queries.UpsertSetting(r.Context(), store.UpsertSettingParams{
		ID:       id,
		SiteID:   siteID,
		Category: req.Category,
		Key:      req.Key,
		Value:    req.Value,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to save setting")
		return
	}

	if req.Category == "analytics" && h.onAnalyticsChange != nil {
		h.onAnalyticsChange(r.Context())
	}

	setting, _ := h.queries.GetSetting(r.Context(), store.GetSettingParams{
		SiteID:   siteID,
		Category: req.Category,
		Key:      req.Key,
	})
	writeJSON(w, http.StatusOK, setting)
}

func (h *SettingsHandler) BulkUpsert(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	var req []struct {
		Category string `json:"category"`
		Key      string `json:"key"`
		Value    string `json:"value"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}

	// Validate every entry up front; reject the whole batch on first
	// failure so the admin doesn't end up with half-applied state.
	for _, s := range req {
		if s.Category == "" || s.Key == "" {
			continue
		}
		if err := validateSetting(s.Category, s.Key, s.Value); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
	}

	touchedAnalytics := false
	for _, s := range req {
		if s.Category == "" || s.Key == "" {
			continue
		}
		if s.Category == "analytics" {
			touchedAnalytics = true
		}
		if err := h.queries.UpsertSetting(r.Context(), store.UpsertSettingParams{
			ID:       newID(),
			SiteID:   siteID,
			Category: s.Category,
			Key:      s.Key,
			Value:    s.Value,
		}); err != nil {
			// Batch was pre-validated, so this is infrastructure-level.
			// Stop and say exactly where it broke; a 200 with silently
			// dropped keys is how half-applied state used to ship.
			slog.Error("settings: bulk upsert write failed", "site_id", siteID, "key", s.Category+"."+s.Key, "err", err)
			writeError(w, http.StatusInternalServerError,
				"Failed to apply settings batch at "+s.Category+"."+s.Key+"; keys before it were written, keys after were not")
			return
		}
	}

	if touchedAnalytics && h.onAnalyticsChange != nil {
		h.onAnalyticsChange(r.Context())
	}

	settings, _ := h.queries.ListSettingsBySite(r.Context(), siteID)
	writeJSON(w, http.StatusOK, settings)
}

func (h *SettingsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	id := urlParam(r, "settingID")
	// No GetSettingByID query; scan the site's own list to verify the
	// setting belongs here before letting Delete fire. Without this
	// scope check, any member of any site could pass a settingID for
	// another tenant and have it removed.
	rows, _ := h.queries.ListSettingsBySite(r.Context(), siteID)
	owns := false
	for _, s := range rows {
		if s.ID == id {
			owns = true
			break
		}
	}
	if !owns {
		writeError(w, http.StatusNotFound, "Setting not found")
		return
	}
	if err := h.queries.DeleteSetting(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete setting")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// CookiePresets returns the cookie declarations atomicsite auto-derives from
// the site's enabled trackers (GA4, language settings, etc.). The admin
// Cookies page calls this to show the read-only "from your stack" rows
// alongside the user-edited list. Computed on the fly from current
// settings so flipping GA4 on immediately surfaces the GA cookies.
func (h *SettingsHandler) CookiePresets(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	site, err := h.queries.GetSiteByID(r.Context(), siteID)
	if err != nil {
		writeError(w, http.StatusNotFound, "Site not found")
		return
	}
	rows, err := h.queries.ListSettingsBySite(r.Context(), siteID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list settings")
		return
	}
	settingsMap := make(map[string]string, len(rows))
	for _, s := range rows {
		settingsMap[s.Category+"."+s.Key] = s.Value
	}
	presets := builder.PresetCookieDeclarations(site, settingsMap)
	writeJSON(w, http.StatusOK, presets)
}

// SecurityPreview returns the resolved security headers the next build would
// emit for this site. Used by the admin Security page to show "this is what
// CSP will look like" before triggering a build, and by the kind-routing
// e2e test to verify a frame-only domain lands in frame-src and not
// script-src.
func (h *SettingsHandler) SecurityPreview(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	hdrs, err := builder.BuildSecurityHeaders(r.Context(), h.queries, siteID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to compute headers")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"csp":                               hdrs.CSP,
		"hsts":                              hdrs.HSTS,
		"x_frame_options":                   hdrs.XFrameOptions,
		"x_content_type_options":            hdrs.XContentTypeOptions,
		"referrer_policy":                   hdrs.ReferrerPolicy,
		"permissions_policy":                hdrs.PermissionsPolicy,
		"cross_origin_opener_policy":        hdrs.COOP,
		"cross_origin_resource_policy":      hdrs.CORP,
		"cross_origin_embedder_policy":      hdrs.COEP,
		"x_permitted_cross_domain_policies": hdrs.XPermittedCrossDomainPolicies,
		"x_xss_protection":                  hdrs.XXSSProtection,
	})
}
