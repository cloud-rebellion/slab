package agent

import (
	"strings"
)

// secretSettingKeys are the "<category>.<key>" settings whose VALUE is a
// bearer-equivalent secret and must never be returned to an LLM/agent (the
// agent may read the descriptor, not the value). Kept in sync with the catalog
// entries marked sensitive (Mollie key, CRM webhook HMAC secret).
var secretSettingKeys = map[string]bool{
	"payments.mollie_api_key":      true,
	"analytics.crm_webhook_secret": true,
}

// IsSecretSetting reports whether the value for category.key is a secret that
// must be masked before exposure over the agent API / MCP. It checks the
// explicit allowlist plus a defensive suffix heuristic so a future secret key
// is masked by default rather than leaked.
func IsSecretSetting(category, key string) bool {
	if secretSettingKeys[category+"."+key] {
		return true
	}
	k := strings.ToLower(key)
	for _, suffix := range []string{"_secret", "_api_key", "_apikey", "_token", "_password", "_private_key"} {
		if strings.HasSuffix(k, suffix) {
			return true
		}
	}
	return false
}

// SettingDescriptor describes one setting key the agent can reason about.
// The catalog covers every key that has a real backend consumer. Settings
// that exist only on the human admin UI (no backend wiring) are deliberately
// excluded so the agent doesn't waste tokens writing values that go nowhere.
//
// Reading: every entry's CurrentValue reflects the row currently in the
// database (or the documented default when the row is absent). The agent
// uses this to answer "what is this site set to?" without making a separate
// query per key.
//
// Writing: AgentWritable=true means the agent can flip this via PATCH
// /api/agent/settings (the request body is {category, key, value}). Anything
// admin-only (security headers, allowed_scripts, deploy targets) carries
// AgentWritable=false so the agent knows to ask the human to flip it via
// the link in HumanAdminURL.
type SettingDescriptor struct {
	// Category is the bucket the setting lives under (general / seo /
	// analytics / security / profile). Maps to the row's category column.
	Category string `json:"category"`
	// Key is the unique row key under that category. The fully-qualified
	// name is "{category}.{key}" (e.g. "seo.meta_title_template").
	Key string `json:"key"`
	// Label is the human-readable name the admin UI uses for this setting.
	Label string `json:"label"`
	// Description is one or two sentences explaining what the builder /
	// renderer / nginx does with this value. Read this before writing.
	Description string `json:"description"`
	// ValueType is the shape the value column holds. Valid values:
	//   "bool"  → "0" or "1" (use 0/1 strings, not "true"/"false")
	//   "int"   → integer rendered as a decimal string
	//   "string"→ free-form
	//   "enum"  → one of EnumValues
	//   "url"   → absolute URL with scheme
	//   "email" → RFC 5322 email
	//   "css"   → CSS source (selectors + properties)
	//   "html"  → HTML/JS snippet (inserted verbatim into <head>)
	//   "csv"   → comma-separated string (each entry trimmed)
	//   "media_id" → references a media row id; resolved to a URL by the builder
	//   "template" → meta-template string with {token} substitutions
	ValueType string `json:"value_type"`
	// EnumValues lists the allowed values when ValueType is "enum". Empty
	// for other types. Validators in BulkUpsertSettings reject unknown
	// values when this list is non-empty.
	EnumValues []string `json:"enum_values,omitempty"`
	// MinInt / MaxInt bound int values (inclusive). Both zero means no bound.
	MinInt int64 `json:"min_int,omitempty"`
	MaxInt int64 `json:"max_int,omitempty"`
	// CurrentValue is what the row currently holds, or the documented
	// default when the row is absent. Always a string (matches column type).
	CurrentValue string `json:"current_value"`
	// IsDefault is true when CurrentValue came from the documented default
	// rather than an explicit row. Use this to spot un-configured settings.
	IsDefault bool `json:"is_default"`
	// AgentWritable mirrors agentWritableSettingsCategories on the
	// handler side. When false, the agent must ask the human admin to
	// flip the setting (point them at HumanAdminURL).
	AgentWritable bool `json:"agent_writable"`
	// HumanAdminURL is the per-site URL for the admin UI page that owns
	// this setting (e.g. /sites/{id}/settings/security). Filled in at
	// build time so the agent can paste a clickable link to the human.
	HumanAdminURL string `json:"human_admin_url"`
	// Tokens lists the {tokens} valid for ValueType="template" entries.
	// Empty for other types.
	Tokens []string `json:"tokens,omitempty"`
}

// SettingsCatalogInfo wraps the catalog with category-level summaries so
// the agent can quickly count writable surfaces.
type SettingsCatalogInfo struct {
	// WritableCategories enumerates the categories the agent can write to.
	// Mirrors agentWritableSettingsCategories on the handler side.
	WritableCategories []string `json:"writable_categories"`
	// AdminOnlyCategories enumerates the categories the agent can read but
	// not write. Surfaces as a hint to ask the human.
	AdminOnlyCategories []string `json:"admin_only_categories"`
	// Items is the per-key descriptor list.
	Items []SettingDescriptor `json:"items"`
}

// buildSettingsCatalog assembles the catalog with current values resolved
// from settingsMap. siteID is used to materialize HumanAdminURL paths.
func buildSettingsCatalog(siteID string, settingsMap map[string]string) SettingsCatalogInfo {
	descriptors := []SettingDescriptor{
		// --- general -------------------------------------------------------
		{
			Category: "general", Key: "additional_langs",
			Label:       "Additional languages",
			Description: "Comma-separated list of locale codes the site publishes besides the default lang (e.g. \"sv,de,fr\"). Pages whose slug starts with /<lang>/ are treated as that locale's counterparts and feed hreflang emission.",
			ValueType:   "csv",
			AgentWritable: true,
		},
		{
			Category: "general", Key: "default_lang",
			Label:       "Default language (mirror)",
			Description: "Mirror of sites.lang. Patching this via /api/agent/settings does not move the column; use PATCH /api/sites/{id} for that. Surfaced here so the agent can read the lang without a separate fetch.",
			ValueType:   "string",
			AgentWritable: true,
		},
		{
			Category: "general", Key: "domain_aliases",
			Label:       "Domain aliases",
			Description: "Comma-separated host list. Each alias gets a 301 redirect to the primary domain via the public/_redirects file (Caddy / Netlify / Cloudflare Pages). Strip protocol; \"www.example.com,example.org\" is the right shape.",
			ValueType:   "csv",
			AgentWritable: true,
		},
		{
			Category: "general", Key: "container_width",
			Label:       "Page container max-width",
			Description: "Maximum width of the centred content column on every page. \"narrow\" = 64rem (good for editorial), \"default\" = 72rem (matches most marketing sites including brightinteraction.com), \"wide\" = 80rem (dashboard-style), \"fluid\" = 100% (full-bleed; rely on per-block max-widths). Renders into a CSS custom property the .block, .container, .site-header, .site-footer all read.",
			ValueType:   "enum",
			EnumValues:  []string{"narrow", "default", "wide", "fluid"},
			AgentWritable: true,
		},
		{
			Category: "general", Key: "mobile_breakpoint",
			Label:       "Mobile breakpoint (px)",
			Description: "Below this viewport width, multi-column layouts collapse to a single column. Default 640. Decrease for tablet-first sites; increase for mobile-heavy traffic where the design should stay simple longer.",
			ValueType:   "int",
			MinInt:      320,
			MaxInt:      960,
			AgentWritable: true,
		},
		{
			Category: "general", Key: "tablet_breakpoint",
			Label:       "Tablet breakpoint (px)",
			Description: "Below this viewport width but above mobile_breakpoint, layouts use a 2-column variant where applicable. Default 1024.",
			ValueType:   "int",
			MinInt:      640,
			MaxInt:      1280,
			AgentWritable: true,
		},
		{
			Category: "general", Key: "hero_layout",
			Label:       "Default hero layout",
			Description: "How hero blocks render when image_id is set. \"centered\" = traditional centred hero with image below the headline (good for product launches), \"split\" = side-by-side image right + text left on desktop (good for SaaS marketing, matches brightinteraction.com). Per-page override via the hero block's data.layout key.",
			ValueType:   "enum",
			EnumValues:  []string{"centered", "split"},
			AgentWritable: true,
		},
		{
			Category: "general", Key: "analytics_retention_days",
			Label:       "Analytics retention (days)",
			Description: "How many days of raw visit_events the daily retention sweep keeps before purging. Default 180 (six months). Aggregates and identified sessions survive the purge. Range 7..3650; values outside the range fall back to the default.",
			ValueType:   "int",
			MinInt:      7,
			MaxInt:      3650,
			AgentWritable: true,
		},
		{
			Category: "general", Key: "consent_retention_days",
			Label:       "Consent proof retention (days)",
			Description: "How long the GDPR proof-of-consent log keeps each consent_records row. Default 730 (two years), the standard EU audit horizon. Range 7..3650.",
			ValueType:   "int",
			MinInt:      7,
			MaxInt:      3650,
			AgentWritable: true,
		},
		{
			Category: "general", Key: "engagement_retention_days",
			Label:       "Engagement metrics retention (days)",
			Description: "How long visit_engagement rows (screen size, time-on-page, scroll depth) are retained. Default 90 days. Higher row volume than visit_events and lower forensic value past a quarter, so kept shorter by default.",
			ValueType:   "int",
			MinInt:      7,
			MaxInt:      3650,
			AgentWritable: true,
		},

		// --- design --------------------------------------------------------
		{
			Category: "design", Key: "fidelity",
			Label:       "Design fidelity",
			Description: "The design-freedom dial. \"performance\" targets perfect scores on every category (static heroes, zero perpetual motion). \"balanced\" (default) is the standard taste rulebook and budgets. \"showcase\" unlocks expressive design (fx utility classes, scroll-driven animation, view transitions, bespoke tokens, 4-animation motion budget) and grades performance against showcase budgets with the fidelity recorded on every evaluation. Security, privacy, accessibility, self-hosted fonts and slop lint are identical in all three. Read design_playbook.fidelity for the full contract; change BEFORE authoring, then rebuild.",
			ValueType:   "enum",
			EnumValues:  []string{"performance", "balanced", "showcase"},
			AgentWritable: true,
		},
		{
			Category: "design", Key: "strict_lint",
			Label:       "Strict design lint",
			Description: "When 1 (default), create_block / update_block refuse writes carrying blocking design-lint findings (slop terms, placeholder names, fake numbers, archetype drift) unless force=true after human approval. When 0, findings are returned as warnings but writes proceed. Independent of design.fidelity: fidelity changes WHICH taste rules apply, strict_lint changes whether hard violations block writes. Slop findings block in every fidelity while strict lint is on.",
			ValueType:   "bool",
			AgentWritable: true,
		},

		// --- seo -----------------------------------------------------------
		{
			Category: "seo", Key: "meta_title_template",
			Label:       "Meta title template",
			Description: "Applied to every page that doesn't override its own meta_title. Empty string means \"use the page title verbatim\". Token expansion happens at build time; missing tokens collapse along with their adjacent separators.",
			ValueType:   "template",
			Tokens:      []string{"{page_title}", "{page_description}", "{site_name}", "{lang}", "{separator}"},
			AgentWritable: true,
		},
		{
			Category: "seo", Key: "meta_description_template",
			Label:       "Meta description template",
			Description: "Applied to every page that doesn't override its own meta_description. Empty string means \"use the page description verbatim\". Same token grammar as meta_title_template.",
			ValueType:   "template",
			Tokens:      []string{"{page_title}", "{page_description}", "{site_name}", "{lang}", "{separator}"},
			AgentWritable: true,
		},
		{
			Category: "seo", Key: "canonical_base",
			Label:       "Canonical base URL",
			Description: "Override for the prefix used when emitting <link rel=\"canonical\">. Defaults to https://<primary domain>. Set this when public URLs differ from the build's domain (CDN, sub-path proxy, staging vs. prod).",
			ValueType:   "url",
			AgentWritable: true,
		},
		{
			Category: "seo", Key: "hreflang_strategy",
			Label:       "Hreflang strategy",
			Description: "How the builder maps locales to URLs. \"path\" (default) uses /<lang>/<slug>; \"subdomain\" uses <lang>.<host>; \"off\" disables hreflang emission entirely. Counterparts are gathered from actually-published pages so monolingual sites stay clean.",
			ValueType:   "enum",
			EnumValues:  []string{"path", "subdomain", "off"},
			AgentWritable: true,
		},
		{
			Category: "seo", Key: "og_default_image_id",
			Label:       "Default Open Graph image",
			Description: "media row id used as the fallback og:image / twitter:image when a page has no own image. The builder resolves to the absolute /media/<id>/original.<ext> URL. Upload via /api/sites/{id}/media first, then PATCH this with the returned id.",
			ValueType:   "media_id",
			AgentWritable: true,
		},
		{
			Category: "seo", Key: "robots_txt",
			Label:       "robots.txt full override",
			Description: "Replaces the auto-generated robots.txt body wholesale. Empty (default) keeps the AI-bot-blocking baseline. Use only if you want to rewrite the entire file by hand; otherwise set robots_txt_extra to append.",
			ValueType:   "string",
			AgentWritable: true,
		},
		{
			Category: "seo", Key: "robots_txt_extra",
			Label:       "robots.txt extras",
			Description: "Appended to the auto-generated robots.txt under a labeled section. Use this for site-specific Disallow rules without losing the AI-bot-blocking baseline.",
			ValueType:   "string",
			AgentWritable: true,
		},
		{
			Category: "seo", Key: "llms_txt",
			Label:       "llms.txt content",
			Description: "Full body of the public /llms.txt file consumed by AI crawlers (Perplexity, ChatGPT search, Claude, Gemini). Empty falls back to a builder-generated summary derived from published pages.",
			ValueType:   "string",
			AgentWritable: true,
		},
		{
			Category: "seo", Key: "sitemap_enabled",
			Label:       "Generate sitemap.xml",
			Description: "When 1 (default), the @astrojs/sitemap integration runs at build and emits /sitemap-index.xml. Set 0 to drop the integration entirely.",
			ValueType:   "bool",
			AgentWritable: true,
		},

		// --- analytics -----------------------------------------------------
		{
			Category: "analytics", Key: "cookieproof_enabled",
			Label:       "Enable cookie consent banner",
			Description: "Flips the bundled, same-origin CookieProof banner. When 1, the layout emits the inline-config widget + the /t/consent receiver lands proofs in atomicsite's consent_records table. When 0, no consent UI is injected. Branding colors flow in automatically; copy/categories are configurable in the Cookies panel.",
			ValueType:   "bool",
			AgentWritable: true,
		},
		{
			Category: "analytics", Key: "cookie_banner_position",
			Label:       "Banner position",
			Description: "Where the cookie banner anchors on screen. One of: bottom (default), top, center.",
			ValueType:   "string",
			AgentWritable: true,
		},
		{
			Category: "analytics", Key: "cookie_banner_title",
			Label:       "Banner title (override)",
			Description: "Optional. Overrides the widget's translated banner heading. Leave empty to use the locale default.",
			ValueType:   "string",
			AgentWritable: true,
		},
		{
			Category: "analytics", Key: "cookie_banner_description",
			Label:       "Banner description (override)",
			Description: "Optional. Overrides the widget's translated banner body. Leave empty to use the locale default.",
			ValueType:   "string",
			AgentWritable: true,
		},
		{
			Category: "analytics", Key: "cookie_banner_accept",
			Label:       "Accept button label (override)",
			Description: "Optional. Overrides the widget's translated 'Accept all' button text. Leave empty to use the locale default.",
			ValueType:   "string",
			AgentWritable: true,
		},
		{
			Category: "analytics", Key: "cookie_banner_reject",
			Label:       "Reject button label (override)",
			Description: "Optional. Overrides the widget's translated 'Reject all' button text. Leave empty to use the locale default. The Reject button uses the same fill color as Accept (IMY 2026 equal-prominence).",
			ValueType:   "string",
			AgentWritable: true,
		},
		{
			Category: "analytics", Key: "cookie_banner_customize",
			Label:       "Settings button label (override)",
			Description: "Optional. Overrides the widget's translated 'Customize' / 'Save preferences' button text. Leave empty to use the locale default.",
			ValueType:   "string",
			AgentWritable: true,
		},
		{
			Category: "analytics", Key: "cookie_declarations",
			Label:       "Per-cookie declarations (JSON)",
			Description: "JSON array of cookie metadata that powers the extended-settings table in the consent modal. Each entry: {category, name, provider, purpose, expiry}. Category is one of necessary|analytics|marketing|preferences. User entries layer on top of presets auto-derived from enabled trackers (GA4 etc.); on (category, name) collision the user value wins. Edit via /sites/{id}/cookies. Max 100 entries; each field <= 200 chars.",
			ValueType:   "json",
			AgentWritable: true,
		},
		{
			Category: "analytics", Key: "cookie_cat_analytics",
			Label:       "Offer analytics category",
			Description: "When 1 (default), the banner exposes an 'Analytics' toggle. When 0, the category is omitted entirely.",
			ValueType:   "bool",
			AgentWritable: true,
		},
		{
			Category: "analytics", Key: "cookie_cat_marketing",
			Label:       "Offer marketing category",
			Description: "When 1 (default), the banner exposes a 'Marketing' toggle. When 0, the category is omitted entirely.",
			ValueType:   "bool",
			AgentWritable: true,
		},
		{
			Category: "analytics", Key: "cookie_cat_preferences",
			Label:       "Offer preferences category",
			Description: "When 1 (default), the banner exposes a 'Preferences' toggle. When 0, the category is omitted entirely.",
			ValueType:   "bool",
			AgentWritable: true,
		},
		{
			Category: "analytics", Key: "atomicsite_tracking_enabled",
			Label:       "Atomicsite server-side tracking",
			Description: "When 1 (default), the nginx access log tail emits visit_events to the admin DB. Pure server-side, no client JS, GDPR-aligned legitimate-interest posture.",
			ValueType:   "bool",
			AgentWritable: true,
		},
		{
			Category: "analytics", Key: "ga4_enabled",
			Label:       "Enable Google Analytics 4",
			Description: "When 1, the layout injects gtag.js + the GA4 config snippet with anonymize_ip=true. Pair with ga4_id (G-XXXXXXX). When CookieProof is also on, the relay gates gtag config until the visitor consents to analytics.",
			ValueType:   "bool",
			AgentWritable: true,
		},
		{
			Category: "analytics", Key: "ga4_id",
			Label:       "GA4 Measurement ID",
			Description: "Google Analytics 4 measurement id, format G-XXXXXXX. When non-empty and ga4_enabled is unset, the builder treats GA4 as enabled (sane default for an agent that just wrote the id).",
			ValueType:   "string",
			AgentWritable: true,
		},
		{
			Category: "analytics", Key: "umami_enabled",
			Label:       "Enable Umami",
			Description: "When 1, the layout injects the Umami script tag pointing at umami_url with data-website-id=umami_site_id. Privacy-aligned analytics, no cookies.",
			ValueType:   "bool",
			AgentWritable: true,
		},
		{
			Category: "analytics", Key: "umami_url",
			Label:       "Umami host URL",
			Description: "Base URL for the Umami instance, e.g. https://analytics.example.com. The script tag becomes <umami_url>/script.js.",
			ValueType:   "url",
			AgentWritable: true,
		},
		{
			Category: "analytics", Key: "umami_site_id",
			Label:       "Umami Site ID",
			Description: "data-website-id attribute on the Umami script tag. Get it from your Umami dashboard.",
			ValueType:   "string",
			AgentWritable: true,
		},
		{
			Category: "analytics", Key: "crm_webhook_url",
			Label:       "CRM webhook URL",
			Description: "Where outbound visitor + consent events get POSTed (HMAC-SHA256 signed via X-Atomicsite-Signature). The companion CRM endpoint receives identified-visitor pushes and replies on /t/inbound to drive personalization.",
			ValueType:   "url",
			AgentWritable: true,
		},
		{
			Category: "analytics", Key: "crm_webhook_secret",
			Label:       "CRM webhook secret",
			Description: "Shared HMAC secret for outbound webhook signing AND inbound /t/inbound verification. Treat as sensitive; never echo into rendered content.",
			ValueType:   "string",
			AgentWritable: true,
		},
		{
			Category: "analytics", Key: "cookie_banner_snippet",
			Label:       "Custom cookie banner snippet",
			Description: "Verbatim HTML/JS injected into <head>. Bring-your-own escape hatch for Cookiebot, OneTrust, Termly, Iubenda. No validation; treat as trusted operator input.",
			ValueType:   "html",
			AgentWritable: true,
		},
		{
			Category: "analytics", Key: "personalization_enabled",
			Label:       "Enable bidirectional CRM personalization",
			Description: "When 1, the layout emits the visitor-hydration script that fetches /t/visitor and reveals [data-asp-when] blocks. Pair with identity_max_age_days. Required for conditional blocks to actually render.",
			ValueType:   "bool",
			AgentWritable: true,
		},
		{
			Category: "analytics", Key: "identity_max_age_days",
			Label:       "Identity max age (days)",
			Description: "Visitors whose identity_confirmed_at is older than this window are downgraded to anonymous-tier metadata only. Default 30. Lower for stricter privacy posture.",
			ValueType:   "int",
			MinInt:      1,
			MaxInt:      3650,
			AgentWritable: true,
		},

		// --- security (admin-only) ----------------------------------------
		{
			Category: "security", Key: "hsts_enabled",
			Label:       "Enable HSTS",
			Description: "When 1, the response carries Strict-Transport-Security with max-age=hsts_max_age + includeSubDomains. Off by default to avoid bricking dev domains.",
			ValueType:   "bool",
			AgentWritable: false,
		},
		{
			Category: "security", Key: "hsts_max_age",
			Label:       "HSTS max-age",
			Description: "Seconds. 31536000 (one year) is the recommended baseline; site-inspector requires >= 1y for the HSTS check to pass.",
			ValueType:   "int",
			MinInt:      0,
			MaxInt:      63072000,
			AgentWritable: false,
		},
		{
			Category: "security", Key: "hsts_preload",
			Label:       "HSTS preload",
			Description: "When 1, appends the preload directive. Implies a public commitment; only enable after submitting to hstspreload.org.",
			ValueType:   "bool",
			AgentWritable: false,
		},
		{
			Category: "security", Key: "csp_enabled",
			Label:       "Enable CSP",
			Description: "When 1, the response carries Content-Security-Policy built from the trusted-domains table + frame_ancestors + csp_extra_directives. Off keeps the pre-CSP behaviour.",
			ValueType:   "bool",
			AgentWritable: false,
		},
		{
			Category: "security", Key: "csp_extra_directives",
			Label:       "CSP extra directives",
			Description: "Appended to the auto-built CSP. Use for report-uri, sandbox, anything atomicsite doesn't expose as its own field. Trailing semicolons trimmed.",
			ValueType:   "string",
			AgentWritable: false,
		},
		{
			Category: "security", Key: "frame_ancestors",
			Label:       "frame-ancestors",
			Description: "Who can embed THIS site in an iframe. 'none' (default) is anti-clickjacking; use 'self' or a host list to allow embedding (e.g. for a customer portal).",
			ValueType:   "string",
			AgentWritable: false,
		},
		{
			Category: "security", Key: "x_frame_options",
			Label:       "X-Frame-Options",
			Description: "Legacy clickjacking protection. DENY refuses all framing; SAMEORIGIN allows same-origin only. Modern browsers honour CSP frame-ancestors instead, but X-Frame-Options remains a useful belt-and-suspenders.",
			ValueType:   "enum",
			EnumValues:  []string{"DENY", "SAMEORIGIN"},
			AgentWritable: false,
		},
		{
			Category: "security", Key: "x_content_type_options",
			Label:       "Send X-Content-Type-Options nosniff",
			Description: "When 1 (default), emits X-Content-Type-Options: nosniff. Recommended on for every public site.",
			ValueType:   "bool",
			AgentWritable: false,
		},
		{
			Category: "security", Key: "referrer_policy",
			Label:       "Referrer-Policy",
			Description: "Controls how much of the URL sites can read in the Referer header. strict-origin-when-cross-origin is the recommended baseline.",
			ValueType:   "enum",
			EnumValues:  []string{"no-referrer", "no-referrer-when-downgrade", "origin", "origin-when-cross-origin", "same-origin", "strict-origin", "strict-origin-when-cross-origin", "unsafe-url"},
			AgentWritable: false,
		},
		{
			Category: "security", Key: "permissions_policy",
			Label:       "Permissions-Policy",
			Description: "Comma-separated feature directives (camera=(), microphone=(), geolocation=()). Empty parens block; (self) allows the page itself.",
			ValueType:   "string",
			AgentWritable: false,
		},
		{
			Category: "security", Key: "coop",
			Label:       "Cross-Origin-Opener-Policy",
			Description: "Default same-origin isolates the browsing context; same-origin-allow-popups loosens for OAuth popups; unsafe-none disables.",
			ValueType:   "enum",
			EnumValues:  []string{"same-origin", "same-origin-allow-popups", "unsafe-none"},
			AgentWritable: false,
		},
		{
			Category: "security", Key: "corp",
			Label:       "Cross-Origin-Resource-Policy",
			Description: "Who may load THIS site's resources cross-origin. same-origin (default) blocks all cross-origin loads; same-site allows sibling subdomains; cross-origin allows everyone.",
			ValueType:   "enum",
			EnumValues:  []string{"same-origin", "same-site", "cross-origin"},
			AgentWritable: false,
		},
		{
			Category: "security", Key: "coep",
			Label:       "Cross-Origin-Embedder-Policy",
			Description: "Empty (default) preserves third-party embeds. require-corp blocks iframes without CORP and unlocks SharedArrayBuffer; credentialless drops cookies on cross-origin loads.",
			ValueType:   "enum",
			EnumValues:  []string{"", "require-corp", "credentialless"},
			AgentWritable: false,
		},
		{
			Category: "security", Key: "x_xss_protection",
			Label:       "X-XSS-Protection",
			Description: "Modern browsers ignore this header (the buggy reflective filter was removed). Default 1; mode=block is purely Site Inspector / securityheaders.com grading parity. OWASP's modern recommendation is 0 (turn the legacy filter off) but it trips graders. Real XSS protection comes from the CSP.",
			ValueType:   "enum",
			EnumValues:  []string{"0", "1", "1; mode=block"},
			AgentWritable: false,
		},
		{
			Category: "security", Key: "x_permitted_cross_domain_policies",
			Label:       "X-Permitted-Cross-Domain-Policies",
			Description: "Locks down the legacy Adobe Flash / Silverlight cross-domain policy file lookup. Default none is a Site Inspector A+ win and has zero downside on modern stacks.",
			ValueType:   "enum",
			EnumValues:  []string{"none", "master-only", "by-content-type", "all"},
			AgentWritable: false,
		},
		{
			Category: "security", Key: "https_redirect",
			Label:       "HTTPS redirect",
			Description: "When 1 (default), the deploy target's reverse proxy enforces HTTPS via 301. Atomicsite Deploys terminate TLS at Caddy / Dockyard so this is implicit; the row is kept so self-hosters can read the intent.",
			ValueType:   "bool",
			AgentWritable: false,
		},

		// --- analytics.cwv_enabled (Sprint 5 quick-win, 2026-05-24) -------
		// Real-user Core Web Vitals collection. Opt-in per site so
		// tenants without dashboard interest don't pay the
		// beacon-per-pageview cost. The /t/cwv receiver writes
		// cwv_events rows; the Analytics dashboard widget reads p75
		// over the last 7 days per metric + device.
		{
			Category: "analytics", Key: "cwv_enabled",
			Label:       "Core Web Vitals collection",
			Description: "When on, every page load runs an inline web-vitals measurement script that posts LCP / INP / CLS / FCP / TTFB samples back to /t/cwv. The Analytics dashboard surfaces p75 values + Good / Needs Improvement / Poor ratings against Google's published thresholds. Consent-gated when CookieProof is configured; legitimate-interest otherwise.",
			ValueType:   "bool",
			AgentWritable: true,
		},

		// --- search (Sprint 5 quick-win, 2026-05-24) ---------------------
		// Pagefind static search index. Opt-in per site to avoid the
		// post-build cost on tenants that don't want search. When on,
		// every successful Astro build appends a pagefind index pass
		// that writes dist/pagefind/ with the search bundle the
		// search_box block loads at runtime.
		{
			Category: "search", Key: "pagefind_enabled",
			Label:       "Pagefind on-site search",
			Description: "Run pagefind after every Astro build to generate a same-origin static search index (dist/pagefind/). Pair with the search_box block to expose the UI on a page. Adds 1-5 seconds to build time on a small marketing site; longer for content-heavy builds.",
			ValueType:   "bool",
			AgentWritable: true,
		},

		// --- payments (Sprint 2 slice C+, 2026-05-22) --------------------
		// Mollie API key + test/live mode flag drive the storefront
		// checkout. Read by OrderHandler.Checkout + .Refund + .Webhook
		// from site_settings (category=payments). AgentWritable=false
		// because Mollie keys are bearer-equivalent secrets; the agent
		// can read the descriptor (and the empty-state guidance) but
		// must point the human at the Settings -> Payments page to
		// rotate or paste a key.
		{
			Category: "payments", Key: "mollie_api_key",
			Label:       "Mollie API key",
			Description: "Per-tenant Mollie API key. Use the test key (prefix test_) for staging and the live key (prefix live_) once the storefront is ready for real payments. Get one at mollie.com -> Developers -> API keys. Required for the checkout_form block to redirect visitors to Mollie; without it the order is created with status=pending and no checkout_url. The webhook at /api/sites/{siteID}/payments/mollie/webhook is also validated against this key so a foreign payment_id cannot drive state on this tenant.",
			ValueType:   "string",
			AgentWritable: false,
		},
		{
			Category: "payments", Key: "mollie_test_mode",
			Label:       "Mollie test mode (informational)",
			Description: "Mirrors whether the configured Mollie API key is a test_ or live_ key. Set to 1 for test, 0 for live. The backend ignores this flag for routing decisions because Mollie's API endpoint is the same for both; the value is surfaced so the admin UI can show a clear test/live badge next to the orders list.",
			ValueType:   "bool",
			AgentWritable: false,
		},
	}

	defaultsByKey := map[string]string{
		"design.fidelity":                   "balanced",
		"design.strict_lint":                "1",
		"general.additional_langs":          "",
		"general.default_lang":              "",
		"general.analytics_retention_days":  "180",
		"general.consent_retention_days":    "730",
		"general.engagement_retention_days": "90",
		"general.domain_aliases":            "",
		"general.container_width":           "default",
		"general.mobile_breakpoint":         "640",
		"general.tablet_breakpoint":         "1024",
		"general.hero_layout":               "centered",
		"seo.meta_title_template":           "",
		"seo.meta_description_template":     "",
		"seo.canonical_base":                "",
		"seo.hreflang_strategy":             "path",
		"seo.og_default_image_id":           "",
		"seo.robots_txt":                    "",
		"seo.robots_txt_extra":              "",
		"seo.llms_txt":                      "",
		"seo.sitemap_enabled":               "1",
		"analytics.cookieproof_enabled":     "0",
		"analytics.cookie_banner_position":  "bottom",
		"analytics.cookie_banner_title":     "",
		"analytics.cookie_banner_description": "",
		"analytics.cookie_banner_accept":    "",
		"analytics.cookie_banner_reject":    "",
		"analytics.cookie_banner_customize": "",
		"analytics.cookie_declarations":     "",
		"analytics.cookie_cat_analytics":    "1",
		"analytics.cookie_cat_marketing":    "1",
		"analytics.cookie_cat_preferences":  "1",
		"analytics.atomicsite_tracking_enabled": "1",
		"analytics.ga4_enabled":             "0",
		"analytics.ga4_id":                  "",
		"analytics.umami_enabled":           "0",
		"analytics.umami_url":               "",
		"analytics.umami_site_id":           "",
		"analytics.crm_webhook_url":         "",
		"analytics.crm_webhook_secret":      "",
		"analytics.cookie_banner_snippet":   "",
		"analytics.personalization_enabled": "0",
		"analytics.identity_max_age_days":   "30",
		"analytics.cwv_enabled":             "0",
		"security.hsts_enabled":             "0",
		"security.hsts_max_age":             "31536000",
		"security.hsts_preload":             "0",
		"security.csp_enabled":              "0",
		"security.csp_extra_directives":     "",
		"security.frame_ancestors":          "'none'",
		"security.x_frame_options":          "SAMEORIGIN",
		"security.x_content_type_options":   "1",
		"security.referrer_policy":          "strict-origin-when-cross-origin",
		"security.permissions_policy":       "camera=(), microphone=(), geolocation=()",
		"security.coop":                     "same-origin",
		"security.corp":                     "same-origin",
		"security.coep":                     "",
		"security.x_xss_protection":         "1; mode=block",
		"security.x_permitted_cross_domain_policies": "none",
		"security.https_redirect":           "1",
		"payments.mollie_api_key":           "",
		"payments.mollie_test_mode":         "1",
		"search.pagefind_enabled":           "0",
	}

	humanAdminURL := func(category string) string {
		base := "/sites/" + siteID + "/settings/"
		switch category {
		case "design":
			// The fidelity dial + strict-lint toggle live on the General page.
			return base + "general"
		case "general":
			return base + "general"
		case "seo":
			return base + "seo"
		case "analytics":
			return base + "analytics"
		case "security":
			return base + "security"
		case "payments":
			return base + "payments"
		default:
			return base
		}
	}

	for i := range descriptors {
		d := &descriptors[i]
		fqn := d.Category + "." + d.Key
		current, present := settingsMap[fqn]
		if !present || current == "" {
			d.CurrentValue = defaultsByKey[fqn]
			d.IsDefault = true
		} else {
			d.CurrentValue = current
			d.IsDefault = false
		}
		d.HumanAdminURL = humanAdminURL(d.Category)
	}

	// Sort writable / admin-only category lists deterministically.
	// Must mirror agentWritableSettingsCategories (handlers/agent_settings.go)
	// and agentWritableCategories (mcp/helpers.go).
	writableSet := map[string]bool{"seo": true, "analytics": true, "general": true, "design": true, "search": true}
	allCats := map[string]bool{}
	for _, d := range descriptors {
		allCats[d.Category] = true
	}
	var writable, adminOnly []string
	for cat := range allCats {
		if writableSet[cat] {
			writable = append(writable, cat)
		} else {
			adminOnly = append(adminOnly, cat)
		}
	}
	sortStrings(writable)
	sortStrings(adminOnly)

	return SettingsCatalogInfo{
		WritableCategories:  writable,
		AdminOnlyCategories: adminOnly,
		Items:               descriptors,
	}
}

// sortStrings is a tiny inline sort.Strings wrapper; lets this file avoid
// importing sort directly when context.go already pulls it in.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && strings.Compare(s[j-1], s[j]) > 0; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
