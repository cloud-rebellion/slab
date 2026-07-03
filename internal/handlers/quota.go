package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/bright-interaction/slab/internal/billing"
	"github.com/bright-interaction/slab/internal/store"
)

// gigabyte is the unit billing.Plan.StorageGB encodes; the workspace
// plan caps storage in GB-of-1024^3-bytes, so we multiply at the gate.
const gigabyte int64 = 1024 * 1024 * 1024

// QuotaHandler exposes per-tenant quota read + admin update endpoints
// and provides shared helpers (CheckStorageQuota / CheckBuildMinutesQuota)
// used inline by the media upload + build trigger handlers.
//
// The quota model is intentionally simple:
//   - storage_quota_bytes caps SUM(media.file_size) per site_id.
//   - build_minutes_quota caps SUM(deployments.duration_ms)/60000 over
//     the rolling 30-day window (time.Now().AddDate(0,0,-30)).
//   - quota_overage_blocked = 1 means a request that would push the
//     site over its quota is rejected with 429 + JSON body
//     {error:"quota_exceeded", kind:"storage"|"build_minutes", used, limit}.
//   - quota_overage_blocked = 0 means the request goes through but the
//     response carries an X-Quota-Warning: <kind> header.
type QuotaHandler struct {
	queries *store.Queries
}

func NewQuotaHandler(queries *store.Queries) *QuotaHandler {
	return &QuotaHandler{queries: queries}
}

// buildMinutesWindow is the rolling lookback used to count build minutes.
// 30 days keeps the math month-agnostic; we don't need cron resets.
const buildMinutesWindow = 30 * 24 * time.Hour

// QuotaCheck is the result of a quota lookup: how much was used, the
// configured limit, whether the request is allowed, and whether the
// site has overage-blocking on. Allowed=false + Blocked=true means the
// caller MUST 429; Allowed=false + Blocked=false means the caller is
// over but should warn-only (set X-Quota-Warning).
type QuotaCheck struct {
	Used    int64
	Limit   int64
	Allowed bool
	Blocked bool
}

// asInt64 reads the interface{} returned by sqlc's COALESCE-on-SQLite
// queries. SQLite returns int64 for SUM-over-INTEGER and float64 for
// SUM-over-non-INTEGER; we accept both and clamp negatives to zero.
func asInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		if n < 0 {
			return 0
		}
		return n
	case int:
		if n < 0 {
			return 0
		}
		return int64(n)
	case float64:
		if n < 0 {
			return 0
		}
		return int64(n)
	default:
		return 0
	}
}

// CheckStorageQuota returns the storage QuotaCheck for siteID with
// `additional` bytes added on top of the current sum. additional=0
// reports current usage; additional>0 simulates a pending upload.
func (h *QuotaHandler) CheckStorageQuota(ctx context.Context, siteID string, additional int64) (QuotaCheck, error) {
	q, err := h.queries.GetSiteQuota(ctx, siteID)
	if err != nil {
		return QuotaCheck{}, err
	}
	used, err := h.queries.SumStorageBytesBySite(ctx, siteID)
	if err != nil {
		return QuotaCheck{}, err
	}
	usedBytes := asInt64(used) + additional
	blocked := q.QuotaOverageBlocked != 0
	allowed := usedBytes <= q.StorageQuotaBytes
	return QuotaCheck{
		Used:    usedBytes,
		Limit:   q.StorageQuotaBytes,
		Allowed: allowed,
		Blocked: blocked,
	}, nil
}

// CheckBuildMinutesQuota returns the rolling-30d build-minutes QuotaCheck
// for siteID.
func (h *QuotaHandler) CheckBuildMinutesQuota(ctx context.Context, siteID string) (QuotaCheck, error) {
	q, err := h.queries.GetSiteQuota(ctx, siteID)
	if err != nil {
		return QuotaCheck{}, err
	}
	cutoff := time.Now().Add(-buildMinutesWindow).UTC().Format("2006-01-02 15:04:05")
	durMs, err := h.queries.SumBuildMinutesBySiteSinceCutoff(ctx, store.SumBuildMinutesBySiteSinceCutoffParams{
		SiteID:    siteID,
		CreatedAt: cutoff,
	})
	if err != nil {
		return QuotaCheck{}, err
	}
	usedMinutes := asInt64(durMs) / 60000
	blocked := q.QuotaOverageBlocked != 0
	allowed := usedMinutes <= q.BuildMinutesQuota
	return QuotaCheck{
		Used:    usedMinutes,
		Limit:   q.BuildMinutesQuota,
		Allowed: allowed,
		Blocked: blocked,
	}, nil
}

// CheckPlanStorageQuota returns the workspace-plan storage check for
// the workspace owning siteID. Returns Allowed=true (and Limit=-1)
// when the plan caps storage as unlimited (oss plan). Cap is
// `plan.StorageGB * 1 GiB`.
//
// `additional` simulates the bytes a pending upload would add so the
// gate fires BEFORE the bytes hit disk.
func (h *QuotaHandler) CheckPlanStorageQuota(ctx context.Context, siteID string, additional int64) (QuotaCheck, error) {
	site, err := h.queries.GetSiteByID(ctx, siteID)
	if err != nil {
		return QuotaCheck{}, err
	}
	if site.WorkspaceID == "" {
		return QuotaCheck{Allowed: true, Limit: -1}, nil
	}
	ws, err := h.queries.GetWorkspaceByID(ctx, site.WorkspaceID)
	if err != nil {
		return QuotaCheck{}, err
	}
	gb := billing.Limit(ws.Plan, "storage_gb")
	if gb < 0 {
		return QuotaCheck{Allowed: true, Limit: -1}, nil
	}
	used, err := h.queries.SumStorageBytesByWorkspace(ctx, site.WorkspaceID)
	if err != nil {
		return QuotaCheck{}, err
	}
	limitBytes := gb * gigabyte
	usedBytes := asInt64(used) + additional
	return QuotaCheck{
		Used:    usedBytes,
		Limit:   limitBytes,
		Allowed: usedBytes <= limitBytes,
		// Plan caps are always block-on-overage. Per-site soft warnings
		// are a different layer (admin-set, customer-visible).
		Blocked: true,
	}, nil
}

// CheckPlanBuildMinutesQuota returns the workspace-plan build-minutes
// rolling-30d check. Same Allowed/Limit/Blocked semantics as
// CheckPlanStorageQuota.
func (h *QuotaHandler) CheckPlanBuildMinutesQuota(ctx context.Context, siteID string) (QuotaCheck, error) {
	site, err := h.queries.GetSiteByID(ctx, siteID)
	if err != nil {
		return QuotaCheck{}, err
	}
	if site.WorkspaceID == "" {
		return QuotaCheck{Allowed: true, Limit: -1}, nil
	}
	ws, err := h.queries.GetWorkspaceByID(ctx, site.WorkspaceID)
	if err != nil {
		return QuotaCheck{}, err
	}
	limit := billing.Limit(ws.Plan, "build_minutes")
	if limit < 0 {
		return QuotaCheck{Allowed: true, Limit: -1}, nil
	}
	cutoff := time.Now().Add(-buildMinutesWindow).UTC().Format("2006-01-02 15:04:05")
	durMs, err := h.queries.SumBuildMinutesByWorkspaceSinceCutoff(ctx, store.SumBuildMinutesByWorkspaceSinceCutoffParams{
		WorkspaceID: site.WorkspaceID,
		CreatedAt:   cutoff,
	})
	if err != nil {
		return QuotaCheck{}, err
	}
	usedMinutes := asInt64(durMs) / 60000
	return QuotaCheck{
		Used:    usedMinutes,
		Limit:   limit,
		Allowed: usedMinutes <= limit,
		Blocked: true,
	}, nil
}

// EnforceStorageQuota is the inline gate the media upload handler
// calls before persisting the file. Returns true if the request was
// short-circuited (caller must return immediately); false to continue.
//
// The workspace-plan cap fires FIRST (so a free-tier customer never
// gets to the per-site soft-warn layer just by raising their per-site
// number), then the per-site quota.
func (h *QuotaHandler) EnforceStorageQuota(w http.ResponseWriter, r *http.Request, siteID string, additional int64) bool {
	plan, err := h.CheckPlanStorageQuota(r.Context(), siteID, additional)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to check plan storage quota")
		return true
	}
	if !plan.Allowed {
		writeJSON(w, http.StatusPaymentRequired, map[string]any{
			"error":     "plan_quota_exceeded",
			"error_msg": "Workspace plan storage cap reached; upgrade to add more.",
			"kind":      "plan_storage",
			"used":      plan.Used,
			"limit":     plan.Limit,
		})
		return true
	}
	check, err := h.CheckStorageQuota(r.Context(), siteID, additional)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to check storage quota")
		return true
	}
	if check.Allowed {
		return false
	}
	if check.Blocked {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error":     "quota_exceeded",
			"error_msg": "Storage quota exceeded",
			"kind":      "storage",
			"used":      check.Used,
			"limit":     check.Limit,
		})
		return true
	}
	w.Header().Set("X-Quota-Warning", "storage")
	return false
}

// EnforceBuildMinutesQuota is the inline gate the build trigger handlers
// call before kicking off a build goroutine. Same return contract as
// EnforceStorageQuota: workspace-plan cap fires before the per-site
// quota.
func (h *QuotaHandler) EnforceBuildMinutesQuota(w http.ResponseWriter, r *http.Request, siteID string) bool {
	plan, err := h.CheckPlanBuildMinutesQuota(r.Context(), siteID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to check plan build minutes quota")
		return true
	}
	if !plan.Allowed {
		writeJSON(w, http.StatusPaymentRequired, map[string]any{
			"error":     "plan_quota_exceeded",
			"error_msg": "Workspace plan build-minutes cap reached; upgrade to add more.",
			"kind":      "plan_build_minutes",
			"used":      plan.Used,
			"limit":     plan.Limit,
		})
		return true
	}
	check, err := h.CheckBuildMinutesQuota(r.Context(), siteID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to check build minutes quota")
		return true
	}
	if check.Allowed {
		return false
	}
	if check.Blocked {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error":     "quota_exceeded",
			"error_msg": "Build minutes quota exceeded",
			"kind":      "build_minutes",
			"used":      check.Used,
			"limit":     check.Limit,
		})
		return true
	}
	w.Header().Set("X-Quota-Warning", "build_minutes")
	return false
}

// GetQuota responds with the current storage + build-minutes usage and
// limits for the site. Site-access gated; tenants can see their own
// usage but cannot raise the limits (admin-only mutation).
//
// GET /api/sites/{siteID}/quota
func (h *QuotaHandler) GetQuota(w http.ResponseWriter, r *http.Request) {
	siteID := chi.URLParam(r, "siteID")
	if !isSafeSiteID(siteID) {
		writeError(w, http.StatusBadRequest, "Invalid site id")
		return
	}
	storage, err := h.CheckStorageQuota(r.Context(), siteID, 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load storage usage")
		return
	}
	minutes, err := h.CheckBuildMinutesQuota(r.Context(), siteID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load build minutes")
		return
	}
	q, err := h.queries.GetSiteQuota(r.Context(), siteID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load site quota")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"storage": map[string]any{
			"used_bytes":  storage.Used,
			"quota_bytes": storage.Limit,
			"pct":         pctOf(storage.Used, storage.Limit),
		},
		"build_minutes": map[string]any{
			"used":        minutes.Used,
			"quota":       minutes.Limit,
			"window_days": int(buildMinutesWindow / (24 * time.Hour)),
			"pct":         pctOf(minutes.Used, minutes.Limit),
		},
		"blocked_on_overage": q.QuotaOverageBlocked != 0,
	})
}

// PatchAdminQuota updates the three quota columns. Admin-only.
//
// PATCH /api/admin/sites/{siteID}/quota
func (h *QuotaHandler) PatchAdminQuota(w http.ResponseWriter, r *http.Request) {
	siteID := chi.URLParam(r, "siteID")
	if !isSafeSiteID(siteID) {
		writeError(w, http.StatusBadRequest, "Invalid site id")
		return
	}
	var req struct {
		StorageQuotaBytes   int64 `json:"storage_quota_bytes"`
		BuildMinutesQuota   int64 `json:"build_minutes_quota"`
		QuotaOverageBlocked *bool `json:"quota_overage_blocked"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.StorageQuotaBytes < 0 || req.BuildMinutesQuota < 0 {
		writeError(w, http.StatusBadRequest, "Quotas must be non-negative")
		return
	}
	current, err := h.queries.GetSiteQuota(r.Context(), siteID)
	if err != nil {
		writeError(w, http.StatusNotFound, "Site not found")
		return
	}
	storage := req.StorageQuotaBytes
	if storage == 0 {
		storage = current.StorageQuotaBytes
	}
	minutes := req.BuildMinutesQuota
	if minutes == 0 {
		minutes = current.BuildMinutesQuota
	}
	blocked := current.QuotaOverageBlocked
	if req.QuotaOverageBlocked != nil {
		if *req.QuotaOverageBlocked {
			blocked = 1
		} else {
			blocked = 0
		}
	}
	if err := h.queries.UpdateSiteQuota(r.Context(), store.UpdateSiteQuotaParams{
		StorageQuotaBytes:   storage,
		BuildMinutesQuota:   minutes,
		QuotaOverageBlocked: blocked,
		ID:                  siteID,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to update quota")
		return
	}
	AuditLog(r.Context(), h.queries, r, "", AuditActionUpdate, "site_quota", siteID, map[string]any{
		"storage_quota_bytes":   storage,
		"build_minutes_quota":   minutes,
		"quota_overage_blocked": blocked,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"storage_quota_bytes":   storage,
		"build_minutes_quota":   minutes,
		"quota_overage_blocked": blocked != 0,
	})
}

// AdminQuotaSummary returns a per-site utilization summary for the
// admin metrics endpoint. Capped at the 50 highest-utilization sites
// to keep the payload bounded; ties broken by storage_pct then name.
//
// Used inline from /api/admin/metrics, not registered as its own route.
func (h *QuotaHandler) AdminQuotaSummary(ctx context.Context) ([]map[string]any, error) {
	rows, err := h.queries.ListSitesForQuotaAudit(ctx)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-buildMinutesWindow).UTC().Format("2006-01-02 15:04:05")
	out := make([]map[string]any, 0, len(rows))
	for _, s := range rows {
		stoBytes, err := h.queries.SumStorageBytesBySite(ctx, s.ID)
		if err != nil {
			continue
		}
		minMs, err := h.queries.SumBuildMinutesBySiteSinceCutoff(ctx, store.SumBuildMinutesBySiteSinceCutoffParams{
			SiteID:    s.ID,
			CreatedAt: cutoff,
		})
		if err != nil {
			continue
		}
		usedBytes := asInt64(stoBytes)
		usedMinutes := asInt64(minMs) / 60000
		out = append(out, map[string]any{
			"site_id":             s.ID,
			"name":                s.Name,
			"slug":                s.Slug,
			"storage_used_bytes":  usedBytes,
			"storage_quota_bytes": s.StorageQuotaBytes,
			"storage_pct":         pctOf(usedBytes, s.StorageQuotaBytes),
			"build_minutes_used":  usedMinutes,
			"build_minutes_quota": s.BuildMinutesQuota,
			"build_minutes_pct":   pctOf(usedMinutes, s.BuildMinutesQuota),
			"blocked_on_overage":  s.QuotaOverageBlocked != 0,
		})
	}
	// Top-50 by max(storage_pct, build_minutes_pct).
	if len(out) > 50 {
		out = topNByMaxPct(out, 50)
	}
	return out, nil
}

func pctOf(used, limit int64) float64 {
	if limit <= 0 {
		return 0
	}
	return float64(used) / float64(limit)
}

func topNByMaxPct(items []map[string]any, n int) []map[string]any {
	// Insertion sort is fine for ListSitesForQuotaAudit results; SQLite
	// per-tenant deployments have order O(thousands) at the absolute
	// upper bound and this runs only on /api/admin/metrics.
	score := func(m map[string]any) float64 {
		s, _ := m["storage_pct"].(float64)
		b, _ := m["build_minutes_pct"].(float64)
		if b > s {
			return b
		}
		return s
	}
	for i := 1; i < len(items); i++ {
		j := i
		for j > 0 && score(items[j]) > score(items[j-1]) {
			items[j], items[j-1] = items[j-1], items[j]
			j--
		}
	}
	return items[:n]
}
