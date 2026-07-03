// Agent bootstrap endpoints: one-click "set me up" flows.
//
// Two surfaces:
//
//  1. Admin POST /api/sites/{siteID}/agent-bootstrap
//     Generates a fresh agent key (default capabilities ["read","write"])
//     and returns a JSON bundle: raw key (visible once), pre-filled
//     CLAUDE.md, .env, and a copy-paste smoke-test curl command.
//
//  2. Agent GET /api/agent/bootstrap
//     Returns the same CLAUDE.md as text/markdown, scoped to the agent's
//     site. Lets an agent fetch its own setup doc on every new session.

package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/bright-interaction/slab/internal/agent"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"
)

// agentBootstrapResponse is the JSON shape returned by the admin bootstrap
// endpoint. The raw key is only present here; we do not echo it back from
// the agent-side endpoint because the caller already has it.
type agentBootstrapResponse struct {
	KeyID      string `json:"key_id"`
	Key        string `json:"key"`
	KeyName    string `json:"key_name"`
	BaseURL    string `json:"base_url"`
	SiteID     string `json:"site_id"`
	SiteName   string `json:"site_name"`
	SiteDomain string `json:"site_domain"`
	ClaudeMD   string `json:"claude_md"`
	EnvFile    string `json:"env_file"`
	SmokeTest  string `json:"smoke_test"`
	Note       string `json:"note"`
}

// AgentBootstrap (admin) issues a fresh key for a site and returns a
// download-ready bundle. The raw key is shown once: it is hashed at rest
// and cannot be retrieved later. UI surfaces a copy button + warning.
//
// POST /api/sites/{siteID}/agent-bootstrap
// Body: { "name": "optional-key-name" }
func (h *AgentHandler) AgentBootstrap(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	if siteID == "" {
		writeError(w, http.StatusBadRequest, "siteID is required")
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	// Body is optional; ignore parse errors so the caller can POST without
	// any body and still get a key.
	_ = parseJSON(r, &req)
	if req.Name == "" {
		req.Name = "Quick start key"
	}

	site, err := h.queries.GetSiteByID(r.Context(), siteID)
	if err != nil {
		writeError(w, http.StatusNotFound, "Site not found")
		return
	}

	// Mint a fresh atomic_<hex> key. Identical to GenerateAgentKey but we
	// want the raw value in scope so we can embed it in the bundle.
	// (Older keys carry the legacy ask_ prefix and still authenticate;
	// the auth middleware hashes the raw string regardless of prefix.)
	rawBytes := make([]byte, 32)
	if _, err := rand.Read(rawBytes); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to generate key")
		return
	}
	rawKey := "atomic_" + hex.EncodeToString(rawBytes)
	keyHash := authmw.HashAgentKey(rawKey)

	keyID := newID()
	if err := h.queries.CreateAgentKey(r.Context(), store.CreateAgentKeyParams{
		ID:           keyID,
		SiteID:       siteID,
		Name:         req.Name,
		KeyHash:      keyHash,
		Capabilities: marshalField([]string{"read", "write"}),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create agent key")
		return
	}

	baseURL := h.cfg.BaseURL
	if baseURL == "" || strings.HasPrefix(baseURL, "http://localhost") {
		baseURL = absoluteBaseURL(r)
	}

	claudeMD := agent.RenderCLAUDEMD(r.Context(), h.queries, siteID, baseURL, rawKey)
	envFile := agent.RenderEnvFile(baseURL, rawKey)
	smokeTest := agent.RenderSmokeTest(baseURL)

	writeJSON(w, http.StatusCreated, agentBootstrapResponse{
		KeyID:      keyID,
		Key:        rawKey,
		KeyName:    req.Name,
		BaseURL:    baseURL,
		SiteID:     siteID,
		SiteName:   site.Name,
		SiteDomain: site.Domain,
		ClaudeMD:   claudeMD,
		EnvFile:    envFile,
		SmokeTest:  smokeTest,
		Note:       "Save the key now. It cannot be retrieved again.",
	})
}

// AgentSelfBootstrap (agent) returns the CLAUDE.md as text/markdown for the
// agent's site. Useful when an agent wants to re-read its own instructions
// at the start of a new session, or to verify that the platform's view of
// pending_setup matches what the agent expects.
//
// GET /api/agent/bootstrap
func (h *AgentHandler) AgentSelfBootstrap(w http.ResponseWriter, r *http.Request) {
	a := requireAgent(w, r)
	if a == nil {
		return
	}
	baseURL := h.cfg.BaseURL
	if baseURL == "" || strings.HasPrefix(baseURL, "http://localhost") {
		baseURL = absoluteBaseURL(r)
	}
	body := agent.RenderCLAUDEMD(r.Context(), h.queries, a.SiteID, baseURL, "")
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// absoluteBaseURL infers the public origin from the request when
// cfg.PublicAPIURL is unset. Strips any trailing slash so callers can
// safely concatenate "/api/agent/...".
func absoluteBaseURL(r *http.Request) string {
	scheme := "https"
	if r.TLS == nil {
		// Behind a reverse proxy that terminates TLS, X-Forwarded-Proto is
		// set; fall back to it before assuming http.
		if xfp := r.Header.Get("X-Forwarded-Proto"); xfp != "" {
			scheme = xfp
		} else {
			scheme = "http"
		}
	}
	host := r.Host
	if xfh := r.Header.Get("X-Forwarded-Host"); xfh != "" {
		host = xfh
	}
	return scheme + "://" + host
}
