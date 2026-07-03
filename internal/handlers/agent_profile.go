// Agent-side site profile + settings API.
//
// Mirrors the admin endpoints so AI agents can do everything the human
// admin can do via the Profile and Settings pages. Auth via X-Agent-Key
// (the agent middleware already runs on /api/agent/*); writes require
// the "write" capability declared on the agent key.
//
// SiteID is read from the agent identity, not from the URL, so an agent
// scoped to site A cannot manipulate site B even if the URL targets it.

package handlers

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/bright-interaction/slab/internal/store"
)

// --- Profile read / write ---

type agentProfilePayload struct {
	BusinessName   *string `json:"business_name,omitempty"`
	RegistrationNr *string `json:"registration_nr,omitempty"`
	Country        *string `json:"country,omitempty"`
	ContactEmail   *string `json:"contact_email,omitempty"`
	ContactPhone   *string `json:"contact_phone,omitempty"`
	PrivacyEmail   *string `json:"privacy_email,omitempty"`
	SecurityEmail  *string `json:"security_email,omitempty"`
	AddressLine1   *string `json:"address_line1,omitempty"`
	AddressLine2   *string `json:"address_line2,omitempty"`
	City           *string `json:"city,omitempty"`
	PostalCode     *string `json:"postal_code,omitempty"`
}

// GetProfile returns the site profile (legal contacts, address, etc.) for
// the agent's site. Drives Organization JSON-LD, security.txt, and the
// auto-generated legal pages.
//
// GET /api/agent/profile
func (h *AgentHandler) GetProfile(w http.ResponseWriter, r *http.Request) {
	a := requireAgent(w, r)
	if a == nil {
		return
	}
	p, err := h.queries.GetSiteProfile(r.Context(), a.SiteID)
	if err != nil {
		// Not having a profile yet is normal for fresh sites; respond with
		// an empty payload rather than 404 so the agent doesn't have to
		// special-case the first call.
		writeJSON(w, http.StatusOK, agentProfilePayload{})
		return
	}
	writeJSON(w, http.StatusOK, agentProfilePayload{
		BusinessName:   strPtr(p.BusinessName),
		RegistrationNr: strPtr(p.RegistrationNr),
		Country:        strPtr(p.Country),
		ContactEmail:   strPtr(p.ContactEmail),
		ContactPhone:   strPtr(p.ContactPhone),
		PrivacyEmail:   strPtr(p.PrivacyEmail),
		SecurityEmail:  strPtr(p.SecurityEmail),
		AddressLine1:   strPtr(p.AddressLine1),
		AddressLine2:   strPtr(p.AddressLine2),
		City:           strPtr(p.City),
		PostalCode:     strPtr(p.PostalCode),
	})
}

// UpdateProfile applies a partial patch to the site profile. Any field left
// nil is preserved. Empty string is treated as "clear this field".
//
// PATCH /api/agent/profile
func (h *AgentHandler) UpdateProfile(w http.ResponseWriter, r *http.Request) {
	a := requireAgent(w, r)
	if a == nil {
		return
	}
	if !requireWrite(w, a) {
		return
	}

	var req agentProfilePayload
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}

	// Read current state, overlay the patch, upsert. UpsertSiteProfile is a
	// full-row write, so we have to load first to avoid clearing fields the
	// caller didn't touch. A read ERROR (unlike no-row-yet) must abort:
	// overlaying a partial PATCH onto a zero-value current would
	// full-row-upsert empty strings over every field the caller omitted.
	current, err := h.queries.GetSiteProfile(r.Context(), a.SiteID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "Failed to load current profile; patch not applied")
		return
	}

	pickStr := func(p *string, fallback string) string {
		if p == nil {
			return fallback
		}
		return *p
	}

	id := current.ID
	if id == "" {
		id = newID()
	}

	if err := h.queries.UpsertSiteProfile(r.Context(), store.UpsertSiteProfileParams{
		ID:             id,
		SiteID:         a.SiteID,
		BusinessName:   pickStr(req.BusinessName, current.BusinessName),
		RegistrationNr: pickStr(req.RegistrationNr, current.RegistrationNr),
		Country:        pickStr(req.Country, current.Country),
		ContactEmail:   pickStr(req.ContactEmail, current.ContactEmail),
		ContactPhone:   pickStr(req.ContactPhone, current.ContactPhone),
		PrivacyEmail:   pickStr(req.PrivacyEmail, current.PrivacyEmail),
		SecurityEmail:  pickStr(req.SecurityEmail, current.SecurityEmail),
		AddressLine1:   pickStr(req.AddressLine1, current.AddressLine1),
		AddressLine2:   pickStr(req.AddressLine2, current.AddressLine2),
		City:           pickStr(req.City, current.City),
		PostalCode:     pickStr(req.PostalCode, current.PostalCode),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to save profile")
		return
	}

	updated, _ := h.queries.GetSiteProfile(r.Context(), a.SiteID)
	writeJSON(w, http.StatusOK, agentProfilePayload{
		BusinessName:   strPtr(updated.BusinessName),
		RegistrationNr: strPtr(updated.RegistrationNr),
		Country:        strPtr(updated.Country),
		ContactEmail:   strPtr(updated.ContactEmail),
		ContactPhone:   strPtr(updated.ContactPhone),
		PrivacyEmail:   strPtr(updated.PrivacyEmail),
		SecurityEmail:  strPtr(updated.SecurityEmail),
		AddressLine1:   strPtr(updated.AddressLine1),
		AddressLine2:   strPtr(updated.AddressLine2),
		City:           strPtr(updated.City),
		PostalCode:     strPtr(updated.PostalCode),
	})
}
