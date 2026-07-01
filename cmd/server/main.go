// Command server is the AtomicSite HTTP server.
package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
	"golang.org/x/crypto/bcrypt"

	"github.com/bright-interaction/slab/internal/analytics"
	"github.com/bright-interaction/slab/internal/analyticsdb"
	"github.com/bright-interaction/slab/internal/builder"
	"github.com/bright-interaction/slab/internal/config"
	dbpkg "github.com/bright-interaction/slab/internal/db"
	"github.com/bright-interaction/slab/internal/domains"
	"github.com/bright-interaction/slab/internal/handlers"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/migration"
	"github.com/bright-interaction/slab/internal/retention"
	"github.com/bright-interaction/slab/internal/server"
	"github.com/bright-interaction/slab/internal/storage"
	"github.com/bright-interaction/slab/internal/store"
	"github.com/bright-interaction/slab/internal/webhook"
)

//go:embed all:frontend/build
var frontendFiles embed.FS

func main() {
	// Subcommand dispatch. Anything other than the (default) HTTP server lands
	// here. Subcommands open their own DB connection and exit.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "reset-password":
			resetPasswordCLI(os.Args[2:])
			return
		case "backup-db":
			backupDBCLI(os.Args[2:])
			return
		case "list-orphans":
			listOrphansCLI(os.Args[2:])
			return
		}
	}

	cfg := config.Load()
	// Refuse to boot in production-like deployments (BaseURL != localhost)
	// when any documented-default secret is still in place. The seeded
	// admin password is checked separately at seed time below. Keeps local
	// dev permissive while making "deployed without env" impossible to miss.
	if err := cfg.Validate(); err != nil {
		slog.Error("config validation failed; refusing to start", "error", err.Error())
		os.Exit(1)
	}
	// Error reporting to Flare (no-op unless FLARE_DSN is set; the DSN is
	// injected by the Hephaestus flare-provision deploy step). This binary
	// has no build-time version var, so the release is pinned to "dev".
	authmw.InitFlare("atomicsite", "dev")
	// Default admin base URL for the personalization hydration script
	// (Phase 18.2). Per-site override via the analytics.admin_base_url
	// setting; falls back to cfg.BaseURL otherwise.
	builder.DefaultAdminBaseURL = cfg.BaseURL

	// Screenshot SSRF allow-list: bind to the configured public domain(s)
	// at boot. Loopback always passes inside validateScreenshotURL so
	// local dev works regardless. An empty list (no env vars set) is
	// fine in dev; in production cfg.Validate() above already required
	// at least one of PrimaryDomain / BuiltSiteSuffix to be set.
	handlers.ConfigureScreenshotAllowList(cfg.PrimaryDomain, cfg.BuiltSiteSuffix)

	// Ensure data directory exists
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		slog.Error("create data dir", "error", err)
		os.Exit(1)
	}

	// Open SQLite DB
	sqlDB, err := sql.Open("sqlite", cfg.DBPath+"?_journal_mode=WAL&_busy_timeout=5000&_pragma=foreign_keys(1)")
	if err != nil {
		slog.Error("open db", "error", err)
		os.Exit(1)
	}
	sqlDB.SetMaxOpenConns(1)
	defer sqlDB.Close()

	// Apply schema
	if err := applySchema(sqlDB); err != nil {
		slog.Error("apply schema", "error", err)
		os.Exit(1)
	}

	queries := store.New(sqlDB)

	// Seed admin user if none exists
	seedAdminUser(cfg, queries, sqlDB)

	// Audit C1 bootstrap: grant the seed admin ownership of every existing
	// site that has no member rows yet. Runs every boot but is idempotent
	// (the SQL guards with NOT EXISTS), so it's free after the first
	// successful migration. Without this, an existing deployment upgrading
	// across the C1 fix would lose admin access to all its sites because
	// non-admin role users (added later via /api/admin/invites) would 403
	// on every site-scoped route.
	if users, _ := queries.ListUsers(context.Background()); len(users) > 0 {
		for _, u := range users {
			if u.Role == "admin" {
				if err := queries.BackfillSiteMembersForAdmin(context.Background(), u.ID); err != nil {
					slog.Warn("backfill site_members for admin failed", "user_id", u.ID, "err", err)
				}
				break // Only the first admin needs the backfill row.
			}
		}
	}

	// Phase 30 bootstrap (Cloud Tier MVP, 2026-05-05): ensure at least
	// one workspace exists, every existing user is a member, every
	// existing site is owned by it. Idempotent: once any workspace
	// exists with members + the sites all have a workspace_id, the
	// helper is a no-op on every subsequent boot.
	//
	// In OSS single-deploy mode this is invisible to the operator: the
	// auto-bootstrap workspace named "default" owns every site and the
	// existing admin gets owner role. Cloud installs (-tags ee) skip
	// the auto-bootstrap when CountWorkspaces > 0 and let the signup
	// flow create workspaces explicitly.
	ensureDefaultWorkspace(queries)

	// Sprint 4 (2026-05-24): seed the curated apps catalogue on every
	// boot. UpsertApp is idempotent so the rows get refreshed (new
	// descriptions / credential schemas / version bumps) without
	// touching site_app_installs. Slice A ships 8 curated apps; new
	// publishers add rows via Slice B's publisher console.
	if err := handlers.NewAppsHandler(cfg, queries).SeedCuratedApps(context.Background()); err != nil {
		slog.Warn("apps: seed curated catalogue failed (non-fatal)", "err", err)
	}

	// Set embedded frontend FS
	sub, err := fs.Sub(frontendFiles, "frontend/build")
	if err != nil {
		slog.Warn("no embedded frontend", "error", err)
	} else {
		server.FrontendFS = sub
	}

	// Media storage
	st, err := storage.NewLocalStore(cfg.MediaDir)
	if err != nil {
		slog.Error("create storage", "error", err)
		os.Exit(1)
	}

	// Analytics manager: tails per-site Nginx JSON logs and writes visit_events.
	// Reload-on-settings-change is delegated to handlers (they call Reload after
	// toggling analytics.atomicsite_tracking_enabled).
	analyticsMgr := analytics.NewManager(queries, cfg.AnalyticsSalt)
	mgrCtx, mgrCancel := context.WithCancel(context.Background())
	defer mgrCancel()
	if err := analyticsMgr.Start(mgrCtx); err != nil {
		slog.Warn("analytics: initial start failed", "error", err)
	}

	// Retention manager: daily sweep that purges old visit_events, visit_engagement,
	// visit_sessions, consent_records, and consent_salts. Per-site retention windows
	// come from the general.{analytics,consent,engagement}_retention_days settings;
	// defaults are 180 / 730 / 90 days. Without this the DB grows unbounded; at
	// moderate traffic (10 sites, 1k visitors/day, 5 pageviews each) visit_events
	// alone gains ~7 GB/year.
	retentionMgr := retention.NewManager(queries, sqlDB)
	// Audit H8: tell the retention manager where workspaces live so its
	// daily sweep can reap orphans (workspaces dirs whose siteID is no
	// longer in the DB).
	retentionMgr.SetDataDir(cfg.DataDir)
	retentionMgr.SetAuditLogRetentionDays(cfg.AuditLogRetentionDays)
	retentionMgr.SetLifecyclePolicy(cfg.LifecyclePauseDays, cfg.LifecycleDeleteDays)
	retentionMgr.SetGDPRDeleteCoolingDays(cfg.GDPRDeleteCoolingDays)
	retentionMgr.Start(mgrCtx)

	// Stitched cookie-analytics layer: DuckDB ATTACHes the live SQLite file
	// read-only and serves the /api/sites/{id}/cookies/analytics/* endpoints.
	// Single source of truth (one .db file), columnar query engine on top.
	// If DuckDB fails to open (missing extension, CGO disabled, etc.) we
	// log + continue without analytics rather than abort startup; the
	// CookieAnalyticsHandler returns 503 from each endpoint in that case.
	analyticsMgrCtx, analyticsMgrCancel := context.WithTimeout(context.Background(), 30*time.Second)
	analyticsMgrConn, err := analyticsdb.Open(analyticsMgrCtx, analyticsdb.Config{SQLitePath: cfg.DBPath})
	analyticsMgrCancel()
	if err != nil {
		slog.Warn("analyticsdb: failed to open; stitched analytics endpoints will return 503", "error", err)
		analyticsMgrConn = nil
	}

	// Verify-job manager: async background worker for verify-live crawls
	// (Sprint 4 of the migration system, 2026-05-06). On boot, the
	// manager's Start reaps any rows left in queued/running state from a
	// previous process so a server crash mid-crawl can't leave a job
	// stuck. Workers are spawned per Enqueue call; cancel + shutdown
	// are wired through Stop below.
	verifyJobMgr := migration.NewJobManager(queries)
	verifyJobMgr.Start(mgrCtx)

	// Outbound webhook delivery worker (#6). Ticks every 5s,
	// pulls pending+retrying deliveries due for delivery, and
	// POSTs each with an HMAC-SHA256 signature. Hard cap at 5
	// attempts before flipping a delivery to 'dropped'. The
	// package-level emitter is wired into handlers so the
	// content-mutation paths (pages, items, builds, forms) can
	// fire events without growing each handler's constructor.
	webhookDeliveryMgr := webhook.NewDeliveryManager(queries)
	webhookDeliveryMgr.Start(mgrCtx)
	handlers.SetWebhookEmitter(webhook.NewEmitter(queries))

	srv := server.New(cfg, sqlDB, queries, st)
	srv.AnalyticsDB = analyticsMgrConn
	srv.RetentionMgr = retentionMgr
	srv.SetVerifyJobManager(verifyJobMgr)
	srv.OnAnalyticsSettingsChange = func(_ context.Context) {
		// Use a fresh background context: the request that triggered this may
		// finish before Reload completes, and we don't want a cancelled context
		// to abort the rescan.
		if err := analyticsMgr.Reload(context.Background()); err != nil {
			slog.Warn("analytics: reload after settings change failed", "error", err)
		}
	}

	// Custom-domain reconciler. Wires together: edge writer (nginx
	// fragments), certbot HTTP-01 issuance, optional Cloudflare A-record
	// helper, and the verify/live probes. All collaborators are config-
	// gated so a dev box without root simply records rows without
	// touching the host. The reconciler is wired BEFORE Router() builds
	// so the domain handlers can call Signal().
	domainEdge := &domains.EdgeWriter{
		NginxConfDir:   cfg.NginxConfDir,
		NginxSitesDir:  cfg.NginxSitesDir,
		AcmeWebrootDir: cfg.AcmeWebrootDir,
		SlugSuffix:     cfg.BuiltSiteSuffix,
		HeadersSnippet: "/etc/nginx/snippets/atomicsite-headers.conf",
		CaddyUpstream:  "127.0.0.1:8080",
	}
	domainCertbot := &domains.CertbotRunner{
		BinaryPath:     cfg.CertbotPath,
		WebrootDir:     cfg.AcmeWebrootDir,
		AdminEmail:     os.Getenv("ATOMICSITE_LE_EMAIL"),
		NonInteractive: true,
	}
	cfZones := parseZoneMap(os.Getenv("ATOMICSITE_CLOUDFLARE_ZONES"))
	domainCF := &domains.CloudflareClient{
		APIToken: os.Getenv("ATOMICSITE_CLOUDFLARE_TOKEN"),
		Zones:    cfZones,
	}
	domains.SetReloadCommand(cfg.NginxReloadCommand)
	appUpstream := fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)
	reconciler := domains.NewReconciler(domains.Config{
		Queries:     queries,
		Edge:        domainEdge,
		Certbot:     domainCertbot,
		Cloudflare:  domainCF,
		EdgeIP:      cfg.EdgeIP,
		AppUpstream: appUpstream,
	})
	if cfg.NginxConfDir != "" {
		slog.Info("domain reconciler enabled",
			"nginx_conf_dir", cfg.NginxConfDir,
			"certbot", domainCertbot.Enabled(),
			"cloudflare_zones", len(cfZones))
	} else {
		slog.Info("domain reconciler running in record-only mode (no host integration configured)")
	}
	srv.DomainReconciler = reconciler
	go reconciler.Run(mgrCtx)
	httpSrv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      srv.Router(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		slog.Info("atomicsite: listening", "port", cfg.Port)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("listen", "error", err)
			os.Exit(1)
		}
	}()

	<-quit
	slog.Info("atomicsite: shutting down")

	shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
	analyticsMgr.Stop(shutCtx)
	retentionMgr.Stop(shutCtx)
	verifyJobMgr.Stop(shutCtx)
	webhookDeliveryMgr.Stop(shutCtx)
	if analyticsMgrConn != nil {
		_ = analyticsMgrConn.Close()
	}

	slog.Info("atomicsite: stopped")
}

func applySchema(sqlDB *sql.DB) error {
	// Run column migrations FIRST. addColumnIfMissing is now table-aware: if
	// the table doesn't exist (fresh DB), it no-ops and the schema below
	// will create the table with all columns. If the table exists from an
	// earlier deploy with fewer columns, the missing columns are ALTER-ADDed
	// here so that subsequent CREATE INDEX statements in the schema (which
	// reference these columns) don't fail with "no such column".
	//
	// This ordering matters: a 502 incident on 2026-04-26 was caused by the
	// analytics enrichment columns (browser, os, device, country, lang,
	// utm_*) being added to schema.sql + indexed in the same statement-set
	// without ALTER migrations, so existing prod DBs failed to apply the
	// schema (CREATE INDEX referenced country which didn't exist).
	migrations := []struct{ table, column, spec string }{
		{"pages", "no_index", "INTEGER NOT NULL DEFAULT 0"},
		{"pages", "canonical_url", "TEXT NOT NULL DEFAULT ''"},
		{"deployments", "target_id", "TEXT NOT NULL DEFAULT ''"},
		{"deployments", "deploy_url", "TEXT NOT NULL DEFAULT ''"},
		{"deployments", "deployed_at", "TEXT NOT NULL DEFAULT ''"},
		{"site_silos", "silo_type", "TEXT NOT NULL DEFAULT 'inherit'"},
		// Analytics enrichment (Phase 12.5) added 2026-04-26.
		{"visit_events", "browser", "TEXT NOT NULL DEFAULT ''"},
		{"visit_events", "os", "TEXT NOT NULL DEFAULT ''"},
		{"visit_events", "device", "TEXT NOT NULL DEFAULT ''"},
		{"visit_events", "country", "TEXT NOT NULL DEFAULT ''"},
		{"visit_events", "lang", "TEXT NOT NULL DEFAULT ''"},
		{"visit_events", "utm_source", "TEXT NOT NULL DEFAULT ''"},
		{"visit_events", "utm_medium", "TEXT NOT NULL DEFAULT ''"},
		{"visit_events", "utm_campaign", "TEXT NOT NULL DEFAULT ''"},
		// Branding extra colour slots (Phase 12.9) added 2026-04-27.
		{"sites", "surface_color", "TEXT NOT NULL DEFAULT '#FFFFFF'"},
		{"sites", "border_color", "TEXT NOT NULL DEFAULT '#E5E7EB'"},
		{"sites", "muted_color", "TEXT NOT NULL DEFAULT '#6B7280'"},
		{"sites", "accent_color", "TEXT NOT NULL DEFAULT ''"},
		{"sites", "on_primary_color", "TEXT NOT NULL DEFAULT '#FFFFFF'"},
		// Media folders (Phase 13) added 2026-04-27.
		{"media", "folder", "TEXT NOT NULL DEFAULT ''"},
		// Bidirectional CRM personalization (Phase 18) added 2026-04-28.
		// Required before applySchema executes the matching CREATE INDEX
		// on existing prod DBs (mirrors the 2026-04-26 502 fix pattern).
		{"visit_sessions", "metadata_json", "TEXT NOT NULL DEFAULT '{}'"},
		{"visit_sessions", "metadata_expires_at", "TEXT NOT NULL DEFAULT ''"},
		{"visit_sessions", "identity_confirmed_at", "TEXT NOT NULL DEFAULT ''"},
		// Per-page suppression of global blocks (Phase 14) added 2026-04-29.
		// Lets a landing page hide site-wide nav and footer for higher
		// conversion without breaking the global blocks for the rest of
		// the site.
		{"pages", "hide_global_blocks", "INTEGER NOT NULL DEFAULT 0"},
		// Trusted-domain kind (Phase 15) added 2026-04-29. Existing rows
		// default to 'script' so the upgrade is no-op for current sites.
		// Note: schema.sql also widens UNIQUE to (site_id, domain, kind)
		// but addColumnIfMissing can't redefine constraints; SQLite ignores
		// the older UNIQUE on existing tables and allows the new (id, kind)
		// combination at the application layer (handler dedupes per kind).
		{"allowed_scripts", "kind", "TEXT NOT NULL DEFAULT 'script'"},
		// Cookie analytics stitch key (Phase 11.2) added 2026-05-01.
		// consent_records.fingerprint joins to visit_events / visit_sessions
		// / visit_engagement on the existing fingerprint column so the
		// DuckDB analytics layer can do (site_id, fingerprint) joins
		// without going through visit_sessions.session_id (which doesn't
		// match the client's atomicsite_sid for new visitors).
		{"consent_records", "fingerprint", "TEXT NOT NULL DEFAULT ''"},
		// Block label (schema-driven editor) added 2026-05-01. Surfaces
		// the human-readable name shown in the editor card header
		// without conflating it with data_json.heading. Empty default
		// keeps existing rows valid; the editor falls back to the
		// heading when name is blank.
		{"blocks", "name", "TEXT NOT NULL DEFAULT ''"},
		// TOTP MFA columns on users (Sprint 3, 2026-05-04). Empty
		// secret means "not enrolled"; non-empty + enrolled_at set
		// means the login flow must accept a code. recovery_json
		// holds the bcrypt-hashed single-use recovery codes.
		{"users", "totp_secret", "TEXT NOT NULL DEFAULT ''"},
		{"users", "totp_enrolled_at", "TEXT NOT NULL DEFAULT ''"},
		{"users", "totp_recovery_json", "TEXT NOT NULL DEFAULT '[]'"},
		// Per-tenant quotas (Sprint 3, 2026-05-04). storage_quota_bytes
		// caps total media bytes per site; build_minutes_quota caps the
		// rolling 30-day sum of deployments.duration_ms (in minutes).
		// quota_overage_blocked = 1 makes overage hard-fail with 429;
		// 0 means warn-only (X-Quota-Warning header).
		{"sites", "storage_quota_bytes", "INTEGER NOT NULL DEFAULT 1073741824"},
		{"sites", "build_minutes_quota", "INTEGER NOT NULL DEFAULT 600"},
		{"sites", "quota_overage_blocked", "INTEGER NOT NULL DEFAULT 1"},
		// GDPR Article 17 erasure (2026-05-09). When a user requests
		// account deletion, the timestamp lands here; the daily
		// retention sweep finds rows older than the cooling-off
		// window and hard-deletes them. Empty default keeps existing
		// rows untouched.
		{"users", "deletion_requested_at", "TEXT NOT NULL DEFAULT ''"},
		// Workspace ownership (Phase 30, Cloud Tier MVP, 2026-05-05).
		// Every site is owned by a workspace; OSS installs auto-bootstrap
		// one workspace at boot (ensureDefaultWorkspace) and backfill
		// every existing site into it. Empty default lets the migration
		// land on prod DBs before the bootstrap runs.
		{"sites", "workspace_id", "TEXT NOT NULL DEFAULT ''"},
		// Page archetype lock (gap 6 of the design-quality push,
		// 2026-05-21). Empty default = no lock; set to one of the
		// playbook's vibe archetypes (mesh / pulse / monogram /
		// audit-receipt) to make create_block / update_block flag
		// drift away from the archetype's hero_graphic + motion
		// budget + token set.
		{"pages", "archetype", "TEXT NOT NULL DEFAULT ''"},
		// Image focal-point (Sprint 5 quick-win, 2026-05-24). Two
		// integer percentages used by the renderer's object-position
		// when a crop is applied. Default 50/50 = centered.
		{"media", "focal_x", "INTEGER NOT NULL DEFAULT 50"},
		{"media", "focal_y", "INTEGER NOT NULL DEFAULT 50"},
		// Font unicode-range subset (Sprint A, 2026-06-01). Drives the
		// @font-face unicode-range descriptor so the browser only fetches
		// the woff2 when the page contains characters in the declared
		// range. Saves ~80% on a Latin-only site that uploaded a
		// full-Unicode font file. Values: latin / latin-ext / cyrillic /
		// greek / vietnamese / arabic / hebrew / all. Default 'latin'
		// matches the EU/UK target market; operators bump to 'all' for
		// multi-script sites or upload one file per range.
		{"site_fonts", "subset", "TEXT NOT NULL DEFAULT 'latin'"},
	}
	for _, m := range migrations {
		if err := addColumnIfMissing(sqlDB, m.table, m.column, m.spec); err != nil {
			return fmt.Errorf("migrate %s.%s: %w", m.table, m.column, err)
		}
	}
	// Sprint 3 slice B (2026-05-24): widen entity_revisions.entity_type
	// CHECK to include 'page_locale' + 'block_locale' so revisions
	// can be recorded for the new overlay tables. SQLite doesn't
	// allow ALTER TABLE on CHECK, so we rebuild the table in place
	// when the existing CHECK lacks the new values. CREATE TABLE IF
	// NOT EXISTS in the schema apply below is a no-op on existing
	// tables, so the rebuild has to land BEFORE the schema apply.
	if err := rebuildEntityRevisionsCheckIfNeeded(sqlDB); err != nil {
		return fmt.Errorf("entity_revisions CHECK rebuild: %w", err)
	}
	// Now apply the full schema. CREATE TABLE IF NOT EXISTS is a no-op for
	// existing tables; CREATE INDEX IF NOT EXISTS now succeeds because the
	// referenced columns exist (newly added by the migrations above on
	// existing DBs, or about to be created on fresh DBs).
	if _, err := sqlDB.Exec(string(dbpkg.Schema)); err != nil {
		return err
	}
	// Seed system 'brand' folder for every existing site that doesn't have
	// one. Idempotent thanks to INSERT OR IGNORE on the composite PK. New
	// sites get the row from their own creation handlers.
	if _, err := sqlDB.Exec(`INSERT OR IGNORE INTO media_folders (site_id, name, is_system)
		SELECT id, 'brand', 1 FROM sites`); err != nil {
		return fmt.Errorf("seed brand folder: %w", err)
	}
	// Record schema version so the operator (and the /api/admin/metrics
	// endpoint) can answer "what migration set is this DB at?" with a
	// single query. The version equals len(migrations) , every additional
	// entry in the migrations slice bumps it. INSERT OR IGNORE keeps the
	// statement idempotent across reboots.
	migVersion := len(migrations)
	if _, err := sqlDB.Exec(
		`INSERT OR IGNORE INTO schema_versions (version, note) VALUES (?, ?)`,
		migVersion, fmt.Sprintf("applied %d column migrations", migVersion),
	); err != nil {
		// Non-fatal: tracking row failure mustn't break startup, but
		// surface it loudly so an operator notices the gap.
		slog.Warn("schema_versions: record version failed (non-fatal)", "version", migVersion, "err", err)
	}
	return nil
}

// addColumnIfMissing checks PRAGMA table_info and adds the column only if it's
// not present. No-ops when the table itself does not exist yet (fresh DB),
// in which case the caller's schema apply will create the table with the
// column already declared. Works on all SQLite versions (doesn't require
// ALTER TABLE IF NOT EXISTS which is SQLite 3.35+).
func addColumnIfMissing(sqlDB *sql.DB, table, column, spec string) error {
	// Probe table existence first. Without this, PRAGMA returns no rows on
	// a non-existent table and the subsequent ALTER TABLE fails with
	// "no such table". When applySchema runs migrations BEFORE the schema
	// exec (the safe ordering), fresh DBs have no tables yet, so we must
	// no-op here and let CREATE TABLE handle column declarations.
	var name string
	if err := sqlDB.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}

	rows, err := sqlDB.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var colName, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &colName, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if colName == column {
			return nil // already present
		}
	}
	_, err = sqlDB.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, spec))
	return err
}

// rebuildEntityRevisionsCheckIfNeeded widens the CHECK constraint on
// entity_revisions.entity_type to include 'page_locale' + 'block_locale'
// when the existing table predates Sprint 3 slice B. SQLite doesn't
// support ALTER TABLE on CHECK so we do a table-rebuild migration:
// detect whether the constraint is current, and if not, rename the
// old table, create the wider-CHECK table from schema, copy rows,
// drop old, recreate indexes. Idempotent: subsequent boots find the
// constraint already wide and no-op.
//
// On fresh DBs (no entity_revisions table yet) this returns nil and
// the schema apply below creates the table with the wide CHECK.
func rebuildEntityRevisionsCheckIfNeeded(sqlDB *sql.DB) error {
	// Read the table's stored SQL definition from sqlite_master.
	var stmt string
	err := sqlDB.QueryRow("SELECT sql FROM sqlite_master WHERE type='table' AND name='entity_revisions'").Scan(&stmt)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	// If the CHECK already lists page_locale, nothing to do.
	if strings.Contains(stmt, "'page_locale'") {
		return nil
	}
	slog.Info("entity_revisions: widening CHECK constraint to include page_locale + block_locale")
	tx, err := sqlDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Drop the FK-sensitive indexes first so the old table can be
	// renamed without index collisions.
	if _, err := tx.Exec(`DROP INDEX IF EXISTS idx_entity_revisions_lookup`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP INDEX IF EXISTS idx_entity_revisions_site_created`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE entity_revisions RENAME TO entity_revisions_old`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TABLE entity_revisions (
		id              TEXT PRIMARY KEY,
		site_id         TEXT NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		entity_type     TEXT NOT NULL
		                CHECK (entity_type IN ('page','block','page_locale','block_locale')),
		entity_id       TEXT NOT NULL,
		version_number  INTEGER NOT NULL,
		snapshot_json   TEXT NOT NULL,
		change_summary  TEXT NOT NULL DEFAULT '',
		created_by      TEXT NOT NULL DEFAULT '',
		created_at      TEXT NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO entity_revisions
		(id, site_id, entity_type, entity_id, version_number, snapshot_json, change_summary, created_by, created_at)
		SELECT id, site_id, entity_type, entity_id, version_number, snapshot_json, change_summary, created_by, created_at
		FROM entity_revisions_old`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE entity_revisions_old`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_entity_revisions_lookup
		ON entity_revisions(site_id, entity_type, entity_id, version_number DESC)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_entity_revisions_site_created
		ON entity_revisions(site_id, created_at DESC)`); err != nil {
		return err
	}
	return tx.Commit()
}

func seedAdminUser(cfg *config.Config, queries *store.Queries, sqlDB *sql.DB) {
	ctx := context.Background()
	users, err := queries.ListUsers(ctx)
	if err != nil || len(users) > 0 {
		return
	}

	email := os.Getenv("ADMIN_EMAIL")
	password := os.Getenv("ADMIN_PASSWORD")
	if email == "" {
		email = "admin@atomicsite.dev"
	}

	// Production-like deployments must set ADMIN_PASSWORD explicitly.
	// The audit's C3 finding: defaulting to "changeme123" lets a deploy
	// without env vars come up with a known-by-everyone admin login.
	// Local dev keeps the convenience default so contributors don't have
	// to set five env vars to run `go run`.
	if err := cfg.ValidateAdminPassword(password); err != nil {
		slog.Error("seed admin: refusing to seed with weak/default password", "error", err.Error())
		// Fail hard rather than silently using changeme123. The first
		// boot's logs are the moment to surface this.
		os.Exit(1)
	}
	if password == "" {
		// Reached only in local dev (Validate would have refused otherwise).
		password = config.DefaultAdminPassword
		slog.Warn("seeded admin with documented default password , local dev only", "email", email)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), handlers.PasswordBcryptCost)
	if err != nil {
		slog.Error("seed admin: hash password", "error", err)
		return
	}

	err = queries.CreateUser(ctx, store.CreateUserParams{
		ID:           "admin",
		Email:        email,
		PasswordHash: string(hash),
		Name:         "Admin",
		Role:         "admin",
	})
	if err != nil {
		slog.Error("seed admin: create user", "error", err)
		return
	}

	slog.Info("seeded admin user", "email", email)
}

// ensureDefaultWorkspace runs after seedAdminUser and the C1
// site_members backfill. Idempotent. Three steps:
//
//  1. If at least one workspace already exists, do nothing further than
//     the per-workspace member backfill (covers OSS upgrades from a
//     prior boot where the workspace was created but a new admin was
//     added later).
//  2. Otherwise create a workspace named "Default" with slug "default"
//     and plan "oss". The workspace id flows into the sites backfill
//     in step 3 so every existing site lands inside it.
//  3. Backfill: every user with role='admin' becomes a workspace owner;
//     every site with workspace_id='' is moved into the new workspace.
//     Both queries are idempotent (NOT EXISTS / WHERE = '').
//
// Phase 30, Cloud Tier MVP, 2026-05-05.
func ensureDefaultWorkspace(queries *store.Queries) {
	ctx := context.Background()
	count, err := queries.CountWorkspaces(ctx)
	if err != nil {
		slog.Warn("ensureDefaultWorkspace: count failed", "err", err)
		return
	}
	var workspaceID string
	if count == 0 {
		workspaceID = newID()
		if err := queries.CreateWorkspace(ctx, store.CreateWorkspaceParams{
			ID:           workspaceID,
			Name:         "Default",
			Slug:         "default",
			Plan:         "oss",
			Region:       "eu",
			BillingEmail: "",
			Status:       "active",
			TrialEndsAt:  "",
		}); err != nil {
			slog.Warn("ensureDefaultWorkspace: create failed", "err", err)
			return
		}
		slog.Info("ensureDefaultWorkspace: created default workspace", "id", workspaceID)
	} else {
		// Use the first existing workspace as the home for any
		// orphaned sites left over from a partial migration. The
		// CountWorkspaces > 0 path also covers Cloud installs where
		// signup creates workspaces explicitly.
		all, err := queries.ListAllWorkspaces(ctx)
		if err != nil || len(all) == 0 {
			return
		}
		workspaceID = all[0].ID
	}

	// Add every admin as workspace owner if no member rows yet.
	users, err := queries.ListUsers(ctx)
	if err == nil {
		for _, u := range users {
			if u.Role == "admin" {
				if err := queries.BackfillWorkspaceMembersForAdmin(ctx, u.ID); err != nil {
					slog.Warn("ensureDefaultWorkspace: backfill admin failed", "user_id", u.ID, "err", err)
				}
				break // First admin is sufficient for the bootstrap row.
			}
		}
	}

	// Backfill orphan sites into the default workspace.
	if err := queries.BackfillSitesWorkspace(ctx, workspaceID); err != nil {
		slog.Warn("ensureDefaultWorkspace: backfill sites failed", "err", err)
	}
}

// newID generates a 24-hex ID matching the existing site/page id
// pattern. Used by ensureDefaultWorkspace; handlers/helpers.go has its
// own copy so the bootstrap stays decoupled from package handlers.
func newID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// Read failures are catastrophic; the boot path will retry
		// next start. Returning an empty id fails the CreateWorkspace
		// fast (PRIMARY KEY violation) so we never silently mint a
		// shared id.
		return ""
	}
	return hex.EncodeToString(b)
}

// resetPasswordCLI is the `atomicsite reset-password <email>` subcommand.
// Reads the new password from stdin (one line, no echo-off - run with
// `docker exec -i` from a terminal you trust). Bumps token_version so any
// existing JWTs for that user are invalidated.
//
// Usage:
//
//	docker exec -i atomicsite /app/server reset-password admin@example.com
//	(then type the new password and press enter)
func resetPasswordCLI(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: atomicsite reset-password <email>")
		os.Exit(2)
	}
	email := strings.TrimSpace(strings.ToLower(args[0]))
	if email == "" {
		fmt.Fprintln(os.Stderr, "email is required")
		os.Exit(2)
	}

	cfg := config.Load()
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "create data dir:", err)
		os.Exit(1)
	}
	sqlDB, err := sql.Open("sqlite", cfg.DBPath+"?_journal_mode=WAL&_busy_timeout=5000&_pragma=foreign_keys(1)")
	if err != nil {
		fmt.Fprintln(os.Stderr, "open db:", err)
		os.Exit(1)
	}
	defer sqlDB.Close()
	queries := store.New(sqlDB)

	ctx := context.Background()
	user, err := queries.GetUserByEmail(ctx, email)
	if err != nil {
		fmt.Fprintf(os.Stderr, "no user with email %q\n", email)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "Enter new password for %s: ", email)
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		fmt.Fprintln(os.Stderr, "no input")
		os.Exit(1)
	}
	newPwd := strings.TrimRight(scanner.Text(), "\r\n")
	// Audit L5: bump CLI password floor to match the API ChangePassword
	// path (12 chars). Same NIST rationale: admin-class accounts get
	// the higher bar.
	if len(newPwd) < 12 {
		fmt.Fprintln(os.Stderr, "password must be at least 12 characters")
		os.Exit(1)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(newPwd), handlers.PasswordBcryptCost)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hash:", err)
		os.Exit(1)
	}
	if err := queries.UpdateUserPassword(ctx, store.UpdateUserPasswordParams{
		PasswordHash: string(hash),
		ID:           user.ID,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "update:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "password reset for %s (active sessions invalidated)\n", email)
}

// parseZoneMap reads ATOMICSITE_CLOUDFLARE_ZONES, formatted as
// "apex1=zoneID1,apex2=zoneID2,...". Returns nil for empty input so
// the Cloudflare helper stays disabled by default.
func parseZoneMap(s string) map[string]string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			continue
		}
		apex := strings.ToLower(strings.TrimSpace(parts[0]))
		zoneID := strings.TrimSpace(parts[1])
		if apex != "" && zoneID != "" {
			out[apex] = zoneID
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// backupDBCLI is the `atomicsite backup-db [--output=PATH]` subcommand.
// Uses SQLite's VACUUM INTO to take a WAL-consistent online snapshot
// without locking writers, then optionally compresses with gzip.
//
// Why VACUUM INTO instead of cp /data/atomicsite.db: a hot copy of
// the .db file can land mid-WAL-checkpoint and produce a corrupt
// snapshot. VACUUM INTO acquires a read transaction, materialises a
// page-by-page copy at the destination, and writes it consistently.
// SQLite docs: https://sqlite.org/lang_vacuum.html#vacuuminto
//
// Usage:
//   atomicsite backup-db                        # /data/backups/atomicsite-{ts}.db
//   atomicsite backup-db --output=/path/file.db
//   atomicsite backup-db --gzip                 # appends .gz, gzip stream
//   atomicsite backup-db --output=/dev/stdout   # pipe to S3, B2, age, gpg
func backupDBCLI(args []string) {
	cfg := config.Load()

	outPath := ""
	gzipOut := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--gzip" || a == "-z":
			gzipOut = true
		case strings.HasPrefix(a, "--output="):
			outPath = strings.TrimPrefix(a, "--output=")
		case a == "--output" || a == "-o":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--output requires a path")
				os.Exit(2)
			}
			outPath = args[i+1]
			i++
		default:
			fmt.Fprintf(os.Stderr, "unknown arg: %s\n", a)
			os.Exit(2)
		}
	}

	if outPath == "" {
		ts := time.Now().UTC().Format("20060102T150405Z")
		ext := ".db"
		if gzipOut {
			ext = ".db.gz"
		}
		outPath = filepath.Join(cfg.DataDir, "backups", "atomicsite-"+ts+ext)
	}

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "create backup dir:", err)
		os.Exit(1)
	}

	sqlDB, err := sql.Open("sqlite", cfg.DBPath+"?_journal_mode=WAL&_busy_timeout=10000")
	if err != nil {
		fmt.Fprintln(os.Stderr, "open db:", err)
		os.Exit(1)
	}
	defer sqlDB.Close()

	if gzipOut {
		// Two-phase: VACUUM INTO a tmp file, then stream-gzip it to the
		// final destination. SQLite can't VACUUM INTO a stream, so the
		// tmp file is unavoidable.
		tmpPath := outPath + ".tmp"
		defer os.Remove(tmpPath)
		if _, err := sqlDB.Exec("VACUUM INTO ?", tmpPath); err != nil {
			fmt.Fprintln(os.Stderr, "vacuum into:", err)
			os.Exit(1)
		}
		src, err := os.Open(tmpPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "open tmp:", err)
			os.Exit(1)
		}
		defer src.Close()
		dst, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, "open output:", err)
			os.Exit(1)
		}
		defer dst.Close()
		gz := gzip.NewWriter(dst)
		if _, err := io.Copy(gz, src); err != nil {
			fmt.Fprintln(os.Stderr, "gzip copy:", err)
			os.Exit(1)
		}
		if err := gz.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "gzip close:", err)
			os.Exit(1)
		}
	} else {
		if _, err := sqlDB.Exec("VACUUM INTO ?", outPath); err != nil {
			fmt.Fprintln(os.Stderr, "vacuum into:", err)
			os.Exit(1)
		}
	}

	info, err := os.Stat(outPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "stat output:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "backup ok: %s (%d bytes)\n", outPath, info.Size())
}

// listOrphansCLI is the `atomicsite list-orphans` subcommand. Walks
// {DataDir}/workspaces/ and reports any directory whose siteID is no
// longer in the sites table. Read-only; the retention manager actually
// reaps these on its daily sweep, but operators want a one-shot
// "what's eating disk right now?" view.
//
// Usage: atomicsite list-orphans
func listOrphansCLI(args []string) {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "list-orphans takes no arguments")
		os.Exit(2)
	}
	cfg := config.Load()
	wsRoot := filepath.Join(cfg.DataDir, "workspaces")
	entries, err := os.ReadDir(wsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "no workspaces dir at", wsRoot)
			return
		}
		fmt.Fprintln(os.Stderr, "read workspaces:", err)
		os.Exit(1)
	}

	sqlDB, err := sql.Open("sqlite", cfg.DBPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		fmt.Fprintln(os.Stderr, "open db:", err)
		os.Exit(1)
	}
	defer sqlDB.Close()
	queries := store.New(sqlDB)
	sites, err := queries.ListSites(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "list sites:", err)
		os.Exit(1)
	}
	known := make(map[string]bool, len(sites))
	for _, s := range sites {
		known[s.ID] = true
	}

	var totalBytes int64
	orphanCount := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		if known[id] {
			continue
		}
		size := dirSizeBytes(filepath.Join(wsRoot, id))
		totalBytes += size
		orphanCount++
		fmt.Printf("%s\t%d\t%s\n", id, size, filepath.Join(wsRoot, id))
	}
	fmt.Fprintf(os.Stderr, "%d orphan workspaces, %.2f MB total\n",
		orphanCount, float64(totalBytes)/1024/1024)
}

// dirSizeBytes walks a directory and sums file sizes. Returns 0 on
// any error so the CLI can still proceed with the rest of the dirs.
func dirSizeBytes(root string) int64 {
	var total int64
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}
