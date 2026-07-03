// Package retention runs a daily background sweep that purges old analytics
// and consent rows per site. Without this the SQLite DB grows unbounded:
// at moderate traffic (10 sites, 1k visitors/day, 5 pageviews/visit) the
// visit_events table alone gains ~7 GB/year. The sweep keeps the DB lean
// without needing an external cron, by running inside the atomicsite Go
// binary on a 24h ticker.
//
// Per-site retention is configurable via three settings (general category):
//
//	general.analytics_retention_days   default 180  range 30..3650
//	general.consent_retention_days     default 730  range 30..3650
//	general.engagement_retention_days  default  90  range  7..3650
//
// Why these defaults:
//   - Analytics 180d: half a year of raw event detail is enough for trend
//     reports + ad-hoc forensics. Aggregates (the planned cookie-analytics
//     rollup tables) survive the purge.
//   - Consent 730d: GDPR audit horizon. EU regulators expect proof-of-consent
//     to be retrievable for the full validity window of the consent itself
//     (which CookieProof's widget also defaults to 365 days), plus a buffer.
//   - Engagement 90d: high row volume, low forensic value past a quarter.
//
// Identified visit_sessions (those with a non-empty identified_at) are NOT
// purged regardless of last_seen_at, since they represent CRM-confirmed
// contacts the dashboard's identified-visitor view depends on.
package retention

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bright-interaction/slab/internal/store"
)

// DefaultAnalyticsDays is the per-site fallback when general.analytics_retention_days
// isn't set. 180 days = two quarters of raw events, enough for ad-hoc
// forensics; aggregates persist beyond this window.
const DefaultAnalyticsDays = 180

// DefaultConsentDays is the per-site fallback when general.consent_retention_days
// isn't set. 730 days = 2 years, the standard GDPR proof-of-consent horizon.
const DefaultConsentDays = 730

// DefaultEngagementDays is the per-site fallback when general.engagement_retention_days
// isn't set. 90 days = one quarter of high-volume engagement metrics.
const DefaultEngagementDays = 90

// MinRetentionDays floor. Anything below this is treated as the default;
// retention shorter than a week breaks any meaningful weekly reporting and
// is almost certainly a misconfiguration.
const MinRetentionDays = 7

// MaxRetentionDays ceiling: 10 years. Going past this is rare and likely
// a typo (someone wrote 36500 thinking days, meaning years).
const MaxRetentionDays = 3650

// SaltRetentionDays is the fixed window for consent_salts. Salts older than
// this can be deleted because no live consent_records still hashed against
// them remain after the consent purge runs (which itself uses
// DefaultConsentDays >= 30).
const SaltRetentionDays = 30

// DefaultAuditLogDays is the per-process retention window for the
// audit_log table. 365 days = 12 months, matching the GDPR + ISO 27001
// minimum proof-of-controls horizon. Operators with stricter
// requirements export rows out-of-band before the sweep clears them.
// Override via SetAuditLogRetentionDays before Start.
const DefaultAuditLogDays = 365

// LifecycleDisabled is the sentinel that disables a lifecycle stage.
// Either pause or delete (or both) can be 0 / unset; the sweep simply
// skips that transition. The OSS default is fully disabled (operators
// don't want their self-hosted workspaces auto-deleted); cloud
// installs set both via env or SetLifecyclePolicy.
const LifecycleDisabled = 0

// DefaultGDPRDeleteCoolingDays is the cooling-off window between a
// user's POST /api/account/delete and the retention sweep's hard
// delete. 7 days gives a real "I changed my mind" path while still
// fulfilling within the 30-day GDPR Art. 12(3) horizon. Operators
// align via SetGDPRDeleteCoolingDays before Start.
const DefaultGDPRDeleteCoolingDays = 7

// PauseAfterStartup gives the rest of the process (HTTP server, schema
// migrations, analytics manager) breathing room before the first sweep.
// SQLite WAL checkpointing also benefits from not being slammed during boot.
const PauseAfterStartup = 60 * time.Second

// SweepInterval is how often the manager wakes up to purge. Daily matches
// the consent_salts daily-rotation cadence and limits the worst-case
// staleness of "purged within the last X" to 24 hours.
const SweepInterval = 24 * time.Hour

// Manager runs the periodic purge loop. One process owns one Manager.
type Manager struct {
	queries *store.Queries
	db      *sql.DB

	mu         sync.Mutex
	lastSweep  time.Time
	lastResult SweepResult
	// dataDir is the workspace root used by the orphan-workspace sweep
	// (audit H8). Empty means the sweep is disabled (e.g. in tests). Set
	// via SetDataDir before Start.
	dataDir string
	// auditLogDays is the retention window for the audit_log table.
	// Zero falls back to DefaultAuditLogDays (365). Set via
	// SetAuditLogRetentionDays before Start.
	auditLogDays int
	// lifecyclePauseDays / lifecycleDeleteDays are the workspace
	// lifecycle thresholds (idle days). 0 = disabled. Set via
	// SetLifecyclePolicy before Start.
	lifecyclePauseDays  int
	lifecycleDeleteDays int
	// gdprDeleteCoolingDays is the cooling-off window between an
	// account-deletion request and the hard cascade. 0 falls back
	// to DefaultGDPRDeleteCoolingDays (7).
	gdprDeleteCoolingDays int

	cancel context.CancelFunc
	done   chan struct{}
}

// SweepResult is the per-run summary the slog line + admin debug endpoint
// expose. Counts are aggregate across all sites; PerSite holds the breakdown.
type SweepResult struct {
	StartedAt        time.Time            `json:"started_at"`
	FinishedAt       time.Time            `json:"finished_at"`
	DurationMs       int64                `json:"duration_ms"`
	SitesScanned     int                  `json:"sites_scanned"`
	VisitEvents      int64                `json:"visit_events_deleted"`
	Engagement       int64                `json:"engagement_deleted"`
	Sessions         int64                `json:"sessions_deleted"`
	Consent          int64                `json:"consent_deleted"`
	Salts            int64                `json:"salts_deleted"`
	AuditLog         int64                `json:"audit_log_deleted"`
	AuditDays        int                  `json:"audit_log_retention_days"`
	Paused           int                  `json:"lifecycle_paused"`
	Deleted          int                  `json:"lifecycle_deleted"`
	UsersHardDeleted int                  `json:"gdpr_users_hard_deleted"`
	QuotaOverages    int                  `json:"plan_quota_overages_detected"`
	PerSite          []PerSiteSweepResult `json:"per_site,omitempty"`
	Errors           []string             `json:"errors,omitempty"`
}

// PerSiteSweepResult is the per-tenant breakdown inside a SweepResult. Useful
// for a "your site dropped N rows last night" admin dashboard line.
type PerSiteSweepResult struct {
	SiteID             string `json:"site_id"`
	AnalyticsDays      int    `json:"analytics_days"`
	ConsentDays        int    `json:"consent_days"`
	EngagementDays     int    `json:"engagement_days"`
	VisitEventsDeleted int64  `json:"visit_events_deleted"`
	EngagementDeleted  int64  `json:"engagement_deleted"`
	SessionsDeleted    int64  `json:"sessions_deleted"`
	ConsentDeleted     int64  `json:"consent_deleted"`
	// CWVDeleted is the count of cwv_events rows purged in this sweep.
	// Shares the analytics retention window.
	CWVDeleted int64 `json:"cwv_deleted"`
}

// NewManager builds a Manager. Pass the same *sql.DB and *store.Queries the
// HTTP server uses; the manager runs scoped DELETEs against the shared
// connection (SQLite WAL mode handles concurrent readers + the single
// writer just fine).
//
// dataDir is the root that contains the workspaces/ subdir. If non-empty,
// the daily sweep also reaps orphan workspace directories (audit H8): a
// workspaces/{siteID}/ dir whose siteID is not in the sites table gets
// removed. Pass "" to disable the orphan sweep (e.g. in tests).
func NewManager(queries *store.Queries, db *sql.DB) *Manager {
	return &Manager{
		queries: queries,
		db:      db,
	}
}

// SetDataDir configures the data directory root. Call before Start. When
// set, the daily sweep also reaps orphan workspace directories (audit H8).
func (m *Manager) SetDataDir(dir string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dataDir = dir
}

// SetGDPRDeleteCoolingDays overrides the cooling-off window the sweep
// applies between a user's deletion_requested_at and the hard
// cascade. Pass 0 to keep the default (7 days). Values below 1 are
// clamped to 1 to prevent same-second commit; values are otherwise
// honored as-is so operators with stricter privacy policies (e.g.
// 24h) can shorten the window.
func (m *Manager) SetGDPRDeleteCoolingDays(days int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gdprDeleteCoolingDays = days
}

// SetLifecyclePolicy configures the workspace lifecycle thresholds.
// pauseDays = idle days before status='active' becomes 'paused'.
// deleteDays = idle days before status (active or paused) becomes
// 'deleted'. Either being 0 disables that stage; both being 0
// disables the sweep entirely. Call before Start.
//
// The transition is a soft state change only (a 'deleted' workspace's
// rows stay in the DB until an operator-initiated hard delete) so a
// sweep mistake doesn't blow up customer data. Cloud operators run
// `atomicsite purge-deleted-workspaces` (a separate CLI verb) to
// commit the hard delete after a confirmation window.
func (m *Manager) SetLifecyclePolicy(pauseDays, deleteDays int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lifecyclePauseDays = pauseDays
	m.lifecycleDeleteDays = deleteDays
}

// SetAuditLogRetentionDays overrides the audit_log retention window.
// Call before Start. Values below MinRetentionDays or above
// MaxRetentionDays are clamped at sweep time. Pass 0 to keep
// DefaultAuditLogDays (365). Compliance tip: any reduction below 365
// should be paired with an out-of-band export job, since GDPR / ISO
// 27001 proof-of-controls expects a year-back window.
func (m *Manager) SetAuditLogRetentionDays(days int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.auditLogDays = days
}

// Start launches the sweep loop in a goroutine. The first sweep fires
// PauseAfterStartup after Start returns, then every SweepInterval. Cancel
// the parent ctx to stop, or call Stop for a bounded-wait shutdown.
func (m *Manager) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.done = make(chan struct{})

	go func() {
		defer close(m.done)
		// Initial delay so we don't slam the DB on boot. Cancellable via ctx.
		select {
		case <-time.After(PauseAfterStartup):
		case <-ctx.Done():
			return
		}

		m.runOnce(ctx)
		ticker := time.NewTicker(SweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.runOnce(ctx)
			}
		}
	}()
}

// Stop cancels the loop and waits up to ctx's deadline for the current
// sweep to finish. Safe to call when Start was never invoked.
func (m *Manager) Stop(ctx context.Context) {
	if m.cancel == nil {
		return
	}
	m.cancel()
	if m.done == nil {
		return
	}
	select {
	case <-m.done:
	case <-ctx.Done():
	}
}

// LastResult returns the most recent SweepResult (zero value if no sweep
// has completed yet). Read-only snapshot under the manager's mutex.
func (m *Manager) LastResult() SweepResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastResult
}

// RunOnce triggers an out-of-band sweep, intended for an admin debug
// endpoint. Returns the SweepResult so the caller can render it in the
// response. Errors landing inside individual site purges are logged and
// surfaced in SweepResult.Errors; the function itself only errors when it
// can't list the sites at all.
func (m *Manager) RunOnce(ctx context.Context) (SweepResult, error) {
	return m.runOnce(ctx)
}

func (m *Manager) runOnce(ctx context.Context) (SweepResult, error) {
	res := SweepResult{StartedAt: time.Now().UTC()}

	sites, err := m.queries.ListSites(ctx)
	if err != nil {
		res.Errors = append(res.Errors, "list sites: "+err.Error())
		res.FinishedAt = time.Now().UTC()
		res.DurationMs = res.FinishedAt.Sub(res.StartedAt).Milliseconds()
		m.recordResult(res)
		return res, err
	}
	res.SitesScanned = len(sites)

	for _, site := range sites {
		if ctx.Err() != nil {
			res.Errors = append(res.Errors, "ctx cancelled mid-sweep")
			break
		}
		ps := m.purgeSite(ctx, site.ID)
		res.PerSite = append(res.PerSite, ps)
		res.VisitEvents += ps.VisitEventsDeleted
		res.Engagement += ps.EngagementDeleted
		res.Sessions += ps.SessionsDeleted
		res.Consent += ps.ConsentDeleted
	}

	// Salt purge is process-level, not per-site. Salts older than 30 days
	// are unreferenced once the consent purge runs (which uses >= 30d
	// retention) and there's no per-tenant story for them.
	// Audit H8: orphan workspace sweep. Looks at every directory under
	// {dataDir}/workspaces/ and removes any whose name is no longer in
	// the sites table. Disabled when dataDir is unset (tests).
	if dir := m.getDataDir(); dir != "" {
		if n, err := m.sweepOrphanWorkspaces(ctx, dir); err != nil {
			res.Errors = append(res.Errors, "orphan workspace sweep: "+err.Error())
		} else if n > 0 {
			slog.Info("retention: orphan workspaces removed", "count", n)
		}
	}

	saltCutoff := time.Now().UTC().AddDate(0, 0, -SaltRetentionDays).Format("2006-01-02")
	if err := m.queries.DeleteConsentSaltsOlderThan(ctx, saltCutoff); err != nil {
		res.Errors = append(res.Errors, "delete consent salts: "+err.Error())
	}

	// audit_log retention. Process-wide (not per-site) because audit
	// rows include workspace + user actions that aren't site-scoped.
	// SQLite's DEFAULT (datetime('now')) writes "YYYY-MM-DD HH:MM:SS"
	// in UTC, so the cutoff string uses the same format for a
	// lexicographic compare.
	auditDays := m.getAuditLogRetentionDays()
	res.AuditDays = auditDays
	auditCutoff := time.Now().UTC().AddDate(0, 0, -auditDays).Format("2006-01-02 15:04:05")
	if n, err := m.queries.DeleteAuditLogOlderThan(ctx, auditCutoff); err != nil {
		res.Errors = append(res.Errors, "delete audit log: "+err.Error())
	} else {
		res.AuditLog = n
	}

	// Workspace lifecycle sweep. Disabled when both thresholds are 0
	// (the OSS default). Operators on cloud configure days via env
	// vars; SetLifecyclePolicy reads them at boot.
	pauseDays, deleteDays := m.getLifecyclePolicy()
	if pauseDays > 0 || deleteDays > 0 {
		paused, deleted, sweepErrs := m.sweepWorkspaceLifecycle(ctx, pauseDays, deleteDays)
		res.Paused = paused
		res.Deleted = deleted
		res.Errors = append(res.Errors, sweepErrs...)
	}

	// GDPR Article 17 erasure: users whose deletion_requested_at is
	// older than the cooling-off window get hard-deleted (cascading
	// via FK ON DELETE CASCADE through site_members, audit_log,
	// password_resets, etc).
	hardDeleted, gdprErrs := m.sweepGDPRDeletions(ctx)
	res.UsersHardDeleted = hardDeleted
	res.Errors = append(res.Errors, gdprErrs...)

	// C2 reconciler: catches workspaces that slipped past the
	// plan-quota gate via concurrent uploads (TOCTOU between the
	// SUM check and the INSERT). Audit_log row lets operators
	// escalate billing or trigger a hard-block flip.
	overages, recErrs := m.sweepPlanQuotaOverages(ctx)
	res.QuotaOverages = overages
	res.Errors = append(res.Errors, recErrs...)

	res.FinishedAt = time.Now().UTC()
	res.DurationMs = res.FinishedAt.Sub(res.StartedAt).Milliseconds()

	slog.Info("retention: sweep complete",
		"sites", res.SitesScanned,
		"visit_events", res.VisitEvents,
		"engagement", res.Engagement,
		"sessions", res.Sessions,
		"consent", res.Consent,
		"audit_log", res.AuditLog,
		"audit_days", res.AuditDays,
		"lifecycle_paused", res.Paused,
		"lifecycle_deleted", res.Deleted,
		"quota_overages", res.QuotaOverages,
		"users_hard_deleted", res.UsersHardDeleted,
		"duration_ms", res.DurationMs,
		"errors", len(res.Errors),
	)

	m.recordResult(res)
	return res, nil
}

func (m *Manager) recordResult(r SweepResult) {
	m.mu.Lock()
	m.lastSweep = r.FinishedAt
	m.lastResult = r
	m.mu.Unlock()
}

// purgeSite resolves the site's retention windows from settings, then
// runs the four DELETEs. Errors land in slog and the returned struct so
// they don't abort the rest of the sweep.
func (m *Manager) purgeSite(ctx context.Context, siteID string) PerSiteSweepResult {
	settings, err := m.queries.ListSettingsBySite(ctx, siteID)
	if err != nil {
		slog.Warn("retention: list settings", "site_id", siteID, "err", err)
		// fall through with defaults; one missing settings read shouldn't
		// stop the purge for the rest of the site.
	}
	settingsMap := map[string]string{}
	for _, s := range settings {
		settingsMap[s.Category+"."+s.Key] = s.Value
	}

	analyticsDays := readDays(settingsMap, "general.analytics_retention_days", DefaultAnalyticsDays)
	consentDays := readDays(settingsMap, "general.consent_retention_days", DefaultConsentDays)
	engagementDays := readDays(settingsMap, "general.engagement_retention_days", DefaultEngagementDays)

	now := time.Now().UTC()
	tsCutoff := now.AddDate(0, 0, -analyticsDays).Format(time.RFC3339)
	engagementCutoff := now.AddDate(0, 0, -engagementDays).Format(time.RFC3339)
	consentCutoffMs := now.AddDate(0, 0, -consentDays).UnixMilli()

	res := PerSiteSweepResult{
		SiteID:         siteID,
		AnalyticsDays:  analyticsDays,
		ConsentDays:    consentDays,
		EngagementDays: engagementDays,
	}

	// Order: engagement -> events -> sessions. Sessions last so any future
	// FK or analytics aggregator that joins events->sessions still has the
	// session row when events are dropped.
	if n, err := m.queries.DeleteVisitEngagementBySiteOlderThan(ctx, store.DeleteVisitEngagementBySiteOlderThanParams{
		SiteID: siteID, Ts: engagementCutoff,
	}); err != nil {
		slog.Warn("retention: delete engagement", "site_id", siteID, "err", err)
	} else {
		res.EngagementDeleted = n
	}

	if n, err := m.queries.DeleteVisitEventsBySiteOlderThan(ctx, store.DeleteVisitEventsBySiteOlderThanParams{
		SiteID: siteID, Ts: tsCutoff,
	}); err != nil {
		slog.Warn("retention: delete visit events", "site_id", siteID, "err", err)
	} else {
		res.VisitEventsDeleted = n
	}

	if n, err := m.queries.DeleteVisitSessionsBySiteOlderThan(ctx, store.DeleteVisitSessionsBySiteOlderThanParams{
		SiteID: siteID, LastSeenAt: tsCutoff,
	}); err != nil {
		slog.Warn("retention: delete sessions", "site_id", siteID, "err", err)
	} else {
		res.SessionsDeleted = n
	}

	if n, err := m.queries.DeleteConsentBySiteOlderThan(ctx, store.DeleteConsentBySiteOlderThanParams{
		SiteID: siteID, CreatedAt: consentCutoffMs,
	}); err != nil {
		slog.Warn("retention: delete consent", "site_id", siteID, "err", err)
	} else {
		res.ConsentDeleted = n
	}

	// Sprint 5 quick-win (2026-05-24): per-site CWV sample retention.
	// Shares the analytics retention window since CWV is part of the
	// real-user analytics surface; a separate window would be more
	// flexibility than tenants need today.
	if n, err := m.queries.DeleteCWVBySiteOlderThan(ctx, store.DeleteCWVBySiteOlderThanParams{
		SiteID: siteID, CreatedAt: tsCutoff,
	}); err != nil {
		slog.Warn("retention: delete cwv events", "site_id", siteID, "err", err)
	} else {
		res.CWVDeleted = n
	}

	return res
}

// readDays parses an int settings value and clamps to [MinRetentionDays,
// MaxRetentionDays]. Falls back to fallbackDays for empty / unparseable /
// out-of-range values.
func readDays(m map[string]string, key string, fallbackDays int) int {
	v := strings.TrimSpace(m[key])
	if v == "" {
		return fallbackDays
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallbackDays
	}
	if n < MinRetentionDays || n > MaxRetentionDays {
		return fallbackDays
	}
	return n
}

// ErrNotStarted is returned by RunOnce when called before Start. Reserved
// for future use; current implementation is permissive and runs even
// without Start since the manager is otherwise idempotent.
var ErrNotStarted = errors.New("retention: manager not started")

// getDataDir returns the configured data dir under the manager mutex.
func (m *Manager) getDataDir() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dataDir
}

// getAuditLogRetentionDays returns the configured audit_log retention
// window, clamped to [MinRetentionDays, MaxRetentionDays]. Zero or
// out-of-range falls back to DefaultAuditLogDays. Same clamp shape
// as readDays() so misconfigured env vars never end up purging
// everything or keeping nothing.
func (m *Manager) getAuditLogRetentionDays() int {
	m.mu.Lock()
	d := m.auditLogDays
	m.mu.Unlock()
	if d <= 0 {
		return DefaultAuditLogDays
	}
	if d < MinRetentionDays {
		return MinRetentionDays
	}
	if d > MaxRetentionDays {
		return MaxRetentionDays
	}
	return d
}

// getLifecyclePolicy returns the configured pause/delete day
// thresholds. Zero in either slot disables that transition.
func (m *Manager) getLifecyclePolicy() (pauseDays, deleteDays int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lifecyclePauseDays, m.lifecycleDeleteDays
}

// getGDPRDeleteCoolingDays returns the configured cooling-off window
// for GDPR account deletions, falling back to the default. Clamped
// to 1 minimum so a misconfigured 0 doesn't accidentally hard-delete
// in the same second the user requests.
func (m *Manager) getGDPRDeleteCoolingDays() int {
	m.mu.Lock()
	d := m.gdprDeleteCoolingDays
	m.mu.Unlock()
	if d <= 0 {
		return DefaultGDPRDeleteCoolingDays
	}
	return d
}

// sweepGDPRDeletions hard-deletes users whose deletion_requested_at
// is older than the configured cooling-off window. Cascade FKs (see
// internal/db/schema.sql: every user_id ON DELETE CASCADE) clean up
// the dependent rows. The audit_log row that the deletion request
// wrote earlier is also removed by cascade, but the more important
// "user_deletion_completed" row we write here records the hard
// delete itself for compliance.
func (m *Manager) sweepGDPRDeletions(ctx context.Context) (deleted int, errs []string) {
	cooling := m.getGDPRDeleteCoolingDays()
	cutoff := time.Now().UTC().AddDate(0, 0, -cooling).Format("2006-01-02 15:04:05")
	due, err := m.queries.ListUsersDueForDeletion(ctx, cutoff)
	if err != nil {
		return 0, []string{"list users due for deletion: " + err.Error()}
	}
	for _, u := range due {
		if ctx.Err() != nil {
			errs = append(errs, "ctx cancelled mid-gdpr-sweep")
			break
		}
		// Audit-log the hard delete BEFORE we delete the user, so
		// the actor_user_id reference still resolves and the row
		// survives the FK cascade. We use a fresh ID to avoid
		// colliding with any existing row.
		if err := m.queries.WriteAuditLog(ctx, store.WriteAuditLogParams{
			ID:           newGDPRAuditID(),
			ActorUserID:  "", // system actor, not the user being deleted
			ActorRole:    "system",
			ActorIp:      "",
			SiteID:       "",
			Action:       "delete",
			ResourceType: "user_deletion_completed",
			ResourceID:   u.ID,
			ChangesJson:  `{"email":"` + escapeJSONString(u.Email) + `","cooling_days":` + strconv.Itoa(cooling) + `}`,
		}); err != nil {
			// Log but proceed; the delete itself is the contract.
			slog.Warn("retention: audit write failed for gdpr delete", "user_id", u.ID, "err", err)
		}
		if err := m.queries.DeleteUser(ctx, u.ID); err != nil {
			errs = append(errs, "delete user "+u.ID+": "+err.Error())
			continue
		}
		deleted++
		slog.Info("retention: gdpr hard delete", "user_id", u.ID, "email", u.Email)
	}
	return deleted, errs
}

// sweepPlanQuotaOverages catches workspaces that have slipped past
// the workspace-plan caps for storage or build minutes. The
// EnforceStorageQuota / EnforceBuildMinutesQuota gates are
// per-request and check usage BEFORE the write; under concurrent
// uploads two requests can both pass the check, both commit, and
// the workspace ends up over the cap. This sweep walks every
// non-deleted workspace once a day, recomputes usage, and writes a
// `plan_quota_overage_detected` audit_log row whenever the workspace
// is over its plan cap. Operators escalate from the audit log
// (billing nudge, hard-block flip).
//
// Detection only - the sweep does NOT auto-block, mark for hard
// deletion, or otherwise mutate the workspace state. Compliance with
// the soft-delete principle: a sweep mistake is recoverable, an
// auto-block isn't.
//
// Imports billing through the same indirection plan_quota.go uses on
// the request path so the two layers always agree on the cap value.
// We don't import the handlers package directly here (would be a
// cycle); instead the values come from billing.Limit which both
// sides call.
func (m *Manager) sweepPlanQuotaOverages(ctx context.Context) (int, []string) {
	rows, err := m.queries.ListWorkspacesForLifecycleSweep(ctx)
	if err != nil {
		return 0, []string{"list workspaces for quota reconcile: " + err.Error()}
	}
	overages := 0
	var errs []string
	now := time.Now().UTC().Add(-30 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	for _, ws := range rows {
		if ctx.Err() != nil {
			errs = append(errs, "ctx cancelled mid-quota-reconcile")
			break
		}
		storageGB := planLimitFor(ws.Plan, "storage_gb")
		buildMin := planLimitFor(ws.Plan, "build_minutes")
		if storageGB < 0 && buildMin < 0 {
			continue // unlimited plan; skip the SUM queries entirely
		}

		storageUsed := int64(0)
		if storageGB >= 0 {
			raw, err := m.queries.SumStorageBytesByWorkspace(ctx, ws.ID)
			if err == nil {
				storageUsed = anyToInt64(raw)
			}
		}
		buildUsed := int64(0)
		if buildMin >= 0 {
			raw, err := m.queries.SumBuildMinutesByWorkspaceSinceCutoff(ctx, store.SumBuildMinutesByWorkspaceSinceCutoffParams{
				WorkspaceID: ws.ID, CreatedAt: now,
			})
			if err == nil {
				buildUsed = anyToInt64(raw) / 60000
			}
		}

		storageCap := storageGB * 1024 * 1024 * 1024
		storageOver := storageGB >= 0 && storageUsed > storageCap
		buildOver := buildMin >= 0 && buildUsed > buildMin

		if !storageOver && !buildOver {
			continue
		}

		changes := `{"plan":"` + escapeJSONString(ws.Plan) +
			`","storage_used":` + strconv.FormatInt(storageUsed, 10) +
			`,"storage_cap":` + strconv.FormatInt(storageCap, 10) +
			`,"build_used":` + strconv.FormatInt(buildUsed, 10) +
			`,"build_cap":` + strconv.FormatInt(buildMin, 10) +
			`,"storage_over":` + boolStr(storageOver) +
			`,"build_over":` + boolStr(buildOver) + `}`
		if err := m.queries.WriteAuditLog(ctx, store.WriteAuditLogParams{
			ID:           newGDPRAuditID(),
			ActorUserID:  "",
			ActorRole:    "system",
			ActorIp:      "",
			SiteID:       "",
			Action:       "detect",
			ResourceType: "plan_quota_overage_detected",
			ResourceID:   ws.ID,
			ChangesJson:  changes,
		}); err != nil {
			errs = append(errs, "audit overage "+ws.ID+": "+err.Error())
			continue
		}
		overages++
		slog.Warn("retention: plan quota overage detected",
			"workspace_id", ws.ID, "plan", ws.Plan,
			"storage_used_bytes", storageUsed, "storage_cap_bytes", storageCap,
			"build_minutes_used", buildUsed, "build_minutes_cap", buildMin)
	}
	return overages, errs
}

// anyToInt64 mirrors handlers.asInt64 since the retention package
// can't import handlers (would invert layering). SQLite COALESCE-on-
// SUM returns int64 for INTEGER columns and float64 for non-INTEGER;
// negatives clamp to 0 so a corrupted row can't drive a negative cap.
func anyToInt64(v any) int64 {
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

// planLimitFor is the retention-package shim around billing.Limit.
// Held here as a thin function so a future tightening of the plan
// ladder lookup (custom EE provider, billing-backend cache) only
// needs one edit point in the retention sweep.
func planLimitFor(planKey, resource string) int64 {
	return billingLimit(planKey, resource)
}

// billingLimit is wired in retention/billing_shim.go (next file) to
// avoid a static import cycle; that file lives in the same package
// and imports `internal/billing` directly.
//
// (Defined in billing_shim.go to keep this file's import set short.)
var billingLimit = func(planKey, resource string) int64 { return -1 }

// boolStr formats a Go bool as the JSON literal "true" / "false" for
// our hand-built audit_log changes_json blob. Keeps the retention
// package free of an encoding/json import for a 1-key dict.
func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// newGDPRAuditID returns a fresh 24-hex audit_log ID. We don't import
// the handlers package here, so we mint locally.
func newGDPRAuditID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// escapeJSONString does just enough JSON escaping for the email field
// in our manually-constructed changes_json blob. We avoid the full
// json package here because the row is a 1-key dict and json.Marshal
// would round-trip through reflection unnecessarily.
func escapeJSONString(s string) string {
	out := make([]byte, 0, len(s)+4)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"', '\\':
			out = append(out, '\\', c)
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		default:
			if c < 0x20 {
				continue // strip control bytes
			}
			out = append(out, c)
		}
	}
	return string(out)
}

// sweepWorkspaceLifecycle walks every non-deleted workspace, computes
// last-activity timestamps, and transitions status:
//
//	active -> paused: idle >= pauseDays (when pauseDays > 0)
//	* -> deleted: idle >= deleteDays (when deleteDays > 0)
//
// "Deleted" here is a soft state (status='deleted'); rows stay in the
// DB until an operator-initiated hard purge. This intentional safety
// margin means a sweep mistake is recoverable; operators run a
// separate verb to commit the hard delete after a confirmation
// window.
//
// Audit-log entries for every transition land via the same audit_log
// table the rest of the system uses, so cloud operators have a paper
// trail for the EU compliance horizon.
func (m *Manager) sweepWorkspaceLifecycle(ctx context.Context, pauseDays, deleteDays int) (paused, deleted int, errs []string) {
	rows, err := m.queries.ListWorkspacesForLifecycleSweep(ctx)
	if err != nil {
		return 0, 0, []string{"list workspaces: " + err.Error()}
	}
	now := time.Now().UTC()
	pauseCutoff := now.AddDate(0, 0, -pauseDays).Format("2006-01-02 15:04:05")
	deleteCutoff := now.AddDate(0, 0, -deleteDays).Format("2006-01-02 15:04:05")
	for _, ws := range rows {
		if ctx.Err() != nil {
			errs = append(errs, "ctx cancelled mid-lifecycle-sweep")
			break
		}
		raw, err := m.queries.GetWorkspaceLastActivity(ctx, ws.ID)
		if err != nil {
			errs = append(errs, "last activity "+ws.ID+": "+err.Error())
			continue
		}
		lastActivity, _ := raw.(string)
		if strings.TrimSpace(lastActivity) == "" {
			// No activity at all - fall back to created_at so we
			// don't auto-pause a freshly-created workspace whose
			// owner hasn't done anything yet on day 31.
			lastActivity = ws.CreatedAt
		}
		// Clock-skew clamp: a workspace whose created_at is in the
		// future (or whose lastActivity falls before created_at due
		// to a corrupt INSERT) must not be considered "idle since
		// before it existed". Use whichever is later.
		if lastActivity < ws.CreatedAt {
			lastActivity = ws.CreatedAt
		}
		// Delete takes precedence: a workspace that's been idle long
		// enough to delete should NOT first transition to paused on
		// the same sweep run.
		if deleteDays > 0 && lastActivity < deleteCutoff && ws.Status != "deleted" {
			if err := m.queries.UpdateWorkspaceStatus(ctx, store.UpdateWorkspaceStatusParams{
				ID: ws.ID, Status: "deleted",
			}); err != nil {
				errs = append(errs, "set deleted "+ws.ID+": "+err.Error())
				continue
			}
			deleted++
			slog.Info("retention: workspace lifecycle delete",
				"workspace_id", ws.ID, "last_activity", lastActivity, "from_status", ws.Status)
			continue
		}
		if pauseDays > 0 && ws.Status == "active" && lastActivity < pauseCutoff {
			if err := m.queries.UpdateWorkspaceStatus(ctx, store.UpdateWorkspaceStatusParams{
				ID: ws.ID, Status: "paused",
			}); err != nil {
				errs = append(errs, "set paused "+ws.ID+": "+err.Error())
				continue
			}
			paused++
			slog.Info("retention: workspace lifecycle pause",
				"workspace_id", ws.ID, "last_activity", lastActivity)
		}
	}
	return paused, deleted, errs
}

// sweepOrphanWorkspaces removes any directory under {dataDir}/workspaces/
// whose name is no longer in the sites table (audit H8). Returns the
// count of removed dirs. Errors do not abort the sweep — every reapable
// dir is given a chance.
//
// Safety: only paths matching the 24-hex siteID shape are considered. Any
// other directory name (e.g. user-created scratch dir) is left alone.
// The directory traversal stays inside `{dataDir}/workspaces/` so a
// symlink doesn't escape the workspaces root.
func (m *Manager) sweepOrphanWorkspaces(ctx context.Context, dataDir string) (int, error) {
	wsRoot := filepath.Join(dataDir, "workspaces")
	entries, err := os.ReadDir(wsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // No workspaces dir yet; nothing to do.
		}
		return 0, err
	}

	// Build the set of live site IDs.
	sites, err := m.queries.ListSites(ctx)
	if err != nil {
		return 0, err
	}
	live := make(map[string]bool, len(sites))
	for _, s := range sites {
		live[s.ID] = true
	}

	removed := 0
	for _, entry := range entries {
		if ctx.Err() != nil {
			return removed, ctx.Err()
		}
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !workspaceNamePattern.MatchString(name) {
			// Doesn't look like a siteID; leave it alone (defensive).
			continue
		}
		if live[name] {
			continue
		}
		path := filepath.Join(wsRoot, name)
		if err := os.RemoveAll(path); err != nil {
			slog.Warn("retention: orphan workspace remove failed",
				"path", path, "err", err)
			continue
		}
		removed++
	}
	return removed, nil
}

// workspaceNamePattern matches the 24-char hex shape that newID() produces
// for siteIDs. Anything else under workspaces/ is left untouched by the
// orphan sweep so a stray scratch dir doesn't get nuked.
var workspaceNamePattern = regexp.MustCompile(`^[a-fA-F0-9]{24}$`)
