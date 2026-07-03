package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"
)

// visitorResponse is the shape /t/visitor returns to the browser hydration
// script. anonymous-tier fields are always present when we have any
// metadata; identified-tier fields are scrubbed when identity_confirmed_at
// is missing or older than analytics.identity_max_age_days. The client
// renders Anonymous-tier conditions ("picking up where you left off") in
// either case; Identified-tier conditions ("Welcome Joe") only when
// Identified is non-nil.
type visitorResponse struct {
	VisitorID           string         `json:"visitor_id"`
	Returning           bool           `json:"returning"`
	IdentifiedAt        string         `json:"identified_at,omitempty"`
	IdentityConfirmedAt string         `json:"identity_confirmed_at,omitempty"`
	Anonymous           map[string]any `json:"anonymous"`
	Identified          map[string]any `json:"identified"`
}

// identifiedKeys are the metadata keys we treat as PII and only echo back
// when identity_confirmed_at is fresh. Everything not in this list is
// always rendered (anonymous-tier).
var identifiedKeys = map[string]bool{
	"name":         true,
	"first_name":   true,
	"last_name":    true,
	"email":        true,
	"phone":        true,
	"company":      true,
	"company_name": true,
	"job_title":    true,
}

// Visitor returns the per-visitor metadata bound to the request's fingerprint.
// Cross-origin friendly: the server re-derives the fingerprint from request
// headers (IP + UA + Accept-Language + AnalyticsSalt), so the built site can
// fetch from <slug><BUILT_SITE_SUFFIX> to the admin host without needing the
// SameSite=Lax fingerprint cookie to round-trip cross-site.
//
// Identity is bound via the IP+UA derivation; only the actual visitor can
// satisfy it. The CORS layer echoes the requesting origin (never `*`) so a
// future credentials-bearing variant can drop in.
//
// GET /t/visitor?site_id=...
//
//	200 - JSON visitorResponse (Anonymous always populated when row exists,
//	      Identified populated only when identity is recently confirmed)
//	400 - missing or invalid site_id
//	404 - unknown site_id
func (h *TrackHandler) Visitor(w http.ResponseWriter, r *http.Request) {
	siteID := strings.TrimSpace(r.URL.Query().Get("site_id"))
	if !isSafeSiteID(siteID) {
		writeError(w, http.StatusBadRequest, "invalid site_id")
		return
	}
	if _, err := h.queries.GetSiteByID(r.Context(), siteID); err != nil {
		writeError(w, http.StatusNotFound, "unknown site_id")
		return
	}

	fp := authmw.DeriveFingerprint(
		authmw.ClientIP(r),
		r.Header.Get("User-Agent"),
		r.Header.Get("Accept-Language"),
		h.cfg.AnalyticsSalt,
	)

	row, err := h.queries.GetVisitorMetadataByFingerprint(r.Context(), store.GetVisitorMetadataByFingerprintParams{
		SiteID:      siteID,
		Fingerprint: fp,
	})

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	if errors.Is(err, sql.ErrNoRows) {
		json.NewEncoder(w).Encode(visitorResponse{
			Returning:  false,
			Anonymous:  map[string]any{},
			Identified: map[string]any{},
		})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load visitor")
		return
	}

	resp := visitorResponse{
		VisitorID:           row.VisitorID,
		Returning:           true,
		IdentifiedAt:        row.IdentifiedAt,
		IdentityConfirmedAt: row.IdentityConfirmedAt,
		Anonymous:           map[string]any{},
		Identified:          map[string]any{},
	}

	if row.MetadataExpiresAt != "" && metadataExpired(row.MetadataExpiresAt) {
		json.NewEncoder(w).Encode(resp)
		return
	}

	var meta map[string]any
	if row.MetadataJson != "" {
		_ = json.Unmarshal([]byte(row.MetadataJson), &meta)
	}

	identityFresh := identityIsFresh(row.IdentityConfirmedAt, h.identityMaxAgeDays(r.Context(), siteID))
	for k, v := range meta {
		if identifiedKeys[k] {
			if identityFresh {
				resp.Identified[k] = v
			}
			continue
		}
		resp.Anonymous[k] = v
	}

	json.NewEncoder(w).Encode(resp)
}

// metadataExpired returns true when the optional CRM-supplied expires_at
// has passed. RFC3339 format. Malformed values are ignored (treated as
// non-expiring) because dropping data on a parse error is worse than
// serving slightly stale data.
func metadataExpired(s string) bool {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return false
	}
	return time.Now().After(t)
}

// identityIsFresh decides whether identified-tier fields are safe to echo
// back. False when the timestamp is missing, malformed, or older than the
// configured window. Default window is 30 days unless the site sets
// analytics.identity_max_age_days.
func identityIsFresh(confirmedAt string, maxAgeDays int) bool {
	if confirmedAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, confirmedAt)
	if err != nil {
		return false
	}
	cutoff := time.Now().Add(-time.Duration(maxAgeDays) * 24 * time.Hour)
	return t.After(cutoff)
}

// identityMaxAgeDays reads the per-site analytics.identity_max_age_days
// setting. Default 30 if missing or malformed.
func (h *TrackHandler) identityMaxAgeDays(ctx context.Context, siteID string) int {
	const def = 30
	row, err := h.queries.GetSetting(ctx, store.GetSettingParams{
		SiteID:   siteID,
		Category: "analytics",
		Key:      "identity_max_age_days",
	})
	if err != nil {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(row.Value))
	if err != nil || n < 1 {
		return def
	}
	return n
}
