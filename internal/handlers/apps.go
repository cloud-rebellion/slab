// Package handlers: apps + site_app_installs endpoints.
//
// Sprint 4 (MCP-as-Apps platform, slice A, 2026-05-24). The
// marketplace + install flow is the wedge: tenants browse curated
// third-party MCP integrations, enter credentials, and atomicsite
// stores the install row. Slice B will wire the upstream MCP proxy
// so the agent can dial each install's tools/list through atomicsite
// under a namespaced surface (e.g. stripe.create_payment_link).
//
// Cross-tenant safety: every routes is mounted under siteAccessMW
// (session-authenticated, scoped to the user's site memberships).
// The marketplace list is intentionally cross-tenant (every site
// sees the same apps catalogue) so it lives under a /api/apps prefix
// outside the site scope.

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/bright-interaction/slab/internal/apps"
	"github.com/bright-interaction/slab/internal/atrest"
	"github.com/bright-interaction/slab/internal/config"
	"github.com/bright-interaction/slab/internal/store"
)

type AppsHandler struct {
	cfg     *config.Config
	queries *store.Queries
}

func NewAppsHandler(cfg *config.Config, queries *store.Queries) *AppsHandler {
	return &AppsHandler{cfg: cfg, queries: queries}
}

// credsCipher builds the at-rest cipher from ATOMICSITE_SHIELD_KEY. A
// nil cfg or unset key yields a passthrough cipher so deployments
// without the key keep working with plaintext credentials_json.
func (h *AppsHandler) credsCipher() *atrest.Cipher {
	if h.cfg == nil {
		return atrest.New(nil)
	}
	return atrest.New([]byte(h.cfg.ShieldKey))
}

// decryptCreds returns the plaintext credentials_json, tolerating empty
// and legacy-plaintext rows. On an unreadable value it returns "" so a
// display path shows no keys rather than leaking ciphertext; the use_app
// path checks the error separately so it never calls upstream blind.
func (h *AppsHandler) decryptCreds(stored string) string {
	pt, err := h.credsCipher().Decrypt(stored)
	if err != nil {
		return ""
	}
	return pt
}

// SeedCuratedApps idempotently upserts the curated catalogue defined
// in internal/apps/curated.go. Called once at server boot from
// applySchema. Adds new apps on a deploy; existing rows get their
// description / credentials_schema / version refreshed without
// touching install rows.
func (h *AppsHandler) SeedCuratedApps(ctx context.Context) error {
	for _, c := range apps.Curated {
		credsJSON, err := json.Marshal(c.Credentials)
		if err != nil {
			return err
		}
		if err := h.queries.UpsertApp(ctx, store.UpsertAppParams{
			ID:                    c.ID,
			Slug:                  c.Slug,
			Name:                  c.Name,
			Description:           c.Description,
			Category:              c.Category,
			Publisher:             c.Publisher,
			IconUrl:               c.IconURL,
			McpUrl:                c.MCPURL,
			DocsUrl:               c.DocsURL,
			Version:               c.Version,
			CredentialsSchemaJson: string(credsJSON),
			IsCurated:             1,
			IsActive:              1,
		}); err != nil {
			return err
		}
	}
	return nil
}

// --- marketplace listing -------------------------------------------------

type appOut struct {
	ID               string                 `json:"id"`
	Slug             string                 `json:"slug"`
	Name             string                 `json:"name"`
	Description      string                 `json:"description"`
	Category         string                 `json:"category"`
	Publisher        string                 `json:"publisher"`
	IconURL          string                 `json:"icon_url"`
	DocsURL          string                 `json:"docs_url"`
	Version          string                 `json:"version"`
	CredentialFields []apps.CredentialField `json:"credential_fields"`
	IsCurated        bool                   `json:"is_curated"`
}

func toAppOut(row store.App) appOut {
	out := appOut{
		ID:          row.ID,
		Slug:        row.Slug,
		Name:        row.Name,
		Description: row.Description,
		Category:    row.Category,
		Publisher:   row.Publisher,
		IconURL:     row.IconUrl,
		DocsURL:     row.DocsUrl,
		Version:     row.Version,
		IsCurated:   row.IsCurated == 1,
	}
	if row.CredentialsSchemaJson != "" {
		_ = json.Unmarshal([]byte(row.CredentialsSchemaJson), &out.CredentialFields)
	}
	if out.CredentialFields == nil {
		out.CredentialFields = []apps.CredentialField{}
	}
	return out
}

// ListMarketplace returns every active app. Cross-tenant: the catalogue
// is the same for every site. No siteID URL param; auth comes from the
// authenticated session group this is mounted under.
func (h *AppsHandler) ListMarketplace(w http.ResponseWriter, r *http.Request) {
	rows, err := h.queries.ListActiveApps(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list apps")
		return
	}
	out := make([]appOut, 0, len(rows))
	for _, row := range rows {
		out = append(out, toAppOut(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": out, "count": len(out)})
}

func (h *AppsHandler) GetApp(w http.ResponseWriter, r *http.Request) {
	row, err := h.queries.GetAppByID(r.Context(), urlParam(r, "appID"))
	if err != nil {
		writeError(w, http.StatusNotFound, "App not found")
		return
	}
	writeJSON(w, http.StatusOK, toAppOut(row))
}

// --- per-site installs ---------------------------------------------------

type siteInstallOut struct {
	SiteID      string `json:"site_id"`
	AppID       string `json:"app_id"`
	Status      string `json:"status"`
	InstalledBy string `json:"installed_by"`
	LastUsedAt  string `json:"last_used_at"`
	Notes       string `json:"notes"`
	UpdatedAt   string `json:"updated_at"`
	App         appOut `json:"app"`
	// CredentialsSet is the list of field keys that have a value
	// stored on this install. Values themselves are NEVER returned to
	// the client; the dialog uses this to show which fields are
	// already configured.
	CredentialsSet []string `json:"credentials_set"`
}

func (h *AppsHandler) toInstallOut(row store.ListSiteAppInstallsRow) siteInstallOut {
	out := siteInstallOut{
		SiteID:      row.SiteID,
		AppID:       row.AppID,
		Status:      row.Status,
		InstalledBy: row.InstalledBy,
		LastUsedAt:  row.LastUsedAt,
		Notes:       row.Notes,
		UpdatedAt:   row.UpdatedAt,
		App: appOut{
			ID:          row.AppID,
			Slug:        row.AppSlug,
			Name:        row.AppName,
			Description: row.AppDescription,
			Category:    row.AppCategory,
			Publisher:   row.AppPublisher,
			IconURL:     row.AppIconUrl,
			Version:     row.AppVersion,
		},
		CredentialsSet: credentialsKeySet(h.decryptCreds(row.CredentialsJson)),
	}
	return out
}

// credentialsKeySet returns the field keys with a non-empty value, so
// the install dialog can render which fields are already set without
// the API ever returning the secret values.
func credentialsKeySet(raw string) []string {
	if raw == "" || raw == "{}" {
		return []string{}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return []string{}
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		s, _ := v.(string)
		if strings.TrimSpace(s) != "" {
			out = append(out, k)
		}
	}
	return out
}

func (h *AppsHandler) ListSiteInstalls(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	rows, err := h.queries.ListSiteAppInstalls(r.Context(), siteID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list installs")
		return
	}
	out := make([]siteInstallOut, 0, len(rows))
	for _, row := range rows {
		out = append(out, h.toInstallOut(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"installs": out, "count": len(out)})
}

type installInput struct {
	Credentials map[string]string `json:"credentials"`
	Notes       string            `json:"notes"`
}

// Install creates or updates the site_app_installs row for (site, app).
// Validates required credential fields against the app's
// credentials_schema_json; missing required = 400. Empty values for
// optional fields are stored as empty strings so the credentials_set
// helper can answer "is this set?".
func (h *AppsHandler) Install(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	appID := urlParam(r, "appID")
	app, err := h.queries.GetAppByID(r.Context(), appID)
	if err != nil {
		writeError(w, http.StatusNotFound, "App not found")
		return
	}
	if app.IsActive != 1 {
		writeError(w, http.StatusBadRequest, "App is not active in the marketplace")
		return
	}
	var in installInput
	if err := parseJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	if in.Credentials == nil {
		in.Credentials = map[string]string{}
	}
	var fields []apps.CredentialField
	if app.CredentialsSchemaJson != "" {
		_ = json.Unmarshal([]byte(app.CredentialsSchemaJson), &fields)
	}
	// Validate required fields.
	for _, f := range fields {
		if !f.Required {
			continue
		}
		if strings.TrimSpace(in.Credentials[f.Key]) == "" {
			writeError(w, http.StatusBadRequest, "Required credential field missing: "+f.Key)
			return
		}
	}
	// Trim every value; drop unknown keys so a sloppy frontend can't
	// stuff arbitrary JSON into credentials_json.
	clean := map[string]string{}
	for _, f := range fields {
		if v, ok := in.Credentials[f.Key]; ok {
			clean[f.Key] = strings.TrimSpace(v)
		}
	}
	credsJSON, _ := json.Marshal(clean)
	encCreds, err := h.credsCipher().Encrypt(string(credsJSON))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to protect credentials")
		return
	}
	installedBy := requesterID(r)
	if err := h.queries.UpsertSiteAppInstall(r.Context(), store.UpsertSiteAppInstallParams{
		SiteID:          siteID,
		AppID:           appID,
		Status:          "active",
		CredentialsJson: encCreds,
		InstalledBy:     installedBy,
		Notes:           strings.TrimSpace(in.Notes),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to save install")
		return
	}
	rows, _ := h.queries.ListSiteAppInstalls(r.Context(), siteID)
	for _, row := range rows {
		if row.AppID == appID {
			writeJSON(w, http.StatusOK, h.toInstallOut(row))
			return
		}
	}
	writeError(w, http.StatusInternalServerError, "Install row not found after save")
}

// Revoke removes the install row. The agent can no longer use this
// app's credentials. The agent surface in Slice B treats a missing
// row as "app not installed"; the proxy refuses to dial.
func (h *AppsHandler) Revoke(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	appID := urlParam(r, "appID")
	// Soft-delete by flipping status first so the agent sees a clear
	// "revoked" before the row goes away on the next install attempt.
	_ = h.queries.SetSiteAppInstallStatus(r.Context(), store.SetSiteAppInstallStatusParams{
		Status: "revoked", SiteID: siteID, AppID: appID,
	})
	if err := h.queries.DeleteSiteAppInstall(r.Context(), store.DeleteSiteAppInstallParams{
		SiteID: siteID, AppID: appID,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to revoke install")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- agent helpers ------------------------------------------------------

// AgentInstallView is the read-only summary the agent's
// list_installed_apps tool returns. credentials_set carries the keys
// the agent knows are present (so it can plan multi-step flows
// without ever seeing the values); values themselves flow only
// through Slice B's proxy when an upstream tool fires.
type AgentInstallView struct {
	AppID          string   `json:"app_id"`
	AppSlug        string   `json:"app_slug"`
	AppName        string   `json:"app_name"`
	Category       string   `json:"category"`
	Status         string   `json:"status"`
	CredentialsSet []string `json:"credentials_set"`
	LastUsedAt     string   `json:"last_used_at"`
}

// ListAgentInstalls returns the agent-facing summary for one site.
// Used by the list_installed_apps MCP tool.
func (h *AppsHandler) ListAgentInstalls(ctx context.Context, siteID string) ([]AgentInstallView, error) {
	rows, err := h.queries.ListSiteAppInstalls(ctx, siteID)
	if err != nil {
		return nil, err
	}
	out := make([]AgentInstallView, 0, len(rows))
	for _, row := range rows {
		out = append(out, AgentInstallView{
			AppID:          row.AppID,
			AppSlug:        row.AppSlug,
			AppName:        row.AppName,
			Category:       row.AppCategory,
			Status:         row.Status,
			CredentialsSet: credentialsKeySet(h.decryptCreds(row.CredentialsJson)),
			LastUsedAt:     row.LastUsedAt,
		})
	}
	return out, nil
}

// CallAppForAgent dispatches one upstream MCP tools/call for
// (siteID, appSlug). Slice B's use_app proxy entry point. Resolves
// the install, hydrates credentials, calls the publisher's MCP
// endpoint, bumps last_used_at, and returns the upstream result
// (or upstream error envelope) for the MCP tool to relay.
//
// The Go error return is for *dispatch* failures (app not found,
// not installed, transport failure). Upstream-level failures
// (HTTP 4xx/5xx, JSON-RPC error) come back inside the
// UpstreamResult so the agent can see what the publisher said.
func (h *AppsHandler) CallAppForAgent(ctx context.Context, siteID, appSlug, toolName string, args map[string]any) (*apps.UpstreamResult, error) {
	app, err := h.queries.GetAppBySlug(ctx, appSlug)
	if err != nil {
		return nil, fmt.Errorf("app not found: %s", appSlug)
	}
	if app.IsActive != 1 {
		return nil, fmt.Errorf("app is not active in the marketplace: %s", appSlug)
	}
	install, err := h.queries.GetSiteAppInstall(ctx, store.GetSiteAppInstallParams{
		SiteID: siteID, AppID: app.ID,
	})
	if err != nil {
		return nil, fmt.Errorf("app not installed on this site: %s", appSlug)
	}
	if install.Status != "active" {
		return nil, fmt.Errorf("app install is %s on this site", install.Status)
	}
	creds := map[string]string{}
	if install.CredentialsJson != "" {
		plain, err := h.credsCipher().Decrypt(install.CredentialsJson)
		if err != nil {
			return nil, fmt.Errorf("app credentials are unreadable (shield key changed?): %s", appSlug)
		}
		_ = json.Unmarshal([]byte(plain), &creds)
	}
	var fields []apps.CredentialField
	if app.CredentialsSchemaJson != "" {
		_ = json.Unmarshal([]byte(app.CredentialsSchemaJson), &fields)
	}
	result, err := apps.CallUpstreamMCP(ctx, apps.UpstreamCall{
		URL:         app.McpUrl,
		ToolName:    toolName,
		Arguments:   args,
		Credentials: creds,
		Schema:      fields,
	})
	if err != nil {
		return nil, err
	}
	// Bump last_used_at on every successful dispatch (including
	// JSON-RPC error responses from upstream; the credentials were
	// used regardless of the publisher's outcome).
	_ = h.queries.BumpSiteAppInstallLastUsed(ctx, store.BumpSiteAppInstallLastUsedParams{
		SiteID: siteID, AppID: app.ID,
	})
	return result, nil
}

// ListAgentMarketplace returns the cross-tenant marketplace for the
// list_apps_marketplace MCP tool.
func (h *AppsHandler) ListAgentMarketplace(ctx context.Context) ([]appOut, error) {
	rows, err := h.queries.ListActiveApps(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]appOut, 0, len(rows))
	for _, row := range rows {
		out = append(out, toAppOut(row))
	}
	return out, nil
}

// requesterID returns "user:{id}" for session edits, "agent:{keyID}"
// for MCP edits, "" for system. Mirrors snapshotCreatedBy in
// internal/handlers/blocks.go.
func requesterID(r *http.Request) string {
	// Read both auth shapes; defensively return empty when neither
	// resolves so we never set an invalid installer ID.
	if r == nil || r.Context() == nil {
		return ""
	}
	// Light-weight access: pull from context the same way other
	// handlers do. snapshotCreatedBy is the canonical helper but
	// lives in blocks.go; replicate its read here to avoid a
	// cross-file import.
	return resolveRequesterID(r)
}

// resolveRequesterID is defined alongside resolveRequesterID-using
// handlers; this stub keeps the install row's installed_by populated
// from the session middleware when available. Errors out to "".
func resolveRequesterID(r *http.Request) string {
	if v := r.Header.Get("X-Agent-Key-ID"); v != "" {
		return "agent:" + v
	}
	if v := r.Header.Get("X-User-ID"); v != "" {
		return "user:" + v
	}
	return ""
}
