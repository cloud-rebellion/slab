package handlers

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bright-interaction/slab/internal/analyticsdb"
	"github.com/bright-interaction/slab/internal/config"
	"github.com/bright-interaction/slab/internal/store"
)

// CookieAnalyticsHandler exposes the stitched analytics surface that
// reads consent_records + visit_events + visit_sessions + visit_engagement
// through the DuckDB ATTACH on the SQLite file. All endpoints are
// authenticated admin routes scoped to a site.
//
// When analyticsMgr is nil (DuckDB failed to open at boot, or the build
// shipped without CGO), every endpoint returns 503 with a human-readable
// error so the dashboard can degrade gracefully instead of throwing.
type CookieAnalyticsHandler struct {
	cfg          *config.Config
	queries      *store.Queries
	analyticsMgr *analyticsdb.Manager
}

// NewCookieAnalyticsHandler constructs the handler. Pass nil for
// analyticsMgr when DuckDB is unavailable; all routes will return 503
// with a "stitched analytics unavailable" body in that case.
func NewCookieAnalyticsHandler(cfg *config.Config, queries *store.Queries, mgr *analyticsdb.Manager) *CookieAnalyticsHandler {
	return &CookieAnalyticsHandler{cfg: cfg, queries: queries, analyticsMgr: mgr}
}

// available is the precondition guard every endpoint runs first. Returns
// true when both the route's site_id is well-formed AND the manager is
// non-nil. On failure it has already written the response.
func (h *CookieAnalyticsHandler) available(w http.ResponseWriter, r *http.Request) (string, bool) {
	siteID := urlParam(r, "siteID")
	if !isSafeSiteID(siteID) {
		writeError(w, http.StatusBadRequest, "Invalid siteID")
		return "", false
	}
	if h.analyticsMgr == nil {
		writeError(w, http.StatusServiceUnavailable, "Stitched analytics unavailable: DuckDB layer not initialised. Check server logs.")
		return "", false
	}
	return siteID, true
}

// analyticsQueryTimeout caps any single stitched-query DB call. Closes
// audit H6: without this, a query against a large visit_events table
// could hang for minutes; the HTTP server's WriteTimeout (120s) closes
// the connection but the DuckDB worker keeps running, exhausting the
// 8-conn pool under repeated bad queries. 30s gives complex stitched
// queries plenty of room while bounding the worst case.
const analyticsQueryTimeout = 30 * time.Second

// queryCtx returns a context derived from r.Context() with the analytics
// query timeout applied. Caller defers the cancel.
func queryCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), analyticsQueryTimeout)
}

// _qctx is the inline-form helper for handlers that want a one-line
// `f(_qctx(r), ...)` call. The cancel is intentionally backgrounded via
// time.AfterFunc so the timeout still fires at analyticsQueryTimeout
// even though the caller doesn't hold the CancelFunc. This is a
// pragmatic shape for the existing handlers; the explicit ctx/cancel
// pattern is preferred for new handlers (see Funnel as the example).
func _qctx(r *http.Request) context.Context {
	ctx, cancel := context.WithTimeout(r.Context(), analyticsQueryTimeout)
	// Schedule cancel after the timeout window elapses to release
	// resources even if the caller never explicitly cancels. This is
	// equivalent to deferring cancel() in the handler body but keeps
	// the call sites one-line.
	time.AfterFunc(analyticsQueryTimeout+time.Second, cancel)
	return ctx
}

// parseTimeWindow reads ?from=&to= as either RFC3339 or unix-ms. Default:
// last 30 days. Returns the same TimeRange the analyticsdb queries expect.
func parseTimeWindow(r *http.Request) analyticsdb.TimeRange {
	now := time.Now().UTC()
	from := now.AddDate(0, 0, -30)
	to := now
	if v := r.URL.Query().Get("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			from = t
		} else if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			from = time.UnixMilli(n).UTC()
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			to = t
		} else if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			to = time.UnixMilli(n).UTC()
		}
	}
	return analyticsdb.NewTimeRange(from, to)
}

// Funnel returns the headline KPIs (visitors / consenting / accepted /
// rejected / GPC / abandoned) over the requested window.
//
// GET /api/sites/{siteID}/cookies/analytics/funnel?from=&to=
func (h *CookieAnalyticsHandler) Funnel(w http.ResponseWriter, r *http.Request) {
	siteID, ok := h.available(w, r)
	if !ok {
		return
	}
	tr := parseTimeWindow(r)
	ctx, cancel := queryCtx(r)
	defer cancel()
	kpis, err := h.analyticsMgr.FunnelKPIs(ctx, siteID, tr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to compute funnel: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"site_id": siteID,
		"from":    tr.FromISO,
		"to":      tr.ToISO,
		"kpis":    kpis,
	})
}

// PreConsent returns the legal-data-gap report: how many pageviews
// happened before any consent decision (or with no consent at all).
//
// GET /api/sites/{siteID}/cookies/analytics/pre-consent?from=&to=
func (h *CookieAnalyticsHandler) PreConsent(w http.ResponseWriter, r *http.Request) {
	siteID, ok := h.available(w, r)
	if !ok {
		return
	}
	tr := parseTimeWindow(r)
	share, err := h.analyticsMgr.PreConsentTrafficShare(_qctx(r), siteID, tr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to compute pre-consent share: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, share)
}

// TimeToConsent reports P50 / P90 / mean seconds from first pageview to
// first consent decision. Lower = banner is doing its job.
//
// GET /api/sites/{siteID}/cookies/analytics/time-to-consent?from=&to=
func (h *CookieAnalyticsHandler) TimeToConsent(w http.ResponseWriter, r *http.Request) {
	siteID, ok := h.available(w, r)
	if !ok {
		return
	}
	tr := parseTimeWindow(r)
	t, err := h.analyticsMgr.TimeToConsent(_qctx(r), siteID, tr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to compute time-to-consent: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// EngagedAcceptRate splits visitors into engaged vs not-engaged cohorts
// and reports accept-all rates.
//
// GET /api/sites/{siteID}/cookies/analytics/engaged-accept-rate?from=&to=
func (h *CookieAnalyticsHandler) EngagedAcceptRate(w http.ResponseWriter, r *http.Request) {
	siteID, ok := h.available(w, r)
	if !ok {
		return
	}
	tr := parseTimeWindow(r)
	er, err := h.analyticsMgr.EngagedAcceptRate(_qctx(r), siteID, tr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to compute engaged accept rate: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, er)
}

// TopAcceptingPages returns the entry-page paths with the highest
// accept-all rate (visitors >= 10).
//
// GET /api/sites/{siteID}/cookies/analytics/top-accepting-pages?from=&to=&limit=
func (h *CookieAnalyticsHandler) TopAcceptingPages(w http.ResponseWriter, r *http.Request) {
	siteID, ok := h.available(w, r)
	if !ok {
		return
	}
	tr := parseTimeWindow(r)
	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	rows, err := h.analyticsMgr.TopAcceptingPages(_qctx(r), siteID, tr, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to compute top accepting pages: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"limit": limit,
		"rows":  rows,
	})
}

// DailySplit returns the per-day stacked-bar data for the analytics tab.
//
// GET /api/sites/{siteID}/cookies/analytics/daily?from=&to=
func (h *CookieAnalyticsHandler) DailySplit(w http.ResponseWriter, r *http.Request) {
	siteID, ok := h.available(w, r)
	if !ok {
		return
	}
	tr := parseTimeWindow(r)
	rows, err := h.analyticsMgr.ConsentDailySplit(_qctx(r), siteID, tr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch daily split: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows})
}

// fingerprintRE constrains the {fingerprint} URL param to a strict
// alphanumeric / dash / underscore shape. Audit L6: without this, a
// path-encoded special character could reach the SQL layer (currently
// safe via parameterised queries, but defence in depth) or confuse
// downstream log parsers that assume an opaque token.
var fingerprintRE = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// VisitorJourney returns a single visitor's stitched timeline. The
// fingerprint comes from the URL path; the time window is the same
// ?from=&to= used elsewhere.
//
// GET /api/sites/{siteID}/cookies/analytics/journey/{fingerprint}?from=&to=
func (h *CookieAnalyticsHandler) VisitorJourney(w http.ResponseWriter, r *http.Request) {
	siteID, ok := h.available(w, r)
	if !ok {
		return
	}
	fp := strings.TrimSpace(urlParam(r, "fingerprint"))
	if !fingerprintRE.MatchString(fp) {
		writeError(w, http.StatusBadRequest, "Invalid fingerprint")
		return
	}
	tr := parseTimeWindow(r)
	steps, err := h.analyticsMgr.VisitorJourney(_qctx(r), siteID, fp, tr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to fetch visitor journey: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"site_id":     siteID,
		"fingerprint": fp,
		"steps":       steps,
	})
}
