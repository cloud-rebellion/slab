// Package handlers: conversion_goals.go (Phase 31.1, 2026-05-06).
//
// Admin CRUD for per-site conversion goals plus the read-side
// aggregation that powers the Analytics tab Goals panel. Routes:
//
//	GET    /api/sites/{siteID}/goals                list (active + inactive)
//	POST   /api/sites/{siteID}/goals                create
//	PATCH  /api/sites/{siteID}/goals/{goalID}       update
//	DELETE /api/sites/{siteID}/goals/{goalID}       delete (cascades events)
//	GET    /api/sites/{siteID}/analytics/goals?since=7d   per-goal counts + rates
//
// All routes are mounted inside the site-access middleware group so
// cross-site reads are blocked at the boundary.
package handlers

import (
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/bright-interaction/slab/internal/conversions"
	"github.com/bright-interaction/slab/internal/store"
)

// ConversionGoalsHandler owns the admin + aggregate routes.
type ConversionGoalsHandler struct {
	queries *store.Queries
}

func NewConversionGoalsHandler(queries *store.Queries) *ConversionGoalsHandler {
	return &ConversionGoalsHandler{queries: queries}
}

type goalView struct {
	ID            string `json:"id"`
	SiteID        string `json:"site_id"`
	Slug          string `json:"slug"`
	Name          string `json:"name"`
	MatchType     string `json:"match_type"`
	MatchValue    string `json:"match_value"`
	ValueCents    int64  `json:"value_cents"`
	ValueCurrency string `json:"value_currency"`
	Active        bool   `json:"active"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

func goalToView(g store.ConversionGoal) goalView {
	return goalView{
		ID:            g.ID,
		SiteID:        g.SiteID,
		Slug:          g.Slug,
		Name:          g.Name,
		MatchType:     g.MatchType,
		MatchValue:    g.MatchValue,
		ValueCents:    g.ValueCents,
		ValueCurrency: g.ValueCurrency,
		Active:        g.Active != 0,
		CreatedAt:     g.CreatedAt,
		UpdatedAt:     g.UpdatedAt,
	}
}

// goalSlugRE enforces a stable, URL-safe slug. The slug is what the
// JS API and embedded events refer to (e.g. atomic.track('signup')).
var goalSlugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// goalRequest is the body accepted by Create + Update.
type goalRequest struct {
	Slug          string `json:"slug"`
	Name          string `json:"name"`
	MatchType     string `json:"match_type"`
	MatchValue    string `json:"match_value"`
	ValueCents    int64  `json:"value_cents"`
	ValueCurrency string `json:"value_currency"`
	Active        *bool  `json:"active"` // pointer so PATCH can omit
}

// validateGoalRequest enforces the constraints we want before the
// row hits the DB. Returns "" on success or the user-facing error
// message on failure.
func validateGoalRequest(req goalRequest, isCreate bool) string {
	if isCreate {
		if !goalSlugRE.MatchString(req.Slug) {
			return "slug must be lowercase letters, digits, -, _ (max 63 chars, no leading dash)"
		}
	}
	if strings.TrimSpace(req.Name) == "" {
		return "name is required"
	}
	if !conversions.IsValidMatchType(req.MatchType) {
		return "match_type must be url_pattern, event_name, or form_submit"
	}
	if strings.TrimSpace(req.MatchValue) == "" {
		return "match_value is required"
	}
	if len(req.MatchValue) > 1024 {
		return "match_value too long"
	}
	if req.ValueCents < 0 {
		return "value_cents must be non-negative"
	}
	if req.ValueCurrency != "" && len(req.ValueCurrency) != 3 {
		return "value_currency must be a 3-letter code"
	}
	return ""
}

// List returns every goal for a site (active + inactive). Mounted
// behind RequireSiteAccess so cross-site lists are blocked.
//
// GET /api/sites/{siteID}/goals
func (h *ConversionGoalsHandler) List(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	rows, err := h.queries.ListAllGoalsBySite(r.Context(), siteID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load goals")
		return
	}
	out := make([]goalView, 0, len(rows))
	for _, g := range rows {
		out = append(out, goalToView(g))
	}
	writeJSON(w, http.StatusOK, map[string]any{"goals": out})
}

// Create adds a new goal. Slug must be unique per site.
//
// POST /api/sites/{siteID}/goals
func (h *ConversionGoalsHandler) Create(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	var req goalRequest
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if msg := validateGoalRequest(req, true); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	currency := req.ValueCurrency
	if currency == "" {
		currency = "EUR"
	}
	active := int64(1)
	if req.Active != nil && !*req.Active {
		active = 0
	}
	id := newID()
	if err := h.queries.CreateGoal(r.Context(), store.CreateGoalParams{
		ID:            id,
		SiteID:        siteID,
		Slug:          req.Slug,
		Name:          strings.TrimSpace(req.Name),
		MatchType:     req.MatchType,
		MatchValue:    req.MatchValue,
		ValueCents:    req.ValueCents,
		ValueCurrency: currency,
		Active:        active,
	}); err != nil {
		writeError(w, http.StatusBadRequest, "Failed to create goal (slug taken?)")
		return
	}
	row, err := h.queries.GetGoalByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to read created goal")
		return
	}
	AuditLog(r.Context(), h.queries, r, siteID, AuditActionCreate, "conversion_goal", id, map[string]any{
		"slug":       row.Slug,
		"match_type": row.MatchType,
	})
	writeJSON(w, http.StatusCreated, goalToView(row))
}

// Update patches a goal. slug is immutable; everything else can
// change.
//
// PATCH /api/sites/{siteID}/goals/{goalID}
func (h *ConversionGoalsHandler) Update(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	id := urlParam(r, "goalID")
	row, err := h.queries.GetGoalByID(r.Context(), id)
	if err != nil || row.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Goal not found")
		return
	}
	var req goalRequest
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if msg := validateGoalRequest(req, false); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	currency := req.ValueCurrency
	if currency == "" {
		currency = row.ValueCurrency
	}
	active := row.Active
	if req.Active != nil {
		if *req.Active {
			active = 1
		} else {
			active = 0
		}
	}
	if err := h.queries.UpdateGoal(r.Context(), store.UpdateGoalParams{
		ID:            id,
		Name:          strings.TrimSpace(req.Name),
		MatchType:     req.MatchType,
		MatchValue:    req.MatchValue,
		ValueCents:    req.ValueCents,
		ValueCurrency: currency,
		Active:        active,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to update goal")
		return
	}
	updated, _ := h.queries.GetGoalByID(r.Context(), id)
	AuditLog(r.Context(), h.queries, r, siteID, AuditActionUpdate, "conversion_goal", id, map[string]any{
		"slug": updated.Slug,
	})
	writeJSON(w, http.StatusOK, goalToView(updated))
}

// Delete removes a goal. The conversion_events FK has ON DELETE
// CASCADE so historical conversions are dropped along with the
// goal definition; that's the desired semantic for "operator
// realised the goal was wrong, get rid of it cleanly".
//
// DELETE /api/sites/{siteID}/goals/{goalID}
func (h *ConversionGoalsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	id := urlParam(r, "goalID")
	row, err := h.queries.GetGoalByID(r.Context(), id)
	if err != nil || row.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Goal not found")
		return
	}
	if err := h.queries.DeleteGoal(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete goal")
		return
	}
	AuditLog(r.Context(), h.queries, r, siteID, AuditActionDelete, "conversion_goal", id, map[string]any{
		"slug": row.Slug,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// goalAggregate is the per-goal view returned by /analytics/goals.
type goalAggregate struct {
	goalView
	Conversions       int64   `json:"conversions"`
	UniqueConverters  int64   `json:"unique_converters"`
	ConversionRatePct float64 `json:"conversion_rate_pct"`
	TotalValueCents   int64   `json:"total_value_cents"`
}

// AnalyticsGoals returns goals + aggregated counts over the given
// since range. The denominator for conversion_rate_pct is the
// site's unique-visitor count over the same range, so the rate is
// "fraction of unique visitors who triggered this goal" which is
// the meaningful per-goal-rate framing for marketing teams.
//
// GET /api/sites/{siteID}/analytics/goals?since=7d
func (h *ConversionGoalsHandler) AnalyticsGoals(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	since := parseSinceRange(r.URL.Query().Get("since"), 7)
	cutoff := time.Now().UTC().Add(-since).Format(time.RFC3339)

	goals, err := h.queries.ListAllGoalsBySite(r.Context(), siteID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load goals")
		return
	}
	uniqueVisitors, err := h.queries.CountUniqueVisitorsSince(r.Context(), store.CountUniqueVisitorsSinceParams{
		SiteID: siteID,
		Ts:     cutoff,
	})
	if err != nil {
		uniqueVisitors = 0
	}

	out := make([]goalAggregate, 0, len(goals))
	for _, g := range goals {
		conv, _ := h.queries.CountConversionsByGoal(r.Context(), store.CountConversionsByGoalParams{
			GoalID: g.ID,
			Ts:     cutoff,
		})
		uniq, _ := h.queries.CountUniqueConvertersByGoal(r.Context(), store.CountUniqueConvertersByGoalParams{
			GoalID: g.ID,
			Ts:     cutoff,
		})
		val, _ := h.queries.SumValueByGoal(r.Context(), store.SumValueByGoalParams{
			GoalID: g.ID,
			Ts:     cutoff,
		})
		valCents := int64(0)
		if v, ok := val.(int64); ok {
			valCents = v
		}
		rate := 0.0
		if uniqueVisitors > 0 {
			rate = float64(uniq) / float64(uniqueVisitors) * 100.0
		}
		out = append(out, goalAggregate{
			goalView:          goalToView(g),
			Conversions:       conv,
			UniqueConverters:  uniq,
			ConversionRatePct: rate,
			TotalValueCents:   valCents,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"since":           since.String(),
		"unique_visitors": uniqueVisitors,
		"goals":           out,
	})
}

// parseSinceRange interprets the same since values the existing
// /analytics/overview handler accepts (1d/7d/30d/90d/all). all
// becomes a 5-year window which is effectively "everything".
func parseSinceRange(s string, defaultDays int) time.Duration {
	switch s {
	case "1d":
		return 24 * time.Hour
	case "7d":
		return 7 * 24 * time.Hour
	case "30d":
		return 30 * 24 * time.Hour
	case "90d":
		return 90 * 24 * time.Hour
	case "all":
		return 5 * 365 * 24 * time.Hour
	}
	return time.Duration(defaultDays) * 24 * time.Hour
}
