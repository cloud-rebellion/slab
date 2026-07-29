// Package config loads configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bright-interaction/slab/ee"
)

// Default sentinel values. The Validate() guard refuses to start in
// production-like deployments when any one of these is still in place.
const (
	DefaultJWTSecret     = "change-me-in-production"
	DefaultAnalyticsSalt = "atomicsite-default-fingerprint-salt-change-me"
	DefaultAdminPassword = "changeme123"
)

// DeploymentMode values. The OSS distribution defaults to "single";
// "cloud" requires a binary built with -tags ee.
const (
	DeploymentModeSingle = "single"
	DeploymentModeCloud  = "cloud"
)

// RequireMFA values. Read from ATOMICSITE_REQUIRE_MFA. Empty string is
// the default (users may enroll, not gated). "admin" forces every user
// with role=admin to enroll. "all" forces every authenticated user.
// Validate() rejects any other value at boot.
const (
	MFAOptional   = ""
	MFAAdminsOnly = "admin"
	MFAAllUsers   = "all"
)

// MustEnrollMFA reports whether a user with the given role must enroll
// in TOTP under the current RequireMFA policy. Pure function so the
// auth handler + the enforcement middleware share one source of truth.
func (c *Config) MustEnrollMFA(role string) bool {
	switch c.RequireMFA {
	case MFAAllUsers:
		return true
	case MFAAdminsOnly:
		return role == "admin"
	default:
		return false
	}
}

type Config struct {
	Port          int
	DataDir       string
	DBPath        string
	MediaDir      string
	FontsDir      string // self-hosted woff2 fonts uploaded per site
	JWTSecret     string
	BaseURL       string
	MaxUploadSize int64
	MediaVariants []int

	// Analytics. The CookieProof widget is now embedded directly into the
	// atomicsite Go binary (see internal/builder/widget_embed.go); the
	// remote-API admin token + base URL fields were removed 2026-04-30.
	TrackPath     string // public tracking endpoint prefix, default "/t"
	AnalyticsSalt string // server secret mixed into visitor fingerprints

	// BrightCRM analytics sync (Phase 10 C2). When either the URL or secret
	// is empty, crmsync becomes a no-op so dev environments don't crash.
	// CRMSyncMinInterval throttles non-identified events per (site, visitor).
	BrightCRMWebhookURL string
	// BrightCRMWebhookSecret is the current shared HMAC value, used both to
	// sign outbound /webhooks/atomicsite calls and to verify inbound /t/inbound
	// pings from BrightCRM. BrightCRMWebhookSecretPrevious is the previous
	// value, accepted by the inbound verifier during a rotation grace window
	// so requests in flight signed with the old key still verify. Outbound
	// signing always uses the current value. See internal/sharedsecret.
	BrightCRMWebhookSecret         string
	BrightCRMWebhookSecretPrevious string
	CRMSyncMinInterval             time.Duration

	// AdminReloadToken gates POST /admin/reload-secrets so Dockyard's rotation
	// engine can push a new pair without a container restart.
	AdminReloadToken string

	// AdminWriteRateCapacity + AdminWriteRateRefill tune the per-user write
	// rate limiter on POST/PUT/PATCH/DELETE (internal/middleware/admin_writes.go).
	// Defaults 20 burst / 1 per second. A capacity <= 0 disables the limiter
	// (pass-through); the E2E harness sets ADMIN_WRITE_RATE_CAPACITY=0 because
	// it bursts createSite/deleteSite far past any production-sane bucket.
	AdminWriteRateCapacity int
	AdminWriteRateRefill   int

	// PrimaryDomain is the apex domain this Atomic Site instance serves
	// (e.g. "example.com"). Empty for local dev or for deployments that
	// host many unrelated tenants. When set, it surfaces in the agent
	// context as the canonical "what domain am I building for?" answer
	// and seeds defaults for fresh sites (cookieproof_domain, default
	// canonical base, CORS allow-list). Read from the
	// ATOMICSITE_PRIMARY_DOMAIN environment variable.
	PrimaryDomain string

	// BuiltSiteSuffix is set ONLY for multi-tenant deployments where
	// each Atomic Site tenant gets a wildcard subdomain (e.g. every
	// tenant lives at <slug>.tenants.example.com). The CORS middleware
	// (server.go isAllowedOriginForPath) widens to accept any origin
	// whose hostname ends with this suffix on public visitor paths.
	//
	// Empty by default. The typical single-deployment-per-customer
	// shape leaves it unset; PrimaryDomain takes its place for any
	// deployment that serves a single root domain. Read from the
	// BUILT_SITE_SUFFIX environment variable.
	BuiltSiteSuffix string

	// DeploymentMode selects the operating shape: "single" (the OSS
	// default, one Atomic Site instance for one root domain) or
	// "cloud" (multi-tenant edge orchestration, requires a binary
	// built with -tags ee). Read from ATOMICSITE_DEPLOYMENT_MODE.
	// Validate() rejects unknown values and refuses to start in
	// "cloud" mode when ee.IsAvailable() returns false.
	DeploymentMode string

	// Custom-domain provisioning paths. The reconciler writes nginx
	// fragments + invokes certbot at these locations on the host.
	// Empty values disable the reconciler (local dev / OSS deploys
	// without root); the admin still records the rows but no edge
	// changes happen. When operating Atomic Site as a SaaS, point
	// these at the host nginx + certbot tree and run with sudoer
	// rights for the reload script.
	NginxConfDir       string // /etc/nginx/conf.d (auto-included by http{})
	NginxSitesDir      string // /etc/nginx/sites-enabled (vhost includes)
	AcmeWebrootDir     string // /var/www/acme, webroot for certbot HTTP-01
	CertbotPath        string // path to the certbot binary, or empty
	NginxReloadCommand string // shell command, e.g. "sudo /usr/local/bin/atomicsite-nginx-reload"
	EdgeIP             string // public A-record target shown to admins

	// DesignReferencesDir points at the bundled MIT-licensed reference
	// repos (astrowind, astro-paper, starlight, shadcn-svelte, shadcn-ui).
	// Used by the search_design_corpus MCP tool. Empty disables corpus
	// search; the tool returns a "not configured" error.
	// Read from DESIGN_REFERENCES_DIR.
	DesignReferencesDir string

	// ShieldKey is the 32-byte AES-256-GCM master key for the shield
	// LLM-boundary tokenizer. Read from ATOMICSITE_SHIELD_KEY. When empty
	// (no key set, or len != 32), shield is disabled and the MCP server
	// returns plaintext responses + accepts plaintext arguments. When
	// set, every PII field crossing the MCP boundary is tokenized.
	ShieldKey string

	// ShieldHintLevel dials how much value-derived metadata appears in
	// shield markers. Read from ATOMICSITE_SHIELD_HINT_LEVEL. Values:
	//
	//   ""         (default) -> HintFull: domain, initials, len, etc.
	//   "bucketed" -> HintBucketed: industry buckets, length buckets
	//   "minimal"  -> HintMinimal: only len_bucket
	//   "none"     -> HintNone: kind only, no value-derived metadata
	//
	// Combined with deterministic per-session token ids, "minimal" or
	// "none" lets the agent recognise the same entity across turns
	// without learning which entity it is.
	ShieldHintLevel string

	// ShieldRedisURL points the Shield store at a Redis instance for
	// multi-node deploys. Read from ATOMICSITE_SHIELD_REDIS_URL. When
	// empty, Shield falls back to the per-node SQLStore (single-process
	// SQLite). When set, every node in a cluster shares the same
	// shield_sessions + shield_tokens state, so an MCP session opened
	// on node A can resolve markers issued on node B.
	//
	// Format: redis://[:password@]host[:port][/db]. TLS via rediss://.
	// Optional shield:<prefix>: prepended via ShieldRedisPrefix below to
	// isolate tenants on a shared Redis.
	ShieldRedisURL string

	// ShieldRedisPrefix is prepended to every key when ShieldRedisURL is
	// set. Empty = "shield:" default. Set to "<tenant>:shield:" when
	// multiple atomicsite tenants share one Redis instance.
	ShieldRedisPrefix string

	// RequireMFA enforces TOTP enrollment policy. Read from
	// ATOMICSITE_REQUIRE_MFA. Values:
	//
	//   ""       (default) -> optional MFA; users may enroll but it's
	//                         not gated.
	//   "admin"           -> users with role=admin must enroll. Login
	//                         succeeds but the dashboard receives
	//                         enroll_required=true and the
	//                         enforcement middleware blocks writes
	//                         until the user enrolls (the TOTP setup
	//                         + verify endpoints stay accessible so
	//                         the user can complete enrollment).
	//   "all"             -> every authenticated user must enroll.
	//
	// Validate rejects unknown values so a typo can't silently
	// downgrade the policy.
	RequireMFA string

	// AuditLogRetentionDays caps how long rows stay in audit_log
	// before the daily retention sweep purges them. Read from
	// ATOMICSITE_AUDIT_LOG_RETENTION_DAYS. Zero (or unset) uses the
	// retention package's DefaultAuditLogDays (365). Clamped to
	// [MinRetentionDays, MaxRetentionDays] inside the manager.
	AuditLogRetentionDays int

	// LifecyclePauseDays / LifecycleDeleteDays drive the daily
	// workspace lifecycle sweep. When a workspace has no activity
	// (no site updates, no builds, no deployments) for N days, the
	// sweep flips status: active -> paused at LifecyclePauseDays,
	// any -> deleted at LifecycleDeleteDays. Both default to 0 =
	// disabled. Self-host operators almost never want this; cloud
	// installs set both via ATOMICSITE_LIFECYCLE_PAUSE_DAYS /
	// ATOMICSITE_LIFECYCLE_DELETE_DAYS. The 'deleted' transition
	// is a SOFT state change; rows stay in the DB until an operator-
	// initiated hard purge so a sweep mistake stays recoverable.
	LifecyclePauseDays  int
	LifecycleDeleteDays int

	// GDPRDeleteCoolingDays is the cooling-off window between a
	// user's POST /api/account/delete and the retention sweep's
	// hard cascade. 0 falls back to retention.DefaultGDPRDeleteCoolingDays
	// (7). Read from ATOMICSITE_GDPR_DELETE_COOLING_DAYS.
	GDPRDeleteCoolingDays int

	// TrustedProxies is the comma-separated list of CIDRs (or bare IPs,
	// auto-widened to /32 or /128) whose X-Forwarded-For / X-Real-IP
	// headers the server should honor. When empty, those headers are
	// ignored entirely and audit logs / rate limiters see r.RemoteAddr
	// (the actual TCP peer), which is the safe default for OSS deploys
	// directly exposed to the internet. When set, only proxy peers in
	// the list can rewrite the client IP via XFF: every other peer's
	// header is discarded.
	//
	// Read from ATOMICSITE_TRUSTED_PROXIES. Common values:
	//   - "127.0.0.1/32,::1/128" for a single-host nginx + atomicsite
	//   - "10.0.0.0/8" for a private VPC fronted by a managed LB
	//   - "172.16.0.0/12" for Docker's default bridge network
	TrustedProxies string

	// OIDC SSO (#5, 2026-05-10). Optional: when OIDCEnabled is false,
	// /auth/oidc/login + callback return 404 and the existing password
	// flow is the only path. It works against any OIDC-compliant issuer
	// (Zitadel, Authentik, Auth0, Keycloak); the same code
	// because we go through /.well-known/openid-configuration.
	//
	// OIDCAllowDomains is a comma-separated list of email domains that
	// are allowed to log in via SSO when no matching local user exists.
	// Empty = SSO requires a pre-existing user (shipping default).
	//
	// OIDCDefaultRole is the role given to an auto-provisioned SSO user.
	// This used to be hardcoded to "admin", which is a global superuser:
	// site_access and workspace_access both short-circuit on role ==
	// "admin", so every auto-created SSO user got access to every site and
	// every workspace in the deployment. Defaults to "editor" and only
	// "admin" or "editor" are accepted, matching the roles the members API
	// allows an operator to set by hand.
	OIDCEnabled      bool
	OIDCIssuerURL    string
	OIDCClientID     string
	OIDCClientSecret string
	OIDCRedirectURL  string
	OIDCAllowDomains string
	OIDCDefaultRole  string
}

func Load() *Config {
	dataDir := envOr("DATA_DIR", "./data")
	return &Config{
		Port:          envInt("PORT", 8080),
		DataDir:       dataDir,
		DBPath:        filepath.Join(dataDir, "atomicsite.db"),
		MediaDir:      filepath.Join(dataDir, "media"),
		FontsDir:      filepath.Join(dataDir, "fonts"),
		JWTSecret:     envOr("JWT_SECRET", "change-me-in-production"),
		BaseURL:       envOr("BASE_URL", "http://localhost:8080"),
		MaxUploadSize: envInt64("MAX_UPLOAD_SIZE", 20<<20), // 20 MB
		MediaVariants: envIntList("MEDIA_VARIANTS", []int{320, 640, 1280, 1920}),

		TrackPath:     envOr("TRACK_PATH", "/t"),
		AnalyticsSalt: envOr("ANALYTICS_SALT", "atomicsite-default-fingerprint-salt-change-me"),

		BrightCRMWebhookURL:            envOr("BRIGHTCRM_WEBHOOK_URL", ""),
		BrightCRMWebhookSecret:         envOr("BRIGHTCRM_WEBHOOK_SECRET", ""),
		BrightCRMWebhookSecretPrevious: envOr("BRIGHTCRM_WEBHOOK_SECRET_PREVIOUS", ""),
		AdminReloadToken:               envOr("ADMIN_RELOAD_TOKEN", ""),
		AdminWriteRateCapacity:         envInt("ADMIN_WRITE_RATE_CAPACITY", 20),
		AdminWriteRateRefill:           envInt("ADMIN_WRITE_RATE_REFILL", 1),
		CRMSyncMinInterval:             time.Duration(envInt("CRM_SYNC_MIN_INTERVAL_SECONDS", 60)) * time.Second,

		PrimaryDomain:   envOr("ATOMICSITE_PRIMARY_DOMAIN", ""),
		BuiltSiteSuffix: envOr("BUILT_SITE_SUFFIX", ""),
		DeploymentMode:  envOr("ATOMICSITE_DEPLOYMENT_MODE", DeploymentModeSingle),

		NginxConfDir:       envOr("ATOMICSITE_NGINX_CONF_DIR", ""),
		NginxSitesDir:      envOr("ATOMICSITE_NGINX_SITES_DIR", ""),
		AcmeWebrootDir:     envOr("ATOMICSITE_ACME_WEBROOT", ""),
		CertbotPath:        envOr("ATOMICSITE_CERTBOT_PATH", ""),
		NginxReloadCommand: envOr("ATOMICSITE_NGINX_RELOAD_CMD", ""),
		EdgeIP:             envOr("ATOMICSITE_EDGE_IP", ""),

		DesignReferencesDir: envOr("DESIGN_REFERENCES_DIR", ""),

		ShieldKey:         envOr("ATOMICSITE_SHIELD_KEY", ""),
		ShieldHintLevel:   envOr("ATOMICSITE_SHIELD_HINT_LEVEL", ""),
		ShieldRedisURL:    envOr("ATOMICSITE_SHIELD_REDIS_URL", ""),
		ShieldRedisPrefix: envOr("ATOMICSITE_SHIELD_REDIS_PREFIX", ""),

		RequireMFA: strings.ToLower(strings.TrimSpace(envOr("ATOMICSITE_REQUIRE_MFA", ""))),

		AuditLogRetentionDays: envInt("ATOMICSITE_AUDIT_LOG_RETENTION_DAYS", 0),

		LifecyclePauseDays:  envInt("ATOMICSITE_LIFECYCLE_PAUSE_DAYS", 0),
		LifecycleDeleteDays: envInt("ATOMICSITE_LIFECYCLE_DELETE_DAYS", 0),

		GDPRDeleteCoolingDays: envInt("ATOMICSITE_GDPR_DELETE_COOLING_DAYS", 0),

		TrustedProxies: envOr("ATOMICSITE_TRUSTED_PROXIES", ""),

		OIDCEnabled:      strings.EqualFold(strings.TrimSpace(envOr("OIDC_ENABLED", "")), "true"),
		OIDCIssuerURL:    strings.TrimSpace(envOr("OIDC_ISSUER_URL", "")),
		OIDCClientID:     strings.TrimSpace(envOr("OIDC_CLIENT_ID", "")),
		OIDCClientSecret: strings.TrimSpace(envOr("OIDC_CLIENT_SECRET", "")),
		OIDCRedirectURL:  strings.TrimSpace(envOr("OIDC_REDIRECT_URL", "")),
		OIDCAllowDomains: strings.TrimSpace(envOr("OIDC_ALLOW_DOMAINS", "")),
		OIDCDefaultRole:  oidcDefaultRole(envOr("OIDC_DEFAULT_ROLE", "")),
	}
}

// Validate returns nil when the loaded config is safe for the deployment
// shape implied by BaseURL. Production-like deployments (BaseURL != localhost)
// must override every secret and operational toggle that has a known default;
// any one still at its sentinel value is treated as a misconfiguration and
// the server refuses to start.
//
// Why a refuse-to-start guard rather than a warning: every previous
// "warning-only" approach in the codebase has been ignored at least once
// in production. The boot-time hard fail is the only mechanism that
// guarantees the operator notices.
//
// Local development (BaseURL starts with http://localhost) keeps the
// permissive behaviour so contributors don't have to set five env vars
// to run `go test` or `go run`.
func (c *Config) Validate() error {
	// Deployment-mode validation runs even on localhost: a misspelt mode
	// or "cloud" on an OSS binary should fail loudly regardless of where
	// the operator is running it. Localhost still bypasses the secret /
	// HTTPS / domain guards below.
	mode := strings.TrimSpace(c.DeploymentMode)
	if mode == "" {
		mode = DeploymentModeSingle
	}
	switch mode {
	case DeploymentModeSingle:
		// always fine
	case DeploymentModeCloud:
		if !ee.IsAvailable() {
			return fmt.Errorf("ATOMICSITE_DEPLOYMENT_MODE=%s requires a binary built with -tags ee; this is the OSS build", mode)
		}
	default:
		return fmt.Errorf("ATOMICSITE_DEPLOYMENT_MODE=%q is not a recognised value; use %q or %q", mode, DeploymentModeSingle, DeploymentModeCloud)
	}

	switch c.RequireMFA {
	case MFAOptional, MFAAdminsOnly, MFAAllUsers:
		// always fine
	default:
		return fmt.Errorf("ATOMICSITE_REQUIRE_MFA=%q is not a recognised value; use %q, %q, or %q", c.RequireMFA, MFAOptional, MFAAdminsOnly, MFAAllUsers)
	}

	if c.IsLocalDev() {
		return nil
	}

	var problems []string

	if c.JWTSecret == "" || c.JWTSecret == DefaultJWTSecret {
		problems = append(problems, "JWT_SECRET is unset or equal to the documented default; set a 32+ byte random value")
	} else if len(c.JWTSecret) < 32 {
		problems = append(problems, fmt.Sprintf("JWT_SECRET is %d bytes; require at least 32 for HS256 collision resistance", len(c.JWTSecret)))
	}

	if c.AnalyticsSalt == "" || c.AnalyticsSalt == DefaultAnalyticsSalt {
		problems = append(problems, "ANALYTICS_SALT is unset or equal to the documented default; set a random value (visitor fingerprints become predictable otherwise)")
	}

	// At-rest crypto + the MCP shield key off the same 32-byte key. Without it
	// atrest.New runs as a passthrough (TOTP seeds, app-install credentials and
	// Stripe/Mollie keys stored PLAINTEXT) and the shield silently forwards
	// order PII to the LLM. Required in production: a missing crypto key must
	// refuse to start, not fail open.
	if len(c.ShieldKey) != 32 {
		problems = append(problems, "ATOMICSITE_SHIELD_KEY is unset or not 32 bytes; production must set a 32-byte key or at-rest encryption (TOTP seeds, app credentials) and MCP PII redaction are silently disabled. Generate one with: head -c 24 /dev/urandom | base64")
	}

	// HTTPS scheme guard. The auth cookie's Secure attribute is gated on
	// "not localhost"; without HTTPS in production, the cookie still gets
	// set Secure=true and never travels, which manifests as login that
	// "doesn't stick". Fail fast with the right error message.
	if !strings.HasPrefix(c.BaseURL, "https://") {
		problems = append(problems, fmt.Sprintf("BASE_URL=%q is not HTTPS; production deployments must serve over TLS or sessions will not persist", c.BaseURL))
	}

	// Either ATOMICSITE_PRIMARY_DOMAIN (single-root-domain shape, the
	// typical SaaS deploy) or BUILT_SITE_SUFFIX (legacy multi-tenant
	// subdomain shape) must be set in production. Both unset means the
	// operator hasn't told Atomic Site which domain it's responsible
	// for, and any defaults we'd seed for fresh sites would be wrong.
	// Allowing both is fine: PrimaryDomain seeds the canonical site,
	// BuiltSiteSuffix widens CORS for legacy subdomain tenancy.
	if strings.TrimSpace(c.PrimaryDomain) == "" && strings.TrimSpace(c.BuiltSiteSuffix) == "" {
		problems = append(problems, "ATOMICSITE_PRIMARY_DOMAIN is unset; set it to the apex domain this Atomic Site instance serves (e.g. ATOMICSITE_PRIMARY_DOMAIN=example.com). Multi-tenant deployments that use wildcard subdomains may set BUILT_SITE_SUFFIX instead.")
	}

	if len(problems) > 0 {
		return errors.New("config validation failed:\n  - " + strings.Join(problems, "\n  - "))
	}
	return nil
}

// ValidateAdminPassword is called at admin-seed time only. It enforces the
// same "no defaults in production" guard as Validate but reports the password
// problem separately so the caller can fail with a useful message even when
// the rest of the config is fine. Empty password in production is a hard
// fail; the seed path must abort rather than silently use the documented
// default.
func (c *Config) ValidateAdminPassword(adminPassword string) error {
	if c.IsLocalDev() {
		return nil
	}
	if adminPassword == "" || adminPassword == DefaultAdminPassword {
		return errors.New("ADMIN_PASSWORD is unset or equal to the documented default; production deployments must set ADMIN_PASSWORD to a strong unique value before first boot")
	}
	if len(adminPassword) < 12 {
		return fmt.Errorf("ADMIN_PASSWORD is only %d characters; require at least 12 (use a passphrase generator)", len(adminPassword))
	}
	return nil
}

// IsLocalDev reports whether the BaseURL points at localhost. Local dev
// keeps the lenient defaults (no secrets, no HTTPS) so contributors can
// run the server with a single command. The Validate guards bypass on
// this branch.
func (c *Config) IsLocalDev() bool {
	return strings.HasPrefix(c.BaseURL, "http://localhost") ||
		strings.HasPrefix(c.BaseURL, "http://127.0.0.1") ||
		strings.HasPrefix(c.BaseURL, "http://0.0.0.0")
}

// oidcDefaultRole resolves OIDC_DEFAULT_ROLE, failing closed to the
// least-privileged role. Anything unrecognised (including a typo like
// "Admin " or "owner", which is a workspace role and not a user role)
// becomes "editor" rather than silently granting more than the operator
// asked for.
func oidcDefaultRole(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "admin":
		return "admin"
	default:
		return "editor"
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envInt64(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return fallback
}

func envIntList(key string, fallback []int) []int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	parts := strings.Split(v, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}
