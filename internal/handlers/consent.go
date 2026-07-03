package handlers

import (
	"encoding/csv"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bright-interaction/slab/internal/config"
	"github.com/bright-interaction/slab/internal/store"
)

// ConsentHandler exposes admin-side reads over the consent_records table .
// the GDPR proof-of-consent log atomicsite became system of record for after
// the CookieProof fold-in. Reads are site-scoped and require an
// authenticated admin (mounted behind the auth middleware in server.go).
type ConsentHandler struct {
	cfg     *config.Config
	queries *store.Queries
}

func NewConsentHandler(cfg *config.Config, queries *store.Queries) *ConsentHandler {
	return &ConsentHandler{cfg: cfg, queries: queries}
}

// proofRow is the JSON shape the dashboard receives. Mirrors the table but
// hides the IP hash to half its length (truncated for display). Server-side
// the full hash stays available for audit lookups.
type proofRow struct {
	ID          string         `json:"id"`
	CreatedAt   string         `json:"created_at"`
	CreatedAtMs int64          `json:"created_at_ms"`
	Domain      string         `json:"domain"`
	Method      string         `json:"method"`
	Version     int64          `json:"version"`
	Categories  map[string]any `json:"categories"`
	Page        string         `json:"page"`
	Referrer    string         `json:"referrer"`
	UserAgent   string         `json:"user_agent"`
	IPHashTrunc string         `json:"ip_hash_trunc"`
	GPCActive   bool           `json:"gpc_active"`
	SessionID   string         `json:"session_id,omitempty"`
}

func toProofRow(r store.ConsentRecord) proofRow {
	cats := map[string]any{}
	if r.CategoriesJson != "" {
		if err := decodeJSON(r.CategoriesJson, &cats); err != nil {
			slog.Warn("consent: invalid categories_json", "id", r.ID, "err", err.Error())
		}
	}
	return proofRow{
		ID:          r.ID,
		CreatedAt:   r.CreatedAtIso,
		CreatedAtMs: r.CreatedAt,
		Domain:      r.Domain,
		Method:      r.ConsentMethod,
		Version:     r.ConsentVersion,
		Categories:  cats,
		Page:        r.PageUrl,
		Referrer:    r.Referrer,
		UserAgent:   r.UserAgent,
		IPHashTrunc: truncateIPHash(r.IpHash),
		GPCActive:   r.GpcActive == 1,
		SessionID:   r.SessionID,
	}
}

func truncateIPHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// parseTimeRange pulls ?from and ?to from the request and converts them to
// unix-ms. Defaults: from = 30 days ago, to = now. Both accept RFC3339
// or unix-ms.
func parseTimeRange(r *http.Request) (int64, int64) {
	now := time.Now().UTC()
	to := now.UnixMilli()
	from := now.Add(-30 * 24 * time.Hour).UnixMilli()
	if v := r.URL.Query().Get("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			from = t.UnixMilli()
		} else if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			from = n
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			to = t.UnixMilli()
		} else if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			to = n
		}
	}
	// Guard inverted ranges: callers that pass from=tomorrow&to=yesterday
	// (or any swap) silently get empty SQL results otherwise. Swap rather
	// than reject so the dashboard renders a sane window for fat-fingers.
	if from > to {
		from, to = to, from
	}
	return from, to
}

// List returns paginated consent proofs for a site. Filters: from, to,
// method (one of accept-all/reject-all/custom/gpc/dns/do-not-sell). Default
// page size 50, max 200.
func (h *ConsentHandler) List(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	if !isSafeSiteID(siteID) {
		writeError(w, http.StatusBadRequest, "Invalid siteID")
		return
	}
	from, to := parseTimeRange(r)
	method := strings.TrimSpace(r.URL.Query().Get("method"))
	if method != "" && !validConsentMethods[method] {
		writeError(w, http.StatusBadRequest, "Invalid method filter")
		return
	}
	limit := int64(50)
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			if n > 200 {
				n = 200
			}
			limit = n
		}
	}
	offset := int64(0)
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			offset = n
		}
	}
	// Two query variants: with or without method filter. Splitting them
	// avoids sqlc generating positional ?6 references when sqlc.arg(method)
	// is reused in the same SQL, which the go-sqlite3 driver mis-binds and
	// returned datatype-mismatch 500s for every Proofs page load.
	var rows []store.ConsentRecord
	var total int64
	if method == "" {
		var qErr error
		rows, qErr = h.queries.ListConsentBySite(r.Context(), store.ListConsentBySiteParams{
			SiteID:      siteID,
			CreatedAt:   from,
			CreatedAt_2: to,
			Limit:       limit,
			Offset:      offset,
		})
		if qErr != nil {
			writeError(w, http.StatusInternalServerError, "Failed to list consent records")
			return
		}
		total, qErr = h.queries.CountConsentBySite(r.Context(), store.CountConsentBySiteParams{
			SiteID:      siteID,
			CreatedAt:   from,
			CreatedAt_2: to,
		})
		if qErr != nil {
			writeError(w, http.StatusInternalServerError, "Failed to count consent records")
			return
		}
	} else {
		var qErr error
		rows, qErr = h.queries.ListConsentBySiteByMethod(r.Context(), store.ListConsentBySiteByMethodParams{
			SiteID:        siteID,
			CreatedAt:     from,
			CreatedAt_2:   to,
			ConsentMethod: method,
			Limit:         limit,
			Offset:        offset,
		})
		if qErr != nil {
			writeError(w, http.StatusInternalServerError, "Failed to list consent records")
			return
		}
		total, qErr = h.queries.CountConsentBySiteByMethod(r.Context(), store.CountConsentBySiteByMethodParams{
			SiteID:        siteID,
			CreatedAt:     from,
			CreatedAt_2:   to,
			ConsentMethod: method,
		})
		if qErr != nil {
			writeError(w, http.StatusInternalServerError, "Failed to count consent records")
			return
		}
	}
	out := make([]proofRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, toProofRow(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"records": out,
		"total":   total,
		"limit":   limit,
		"offset":  offset,
		"from":    from,
		"to":      to,
	})
}

// Stats returns aggregate counters and a daily time-series for charting.
// Same time-range semantics as List.
func (h *ConsentHandler) Stats(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	if !isSafeSiteID(siteID) {
		writeError(w, http.StatusBadRequest, "Invalid siteID")
		return
	}
	from, to := parseTimeRange(r)
	stats, err := h.queries.ConsentStatsBySite(r.Context(), store.ConsentStatsBySiteParams{
		SiteID:      siteID,
		CreatedAt:   from,
		CreatedAt_2: to,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch stats")
		return
	}
	daily, err := h.queries.ConsentDailyBySite(r.Context(), store.ConsentDailyBySiteParams{
		SiteID:      siteID,
		CreatedAt:   from,
		CreatedAt_2: to,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch time-series")
		return
	}
	type daySplit struct {
		Day      string           `json:"day"`
		ByMethod map[string]int64 `json:"by_method"`
		Total    int64            `json:"total"`
	}
	dayIndex := map[string]*daySplit{}
	dayOrder := []string{}
	for _, row := range daily {
		day := fmt.Sprint(row.Day)
		ds, ok := dayIndex[day]
		if !ok {
			ds = &daySplit{Day: day, ByMethod: map[string]int64{}}
			dayIndex[day] = ds
			dayOrder = append(dayOrder, day)
		}
		ds.ByMethod[row.ConsentMethod] += row.N
		ds.Total += row.N
	}
	series := make([]daySplit, 0, len(dayOrder))
	for _, d := range dayOrder {
		series = append(series, *dayIndex[d])
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total":       stats.Total,
		"accepts":     nullFloatToInt64(stats.Accepts),
		"rejects":     nullFloatToInt64(stats.Rejects),
		"customs":     nullFloatToInt64(stats.Customs),
		"gpcs":        nullFloatToInt64(stats.Gpcs),
		"dns":         nullFloatToInt64(stats.DnsCount),
		"do_not_sell": nullFloatToInt64(stats.DoNotSellCount),
		"daily":       series,
		"from":        from,
		"to":          to,
	})
}

// StatsByCategory returns per-category opt-in / opt-out counts derived
// from the categories JSON in each proof. Useful for the admin Cookies
// page to answer "what % of visitors said yes to marketing?" without
// post-processing the CSV. Computed in Go because SQLite has no first-
// class JSON aggregation across columns; the volume is bounded by the
// retention window so this is cheap.
//
// Response shape:
//
//	{
//	  "from": <unix-ms>, "to": <unix-ms>, "total": N,
//	  "categories": {
//	    "analytics": {"granted": A, "denied": B, "rate": A/(A+B)},
//	    "marketing": {...},
//	    "preferences": {...},
//	    "necessary": {...}
//	  }
//	}
//
// The `necessary` row is reported as fully granted for completeness;
// the widget never asks consent for required categories so its denied
// count is always 0.
func (h *ConsentHandler) StatsByCategory(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	if !isSafeSiteID(siteID) {
		writeError(w, http.StatusBadRequest, "Invalid siteID")
		return
	}
	from, to := parseTimeRange(r)
	rows, err := h.queries.StreamConsentBySite(r.Context(), store.StreamConsentBySiteParams{
		SiteID:      siteID,
		CreatedAt:   from,
		CreatedAt_2: to,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch consent records")
		return
	}
	type catCounts struct {
		Granted int64   `json:"granted"`
		Denied  int64   `json:"denied"`
		Rate    float64 `json:"rate"`
	}
	categories := map[string]*catCounts{
		"necessary":   {},
		"analytics":   {},
		"marketing":   {},
		"preferences": {},
	}
	total := int64(0)
	for _, row := range rows {
		total++
		cats := map[string]any{}
		if row.CategoriesJson != "" {
			_ = decodeJSON(row.CategoriesJson, &cats)
		}
		for name, cc := range categories {
			v, ok := cats[name]
			granted := false
			switch x := v.(type) {
			case bool:
				granted = x
			case float64:
				granted = x != 0
			case string:
				lc := strings.ToLower(strings.TrimSpace(x))
				granted = lc == "true" || lc == "1" || lc == "yes" || lc == "on"
			}
			// Necessary defaults to granted when missing.
			if !ok && name == "necessary" {
				granted = true
			}
			if granted {
				cc.Granted++
			} else {
				cc.Denied++
			}
		}
	}
	for _, cc := range categories {
		denom := cc.Granted + cc.Denied
		if denom > 0 {
			cc.Rate = float64(cc.Granted) / float64(denom)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"from":       from,
		"to":         to,
		"total":      total,
		"categories": categories,
	})
}

// Purge deletes consent_records for the site older than `days` days. Used
// for GDPR right-to-be-forgotten / retention enforcement at the admin's
// request, in addition to the daily retention manager.
//
// `days` is a query param; minimum 1 (we never let an admin nuke the
// entire log in one call), default rejected to force the admin to think.
//
// Response: {deleted: N, days: X}.
func (h *ConsentHandler) Purge(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	if !isSafeSiteID(siteID) {
		writeError(w, http.StatusBadRequest, "Invalid siteID")
		return
	}
	daysStr := strings.TrimSpace(r.URL.Query().Get("days"))
	if daysStr == "" {
		writeError(w, http.StatusBadRequest, "Missing required query param: days")
		return
	}
	days, err := strconv.Atoi(daysStr)
	if err != nil || days < 1 || days > 36500 {
		writeError(w, http.StatusBadRequest, "days must be an integer between 1 and 36500")
		return
	}
	cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
	deleted, err := h.queries.DeleteConsentBySiteOlderThan(r.Context(), store.DeleteConsentBySiteOlderThanParams{
		SiteID:    siteID,
		CreatedAt: cutoff,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to purge consent records")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": deleted,
		"days":    days,
	})
}

// ExportCSV streams the full proof log for a date range as CSV. Designed
// for compliance audits ("show me every consent record from Q1 2026") so it
// uses StreamConsentBySite (no pagination) and writes directly to the
// response.
// MaxCSVExportRows caps a single ExportCSV response. Audit M5: without
// the cap, an admin can pull the entire consent_records table for the
// site (potentially millions of rows after years of traffic), pinning
// memory + bandwidth. 100,000 covers a real audit window for the
// largest plausible site; admins who need more can paginate by
// narrowing ?from / ?to.
const MaxCSVExportRows = 100_000

func (h *ConsentHandler) ExportCSV(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	if !isSafeSiteID(siteID) {
		writeError(w, http.StatusBadRequest, "Invalid siteID")
		return
	}
	from, to := parseTimeRange(r)
	rows, err := h.queries.StreamConsentBySite(r.Context(), store.StreamConsentBySiteParams{
		SiteID:      siteID,
		CreatedAt:   from,
		CreatedAt_2: to,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch consent records")
		return
	}
	if len(rows) > MaxCSVExportRows {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("Export contains %d rows (max %d). Narrow the from/to window and try again.",
				len(rows), MaxCSVExportRows))
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="consent-records-%s.csv"`, siteID))
	cw := csv.NewWriter(w)
	defer cw.Flush()
	_ = cw.Write([]string{
		"id", "created_at_iso", "created_at_ms", "domain", "method", "version",
		"categories_json", "gpc_active", "session_id", "page_url", "referrer",
		"user_agent", "ip_hash_trunc",
	})
	for _, row := range rows {
		_ = cw.Write([]string{
			row.ID,
			row.CreatedAtIso,
			strconv.FormatInt(row.CreatedAt, 10),
			row.Domain,
			row.ConsentMethod,
			strconv.FormatInt(row.ConsentVersion, 10),
			row.CategoriesJson,
			strconv.FormatInt(row.GpcActive, 10),
			row.SessionID,
			row.PageUrl,
			row.Referrer,
			row.UserAgent,
			// Truncated to match the JSON list endpoint and the dashboard
			// display. The full hash is never useful for compliance audits
			// (the salt rotates daily; the hash isn't reversible) but does
			// expose unnecessary fingerprintability if the CSV is shared.
			truncateIPHash(row.IpHash),
		})
	}
}

// Get returns a single proof record (for legal request lookup by ID).
func (h *ConsentHandler) Get(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	id := urlParam(r, "recordID")
	if !isSafeSiteID(siteID) || strings.TrimSpace(id) == "" {
		writeError(w, http.StatusBadRequest, "Invalid path")
		return
	}
	row, err := h.queries.GetConsentByID(r.Context(), store.GetConsentByIDParams{
		ID:     id,
		SiteID: siteID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "Record not found")
		return
	}
	writeJSON(w, http.StatusOK, toProofRow(row))
}
