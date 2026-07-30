// Package server sets up the chi router and wires all handlers.
package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/bright-interaction/slab/ee"
	"github.com/bright-interaction/slab/internal/analyticsdb"
	"github.com/bright-interaction/slab/internal/config"
	"github.com/bright-interaction/slab/internal/domains"
	"github.com/bright-interaction/slab/internal/email"
	"github.com/bright-interaction/slab/internal/handlers"
	"github.com/bright-interaction/slab/internal/mcp"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/migration"
	"github.com/bright-interaction/slab/internal/retention"
	"github.com/bright-interaction/slab/internal/revisions"
	"github.com/bright-interaction/slab/internal/shield"
	"github.com/bright-interaction/slab/internal/storage"
	"github.com/bright-interaction/slab/internal/store"
)

// FrontendFS is the embedded frontend dist. Populated by cmd/server/main.go.
var FrontendFS fs.FS

// Server holds all dependencies.
type Server struct {
	cfg           *config.Config
	db            *sql.DB
	queries       *store.Queries
	storage       storage.Store
	authMW        *authmw.AuthMiddleware
	agentMW       *authmw.AgentAuthMiddleware
	adminWriteLim *authmw.AdminWriteLimiter
	// AnalyticsDB is the read-only DuckDB ATTACH on the SQLite file.
	// Optional: nil when DuckDB couldn't open at boot. Handlers that
	// depend on it return 503 in that case rather than crashing.
	AnalyticsDB *analyticsdb.Manager
	// RetentionMgr exposes the last sweep result via /api/admin/metrics
	// (audit I2). Optional; nil disables the retention metrics field.
	RetentionMgr *retention.Manager
	// OnAnalyticsSettingsChange, when set, is invoked after settings writes that
	// touch the analytics category so the analytics Manager can rescan parsers.
	OnAnalyticsSettingsChange func(context.Context)
	// DomainReconciler drives custom-hostname provisioning (DNS verify
	// → certbot → nginx vhost → live probe). Optional: nil keeps the
	// admin endpoints functional but no edge changes occur. Wired in
	// cmd/server/main.go after Server is constructed.
	DomainReconciler *domains.Reconciler
	// verifyJobMgr drives the async verify-live worker (Sprint 4 of the
	// migration system, 2026-05-06). Wired in cmd/server/main.go after
	// Server construction; nil makes VerifyLive return 503 so the
	// process is observably misconfigured rather than silently sync.
	verifyJobMgr *migration.JobManager
	// previewTokens mints single-use loopback tokens that back the
	// preview_screenshot MCP tool. Constructed in Routes(); the MCP
	// server gets a reference via WithPreviewTokenManager.
	previewTokens *handlers.PreviewTokenManager
	// clarificationsH backs the request_clarification + get_clarification
	// MCP tools and the dashboard inbox surface. Same handler instance
	// is reused on both REST + MCP boundaries so the validation lives
	// in one place.
	clarificationsH *handlers.ClarificationsHandler

	// revisionsH backs the version-history MCP tools + the
	// dashboard history drawer surface. Shared handler instance so
	// REST + MCP go through the same cross-tenant guards.
	revisionsH *handlers.RevisionsHandler

	// productsH + discountCodesH back the Store admin tab + the
	// catalog MCP tools (Sprint 2 slice A of the WP/Webflow roadmap).
	productsH      *handlers.ProductHandler
	discountCodesH *handlers.DiscountCodeHandler

	// ordersH backs the Orders admin tab + the order MCP tools +
	// the public Checkout + Mollie Webhook endpoints (Sprint 2
	// slice B of the WP/Webflow roadmap).
	ordersH *handlers.OrderHandler

	// localesH backs the Settings -> Locales admin page, the
	// per-locale page editor, and the translate_entity MCP tool
	// (Sprint 3 multilingual v1).
	localesH *handlers.LocalesHandler

	// appsH backs the Apps marketplace + per-site install ledger +
	// the list_apps_marketplace / list_installed_apps MCP tools
	// (Sprint 4 MCP-as-Apps platform slice A).
	appsH *handlers.AppsHandler
}

// SetVerifyJobManager wires the async verify-live worker. Called from
// cmd/server/main.go after the JobManager is constructed and Started.
func (s *Server) SetVerifyJobManager(m *migration.JobManager) {
	s.verifyJobMgr = m
}

// newAdminWriteLimiter builds the per-user write rate limiter from config.
// A capacity <= 0 returns nil, which AdminWriteRateLimit treats as a
// pass-through (the E2E harness sets ADMIN_WRITE_RATE_CAPACITY=0 because it
// bursts writes far past any production-sane bucket). Production keeps the
// 20 burst / 1 per second defaults.
func newAdminWriteLimiter(cfg *config.Config) *authmw.AdminWriteLimiter {
	if cfg.AdminWriteRateCapacity <= 0 {
		return nil
	}
	return authmw.NewAdminWriteLimiter(float64(cfg.AdminWriteRateCapacity), float64(cfg.AdminWriteRateRefill), 30*time.Minute)
}

// New creates a Server.
func New(cfg *config.Config, db *sql.DB, queries *store.Queries, st storage.Store) *Server {
	if cfg.BrightCRMWebhookURL != "" && cfg.BrightCRMWebhookSecret != "" {
		slog.Info("brightcrm integration enabled (outbound + /t/inbound verify)", "url", cfg.BrightCRMWebhookURL)
	} else {
		slog.Warn("brightcrm integration DISABLED (BRIGHTCRM_WEBHOOK_URL or BRIGHTCRM_WEBHOOK_SECRET unset)")
	}
	return &Server{
		cfg:           cfg,
		db:            db,
		queries:       queries,
		storage:       st,
		authMW:        authmw.NewAuthMiddleware(cfg, queries),
		agentMW:       authmw.NewAgentAuthMiddleware(queries),
		adminWriteLim: newAdminWriteLimiter(cfg),
	}
}

// Router builds and returns the chi router.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()

	// Shared MCP surface provider. Captured here at function-body scope
	// so both the admin route group (siteR.Get /agent-surface) and the
	// agent-key route group (which constructs the MCP server) can
	// reach it. Set inside the agent-key block after MCP construction;
	// the admin route reads via a wrapper closure so request order is
	// safe even though the admin route is registered first.
	var mcpSurfaceFn func() any

	r.Use(middleware.RequestID)
	// Trusted-proxy XFF middleware (replaces chi.middleware.RealIP).
	// Honours X-Forwarded-For / X-Real-IP only when the immediate TCP
	// peer is in the configured ATOMICSITE_TRUSTED_PROXIES list. Empty
	// list = ignore those headers entirely (the safe default for OSS
	// deploys directly exposed to the internet).
	r.Use(authmw.TrustedProxyRealIP(s.cfg.TrustedProxies))
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	// Capture panics to Flare, then re-panic for Recoverer's 500. Must sit
	// after Recoverer (inner) so it sees the panic first.
	r.Use(authmw.FlareRecoverer)
	r.Use(corsMiddleware(s.cfg))
	// Global body-size cap. Sets an upper bound on every request body
	// before any handler reads it, sized to cfg.MaxUploadSize so the
	// JSON-API surface (which never legitimately needs more than a
	// few MB) and the multipart-upload surface (which can hit the
	// configured ceiling) share the same limit. Without this, a
	// malicious client can POST gigabytes of JSON to /api/sites/...
	// and exhaust the goroutine's memory before json.Decode finishes.
	r.Use(jsonBodySizeMiddleware(s.cfg.MaxUploadSize))

	// Health (public). Audit M4: probes real component status instead of
	// returning a fixed `{status:ok}`. Kubernetes / Dockyard / external
	// monitors that hit this endpoint now see actionable green/yellow/red.
	//
	// Components probed (each is "ok" / "fail" / "unconfigured"):
	//   - sqlite: SELECT 1 against the OLTP DB (fails only when the file
	//     handle is broken; WAL contention surfaces here as slowness, not
	//     a false fail).
	//   - duckdb: only when AnalyticsDB is non-nil; `analyticsdb.Healthy`
	//     does a trivial query against the ATTACHed schema.
	//
	// The endpoint returns 200 when sqlite is ok regardless of duckdb so
	// load balancers don't take the whole service out for a degraded
	// analytics layer.
	// /api/mode (Phase 30.6, 2026-05-06). Public, unauthenticated.
	// Tells the admin SPA whether it's running against an OSS binary
	// or a Cloud (-tags ee) binary so cloud-only UI (Billing page,
	// WorkspaceSwitcher, plan upgrade prompts) can hide gracefully
	// in OSS rather than rendering 503-ing pages. Plus the deployment
	// mode env var so the frontend can branch on single vs cloud.
	r.Get("/api/mode", func(w http.ResponseWriter, req *http.Request) {
		mode := "single"
		if s.cfg != nil && s.cfg.DeploymentMode != "" {
			mode = s.cfg.DeploymentMode
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		body := `{"mode":"` + mode + `","ee":` + boolToStr(ee.IsAvailable()) + `}`
		_, _ = w.Write([]byte(body))
	})

	r.Get("/api/health", func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
		defer cancel()
		dbOK := func() bool {
			row := s.db.QueryRowContext(ctx, "SELECT 1")
			var n int
			return row.Scan(&n) == nil
		}()
		analyticsOK := "unconfigured"
		if s.AnalyticsDB != nil {
			if s.AnalyticsDB.Healthy(ctx) {
				analyticsOK = "ok"
			} else {
				analyticsOK = "fail"
			}
		}
		status := "ok"
		httpStatus := http.StatusOK
		if !dbOK {
			status = "fail"
			httpStatus = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(httpStatus)
		body := `{"status":"` + status +
			`","sqlite":"` + boolToOKFail(dbOK) +
			`","analyticsdb":"` + analyticsOK + `"}`
		_, _ = w.Write([]byte(body))
	})

	// Public analytics receiver (/t/*). Mounted high in the router so it
	// can't be shadowed by the SPA fallback. No auth: built sites POST
	// here cross-origin (same-origin if behind same hostname). Fingerprint
	// middleware runs only on this group so admin requests don't pick up
	// visitor cookies.
	trackH := handlers.NewTrackHandler(s.cfg, s.queries, s.db)
	r.Group(func(r chi.Router) {
		r.Use(authmw.FingerprintMiddleware(s.cfg))
		r.Post("/t/consent", trackH.Consent)
		r.Post("/t/pageview", trackH.PageView)
		r.Post("/t/engagement", trackH.Engagement)
		// /t/cwv (Sprint 5 quick-win, 2026-05-24): Core Web Vitals
		// metric beacon. The inline web-vitals script fires one POST
		// per observed metric per visit; the receiver de-dups via row
		// insert + last-write-wins aggregation on the dashboard side.
		r.Post("/t/cwv", trackH.CWV)
		// /t/event (Phase 31.1): explicit conversion-event ping fired
		// by window.atomic.track(name, props). Goal-eval matches the
		// posted name against active event_name goals.
		r.Post("/t/event", trackH.EventTrack)
		// /t/identify (Phase 31.3): explicit visitor-email
		// identification. Stamps email + identified_at on the
		// session keyed by the cookie-derived fingerprint and
		// fires a crmsync event so BrightCRM matches the freshly-
		// named contact to their behavioural history.
		r.Post("/t/identify", trackH.Identify)
		// Bidirectional CRM personalization (Phase 18.1).
		// /t/inbound accepts CRM-pushed metadata for a known visitor;
		// HMAC-SHA256 (same secret as outbound) on X-Atomicsite-Signature.
		r.Post("/t/inbound", trackH.Inbound)
		// /t/visitor (Phase 18.2): per-visitor metadata read endpoint for
		// the built-site hydration script. Re-derives fingerprint from
		// request headers; cross-origin friendly.
		r.Get("/t/visitor", trackH.Visitor)
	})

	// Admin reload endpoint. Dockyard's rotation engine POSTs new shared
	// secrets here with Authorization: Bearer <ADMIN_RELOAD_TOKEN>. The
	// handler does its own bearer check and hot-swaps the in-memory
	// HMAC verifier + signer without restarting the container.
	adminReloadH := handlers.NewAdminReloadHandler(s.cfg, trackH)
	r.Post("/admin/reload-secrets", adminReloadH.ReloadSecrets)

	// Public domain-verify endpoint. The host nginx atomicsite-acme.conf
	// block proxies /.well-known/atomic-verify/* here. No auth: the
	// token is the credential. We expose it BEFORE the auth middleware
	// group so a freshly-pointed custom domain can prove ownership
	// before TLS / login exists.
	publicDomH := handlers.NewDomainHandler(s.queries, nil, "")
	r.Get("/.well-known/atomic-verify/{token}", publicDomH.VerifyToken)

	// Quota handler is shared by build, media, and the dedicated
	// /api/sites/{id}/quota + /api/admin/sites/{id}/quota endpoints.
	// Sprint 3 (2026-05-04).
	quotaH := handlers.NewQuotaHandler(s.queries)

	// Auth (public)
	ah := handlers.NewAuthHandler(s.cfg, s.queries)
	r.Post("/api/auth/login", ah.Login)
	r.Post("/api/auth/logout", ah.Logout)
	// Forgot-password flow. All three endpoints unauthenticated by
	// design , the random token in the URL is the credential.
	r.Post("/api/auth/forgot-password", ah.ForgotPassword)
	r.Get("/api/auth/reset-password/{token}", ah.ResetPasswordInfo)
	r.Post("/api/auth/reset-password/{token}", ah.ResetPassword)

	// OIDC SSO (#5, 2026-05-10). All three endpoints are public: Config
	// just exposes whether SSO is enabled so the SPA can render the
	// "Sign in with SSO" button; Login redirects to the issuer; Callback
	// is the redirect-back target. State + PKCE verifier ride a single
	// HMAC-signed cookie, so no server-side session storage is needed.
	oidcH := handlers.NewOIDCHandler(s.cfg, s.queries)
	r.Get("/api/auth/oidc/config", oidcH.Config)
	r.Get("/auth/oidc/login", oidcH.Login)
	r.Get("/auth/oidc/callback", oidcH.Callback)
	// SSO second factor. Public like the rest of the login surface: the
	// credential is the short-lived HMAC-signed MFA-pending cookie the
	// callback set, plus a valid TOTP code. Handled by AuthHandler so it
	// reuses the password path's replay-guarded TOTP validation and rate
	// limiter rather than a second copy of either.
	r.Post("/api/auth/oidc/mfa", ah.CompleteOIDCMFA)

	// Sprint 3 (2026-05-04): public form-submission endpoint.
	// Cross-origin from `*.<BuiltSiteSuffix>` inherits the existing
	// CORS allowance for /t/* paths. Spam defense lives inside the
	// handler (honeypot + per-IP rate limit + per-form burst).
	//
	// Phase 31.3 (2026-05-06): wrapped with FingerprintMiddleware so
	// the auto-identify path inside Submit() can pull the visitor's
	// fingerprint from cookie + link a submitted email to the
	// existing visit_session. The shared idRecorder from trackH is
	// injected so every identify path -- explicit /t/identify and
	// auto-form -- runs the same DB update + crmsync dispatch.
	formsH := handlers.NewFormHandler(s.cfg, s.queries, ah.MailSender, trackH.IdentifyRecorder())
	r.With(authmw.FingerprintMiddleware(s.cfg)).Post("/t/forms/{formID}/submit", formsH.Submit)

	// TOTP MFA enrollment + verify + status + disable. All four
	// require an authenticated session, so they live behind the
	// auth middleware group below. This block reserves the route
	// names; the actual mounts happen inside r.Group(authMW.Middleware).

	// Public invite redemption (no auth - token in path is the credential).
	invH := handlers.NewInvitesHandler(s.cfg, s.queries)
	r.Get("/api/auth/signup/{token}", invH.PublicInfo)
	r.Post("/api/auth/signup/{token}", invH.Redeem)

	// Public workspace-invite redemption: the /cloud/signup/{token} URLs
	// CreateInvite hands out. Same token-in-path posture as above.
	wsSignupH := handlers.NewWorkspaceHandler(s.cfg, s.queries)
	r.Get("/api/cloud/signup/{token}", wsSignupH.SignupInfo)
	r.Post("/api/cloud/signup/{token}", wsSignupH.SignupRedeem)

	// Sites handler (used by both public and authenticated routes).
	sh := handlers.NewSiteHandler(s.cfg, s.queries, s.db)

	// Starter kit catalog (public): the onboarding wizard fetches this
	// before any site exists / before the admin is logged in.
	r.Get("/api/starter-kits", sh.ListStarterKits)

	// Authenticated admin routes
	r.Group(func(r chi.Router) {
		r.Use(s.authMW.Middleware)
		// Per-(user, peer-IP) write throttle. Applies only to
		// POST/PUT/PATCH/DELETE; reads are unaffected. Mounted right
		// after authMW so GetUser(r) is populated. Capacity 20,
		// refill 1/s = 60 writes/min sustained per (user, IP).
		r.Use(authmw.AdminWriteRateLimit(s.adminWriteLim))
		// MFA enrollment enforcement (feature #14). Pass-through when
		// ATOMICSITE_REQUIRE_MFA is empty; otherwise blocks writes
		// from users who haven't enrolled. Whitelists the TOTP setup
		// + change-password + logout flows so an unenrolled user can
		// still complete enrollment.
		r.Use(authmw.EnforceMFAEnrollment(s.cfg, s.queries))

		// Auth
		r.Get("/api/auth/me", ah.Me)
		r.Post("/api/auth/change-password", ah.ChangePassword)
		r.Post("/api/auth/sign-out-everywhere", ah.SignOutEverywhere)

		// TOTP MFA flow (auth required; the user's existing session
		// gates the enrollment / disable surface).
		r.Get("/api/auth/totp/status", ah.TOTPStatus)
		r.Post("/api/auth/totp/setup", ah.TOTPSetup)
		r.Post("/api/auth/totp/verify", ah.TOTPVerify)
		r.Post("/api/auth/totp/disable", ah.TOTPDisable)

		// GDPR Article 17 (erasure) + Article 20 (portability).
		// The export ZIPs every record we hold about the calling
		// user; the delete soft-flags the user for hard cascade
		// after a configurable cooling-off window (default 7 days)
		// committed by the daily retention sweep. Cancel undoes
		// the request before the window expires.
		gdprH := handlers.NewAccountGDPRHandler(s.queries)
		r.Get("/api/account/export", gdprH.Export)
		r.Post("/api/account/delete", gdprH.Delete)
		r.Post("/api/account/delete/cancel", gdprH.CancelDelete)

		// Account profile + workspace member management.
		memh := handlers.NewMembersHandler(s.cfg, s.queries)
		r.Patch("/api/auth/me", memh.UpdateProfile)
		r.Group(func(r chi.Router) {
			r.Use(authmw.RequireAdmin)
			// List returns the whole user directory (ListUsers has no tenant
			// predicate), so it must be admin-gated like the sibling mutations; it
			// was registered outside this group and leaked every user's email/role
			// to any authenticated non-admin.
			r.Get("/api/admin/members", memh.List)
			r.Patch("/api/admin/members/{userID}/role", memh.UpdateRole)
			r.Delete("/api/admin/members/{userID}", memh.Delete)
			r.Get("/api/admin/invites", invH.List)
			r.Post("/api/admin/invites", invH.Create)
			r.Delete("/api/admin/invites/{inviteID}", invH.Delete)

			// Waitlist admin routes (Phase 30.5). Wired here inside
			// the RequireAdmin group; the public submit endpoint
			// lives outside auth at /api/waitlist.
			waitlistAdminH := handlers.NewWaitlistHandler(s.cfg, s.queries, email.NewMailerSendClient(
				os.Getenv("MAILERSEND_API_TOKEN"),
				os.Getenv("MAIL_FROM"),
				os.Getenv("MAIL_FROM_NAME"),
			))
			r.Get("/api/admin/waitlist", waitlistAdminH.AdminList)
			r.Post("/api/admin/waitlist/{id}/invite", waitlistAdminH.AdminInvite)
			r.Delete("/api/admin/waitlist/{id}", waitlistAdminH.AdminDelete)

			// Admin observability surface (audit I1 + I2). Returns the
			// last retention sweep result, DB stats, and DuckDB
			// availability so the operator has one URL to check from
			// the browser when something feels off. Behind RequireAdmin
			// because it exposes per-site row counts.
			r.Get("/api/admin/metrics", func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				out := map[string]any{
					"sqlite_open_conns": s.db.Stats().OpenConnections,
					"sqlite_in_use":     s.db.Stats().InUse,
					"sqlite_idle":       s.db.Stats().Idle,
					"analyticsdb":       s.AnalyticsDB != nil,
				}
				if s.AnalyticsDB != nil {
					out["analyticsdb_healthy"] = s.AnalyticsDB.Healthy(req.Context())
				}
				if s.RetentionMgr != nil {
					out["retention_last"] = s.RetentionMgr.LastResult()
				}
				if v, err := s.queries.GetMaxSchemaVersion(req.Context()); err == nil {
					out["schema_version"] = v
				}
				if summary, err := quotaH.AdminQuotaSummary(req.Context()); err == nil {
					out["per_site_quotas"] = summary
				}
				_ = json.NewEncoder(w).Encode(out)
			})

			// Admin-only quota mutation. Site members can read their
			// own usage at /api/sites/{id}/quota; only admins can
			// raise or block-toggle.
			r.Patch("/api/admin/sites/{siteID}/quota", quotaH.PatchAdminQuota)

			// Audit log: workspace admin sees the global feed.
			auditH := handlers.NewAuditHandler(s.queries)
			r.Get("/api/admin/audit-log", auditH.GlobalFeed)
		})

		// Workspaces (Phase 30, Cloud Tier MVP, 2026-05-05). The
		// non-{workspaceID} list + create endpoints stay on plain `r`;
		// every /api/workspaces/{workspaceID}/* route gets the
		// WorkspaceAccessMiddleware so cross-workspace reads are
		// rejected before the handler runs. Owner-only mutations
		// (delete, billing, member changes) are gated inside each
		// handler via authmw.RequireWorkspaceRole.
		wsH := handlers.NewWorkspaceHandler(s.cfg, s.queries)
		r.Get("/api/workspaces", wsH.ListMine)
		r.Post("/api/workspaces", wsH.Create)
		workspaceAccessMW := authmw.WorkspaceAccessMiddleware(s.queries)
		wsR := r.With(workspaceAccessMW)
		wsR.Get("/api/workspaces/{workspaceID}", wsH.Get)
		wsR.Patch("/api/workspaces/{workspaceID}", wsH.Update)
		wsR.Delete("/api/workspaces/{workspaceID}", wsH.Delete)
		wsR.Get("/api/workspaces/{workspaceID}/members", wsH.ListMembers)
		wsR.Post("/api/workspaces/{workspaceID}/members", wsH.AddMember)
		wsR.Delete("/api/workspaces/{workspaceID}/members/{userID}", wsH.RemoveMember)
		wsR.Get("/api/workspaces/{workspaceID}/invites", wsH.ListInvites)
		wsR.Post("/api/workspaces/{workspaceID}/invites", wsH.CreateInvite)
		wsR.Delete("/api/workspaces/{workspaceID}/invites/{inviteID}", wsH.DeleteInvite)
		wsR.Get("/api/workspaces/{workspaceID}/sites", wsH.ListSites)

		// Billing (Phase 30.2). The handler is OSS-safe (returns 503
		// when Mollie not wired); the EE build's cmd/server/main.go
		// calls WireMollie() to attach the real client. The webhook
		// route lives outside the workspace group because Mollie
		// posts unauthenticated; verification is via re-fetching the
		// payment from the Mollie API (never trust the body).
		billingH := handlers.NewBillingHandler(s.cfg, s.queries)
		handlers.WireMollie(billingH, os.Getenv("MOLLIE_API_KEY"))
		wsR.Post("/api/workspaces/{workspaceID}/billing/checkout", billingH.StartCheckout)
		wsR.Get("/api/workspaces/{workspaceID}/billing", billingH.GetBilling)
		r.Post("/api/billing/webhook", billingH.Webhook)

		// Waitlist (Phase 30.5). Public capture is rate-limited at
		// the handler layer (5/hour/IP) so a public route is safe
		// without auth. Admin-only review + invite flow gated by
		// RequireAdmin.
		mailer := email.NewMailerSendClient(
			os.Getenv("MAILERSEND_API_TOKEN"),
			os.Getenv("MAIL_FROM"),
			os.Getenv("MAIL_FROM_NAME"),
		)
		waitlistH := handlers.NewWaitlistHandler(s.cfg, s.queries, mailer)
		r.Post("/api/waitlist", waitlistH.Submit)

		// Audit C1: every /api/sites/{siteID}/* route below must verify
		// the authenticated user has a site_members row for that siteID
		// (or is a workspace admin). The siteR sub-router applies
		// SiteAccessMiddleware to every route registered through it; the
		// non-siteID list/create/seed endpoints stay on plain `r`.
		siteAccessMW := authmw.SiteAccessMiddleware(s.queries)
		siteR := r.With(siteAccessMW)

		// Per-site audit feed: any member with site access can read.
		auditFeedH := handlers.NewAuditHandler(s.queries)
		siteR.Get("/api/sites/{siteID}/audit-log", auditFeedH.SiteFeed)

		// Sites
		r.Get("/api/sites", sh.List)
		r.Post("/api/sites", sh.Create)
		r.Post("/api/sites/seed", sh.Seed)
		siteR.Get("/api/sites/{siteID}", sh.Get)
		siteR.Patch("/api/sites/{siteID}", sh.Update)
		siteR.Delete("/api/sites/{siteID}", sh.Delete)
		siteR.Get("/api/sites/{siteID}/silos", sh.ListSilos)

		// Per-tenant quota usage (Sprint 3, 2026-05-04). Read-only for
		// site members; admin-only mutation lives at /api/admin/sites
		// further below so tenants can't raise their own ceilings.
		siteR.Get("/api/sites/{siteID}/quota", quotaH.GetQuota)

		// Form submissions admin surface (Sprint 3, 2026-05-04). The
		// public Submit endpoint is /t/forms/{id}/submit, registered
		// above without site-access gating. Admin list + CSV + delete
		// inherit the siteR cross-tenant guard.
		siteR.Get("/api/sites/{siteID}/forms", formsH.ListAdminForms)
		siteR.Get("/api/sites/{siteID}/forms/{formID}/submissions", formsH.ListAdminSubmissions)
		siteR.Get("/api/sites/{siteID}/forms/{formID}/submissions.csv", formsH.ExportAdminSubmissionsCSV)
		siteR.Delete("/api/sites/{siteID}/forms/{formID}/submissions/{submissionID}", formsH.DeleteAdminSubmission)

		// Conversion goals (Phase 31.1, 2026-05-06). Per-site CRUD +
		// aggregated rates feed the Analytics tab Goals panel.
		goalsH := handlers.NewConversionGoalsHandler(s.queries)
		siteR.Get("/api/sites/{siteID}/goals", goalsH.List)
		siteR.Post("/api/sites/{siteID}/goals", goalsH.Create)
		siteR.Patch("/api/sites/{siteID}/goals/{goalID}", goalsH.Update)
		siteR.Delete("/api/sites/{siteID}/goals/{goalID}", goalsH.Delete)
		siteR.Get("/api/sites/{siteID}/analytics/goals", goalsH.AnalyticsGoals)

		// Custom domains. The reconciler may be nil (no host integration);
		// the handler still records rows + serves the verify endpoint.
		var reconcileSignal func()
		if s.DomainReconciler != nil {
			reconcileSignal = s.DomainReconciler.Signal
		}
		// reservedSuffix prevents a custom hostname from shadowing the
		// wildcard tenant vhost the deployment already terminates. Empty
		// disables the check, which is the right behaviour for OSS
		// single-tenant deploys with no wildcard subdomain.
		// Sprint 4.7.4 (2026-05-09): expose edge IP + Cloudflare zone
		// list to the admin UI so the "Add an A record" instructions
		// show real values instead of "[your edge IP]" placeholder.
		// ATOMICSITE_CLOUDFLARE_ZONES is a comma-separated apex=zone_id
		// list (matches the format parseZoneMap in cmd/server/main.go uses).
		cfZoneApexes := []string{}
		for _, pair := range strings.Split(os.Getenv("ATOMICSITE_CLOUDFLARE_ZONES"), ",") {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			if eq := strings.Index(pair, "="); eq > 0 {
				cfZoneApexes = append(cfZoneApexes, strings.TrimSpace(pair[:eq]))
			}
		}
		domH := handlers.NewDomainHandler(s.queries, reconcileSignal, s.cfg.BuiltSiteSuffix).
			WithEdgeIP(s.cfg.EdgeIP).
			WithCloudflareZones(cfZoneApexes)
		siteR.Get("/api/sites/{siteID}/domains", domH.List)
		siteR.Get("/api/sites/{siteID}/domains/help", domH.Help)
		siteR.Post("/api/sites/{siteID}/domains", domH.Create)
		siteR.Post("/api/sites/{siteID}/domains/{domainID}/canonical", domH.SetCanonical)
		siteR.Post("/api/sites/{siteID}/domains/{domainID}/refresh", domH.Refresh)
		siteR.Delete("/api/sites/{siteID}/domains/{domainID}", domH.Delete)

		// Revisions recorder: writes entity_revisions snapshots before
		// every page/block mutation so the dashboard + MCP can restore
		// prior states. Sprint 1 of the WP/Webflow roadmap.
		recorder := revisions.New(s.queries)

		// Pages
		ph := handlers.NewPageHandler(s.cfg, s.queries)
		ph.SetRecorder(recorder)
		siteR.Get("/api/sites/{siteID}/pages", ph.List)
		siteR.Post("/api/sites/{siteID}/pages", ph.Create)
		siteR.Get("/api/sites/{siteID}/pages/{pageID}", ph.Get)
		siteR.Get("/api/sites/{siteID}/pages/{pageID}/preview", ph.PreviewSource)
		siteR.Get("/api/sites/{siteID}/pages/{pageID}/draft-preview", ph.PageDraftPreview)
		siteR.Patch("/api/sites/{siteID}/pages/{pageID}", ph.Update)
		siteR.Delete("/api/sites/{siteID}/pages/{pageID}", ph.Delete)
		siteR.Post("/api/sites/{siteID}/pages/bulk-delete", ph.BulkDelete)
		siteR.Post("/api/sites/{siteID}/pages/reorder", ph.Reorder)

		// Blocks
		bh := handlers.NewBlockHandler(s.cfg, s.queries, s.db)
		bh.SetRecorder(recorder)
		r.Get("/api/blocks/schemas", bh.Schemas)
		siteR.Get("/api/sites/{siteID}/pages/{pageID}/blocks", bh.List)
		siteR.Post("/api/sites/{siteID}/pages/{pageID}/blocks", bh.Create)
		siteR.Get("/api/sites/{siteID}/pages/{pageID}/blocks/{blockID}/preview", bh.Preview)
		siteR.Post("/api/sites/{siteID}/pages/{pageID}/blocks/{blockID}/preview-html", bh.PreviewHTML)
		siteR.Patch("/api/sites/{siteID}/pages/{pageID}/blocks/{blockID}", bh.Update)
		siteR.Delete("/api/sites/{siteID}/pages/{pageID}/blocks/{blockID}", bh.Delete)
		siteR.Post("/api/sites/{siteID}/pages/{pageID}/blocks/reorder", bh.Reorder)
		siteR.Put("/api/sites/{siteID}/pages/{pageID}/blocks/bulk", bh.BulkSave)

		// Global blocks (header / footer slots)
		gbh := handlers.NewGlobalBlockHandler(s.cfg, s.queries)
		siteR.Get("/api/sites/{siteID}/global-blocks", gbh.List)
		siteR.Post("/api/sites/{siteID}/global-blocks", gbh.Create)
		siteR.Get("/api/sites/{siteID}/global-blocks/{blockID}", gbh.Get)
		siteR.Patch("/api/sites/{siteID}/global-blocks/{blockID}", gbh.Update)
		siteR.Delete("/api/sites/{siteID}/global-blocks/{blockID}", gbh.Delete)
		// Render an arbitrary block payload to HTML for the admin UI's
		// "Rendered" tab. Operates on the supplied data so editors can
		// preview unsaved edits.
		siteR.Post("/api/sites/{siteID}/global-blocks/render", gbh.Render)

		// Knowledgebase
		kbh := handlers.NewKnowledgebaseHandler(s.cfg, s.queries)
		siteR.Get("/api/sites/{siteID}/knowledgebase", kbh.List)
		siteR.Post("/api/sites/{siteID}/knowledgebase", kbh.Create)
		siteR.Get("/api/sites/{siteID}/knowledgebase/{entryID}", kbh.Get)
		siteR.Patch("/api/sites/{siteID}/knowledgebase/{entryID}", kbh.Update)
		siteR.Delete("/api/sites/{siteID}/knowledgebase/{entryID}", kbh.Delete)

		// Custom Collections (Sprint 4, 2026-05-04). True CMS layer:
		// schema definition + items + bulk import. Items render as
		// pages or feed collection_list blocks. Schema validation,
		// cross-tenant guard, AuditLog all mirror the knowledgebase
		// pattern.
		colH := handlers.NewCollectionHandler(s.cfg, s.queries)
		siteR.Get("/api/sites/{siteID}/collections", colH.ListCollections)
		siteR.Post("/api/sites/{siteID}/collections", colH.CreateCollection)
		siteR.Get("/api/sites/{siteID}/collections/{collectionID}", colH.GetCollection)
		siteR.Patch("/api/sites/{siteID}/collections/{collectionID}", colH.UpdateCollection)
		siteR.Delete("/api/sites/{siteID}/collections/{collectionID}", colH.DeleteCollection)
		siteR.Get("/api/sites/{siteID}/collections/{collectionID}/items", colH.ListItems)
		siteR.Post("/api/sites/{siteID}/collections/{collectionID}/items", colH.CreateItem)
		siteR.Post("/api/sites/{siteID}/collections/{collectionID}/items/import", colH.BulkImport)
		siteR.Get("/api/sites/{siteID}/collections/{collectionID}/items/{itemID}", colH.GetItem)
		siteR.Patch("/api/sites/{siteID}/collections/{collectionID}/items/{itemID}", colH.UpdateItem)
		siteR.Delete("/api/sites/{siteID}/collections/{collectionID}/items/{itemID}", colH.DeleteItem)
		siteR.Post("/api/sites/{siteID}/collections/{collectionID}/items/bulk-delete", colH.BulkDeleteItems)

		// Redirects (Layer 2 of the migration system, 2026-05-05). CRUD +
		// CSV bulk import + pre-launch verify. The slug-edit auto-301 path
		// in agent.go writes directly to the same store; this surface is
		// what humans and the agent use for migrations and manual cleanup.
		// Builder emission is wired in internal/builder/nginx.go.
		redirH := handlers.NewRedirectHandler(s.cfg, s.queries)
		siteR.Get("/api/sites/{siteID}/redirects", redirH.List)
		siteR.Post("/api/sites/{siteID}/redirects", redirH.Create)
		siteR.Post("/api/sites/{siteID}/redirects/import", redirH.BulkImport)
		siteR.Post("/api/sites/{siteID}/redirects/verify", redirH.Verify)
		siteR.Get("/api/sites/{siteID}/redirects/{redirectID}", redirH.Get)
		siteR.Put("/api/sites/{siteID}/redirects/{redirectID}", redirH.Update)
		siteR.Delete("/api/sites/{siteID}/redirects/{redirectID}", redirH.Delete)

		// Migrations (Layer 3 of the migration system, 2026-05-06). Crawl
		// a source CMS (sitemap / WordPress / Webflow / Ghost), store the
		// normalised manifest, generate a URL plan, then apply: pages +
		// collection items + redirects + re-uploaded media all commit in
		// one call.
		//
		// Sprint 1 of the post-Layer-3d plan (2026-05-06): the migration
		// porter now hands off media to the production uploader, which
		// runs every imported image through the same SSRF-guarded fetch
		// + imaging pipeline that drives /api/sites/{id}/media/upload-
		// from-url. Result: migrated sites are fully self-hosted (no
		// hot-linking the source CDN) and inherit content-type + quota
		// guarantees automatically.
		migrationMediaH := handlers.NewMediaHandler(s.cfg, s.queries, s.storage, quotaH)
		migrH := handlers.NewMigrationHandler(s.cfg, s.queries, handlers.NewProductionMediaUploader(migrationMediaH))
		// Sprint 4 (2026-05-06): inject the process-wide async verify-job
		// manager constructed in cmd/server/main.go. Without it,
		// VerifyLive returns 503; production always wires it.
		if s.verifyJobMgr != nil {
			migrH.SetVerifyJobManager(s.verifyJobMgr)
		}
		siteR.Get("/api/sites/{siteID}/migrations", migrH.List)
		siteR.Post("/api/sites/{siteID}/migrations", migrH.Start)
		siteR.Get("/api/sites/{siteID}/migrations/{migrationID}", migrH.Get)
		siteR.Post("/api/sites/{siteID}/migrations/{migrationID}/plan", migrH.Plan)
		siteR.Post("/api/sites/{siteID}/migrations/{migrationID}/apply", migrH.Apply)
		// Sprint 2 (2026-05-06): pre-launch dry-run + post-launch crawl
		// + verification history. verify-coverage runs the same diff as
		// /redirects/verify but scoped to a migration's manifest so the
		// agent reasons about post-apply state. verify-live actually
		// crawls the deployed domain to assert each old URL one-hops to
		// 200, storing per-URL results in migration_verifications.
		siteR.Post("/api/sites/{siteID}/migrations/{migrationID}/verify-coverage", migrH.VerifyCoverage)
		siteR.Post("/api/sites/{siteID}/migrations/{migrationID}/verify-live", migrH.VerifyLive)
		siteR.Get("/api/sites/{siteID}/migrations/{migrationID}/verifications", migrH.ListVerifications)
		// Sprint 4 (2026-05-06): async verify-live job lifecycle. Run
		// status + cancel endpoints back the polling progress bar in
		// the migration UI.
		siteR.Get("/api/sites/{siteID}/migrations/{migrationID}/verify-jobs", migrH.ListVerifyJobs)
		siteR.Get("/api/sites/{siteID}/migrations/{migrationID}/verify-jobs/{jobID}", migrH.GetVerifyJob)
		siteR.Post("/api/sites/{siteID}/migrations/{migrationID}/verify-jobs/{jobID}/cancel", migrH.CancelVerifyJob)
		siteR.Delete("/api/sites/{siteID}/migrations/{migrationID}", migrH.Delete)

		// Missing URLs (Sprint 2 of the migration system, 2026-05-06).
		// 404s captured in real-time by the analytics nginx-log parser
		// surface here so the operator (or agent) can convert them into
		// 301/410 redirects with one click. The conversion creates a
		// redirect row + clears the missing-url entry atomically.
		missH := handlers.NewMissingURLsHandler(s.cfg, s.queries)
		siteR.Get("/api/sites/{siteID}/missing-urls", missH.List)
		siteR.Post("/api/sites/{siteID}/missing-urls/{missingID}/redirect", missH.CreateRedirect)
		siteR.Delete("/api/sites/{siteID}/missing-urls/{missingID}", missH.Dismiss)

		// Components
		ch := handlers.NewComponentHandler(s.cfg, s.queries)
		siteR.Get("/api/sites/{siteID}/components", ch.List)
		siteR.Post("/api/sites/{siteID}/components", ch.Create)
		siteR.Get("/api/sites/{siteID}/components/{componentID}", ch.Get)
		siteR.Patch("/api/sites/{siteID}/components/{componentID}", ch.Update)
		siteR.Delete("/api/sites/{siteID}/components/{componentID}", ch.Delete)

		// CSS Classes
		cch := handlers.NewCSSClassHandler(s.cfg, s.queries)
		siteR.Get("/api/sites/{siteID}/css-classes", cch.List)
		siteR.Post("/api/sites/{siteID}/css-classes", cch.Create)
		siteR.Get("/api/sites/{siteID}/css-classes/{classID}", cch.Get)
		siteR.Patch("/api/sites/{siteID}/css-classes/{classID}", cch.Update)
		siteR.Delete("/api/sites/{siteID}/css-classes/{classID}", cch.Delete)

		// Settings
		seth := handlers.NewSettingsHandler(s.cfg, s.queries)
		if s.OnAnalyticsSettingsChange != nil {
			seth.OnAnalyticsChange(s.OnAnalyticsSettingsChange)
		}
		siteR.Get("/api/sites/{siteID}/settings", seth.List)
		siteR.Get("/api/sites/{siteID}/settings/security/preview", seth.SecurityPreview)
		siteR.Get("/api/sites/{siteID}/settings/cookie-presets", seth.CookiePresets)
		siteR.Get("/api/sites/{siteID}/settings/{category}", seth.ListByCategory)
		siteR.Put("/api/sites/{siteID}/settings", seth.Upsert)
		siteR.Put("/api/sites/{siteID}/settings/bulk", seth.BulkUpsert)
		siteR.Delete("/api/sites/{siteID}/settings/{settingID}", seth.Delete)

		// Guardrails
		grh := handlers.NewGuardrailHandler(s.cfg, s.queries)
		siteR.Get("/api/sites/{siteID}/guardrails", grh.List)
		siteR.Post("/api/sites/{siteID}/guardrails", grh.Create)
		siteR.Get("/api/sites/{siteID}/guardrails/{ruleID}", grh.Get)
		siteR.Patch("/api/sites/{siteID}/guardrails/{ruleID}", grh.Update)
		siteR.Delete("/api/sites/{siteID}/guardrails/{ruleID}", grh.Delete)

		// Profile (business info, legal contacts)
		ph2 := handlers.NewProfileHandler(s.cfg, s.queries)
		siteR.Get("/api/sites/{siteID}/profile", ph2.Get)
		siteR.Put("/api/sites/{siteID}/profile", ph2.Upsert)

		// Allowed scripts (CSP whitelist)
		ash := handlers.NewAllowedScriptHandler(s.cfg, s.queries)
		siteR.Get("/api/sites/{siteID}/allowed-scripts", ash.List)
		siteR.Post("/api/sites/{siteID}/allowed-scripts", ash.Create)
		siteR.Patch("/api/sites/{siteID}/allowed-scripts/{scriptID}", ash.Update)
		siteR.Delete("/api/sites/{siteID}/allowed-scripts/{scriptID}", ash.Delete)

		// Media
		mh := handlers.NewMediaHandler(s.cfg, s.queries, s.storage, quotaH)
		siteR.Get("/api/sites/{siteID}/media", mh.List)
		siteR.Post("/api/sites/{siteID}/media", mh.Upload)
		siteR.Get("/api/sites/{siteID}/media/folders", mh.ListFolders)
		siteR.Post("/api/sites/{siteID}/media/folders", mh.CreateFolder)
		siteR.Delete("/api/sites/{siteID}/media/folders/{folderName}", mh.DeleteFolder)
		siteR.Get("/api/sites/{siteID}/media/{mediaID}", mh.Get)
		siteR.Patch("/api/sites/{siteID}/media/{mediaID}", mh.Update)
		siteR.Delete("/api/sites/{siteID}/media/{mediaID}", mh.Delete)

		// Outbound webhook subscriptions (#6). Site-scoped CRUD; the
		// DeliveryManager goroutine in cmd/server/main.go handles
		// the actual POSTs + retries. Listing the deliveries log
		// helps the operator debug a misbehaving receiver.
		webhookH := handlers.NewWebhookHandler(s.queries)
		siteR.Get("/api/sites/{siteID}/webhooks", webhookH.List)
		siteR.Post("/api/sites/{siteID}/webhooks", webhookH.Create)
		siteR.Get("/api/sites/{siteID}/webhooks/deliveries", webhookH.Deliveries)
		siteR.Get("/api/sites/{siteID}/webhooks/{webhookID}", webhookH.Get)
		siteR.Patch("/api/sites/{siteID}/webhooks/{webhookID}", webhookH.Update)
		siteR.Delete("/api/sites/{siteID}/webhooks/{webhookID}", webhookH.Delete)

		// Global content search (FTS5). Site-scoped: every workspace
		// member can query the search index for their own sites,
		// nothing else. Reindex is mounted under the same gate so
		// any member can trigger a rebuild after a bulk import.
		searchH := handlers.NewSearchHandler(s.db, s.queries)
		siteR.Get("/api/sites/{siteID}/search", searchH.Search)
		siteR.Post("/api/sites/{siteID}/search/reindex", searchH.Reindex)

		// Builds (admin)
		buildH := handlers.NewBuildHandler(s.cfg, s.queries, quotaH)
		siteR.Post("/api/sites/{siteID}/build", buildH.TriggerBuildAdmin)
		siteR.Get("/api/sites/{siteID}/builds/{buildID}/status", buildH.BuildStatusAdmin)
		siteR.Post("/api/sites/{siteID}/builds/{buildID}/cancel", buildH.CancelBuild)
		siteR.Post("/api/sites/{siteID}/deploy", buildH.Deploy)
		// Static export of the built dist/ tree as a ZIP. Members of
		// the site can download the latest successful build (or
		// pass build_id=...) so operators can archive snapshots or
		// hand the bundle to a static host.
		siteR.Get("/api/sites/{siteID}/dist.zip", buildH.ExportDistZip)

		// Deploy targets (admin)
		dh := handlers.NewDeployHandler(s.cfg, s.queries, s.db)
		siteR.Get("/api/sites/{siteID}/deploy-targets", dh.ListTargets)
		siteR.Post("/api/sites/{siteID}/deploy-targets", dh.CreateTarget)
		siteR.Get("/api/sites/{siteID}/deploy-targets/{targetID}", dh.GetTarget)
		siteR.Patch("/api/sites/{siteID}/deploy-targets/{targetID}", dh.UpdateTarget)
		siteR.Delete("/api/sites/{siteID}/deploy-targets/{targetID}", dh.DeleteTarget)
		siteR.Post("/api/sites/{siteID}/deploy-targets/{targetID}/default", dh.SetDefault)

		// Evaluations (admin)
		evalH := handlers.NewEvaluationHandler(s.cfg, s.queries)
		siteR.Get("/api/sites/{siteID}/evaluations", evalH.ListBySite)
		siteR.Get("/api/sites/{siteID}/evaluations/{buildID}", evalH.ListByBuild)

		// Analytics reads (admin) - visit_events populated by the nginx log tailer.
		anH := handlers.NewAnalyticsHandler(s.cfg, s.queries)
		siteR.Get("/api/sites/{siteID}/visit-events", anH.VisitEvents)
		siteR.Get("/api/sites/{siteID}/analytics/overview", anH.AnalyticsOverview)
		siteR.Get("/api/sites/{siteID}/analytics/sessions", anH.AnalyticsSessions)
		siteR.Get("/api/sites/{siteID}/analytics/conversion-paths", anH.AnalyticsConversionPaths)
		siteR.Get("/api/sites/{siteID}/analytics/tracked-fields", anH.AnalyticsTrackedFields)
		siteR.Get("/api/sites/{siteID}/analytics/cwv", anH.AnalyticsCWV)

		// Consent records (GDPR proof log). atomicsite became system of record
		// for tenant consent after the CookieProof fold-in (2026-04-30); these
		// endpoints back the dashboard's Cookies section.
		coH := handlers.NewConsentHandler(s.cfg, s.queries)
		siteR.Get("/api/sites/{siteID}/consent/records", coH.List)
		siteR.Get("/api/sites/{siteID}/consent/records/{recordID}", coH.Get)
		siteR.Get("/api/sites/{siteID}/consent/stats", coH.Stats)
		siteR.Get("/api/sites/{siteID}/consent/stats-by-category", coH.StatsByCategory)
		siteR.Get("/api/sites/{siteID}/consent/export.csv", coH.ExportCSV)
		siteR.Delete("/api/sites/{siteID}/consent/purge", coH.Purge)

		// Stitched cookie analytics , DuckDB ATTACH layer over the SQLite
		// file. Returns 503 from each endpoint when the DuckDB layer
		// failed to open at boot (e.g. a CGO-disabled build), so the
		// dashboard can fall back gracefully.
		// Cookie banner live preview: returns an HTML page with the real
		// CookieProof widget mounted in previewMode and the preferences
		// modal auto-opened. The admin SPA loads this as a same-origin
		// iframe on the Settings -> Cookies page so editors see the
		// exact production banner (cookie tables, language toggle,
		// privacy policy link, branding colors) instead of a mockup.
		cpH := handlers.NewCookiesPreviewHandler(s.cfg, s.queries)
		siteR.Get("/api/sites/{siteID}/cookies/preview", cpH.Render)

		caH := handlers.NewCookieAnalyticsHandler(s.cfg, s.queries, s.AnalyticsDB)
		siteR.Get("/api/sites/{siteID}/cookies/analytics/funnel", caH.Funnel)
		siteR.Get("/api/sites/{siteID}/cookies/analytics/pre-consent", caH.PreConsent)
		siteR.Get("/api/sites/{siteID}/cookies/analytics/time-to-consent", caH.TimeToConsent)
		siteR.Get("/api/sites/{siteID}/cookies/analytics/engaged-accept-rate", caH.EngagedAcceptRate)
		siteR.Get("/api/sites/{siteID}/cookies/analytics/top-accepting-pages", caH.TopAcceptingPages)
		siteR.Get("/api/sites/{siteID}/cookies/analytics/daily", caH.DailySplit)
		siteR.Get("/api/sites/{siteID}/cookies/analytics/journey/{fingerprint}", caH.VisitorJourney)

		// Agent keys (admin management)
		agh := handlers.NewAgentHandler(s.cfg, s.queries, s.db)
		siteR.Get("/api/sites/{siteID}/agent-keys", agh.ListAgentKeys)
		siteR.Post("/api/sites/{siteID}/agent-keys", agh.GenerateAgentKey)
		siteR.Delete("/api/sites/{siteID}/agent-keys/{keyID}", agh.RevokeAgentKey)

		// One-click agent bootstrap (Phase 14): generates a key + returns
		// a download-ready CLAUDE.md / .env / smoke-test bundle. Lets a
		// user wire an agent in one click instead of four manual steps.
		siteR.Post("/api/sites/{siteID}/agent-bootstrap", agh.AgentBootstrap)

		// MCP agent surface inventory for the admin Settings -> Agent
		// page. Read-only; payload is the live list of tools, resources,
		// prompts, and curriculum docs the agent has access to. Surface
		// function is wired below after MCP server construction; the
		// closure resolves at request time so this registration order
		// is correct.
		agh.SetSurfaceFn(func() any {
			if mcpSurfaceFn == nil {
				return nil
			}
			return mcpSurfaceFn()
		})
		siteR.Get("/api/sites/{siteID}/agent-surface", agh.AgentSurface)

		// Figma design-tokens import (admin)
		fh := handlers.NewFigmaHandler(s.cfg, s.queries)
		siteR.Post("/api/sites/{siteID}/figma/import", fh.ImportDesignTokens)

		// Self-hosted woff2 fonts (admin upload + delete; public serves Serve below)
		fonth := handlers.NewFontsHandler(s.cfg, s.queries)
		siteR.Get("/api/sites/{siteID}/fonts", fonth.List)
		siteR.Post("/api/sites/{siteID}/fonts", fonth.Upload)
		siteR.Delete("/api/sites/{siteID}/fonts/{fontID}", fonth.Delete)

		// GitHub design references (admin)
		drh := handlers.NewDesignReferencesHandler(s.cfg, s.queries)
		siteR.Get("/api/sites/{siteID}/design-references", drh.List)
		siteR.Post("/api/sites/{siteID}/design-references", drh.Create)
		siteR.Post("/api/sites/{siteID}/design-references/{refID}/refresh", drh.Refresh)
		siteR.Delete("/api/sites/{siteID}/design-references/{refID}", drh.Delete)

		// Clarifications: agent asks a question via MCP, human resolves
		// via the dashboard inbox. Closes the "agent guesses and ships
		// wrong" loop for ambiguous decisions.
		s.clarificationsH = handlers.NewClarificationsHandler(s.cfg, s.queries)
		siteR.Get("/api/sites/{siteID}/clarifications", s.clarificationsH.List)
		siteR.Post("/api/sites/{siteID}/clarifications", s.clarificationsH.Create)
		siteR.Get("/api/sites/{siteID}/clarifications/{id}", s.clarificationsH.Get)
		siteR.Post("/api/sites/{siteID}/clarifications/{id}/resolve", s.clarificationsH.Resolve)
		siteR.Delete("/api/sites/{siteID}/clarifications/{id}", s.clarificationsH.Cancel)

		// Revisions: per-entity history + restore (Sprint 1 of the
		// WP/Webflow replacement roadmap). Lists snapshots written by
		// the page/block update + delete paths, returns single
		// snapshot, restores onto live row.
		s.revisionsH = handlers.NewRevisionsHandler(s.cfg, s.queries, recorder)
		siteR.Get("/api/sites/{siteID}/revisions/{entityType}/{entityID}", s.revisionsH.List)
		siteR.Get("/api/sites/{siteID}/revisions/{entityType}/{entityID}/{version}", s.revisionsH.Get)
		siteR.Post("/api/sites/{siteID}/revisions/{entityType}/{entityID}/{version}/restore", s.revisionsH.Restore)

		// Store: products + variants + inventory + discount codes
		// (Sprint 2 slice A of the WP/Webflow replacement roadmap).
		// Orders + checkout land in slice B with the Mollie webhook.
		s.productsH = handlers.NewProductHandler(s.cfg, s.queries)
		siteR.Get("/api/sites/{siteID}/products", s.productsH.List)
		siteR.Post("/api/sites/{siteID}/products", s.productsH.Create)
		siteR.Get("/api/sites/{siteID}/products/{productID}", s.productsH.Get)
		siteR.Patch("/api/sites/{siteID}/products/{productID}", s.productsH.Update)
		siteR.Delete("/api/sites/{siteID}/products/{productID}", s.productsH.Delete)
		siteR.Get("/api/sites/{siteID}/products/{productID}/variants", s.productsH.ListVariants)
		siteR.Post("/api/sites/{siteID}/products/{productID}/variants", s.productsH.CreateVariant)
		siteR.Patch("/api/sites/{siteID}/variants/{variantID}", s.productsH.UpdateVariant)
		siteR.Delete("/api/sites/{siteID}/variants/{variantID}", s.productsH.DeleteVariant)
		siteR.Post("/api/sites/{siteID}/variants/{variantID}/inventory", s.productsH.SetInventory)
		siteR.Get("/api/sites/{siteID}/variants/{variantID}/inventory", s.productsH.ListInventoryAdjustments)

		s.discountCodesH = handlers.NewDiscountCodeHandler(s.cfg, s.queries)
		siteR.Get("/api/sites/{siteID}/discount-codes", s.discountCodesH.List)
		siteR.Post("/api/sites/{siteID}/discount-codes", s.discountCodesH.Create)
		siteR.Get("/api/sites/{siteID}/discount-codes/{codeID}", s.discountCodesH.Get)
		siteR.Patch("/api/sites/{siteID}/discount-codes/{codeID}", s.discountCodesH.Update)
		siteR.Delete("/api/sites/{siteID}/discount-codes/{codeID}", s.discountCodesH.Delete)

		// Orders admin (Sprint 2 slice B). List + Get + state machine
		// transition + refund. Mounted under siteR (session-auth).
		s.ordersH = handlers.NewOrderHandler(s.cfg, s.queries, s.db)
		siteR.Get("/api/sites/{siteID}/orders", s.ordersH.List)
		siteR.Get("/api/sites/{siteID}/orders/{orderID}", s.ordersH.Get)
		siteR.Post("/api/sites/{siteID}/orders/{orderID}/status", s.ordersH.UpdateStatus)
		siteR.Post("/api/sites/{siteID}/orders/{orderID}/refund", s.ordersH.Refund)

		// Apps marketplace + installs (Sprint 4 MCP-as-Apps platform
		// slice A, 2026-05-24). Marketplace listing is cross-tenant
		// (every site sees the same active apps), install + revoke
		// are per-site. Credentials are stored in credentials_json
		// plain at rest (matches the precedent for Mollie/GA4/Umami;
		// Sprint 1.5 hardening will route every tenant secret through
		// Shield encryption).
		appsH := handlers.NewAppsHandler(s.cfg, s.queries)
		r.Get("/api/apps", appsH.ListMarketplace)
		r.Get("/api/apps/{appID}", appsH.GetApp)
		siteR.Get("/api/sites/{siteID}/apps", appsH.ListSiteInstalls)
		siteR.Post("/api/sites/{siteID}/apps/{appID}", appsH.Install)
		siteR.Delete("/api/sites/{siteID}/apps/{appID}", appsH.Revoke)
		s.appsH = appsH

		// Locales (Sprint 3 multilingual v1, 2026-05-22). Three CRUD
		// surfaces: site_locales (the configured locale list),
		// page_locales (per-locale page metadata overrides),
		// block_locales (per-locale block content overrides). All
		// session-auth admin only; the build pipeline reads them
		// directly without going through the REST handler.
		s.localesH = handlers.NewLocalesHandler(s.cfg, s.queries)
		s.localesH.SetRecorder(recorder)
		siteR.Get("/api/sites/{siteID}/locales", s.localesH.ListSiteLocales)
		siteR.Post("/api/sites/{siteID}/locales", s.localesH.UpsertSiteLocale)
		siteR.Delete("/api/sites/{siteID}/locales/{locale}", s.localesH.DeleteSiteLocale)
		siteR.Get("/api/sites/{siteID}/pages/{pageID}/locales", s.localesH.ListPageLocales)
		siteR.Put("/api/sites/{siteID}/pages/{pageID}/locales/{locale}", s.localesH.UpsertPageLocale)
		siteR.Delete("/api/sites/{siteID}/pages/{pageID}/locales/{locale}", s.localesH.DeletePageLocale)
		siteR.Get("/api/sites/{siteID}/blocks/{blockID}/locales", s.localesH.ListBlockLocales)
		siteR.Put("/api/sites/{siteID}/blocks/{blockID}/locales/{locale}", s.localesH.UpsertBlockLocale)
		siteR.Delete("/api/sites/{siteID}/blocks/{blockID}/locales/{locale}", s.localesH.DeleteBlockLocale)
		siteR.Get("/api/sites/{siteID}/pages/{pageID}/block-locales", s.localesH.ListBlockLocalesByPage)
	})

	// Public font serving (no auth, long cache, CORS *).
	publicFontsH := handlers.NewFontsHandler(s.cfg, s.queries)
	r.Get("/atomicsite-fonts/{siteID}/{fontID}.woff2", publicFontsH.Serve)

	// Storefront public endpoints (Sprint 2 slice B). No session auth
	// because checkout is from an anonymous visitor browser; Mollie's
	// webhook is also unauthenticated against us. Cross-tenant safety
	// comes from the siteID URL param: the server uses that site's
	// Mollie API key to fetch the payment, so a foreign key cannot
	// drive state on a different tenant.
	if s.ordersH != nil {
		r.Post("/api/sites/{siteID}/checkout", s.ordersH.Checkout)
		r.Post("/api/sites/{siteID}/payments/mollie/webhook", s.ordersH.Webhook)
	}

	// Preview-token redemption (no auth, loopback-only, single-use). Used
	// by the preview_screenshot MCP tool: chromedp navigates to
	// /_preview/{token} on 127.0.0.1, the handler swaps the token for the
	// rendered draft HTML so chromedp can capture the visual.
	s.previewTokens = handlers.NewPreviewTokenManager(s.queries)
	r.Get("/_preview/{token}", s.previewTokens.Serve)

	// Agent API routes (API key auth)
	r.Group(func(r chi.Router) {
		r.Use(s.agentMW.Middleware)

		agentH := handlers.NewAgentHandler(s.cfg, s.queries, s.db)

		// Context
		r.Get("/api/agent/context", agentH.Context)

		// Bootstrap: re-fetch CLAUDE.md (text/markdown) any time. Lets an
		// agent re-read its own instructions on session start.
		r.Get("/api/agent/bootstrap", agentH.AgentSelfBootstrap)

		// Personalization debug (Phase 18.4): inspect what metadata the
		// CRM has pushed for a given visitor. Bound to the agent's site
		// via the agent identity.
		r.Get("/api/agent/visitor-metadata", agentH.GetVisitorMetadata)

		// Pages (slug via path or ?page= query param)
		r.Post("/api/agent/pages", agentH.CreatePage)
		r.Patch("/api/agent/pages/{slug}", agentH.UpdatePage)
		r.Delete("/api/agent/pages/{slug}", agentH.DeletePage)
		r.Get("/api/agent/pages/{slug}/draft-preview", agentH.DraftPreview)

		// Blocks (page slug via ?page= query param)
		r.Post("/api/agent/blocks", agentH.CreateBlock)
		r.Post("/api/agent/pages/{slug}/blocks", agentH.CreateBlock)
		r.Patch("/api/agent/blocks/{blockID}", agentH.UpdateBlock)
		r.Delete("/api/agent/blocks/{blockID}", agentH.DeleteBlock)

		// Components
		r.Post("/api/agent/components", agentH.CreateComponent)
		r.Patch("/api/agent/components/{name}", agentH.UpdateComponent)

		// CSS Classes
		r.Post("/api/agent/css-classes", agentH.CreateCSSClass)
		r.Patch("/api/agent/css-classes/{name}", agentH.UpdateCSSClass)

		// Global Blocks
		r.Put("/api/agent/global/{slot}", agentH.UpdateGlobalBlock)

		// Build
		agentBuildH := handlers.NewBuildHandler(s.cfg, s.queries, quotaH)
		r.Post("/api/agent/build", agentBuildH.TriggerBuild)
		r.Get("/api/agent/build/{buildID}/status", agentBuildH.BuildStatus)

		// Design critique. Reads the persisted "design" category
		// evaluations rows, derived from DesignPlaybook anti-patterns +
		// slop-term rules. buildID may be "latest" to fall back to the
		// site's most-recent successful build.
		agentCritiqueH := handlers.NewEvaluationHandler(s.cfg, s.queries)
		r.Get("/api/agent/critique/{buildID}", agentCritiqueH.AgentCritique)

		// Screenshot. Closes the visual-feedback loop: agent edits blocks,
		// triggers build, calls /api/agent/screenshot to get a base64 PNG of
		// the rendered page, and can iterate against pixels in the same turn.
		// SSRF-locked to the configured tenant subdomains and primary domain.
		screenshotH := handlers.NewScreenshotHandler()
		r.Post("/api/agent/screenshot", screenshotH.Screenshot)

		// MCP server (Model Context Protocol). One JSON-RPC endpoint
		// translating tools/call + resources/read into the same store
		// + builder logic the REST handlers use. Allow-list driven so
		// future REST endpoints can't accidentally leak PII through MCP.
		// Auth: same X-Agent-Key as the rest of /api/agent/*.
		// Construct a fresh media handler for the MCP migration uploader;
		// the siteR-scoped migrationMediaH from earlier in this file
		// isn't visible here. Both wrap the same s.queries / s.storage
		// so the underlying upload pipeline is identical.
		mcpMigrationMediaH := handlers.NewMediaHandler(s.cfg, s.queries, s.storage, handlers.NewQuotaHandler(s.queries))
		mcpServer := mcp.NewServer(s.queries, agentBuildH).
			WithFontsDir(s.cfg.FontsDir).
			WithBaseURL(s.cfg.BaseURL).
			WithDesignReferencesDir(s.cfg.DesignReferencesDir).
			WithVerifyJobManager(s.verifyJobMgr).
			WithMediaUploader(handlers.NewProductionMediaUploader(mcpMigrationMediaH)).
			WithPreviewTokens(s.previewTokens, s.cfg.Port).
			WithClarifications(s.clarificationsH).
			WithRevisions(s.revisionsH).
			WithProducts(s.productsH).
			WithDiscountCodes(s.discountCodesH).
			WithOrders(s.ordersH).
			WithLocales(s.localesH).
			WithApps(s.appsH)

		if len(s.cfg.ShieldKey) == 32 {
			level := shield.ParseHintLevel(s.cfg.ShieldHintLevel)
			// 24-hour TTL: real Claude Code / agent sessions span a full
			// working day. The previous 30-minute TTL surfaced a hard
			// "shield: session expired" MCP error after the smallest
			// coffee break, which killed iteration flow. Tokens are
			// per-session and the X-Agent-Key auth is the real boundary;
			// the TTL is just a housekeeping cap, not a security gate.
			//
			// Store backend: Redis when ATOMICSITE_SHIELD_REDIS_URL is set
			// (unlocks multi-node SaaS - every cluster member sees the same
			// session+token state). SQLStore fallback for single-node OSS
			// installs which still scale to dozens of concurrent MCP sessions
			// per box without distributed coordination.
			shieldStore, storeKind, err := buildShieldStore(s.cfg, s.db)
			if err != nil {
				slog.Error("atomicsite: shield store init failed; shield disabled", "err", err)
			} else {
				mcpServer.WithShield(shieldStore, []byte(s.cfg.ShieldKey), 24*time.Hour, level)
				slog.Info("atomicsite: shield enabled on MCP boundary",
					"key_id", "atomicsite", "ttl_hours", 24,
					"hint_level", s.cfg.ShieldHintLevel, "store", storeKind)
			}
		} else if s.cfg.ShieldKey != "" {
			slog.Warn("atomicsite: ATOMICSITE_SHIELD_KEY set but not 32 bytes; shield disabled",
				"len", len(s.cfg.ShieldKey))
		}
		// Wire the MCP shield-key updater into the admin reload handler
		// AFTER mcpServer exists. ReloadSecrets reads h.mcp at request
		// time (not at route registration), so this late binding is safe
		// and lets us keep the route registered earlier in the file with
		// the rest of the auth-bypass surface.
		adminReloadH.WithMCP(mcpServer)
		r.Mount("/mcp", mcpServer.Handler())

		// Surface inventory: set the shared closure so the admin
		// agent-surface route (registered earlier on agh) and the agent
		// chain (agentH) both resolve to the same live inventory. The
		// closure resolves the MCP server reference at request time, so
		// future runtime registry changes show up on the next admin
		// load without a restart.
		mcpSurfaceFn = func() any { return mcpServer.AgentSurface() }
		agentH.SetSurfaceFn(mcpSurfaceFn)

		// Evaluation
		r.Get("/api/agent/evaluation/{buildID}", agentH.GetEvaluation)

		// Branding (Phase 12.9): agents can read + patch the same fields the
		// human admin edits in the Branding UI. Writes require "write" cap.
		r.Get("/api/agent/branding", agentH.GetBranding)
		r.Patch("/api/agent/branding", agentH.UpdateBranding)

		// Profile (Phase 14): agents can read + patch the site_profiles row
		// (business name, address, contact emails) so they can fill the
		// Organization JSON-LD, security.txt, and legal pages on their own.
		r.Get("/api/agent/profile", agentH.GetProfile)
		r.Patch("/api/agent/profile", agentH.UpdateProfile)

		// Settings (Phase 14): read any category, write to seo / analytics /
		// general only. Security, allowed-scripts, nginx, danger stay
		// admin-only because they affect attack surface.
		r.Get("/api/agent/settings", agentH.ListSettings)
		r.Get("/api/agent/settings/{category}", agentH.ListSettingsByCategory)
		r.Patch("/api/agent/settings", agentH.BulkUpsertSettings)

		// Trusted external domains (CSP allowlist) + resolved security
		// headers preview. Read-only for the agent: writes here widen
		// the site's attack surface so they stay admin-only via the
		// /api/sites/{id}/allowed-scripts endpoint and the
		// /api/sites/{id}/settings/security UI.
		r.Get("/api/agent/allowed-scripts", agentH.ListAllowedScripts)
		r.Get("/api/agent/security/preview", agentH.SecurityPreview)

		// Self-hosted woff2 fonts (Phase 12.9 agent parity).
		r.Get("/api/agent/fonts", agentH.ListFonts)
		r.Post("/api/agent/fonts", agentH.UploadFont)
		r.Delete("/api/agent/fonts/{fontID}", agentH.DeleteFont)

		// GitHub design references (Phase 12.9 agent parity).
		r.Get("/api/agent/design-references", agentH.ListDesignReferences)
		r.Post("/api/agent/design-references", agentH.AddDesignReference)
		r.Delete("/api/agent/design-references/{refID}", agentH.DeleteDesignReference)

		// Media
		agentMediaH := handlers.NewMediaHandler(s.cfg, s.queries, s.storage, quotaH)
		r.Get("/api/agent/media", agentH.ListMedia)
		r.Get("/api/agent/media/folders", agentH.ListMediaFolders)
		r.Post("/api/agent/media/folders", agentH.CreateMediaFolder)
		r.Delete("/api/agent/media/folders/{folderName}", agentH.DeleteMediaFolder)
		r.Post("/api/agent/media", agentMediaH.AgentUpload)
		r.Post("/api/agent/media/from-url", agentMediaH.AgentUploadFromURL)
		r.Post("/api/agent/media/from-base64", agentMediaH.AgentUploadFromBase64)
		r.Patch("/api/agent/media/{mediaID}", agentMediaH.AgentUpdate)
		r.Delete("/api/agent/media/{mediaID}", agentMediaH.AgentDelete)
	})

	// Public media serving (no auth, long cache). Must come BEFORE mountFrontend.
	publicMediaH := handlers.NewMediaHandler(s.cfg, s.queries, s.storage, quotaH)
	r.Get("/media/*", publicMediaH.ServePublic)
	r.Head("/media/*", publicMediaH.ServePublic)

	// Serve embedded frontend (SPA)
	s.mountFrontend(r)

	return r
}

func (s *Server) mountFrontend(r chi.Router) {
	if FrontendFS == nil {
		return
	}
	indexHTML, err := fs.ReadFile(FrontendFS, "index.html")
	if err != nil {
		slog.Warn("frontend index.html missing", "error", err)
		return
	}
	startedAt := time.Now()

	serveIndex := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(indexHTML)
	}

	r.Get("/*", func(w http.ResponseWriter, req *http.Request) {
		path := strings.TrimPrefix(req.URL.Path, "/")
		if path == "" {
			serveIndex(w)
			return
		}
		// Reject path traversal.
		if strings.Contains(path, "..") {
			http.NotFound(w, req)
			return
		}
		f, err := FrontendFS.Open(path)
		if err != nil {
			// Unknown path: SPA client-side router takes over.
			serveIndex(w)
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || info.IsDir() {
			serveIndex(w)
			return
		}
		seeker, ok := f.(io.ReadSeeker)
		if !ok {
			data, err := io.ReadAll(f)
			if err != nil {
				http.Error(w, "read embed", http.StatusInternalServerError)
				return
			}
			seeker = bytes.NewReader(data)
		}
		if ct := mime.TypeByExtension(filepath.Ext(path)); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		if strings.HasPrefix(path, "_app/immutable/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		http.ServeContent(w, req, info.Name(), startedAt, seeker)
	})
}

// corsMiddleware returns a path-aware CORS handler. The audit's C2
// finding: the previous unified policy granted Access-Control-Allow-
// Credentials: true to every `*.<BuiltSiteSuffix>` origin on every
// route, including `/api/sites/...`. Combined with the missing
// per-site authorization (audit C1), a tenant who controls their own
// built-site origin could pivot a single line of injected JS into a
// cross-tenant takeover.
//
// Two policies now:
//
//  1. Admin routes (`/api/*` and the SPA fallback): credentialed CORS
//     allowed ONLY from `cfg.BaseURL` exact origin (and localhost in
//     dev). Built-site subdomains do not need to call admin routes;
//     all dashboard traffic is same-origin.
//
//  2. Public visitor endpoints (`/t/*`): credentialed CORS allowed
//     from `cfg.BaseURL` AND any host suffix-matching
//     `cfg.BuiltSiteSuffix`. Built sites legitimately need to reach
//     `/t/consent`, `/t/visitor`, `/t/inbound` cross-origin to lift
//     anonymous visits to identified.
//
// Preflight OPTIONS uses the same path-based policy so the browser
// reflects the correct Access-Control-Allow-Origin during the
// preflight handshake.
func corsMiddleware(cfg *config.Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && isAllowedOriginForPath(cfg, origin, r.URL.Path) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With, X-Agent-Key")
				// Vary: Origin keeps cache layers from serving the wrong
				// allow-origin to a different visitor.
				w.Header().Add("Vary", "Origin")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// isPublicVisitorPath reports whether the path is one of the public
// `/t/*` endpoints that built tenant sites legitimately call cross-origin.
func isPublicVisitorPath(p string) bool {
	return strings.HasPrefix(p, "/t/")
}

// isAllowedOriginForPath is the path-aware CORS decision. Admin routes
// only accept the configured BaseURL (plus localhost in dev). Public
// `/t/*` routes additionally accept any built-site subdomain.
func isAllowedOriginForPath(cfg *config.Config, origin, path string) bool {
	// Always allow the configured base URL exactly.
	if origin == cfg.BaseURL || strings.HasPrefix(origin, cfg.BaseURL+"/") {
		return true
	}
	// Local dev: any localhost origin (admin + built-site preview both
	// run on localhost during `go run`).
	if cfg.IsLocalDev() {
		if strings.HasPrefix(origin, "http://localhost") ||
			strings.HasPrefix(origin, "http://127.0.0.1") {
			return true
		}
	}
	// Built-site subdomains: ONLY for the public visitor endpoints.
	// Admin routes (`/api/*`, `/admin/*`, SPA) do not need this and
	// granting it widens the attack surface without benefit (C2).
	if isPublicVisitorPath(path) && cfg.BuiltSiteSuffix != "" {
		host := stripScheme(origin)
		if strings.HasSuffix(host, cfg.BuiltSiteSuffix) {
			return true
		}
	}
	return false
}

// boolToOKFail maps a healthcheck bool to the "ok" / "fail" string the
// /api/health JSON response uses.
func boolToOKFail(ok bool) string {
	if ok {
		return "ok"
	}
	return "fail"
}

// boolToStr formats a bool as a JSON literal "true" / "false" for the
// /api/mode response, which builds JSON via concat.
func boolToStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// stripScheme reduces an Origin header value (e.g. https://foo.example:443)
// to bare host for suffix comparison. Returns "" when the input is malformed.
func stripScheme(origin string) string {
	host := origin
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	return host
}

// jsonBodySizeMiddleware caps r.Body to maxBytes before any handler
// reads it. Without this, a malicious client can POST gigabytes of
// JSON and exhaust the goroutine's memory before json.Decoder hits a
// natural error. http.MaxBytesReader emits a clean 413-style failure
// at the decode site once the cap is exceeded.
//
// File-upload routes that need a higher cap call MaxBytesReader
// themselves with a larger value, which overrides the wrap below
// (the inner Read returns ErrBodyTooLarge once either limit hits).
//
// Small file like this lives next to the middleware it wraps for
// discoverability. If we add a third middleware we'll split into
// internal/server/middleware.go.
func jsonBodySizeMiddleware(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil && r.ContentLength != 0 {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}
