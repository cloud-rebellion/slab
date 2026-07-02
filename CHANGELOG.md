# Changelog

All notable changes to Atomic Site land here. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the
project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- **Admin write rate limiter** (`AdminWriteRateLimit` middleware,
  `internal/middleware/admin_writes.go`). Per-(user, peer-IP) token
  bucket: 60 writes/min sustained, 20-burst, returns 429 with
  `Retry-After`. Mounts after auth on the admin route group; reads,
  HEAD, and OPTIONS bypass. Closes the gap between the login limiter
  (which only catches credential brute force) and the actual write
  surface a logged-in attacker or compromised session would target.
- **RSS / Atom feeds for collections.** Each Collection whose
  `schema_org_type` is article-like (BlogPosting, NewsArticle,
  Article, etc.) auto-emits `public/{collection.slug}/feed.xml` at
  build time. Explicit `feed_enabled` / `feed_title` /
  `feed_description` / `feed_max_entries` overrides via the existing
  `settings_json`. Atom 1.0 (RFC 4287) over RSS 2.0 for the spec
  rigor; encoding/xml escapes hostile input. Drafts excluded;
  ordered by `published_at` DESC; capped at 50 entries by default.
- **Audit log retention sweep.** `audit_log` rows older than the
  configured window (`ATOMICSITE_AUDIT_LOG_RETENTION_DAYS`, default
  365 days, clamped to [7, 3650]) drop in the existing daily
  retention manager. Counts surface in the per-sweep `SweepResult`
  exposed via `/api/admin/metrics`. Closes the
  log-grows-forever gap on the GDPR / ISO 27001 proof-of-controls
  table.
- **MFA enrollment policy.** New `ATOMICSITE_REQUIRE_MFA` env var:
  `""` (optional, default), `"admin"` (admins must enroll),
  `"all"` (every user must enroll). Login still issues a session
  cookie so the user can complete enrollment; the response carries
  `enroll_required: true`. The new `EnforceMFAEnrollment`
  middleware blocks state-changing routes for non-enrolled users
  with 403 + `error_code=mfa_enroll_required`, except the TOTP
  setup, change-password, and logout paths so the user can finish
  enrollment without getting stuck.
- **ZIP export of built site.** `GET
  /api/sites/{siteID}/dist.zip[?build_id=…]`. Streams the latest
  successful deployment's `dist/` tree as a ZIP archive. Symlink-
  aware path-traversal guard rejects entries that escape the dist
  root. Filename embeds the site slug, the deployment timestamp,
  and a 12-char prefix of the build ID. SiteAccess-gated;
  `build_id` is cross-tenant guarded so a guess from another site
  returns 404.
- New tests: `admin_writes_test.go` (8 cases), `feeds_test.go` (8
  cases), `audit_log_test.go` retention (4 cases),
  `mfa_enforce_test.go` (10 cases), `build_export_test.go` (8
  cases).
- New env vars documented in `.env.example`:
  `ATOMICSITE_TRUSTED_PROXIES`, `ATOMICSITE_REQUIRE_MFA`,
  `ATOMICSITE_AUDIT_LOG_RETENTION_DAYS`.

### Changed
- **OSS readiness sweep.** Removed every hardcoded reference to the
  internal SaaS host (`slab.example.com`) from
  production code. The custom-domain edge writer, the route reservation
  guard, and the screenshot tool's SSRF allow-list are now driven by
  `ATOMICSITE_PRIMARY_DOMAIN` + `BUILT_SITE_SUFFIX`. Loopback hosts
  always pass the screenshot allow-list so `make dev` works without any
  env vars.
- **Bcrypt cost standardised to 12.** `seedAdmin`, `resetPassword` CLI,
  `ChangePassword`, invite redemption, password reset, and TOTP
  recovery-code hashing all share `handlers.PasswordBcryptCost = 12`.
  Existing hashes verify normally; subsequent password changes upgrade
  to the new cost transparently.
- **TOTP recovery codes raised from 32 to 128 bits of entropy.**
  `recoveryCodeBytes` went from 4 to 16, so each generated code is now
  32 hex chars. Older 8-char codes still verify because
  `bcrypt.CompareHashAndPassword` is length-agnostic; only newly minted
  enrollments use the longer format.
- **Custom trusted-proxy `RealIP` middleware** replaces
  `chi.middleware.RealIP`. `X-Forwarded-For` and `X-Real-IP` are honored
  only when the immediate TCP peer is in
  `ATOMICSITE_TRUSTED_PROXIES`. With the env var unset, the headers are
  ignored entirely so audit logs and rate limiters see the actual peer.
  This closes the audit-log spoofing vector when atomicsite is exposed
  directly to the internet.

### Fixed
- **Built sites now serve /favicon.ico and /apple-touch-icon.png.**
  New build step `CopyBrandIcons` (`internal/builder/media.go`, wired
  in `renderer.go` after `CopyMedia`) copies the branding favicon media
  (`sites.favicon_id`) into the workspace `public/` root as both files,
  so Astro carries them into `dist/`. The layout has always linked both
  paths unconditionally on every page, but nothing emitted the files,
  so every built site 404'd on them (broken tab icon, no iOS shortcut
  icon, failed release hard checks). Degenerate states (no favicon_id,
  missing media row, missing file on disk) skip with a warning instead
  of failing the build, matching the previous degrade-silently
  behavior.
- **verify-live worker honors cancellation.** In-flight progress writes
  (`UpdateVerifyJobProgress`, `CreateMigrationVerification`) now use the
  worker's cancellable context instead of `context.Background()`, so
  clicking Cancel actually halts further DB writes. The terminal
  `FinishVerifyJob` write stays on a fresh background context with a
  10s timeout so the cancelled job's terminal state row still lands.
- **Frontend type errors zeroed.** Resolved 16 svelte-check errors and
  2 warnings: dead `onValueChange` prop on the Bits-UI `Select` (member
  role-change was non-functional), `SubmitEvent`/`MouseEvent` mismatch
  on the Create-Invite button, possibly-undefined accesses on the
  per-tenant quotas page, and the `signup/[token]` route param type
  drift. `bunx svelte-check --tsconfig ./tsconfig.json` reports a clean
  4650-file run.
- **Audit-log IP spoofing closed.** The two `auditClientIP`
  helpers (handlers + workspace middleware) used to read
  `X-Forwarded-For` / `CF-Connecting-IP` directly, bypassing the
  trusted-proxy gate added earlier in this release. Both now read
  only `r.RemoteAddr`, which `TrustedProxyRealIP` canonicalizes at
  the top of the request stack. Untrusted peers can no longer
  inject the audit log's `actor_ip` column.

### Tests
- Earlier in this release window: `screenshot_test.go` (7 cases
  locking the configurable SSRF allow-list), `realip_test.go` (9
  cases covering trusted-proxy XFF resolution, IPv6 round-trip,
  and untrusted-peer spoof prevention).

## [0.1.0] - 2026-05-08

First public release as Apache 2.0 open core.

### What's in this release

- **Single-binary admin server** (Go + chi + sqlc + SQLite) with an
  embedded SvelteKit SPA, Astro-based site builder, and `dist/` static
  output suitable for any host (VPS, Vercel, Netlify, R2, Cloudflare
  Pages, Dockyard, Kubernetes).
- **Agent-first content authoring**: HTTP + MCP surfaces with 78 tools
  spanning settings, blocks, pages, collections, knowledgebase,
  branding, builds, screenshots, and migrations.
- **CMS migration system** for sitemap, WordPress, Webflow, and Ghost
  sources, with URL planner, redirect porter, async verify-live worker
  (up to 25k URLs), 404 inbox, and one-click 301/302/307/308/410.
- **Conversion goals + identify** for first-party analytics. Per-site
  goals match by URL pattern, event name, or `form_submit`. Form
  submissions auto-identify the visitor when an email field is
  detected.
- **DuckDB-backed analytics** (read-only ATTACH on the live SQLite
  file) with retention sweeps that respect per-site
  `general.{analytics,consent,engagement}_retention_days` settings.
- **Embedded CookieProof widget** so consent gating works without a
  separate service.
- **Per-tenant quotas + workspace RBAC** (sites, pages, redirects,
  storage bytes, build minutes; owner/admin/editor roles).
- **Custom-domain reconciler** (nginx fragments + certbot HTTP-01 +
  optional Cloudflare zone helper) with verify probes and an
  operator-facing admin UI.
- **Admin SPA** covering content (pages, blocks, collections,
  knowledgebase, media), site (branding, fonts, settings, deploy
  targets, domains, allowed scripts), workflow (migrations, agent
  surface, MCP), and account (members, invites, MFA, billing for the
  EE / Cloud build).
- **A+ security defaults**: HttpOnly + Secure + SameSite=Strict
  session cookies, headers fragment for nginx, refuse-to-start on weak
  secrets in non-localhost deploys, SSRF guards in every fetch path,
  shield-tokenizer LLM boundary, audit log on every cross-workspace
  bypass.
- **Apache 2.0 open core**: cloud-only multi-tenant edge code lives
  behind the `ee` build tag.
