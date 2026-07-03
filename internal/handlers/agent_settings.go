// Agent-side site settings API.
//
// Mirrors the admin settings endpoints. Lets an agent read settings (any
// category) and write to safe categories (seo, analytics). Categories
// that affect security posture (security, allowed-scripts, nginx, danger)
// stay admin-only.

package handlers

import (
	"net/http"

	"github.com/bright-interaction/slab/internal/builder"
	"github.com/bright-interaction/slab/internal/settingspolicy"
	"github.com/bright-interaction/slab/internal/store"
)

// agentWritableSettingsCategories reports whether an agent may write the
// settings category. Policy lives in internal/settingspolicy (single
// source of truth shared with MCP + the settings catalog).
func agentWritableSettingsCategories(category string) bool {
	return settingspolicy.AgentWritableCategory(category)
}

// ListSettings returns all settings rows for the agent's site (any category).
//
// GET /api/agent/settings
func (h *AgentHandler) ListSettings(w http.ResponseWriter, r *http.Request) {
	a := requireAgent(w, r)
	if a == nil {
		return
	}
	rows, err := h.queries.ListSettingsBySite(r.Context(), a.SiteID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list settings")
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// ListSettingsByCategory returns settings for one category.
//
// GET /api/agent/settings/{category}
func (h *AgentHandler) ListSettingsByCategory(w http.ResponseWriter, r *http.Request) {
	a := requireAgent(w, r)
	if a == nil {
		return
	}
	category := urlParam(r, "category")
	if category == "" {
		writeError(w, http.StatusBadRequest, "category is required")
		return
	}
	rows, err := h.queries.ListSettingsByCategory(r.Context(), store.ListSettingsByCategoryParams{
		SiteID:   a.SiteID,
		Category: category,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list settings")
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// BulkUpsertSettings applies a list of {category, key, value} writes in one
// call. Categories not in agentWritableSettingsCategories are silently
// skipped (the response carries the full settings list afterwards so the
// caller can see which writes landed).
//
// PATCH /api/agent/settings
func (h *AgentHandler) BulkUpsertSettings(w http.ResponseWriter, r *http.Request) {
	a := requireAgent(w, r)
	if a == nil {
		return
	}
	if !requireWrite(w, a) {
		return
	}

	var req []struct {
		Category string `json:"category"`
		Key      string `json:"key"`
		Value    string `json:"value"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}

	// Pre-validate every writable entry so a typo in one key doesn't half-
	// apply the batch. Admin-only categories are quietly bucketed into
	// rejected_admin_only (the agent should ask the human via the per-key
	// settings_catalog.human_admin_url link).
	for _, s := range req {
		if s.Category == "" || s.Key == "" {
			continue
		}
		if !agentWritableSettingsCategories(s.Category) {
			continue
		}
		if err := validateSetting(s.Category, s.Key, s.Value); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
	}

	rejected := []string{}

	// Apply the writable batch in a single transaction so a mid-batch
	// failure (DB hiccup, FK constraint we didn't anticipate) doesn't
	// leave half the values landed and the rest dropped silently. The
	// caller already pre-validated above so the only remaining failure
	// modes are infrastructure-level. Falls back to a non-tx loop when
	// the handler was constructed without a *sql.DB (legacy callers).
	if h.db != nil {
		tx, err := h.db.BeginTx(r.Context(), nil)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to start transaction")
			return
		}
		qtx := h.queries.WithTx(tx)
		commit := true
		for _, s := range req {
			if s.Category == "" || s.Key == "" {
				continue
			}
			if !agentWritableSettingsCategories(s.Category) {
				rejected = append(rejected, s.Category+"."+s.Key)
				continue
			}
			if err := qtx.UpsertSetting(r.Context(), store.UpsertSettingParams{
				ID:       newID(),
				SiteID:   a.SiteID,
				Category: s.Category,
				Key:      s.Key,
				Value:    s.Value,
			}); err != nil {
				_ = tx.Rollback()
				commit = false
				writeError(w, http.StatusInternalServerError, "Failed to apply settings batch")
				return
			}
		}
		if commit {
			if err := tx.Commit(); err != nil {
				writeError(w, http.StatusInternalServerError, "Failed to commit settings batch")
				return
			}
		}
	} else {
		for _, s := range req {
			if s.Category == "" || s.Key == "" {
				continue
			}
			if !agentWritableSettingsCategories(s.Category) {
				rejected = append(rejected, s.Category+"."+s.Key)
				continue
			}
			if err := h.queries.UpsertSetting(r.Context(), store.UpsertSettingParams{
				ID:       newID(),
				SiteID:   a.SiteID,
				Category: s.Category,
				Key:      s.Key,
				Value:    s.Value,
			}); err != nil {
				// Non-tx fallback cannot roll back; stop and report the
				// partial write honestly instead of claiming success.
				writeError(w, http.StatusInternalServerError,
					"Failed to apply settings batch at "+s.Category+"."+s.Key+"; earlier keys in the batch were written")
				return
			}
		}
	}

	rows, _ := h.queries.ListSettingsBySite(r.Context(), a.SiteID)
	writeJSON(w, http.StatusOK, map[string]any{
		"settings":            rows,
		"rejected_admin_only": rejected,
		"writable_categories": settingspolicy.AgentWritableCategories(),
	})
}

// ListAllowedScripts returns the trusted-domain rows for the agent's site.
// Read-only: writes stay admin-only because adding a CSP host widens the
// attack surface. The agent uses this to detect "is cal.com already
// trusted?" before asking the human to add it.
//
// GET /api/agent/allowed-scripts
func (h *AgentHandler) ListAllowedScripts(w http.ResponseWriter, r *http.Request) {
	a := requireAgent(w, r)
	if a == nil {
		return
	}
	rows, err := h.queries.ListAllowedScriptsBySite(r.Context(), a.SiteID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list allowed scripts")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"trusted_domains":   rows,
		"allowed_kinds":     []string{"script", "frame", "image", "media", "connect", "all"},
		"writable_by_agent": false,
		"hint":              "Domains are added by the human admin in Settings -> Trusted external domains. Pick the right kind so the domain lands in the right CSP directive: script-src for JS, frame-src for iframes (cal.com / YouTube / Stripe Checkout), img-src for image CDNs, media-src for audio/video hosts, connect-src for fetch-only API hosts.",
	})
}

// SecurityPreview returns the resolved security headers the next build
// would emit for the agent's site. Read-only mirror of the admin
// /api/sites/{id}/settings/security/preview endpoint.
//
// GET /api/agent/security/preview
func (h *AgentHandler) SecurityPreview(w http.ResponseWriter, r *http.Request) {
	a := requireAgent(w, r)
	if a == nil {
		return
	}
	hdrs, err := builder.BuildSecurityHeaders(r.Context(), h.queries, a.SiteID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to compute headers")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"csp":                          hdrs.CSP,
		"hsts":                         hdrs.HSTS,
		"x_frame_options":              hdrs.XFrameOptions,
		"x_content_type_options":       hdrs.XContentTypeOptions,
		"referrer_policy":              hdrs.ReferrerPolicy,
		"permissions_policy":           hdrs.PermissionsPolicy,
		"cross_origin_opener_policy":   hdrs.COOP,
		"cross_origin_resource_policy": hdrs.CORP,
		"cross_origin_embedder_policy": hdrs.COEP,
	})
}
