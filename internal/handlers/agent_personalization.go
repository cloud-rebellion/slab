// Phase 18.4: agent-side personalization debug endpoint.
//
// The agent (and the human admin via the same key) sometimes needs to
// inspect what metadata the CRM has actually pushed for a given visitor:
// to verify that the inbound webhook is wired correctly, to debug why a
// conditional block isn't showing, to dump example values when authoring
// new conditions. This endpoint is the only first-party readback path.
//
// Identity is bound by fingerprint OR visitor_id (both query params,
// fingerprint takes precedence). The siteID comes from the agent identity,
// never the URL, so a key for site A cannot peek at site B.

package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/bright-interaction/slab/internal/store"
)

// GetVisitorMetadata returns the visit_sessions row for a given fingerprint
// (or visitor_id) on the agent's site. Useful for debugging the CRM
// roundtrip and for the agent to read example values when authoring
// conditional blocks.
//
//	GET /api/agent/visitor-metadata?fingerprint=...
//	GET /api/agent/visitor-metadata?visitor_id=...
//
// 200 - JSON {visitor_id, fingerprint, identified_at, identity_confirmed_at,
//
//	metadata: {...}, metadata_expires_at}
//
// 400 - missing identifier
// 404 - no matching row on this site
func (h *AgentHandler) GetVisitorMetadata(w http.ResponseWriter, r *http.Request) {
	a := requireAgent(w, r)
	if a == nil {
		return
	}

	fingerprint := strings.TrimSpace(r.URL.Query().Get("fingerprint"))
	visitorID := strings.TrimSpace(r.URL.Query().Get("visitor_id"))
	if fingerprint == "" && visitorID == "" {
		writeError(w, http.StatusBadRequest, "fingerprint or visitor_id query param required")
		return
	}

	if fingerprint != "" {
		row, err := h.queries.GetVisitorMetadataByFingerprint(r.Context(), store.GetVisitorMetadataByFingerprintParams{
			SiteID:      a.SiteID,
			Fingerprint: fingerprint,
		})
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "no visitor on this site with that fingerprint")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to load visitor")
			return
		}
		writeVisitorRow(w, row)
		return
	}

	// visitor_id path: cheap raw query because there is no by-visitor-id sqlc
	// query for visit_sessions and adding one for a debug endpoint would be
	// noise. Bound to the agent's site so cross-site leakage is impossible.
	const q = `SELECT id, site_id, fingerprint, visitor_id, email, consent_method,
		consent_categories_json, started_at, last_seen_at, page_count,
		identified_at, metadata_json, metadata_expires_at, identity_confirmed_at
	FROM visit_sessions WHERE site_id = ? AND visitor_id = ?`
	rows, err := h.queries.Raw().QueryContext(r.Context(), q, a.SiteID, visitorID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to query visitor")
		return
	}
	defer rows.Close()
	if !rows.Next() {
		writeError(w, http.StatusNotFound, "no visitor on this site with that visitor_id")
		return
	}
	var row store.VisitSession
	if err := rows.Scan(
		&row.ID, &row.SiteID, &row.Fingerprint, &row.VisitorID, &row.Email,
		&row.ConsentMethod, &row.ConsentCategoriesJson, &row.StartedAt,
		&row.LastSeenAt, &row.PageCount, &row.IdentifiedAt,
		&row.MetadataJson, &row.MetadataExpiresAt, &row.IdentityConfirmedAt,
	); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to scan visitor")
		return
	}
	writeVisitorRow(w, row)
}

func writeVisitorRow(w http.ResponseWriter, row store.VisitSession) {
	var meta map[string]any
	if row.MetadataJson != "" {
		_ = json.Unmarshal([]byte(row.MetadataJson), &meta)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"visitor_id":            row.VisitorID,
		"fingerprint":           row.Fingerprint,
		"email":                 row.Email,
		"identified_at":         row.IdentifiedAt,
		"identity_confirmed_at": row.IdentityConfirmedAt,
		"metadata":              meta,
		"metadata_expires_at":   row.MetadataExpiresAt,
		"started_at":            row.StartedAt,
		"last_seen_at":          row.LastSeenAt,
	})
}
