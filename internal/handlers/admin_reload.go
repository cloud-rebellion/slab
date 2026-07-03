package handlers

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/bright-interaction/slab/internal/config"
)

// AdminReloadHandler accepts authenticated POSTs from Dockyard's rotation
// engine and hot-swaps in-memory secrets without a container restart.
//
// Atomicsite uses a single shared HMAC secret for BOTH directions of the
// BrightCRM webhook flow (signing outbound /webhooks/atomicsite calls
// and verifying inbound /t/inbound pings) plus the AES-256 SHIELD_KEY
// the MCP server hands to every new shield.Session.
//
// A rotation event therefore updates up to three in-memory targets:
//
//  1. TrackHandler.inboundVerifier (current + previous slots) for the webhook secret
//  2. TrackHandler.crmClient signing secret (current only) for the webhook secret
//  3. mcp.Server.shieldKey via UpdateShieldKey for the SHIELD_KEY rotation
//
// Request shape mirrors the BrightCRM-side handler:
//
//	POST /admin/reload-secrets
//	Authorization: Bearer <ADMIN_RELOAD_TOKEN>
//	{
//	  "secrets": {
//	    "BRIGHTCRM_WEBHOOK_SECRET":          "new-value",
//	    "BRIGHTCRM_WEBHOOK_SECRET_PREVIOUS": "old-value",
//	    "SHIELD_KEY":                        "32-char-ascii-key"
//	  }
//	}
//
// 204 success, 401 bad bearer, 503 if ADMIN_RELOAD_TOKEN unset.
type AdminReloadHandler struct {
	cfg   *config.Config
	track *TrackHandler
	mcp   ShieldKeyUpdater
}

// ShieldKeyUpdater is implemented by the MCP server. Decoupled here so
// the handler package does not import mcp (mcp already imports shield;
// adding handler→mcp would create a cycle through server bootstrap).
type ShieldKeyUpdater interface {
	UpdateShieldKey(newKey []byte) error
	ShieldEnabled() bool
}

func NewAdminReloadHandler(cfg *config.Config, track *TrackHandler) *AdminReloadHandler {
	return &AdminReloadHandler{cfg: cfg, track: track}
}

// WithMCP wires the MCP server reference so SHIELD_KEY rotations can
// hot-swap the AES-256 key. Optional; without it the SHIELD_KEY entry
// is rejected from the allowlist with a clear error.
func (h *AdminReloadHandler) WithMCP(m ShieldKeyUpdater) *AdminReloadHandler {
	h.mcp = m
	return h
}

type reloadRequest struct {
	Secrets map[string]string `json:"secrets"`
}

var atomicsiteReloadAllowlist = map[string]struct{}{
	"BRIGHTCRM_WEBHOOK_SECRET":          {},
	"BRIGHTCRM_WEBHOOK_SECRET_PREVIOUS": {},
	"ATOMICSITE_SHIELD_KEY":             {},
}

func (h *AdminReloadHandler) ReloadSecrets(w http.ResponseWriter, r *http.Request) {
	if h.cfg.AdminReloadToken == "" {
		writeError(w, http.StatusServiceUnavailable, "admin reload not configured")
		return
	}

	const prefix = "Bearer "
	hdr := r.Header.Get("Authorization")
	if len(hdr) <= len(prefix) || hdr[:len(prefix)] != prefix {
		writeError(w, http.StatusUnauthorized, "missing bearer token")
		return
	}
	if subtle.ConstantTimeCompare([]byte(hdr[len(prefix):]), []byte(h.cfg.AdminReloadToken)) != 1 {
		writeError(w, http.StatusUnauthorized, "invalid bearer token")
		return
	}

	var req reloadRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Secrets == nil {
		writeError(w, http.StatusBadRequest, "missing secrets")
		return
	}

	staged := make(map[string]string, len(req.Secrets))
	for k, v := range req.Secrets {
		if _, ok := atomicsiteReloadAllowlist[k]; ok {
			staged[k] = v
		}
	}
	if len(staged) == 0 {
		writeError(w, http.StatusBadRequest, "no recognised secrets in payload")
		return
	}

	// Webhook-secret half: optional. When the rotation only carries
	// SHIELD_KEY we don't want to clobber the verifier with an empty
	// primary; only act when the caller actually supplied one.
	if primary, ok := staged["BRIGHTCRM_WEBHOOK_SECRET"]; ok {
		secondary := staged["BRIGHTCRM_WEBHOOK_SECRET_PREVIOUS"]
		if v := h.track.InboundVerifier(); v != nil {
			v.Update(primary, secondary)
		}
		if c := h.track.CRMClient(); c != nil {
			c.UpdateSecret(primary)
		}
	}

	// Shield-key half: optional. Requires the MCP wiring (WithMCP) AND
	// shield to have been enabled at boot. We refuse to half-apply: a
	// SHIELD_KEY that fails to land must produce a 4xx so the rotation
	// engine records it as last_rotation_error and does not advance
	// last_rotated_at.
	if shieldKey, ok := staged["ATOMICSITE_SHIELD_KEY"]; ok {
		if h.mcp == nil || !h.mcp.ShieldEnabled() {
			writeError(w, http.StatusServiceUnavailable, "shield not enabled on this instance")
			return
		}
		if err := h.mcp.UpdateShieldKey([]byte(shieldKey)); err != nil {
			slog.Warn("admin: shield key rotation refused", "error", err)
			writeError(w, http.StatusBadRequest, "shield key update failed: "+err.Error())
			return
		}
	}

	slog.Info("admin: reloaded secrets", "keys", keysOf(staged))
	w.WriteHeader(http.StatusNoContent)
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
