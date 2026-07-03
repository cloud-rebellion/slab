package analyticsdb

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// All queries below run against the ATTACHed SQLite schema "oltp" through
// DuckDB's columnar engine. The stitch keys are:
//
//   visit_events.fingerprint     == consent_records.fingerprint  (added 2026-05-01)
//   visit_events.fingerprint     == visit_sessions.fingerprint
//   visit_events.fingerprint     == visit_engagement.fingerprint
//
// Date-range filters universally use ISO RFC3339 strings against the
// `ts` (visit_events / visit_engagement) and `last_seen_at` (sessions)
// columns, and unix-ms integers against `consent_records.created_at`.

// TimeRange is the canonical date-window argument for every report.
// FromMs and ToMs cover unix-millis comparisons; FromISO and ToISO cover
// the RFC3339 columns. Caller fills both representations from the same
// time.Time so the query layer doesn't have to convert.
type TimeRange struct {
	FromMs  int64
	ToMs    int64
	FromISO string
	ToISO   string
}

// NewTimeRange builds a TimeRange from a from/to pair, or sensible
// defaults (last 30 days) when either is zero.
func NewTimeRange(from, to time.Time) TimeRange {
	if to.IsZero() {
		to = time.Now().UTC()
	}
	if from.IsZero() {
		from = to.AddDate(0, 0, -30)
	}
	return TimeRange{
		FromMs:  from.UnixMilli(),
		ToMs:    to.UnixMilli(),
		FromISO: from.UTC().Format(time.RFC3339),
		ToISO:   to.UTC().Format(time.RFC3339),
	}
}

// FunnelKPIs is the headline numbers that drive the cookie-analytics
// dashboard top-of-page cards. All counts are over the requested
// (site_id, time-window).
//
//	UniqueVisitors         distinct fingerprints with any visit_event
//	ConsentingVisitors     distinct fingerprints with any consent_record
//	AcceptAllVisitors      distinct fingerprints with method=accept-all
//	RejectAllVisitors      distinct fingerprints with method=reject-all
//	CustomVisitors         distinct fingerprints with method=custom
//	GPCVisitors            distinct fingerprints with method=gpc
//	IdentifiedVisitors     distinct fingerprints from visit_sessions where identified_at != ''
//	AbandonedBannerVisitors visitors who saw the banner (had visit_events) but never made a consent decision
type FunnelKPIs struct {
	UniqueVisitors          int64 `json:"unique_visitors"`
	ConsentingVisitors      int64 `json:"consenting_visitors"`
	AcceptAllVisitors       int64 `json:"accept_all_visitors"`
	RejectAllVisitors       int64 `json:"reject_all_visitors"`
	CustomVisitors          int64 `json:"custom_visitors"`
	GPCVisitors             int64 `json:"gpc_visitors"`
	DoNotSellVisitors       int64 `json:"do_not_sell_visitors"`
	IdentifiedVisitors      int64 `json:"identified_visitors"`
	AbandonedBannerVisitors int64 `json:"abandoned_banner_visitors"`
}

// FunnelKPIs runs a single stitched query that returns all 9 counters
// above. One scan over the relevant subset of visit_events +
// consent_records + visit_sessions, aggregated server-side in DuckDB.
//
// The LEFT JOIN to consent_records is the stitch that nobody else can
// do: it links pageviews to consent decisions on the same fingerprint
// so we can compute "visitors who saw the page but made no decision".
func (m *Manager) FunnelKPIs(ctx context.Context, siteID string, tr TimeRange) (FunnelKPIs, error) {
	const q = `
WITH ev AS (
    SELECT DISTINCT fingerprint
    FROM oltp.visit_events
    WHERE site_id = ? AND ts >= ? AND ts <= ?
),
cr AS (
    SELECT fingerprint, consent_method, gpc_active
    FROM oltp.consent_records
    WHERE site_id = ? AND created_at >= ? AND created_at <= ?
),
vs AS (
    SELECT DISTINCT fingerprint
    FROM oltp.visit_sessions
    WHERE site_id = ? AND identified_at != ''
      AND last_seen_at >= ? AND last_seen_at <= ?
)
SELECT
    (SELECT COUNT(*) FROM ev)                                                AS unique_visitors,
    (SELECT COUNT(DISTINCT fingerprint) FROM cr)                             AS consenting_visitors,
    (SELECT COUNT(DISTINCT fingerprint) FROM cr WHERE consent_method='accept-all')  AS accept_all,
    (SELECT COUNT(DISTINCT fingerprint) FROM cr WHERE consent_method='reject-all')  AS reject_all,
    (SELECT COUNT(DISTINCT fingerprint) FROM cr WHERE consent_method='custom')      AS custom_count,
    (SELECT COUNT(DISTINCT fingerprint) FROM cr WHERE consent_method='gpc' OR gpc_active=1) AS gpc_count,
    (SELECT COUNT(DISTINCT fingerprint) FROM cr WHERE consent_method='do-not-sell') AS dns_count,
    (SELECT COUNT(*) FROM vs)                                                AS identified,
    (SELECT COUNT(*) FROM ev WHERE fingerprint NOT IN (SELECT fingerprint FROM cr)) AS abandoned
`
	var k FunnelKPIs
	row := m.DB().QueryRowContext(ctx, q,
		siteID, tr.FromISO, tr.ToISO,
		siteID, tr.FromMs, tr.ToMs,
		siteID, tr.FromISO, tr.ToISO,
	)
	if err := row.Scan(
		&k.UniqueVisitors, &k.ConsentingVisitors,
		&k.AcceptAllVisitors, &k.RejectAllVisitors, &k.CustomVisitors,
		&k.GPCVisitors, &k.DoNotSellVisitors,
		&k.IdentifiedVisitors, &k.AbandonedBannerVisitors,
	); err != nil {
		return FunnelKPIs{}, fmt.Errorf("analyticsdb: funnel kpis: %w", err)
	}
	return k, nil
}

// PreConsentTrafficShare returns how many pageviews fired before the
// visitor made a consent decision (or never made one), versus after.
// This is the number GA4 / GTM owners care about: the legal data gap.
//
// "Before" = pageviews whose ts is older than the visitor's first
// consent_records.created_at on the same fingerprint, OR pageviews
// from fingerprints that never consented at all.
//
// "After" = pageviews whose ts is at-or-after the first consent decision
// AND that decision was accept-all OR custom-with-analytics-true.
type PreConsentShare struct {
	Total          int64 `json:"total_pageviews"`
	BeforeOrNever  int64 `json:"pageviews_before_or_no_consent"`
	AfterConsented int64 `json:"pageviews_after_consent"`
	AfterRejected  int64 `json:"pageviews_after_reject"`
}

func (m *Manager) PreConsentTrafficShare(ctx context.Context, siteID string, tr TimeRange) (PreConsentShare, error) {
	const q = `
WITH first_consent AS (
    SELECT fingerprint, MIN(created_at) AS first_consent_ms,
           ARG_MIN(consent_method, created_at) AS first_method,
           ARG_MIN(categories_json, created_at) AS first_categories
    FROM oltp.consent_records
    WHERE site_id = ? AND created_at >= ? AND created_at <= ?
    GROUP BY fingerprint
),
ev AS (
    SELECT ve.fingerprint,
           CAST(epoch_ms(CAST(ve.ts AS TIMESTAMP)) AS BIGINT) AS ts_ms,
           fc.first_consent_ms,
           fc.first_method
    FROM oltp.visit_events ve
    LEFT JOIN first_consent fc ON ve.fingerprint = fc.fingerprint
    WHERE ve.site_id = ? AND ve.ts >= ? AND ve.ts <= ?
)
SELECT
    COUNT(*) AS total,
    COUNT(*) FILTER (WHERE first_consent_ms IS NULL OR ts_ms < first_consent_ms) AS before_or_never,
    COUNT(*) FILTER (WHERE first_consent_ms IS NOT NULL AND ts_ms >= first_consent_ms
                          AND first_method IN ('accept-all','custom')) AS after_consented,
    COUNT(*) FILTER (WHERE first_consent_ms IS NOT NULL AND ts_ms >= first_consent_ms
                          AND first_method IN ('reject-all','gpc','dns','do-not-sell')) AS after_rejected
FROM ev
`
	var p PreConsentShare
	row := m.DB().QueryRowContext(ctx, q,
		siteID, tr.FromMs, tr.ToMs,
		siteID, tr.FromISO, tr.ToISO,
	)
	if err := row.Scan(&p.Total, &p.BeforeOrNever, &p.AfterConsented, &p.AfterRejected); err != nil {
		return PreConsentShare{}, fmt.Errorf("analyticsdb: pre-consent share: %w", err)
	}
	return p, nil
}

// TimeToConsent returns percentile latencies between a visitor's first
// pageview and their consent decision, in seconds. P50 = median; P90 =
// 90th percentile (slow accepters). Lower = banner is doing its job.
type TimeToConsent struct {
	SampleSize  int64   `json:"sample_size"`
	P50Seconds  float64 `json:"p50_seconds"`
	P90Seconds  float64 `json:"p90_seconds"`
	MeanSeconds float64 `json:"mean_seconds"`
}

func (m *Manager) TimeToConsent(ctx context.Context, siteID string, tr TimeRange) (TimeToConsent, error) {
	// Per-visitor delta between first visit_event and first consent_record.
	// Negative or zero deltas (consent posted before any pageview, or
	// in the same millisecond) are dropped as artifacts.
	const q = `
WITH first_event AS (
    SELECT fingerprint,
           MIN(CAST(epoch_ms(CAST(ts AS TIMESTAMP)) AS BIGINT)) AS first_ev_ms
    FROM oltp.visit_events
    WHERE site_id = ? AND ts >= ? AND ts <= ?
    GROUP BY fingerprint
),
first_consent AS (
    SELECT fingerprint, MIN(created_at) AS first_consent_ms
    FROM oltp.consent_records
    WHERE site_id = ? AND created_at >= ? AND created_at <= ?
    GROUP BY fingerprint
),
deltas AS (
    SELECT (fc.first_consent_ms - fe.first_ev_ms) / 1000.0 AS seconds
    FROM first_event fe JOIN first_consent fc USING (fingerprint)
    WHERE fc.first_consent_ms > fe.first_ev_ms
)
SELECT
    COUNT(*)                               AS sample_size,
    COALESCE(quantile_cont(seconds, 0.5),  0) AS p50,
    COALESCE(quantile_cont(seconds, 0.9),  0) AS p90,
    COALESCE(AVG(seconds),                 0) AS mean
FROM deltas
`
	var t TimeToConsent
	row := m.DB().QueryRowContext(ctx, q,
		siteID, tr.FromISO, tr.ToISO,
		siteID, tr.FromMs, tr.ToMs,
	)
	if err := row.Scan(&t.SampleSize, &t.P50Seconds, &t.P90Seconds, &t.MeanSeconds); err != nil {
		return TimeToConsent{}, fmt.Errorf("analyticsdb: time to consent: %w", err)
	}
	return t, nil
}

// EngagedAcceptRate splits visitors into "engaged" (scrolled past 50% OR
// spent > 30 seconds on a page) versus "not engaged", and reports the
// accept-all rate within each cohort. The hypothesis is that engaged
// visitors accept more . useful for tenants weighing how much to invest
// in landing-page quality.
type EngagedAcceptRate struct {
	EngagedVisitors         int64   `json:"engaged_visitors"`
	EngagedAcceptedVisitors int64   `json:"engaged_accepted"`
	EngagedAcceptRatePct    float64 `json:"engaged_accept_rate_pct"`
	NotEngagedVisitors      int64   `json:"not_engaged_visitors"`
	NotEngagedAcceptedCount int64   `json:"not_engaged_accepted"`
	NotEngagedAcceptRatePct float64 `json:"not_engaged_accept_rate_pct"`
}

func (m *Manager) EngagedAcceptRate(ctx context.Context, siteID string, tr TimeRange) (EngagedAcceptRate, error) {
	const q = `
WITH eng AS (
    SELECT fingerprint,
           MAX(time_on_page_ms) AS max_time,
           MAX(max_scroll_pct)  AS max_scroll
    FROM oltp.visit_engagement
    WHERE site_id = ? AND ts >= ? AND ts <= ?
    GROUP BY fingerprint
),
accepted AS (
    SELECT DISTINCT fingerprint
    FROM oltp.consent_records
    WHERE site_id = ? AND created_at >= ? AND created_at <= ?
      AND consent_method = 'accept-all'
),
visitors AS (
    SELECT DISTINCT fingerprint FROM oltp.visit_events
    WHERE site_id = ? AND ts >= ? AND ts <= ?
)
SELECT
    COUNT(*) FILTER (WHERE engaged)                                     AS engaged_total,
    COUNT(*) FILTER (WHERE engaged AND accepted)                        AS engaged_accepted,
    COUNT(*) FILTER (WHERE NOT engaged)                                 AS not_engaged_total,
    COUNT(*) FILTER (WHERE NOT engaged AND accepted)                    AS not_engaged_accepted
FROM (
    SELECT v.fingerprint,
           COALESCE(eng.max_time, 0) > 30000 OR COALESCE(eng.max_scroll, 0) >= 50 AS engaged,
           a.fingerprint IS NOT NULL AS accepted
    FROM visitors v
    LEFT JOIN eng ON eng.fingerprint = v.fingerprint
    LEFT JOIN accepted a ON a.fingerprint = v.fingerprint
)
`
	var er EngagedAcceptRate
	row := m.DB().QueryRowContext(ctx, q,
		siteID, tr.FromISO, tr.ToISO,
		siteID, tr.FromMs, tr.ToMs,
		siteID, tr.FromISO, tr.ToISO,
	)
	var et, ea, ne, na int64
	if err := row.Scan(&et, &ea, &ne, &na); err != nil {
		return EngagedAcceptRate{}, fmt.Errorf("analyticsdb: engaged accept rate: %w", err)
	}
	er.EngagedVisitors = et
	er.EngagedAcceptedVisitors = ea
	if et > 0 {
		er.EngagedAcceptRatePct = 100.0 * float64(ea) / float64(et)
	}
	er.NotEngagedVisitors = ne
	er.NotEngagedAcceptedCount = na
	if ne > 0 {
		er.NotEngagedAcceptRatePct = 100.0 * float64(na) / float64(ne)
	}
	return er, nil
}

// TopAcceptingPages returns the landing paths with the highest
// accept-all rate among visitors who entered the site on that path.
// "Entry path" = the visitor's first visit_events.path within the
// window. Limited to paths with at least 10 visitors so a single
// tester clicking accept on /test doesn't dominate.
type AcceptingPage struct {
	Path          string  `json:"path"`
	Visitors      int64   `json:"visitors"`
	Accepted      int64   `json:"accepted"`
	AcceptRatePct float64 `json:"accept_rate_pct"`
}

func (m *Manager) TopAcceptingPages(ctx context.Context, siteID string, tr TimeRange, limit int) ([]AcceptingPage, error) {
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	const q = `
WITH first_path AS (
    SELECT fingerprint,
           ARG_MIN(path, ts) AS entry_path
    FROM oltp.visit_events
    WHERE site_id = ? AND ts >= ? AND ts <= ?
    GROUP BY fingerprint
),
accepted AS (
    SELECT DISTINCT fingerprint FROM oltp.consent_records
    WHERE site_id = ? AND created_at >= ? AND created_at <= ?
      AND consent_method = 'accept-all'
)
SELECT entry_path,
       COUNT(*) AS visitors,
       COUNT(*) FILTER (WHERE accepted IS NOT NULL) AS accepted,
       100.0 * COUNT(*) FILTER (WHERE accepted IS NOT NULL) / NULLIF(COUNT(*), 0) AS accept_rate
FROM (
    SELECT fp.entry_path, fp.fingerprint, a.fingerprint AS accepted
    FROM first_path fp
    LEFT JOIN accepted a ON a.fingerprint = fp.fingerprint
)
GROUP BY entry_path
HAVING COUNT(*) >= 10
ORDER BY accept_rate DESC, visitors DESC
LIMIT ?
`
	rows, err := m.DB().QueryContext(ctx, q,
		siteID, tr.FromISO, tr.ToISO,
		siteID, tr.FromMs, tr.ToMs,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("analyticsdb: top accepting pages: %w", err)
	}
	defer rows.Close()
	var out []AcceptingPage
	for rows.Next() {
		var ap AcceptingPage
		var rate sql.NullFloat64
		if err := rows.Scan(&ap.Path, &ap.Visitors, &ap.Accepted, &rate); err != nil {
			return nil, fmt.Errorf("analyticsdb: scan accepting page: %w", err)
		}
		if rate.Valid {
			ap.AcceptRatePct = rate.Float64
		}
		out = append(out, ap)
	}
	return out, rows.Err()
}

// ConsentDailySplit returns a per-day count of consent decisions split
// by method. Drives the stacked-bar chart on the analytics tab. Same
// shape as the existing /consent/stats endpoint but resolved through
// DuckDB so it's fast even at millions of rows.
type ConsentDayRow struct {
	Day       string `json:"day"`
	AcceptAll int64  `json:"accept_all"`
	RejectAll int64  `json:"reject_all"`
	Custom    int64  `json:"custom"`
	GPC       int64  `json:"gpc"`
	DNS       int64  `json:"dns"`
	DoNotSell int64  `json:"do_not_sell"`
}

func (m *Manager) ConsentDailySplit(ctx context.Context, siteID string, tr TimeRange) ([]ConsentDayRow, error) {
	const q = `
SELECT
    strftime(epoch_ms(created_at), '%Y-%m-%d') AS day,
    COUNT(*) FILTER (WHERE consent_method = 'accept-all')  AS accept_all,
    COUNT(*) FILTER (WHERE consent_method = 'reject-all')  AS reject_all,
    COUNT(*) FILTER (WHERE consent_method = 'custom')      AS custom_count,
    COUNT(*) FILTER (WHERE consent_method = 'gpc')         AS gpc_count,
    COUNT(*) FILTER (WHERE consent_method = 'dns')         AS dns_count,
    COUNT(*) FILTER (WHERE consent_method = 'do-not-sell') AS dns_only
FROM oltp.consent_records
WHERE site_id = ? AND created_at >= ? AND created_at <= ?
GROUP BY day ORDER BY day
`
	rows, err := m.DB().QueryContext(ctx, q, siteID, tr.FromMs, tr.ToMs)
	if err != nil {
		return nil, fmt.Errorf("analyticsdb: consent daily split: %w", err)
	}
	defer rows.Close()
	var out []ConsentDayRow
	for rows.Next() {
		var r ConsentDayRow
		if err := rows.Scan(&r.Day, &r.AcceptAll, &r.RejectAll, &r.Custom, &r.GPC, &r.DNS, &r.DoNotSell); err != nil {
			return nil, fmt.Errorf("analyticsdb: scan consent day: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// VisitorJourneyStep is one row in the chronological visitor narrative
// the dashboard renders for an admin who clicks a single visitor.
// Combines the four data streams (page, engagement, consent, identity)
// into one timeline.
type VisitorJourneyStep struct {
	TimestampMs int64  `json:"ts_ms"`
	Kind        string `json:"kind"` // "pageview" | "engagement" | "consent" | "identified"
	Path        string `json:"path,omitempty"`
	Method      string `json:"method,omitempty"`
	Categories  string `json:"categories,omitempty"`
	GPCActive   bool   `json:"gpc_active,omitempty"`
	ScrollPct   int64  `json:"scroll_pct,omitempty"`
	TimeOnPageS int64  `json:"time_on_page_seconds,omitempty"`
}

func (m *Manager) VisitorJourney(ctx context.Context, siteID, fingerprint string, tr TimeRange) ([]VisitorJourneyStep, error) {
	const q = `
SELECT * FROM (
    SELECT CAST(epoch_ms(CAST(ts AS TIMESTAMP)) AS BIGINT) AS ts_ms,
           'pageview'::TEXT AS kind, path, ''::TEXT AS method,
           ''::TEXT AS categories, FALSE AS gpc, 0::BIGINT AS scroll, 0::BIGINT AS top_s
    FROM oltp.visit_events
    WHERE site_id = ? AND fingerprint = ? AND ts >= ? AND ts <= ?

    UNION ALL

    SELECT CAST(epoch_ms(CAST(ts AS TIMESTAMP)) AS BIGINT) AS ts_ms,
           'engagement'::TEXT AS kind, path, '' AS method, '' AS categories,
           FALSE AS gpc,
           CAST(max_scroll_pct AS BIGINT) AS scroll,
           CAST(time_on_page_ms / 1000 AS BIGINT) AS top_s
    FROM oltp.visit_engagement
    WHERE site_id = ? AND fingerprint = ? AND ts >= ? AND ts <= ?

    UNION ALL

    SELECT created_at AS ts_ms,
           'consent'::TEXT AS kind, page_url AS path, consent_method AS method,
           categories_json AS categories,
           gpc_active = 1 AS gpc, 0 AS scroll, 0 AS top_s
    FROM oltp.consent_records
    WHERE site_id = ? AND fingerprint = ? AND created_at >= ? AND created_at <= ?
)
ORDER BY ts_ms ASC
LIMIT 500
`
	rows, err := m.DB().QueryContext(ctx, q,
		siteID, fingerprint, tr.FromISO, tr.ToISO,
		siteID, fingerprint, tr.FromISO, tr.ToISO,
		siteID, fingerprint, tr.FromMs, tr.ToMs,
	)
	if err != nil {
		return nil, fmt.Errorf("analyticsdb: visitor journey: %w", err)
	}
	defer rows.Close()
	var out []VisitorJourneyStep
	for rows.Next() {
		var s VisitorJourneyStep
		if err := rows.Scan(&s.TimestampMs, &s.Kind, &s.Path, &s.Method, &s.Categories, &s.GPCActive, &s.ScrollPct, &s.TimeOnPageS); err != nil {
			return nil, fmt.Errorf("analyticsdb: scan journey step: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
