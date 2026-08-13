package handlers

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/bright-interaction/slab/internal/builder"
	"github.com/bright-interaction/slab/internal/config"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/settingspolicy"
	"github.com/bright-interaction/slab/internal/store"
)

// settingsWriteNeedsOwner reports whether one {category, key} write on the
// human REST surface requires the site owner / platform admin rather than
// any site member. Both halves come from the shared policy packages; this
// file invents no policy of its own.
//
//   - settingspolicy.PrivilegedCategory: the whole category changes the
//     site's security posture or moves the tenant's money (security,
//     payments). Same bar allowed_scripts.go already applies to the other
//     half of the CSP.
//   - settingspolicy.IsSecretValue: the individual key is
//     bearer-equivalent, wherever it lives. analytics is a category a
//     member legitimately edits, but analytics.crm_webhook_secret inside
//     it is still a credential.
//
// The key half is not optional decoration. visibleSettings below hides
// secret values from a non-owner, and a field the caller cannot read is a
// field they would blank on their next save, so hiding a value without
// also refusing the write would turn a leak into silent destruction of
// the credential.
func settingsWriteNeedsOwner(category, key string) bool {
	return settingspolicy.PrivilegedCategory(category) || settingspolicy.IsSecretValue(category, key)
}

// requireSettingsWrite gates a single settings write. Ordinary
// category/key pairs pass through untouched so a member can still edit
// content settings.
//
// Returns true when the write may proceed; on false the caller has
// already received a 403 written by RequireOwnerOrAdmin.
func (h *SettingsHandler) requireSettingsWrite(w http.ResponseWriter, r *http.Request, category, key string) bool {
	if !settingsWriteNeedsOwner(category, key) {
		return true
	}
	return authmw.RequireOwnerOrAdmin(w, r, h.queries)
}

// visibleSettings strips rows whose VALUE is bearer-equivalent
// (settingspolicy.IsSecretValue: payments.mollie_api_key,
// analytics.crm_webhook_secret, any *_token / *_api_key / *_secret) when
// the caller is not the site owner or a platform admin.
//
// Without it the write gate above is decorative: a member who may not
// SET the Mollie key could still GET it from /api/sites/{id}/settings and
// use it directly against Mollie, which is the whole value of the key.
// The row is dropped rather than masked so the response cannot carry a
// mask literal back into an <input> and from there into the database.
//
// Owners and admins keep the raw value: the admin Payments page renders
// it into a field and detects the test_/live_ prefix from it. Masking the
// human surface the way the agent surfaces are masked needs that page
// reworked into a write-only field first; see the note in the fix report.
func (h *SettingsHandler) visibleSettings(r *http.Request, rows []store.SiteSetting) []store.SiteSetting {
	secret := false
	for _, s := range rows {
		if settingspolicy.IsSecretValue(s.Category, s.Key) {
			secret = true
			break
		}
	}
	// Skip the membership lookup entirely when there is nothing to hide.
	if !secret || authmw.IsOwnerOrAdmin(r, h.queries) {
		return rows
	}
	out := make([]store.SiteSetting, 0, len(rows))
	for _, s := range rows {
		if settingspolicy.IsSecretValue(s.Category, s.Key) {
			continue
		}
		out = append(out, s)
	}
	return out
}

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
	writeJSON(w, http.StatusOK, h.visibleSettings(r, settings))
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
	// The per-category read is the twin of List, not a narrower surface:
	// GET /settings/payments returns exactly the row GET /settings does.
	writeJSON(w, http.StatusOK, h.visibleSettings(r, settings))
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
	if !h.requireSettingsWrite(w, r, req.Category, req.Key) {
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

	// Authorize the batch ENTRY BY ENTRY, then validate, then write.
	//
	// Shape: entries the caller may not write are dropped and named back
	// in rejected_admin_only; the rest are applied. This is deliberately
	// the shape the other two bulk-settings surfaces already ship
	// (handlers/agent_settings.go BulkUpsertSettings and the MCP
	// bulk_upsert_settings tool), both of which are pinned by e2e specs
	// and documented to agents in mcp/prompts.go. Refusing the whole
	// batch here, as the first version of this gate did, made the human
	// path a third shape of one policy for no security gain: the
	// privileged entries never land either way, and the only difference
	// is whether the caller's OWN permitted keys land with them.
	//
	// The owner lookup is hoisted out of the loop: one membership query
	// per request, not one per entry.
	rejected := []string{}
	needsOwner := false
	for _, s := range req {
		if s.Category == "" || s.Key == "" {
			continue
		}
		if settingsWriteNeedsOwner(s.Category, s.Key) {
			needsOwner = true
			break
		}
	}
	allowPrivileged := true
	if needsOwner {
		allowPrivileged = authmw.IsOwnerOrAdmin(r, h.queries)
	}
	permitted := func(category, key string) bool {
		return allowPrivileged || !settingsWriteNeedsOwner(category, key)
	}

	// Validate every entry the caller may actually write; reject the
	// whole batch on first failure so the admin doesn't end up with
	// half-applied state. Entries already headed for rejected_admin_only
	// are not validated: a member must not be able to tell a valid CSP
	// from an invalid one through a 422 on a key they cannot set.
	for _, s := range req {
		if s.Category == "" || s.Key == "" {
			continue
		}
		if !permitted(s.Category, s.Key) {
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
		if !permitted(s.Category, s.Key) {
			rejected = append(rejected, s.Category+"."+s.Key)
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

	// The echo-back is a read of every setting on the site, so it goes
	// through the same visibility filter as List. Without it the write
	// path is a read route-around: PUT one harmless seo key, get the
	// Mollie key in the response.
	settings, _ := h.queries.ListSettingsBySite(r.Context(), siteID)
	writeJSON(w, http.StatusOK, map[string]any{
		"settings":            h.visibleSettings(r, settings),
		"rejected_admin_only": rejected,
	})
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
	category, key := "", ""
	for _, s := range rows {
		if s.ID == id {
			owns = true
			category, key = s.Category, s.Key
			break
		}
	}
	if !owns {
		writeError(w, http.StatusNotFound, "Setting not found")
		return
	}
	// Deleting a privileged row is a write to that category: dropping
	// security.csp_extra_directives removes hardening the owner added,
	// and a gate on Upsert alone would just be routed around by DELETE.
	// The same holds for a secret row, where DELETE is the shortest path
	// to taking the tenant's shop offline.
	if !h.requireSettingsWrite(w, r, category, key) {
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
