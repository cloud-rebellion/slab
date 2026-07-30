// Package mcp - tools_migrations.go (Sprint 4.5, 2026-05-06).
//
// Exposes the migration system + Sprint 2 missing-URLs + Sprint 4 async
// verify-jobs through MCP so an AI orchestrator can drive an end-to-end
// site migration without going around the MCP boundary. Each tool is a
// thin wrapper that calls the same store / migration package functions
// the HTTP handlers use, scoped to agent.SiteID for cross-tenant safety.
//
// Privacy boundary: this surface mirrors the routes under
// /api/sites/{siteID}/migrations/* and /api/sites/{siteID}/missing-urls/*
// (no visitor PII, no identified-tier data). The agent reads + writes
// the same rows the operator would via the admin UI - same validation,
// same audit shape (note: AuditLog calls require an *http.Request and
// thus aren't fired here; an MCP-call audit story lives in mcp.go's
// dispatcher metadata).
//
// What's NOT exposed: visitor-metadata reads, billing endpoints, user
// management. The migration system was designed agent-friendly but
// every tool here still respects the per-key write capability gate
// (RequiresWrite=true on mutators).

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"

	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/migration"
	"github.com/bright-interaction/slab/internal/store"
)

// registerMigrationTools wires the migration / missing-urls / verify-job
// tools. Called from registerTools() in tools.go alongside the other
// surface registries.
func (s *Server) registerMigrationTools() {
	register := func(t Tool) { s.tools[t.Name] = t }

	// ----------------------------------------------------------------
	// Read-only migration surface
	// ----------------------------------------------------------------

	register(Tool{
		Name:        "list_migrations",
		Description: "Lists every migration row for the site, newest first. Each entry has id, source_url, source_type (sitemap|wordpress|webflow|ghost), status (pending|ready|applied|failed), error, created_at, updated_at, plus a small stats summary {pages, collections, media} parsed from the manifest. Use to see what has been crawled and which migration is ready to apply.",
		InputSchema: schema(`{"type":"object","properties":{}}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, _ json.RawMessage) (string, error) {
			rows, err := s.queries.ListMigrationsBySite(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			out := make([]map[string]any, 0, len(rows))
			for _, m := range rows {
				out = append(out, migrationSummaryForMCP(m))
			}
			return mustJSON(out), nil
		},
	})

	register(Tool{
		Name:        "get_migration",
		Description: "Returns the full manifest body for a migration row (potentially big - 10s of MB on large crawls). Includes parsed pages, collections, items, media, and the operator-facing stats. Pair with plan_migration to see how the URL planner would rewrite slugs before apply_migration commits the import.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"migration_id":{"type":"string"}
			},
			"required":["migration_id"]
		}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				MigrationID string `json:"migration_id"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			row, err := s.queries.GetMigrationByID(ctx, args.MigrationID)
			if err != nil || row.SiteID != agent.SiteID {
				return "", errors.New("migration not found")
			}
			var manifest migration.MigrationManifest
			_ = json.Unmarshal([]byte(row.ManifestJson), &manifest)
			return mustJSON(map[string]any{
				"id":          row.ID,
				"site_id":     row.SiteID,
				"source_url":  row.SourceUrl,
				"source_type": row.SourceType,
				"status":      row.Status,
				"error":       row.Error,
				"created_at":  row.CreatedAt,
				"updated_at":  row.UpdatedAt,
				"manifest":    manifest,
			}), nil
		},
	})

	// ----------------------------------------------------------------
	// Write-side: start crawl
	// ----------------------------------------------------------------

	register(Tool{
		Name:        "start_migration_crawl",
		Description: "Starts an ASYNC crawl of a source CMS and stores the parsed manifest when done. Returns immediately with the migration id and status 'crawling'; poll get_migration until status is 'ready' (then plan_migration/apply_migration) or 'failed' (error field explains). Large WordPress sites (1000+ posts) take minutes, which is why the crawl no longer runs inside the request (it used to exceed the server's write timeout and the response was lost). Set source_type to one of: sitemap, wordpress, webflow, ghost. Auth varies per source: WordPress uses Application Password via wordpress_auth_header (\"Basic base64(user:apppass)\"); Webflow needs webflow_site_id + webflow_auth_token; Ghost needs ghost_content_key. Status flow: crawling -> ready -> applied | failed.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"source_type":{"type":"string","enum":["sitemap","wordpress","webflow","ghost"]},
				"source_url":{"type":"string"},
				"wordpress_auth_header":{"type":"string"},
				"webflow_site_id":{"type":"string"},
				"webflow_auth_token":{"type":"string"},
				"ghost_content_key":{"type":"string"}
			},
			"required":["source_type","source_url"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				SourceType       string `json:"source_type"`
				SourceURL        string `json:"source_url"`
				WordPressAuth    string `json:"wordpress_auth_header"`
				WebflowSiteID    string `json:"webflow_site_id"`
				WebflowAuthToken string `json:"webflow_auth_token"`
				GhostContentKey  string `json:"ghost_content_key"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			if args.SourceURL == "" {
				return "", errors.New("source_url is required")
			}
			// Create the row first, then crawl in the background: a
			// multi-minute crawl inside the JSON-RPC request used to
			// blow past the HTTP server's write timeout, so the tool
			// result never reached the agent even when the crawl worked.
			id := newID()
			if err := s.queries.CreateMigration(ctx, store.CreateMigrationParams{
				ID: id, SiteID: agent.SiteID, SourceUrl: args.SourceURL, SourceType: args.SourceType,
				ManifestJson: "{}", Status: "crawling", Error: "",
			}); err != nil {
				return "", err
			}
			go func() {
				crawlCtx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
				defer cancel()
				manifest, err := runMCPImporter(crawlCtx, args.SourceType, args.SourceURL, args.WordPressAuth, args.WebflowSiteID, args.WebflowAuthToken, args.GhostContentKey)
				if err == nil {
					var manifestJSON []byte
					// Redact addresses from crawl warnings before they are stored. Warnings
					// travel back to the agent inside the manifest and are read by the same
					// tenant-facing surfaces as the error field, so they carry the same
					// disclosure risk. Redacted in place rather than dropped, so the tenant
					// still learns which page had a problem.
					manifest.Warnings = migration.SafeWarnings(manifest.Warnings)
					manifestJSON, err = json.Marshal(manifest)
					if err == nil {
						err = s.queries.UpdateMigrationManifest(crawlCtx, store.UpdateMigrationManifestParams{
							ManifestJson: string(manifestJSON), Status: "ready", Error: "", ID: id,
						})
					}
				}
				if err != nil {
					slog.Warn("migration crawl failed", "migration_id", id, "site_id", agent.SiteID, "err", err)
					if uerr := s.queries.UpdateMigrationStatus(context.Background(), store.UpdateMigrationStatusParams{
						ID: id, Status: "failed", Error: migration.SafeErrorText(err),
					}); uerr != nil {
						slog.Error("migration crawl: persisting failed status failed", "migration_id", id, "err", uerr)
					}
				}
			}()
			return mustJSON(map[string]any{
				"id":     id,
				"status": "crawling",
				"hint":   "Crawl runs in the background. Poll get_migration with this id until status is ready (then plan_migration), or failed (error field explains).",
			}), nil
		},
	})

	// ----------------------------------------------------------------
	// Write-side: plan + apply
	// ----------------------------------------------------------------

	register(Tool{
		Name:        "plan_migration",
		Description: "Generates a URL plan for a stored manifest without committing. Returns the per-page rewrite (source_path -> new_slug) plus any conflicts (slugs that collide with live pages on the site). Use to preview what apply_migration will do. Optional: skip_paths (array of source paths to drop from the import), collapse_date_archives (defaults true; rolls /2024/01/post into /post), preserve_locale_prefix (defaults false).",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"migration_id":{"type":"string"},
				"skip_paths":{"type":"array","items":{"type":"string"}},
				"collapse_date_archives":{"type":"boolean"},
				"preserve_locale_prefix":{"type":"boolean"}
			},
			"required":["migration_id"]
		}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				MigrationID          string   `json:"migration_id"`
				SkipPaths            []string `json:"skip_paths"`
				CollapseDateArchives *bool    `json:"collapse_date_archives"`
				PreserveLocalePrefix *bool    `json:"preserve_locale_prefix"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			row, err := s.queries.GetMigrationByID(ctx, args.MigrationID)
			if err != nil || row.SiteID != agent.SiteID {
				return "", errors.New("migration not found")
			}
			var manifest migration.MigrationManifest
			if err := json.Unmarshal([]byte(row.ManifestJson), &manifest); err != nil {
				return "", fmt.Errorf("decode manifest: %w", err)
			}
			pages, _ := s.queries.ListPagesBySite(ctx, agent.SiteID)
			existing := make(map[string]bool, len(pages))
			for _, p := range pages {
				existing[strings.TrimPrefix(p.Slug, "/")] = true
			}
			opts := migration.DefaultPlanOptions()
			if args.CollapseDateArchives != nil {
				opts.CollapseDateArchives = *args.CollapseDateArchives
			}
			if args.PreserveLocalePrefix != nil {
				opts.PreserveLocalePrefix = *args.PreserveLocalePrefix
			}
			for _, p := range args.SkipPaths {
				opts.SkipPaths[p] = true
			}
			plan := migration.PlanURLs(&manifest, existing, opts)
			return mustJSON(plan), nil
		},
	})

	register(Tool{
		Name:        "apply_migration",
		Description: "Commits a migration's plan to the live store: creates pages + collection_items + redirects + (re-uploads) media. Returns ApplyResult with per-section counts. Set upsert=true for re-imports (annual content refresh) - existing rows are UPDATED in place by their natural key (pages by slug, redirects by from_path, items by collection_id+locale+slug) instead of erroring on the unique-index conflict; the response splits PagesCreated/PagesUpdated and the plan-quota gate subtracts already-existing slugs from the projected addition. Plan-quota: if the workspace plan is solo/studio/agency the import is gated by max_pages_per_site - a 51-page manifest on solo cap=50 returns an error here. Same skip_paths / planner overrides as plan_migration.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"migration_id":{"type":"string"},
				"upsert":{"type":"boolean"},
				"skip_paths":{"type":"array","items":{"type":"string"}},
				"collapse_date_archives":{"type":"boolean"},
				"preserve_locale_prefix":{"type":"boolean"}
			},
			"required":["migration_id"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				MigrationID          string   `json:"migration_id"`
				Upsert               bool     `json:"upsert"`
				SkipPaths            []string `json:"skip_paths"`
				CollapseDateArchives *bool    `json:"collapse_date_archives"`
				PreserveLocalePrefix *bool    `json:"preserve_locale_prefix"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			row, err := s.queries.GetMigrationByID(ctx, args.MigrationID)
			if err != nil || row.SiteID != agent.SiteID {
				return "", errors.New("migration not found")
			}
			if row.Status == "applied" && !args.Upsert {
				return "", errors.New("migration already applied; pass upsert=true to refresh")
			}
			var manifest migration.MigrationManifest
			if err := json.Unmarshal([]byte(row.ManifestJson), &manifest); err != nil {
				return "", fmt.Errorf("decode manifest: %w", err)
			}
			// Mirror handler.Apply: under upsert pass empty existing map
			// so the planner doesn't generate auto-rename conflicts.
			existing := map[string]bool{}
			if !args.Upsert {
				pages, _ := s.queries.ListPagesBySite(ctx, agent.SiteID)
				existing = make(map[string]bool, len(pages))
				for _, p := range pages {
					existing[strings.TrimPrefix(p.Slug, "/")] = true
				}
			}
			opts := migration.DefaultPlanOptions()
			if args.CollapseDateArchives != nil {
				opts.CollapseDateArchives = *args.CollapseDateArchives
			}
			if args.PreserveLocalePrefix != nil {
				opts.PreserveLocalePrefix = *args.PreserveLocalePrefix
			}
			for _, p := range args.SkipPaths {
				opts.SkipPaths[p] = true
			}
			plan := migration.PlanURLs(&manifest, existing, opts)

			media := s.media
			if media == nil {
				// Nil media uploader: porter records each image as a
				// warning. Migration still applies; operator retries
				// media later. Mirrors the boot default in server.go
				// when migrationMediaH isn't wired.
				media = noopMCPMediaUploader{}
			}
			porter := migration.NewPorter(s.queries, media)
			result, err := porter.ApplyWithOptions(ctx, agent.SiteID, &manifest, plan,
				migration.ApplyOptions{Upsert: args.Upsert})
			if err != nil {
				_ = s.queries.UpdateMigrationStatus(ctx, store.UpdateMigrationStatusParams{
					ID: args.MigrationID, Status: "failed", Error: migration.SafeErrorText(err),
				})
				return "", err
			}
			if err := s.queries.UpdateMigrationStatus(ctx, store.UpdateMigrationStatusParams{
				ID: args.MigrationID, Status: "applied", Error: "",
			}); err != nil {
				// The apply itself succeeded; a stuck 'ready' status
				// invites a second apply (duplicate pages), so be loud.
				slog.Error("migration: apply succeeded but status flip to applied failed", "migration_id", args.MigrationID, "site_id", agent.SiteID, "err", err)
			}
			return mustJSON(result), nil
		},
	})

	register(Tool{
		Name:        "delete_migration",
		Description: "Drops a stored migration row. Pages/items/redirects already created from a previously-applied migration are NOT deleted - the operator owns those rows now. Use after a failed crawl when you want to retry from scratch, or after an applied migration to clear the stored manifest.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"migration_id":{"type":"string"}
			},
			"required":["migration_id"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				MigrationID string `json:"migration_id"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			row, err := s.queries.GetMigrationByID(ctx, args.MigrationID)
			if err != nil || row.SiteID != agent.SiteID {
				return "", errors.New("migration not found")
			}
			if err := s.queries.DeleteMigration(ctx, args.MigrationID); err != nil {
				return "", err
			}
			return mustJSON(map[string]any{"status": "deleted"}), nil
		},
	})

	// ----------------------------------------------------------------
	// Verify-live (async post-launch crawl)
	//
	// Note: pre-launch verify-coverage (the 5-bucket dry-run diff) is
	// intentionally NOT exposed via MCP. The bucketing logic lives in
	// MigrationHandler.VerifyCoverage and depends on handler-private
	// helpers (compiledRegexRedirect, normalizeVerifyPath, extractURLPath)
	// that would have to duplicate ~80 lines here. Until those are
	// promoted to the migration package, the AI uses verify_migration_live
	// for the higher-leverage post-DNS-flip variant.
	// ----------------------------------------------------------------

	register(Tool{
		Name:        "verify_migration_live",
		Description: "Kicks off an ASYNC post-launch crawl against the deployed atomicsite. Returns a job_id immediately (status=queued); poll get_verify_job for progress. Each URL is fetched, redirects followed up to 3 hops, terminal status recorded. Per-URL results land in migration_verifications (read via list_migration_verifications). The verify_jobs row aggregates total + processed + ok/fail counts. Cap: 25,000 URLs per run. deployed_domain is what DNS now points at (e.g. \"www.acme.com\"). Use AFTER flipping DNS to prove every old URL one-hops to 200 against the live site.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"migration_id":{"type":"string"},
				"urls":{"type":"array","items":{"type":"string"}},
				"deployed_domain":{"type":"string"}
			},
			"required":["migration_id","urls","deployed_domain"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			if s.verifyJobMgr == nil {
				return "", errors.New("verify job manager not configured")
			}
			var args struct {
				MigrationID    string   `json:"migration_id"`
				URLs           []string `json:"urls"`
				DeployedDomain string   `json:"deployed_domain"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			if strings.TrimSpace(args.DeployedDomain) == "" {
				return "", errors.New("deployed_domain is required")
			}
			if len(args.URLs) == 0 {
				return "", errors.New("urls is required")
			}
			if len(args.URLs) > 25000 {
				return "", errors.New("verify-live capped at 25,000 URLs per run; split the input set")
			}
			row, err := s.queries.GetMigrationByID(ctx, args.MigrationID)
			if err != nil || row.SiteID != agent.SiteID {
				return "", errors.New("migration not found")
			}
			jobID, err := s.verifyJobMgr.Enqueue(ctx, agent.SiteID, args.MigrationID, args.URLs, args.DeployedDomain)
			if err != nil {
				return "", err
			}
			return mustJSON(map[string]any{
				"job_id":     jobID,
				"status":     "queued",
				"total_urls": len(args.URLs),
			}), nil
		},
	})

	register(Tool{
		Name:        "list_verify_jobs",
		Description: "Returns every verify-live job for a migration, newest first. Each row has job id, status (queued|running|done|failed|cancelled), total_urls, processed_urls, ok_count, fail_count, deployed_domain, error, created_at, started_at, completed_at. Use to surface past run history + spot a still-running job that may need cancelling.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"migration_id":{"type":"string"}
			},
			"required":["migration_id"]
		}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				MigrationID string `json:"migration_id"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			row, err := s.queries.GetMigrationByID(ctx, args.MigrationID)
			if err != nil || row.SiteID != agent.SiteID {
				return "", errors.New("migration not found")
			}
			jobs, err := s.queries.ListVerifyJobsByMigration(ctx, args.MigrationID)
			if err != nil {
				return "", err
			}
			return mustJSON(jobs), nil
		},
	})

	register(Tool{
		Name:        "get_verify_job",
		Description: "Polling endpoint for an async verify-live run. Returns current status + processed/ok/fail counters. The agent should poll every 2-5s while status is queued or running. Terminal states: done | failed | cancelled. After done, call list_migration_verifications for per-URL detail.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"migration_id":{"type":"string"},
				"job_id":{"type":"string"}
			},
			"required":["migration_id","job_id"]
		}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				MigrationID string `json:"migration_id"`
				JobID       string `json:"job_id"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			row, err := s.queries.GetMigrationByID(ctx, args.MigrationID)
			if err != nil || row.SiteID != agent.SiteID {
				return "", errors.New("migration not found")
			}
			job, err := s.queries.GetVerifyJob(ctx, args.JobID)
			if err != nil || job.SiteID != agent.SiteID || job.MigrationID != args.MigrationID {
				return "", errors.New("verify job not found")
			}
			return mustJSON(job), nil
		},
	})

	register(Tool{
		Name:        "cancel_verify_job",
		Description: "Signals a running verify-live worker to stop. Returns immediately; the worker observes context cancellation through the errgroup and writes status='cancelled' on its next probe boundary. Errors if the job is unknown or already terminal.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"migration_id":{"type":"string"},
				"job_id":{"type":"string"}
			},
			"required":["migration_id","job_id"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			if s.verifyJobMgr == nil {
				return "", errors.New("verify job manager not configured")
			}
			var args struct {
				MigrationID string `json:"migration_id"`
				JobID       string `json:"job_id"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			row, err := s.queries.GetMigrationByID(ctx, args.MigrationID)
			if err != nil || row.SiteID != agent.SiteID {
				return "", errors.New("migration not found")
			}
			job, err := s.queries.GetVerifyJob(ctx, args.JobID)
			if err != nil || job.SiteID != agent.SiteID || job.MigrationID != args.MigrationID {
				return "", errors.New("verify job not found")
			}
			switch job.Status {
			case "done", "failed", "cancelled":
				return "", errors.New("verify job already terminal")
			}
			if !s.verifyJobMgr.Cancel(args.JobID) {
				return "", errors.New("verify job not running on this server")
			}
			return mustJSON(map[string]string{"status": "cancelling"}), nil
		},
	})

	register(Tool{
		Name:        "list_migration_verifications",
		Description: "Per-URL verification history for a migration, newest first. Use after get_verify_job reports done to walk the failures. Each row has source_url, status_code, final_url, hops, ok (boolean), error, checked_at. Aggregate ok/failed counts also returned.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"migration_id":{"type":"string"}
			},
			"required":["migration_id"]
		}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				MigrationID string `json:"migration_id"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			row, err := s.queries.GetMigrationByID(ctx, args.MigrationID)
			if err != nil || row.SiteID != agent.SiteID {
				return "", errors.New("migration not found")
			}
			rows, err := s.queries.ListMigrationVerifications(ctx, args.MigrationID)
			if err != nil {
				return "", err
			}
			counts, _ := s.queries.CountMigrationVerificationsByOK(ctx, args.MigrationID)
			return mustJSON(map[string]any{
				"items":  rows,
				"ok":     counts.OkCount,
				"failed": counts.FailCount,
			}), nil
		},
	})

	// ----------------------------------------------------------------
	// Missing URLs (the 404 inbox)
	// ----------------------------------------------------------------

	register(Tool{
		Name:        "list_missing_urls",
		Description: "Top-N 404 paths captured in real-time by the analytics nginx-log parser. Each row: id, path, hit_count, first_seen, last_seen, referer (best-effort first-hit referer). Ordered by hit_count desc so the highest-leverage redirects to create surface first. Default limit 50; max 500. The 404 inbox is the migration system's highest-leverage feature - no other CMS captures + offers one-click conversion to a 301.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"limit":{"type":"integer","minimum":1,"maximum":500}
			}
		}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				Limit int64 `json:"limit"`
			}
			_ = json.Unmarshal(raw, &args)
			if args.Limit <= 0 || args.Limit > 500 {
				args.Limit = 50
			}
			rows, err := s.queries.ListMissingURLsBySite(ctx, store.ListMissingURLsBySiteParams{
				SiteID: agent.SiteID,
				Limit:  args.Limit,
			})
			if err != nil {
				return "", err
			}
			return mustJSON(rows), nil
		},
	})

	register(Tool{
		Name:        "create_redirect_from_missing",
		Description: "One-click conversion of a missing-URL 404 into a 301/302/307/308/410 redirect. Given the missing URL's id and a to_path, creates the redirect row + clears the missing-url entry atomically. Validates: to_path must start with / (off-site rejected), no // (open-redirect vector), no nginx-token-unsafe characters ({,},;,\",\\,'), no-op (from==to) rejected, status_code must be one of 301|302|307|308|410. Idempotent: if a redirect already exists at the from_path, the missing-url row is cleared without overwriting the existing redirect.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"missing_id":{"type":"string"},
				"to_path":{"type":"string"},
				"status_code":{"type":"integer","enum":[301,302,307,308,410]}
			},
			"required":["missing_id","to_path"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				MissingID  string `json:"missing_id"`
				ToPath     string `json:"to_path"`
				StatusCode int64  `json:"status_code"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			row, err := s.queries.GetMissingURLByID(ctx, args.MissingID)
			if err != nil || row.SiteID != agent.SiteID {
				return "", errors.New("missing URL not found")
			}
			to := strings.TrimSpace(args.ToPath)
			if to == "" {
				return "", errors.New("to_path is required")
			}
			statusCode := args.StatusCode
			if statusCode == 0 {
				statusCode = 301
			}
			if !validRedirectStatusForMCP(statusCode) {
				return "", errors.New("status_code must be one of 301, 302, 307, 308, 410")
			}
			if !strings.HasPrefix(to, "/") {
				return "", errors.New("to_path must start with / (off-site redirects not supported)")
			}
			if strings.HasPrefix(to, "//") {
				return "", errors.New("to_path cannot start with // (open-redirect vector)")
			}
			if !pathTokenSafeForMCP(to) {
				return "", errors.New("to_path contains forbidden characters")
			}
			if to == row.Path {
				return "", errors.New("to_path equals from_path; no-op redirect rejected")
			}
			// Idempotency: if a redirect already covers this from_path,
			// clear the missing-url row and report.
			if existing, err := s.queries.GetRedirectByPath(ctx, store.GetRedirectByPathParams{
				SiteID: agent.SiteID, FromPath: row.Path,
			}); err == nil && existing.ID != "" {
				_ = s.queries.DeleteMissingURL(ctx, args.MissingID)
				return mustJSON(map[string]any{
					"status":      "already_existed",
					"redirect_id": existing.ID,
					"from":        existing.FromPath,
					"to":          existing.ToPath,
				}), nil
			}
			rid := newID()
			if err := s.queries.CreateRedirect(ctx, store.CreateRedirectParams{
				ID: rid, SiteID: agent.SiteID, FromPath: row.Path, ToPath: to,
				StatusCode: statusCode, IsAuto: 0,
			}); err != nil {
				return "", err
			}
			_ = s.queries.DeleteMissingURL(ctx, args.MissingID)
			return mustJSON(map[string]any{
				"status":      "created",
				"redirect_id": rid,
				"from":        row.Path,
				"to":          to,
				"status_code": statusCode,
			}), nil
		},
	})

	register(Tool{
		Name:        "dismiss_missing_url",
		Description: "Removes a missing-URL row without creating a redirect. Useful for known-junk paths (bot scans, accidental hits) the operator never wants surfaced again. The row will REAPPEAR if the path 404s again - to make a dismissal stick permanently, create a 410 redirect via create_redirect_from_missing.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"missing_id":{"type":"string"}
			},
			"required":["missing_id"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				MissingID string `json:"missing_id"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			row, err := s.queries.GetMissingURLByID(ctx, args.MissingID)
			if err != nil || row.SiteID != agent.SiteID {
				return "", errors.New("missing URL not found")
			}
			if err := s.queries.DeleteMissingURL(ctx, args.MissingID); err != nil {
				return "", err
			}
			return mustJSON(map[string]any{"status": "dismissed"}), nil
		},
	})
}

// migrationSummaryForMCP is the small-row shape list_migrations returns.
// Drops the manifest_json blob so the listing endpoint stays under a few KB.
func migrationSummaryForMCP(m store.Migration) map[string]any {
	stats := map[string]any{}
	var manifest migration.MigrationManifest
	if json.Unmarshal([]byte(m.ManifestJson), &manifest) == nil {
		stats = map[string]any{
			"pages":       manifest.Stats.PagesFound,
			"collections": manifest.Stats.CollectionsFound,
			"media":       manifest.Stats.MediaFound,
		}
	}
	return map[string]any{
		"id":          m.ID,
		"source_url":  m.SourceUrl,
		"source_type": m.SourceType,
		"status":      m.Status,
		"error":       m.Error,
		"created_at":  m.CreatedAt,
		"updated_at":  m.UpdatedAt,
		"stats":       stats,
	}
}

// runMCPImporter dispatches to the right migration package client. Mirrors
// MigrationHandler.runImporter but lives here so the MCP tool doesn't
// depend on the handlers package.
func runMCPImporter(ctx context.Context, sourceType, sourceURL, wpAuth, webflowSiteID, webflowAuthToken, ghostKey string) (*migration.MigrationManifest, error) {
	switch strings.ToLower(sourceType) {
	case "sitemap", "":
		return migration.CrawlSitemap(ctx, sourceURL, migration.CrawlOptions{})
	case "wordpress":
		c := migration.NewWordPressClient(sourceURL, migration.WordPressOptions{
			AuthHeader: wpAuth,
		})
		return c.Crawl(ctx, migration.WordPressOptions{})
	case "webflow":
		c, err := migration.NewWebflowClient(migration.WebflowOptions{
			SiteID:    webflowSiteID,
			AuthToken: webflowAuthToken,
		})
		if err != nil {
			return nil, err
		}
		return c.Crawl(ctx, migration.WebflowOptions{})
	case "ghost":
		c, err := migration.NewGhostClient(migration.GhostOptions{
			BaseURL:    sourceURL,
			ContentKey: ghostKey,
		})
		if err != nil {
			return nil, err
		}
		return c.Crawl(ctx, migration.GhostOptions{})
	default:
		return nil, errors.New("unknown source_type; want sitemap, wordpress, webflow, or ghost")
	}
}

// validRedirectStatusForMCP mirrors handlers.validRedirectStatusCodes.
// Inlined here to avoid a cross-package import for a 5-element check.
func validRedirectStatusForMCP(c int64) bool {
	return c == 301 || c == 302 || c == 307 || c == 308 || c == 410
}

// pathTokenSafeForMCP mirrors handlers.pathTokenSafeForHandler. Same
// rejection rules: no nginx-config-breaking chars, no control bytes, no
// non-printable runes.
func pathTokenSafeForMCP(s string) bool {
	for _, r := range s {
		if r == '{' || r == '}' || r == ';' || r == '"' || r == '\\' || r == '\'' {
			return false
		}
		if r < 0x20 || r == 0x7f {
			return false
		}
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// noopMCPMediaUploader is the fallback porter media path when MCP is
// constructed without a media uploader (test fixtures). Mirrors
// handlers.NoopMediaUploader so the porter records each image as a
// warning instead of failing the whole apply.
type noopMCPMediaUploader struct{}

func (noopMCPMediaUploader) UploadFromURL(ctx context.Context, siteID, sourceURL, alt, caption string) (string, string, error) {
	return "", "", errors.New("media uploader not configured for MCP")
}
