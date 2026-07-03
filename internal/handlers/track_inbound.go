package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/bright-interaction/slab/internal/store"
)

// inboundRequest is the body the CRM POSTs to /t/inbound. The CRM has
// recognised a visitor (fingerprint or visitor_id), looked them up in its
// own records, and is pushing knowledge back: lifecycle stage, lead score,
// last_topic, name (only when identity is recently confirmed), etc.
//
// Identifiers: at least one of visitor_id or fingerprint must be present.
// Resolution order: (site_id, visitor_id) -> (site_id, fingerprint) -> 404.
// We never auto-create sessions; the CRM only knows visitors who pinged us
// first, so a missing row is a bug, not a use case.
type inboundRequest struct {
	SiteID              string         `json:"site_id"`
	VisitorID           string         `json:"visitor_id"`
	Fingerprint         string         `json:"fingerprint"`
	Email               string         `json:"email"`
	Metadata            map[string]any `json:"metadata"`
	ExpiresAt           string         `json:"expires_at"`
	IdentityConfirmedAt string         `json:"identity_confirmed_at"`
}

// Inbound receives metadata pushes from the CRM. HMAC-SHA256 verified via
// X-Atomicsite-Signature using the same global webhook secret used outbound,
// so the relationship is symmetric.
//
//	401 - missing or invalid signature
//	400 - malformed JSON / missing site_id / missing both identifiers
//	404 - unknown site, OR no matching visit_sessions row
//	413 - body too large (caps at trackBodyMaxBytes)
//	503 - secret not configured (server admin must set BRIGHTCRM_WEBHOOK_SECRET)
//	204 - success
func (h *TrackHandler) Inbound(w http.ResponseWriter, r *http.Request) {
	// Dual-secret verifier (long-lived on TrackHandler so /admin/reload-secrets
	// can hot-swap its values during a Dockyard rotation): accepts the current
	// value plus an optional "previous" value during a rotation grace window
	// so requests in flight signed with the old key still verify.
	verifier := h.inboundVerifier
	if !verifier.Configured() {
		slog.Warn("track: inbound rejected, BRIGHTCRM_WEBHOOK_SECRET not configured")
		writeError(w, http.StatusServiceUnavailable, "inbound webhook not configured")
		return
	}

	sig := r.Header.Get("X-Atomicsite-Signature")
	if sig == "" {
		writeError(w, http.StatusUnauthorized, "missing signature")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, trackBodyMaxBytes)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read body")
		return
	}

	if !verifier.Verify(body, sig) {
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}

	var req inboundRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	siteID := strings.TrimSpace(req.SiteID)
	if !isSafeSiteID(siteID) {
		writeError(w, http.StatusBadRequest, "invalid site_id")
		return
	}
	if _, err := h.queries.GetSiteByID(r.Context(), siteID); err != nil {
		writeError(w, http.StatusNotFound, "unknown site_id")
		return
	}

	visitorID := strings.TrimSpace(req.VisitorID)
	fingerprint := strings.TrimSpace(req.Fingerprint)
	if visitorID == "" && fingerprint == "" {
		writeError(w, http.StatusBadRequest, "must provide visitor_id or fingerprint")
		return
	}

	metadataJSON := encodeMetadata(req.Metadata)

	var rowsAffected int64
	if visitorID != "" {
		rowsAffected, err = h.queries.UpsertVisitorMetadataByVisitorID(r.Context(), store.UpsertVisitorMetadataByVisitorIDParams{
			MetadataJson:        metadataJSON,
			MetadataExpiresAt:   strings.TrimSpace(req.ExpiresAt),
			IdentityConfirmedAt: strings.TrimSpace(req.IdentityConfirmedAt),
			SiteID:              siteID,
			VisitorID:           visitorID,
		})
	}
	if err != nil && !errors.Is(err, io.EOF) {
		slog.Error("track: inbound upsert by visitor_id", "site_id", siteID, "err", err)
		writeError(w, http.StatusInternalServerError, "failed to record metadata")
		return
	}

	if rowsAffected == 0 && fingerprint != "" {
		rowsAffected, err = h.queries.UpsertVisitorMetadataByFingerprint(r.Context(), store.UpsertVisitorMetadataByFingerprintParams{
			MetadataJson:        metadataJSON,
			MetadataExpiresAt:   strings.TrimSpace(req.ExpiresAt),
			IdentityConfirmedAt: strings.TrimSpace(req.IdentityConfirmedAt),
			SiteID:              siteID,
			Fingerprint:         fingerprint,
		})
		if err != nil {
			slog.Error("track: inbound upsert by fingerprint", "site_id", siteID, "err", err)
			writeError(w, http.StatusInternalServerError, "failed to record metadata")
			return
		}
	}

	if rowsAffected == 0 {
		writeError(w, http.StatusNotFound, "no matching visit_sessions row")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// encodeMetadata serialises the inbound metadata blob to JSON for storage.
// nil/empty -> "{}". Map values can be string/number/bool; the DSL evaluator
// coerces at read time. Marshal errors are rare (typically a non-encodable
// chan or func value the CRM somehow sent) but get logged so the silent
// "{}" fallback is at least observable.
func encodeMetadata(m map[string]any) string {
	if len(m) == 0 {
		return "{}"
	}
	b, err := json.Marshal(m)
	if err != nil {
		slog.Warn("track: encodeMetadata marshal failed, falling back to empty", "err", err)
		return "{}"
	}
	return string(b)
}
