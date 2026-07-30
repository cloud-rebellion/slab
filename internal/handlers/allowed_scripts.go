package handlers

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/bright-interaction/slab/internal/config"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"
)

// domainPattern matches RFC 1123 hostnames (optional leading "*." for wildcard
// subdomains), with an optional "https://" or "http://" prefix.
// This is strict: no spaces, no semicolons, no single quotes, no wildcards in
// the middle of the host. Each label is [a-z0-9][a-z0-9-]{0,62} and TLD must
// be at least 2 chars.
var domainPattern = regexp.MustCompile(`^(?:https?://)?(?:\*\.)?(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}$`)

// validateDomain rejects any string that could break a CSP directive when
// interpolated into "script-src 'self' {domain}".
func validateDomain(d string) error {
	d = strings.TrimSpace(d)
	if d == "" {
		return fmt.Errorf("domain is required")
	}
	if len(d) > 253 {
		return fmt.Errorf("domain too long")
	}
	// Block CSP-breaking characters explicitly for clarity
	for _, c := range "; '\"()<>," {
		if strings.ContainsRune(d, c) {
			return fmt.Errorf("domain contains disallowed character: %q", c)
		}
	}
	if strings.Contains(d, " ") || strings.Contains(d, "\n") || strings.Contains(d, "\r") || strings.Contains(d, "\t") {
		return fmt.Errorf("domain contains whitespace")
	}
	if !domainPattern.MatchString(d) {
		return fmt.Errorf("invalid domain format (expected e.g. example.com or *.example.com)")
	}
	return nil
}

// AllowedKinds is the closed set of values for allowed_scripts.kind. Keep in
// sync with the schema comment in internal/db/schema.sql and with the CSP
// directive routing in builder/security.go:buildCSP.
var AllowedKinds = map[string]bool{
	"script":  true,
	"frame":   true,
	"image":   true,
	"media":   true,
	"connect": true,
	"all":     true,
}

func validateKind(k string) (string, error) {
	k = strings.TrimSpace(strings.ToLower(k))
	if k == "" {
		return "script", nil
	}
	if !AllowedKinds[k] {
		return "", fmt.Errorf("kind must be one of: script, frame, image, media, connect, all")
	}
	return k, nil
}

// AllowedScriptHandler handles the CSP allowlist for external script domains.
type AllowedScriptHandler struct {
	cfg     *config.Config
	queries *store.Queries
}

func NewAllowedScriptHandler(cfg *config.Config, queries *store.Queries) *AllowedScriptHandler {
	return &AllowedScriptHandler{cfg: cfg, queries: queries}
}

func (h *AllowedScriptHandler) List(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	rows, err := h.queries.ListAllowedScriptsBySite(r.Context(), siteID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list allowed scripts")
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// Mutating the allowlist widens the site's CSP script-src, which is the
// difference between "attacker-controlled JS cannot run here" and "it can". The
// MCP side already treats this as an admin surface and gates its equivalent
// tool behind an extra named capability, justifying itself with "CSP allowlist
// mutation is admin-only on REST". That was not true: these three handlers were
// registered on the plain site router with no role check, so any site member
// could widen the CSP directly. Now the code matches the claim.
func (h *AllowedScriptHandler) Create(w http.ResponseWriter, r *http.Request) {
	if !authmw.RequireOwnerOrAdmin(w, r, h.queries) {
		return
	}
	siteID := urlParam(r, "siteID")
	var req struct {
		Domain  string `json:"domain"`
		Purpose string `json:"purpose"`
		Kind    string `json:"kind"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	if err := validateDomain(req.Domain); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	kind, err := validateKind(req.Kind)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	id := newID()
	if err := h.queries.CreateAllowedScript(r.Context(), store.CreateAllowedScriptParams{
		ID:      id,
		SiteID:  siteID,
		Domain:  req.Domain,
		Purpose: req.Purpose,
		Kind:    kind,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create allowed script")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "domain": req.Domain, "kind": kind})
}

func (h *AllowedScriptHandler) Update(w http.ResponseWriter, r *http.Request) {
	if !authmw.RequireOwnerOrAdmin(w, r, h.queries) {
		return
	}
	id := urlParam(r, "scriptID")
	var req struct {
		Domain   *string `json:"domain"`
		Purpose  *string `json:"purpose"`
		Kind     *string `json:"kind"`
		IsActive *int64  `json:"is_active"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}

	// Load existing to preserve unchanged fields
	rows, _ := h.queries.ListAllowedScriptsBySite(r.Context(), urlParam(r, "siteID"))
	var existing *store.AllowedScript
	for _, s := range rows {
		if s.ID == id {
			existing = &s
			break
		}
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "Allowed script not found")
		return
	}

	domain := existing.Domain
	if req.Domain != nil {
		if err := validateDomain(*req.Domain); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		domain = *req.Domain
	}
	purpose := existing.Purpose
	if req.Purpose != nil {
		purpose = *req.Purpose
	}
	kind := existing.Kind
	if req.Kind != nil {
		k, err := validateKind(*req.Kind)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		kind = k
	}
	active := existing.IsActive
	if req.IsActive != nil {
		active = *req.IsActive
	}

	if err := h.queries.UpdateAllowedScript(r.Context(), store.UpdateAllowedScriptParams{
		ID:       id,
		Domain:   domain,
		Purpose:  purpose,
		Kind:     kind,
		IsActive: active,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to update allowed script")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (h *AllowedScriptHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if !authmw.RequireOwnerOrAdmin(w, r, h.queries) {
		return
	}
	siteID := urlParam(r, "siteID")
	id := urlParam(r, "scriptID")
	// Defence against cross-tenant deletes: scan the site's own list
	// before deleting so a member of site A can't pass a scriptID
	// belonging to site B and get it removed.
	rows, _ := h.queries.ListAllowedScriptsBySite(r.Context(), siteID)
	owns := false
	for _, s := range rows {
		if s.ID == id {
			owns = true
			break
		}
	}
	if !owns {
		writeError(w, http.StatusNotFound, "Allowed script not found")
		return
	}
	if err := h.queries.DeleteAllowedScript(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete allowed script")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
