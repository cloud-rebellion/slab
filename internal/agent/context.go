// Package agent provides the AI agent API logic: context building, guardrails, and operations.
package agent

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/bright-interaction/slab/internal/blocks"
	"github.com/bright-interaction/slab/internal/store"
)

// ContextBuilder assembles the full context response for an AI agent.
type ContextBuilder struct {
	queries *store.Queries
}

func NewContextBuilder(queries *store.Queries) *ContextBuilder {
	return &ContextBuilder{queries: queries}
}

// SiteContext is the full context payload returned to AI agents.
type SiteContext struct {
	Site             SiteInfo              `json:"site"`
	Structure        Structure             `json:"structure"`
	Knowledgebase    []KBEntry             `json:"knowledgebase"`
	Components       []ComponentInfo       `json:"components"`
	CSSClasses       []CSSClassInfo        `json:"css_classes"`
	// Collections lists every Custom Collection (custom content type)
	// the agent can read or write via the *_collection / *_collection_item
	// MCP tools. Schema describes field shape; item_count + locales help
	// the agent decide whether bulk-import or per-item create is the
	// right path.
	Collections []CollectionInfo `json:"collections"`
	Constraints      Constraints           `json:"constraints"`
	Architecture     ArchitectureInfo      `json:"architecture"`
	DesignReferences []DesignReferenceInfo `json:"design_references"`
	// PendingSetup lists configuration gaps the agent should resolve before
	// declaring a site "done". Empty when the site is fully configured.
	// The CLAUDE.md template tells agents to walk through this list with the
	// user as the first step of any session.
	PendingSetup []SetupTask `json:"pending_setup"`
	// Personalization tells the agent what visitor metadata the CRM has
	// pushed to this site, so it can author conditional blocks. Phase 18.4.
	Personalization PersonalizationInfo `json:"personalization"`
	// Endpoints surfaces the admin and agent API paths the agent uses
	// most often, so it doesn't need to look them up in docs. Each entry
	// names the use case alongside the HTTP method + URL template. Added
	// 2026-04-29 alongside the per-block code-view + page View Source
	// dialogs so the agent can fetch the same source the human admin
	// sees.
	Endpoints EndpointsInfo `json:"endpoints"`
	// BlockSchemas tells the agent which JSON keys each block_type
	// expects under data_json. The keys come from atomicsite's renderer
	// (internal/builder/pages.go) and the Text Mode UI's canonical text
	// field set. Use this to put copy in the right field instead of
	// inventing new keys that the renderer ignores.
	BlockSchemas []BlockSchemaInfo `json:"block_schemas"`
	// EditingModes documents the two human surfaces an authoring agent
	// shares the canvas with. The agent uses this to phrase its outputs
	// (e.g. "your headline is now editable in Text mode at /sites/{id}/
	// pages/{id}").
	EditingModes []EditingModeInfo `json:"editing_modes"`
	// SecurityPosture surfaces what the agent needs to reason about
	// embeds, scripts, and CSP without hitting a separate endpoint.
	// trusted_domains carries every active allowed_scripts row keyed by
	// kind so the agent can answer "is cal.com already trusted?"; csp
	// carries the resolved Content-Security-Policy the next build will
	// emit; generated_files names the artifacts atomicsite ships for
	// every site so the agent doesn't try to manually re-author them.
	// Added 2026-04-29 alongside the per-kind CSP allowlist + Security
	// settings UI rebuild.
	SecurityPosture SecurityPostureInfo `json:"security_posture"`
	// I18n surfaces the multi-language story for one site: default lang,
	// declared additional langs, the hreflang strategy, the meta-title
	// and -description templates, and the locale prefixes that have
	// actual published pages. Use this to author copy in the right
	// locale, to know whether you should emit hreflang as part of a
	// page (you don't, atomicsite does it for you), and to pick the
	// right slug prefix when creating a page in a non-default locale.
	I18n I18nInfo `json:"i18n"`
	// SettingsCatalog is the per-key dictionary of every setting the
	// builder, renderer, or nginx config consumes, with current value +
	// type + valid range + writable flag + a link to the human admin
	// page that owns it. Read this BEFORE writing PATCH /api/agent/settings
	// so you know what each key does, what shape its value should be, and
	// whether the agent (you) is permitted to write it. Settings that exist
	// only on the human admin UI (no backend wiring) are deliberately
	// excluded from the catalog so an agent doesn't spend tokens setting
	// values that go nowhere.
	SettingsCatalog SettingsCatalogInfo `json:"settings_catalog"`
	// EvalPlaybook is a hard-coded checklist the agent reads on first
	// session to know exactly how to converge the site to A+ on the
	// atomicsite eval engine. It connects every block-renderer field,
	// every settings_catalog key, and every pending_setup item to the
	// specific eval check it satisfies, so the agent doesn't have to
	// trial-and-error the eval to figure out what each failing check
	// wants. Updated 2026-04-30 after the first end-to-end agent
	// walkthrough surfaced 7 contradictions between documented surface
	// and handler behaviour. Treat as the spec; the eval engine is the
	// scoreboard.
	EvalPlaybook EvalPlaybookInfo `json:"eval_playbook"`
	// DesignPlaybook is the platform-level design DNA the agent reads
	// before composing pages. Where EvalPlaybook tells the agent what
	// the eval engine rewards, DesignPlaybook tells it what makes a
	// site actually look good and feel right to a human. First
	// principles, page archetypes, typography + spacing scales, block
	// taxonomy decisions ("when do I use stat_grid vs feature_grid"),
	// and the common mistakes the agent must avoid. Hard-coded so every
	// site agent gets the same DNA on first read, no per-site seeding.
	DesignPlaybook DesignPlaybookInfo `json:"design_playbook"`
}

// EvalPlaybookInfo is a structured playbook the agent reads to know the
// shape every page should take and which fields/settings drive which
// eval checks. Hard-coded in agent/context.go so every site gets it
// without per-site KB seeding.
type EvalPlaybookInfo struct {
	Goal             string          `json:"goal"`
	Mastery          MasteryInfo     `json:"mastery"`
	PageTemplate     PageTemplate    `json:"page_template"`
	HardRules        []HardRule      `json:"hard_rules"`
	BuildLoop        []string        `json:"build_loop"`
	AdminOnlyItems   []AdminOnlyItem `json:"admin_only_items"`
	VerificationGate []string        `json:"verification_gate"`
}

// MasteryInfo points the agent at the curriculum + cross-reference graph
// surfaced via MCP. Read on every connection so agents that do not pick
// the master_the_stack prompt still discover the curriculum and the
// graph; without this, the new resources would be invisible to default
// flows. The graph URI returns nodes + edges for every doc, tool,
// resource, prompt, and concept tag (mentions, tagged, ordered_after
// edges) so the agent can pick what to read instead of crawling 30+
// resources sequentially.
type MasteryInfo struct {
	GraphURI     string   `json:"graph_uri"`
	CatalogURI   string   `json:"catalog_uri"`
	ReadingOrder []string `json:"reading_order"`
	Hint         string   `json:"hint"`
}

// DesignPlaybookInfo is the agent's design DNA. Read before composing any
// page. Distilled from four in-house design skills (taste-skill,
// high-end-visual-design, minimalist-ui, redesign-existing) + Bright
// Interaction's Swiss-Minimal CRM design system + a year of
// marketing-site audits. The agent that reads this carefully should
// one-shot a B+ site that looks like a real agency built it, not
// "AI template + nice fonts". Every rule has been adapted to
// atomicsite's static-Astro reality (no React/Framer Motion, CSS
// animations only, JSON authoring instead of utility classes).
type DesignPlaybookInfo struct {
	// Fidelity is the active design-freedom dial (performance |
	// balanced | showcase) plus its contract: what it unlocks, how the
	// Inspector grades under it, and what is never relaxed. Read this
	// FIRST; the sections below are already adapted to it.
	Fidelity FidelityInfo `json:"fidelity"`

	Stack             string                  `json:"stack"`
	Principles        []DesignPrinciple       `json:"principles"`
	PageArchetypes    []PageArchetype         `json:"page_archetypes"`
	BlockSelection    []BlockSelectionRule    `json:"block_selection"`
	Typography        TypographyScale         `json:"typography"`
	Spacing           SpacingScale            `json:"spacing"`
	Color             ColorGuidance           `json:"color"`
	CommonMistakes    []DesignMistake         `json:"common_mistakes"`
	OneShotRecipe     OneShotRecipe           `json:"one_shot_recipe"`
	ReferenceRepos    []DesignReferenceRepo   `json:"reference_repos"`

	// AntiPatterns lists the AI design tells that make a site look
	// generic. Each entry pairs a banned pattern with the preferred
	// alternative AND the atomicsite-specific way to apply it. Read
	// before every site build, nothing else in this playbook helps
	// if the output ships an AI tell.
	AntiPatterns []AntiPattern `json:"anti_patterns"`

	// VibeArchetypes are the three high-level aesthetic identities
	// the agent picks ONE of per site. Picking commits the agent to
	// a coherent palette + typography + materiality choice, killing
	// the "soup of patterns" that ships when an agent borrows from
	// every reference at once.
	VibeArchetypes []VibeArchetype `json:"vibe_archetypes"`

	// Materiality describes how surfaces look, borders, shadows,
	// glass, double-bezel patterns. Atomicsite's renderer ships
	// most of these as defaults; this section tells the agent
	// what's already wired up so it doesn't try to invent custom CSS.
	Materiality MaterialityGuidance `json:"materiality"`

	// ContentAuthenticity is the rule set against AI slop copy.
	// "John Doe", "99.99%", "Acme Corp", "Elevate", "Seamless", all
	// banned. The agent reads this BEFORE writing copy, not after.
	ContentAuthenticity ContentRules `json:"content_authenticity"`

	// MotionGuidance is the agent's motion-design rules. Atomicsite
	// is static Astro, no Framer Motion, no scroll listeners. CSS
	// animations only, prefers-reduced-motion respected, transform
	// + opacity only.
	Motion MotionGuidance `json:"motion"`

	// StrategicOmissions is the checklist of things AI typically
	// forgets. Skip-to-content, custom 404, form validation,
	// favicon, legal links, cookie consent, "back" navigation.
	// Atomicsite handles many automatically, this section tells
	// the agent which ones it gets for free vs which need explicit
	// authorship.
	StrategicOmissions []OmissionItem `json:"strategic_omissions"`

	// AuditChecklist is the final pre-flight checklist the agent
	// runs against its own output before declaring the site done.
	// Mirrors redesign-existing's audit but scoped to atomicsite
	// authoring (block_type + data + settings, not arbitrary CSS).
	AuditChecklist []AuditItem `json:"audit_checklist"`

	// IconPolicy says which icon set to use, banned strokes, and how
	// to wire icons through atomicsite's icons.go dictionary.
	IconPolicy IconRules `json:"icon_policy"`

	// CopyVoice gives the agent a tight, opinionated voice rule set
	// for writing eyebrows, headlines, subheadings, and CTAs.
	// Distilled from Bright Interaction's voice guidelines + the
	// taste-skill content rules.
	CopyVoice VoiceRules `json:"copy_voice"`

	// Fonts documents atomicsite's font system: self-hosted woff2 only,
	// per-site uploads, admin UI path, recommended families with
	// download sources, system fallback behaviour. The agent reads
	// this BEFORE setting branding.font_heading or font_body, picking
	// a family that's not uploaded results in system-ui fallback.
	Fonts FontGuidance `json:"fonts"`

	// StackRecommendations names the four canonical site stacks
	// atomicsite supports: pure static Astro (default), Astro + Svelte
	// islands (light interactivity), Astro + Svelte + headless commerce
	// (ecom), and Astro + Svelte + Stripe Checkout (paid digital
	// goods). Tells the agent which to pick AND which payment provider
	// (Stripe vs Mollie) to wire based on customer geography.
	StackRecommendations StackGuidance `json:"stack_recommendations"`

	// HeroGraphics is the curated set of hero-visual options the
	// renderer ships. Picked via hero.hero_graphic / split_hero.hero_graphic.
	// Each is pre-vetted for inspector grading (LCP, contrast, motion).
	// Lets the agent pick a coherent visual instead of inventing custom
	// markup that drifts from the design tokens.
	HeroGraphics []HeroGraphic `json:"hero_graphics"`

	// DesignWorkflow tells the agent which installed design skill to
	// invoke for each vibe before writing custom HTML/CSS. Cross-links
	// taste-skill, high-end-visual-design, stitch-design, minimalist-ui
	// to atomicsite vibes.
	DesignWorkflow DesignWorkflow `json:"design_workflow"`
}

// DesignPrinciple is a first-principles design rule with a concrete
// "how-to-apply-via-atomicsite" mapping so the agent isn't left
// translating abstract advice into block fields.
type DesignPrinciple struct {
	Name        string `json:"name"`
	Rule        string `json:"rule"`
	WhyItMatters string `json:"why"`
	HowToApply   string `json:"how"`
}

// PageArchetype is a recommended block sequence for one common page
// type. Agents pick an archetype, instantiate it, then customize copy.
// Each block entry names the block_type the agent should create_block
// against, plus a one-line "why this slot."
type PageArchetype struct {
	Name        string                `json:"name"`
	Use         string                `json:"use"`
	Description string                `json:"description"`
	Blocks      []ArchetypeBlock      `json:"blocks"`
}

type ArchetypeBlock struct {
	BlockType string `json:"block_type"`
	Role      string `json:"role"`
	Notes     string `json:"notes"`
}

// BlockSelectionRule resolves "which block_type should I use?" decisions
// the agent commonly faces. Phrased as a question the agent might ask
// itself, with the decision criterion that picks the right primitive.
type BlockSelectionRule struct {
	Question string `json:"question"`
	Choose   string `json:"choose"`
	Why      string `json:"why"`
}

// TypographyScale documents the rhythm the CSS pipeline emits via
// --font-heading / --font-body / --font-mono. Sizes are clamp() values
// the renderer ships, expressed in rem so the agent knows the ratios
// without inspecting CSS.
type TypographyScale struct {
	HeadingFont      string         `json:"heading_font"`
	BodyFont         string         `json:"body_font"`
	MonoFont         string         `json:"mono_font"`
	Scale            []TypeStep     `json:"scale"`
	UsageRules       []string       `json:"usage_rules"`
}

type TypeStep struct {
	Element string `json:"element"`
	Size    string `json:"size"`
	Weight  string `json:"weight"`
	Use     string `json:"use"`
}

// SpacingScale documents block padding, container max-widths, and the
// rhythm between sections so the agent knows what's already wired into
// the renderer (and shouldn't try to fight via custom CSS).
type SpacingScale struct {
	ContainerWidths   map[string]string `json:"container_widths"`
	BlockPadding      map[string]string `json:"block_padding"`
	SectionRhythm     []string          `json:"section_rhythm"`
}

// ColorGuidance maps the CSS custom properties to "what they're for."
// Atomicsite emits --color-primary / --color-text / --color-bg /
// --color-surface-elevated and others; this tells the agent which one
// belongs in which UI moment.
type ColorGuidance struct {
	Tokens          []ColorToken `json:"tokens"`
	UsageRules      []string     `json:"usage_rules"`
}

type ColorToken struct {
	Name string `json:"name"`
	Use  string `json:"use"`
}

type DesignMistake struct {
	Mistake  string `json:"mistake"`
	Symptom  string `json:"symptom"`
	Fix      string `json:"fix"`
}

// OneShotRecipe is the canonical block sequence for a B2B SaaS marketing
// homepage that scores B+ on the eval and looks intentional. Used as
// the default when the agent has no other guidance.
type OneShotRecipe struct {
	Description string             `json:"description"`
	Blocks      []ArchetypeBlock   `json:"blocks"`
	Settings    []string           `json:"settings"`
}

// DesignReferenceRepo points at one of the curated repos checked into
// automations/design-references/. Agents reading this know which folder
// to inspect for production-grade Astro+Tailwind patterns.
type DesignReferenceRepo struct {
	Path        string   `json:"path"`
	Stack       string   `json:"stack"`
	BestFor     []string `json:"best_for"`
	License     string   `json:"license"`
	Notes       string   `json:"notes"`
}

// AntiPattern is one banned AI-design tell with its preferred replacement
// and how to apply that replacement through atomicsite primitives.
type AntiPattern struct {
	Banned     string `json:"banned"`
	Preferred  string `json:"preferred"`
	HowInAtomicsite string `json:"how_in_atomicsite"`
}

// VibeArchetype names one of three coherent aesthetic identities. The
// agent picks ONE per site, never mixes, and follows its palette,
// typography, materiality choices through every block.
type VibeArchetype struct {
	Name         string   `json:"name"`
	BestFor      []string `json:"best_for"`
	Palette      string   `json:"palette"`
	Typography   string   `json:"typography"`
	Materiality  string   `json:"materiality"`
	BgColor      string   `json:"bg_color"`
	PrimaryColor string   `json:"primary_color"`
	TextColor    string   `json:"text_color"`
	FontHeading  string   `json:"font_heading"`
	FontBody     string   `json:"font_body"`
	ApplyVia     string   `json:"apply_via"`
}

// MaterialityGuidance describes the surface treatments atomicsite
// supports + the patterns the agent should reach for vs avoid.
type MaterialityGuidance struct {
	Defaults  []string `json:"defaults"`
	DoUse     []string `json:"do_use"`
	DontUse   []string `json:"dont_use"`
	HowToApply []string `json:"how_to_apply"`
}

// ContentRules is the AI-slop firewall for copy.
type ContentRules struct {
	BannedNames     []string `json:"banned_names"`
	BannedNumbers   []string `json:"banned_numbers"`
	BannedCompanies []string `json:"banned_companies"`
	BannedPhrases   []string `json:"banned_phrases"`
	StyleRules      []string `json:"style_rules"`
	HowInAtomicsite string   `json:"how_in_atomicsite"`
}

// MotionGuidance scopes animation to atomicsite reality (CSS only, no JS).
type MotionGuidance struct {
	StackReality string   `json:"stack_reality"`
	DoUse        []string `json:"do_use"`
	DontUse      []string `json:"dont_use"`
	Performance  []string `json:"performance"`
	A11y         []string `json:"a11y"`
}

// OmissionItem is one thing AI typically forgets, and whether
// atomicsite handles it automatically or needs explicit authorship.
type OmissionItem struct {
	Item       string `json:"item"`
	Importance string `json:"importance"`
	Status     string `json:"status"`
	HowApplied string `json:"how_applied"`
}

// AuditItem is one final-pass check the agent runs before claiming the
// site is done.
type AuditItem struct {
	Check string `json:"check"`
	Why   string `json:"why"`
}

// HeroGraphic is one curated hero-visual the renderer ships. Picked via
// the hero/split_hero block's hero_graphic field; each name maps to a
// CSS+SVG fragment in builder/pages.go. Curated set (replaces ad-hoc
// custom-block heroes) so every hero is pre-vetted against the
// inspector for performance, accessibility, and brand coherence.
type HeroGraphic struct {
	Name        string   `json:"name"`
	BestFor     []string `json:"best_for"`
	Description string   `json:"description"`
	Materiality string   `json:"materiality"`
	Performance string   `json:"performance"`
}

// DesignWorkflow cross-links the harness-installed design skills to the
// atomicsite vibe archetypes. The agent reads this BEFORE composing a
// hero or styling a page so it invokes the right skill (taste-skill,
// high-end-visual-design, stitch-design, minimalist-ui) instead of
// reinventing patterns inline.
type DesignWorkflow struct {
	WhenToInvoke   string             `json:"when_to_invoke"`
	SkillsByVibe   []SkillVibeMapping `json:"skills_by_vibe"`
	BeforeAuthoring []string          `json:"before_authoring"`
}

type SkillVibeMapping struct {
	Vibe   string `json:"vibe"`
	Skill  string `json:"skill"`
	HowToInvoke string `json:"how_to_invoke"`
}

// IconRules tells the agent which icons to use and which to avoid.
type IconRules struct {
	UseSet      string   `json:"use_set"`
	StrokeWidth string   `json:"stroke_width"`
	Banned      []string `json:"banned"`
	Available   []string `json:"available"`
	HowToUse    string   `json:"how_to_use"`
}

// VoiceRules is the copywriting voice for marketing pages.
type VoiceRules struct {
	Tone       string   `json:"tone"`
	Eyebrow    []string `json:"eyebrow"`
	Headline   []string `json:"headline"`
	Subheading []string `json:"subheading"`
	CTA        []string `json:"cta"`
	Forbidden  []string `json:"forbidden"`
}

// FontGuidance tells the agent how atomicsite's font system works,
// where to upload fonts in the admin UI, how to list / register
// fonts via the agent API, and which families are good picks for
// each archetype + how to obtain the woff2 files (all SIL OFL 1.1
// licensed, free to self-host).
type FontGuidance struct {
	Philosophy       string             `json:"philosophy"`
	System           []string           `json:"system"`
	APIEndpoints     []FontEndpoint     `json:"api_endpoints"`
	AdminUI          []string           `json:"admin_ui"`
	UploadFlow       []string           `json:"upload_flow"`
	Recommended      []FontFamily       `json:"recommended"`
	SystemFallback   string             `json:"system_fallback"`
	HowToSet         string             `json:"how_to_set"`
}

type FontEndpoint struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Use    string `json:"use"`
}

type FontFamily struct {
	Name        string   `json:"name"`
	GoodFor     []string `json:"good_for"`
	License     string   `json:"license"`
	DownloadFrom string  `json:"download_from"`
	Notes       string   `json:"notes"`
}

// StackGuidance documents the four canonical stacks atomicsite
// supports + a decision tree for picking one + payment-provider rules.
type StackGuidance struct {
	Philosophy string         `json:"philosophy"`
	Stacks     []StackVariant `json:"stacks"`
	Payments   PaymentRules   `json:"payments"`
	WhenToPick []StackPick    `json:"when_to_pick"`
}

type StackVariant struct {
	Name        string   `json:"name"`
	Use         string   `json:"use"`
	Composition []string `json:"composition"`
	Constraints []string `json:"constraints"`
	HowApplied  string   `json:"how_applied"`
}

type StackPick struct {
	IfSiteIs string `json:"if_site_is"`
	Pick     string `json:"pick"`
	Why      string `json:"why"`
}

type PaymentRules struct {
	Philosophy string             `json:"philosophy"`
	Providers  []PaymentProvider  `json:"providers"`
	WhenToPick []PaymentPick      `json:"when_to_pick"`
	HowApplied string             `json:"how_applied"`
}

type PaymentProvider struct {
	Name        string   `json:"name"`
	BestFor     []string `json:"best_for"`
	Strengths   []string `json:"strengths"`
	Weaknesses  []string `json:"weaknesses"`
	Geography   string   `json:"geography"`
	Methods     []string `json:"methods"`
	Pricing     string   `json:"pricing"`
}

type PaymentPick struct {
	IfCustomerIs string `json:"if_customer_is"`
	Pick         string `json:"pick"`
	Why          string `json:"why"`
}

// PageTemplate is the canonical block sequence the eval engine rewards.
// Use as the default when creating a fresh content page.
type PageTemplate struct {
	Description string             `json:"description"`
	Blocks      []TemplateBlock    `json:"blocks"`
}

type TemplateBlock struct {
	BlockType string `json:"block_type"`
	Required  bool   `json:"required"`
	Notes     string `json:"notes"`
}

type HardRule struct {
	Rule      string `json:"rule"`
	WhyItMatters string `json:"why"`
	HowToApply   string `json:"how"`
}

type AdminOnlyItem struct {
	Setting       string `json:"setting"`
	HumanAdminURL string `json:"human_admin_url"`
	EvalImpact    string `json:"eval_impact"`
}

// I18nInfo is the agent-facing summary of the site's multi-language
// configuration. Read-only context; the agent writes general /
// additional_langs / seo.hreflang_strategy via PATCH /api/agent/settings
// (general + seo categories are agent-writable).
type I18nInfo struct {
	// DefaultLang is the site's primary language (sites.lang). Pages
	// without a /<lang>/ prefix in their slug belong to this locale.
	DefaultLang string `json:"default_lang"`
	// AdditionalLangs is the operator-declared CSV expanded into the
	// list of other languages the site publishes (general.additional_langs).
	AdditionalLangs []string `json:"additional_langs"`
	// Strategy is "path" (default), "subdomain", or "off". Atomicsite
	// optimizes for path-based; subdomain is opt-in for sites already
	// running sv.example.com etc; off disables hreflang emission.
	Strategy string `json:"hreflang_strategy"`
	// LocaleRoots are the actual locale prefixes that have published
	// pages right now (e.g. ["", "/sv"] when /about + /sv/about exist).
	// Use this to pick the right slug prefix when creating a page in a
	// new locale: append the lang code to the prefix list with a slash.
	LocaleRoots []string `json:"locale_roots"`
	// MetaTitleTemplate is seo.meta_title_template applied to every
	// page that doesn't override its own meta_title. Tokens:
	// {page_title}, {site_name}, {lang}, {page_description},
	// {separator}.
	MetaTitleTemplate string `json:"meta_title_template"`
	// MetaDescriptionTemplate is seo.meta_description_template; same
	// tokens as MetaTitleTemplate.
	MetaDescriptionTemplate string `json:"meta_description_template"`
	// CanonicalBase overrides the default https://{domain}. Useful when
	// the public URL differs from the build's domain (CDN, sub-path
	// proxy).
	CanonicalBase string `json:"canonical_base"`
}

// SecurityPostureInfo is the agent-facing summary of the security
// surface for one site. Read-only: the agent cannot mutate any of these
// directly; CSP-modifying writes (allowed_scripts, security settings)
// require the human admin so the AI can never silently widen the attack
// surface. The agent uses this view to (a) avoid asking for an embed
// that already works and (b) tell the human exactly what to whitelist
// when an iframe is needed.
type SecurityPostureInfo struct {
	// TrustedDomains lists every active allowed_scripts row, keyed by
	// kind, so the agent can scan "is cal.com in the frame list?" in
	// one pass.
	TrustedDomains TrustedDomainsByKind `json:"trusted_domains"`
	// AllowedKinds is the closed set of valid kind values, mirrored
	// from internal/handlers/allowed_scripts.go:AllowedKinds.
	AllowedKinds []string `json:"allowed_kinds"`
	// CSP is the resolved Content-Security-Policy the next build will
	// emit (computed via builder.BuildSecurityHeaders).
	CSP string `json:"csp"`
	// HSTS is the resolved Strict-Transport-Security header value.
	HSTS string `json:"hsts"`
	// FrameAncestors is the current frame-ancestors directive value
	// (defaults to 'none', which forbids being embedded).
	FrameAncestors string `json:"frame_ancestors"`
	// HumanAdminURL is the path the agent should point the user at
	// when an iframe / image / script host needs whitelisting.
	HumanAdminURL string `json:"human_admin_url"`
	// GeneratedFiles names every artifact atomicsite emits on every
	// build so the agent doesn't try to re-create them.
	GeneratedFiles []GeneratedFileInfo `json:"generated_files"`
}

// TrustedDomainsByKind groups active allowed_scripts entries by the
// CSP directive they feed.
type TrustedDomainsByKind struct {
	Script  []string `json:"script"`
	Frame   []string `json:"frame"`
	Image   []string `json:"image"`
	Media   []string `json:"media"`
	Connect []string `json:"connect"`
	All     []string `json:"all"`
}

// GeneratedFileInfo names one artifact atomicsite auto-ships.
type GeneratedFileInfo struct {
	Path        string `json:"path"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// EndpointInfo names one URL template the agent calls. Method is the HTTP
// verb; Path uses {var} placeholders matching chi route params. Use lists
// the persona-level intent (humans + agent share these endpoints).
type EndpointInfo struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Use    string `json:"use"`
}

// EndpointsInfo groups EndpointInfo entries by area so the agent can
// scan the relevant set quickly.
type EndpointsInfo struct {
	Pages          []EndpointInfo `json:"pages"`
	Blocks         []EndpointInfo `json:"blocks"`
	GlobalBlocks   []EndpointInfo `json:"global_blocks"`
	Media          []EndpointInfo `json:"media"`
	Build          []EndpointInfo `json:"build"`
	Preview        []EndpointInfo `json:"preview"`
	Security       []EndpointInfo `json:"security"`
	TrustedDomains []EndpointInfo `json:"trusted_domains"`
}

// BlockSchemaInfo describes the keys data_json should carry for one
// block_type. Use this as the source of truth for "where does the
// headline go" when authoring a block.
type BlockSchemaInfo struct {
	BlockType string             `json:"block_type"`
	Use       string             `json:"use"`
	TextKeys  []BlockSchemaField `json:"text_keys"`
	OtherKeys []BlockSchemaField `json:"other_keys"`
}

// BlockSchemaField is one key inside a block's data_json: its name, a
// short label for human display, and whether multiline copy is expected
// (multiline keys belong in textareas; single-line keys are short copy
// like CTAs and titles).
type BlockSchemaField struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Multiline bool   `json:"multiline"`
}

// EditingModeInfo documents one of the two authoring surfaces the agent
// shares with humans on the page editor.
type EditingModeInfo struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	Description string `json:"description"`
}

// PersonalizationInfo summarises what the agent needs to author conditional
// blocks: whether the feature is enabled, the identity-freshness window,
// the actual metadata keys the CRM has been pushing, and a few example
// conditions to seed pattern recognition.
type PersonalizationInfo struct {
	Enabled             bool     `json:"enabled"`
	IdentityMaxAgeDays  int      `json:"identity_max_age_days"`
	KnownKeys           []string `json:"known_keys"`
	ExampleConditions   []string `json:"example_conditions"`
}

// SetupTask represents one configuration gap the agent should close. The
// agent reads this list, asks the user when needed, and calls the listed
// endpoint to apply the fix.
type SetupTask struct {
	// ID is a stable string the agent can match on (e.g. across two consecutive
	// /api/agent/context calls).
	ID string `json:"id"`
	// Category groups related tasks: branding, profile, seo, analytics,
	// content, media.
	Category string `json:"category"`
	// Title is a one-line description of what is missing.
	Title string `json:"title"`
	// Why explains the consequence of leaving this unfixed (eval impact,
	// compliance, conversion).
	Why string `json:"why"`
	// Action describes what the agent should do.
	Action string `json:"action"`
	// Endpoint is the primary API path the agent should call to resolve.
	Endpoint string `json:"endpoint"`
	// Severity is "required" (blocks an A+ build) or "recommended"
	// (improves quality but the build will still pass without it).
	Severity string `json:"severity"`
}

// DesignReferenceInfo surfaces a fetched GitHub bundle so the AI agent
// has the user's preferred design vocabulary to compose with. The bundle
// is read-only pattern reference, not a code-copy mechanism.
type DesignReferenceInfo struct {
	URL     string         `json:"url"`
	Label   string         `json:"label"`
	RefType string         `json:"ref_type"`
	Bundle  map[string]any `json:"bundle"`
}

type SiteInfo struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Domain   string   `json:"domain"`
	Lang     string   `json:"lang"`
	Status   string   `json:"status"`
	Branding Branding `json:"branding"`
	SEO      SEOInfo  `json:"seo"`
}

type Branding struct {
	PrimaryColor   string `json:"primary_color"`
	SecondaryColor string `json:"secondary_color"`
	BgColor        string `json:"bg_color"`
	TextColor      string `json:"text_color"`
	// Phase 12.9: extra colour slots so the agent has the same palette
	// the human admin edits in the Branding UI. accent_color = "" means
	// "fall back to primary_color" (preserved by the builder).
	SurfaceColor   string `json:"surface_color"`
	BorderColor    string `json:"border_color"`
	MutedColor     string `json:"muted_color"`
	AccentColor    string `json:"accent_color"`
	OnPrimaryColor string `json:"on_primary_color"`
	FontHeading    string `json:"font_heading"`
	FontBody       string `json:"font_body"`
}

type SEOInfo struct {
	MetaTitle       string `json:"meta_title"`
	MetaDescription string `json:"meta_description"`
	OgImageID       string `json:"og_image_id"`
	FaviconID       string `json:"favicon_id"`
}

type Structure struct {
	Pages        []PageInfo       `json:"pages"`
	GlobalBlocks []GlobalBlockInfo `json:"global_blocks"`
	Silos        []SiloInfo       `json:"silos"`
}

type PageInfo struct {
	ID               string      `json:"id"`
	Title            string      `json:"title"`
	Slug             string      `json:"slug"`
	Status           string      `json:"status"`
	Layout           string      `json:"layout"`
	SortOrder        int64       `json:"sort_order"`
	ShowInNav        bool        `json:"show_in_nav"`
	NavLabel         string      `json:"nav_label"`
	HideGlobalBlocks bool        `json:"hide_global_blocks"`
	Blocks           []BlockInfo `json:"blocks"`
}

type BlockInfo struct {
	ID        string `json:"id"`
	BlockType string `json:"block_type"`
	SortOrder int64  `json:"sort_order"`
	DataJSON  any    `json:"data"`
	StyleJSON any    `json:"style"`
	IsVisible bool   `json:"is_visible"`
}

type GlobalBlockInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Slot      string `json:"slot"`
	BlockType string `json:"block_type"`
	DataJSON  any    `json:"data"`
	StyleJSON any    `json:"style"`
	IsActive  bool   `json:"is_active"`
}

type SiloInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	SlugPrefix string `json:"slug_prefix"`
	SortOrder  int64  `json:"sort_order"`
}

type KBEntry struct {
	Category string `json:"category"`
	Title    string `json:"title"`
	Content  string `json:"content"`
}

type ComponentInfo struct {
	Name       string `json:"name"`
	Category   string `json:"category"`
	PropsSchema any   `json:"props_schema"`
	CSSClasses []string `json:"css_classes"`
	UsageNote  string `json:"usage_note"`
}

type CSSClassInfo struct {
	Name      string `json:"name"`
	Category  string `json:"category"`
	CSS       string `json:"css"`
	UsageNote string `json:"usage_note"`
}

// CollectionInfo summarises one Custom Collection for the agent
// context. Sprint 4 (2026-05-04). The agent uses this to author
// items via the bulk_import_collection_items MCP tool without a
// separate round-trip to /api/sites/{id}/collections.
type CollectionInfo struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Slug          string         `json:"slug"`
	Schema        any            `json:"schema"`
	Settings      map[string]any `json:"settings"`
	ItemCount     int64          `json:"item_count"`
	Locales       []string       `json:"locales"`
	SchemaOrgType string         `json:"schema_org_type,omitempty"`
	RenderAsPages bool           `json:"render_as_pages"`
}

type Constraints struct {
	AllowedBlockTypes  []string `json:"allowed_block_types"`
	ForbiddenPatterns  []string `json:"forbidden_patterns"`
	AllowedHosts       []string `json:"allowed_hosts"`
	MaxBlocksPerPage   int      `json:"max_blocks_per_page"`
	MaxURLDepth        int      `json:"max_url_depth"`
	RequiredBlocks     map[string][]string `json:"required_blocks"`
	// ActiveDesignFidelity mirrors the design.fidelity setting so the
	// agent sees the dial at top level (the full contract lives in
	// design_playbook.fidelity).
	ActiveDesignFidelity string `json:"active_design_fidelity"`
}

type ArchitectureInfo struct {
	StructureType string `json:"structure_type"`
	MaxDepth      int    `json:"max_depth"`
}

// Build assembles the complete context for a site.
func (b *ContextBuilder) Build(ctx context.Context, siteID string) (*SiteContext, error) {
	site, err := b.queries.GetSiteByID(ctx, siteID)
	if err != nil {
		return nil, err
	}

	pages, err := b.queries.ListPagesBySite(ctx, siteID)
	if err != nil {
		return nil, err
	}

	// Audit H1: fetch every block for the site in one query, then group
	// by page_id in Go. Was 51 round-trips for 50 pages (1 list +
	// per-page ListBlocksByPage); now 2 round-trips total regardless of
	// page count. The new ListBlocksBySite query joins blocks→pages so
	// the site_id filter is enforced at the SQL layer.
	allBlocks, err := b.queries.ListBlocksBySite(ctx, siteID)
	if err != nil {
		return nil, err
	}
	blocksByPage := make(map[string][]store.ListBlocksBySiteRow, len(pages))
	for _, bl := range allBlocks {
		blocksByPage[bl.PageID] = append(blocksByPage[bl.PageID], bl)
	}

	var pageInfos []PageInfo
	for _, p := range pages {
		blocks := blocksByPage[p.ID]
		var blockInfos []BlockInfo
		for _, bl := range blocks {
			blockInfos = append(blockInfos, BlockInfo{
				ID:        bl.ID,
				BlockType: bl.BlockType,
				SortOrder: bl.SortOrder,
				DataJSON:  parseJSONField(bl.DataJson),
				StyleJSON: parseJSONField(bl.StyleJson),
				IsVisible: bl.IsVisible == 1,
			})
		}
		pageInfos = append(pageInfos, PageInfo{
			ID:               p.ID,
			Title:            p.Title,
			Slug:             p.Slug,
			Status:           p.Status,
			Layout:           p.Layout,
			SortOrder:        p.SortOrder,
			ShowInNav:        p.ShowInNav == 1,
			NavLabel:         p.NavLabel,
			HideGlobalBlocks: p.HideGlobalBlocks == 1,
			Blocks:           blockInfos,
		})
	}

	globalBlocks, err := b.queries.ListActiveGlobalBlocksBySite(ctx, siteID)
	if err != nil {
		return nil, err
	}
	var gbInfos []GlobalBlockInfo
	for _, gb := range globalBlocks {
		gbInfos = append(gbInfos, GlobalBlockInfo{
			ID:        gb.ID,
			Name:      gb.Name,
			Slot:      gb.Slot,
			BlockType: gb.BlockType,
			DataJSON:  parseJSONField(gb.DataJson),
			StyleJSON: parseJSONField(gb.StyleJson),
			IsActive:  gb.IsActive == 1,
		})
	}

	silos, err := b.queries.ListSilosBySite(ctx, siteID)
	if err != nil {
		return nil, err
	}
	var siloInfos []SiloInfo
	for _, s := range silos {
		siloInfos = append(siloInfos, SiloInfo{
			ID:         s.ID,
			Name:       s.Name,
			SlugPrefix: s.SlugPrefix,
			SortOrder:  s.SortOrder,
		})
	}

	kbEntries, err := b.queries.ListActiveKnowledgebaseBySite(ctx, siteID)
	if err != nil {
		return nil, err
	}
	var kbInfos []KBEntry
	for _, kb := range kbEntries {
		kbInfos = append(kbInfos, KBEntry{
			Category: kb.Category,
			Title:    kb.Title,
			Content:  kb.Content,
		})
	}

	components, err := b.queries.ListComponentsBySite(ctx, siteID)
	if err != nil {
		return nil, err
	}
	var compInfos []ComponentInfo
	for _, c := range components {
		var cssClasses []string
		_ = json.Unmarshal([]byte(c.CssClasses), &cssClasses)
		compInfos = append(compInfos, ComponentInfo{
			Name:        c.Name,
			Category:    c.Category,
			PropsSchema: parseJSONField(c.PropsSchema),
			CSSClasses:  cssClasses,
			UsageNote:   c.UsageNote,
		})
	}

	cssClasses, err := b.queries.ListCSSClassesBySite(ctx, siteID)
	if err != nil {
		return nil, err
	}
	var cssInfos []CSSClassInfo
	for _, cc := range cssClasses {
		cssInfos = append(cssInfos, CSSClassInfo{
			Name:      cc.Name,
			Category:  cc.Category,
			CSS:       cc.Css,
			UsageNote: cc.UsageNote,
		})
	}

	// Sprint 4 (2026-05-04): Custom Collections summary.
	var collectionInfos []CollectionInfo
	if cols, err := b.queries.ListCollectionsBySite(ctx, siteID); err == nil {
		for _, c := range cols {
			var schema any
			_ = json.Unmarshal([]byte(c.SchemaJson), &schema)
			var settings map[string]any
			_ = json.Unmarshal([]byte(c.SettingsJson), &settings)
			if settings == nil {
				settings = map[string]any{}
			}
			itemCount, _ := b.queries.CountItemsByCollection(ctx, c.ID)
			locales, _ := b.queries.ListLocalesByCollection(ctx, c.ID)
			schemaOrgType, _ := settings["schema_org_type"].(string)
			renderAsPages, _ := settings["render_as_pages"].(bool)
			collectionInfos = append(collectionInfos, CollectionInfo{
				ID:            c.ID,
				Name:          c.Name,
				Slug:          c.Slug,
				Schema:        schema,
				Settings:      settings,
				ItemCount:     itemCount,
				Locales:       locales,
				SchemaOrgType: schemaOrgType,
				RenderAsPages: renderAsPages,
			})
		}
	}

	constraints := b.buildConstraints(ctx, siteID)

	// Design references: pull every saved GitHub bundle so the agent can
	// borrow vocabulary (component shapes, naming, tailwind config). The
	// bundle JSON is whatever the design-references handler stored at
	// fetch / refresh time. Failures here are non-fatal: an agent without
	// references is still a useful agent.
	var refInfos []DesignReferenceInfo
	if refRows, err := b.queries.ListDesignReferences(ctx, siteID); err == nil {
		for _, r := range refRows {
			var bundle map[string]any
			_ = json.Unmarshal([]byte(r.FetchedJson), &bundle)
			refInfos = append(refInfos, DesignReferenceInfo{
				URL:     r.Url,
				Label:   r.Label,
				RefType: r.RefType,
				Bundle:  bundle,
			})
		}
	}

	arch, _ := b.queries.GetSiteArchitecture(ctx, siteID)
	archInfo := ArchitectureInfo{
		StructureType: "soft-silo",
		MaxDepth:      3,
	}
	if arch.ID != "" {
		archInfo.StructureType = arch.StructureType
		archInfo.MaxDepth = int(arch.MaxDepth)
	}

	pending := b.computePendingSetup(ctx, siteID, site, pageInfos)

	// One settings load shared by SettingsCatalog (and any future
	// context-level consumers). The per-feature compute* functions still
	// load their own slice for now; not worth refactoring those today.
	settingsRows, _ := b.queries.ListSettingsBySite(ctx, siteID)
	settingMap := make(map[string]string, len(settingsRows))
	for _, s := range settingsRows {
		settingMap[s.Category+"."+s.Key] = s.Value
	}
	personalization := b.computePersonalization(ctx, siteID)

	// Design fidelity adapts the playbooks the agent reads AND the
	// rubric the critique engine grades against (same choke point), so
	// guidance and grading always move together.
	fidelity := FidelityFromSettings(settingMap)
	constraints.ActiveDesignFidelity = string(fidelity)

	return &SiteContext{
		Site: SiteInfo{
			ID:     site.ID,
			Name:   site.Name,
			Domain: site.Domain,
			Lang:   site.Lang,
			Status: site.Status,
			Branding: Branding{
				PrimaryColor:   site.PrimaryColor,
				SecondaryColor: site.SecondaryColor,
				BgColor:        site.BgColor,
				TextColor:      site.TextColor,
				SurfaceColor:   site.SurfaceColor,
				BorderColor:    site.BorderColor,
				MutedColor:     site.MutedColor,
				AccentColor:    site.AccentColor,
				OnPrimaryColor: site.OnPrimaryColor,
				FontHeading:    site.FontHeading,
				FontBody:       site.FontBody,
			},
			SEO: SEOInfo{
				MetaTitle:       site.MetaTitle,
				MetaDescription: site.MetaDescription,
				OgImageID:       site.OgImageID,
				FaviconID:       site.FaviconID,
			},
		},
		Structure: Structure{
			Pages:        pageInfos,
			GlobalBlocks: gbInfos,
			Silos:        siloInfos,
		},
		Knowledgebase:    kbInfos,
		Components:       compInfos,
		CSSClasses:       cssInfos,
		Collections:      collectionInfos,
		Constraints:      constraints,
		Architecture:     archInfo,
		DesignReferences: refInfos,
		PendingSetup:     pending,
		Personalization:  personalization,
		Endpoints:        defaultEndpoints(),
		BlockSchemas:     defaultBlockSchemas(),
		EditingModes:     defaultEditingModes(),
		SecurityPosture:  b.computeSecurityPosture(ctx, siteID),
		I18n:             b.computeI18n(ctx, siteID, site, pageInfos),
		SettingsCatalog:  buildSettingsCatalog(siteID, settingMap),
		EvalPlaybook:     EvalPlaybookFor(siteID, fidelity),
		DesignPlaybook:   DesignPlaybookFor(fidelity),
	}, nil
}

// defaultEvalPlaybook returns the hard-coded agent playbook for converging
// a site to A+ on the atomicsite eval engine. Connects every block-renderer
// field, every settings_catalog key, and every pending_setup item to the
// eval check it satisfies, so an agent reading the context can plan the
// whole setup pass without trial-and-error against the eval.
func defaultEvalPlaybook(siteID string) EvalPlaybookInfo {
	return EvalPlaybookInfo{
		Mastery: MasteryInfo{
			GraphURI:   "atomicsite://meta/knowledge-graph",
			CatalogURI: "atomicsite://knowledge/index",
			ReadingOrder: []string{
				// Stack mastery first: how the builder emits Astro + TS + custom CSS,
				// the block registry vocabulary, the Collection / schema-org layer
				// added in Sprint 4, then the i18n / security / privacy boundaries.
				"astro-conventions",
				"typescript-strict",
				"css-variable-system",
				"block-renderer-patterns",
				"collection-design-patterns",
				"schema-org-per-collection-type",
				"i18n-authoring",
				"security-authoring",
				"personalization",
				"cookieproof-integration",
				// UX mastery: foundational tokens before composition, accessibility before motion,
				// performance before forms, premium-design-principles last as the synthesis.
				"typography-scale",
				"color-system",
				"spacing-rhythm",
				"motion-curves",
				"accessibility-patterns",
				"performance-budgets",
				"forms-ux",
				"nav-ux",
				"dark-mode",
				"premium-design-principles",
			},
			Hint: "Read atomicsite://meta/knowledge-graph FIRST on session start. It returns nodes + edges for every doc, tool, resource, prompt, and concept tag, so you can pick what's relevant in one fetch instead of crawling 30+ resources. Follow up with atomicsite://knowledge/<slug> for full bodies, and tools/list / resources/list for the live MCP surface. The reading_order above is the deterministic curriculum sequence the master_the_stack prompt walks; honor it when picking docs to read.",
		},
		Goal: "Atomicsite is a website builder where designers / agents pick blocks and content freely; the platform guardrails the technical side so the site ends up correctly architected. You handle: design (block sequence, copy, imagery), brand (palette, fonts, logo), structure (pages, slugs, locales). Atomicsite handles automatically: SEO meta + JSON-LD, hreflang emission, sitemap, robots.txt, llms.txt, security.txt, security headers (HSTS, CSP, X-Frame-Options, COOP/CORP/COEP, Permissions-Policy), <picture> + WebP variants + width/height, lazy loading, mobile breakpoints, focus indicators, semantic landmarks, FAQPage / Organization JSON-LD, h1/h2 hierarchy enforcement at block-render time. Drive the site to 95%+ on agent-writable eval categories. Custom interactive widgets (calculators, scanners, custom visualisations) are bespoke, build them as Astro components in the site's component catalog (see component block_type) when a customer needs one; atomicsite ships the generic primitives (form, feature_grid, replacement_grid, embed, logo_carousel, etc.) the agent composes from.",
		PageTemplate: PageTemplate{
			Description: "Canonical block sequence for a marketing-grade homepage modeled after world-class marketing sites (Linear, Stripe, Vercel, Resend). Use as the default when creating a fresh page; remove blocks only when the page is short-form (legal, 404).",
			Blocks: []TemplateBlock{
				{BlockType: "split_hero", Required: true, Notes: "Sort 0. Side-by-side hero (text left, image right) for SaaS marketing. headline -> h1 (REQUIRED for eval Has H1 + Single H1). eyebrow + subheading + cta_text + cta_url + secondary_label + secondary_url + image_id. Use 'hero' (centered) instead when the page sells a single product or service with one big CTA."},
				{BlockType: "logo_carousel", Required: false, Notes: "Sort 1. Rolling marquee of customer / partner logos directly under hero. Pure CSS animation, no JS, pauses on hover. Skip when you don't have actual logos to show, empty marquee feels worse than no marquee."},
				{BlockType: "stat_grid", Required: false, Notes: "Trust signals row. items[] = {value, label}. 3-4 stats hits the AI-Friendly Formatting eval check (>= 3-item list)."},
				{BlockType: "replacement_grid", Required: false, Notes: "For SaaS-alternative / migration sites: bento grid of 'old → new' cards with strikethrough on the 'from' name + arrow + bold replacement. Items can span 2 columns via items[i].span='wide'. Better than feature_grid for before/after pages."},
				{BlockType: "feature_grid", Required: false, Notes: "Services / How it works / generic 3-6-card sections. heading + subheading + items[] each with title (h3), body (p), icon (Lucide name). Alternating sections get a tinted background automatically."},
				{BlockType: "text", Required: false, Notes: "Long-form section: founder bio, About, methodology, deep-dive. eyebrow + heading + multi-paragraph body (split with \\n\\n; single \\n becomes <br>). Targets the 300+ word eval check."},
				{BlockType: "pricing", Required: false, Notes: "3-up tier cards. heading + subheading + tiers[] each with name + price + price_period + description + features[] + cta_text + cta_url + featured?. Set featured=true on the recommended plan for primary border + shadow."},
				{BlockType: "accordion_faq", Required: false, Notes: "Q&A section near the bottom. heading + items[] each with question + answer. Auto-emits FAQPage JSON-LD."},
				{BlockType: "cta", Required: true, Notes: "Last block. heading + text + cta_text + cta_url + variant ('primary'|'secondary'). Renders as a tinted banner card. Don't end a page on a feature_grid; conversion + content-density both improve with a closing CTA."},
			},
		},
		HardRules: []HardRule{
			{Rule: "Set both meta_title (30-60 chars) AND meta_description (120-160 chars with action verbs like learn / try / get / book / läs / prova) on every page row via update_page.", WhyItMatters: "Eval flags Title Length 30-60, Meta Description 120-160, Meta Description Has CTA per page; branding fallback alone fails these.", HowToApply: "update_page(slug=..., meta_title=..., meta_description=...) for every published page including legal + 404."},
			{Rule: "Use a hero block at sort_order=0 on every content page so the renderer emits an h1 from the headline field.", WhyItMatters: "Eval Has H1 + Single H1 are page-level checks. Without a hero block, no h1 renders, both checks fail.", HowToApply: "create_block(page_slug, block_type='hero', sort_order=0, data={headline, subheading, cta_text, cta_url, ...})"},
			{Rule: "Set per-page no_index=0 on real content pages. Use no_index=1 only for splash + admin pages.", WhyItMatters: "Eval Not Noindexed expects content pages to be indexable. Splash pages are fine to flag here as long as they're the only ones.", HowToApply: "update_page(slug=..., no_index=0). Default is 0; only flip to 1 for splash."},
			{Rule: "For multi-language sites, every page needs a counterpart in every other locale at the matching slug pattern.", WhyItMatters: "Eval Hreflang Tags fires when general.additional_langs is set; missing counterparts cause atomicsite to skip emitting hreflang for those pages, which the eval flags as missing.", HowToApply: "create_page for each locale. Slug convention: /<lang>/<slug>. Mirror /en/about ↔ /sv/about, /en/privacy ↔ /sv/privacy, etc."},
			{Rule: "PATCH /api/agent/branding to set meta_title, meta_description, og_image_id, favicon_id, lang as a fallback layer.", WhyItMatters: "Pages that don't override get the fallback. og_image_id + favicon_id drive the OG Image, OG Image Size 1200x630, Favicon, Apple Touch Icon, Organization Schema (logo) eval checks all at once.", HowToApply: "Upload a 1200x630 PNG to brand folder via /api/agent/media (no MCP wrapper for upload, use curl POST /api/agent/media/from-url or media/from-base64). Then PATCH branding with the returned media id."},
			{Rule: "Wire seo.same_as with newline-separated social URLs (LinkedIn, GitHub, Instagram, X, YouTube).", WhyItMatters: "Drives JSON-LD Organization sameAs which is the Organization Schema Completeness eval check.", HowToApply: "bulk_upsert_settings([{category:'seo', key:'same_as', value:'https://...\\nhttps://...'}])"},
			{Rule: "trigger_build is the publish verb. After it returns success, the live URL reflects the new content via auto-deploy.", WhyItMatters: "Build also runs eval. The eval response is the source of truth for what's failing; act on its checks_json directly rather than guessing.", HowToApply: "trigger_build -> poll get_build_status until status=='success' -> get_evaluation(build_id) -> walk failing checks -> fix -> repeat."},
			{Rule: "No plaintext emails in block text. Use mailto: links via raw block sparingly OR refer the reader to a contact page.", WhyItMatters: "Block-time guardrail rejects plaintext emails on create/update_block. Eval also flags any that slip through.", HowToApply: "Rephrase 'email privacy@example.com' to 'reach our privacy team via the address on the Privacy Policy page' or use 'privacy at example dot com' as obfuscation."},
			{Rule: "Update block sort_order via update_block (now persists; previously dropped). Don't delete-and-recreate to reorder.", WhyItMatters: "Was a contradiction in the agent surface; fixed in the same session that authored this playbook (commit 8a0f87d0). Block sort_order writes are now reliable.", HowToApply: "update_block(block_id, sort_order=N, data=...). Use unique sort_order values per page so render order is deterministic."},
		},
		BuildLoop: []string{
			"1. Read /api/agent/context to get pending_setup + settings_catalog + block_schemas + this playbook. Then fetch atomicsite://meta/knowledge-graph for the cross-reference map of curriculum + tools + resources + prompts; pick relevant nodes from the graph instead of crawling. Walk atomicsite://knowledge/<slug> for any doc whose summary is on point. The reading_order in eval_playbook.mastery is the deterministic curriculum sequence.",
			"2. Profile + branding + settings via update_profile / PATCH /api/agent/branding / bulk_upsert_settings.",
			"3. Page structure: create_page for every page in every locale.",
			"4. Per-page meta: update_page(slug, meta_title, meta_description, no_index, og_image_id) for every page.",
			"5. Block content: create_block per the page template (hero -> feature_grids/text -> cta).",
			"6. Global blocks: PUT /api/agent/global/{header,footer} once per site.",
			"7. trigger_build, wait for success, get_evaluation.",
			"8. For every failing check the agent can write: fix, re-build. For admin-only: collect into a final report.",
			"9. Stop when the agent-writable categories hit 95%+. Hand off the admin-only list with the human_admin_url for each item.",
		},
		AdminOnlyItems: []AdminOnlyItem{
			{Setting: "security category (HSTS preload, CSP extra directives, COOP/CORP/COEP, X-Frame-Options)", HumanAdminURL: "/sites/" + siteID + "/settings/security", EvalImpact: "Up to 6 security points (HSTS preload directive, CSP customisation)"},
			{Setting: "allowed-scripts (CSP allowlist for cal.com, YouTube, Stripe Checkout, GA, GTM)", HumanAdminURL: "/sites/" + siteID + "/settings/allowed-scripts", EvalImpact: "Required before iframe / third-party script blocks render. CSP Quality eval check at risk if you skip."},
			{Setting: "Cross-Origin-Embedder-Policy", HumanAdminURL: "/sites/" + siteID + "/settings/security", EvalImpact: "1 security point"},
		},
		VerificationGate: []string{
			"list_pages returns every page you intended (count matches your plan).",
			"list_blocks for the home page returns 5+ blocks in increasing sort_order.",
			"get_site_context.pending_setup is empty (or only admin-only items remain).",
			"Latest get_evaluation shows >= 95% on at least 3 of 5 categories (accessibility / seo / privacy on agent-writable parts; security depends on admin items).",
			"curl -I returns 200 on /, /<en>/, /<sv>/, /<en>/privacy, etc.",
			"Live HTML at the homepage contains <h1> from the hero block, <img> with /media/<og_image_id> reference, og:image meta tag, JSON-LD Organization schema with logo + sameAs.",
			"Response headers carry Strict-Transport-Security, Content-Security-Policy, X-Frame-Options, Referrer-Policy, Permissions-Policy, X-Content-Type-Options.",
		},
	}
}

// computeI18n surfaces the multi-language config for one site. Reads
// sites.lang, settings (general.additional_langs, seo.hreflang_strategy,
// seo.meta_title_template, seo.meta_description_template,
// seo.canonical_base), then derives the actual published locale roots
// from the page list so the agent knows which language paths are live.
func (b *ContextBuilder) computeI18n(ctx context.Context, siteID string, site store.Site, pages []PageInfo) I18nInfo {
	settings, _ := b.queries.ListSettingsBySite(ctx, siteID)
	sm := make(map[string]string, len(settings))
	for _, s := range settings {
		sm[s.Category+"."+s.Key] = s.Value
	}

	defaultLang := strings.ToLower(strings.TrimSpace(site.Lang))
	if defaultLang == "" {
		defaultLang = "en"
	}

	addl := []string{}
	seen := map[string]bool{defaultLang: true}
	for _, raw := range strings.Split(sm["general.additional_langs"], ",") {
		l := strings.ToLower(strings.TrimSpace(raw))
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		addl = append(addl, l)
	}

	strategy := strings.ToLower(strings.TrimSpace(sm["seo.hreflang_strategy"]))
	switch strategy {
	case "path", "subdomain", "off":
	default:
		strategy = "path"
	}

	// Derive actual locale roots from the published-page slugs. Default
	// locale root is "" (root). Each /<lang>/ prefix that has at least
	// one published page becomes a root.
	rootSet := map[string]bool{"": true}
	knownLangs := map[string]bool{defaultLang: true}
	for _, l := range addl {
		knownLangs[l] = true
	}
	for _, p := range pages {
		s := strings.TrimPrefix(p.Slug, "/")
		if s == "" {
			continue
		}
		first := strings.ToLower(strings.SplitN(s, "/", 2)[0])
		if knownLangs[first] {
			rootSet["/"+first] = true
		}
	}
	roots := make([]string, 0, len(rootSet))
	for r := range rootSet {
		roots = append(roots, r)
	}
	sort.Strings(roots)

	return I18nInfo{
		DefaultLang:             defaultLang,
		AdditionalLangs:         addl,
		Strategy:                strategy,
		LocaleRoots:             roots,
		MetaTitleTemplate:       sm["seo.meta_title_template"],
		MetaDescriptionTemplate: sm["seo.meta_description_template"],
		CanonicalBase:           sm["seo.canonical_base"],
	}
}

// computeSecurityPosture builds the agent-facing security summary. Reads
// allowed_scripts to group trusted domains by kind, computes the resolved
// CSP via the same builder helper the build pipeline uses, and lists the
// auto-generated files atomicsite emits for every site.
func (b *ContextBuilder) computeSecurityPosture(ctx context.Context, siteID string) SecurityPostureInfo {
	rows, _ := b.queries.ListAllowedScriptsBySite(ctx, siteID)
	td := TrustedDomainsByKind{
		Script:  []string{},
		Frame:   []string{},
		Image:   []string{},
		Media:   []string{},
		Connect: []string{},
		All:     []string{},
	}
	for _, s := range rows {
		if s.IsActive != 1 {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(s.Kind)) {
		case "frame":
			td.Frame = append(td.Frame, s.Domain)
		case "image":
			td.Image = append(td.Image, s.Domain)
		case "media":
			td.Media = append(td.Media, s.Domain)
		case "connect":
			td.Connect = append(td.Connect, s.Domain)
		case "all":
			td.All = append(td.All, s.Domain)
		default: // "" or "script"
			td.Script = append(td.Script, s.Domain)
		}
	}

	// Resolved CSP / HSTS / frame-ancestors. Same source-of-truth as
	// the build pipeline; written via internal/builder/security.go but
	// imported here would be a circular dep, so we re-read the same
	// settings + scripts and inline the few fields we need.
	settings, _ := b.queries.ListSettingsBySite(ctx, siteID)
	sm := make(map[string]string)
	for _, s := range settings {
		sm[s.Category+"."+s.Key] = s.Value
	}
	frameAncestors := sm["security.frame_ancestors"]
	if frameAncestors == "" || frameAncestors == "auto" {
		frameAncestors = "'none'"
	}
	// CSP + HSTS strings are computed by builder.BuildSecurityHeaders;
	// to keep this package free of a builder import we leave them empty
	// and let the agent fetch them via GET /api/agent/security/preview
	// when it needs the resolved policy verbatim. Most of the time the
	// trusted_domains map + frame_ancestors are enough.
	return SecurityPostureInfo{
		TrustedDomains: td,
		AllowedKinds:   []string{"script", "frame", "image", "media", "connect", "all"},
		FrameAncestors: frameAncestors,
		HumanAdminURL:  "/sites/{site_id}/settings/allowed-scripts",
		GeneratedFiles: []GeneratedFileInfo{
			{Path: "/sitemap-index.xml", Label: "XML Sitemap", Description: "Auto-built from every published page via @astrojs/sitemap."},
			{Path: "/robots.txt", Label: "robots.txt", Description: "Auto-built; AI training bots blocked by default. Override via seo.robots_txt setting."},
			{Path: "/.well-known/security.txt", Label: "security.txt", Description: "RFC 9116 contact, emitted when Profile.security_email is set."},
			{Path: "/llms.txt", Label: "llms.txt", Description: "AI search readiness (Perplexity, ChatGPT). Built from published pages."},
			{Path: "/humans.txt", Label: "humans.txt", Description: "Team + site metadata."},
			{Path: "/favicon.ico", Label: "Favicon", Description: "Linked in every page from the brand media folder."},
			{Path: "/apple-touch-icon.png", Label: "Apple touch icon", Description: "iOS home-screen icon."},
			{Path: "<head> JSON-LD Organization", Label: "Organization schema", Description: "Auto-emitted from Profile (business name + sameAs URLs)."},
			{Path: "<head> JSON-LD BreadcrumbList", Label: "BreadcrumbList schema", Description: "Per page (depth >= 2)."},
			{Path: "<head> JSON-LD Article", Label: "Article schema", Description: "Article-type pages: datePublished + dateModified + author."},
			{Path: "<head> OG + Twitter Card", Label: "Social meta", Description: "og:title/description/image/site_name/locale + twitter:card=summary_large_image."},
		},
	}
}

// defaultEndpoints returns the endpoint catalogue surfaced to AI agents.
// Static for now; if a future endpoint becomes site-conditional it can
// move into the Build closure.
func defaultEndpoints() EndpointsInfo {
	return EndpointsInfo{
		Pages: []EndpointInfo{
			{Method: "GET", Path: "/api/agent/pages", Use: "List published + draft pages."},
			{Method: "POST", Path: "/api/agent/pages", Use: "Create a page. Title + slug required; layout defaults to 'default'."},
			{Method: "PATCH", Path: "/api/agent/pages/{slug}", Use: "Update title, slug, status, meta_title, meta_description, og_image_id, layout, hide_global_blocks, no_index, canonical_url. Slug-addressed."},
		},
		Blocks: []EndpointInfo{
			{Method: "GET", Path: "/api/agent/pages/{slug}/blocks", Use: "List blocks on a page, ordered by sort_order."},
			{Method: "POST", Path: "/api/agent/pages/{slug}/blocks", Use: "Append a block. block_type + data are required; data should match the block_schemas catalogue below."},
			{Method: "PATCH", Path: "/api/agent/blocks/{blockID}", Use: "Patch a block's data, style, sort_order, or is_visible without re-creating it."},
			{Method: "DELETE", Path: "/api/agent/blocks/{blockID}", Use: "Remove a block from a page."},
		},
		GlobalBlocks: []EndpointInfo{
			{Method: "PUT", Path: "/api/agent/global/{slot}", Use: "Upsert the header or footer global block. Slot must be 'header' or 'footer'. Body: name, block_type, data."},
		},
		Media: []EndpointInfo{
			{Method: "GET", Path: "/api/agent/media", Use: "List uploaded media."},
			{Method: "POST", Path: "/api/agent/media", Use: "Multipart upload. Optional folder field puts files under a folder; 'brand' is the system folder for favicon, OG image, logos."},
			{Method: "POST", Path: "/api/agent/media/from-url", Use: "Server-side fetch with SSRF guard. Same folder field as multipart."},
			{Method: "POST", Path: "/api/agent/media/from-base64", Use: "For agents that emit base64 but not multipart. Same folder field."},
		},
		Build: []EndpointInfo{
			{Method: "POST", Path: "/api/agent/build", Use: "Trigger a full build. Returns a build_id for status polling."},
			{Method: "GET", Path: "/api/agent/evaluation/{buildID}", Use: "Read the post-build evaluation results so the agent can self-correct against the 96 Site Inspector parity checks."},
		},
		Preview: []EndpointInfo{
			{Method: "GET", Path: "/api/sites/{siteID}/pages/{pageID}/blocks/{blockID}/preview", Use: "Returns the rendered Astro source for one block. Use this when the user asks 'show me the code' for a specific section. Same source the build pipeline writes to disk."},
			{Method: "GET", Path: "/api/sites/{siteID}/pages/{pageID}/preview", Use: "Returns the assembled .astro source for the whole page. Use to inspect what bun build will see without triggering a build."},
		},
		Security: []EndpointInfo{
			{Method: "GET", Path: "/api/agent/security/preview", Use: "Returns the resolved security headers (CSP, HSTS, X-Frame-Options, Referrer-Policy, Permissions-Policy, COOP, CORP, COEP) the next build will emit. Use to verify a CSP includes the host you need before recommending a content change to the user."},
			{Method: "GET", Path: "/api/agent/settings/security", Use: "Read-only view of the security category settings (HSTS, CSP toggles, frame_ancestors, csp_extra_directives, COOP/CORP/COEP). Writes here stay admin-only because they widen attack surface."},
			{Method: "GET", Path: "/api/agent/settings/general", Use: "Read general-category settings (default_lang, additional_langs, domain_aliases). Writes via PATCH /api/agent/settings."},
			{Method: "GET", Path: "/api/agent/settings/seo", Use: "Read seo-category settings (meta_title_template, meta_description_template, canonical_base, hreflang_strategy, robots_txt override, llms_txt override, same_as, logo_url). Writes via PATCH /api/agent/settings."},
			{Method: "PATCH", Path: "/api/agent/settings", Use: "Bulk upsert. seo + general + analytics are agent-writable; security + allowed-scripts + nginx + danger reject silently and the response carries rejected_admin_only."},
		},
		TrustedDomains: []EndpointInfo{
			{Method: "GET", Path: "/api/agent/allowed-scripts", Use: "Read the trusted-domain rows that feed CSP. Each row has a kind (script | frame | image | media | connect | all). Use this to detect 'is cal.com already trusted?' before asking the user to add it. Mutations are admin-only at /api/sites/{id}/allowed-scripts (UI: /sites/{id}/settings/allowed-scripts)."},
		},
	}
}

// defaultDesignPlaybook is the platform-level design DNA every agent
// reads on first /api/agent/context call. Curated against world-class
// marketing sites (Linear, Stripe, Vercel, Resend), onwidget/astrowind,
// and a year of marketing-site audits. Keep punchy and actionable;
// agents skim, they don't study.
// DefaultDesignPlaybook returns the platform-level design DNA every
// agent reads. Exported so internal/critique can run the same playbook
// rules against rendered output without duplicating the source of truth.
// DefaultDesignPlaybook returns the balanced-fidelity playbook. Callers
// that know the site should prefer DesignPlaybookFor(fidelity) so
// guidance and grading follow the site's design.fidelity dial.
func DefaultDesignPlaybook() DesignPlaybookInfo { return DesignPlaybookFor(FidelityBalanced) }

func defaultDesignPlaybook() DesignPlaybookInfo {
	return DesignPlaybookInfo{
		Stack: "Atomicsite renders agent input into static Astro 5 sites styled with Tailwind 4 utilities + a per-site CSS pipeline (internal/builder/css.go). Block content is authored as JSON via the agent API; the renderer turns it into semantic HTML. TypeScript is reserved for built-in interactive widgets (CookieProof, hydration script, circuit canvas), agents do not write Astro/TS by hand. Every visual decision flows through block_type + data fields + settings + the eight CSS custom properties (--color-primary, --color-text, --color-bg, --color-surface-elevated, --font-heading, --font-body, --font-mono, --container-width).",
		Principles: []DesignPrinciple{
			{
				Name:         "Hierarchy first, decoration last",
				Rule:         "The visitor's eye moves down the page in this order: eyebrow → headline → subheading → primary CTA. Anything that fights that order is decoration.",
				WhyItMatters: "Marketing sites convert when one path is obvious within 1.5s. A reader who has to choose between two equal-weight options reads neither.",
				HowToApply:   "Use the section-header pattern (eyebrow + h2 + subheading) on every block. Set ONE primary CTA per section via cta_text. Use secondary_label sparingly, at most one per page. Never give two buttons the same colour.",
			},
			{
				Name:         "One eyebrow colour, one headline accent, one CTA dark",
				Rule:         "The brand primary colour appears in three places only: eyebrows, the [[bracketed]] accent in headlines, and inline link/btn-accent strokes. The primary CTA uses --color-text (near-black) on white, not the brand colour. Colour-everywhere reads cheap.",
				WhyItMatters: "Brightinteraction.com, Linear, Vercel, Stripe all follow this pattern. A reserved palette with one accent = expensive feel. A rainbow = template feel.",
				HowToApply:   "Set primary CTA via .btn-primary (dark fill). Use .btn-accent (brand fill) only for high-stakes secondary moments (final CTA section, featured pricing tier). Eyebrows and accent spans are the brand-colour budget, spend them.",
			},
			{
				Name:         "White space is a feature, not a bug",
				Rule:         "Section padding-block ≥ 5rem on desktop, ≥ 3rem on mobile. Subheading max-width ≤ 50ch. Don't fill empty space with extra cards.",
				WhyItMatters: "Density signals downmarket. The most expensive sites in a category have the most whitespace. AstroWind, Linear, Resend all use 5-7rem section gutters.",
				HowToApply:   "Atomicsite's renderer ships these defaults, don't override them with style_json overrides unless you have a specific reason. If a section feels cramped, REMOVE content (drop one feature card), don't reduce padding.",
			},
			{
				Name:         "Visual rhythm via alternating surfaces",
				Rule:         "Sections alternate between --color-bg (white) and --color-surface-elevated (whisper-grey). Never run two same-colour sections back-to-back, never tint adjacent sections at different intensities.",
				WhyItMatters: "Visitors scroll based on perceived structure, not content. Alternating bg gives the page a heartbeat. The renderer wires this automatically via :nth-of-type(even).",
				HowToApply:   "Use the default block sequence. The CSS pipeline alternates :nth-of-type(even) main-blocks (excluding hero/cta/logos) automatically. Don't fight it with custom backgrounds.",
			},
			{
				Name:         "Three primitives carry 80% of marketing pages",
				Rule:         "hero (or split_hero) → stat_grid (proof) → feature_grid or replacement_grid (offer) → process_steps or about_split (story) → pricing → accordion_faq → cta. Most B2B SaaS homepages need only this set.",
				WhyItMatters: "Resisting the urge to invent new primitives keeps the renderer surface stable AND keeps the agent from drifting into bespoke implementations. If it's not in the block taxonomy, it's probably an embed (Cal.com, YouTube, Typeform) or a custom block with raw markup, not a missing primitive.",
				HowToApply:   "Read block_schemas before reaching for block_type=custom. The 21 built-in types cover the standard marketing-site catalog; custom is a last resort for genuinely bespoke widgets.",
			},
			{
				Name:         "Copy is design",
				Rule:         "An eyebrow tells the visitor what category they're in. A headline tells them what changes. A subheading tells them why now. A CTA tells them what happens next. If any of those is generic, the section reads as filler.",
				WhyItMatters: "Atomicsite renders whatever you give it, beautifully. But beautiful generic copy still converts at 0.5%. Specific copy at 3-4%.",
				HowToApply:   "Reject 'Welcome', 'Our services', 'Get in touch' as eyebrows. Use the noun the visitor is shopping for ('Open-source infrastructure', 'GDPR compliance', 'EU hosting'). For headlines, use a transformation verb + concrete noun ('Stop renting your business. Own it.' beats 'Our solutions for your business.').",
			},
			{
				Name:         "Mobile is the design, desktop is a remix",
				Rule:         "Compose for a 375px column first. If the section reads at that width, it'll work on desktop. The renderer's @media queries collapse multi-column blocks below 768px.",
				WhyItMatters: "60-70% of marketing-site traffic is mobile. The renderer's mobile rules are the silent default, agents who design for desktop and ignore mobile create layout shifts and orphan content on small screens.",
				HowToApply:   "Don't put more than 4 items in feature_grid (it stacks on mobile, becoming a long list). Don't use replacement_grid spans wider than 1 unless paired with a span-2 sibling. Prefer process_steps over feature_grid for sequential content (numbered cards stack cleaner).",
			},
		},
		PageArchetypes: []PageArchetype{
			{
				Name:        "B2B SaaS homepage",
				Use:         "Software/service company selling to other companies. Primary goal: book a call or trial signup.",
				Description: "9 blocks. Hero (centred or split), stats for proof, optional logo strip, replacement_grid or feature_grid for the offer, process_steps for how-it-works, pricing, about_split for trust, FAQ for objection-handling, final CTA.",
				Blocks: []ArchetypeBlock{
					{BlockType: "hero", Role: "Above-the-fold attention grab", Notes: "bg=circuit for tech/security/infra, bg=image for everything else. ONE primary CTA + one secondary_label. headline gets a [[bracketed]] accent on the verb that matters."},
					{BlockType: "stat_grid", Role: "Receipts under the hero", Notes: "3-4 numbers with label + context. Hits the AI-Friendly Formatting eval check (≥3-item list)."},
					{BlockType: "logo_carousel", Role: "Trust signal", Notes: "Use logo_strip if logos are stationary; logo_carousel if marquee. Short label like 'Trusted by' or 'Founder previously worked with'."},
					{BlockType: "replacement_grid", Role: "What we offer (with strikethrough comparison)", Notes: "Best when positioning AS A REPLACEMENT for an incumbent (X → Y). Otherwise use feature_grid."},
					{BlockType: "process_steps", Role: "How it works", Notes: "Always 4 steps. Numbered 01-04, mono font for numerals via --font-mono."},
					{BlockType: "pricing", Role: "Three ways to work", Notes: "3 tiers. Mark middle tier featured=true. Use tiers[].step for STEP 01/02/03 eyebrows. Tier names ≤ 3 words."},
					{BlockType: "about_split", Role: "Founder / team trust", Notes: "Photo right by default. Three stats next to bio. CTA links to /about/<slug>/ for long-form."},
					{BlockType: "accordion_faq", Role: "Objection handling", Notes: "5-7 items. Auto-emits FAQPage JSON-LD. Mix factual + sales-objection questions."},
					{BlockType: "cta", Role: "Final close", Notes: "Centred banner. variant=primary (default) or secondary. Single CTA, same destination as hero CTA."},
				},
			},
			{
				Name:        "Pricing page",
				Use:         "Standalone pricing page when homepage pricing isn't enough.",
				Description: "5 blocks. Skinny hero, pricing table, comparison feature_grid, FAQ, CTA.",
				Blocks: []ArchetypeBlock{
					{BlockType: "hero", Role: "One-line proposition", Notes: "Smaller padding than homepage hero. No image. Single sentence subheading."},
					{BlockType: "pricing", Role: "The tiers", Notes: "Same structure as homepage but with a richer features[] list per tier."},
					{BlockType: "feature_grid", Role: "Cross-tier comparison", Notes: "Grid of 'What's included in every tier'."},
					{BlockType: "accordion_faq", Role: "Pricing-specific FAQ", Notes: "Billing, refunds, can-I-cancel, what-if-I-grow. Different from homepage FAQ."},
					{BlockType: "cta", Role: "Talk-to-sales escape hatch", Notes: "For enterprise leads who don't fit the tiers."},
				},
			},
			{
				Name:        "About page",
				Use:         "Origin story + team. Builds trust for skeptical buyers.",
				Description: "4-5 blocks. Hero, founder story (text), stat_grid, optional team feature_grid, CTA.",
				Blocks: []ArchetypeBlock{
					{BlockType: "hero", Role: "Mission statement", Notes: "Single h1, no image. Short subheading."},
					{BlockType: "text", Role: "Founder story", Notes: "Long-form paragraphs. max-width: 42rem auto-applied."},
					{BlockType: "stat_grid", Role: "Track record", Notes: "Years of experience, clients served, etc."},
					{BlockType: "feature_grid", Role: "Team (optional)", Notes: "icon=user per item; body is role + bio one-liner."},
					{BlockType: "cta", Role: "Book a call", Notes: "Pull through to the same booking link the homepage uses."},
				},
			},
			{
				Name:        "Blog post / insight article",
				Use:         "Long-form content. SEO surface for keyword targeting.",
				Description: "Hero with article meta, table of contents component, body text blocks, related-articles feature_grid, CTA.",
				Blocks: []ArchetypeBlock{
					{BlockType: "hero", Role: "Article title + meta", Notes: "h1 = article title. Subheading = TL;DR. eyebrow = category."},
					{BlockType: "text", Role: "Article body", Notes: "Multiple text blocks separated by image, quote, or code_block as visual breaks. Don't compose one giant text block, Site Inspector dings under-broken articles."},
					{BlockType: "feature_grid", Role: "Related articles", Notes: "3-up grid of links to other articles."},
					{BlockType: "cta", Role: "Convert the reader", Notes: "Article → product transition CTA."},
				},
			},
			{
				Name:        "Contact page",
				Use:         "Lead capture + direct contact info.",
				Description: "3 blocks. Hero, form, contact strip.",
				Blocks: []ArchetypeBlock{
					{BlockType: "hero", Role: "What happens after they submit", Notes: "Set expectations: 'I reply within 24h'. No image."},
					{BlockType: "form", Role: "The form itself", Notes: "5-6 fields max. action= a worker URL or n8n webhook. Submit label is action-oriented ('Send', not 'Submit')."},
					{BlockType: "text", Role: "Direct contact escape hatch", Notes: "Email + phone for visitors who don't trust forms."},
				},
			},
			{
				Name:        "Ecommerce, catalog + product detail + checkout",
				Use:         "Selling physical/digital goods on Atomicsite's first-party storefront stack. Covers three page types: catalog (product grid), product detail (single item), checkout (form). Cart drawer is a global block placed once.",
				Description: "Sprint 2 (2026-05-22) shipped a first-party storefront. Use the four typed storefront blocks: product_grid for catalog pages, product_detail for product pages, cart_drawer placed once globally, checkout_form on a dedicated /checkout page. All four are server-rendered Go HTML at build time; cart state + checkout submit are driven by a per-site vanilla JS island (_atomic-storefront.js) injected automatically by the layout. The checkout form POSTs to /api/sites/{siteID}/checkout, which server-validates the cart against the catalog, applies discount codes, creates an order row, and returns a Mollie checkout URL. Payment is OUTSOURCED to Mollie hosted pages, atomicsite does not handle PCI/SCA. After payment, Mollie redirects to checkout_form.return_url with ?order=ATM-... appended. Inventory + discount-used-count + order-state side-effects fire on the Mollie webhook server-side. Catalog page (typical), hero -> product_grid -> accordion_faq -> cta. Product detail page (typical), product_detail -> feature_grid (specs / what's included) -> stat_grid (reviews / stock / ship time) -> accordion_faq (shipping + returns). Checkout page, hero (light, hide_global_blocks=1 for conversion) -> checkout_form. Place cart_drawer once as a global block so every page has the trigger + drawer shell.",
				Blocks: []ArchetypeBlock{
					{BlockType: "product_detail", Role: "Hero + gallery + variant picker + Add-to-cart", Notes: "Required. Set product_slug to the product.slug from the Store dashboard. The renderer resolves variants at build time and emits a radio variant picker plus an Add-to-cart button carrying the snapshot the storefront island reads."},
					{BlockType: "feature_grid", Role: "What's included / specs / materials", Notes: "Each item is a spec line. Use icon + body. 4-6 items max."},
					{BlockType: "stat_grid", Role: "Trust signals: rating, in-stock, ships-in", Notes: "Items: {value: '4.8/5', label: 'Reviews', context: '847 ratings'}, {value: 'In stock', context: 'Ships next day'}. Use real numbers from your store, not placeholders."},
					{BlockType: "accordion_faq", Role: "Shipping, returns, sizing, care instructions", Notes: "Pre-purchase objection handling. 5-7 items. Auto-emits FAQPage JSON-LD."},
					{BlockType: "about_split", Role: "Brand story / craftsmanship moment", Notes: "Why this product, who makes it. Builds trust premium ecom needs."},
				},
			},
		},
		BlockSelection: []BlockSelectionRule{
			{Question: "Centered hero with no image vs split_hero with image right?", Choose: "hero (centered) when one CTA + clean message. split_hero when the image carries actual information (dashboard preview, product screenshot).", Why: "A decorative image right of the hero competes with the headline. Use split_hero only when the image is part of the value prop."},
			{Question: "stat_grid vs feature_grid?", Choose: "stat_grid for numbers (599, 100/100, 60-90%, 20+). feature_grid for ideas (icon + title + body).", Why: "stat_grid renders large numerals with hierarchy; feature_grid is icon + text-balanced cards. Mixing them muddies both."},
			{Question: "feature_grid vs replacement_grid?", Choose: "replacement_grid when positioning as 'X but better than Y' (Notion → Outline, Slack → Mattermost). feature_grid for standalone capabilities.", Why: "replacement_grid renders strikethrough+arrow+bold pattern that explicit comparison demands. feature_grid is for non-comparative cards."},
			{Question: "process_steps vs ordered text list?", Choose: "process_steps for 3-5 sequential phases with names. text-with-list when the steps are sub-points of a paragraph.", Why: "process_steps uses big mono numerals + heading-font titles. Text lists are inline. Don't put numbered-step content into text bullet lists, you lose the hierarchy."},
			{Question: "logo_strip vs logo_carousel?", Choose: "logo_strip for ≤6 stationary logos. logo_carousel for ≥8 OR when motion adds energy.", Why: "Carousel pauses on hover, so it's accessible. But always-on motion creates noise on small pages."},
			{Question: "accordion_faq vs feature_grid Q+A?", Choose: "accordion_faq for ANY FAQ, never Q+A as feature_grid items.", Why: "accordion_faq auto-emits FAQPage JSON-LD which Google + AI search engines reward. feature_grid Q+A produces no schema."},
			{Question: "Should this page have hide_global_blocks=true?", Choose: "Yes only for: splash language-chooser, dedicated landing pages with no nav, embed-style pages.", Why: "Per-page suppression of header + footer destroys cross-section navigation. Default is keep them on."},
			{Question: "How many blocks per page?", Choose: "Homepage 8-10. Subpage 4-7. Blog post variable.", Why: "Past 12 blocks readers get fatigue and bounce. Past 10 the renderer's alternating-bg pattern starts to feel mechanical."},
			{Question: "When to use the custom block_type?", Choose: "Only when the agent has tried 3 built-in primitives + the embed block, AND none fit. Final escape hatch.", Why: "custom emits raw markup which doesn't get the auto-section-id, eyebrow CSS, or any other render polish. Built-in blocks pull their weight automatically."},
			{Question: "headline_accent field vs [[brackets]] in headline?", Choose: "[[brackets]] for inline accent in the same line ('Stop renting. Own [[it]].'). headline_accent for a coloured second line ('Stop renting your business. / Own it.' where 'Own it' is the second line).", Why: "Inline brackets read as one sentence with emphasis. Two-line accent reads as sequential statement. Pick the rhythm the headline wants."},
		},
		Typography: TypographyScale{
			HeadingFont: "Space Grotesk (loaded via Google Fonts in the layout). Falls back to system-ui, sans-serif. Used for h1-h6, brand badges, process step numbers, stat values, tier names.",
			BodyFont:    "Inter (Google Fonts). Falls back to system-ui, sans-serif. Used for paragraphs, lists, form labels, FAQ summaries.",
			MonoFont:    "Space Mono (Google Fonts). Used for eyebrows, brand wordmarks, pricing tier-step labels (STEP 01), code blocks, footer copy fine-print.",
			Scale: []TypeStep{
				{Element: "Hero h1", Size: "clamp(2.5rem, 6vw, 5rem)", Weight: "700", Use: "Above-the-fold headline. Max 18ch line-length. Letter-spacing -0.025em."},
				{Element: "split_hero h1", Size: "clamp(2.25rem, 5vw, 4rem)", Weight: "700", Use: "Slightly smaller, sharing space with hero image."},
				{Element: "Section h2", Size: "clamp(1.875rem, 3.5vw, 2.5rem)", Weight: "700", Use: "Block headings (stat_grid, pricing, FAQ, etc). Letter-spacing -0.02em."},
				{Element: "Card h3", Size: "1.25rem", Weight: "600", Use: "feature_grid card titles, process step titles, FAQ summaries."},
				{Element: "Eyebrow", Size: "0.75rem", Weight: "700", Use: "Mono font, uppercase, letter-spacing 0.18em, --color-primary. The genre marker above headlines."},
				{Element: "Subheading", Size: "1.0625-1.25rem", Weight: "400", Use: "Below headlines. Max-width 36-50ch. Line-height 1.55."},
				{Element: "Body", Size: "1rem (16px)", Weight: "400", Use: "Default paragraph size. Line-height 1.6."},
				{Element: "Caption / context", Size: "0.8125rem", Weight: "400", Use: "stat_grid context line, footer fine print, captions. Color 55% text-mix."},
			},
			UsageRules: []string{
				"Headings ≤ 4 lines visually. If a headline wraps to 5 lines on desktop, the copy is too long.",
				"Eyebrow ≤ 4 words. Single-noun categories work best ('Open Source Infrastructure', not 'About our open source infrastructure offerings').",
				"Subheadings ≤ 2 sentences. The reader needs to be moved to the CTA, not informed of every detail.",
				"NEVER use the heading font on body paragraphs, it's display-grade, gets tiring at small sizes.",
				"NEVER use the body font on h1/h2, flat hierarchy looks unintentional.",
			},
		},
		Spacing: SpacingScale{
			ContainerWidths: map[string]string{
				"narrow":  "64rem (1024px), for blogs, long-form articles, landing pages with focused content",
				"default": "72rem (1152px), recommended default. Same as Tailwind's max-w-6xl approx.",
				"wide":    "80rem (1280px), for dashboards, dense feature pages.",
				"fluid":   "100%, only for bleed sections that handle their own padding (logo_carousel does this).",
			},
			BlockPadding: map[string]string{
				"hero":           "padding-block: 6rem 5rem (desktop), 3.5rem 3rem (mobile)",
				"split_hero":     "padding-block: 6rem 5rem (desktop), 3.5rem 3rem (mobile)",
				"standard block": "padding-block: 4rem (desktop), 3rem (mobile). Renderer default.",
				"cta":            "padding: 4rem 1.5rem. Tinted background card.",
			},
			SectionRhythm: []string{
				"Hero (white) → stat_grid (white) → logos (white, no padding-inline) → replacement_grid (alternating tinted) → process_steps (white) → pricing (alternating tinted) → about_split (white) → FAQ (alternating tinted) → cta (its own bg colour).",
				"The :nth-of-type(even) selector handles alternating automatically, agents do not set background per block. Excluded blocks: hero, split_hero, cta, logo_carousel, logo_strip (they manage their own visual rhythm).",
				"NEVER set padding-block via style_json. The renderer's defaults are tuned. If a section feels cramped, you have too much content, remove an item, don't reduce padding.",
				"Margin-block-start between sections is 0 (sections butt up against each other). The padding inside each section provides the breathing room.",
			},
		},
		Color: ColorGuidance{
			Tokens: []ColorToken{
				{Name: "--color-primary", Use: "Brand accent. Eyebrows, headline-accent spans, links, btn-accent fills, replacement-grid arrows, FAQ + icons, stat-value numerals (when no stat-context override), pricing-tier-step labels."},
				{Name: "--color-text", Use: "Near-black body + heading text. Primary CTA fill (.btn-primary uses this, NOT --color-primary). Featured pricing-tier dark fill. Footer dark variant background. Brand-badge fill."},
				{Name: "--color-bg", Use: "Page background. Default block background. .btn-primary text colour."},
				{Name: "--color-surface-elevated", Use: "Whisper-grey for alternating sections (97.5% bg + 2.5% text mix). Set automatically, never override."},
				{Name: "--color-on-primary", Use: "Text colour ON --color-primary fills (defaults to white). Override only when the brand primary is light enough that white text fails contrast."},
				{Name: "--font-heading", Use: "Heading family. Whatever the site sets in branding."},
				{Name: "--font-body", Use: "Body family."},
				{Name: "--font-mono", Use: "Mono family for eyebrows + brand wordmarks + code."},
				{Name: "--container-width", Use: "Driven by general.container_width setting (narrow|default|wide|fluid)."},
			},
			UsageRules: []string{
				"Primary colour appears in 4-7 places per page max. More than that = brand budget exceeded.",
				"NEVER use --color-primary as a section background. It's a stroke colour, not a fill colour. Backgrounds are bg / surface-elevated / text (dark variants only).",
				"NEVER use --color-text as a fill in the middle of the page. It's reserved for: primary CTA, featured pricing tier, dark footer, brand badge. If you fill a generic section with --color-text it competes with the navbar.",
				"For dark sections in the page body, use the .block--cta block (which has its own tinted background) or .pricing-tier.is-featured (single-tier dark fill). Don't invent dark sections via custom blocks.",
				"Contrast ratio ≥ 4.5:1 for body text, verified by the eval. If you change --color-primary to a light tint, also set --color-on-primary so .btn-accent text stays readable.",
			},
		},
		CommonMistakes: []DesignMistake{
			{Mistake: "Filling every section's headline + subheading with the maximum char count", Symptom: "Page feels heavy, scroll feels long, reader bounces before pricing", Fix: "Cut subheadings to one sentence. Cut headlines to ≤ 6 words. Whitespace carries the message, copy fills the gaps."},
			{Mistake: "Using feature_grid items as Q+A or as numbered process steps", Symptom: "Hierarchy feels flat. FAQ schema missing. Process visual rhythm broken.", Fix: "Use accordion_faq for Q+A (auto JSON-LD), process_steps for numbered phases (auto big numerals)."},
			{Mistake: "Using the brand colour for the primary CTA", Symptom: "Page reads as 'template-y'. Primary doesn't stand apart from eyebrows + accents.", Fix: ".btn-primary uses --color-text (dark) by design. The brand colour is reserved for accents (eyebrows, links, btn-accent). To make CTAs more prominent, use larger padding or icon, NOT colour saturation."},
			{Mistake: "Setting style_json overrides on standard blocks", Symptom: "Visual inconsistency between sections. Rhythm broken. Site feels like 'agent grabbed every option.'", Fix: "Resist style_json. The renderer's defaults are tuned. If you can't get the look you want, the answer is usually a different block_type, not custom CSS."},
			{Mistake: "Embedding raw HTML via block_type=raw or custom when a built-in primitive fits", Symptom: "Section misses auto-section-id, alternating bg, eyebrow styling. Drifts from house style.", Fix: "Read block_schemas first. Use custom only when none of the 21 primitives match."},
			{Mistake: "Multiple H1s per page", Symptom: "SEO eval fails Single H1. Heading hierarchy fails.", Fix: "Exactly one hero or split_hero per page (h1). Every other block uses h2 (heading or h2 field). Sub-cards use h3."},
			{Mistake: "Hero with no secondary_label", Symptom: "Visitor with one-question hesitation has no soft path. Bounce.", Fix: "secondary_label like 'See pricing' or 'Read the docs', points lower on the same page (#pricing) so the hesitant visitor scrolls instead of leaving."},
			{Mistake: "Pricing tiers with no featured tier", Symptom: "Visitor compares all three equally → analysis paralysis → no decision.", Fix: "Mark the recommended middle tier featured=true. Dark fill + accent CTA pulls the eye and makes the choice obvious."},
			{Mistake: "Long FAQ where every answer is one sentence", Symptom: "Reads as marketing fluff. AI search snippets get under-filled.", Fix: "Each FAQ answer should be 2-4 sentences with one concrete number or example. Search engines reward specificity."},
			{Mistake: "Process_steps with text-only step descriptions where action verbs are missing", Symptom: "Steps feel passive. Process feels long.", Fix: "Each step starts with a verb (Audit, Plan, Migrate, Monitor, not Audit phase, Plan stage, Migration step, Ongoing monitoring)."},
			{Mistake: "About_split with stock photo + generic stats", Symptom: "Trust signal works backwards, visitor distrusts.", Fix: "Real photo of the actual person. Specific stats with context (599 audited, not '500+ projects'). Without these the section is worse than removing it."},
			{Mistake: "Splash language-chooser pages indexed by search engines", Symptom: "SEO eval fails Not Noindexed; root URL outranks the locale roots.", Fix: "Set no_index=1 on the splash. Locale roots (/en/, /sv/) get indexed; splash is a UX detail."},
		},
		OneShotRecipe: OneShotRecipe{
			Description: "The 9-block + 5-setting recipe that produces a B+/A homepage on the eval, every time. Use as the default when the user gives short briefs ('build me a homepage for X').",
			Blocks: []ArchetypeBlock{
				{BlockType: "hero", Role: "Sort 0", Notes: "bg=circuit OR image_id, eyebrow (3-4 words), headline with [[bracket-accent]], subheading (1-2 sentences), cta_text + cta_url (booking), secondary_label (anchor to #pricing or similar)."},
				{BlockType: "stat_grid", Role: "Sort 1", Notes: "4 items. Each {value, label, context}. Hits AI-Friendly Formatting eval (≥3 items)."},
				{BlockType: "logo_carousel", Role: "Sort 2", Notes: "Optional but high-trust. label='Trusted by' or 'Founder previously worked with'. 6 entries minimum."},
				{BlockType: "replacement_grid OR feature_grid", Role: "Sort 3", Notes: "6 items max. replacement_grid if you have an incumbent to position against; feature_grid otherwise. heading + subheading + items[] with span variants for visual interest."},
				{BlockType: "process_steps", Role: "Sort 4", Notes: "4 steps. items[] = {number, title, description}. eyebrow='How it works' (or local equivalent)."},
				{BlockType: "pricing", Role: "Sort 5", Notes: "3 tiers. Middle tier featured=true. Each tier: step (STEP 01/02/03) + name + price + price_period + description + features[] + cta_text + cta_url."},
				{BlockType: "about_split", Role: "Sort 6", Notes: "Real founder photo (image_id), eyebrow='About', heading, 3 paragraphs, 3 stats next to bio, optional cta_text linking to long-form."},
				{BlockType: "accordion_faq", Role: "Sort 7", Notes: "5-7 items. Mix factual + objection-handling questions. Each answer 2-4 sentences with concrete numbers."},
				{BlockType: "cta", Role: "Sort 8", Notes: "Final close. Same destination as hero CTA. heading + text + cta_text + cta_url."},
			},
			Settings: []string{
				"general.container_width: default (72rem)",
				"general.mobile_breakpoint: 640",
				"general.tablet_breakpoint: 1024",
				"branding.font_heading: Space Grotesk",
				"branding.font_body: Inter",
				"branding.primary_color: brand colour (NOT #000 or #fff)",
				"seo.canonical_base: https://<domain>",
				"seo.meta_title_template: '{page_title} {separator} {site_name}'",
				"seo.meta_description_template: '{page_description}'",
				"analytics.cookieproof_enabled: 1 (auto-emits consent banner)",
				"+ global header + footer block (PUT /api/agent/global/header and /footer)",
			},
		},
		ReferenceRepos: []DesignReferenceRepo{
			{
				Path:    "automations/design-references/astrowind/",
				Stack:   "Astro 5 + Tailwind 3",
				BestFor: []string{"B2B SaaS marketing, Hero/Pricing/FAQs/Stats/Steps/Brands/CTA widgets", "Bento grid patterns", "Primary archetype: Soft Structuralism"},
				License: "MIT",
				Notes:   "src/components/widgets/ has 1:1 analogues for most atomicsite block types. Same stack as atomicsite output. The default reference for marketing-site builds.",
			},
			{
				Path:    "automations/design-references/astro-paper/",
				Stack:   "Astro 5 + TailwindCSS",
				BestFor: []string{"Blog/portfolio archetype", "Long-form article rendering", "Tag/category systems", "Pagination patterns", "Search UX (fuzzy search component)", "Light/dark theme toggle patterns"},
				License: "MIT",
				Notes:   "Production-grade blog template (5k+ stars). Use when atomicsite renders insights/blog/article archetypes. src/components/ has clean Card, Pagination, Header, Footer, Datetime, ShareLinks patterns. src/layouts/ shows article-detail page composition. Particularly strong on typography for long-form prose.",
			},
			{
				Path:    "automations/design-references/starlight/",
				Stack:   "Astro 5 + native CSS (custom theme system, no Tailwind)",
				BestFor: []string{"Documentation sites + knowledge bases", "Sidebar navigation patterns", "Search overlays", "Versioned docs / i18n routing", "MDX content pipelines", "Component-libraries patterns (callouts, tabs, code blocks)"},
				License: "MIT",
				Notes:   "Official Astro docs framework, used by Astro itself, Bun, Tauri, and 100+ open-source projects. packages/starlight/components/ has the canonical implementations of Sidebar, Search, TableOfContents, Tabs, Card, LinkCard, FileTree, Badge, Aside (callout). docs/ contains the actual Astro.build documentation site as a real production reference. Use when atomicsite needs to render docs/wiki/changelog archetypes (which is on the roadmap, not currently a built-in primitive). NOT a Tailwind reference, starlight uses its own CSS variable system, which is itself a clean reference for design-token architecture.",
			},
			{
				Path:    "automations/design-references/shadcn-svelte/",
				Stack:   "SvelteKit + Tailwind 4 + bits-ui (Svelte port of Radix UI)",
				BestFor: []string{"Svelte 5 component DNA, Button, Card, Dialog, Tabs, Form, Select, Combobox, DatePicker, DataTable, Toast", "Component-library patterns the agent's interactive islands should follow", "Ecom UI: Cart drawer, Variant picker, Quickview modal, built on Sheet + Dialog + Select", "Calendar/date pickers (calendar-01 through calendar-10 in registry/blocks)", "Form patterns with proper a11y + validation"},
				License: "MIT",
				Notes:   "The OFFICIAL Svelte port of shadcn, same design language, same component DNA, but Svelte 5 instead of React. ALIGNED with atomicsite's stack: the admin frontend already uses Svelte 5 + SvelteKit + Tailwind 4. When atomicsite renders interactive islands (cart drawer, variant picker, search-as-you-type, modals), this is the design vocabulary to copy. docs/src/lib/registry/blocks/ has 100+ production block compositions including calendars, dashboards, login flows. docs/src/lib/registry/ui/ has the primitives (Button, Card, Dialog, etc).",
			},
			{
				Path:    "automations/design-references/shadcn-ui/",
				Stack:   "Next.js + React + Tailwind 4 + Radix UI",
				BestFor: []string{"Component design language gold standard (50k+ stars)", "Cross-framework component vocabulary", "Tailwind class composition patterns", "Radix accessibility primitives mapped to common components"},
				License: "MIT",
				Notes:   "Sparse-checkout of apps/v4/registry + apps/v4/lib only (~9MB instead of full 111MB monorepo). React stack, does NOT match atomicsite's output. Used as cross-stack reference for: Tailwind class composition patterns, accessibility patterns (Radix), component prop shapes. When you need the canonical 'how should a Combobox / DatePicker / DataTable look', read this. When you need it in our stack, read shadcn-svelte instead.",
			},
		},

		AntiPatterns: []AntiPattern{
			// Typography
			{Banned: "Inter font (anywhere)", Preferred: "Space Grotesk (heading), Inter is acceptable as body fallback only when paired with a display heading. For premium feel: Geist, Outfit, Cabinet Grotesk, Satoshi, Switzer, Plus Jakarta Sans.", HowInAtomicsite: "Update branding.font_heading to Space Grotesk (default already) or one of the premium families. Do NOT set font_heading to Inter. Body can be Inter for legibility but the eye reads heading first."},
			{Banned: "Roboto, Open Sans, Helvetica, Arial as heading fonts", Preferred: "Same as above, Space Grotesk default, Geist/Outfit/Cabinet Grotesk for character.", HowInAtomicsite: "branding.font_heading. The Google Fonts link in the Astro layout already loads Inter + Space Grotesk + Space Mono; Geist/Outfit etc. require updating the layout's <link> href."},
			{Banned: "Pure black (#000000) anywhere", Preferred: "Off-black: zinc-950 (#0a0a0a), charcoal (#18181b, atomicsite default --color-text), or a tinted dark.", HowInAtomicsite: "Default --color-text is #18181b. Don't override branding.text_color to #000000."},
			{Banned: "Generic shadow-md / shadow-lg / shadow-xl on cards", Preferred: "Tinted shadows that carry the bg hue. Or no shadow + 1px subtle border. Or inner shadow for elevation.", HowInAtomicsite: "Atomicsite's renderer ships subtle box-shadows tuned for each block (.replacement-card, .pricing-tier, .about-split-image). Do not override via style_json, the defaults match this rule."},
			{Banned: "Oversaturated brand colours (saturation > 80%)", Preferred: "Desaturated, muted accents (max ~75% saturation). Brand colour appears in 4-7 places per page max.", HowInAtomicsite: "branding.primary_color. Avoid pure #FF0000, #00FF00, #0000FF. Use OKLCH-tuned colours or muted variants like #0E7490 (BI's teal)."},
			{Banned: "Purple/blue 'AI gradient' aesthetic (the most common AI fingerprint)", Preferred: "Neutral bases (Zinc/Slate) with one considered accent: Emerald, Electric Blue, Deep Rose, Burnt Orange, Forest Green.", HowInAtomicsite: "Set branding.primary_color to a single non-purple accent. NEVER use linear gradients on backgrounds (atomicsite has no gradient block by design)."},
			{Banned: "Mixing warm and cool grays in the same site", Preferred: "Pick ONE gray family. All borders, muted text, surfaces follow it.", HowInAtomicsite: "branding.border_color + muted_color + surface_color must share a temperature with text_color + bg_color."},
			{Banned: "Linear or ease-in-out CSS transitions", Preferred: "Custom cubic-bezier with weight: cubic-bezier(0.16, 1, 0.3, 1) for 'soft landing', cubic-bezier(0.32, 0.72, 0, 1) for premium feel.", HowInAtomicsite: "Atomicsite's CSS uses ease-out + 150-200ms by default. For richer feel, the agent can author CSS classes via /api/agent/css-classes; the renderer respects them."},
			// Icons
			{Banned: "Lucide / Feather icons in their default state", Preferred: "Phosphor Light (icon stroke 1.5), Radix Icons, Heroicons. Standardize stroke width across all icons.", HowInAtomicsite: "Atomicsite ships a curated 52-icon Lucide subset in internal/builder/icons.go. They're already tuned (stroke 2). For non-marketing icon needs the agent registers a custom component via /api/agent/components."},
			{Banned: "Cliché icons: rocket for 'Launch', shield for 'Security', cog for 'Settings'", Preferred: "Less obvious metaphors: bolt for speed, fingerprint for security, spark for launch, vault for storage.", HowInAtomicsite: "feature_grid items[].icon picks from the icon dictionary. Pick the less-obvious match."},
			// Layouts
			{Banned: "Three equal cards horizontally as feature row", Preferred: "2-column zig-zag, asymmetric grid (replacement_grid with .is-wide spans), horizontal scroll, or masonry.", HowInAtomicsite: "Use replacement_grid (which has .is-wide span variants for visual interest), or feature_grid with 4 items (auto-flow handles 2/2 on tablet, 4-up on desktop)."},
			{Banned: "Centered hero with text over an image", Preferred: "Asymmetric: left-aligned content with image right (split_hero), or centered text WITHOUT image but with circuit-canvas / mesh-gradient bg.", HowInAtomicsite: "split_hero (image right) OR hero with bg=circuit (canvas animation). Don't use hero with image_id + center alignment, atomicsite's hero is always centered without image."},
			{Banned: "h-screen on hero (iOS Safari viewport bug)", Preferred: "min-h-[100dvh] always.", HowInAtomicsite: "Atomicsite's .block--hero uses min-height: clamp(36rem, 70vh, 50rem), already correct. Do not override via style_json."},
			{Banned: "Edge-to-edge content (no max-width container)", Preferred: "Container max-width 1024-1440px (--container-width).", HowInAtomicsite: "general.container_width setting (narrow|default|wide|fluid). Default is 72rem (1152px), leave it alone unless the brand demands otherwise."},
			{Banned: "Symmetric vertical padding (top = bottom always)", Preferred: "Optical adjustment, bottom often slightly larger.", HowInAtomicsite: "Renderer's defaults have this baked in (.block--hero { padding-block: 6rem 5rem }). Don't override."},
			// Visual
			{Banned: "Generic 1px solid gray border on every card", Preferred: "Hairline border at 8-12% opacity of text colour. Tinted to bg.", HowInAtomicsite: "Renderer uses border: 1px solid color-mix(in oklab, var(--color-text) 10%, transparent), already correct."},
			{Banned: "Edge-to-edge sticky navbars glued to top", Preferred: "Floating glass pill or detached fixed bar with mt-6. backdrop-blur on fixed elements only.", HowInAtomicsite: "Atomicsite ships a sticky h-14 header with backdrop-blur. Don't author custom navbars."},
			{Banned: "Filling sections with full-width container + max char count", Preferred: "Narrow content columns. Subheading max-width 36-50ch. Heroic whitespace.", HowInAtomicsite: "Renderer enforces max-width per block (.block--text 42rem, .block--accordion_faq 56rem). Don't override."},
			// Content
			{Banned: "John Doe / Jane Smith / Sarah Chan in testimonials", Preferred: "Realistic, specific names (e.g. Anna Lindqvist, Mehdi Ahmadi, Tom Isgren).", HowInAtomicsite: "Quote blocks + about_split. Author real names, never the AI defaults."},
			{Banned: "Round fake numbers: 99.99%, 50%, $100.00, 1234567", Preferred: "Organic data: 47.2%, 599 audited, +46 76 297 80 35, $4,892.", HowInAtomicsite: "stat_grid items[].value, replacement_grid descriptions, FAQ answers. Specific > round."},
			{Banned: "Acme Corp / Nexus / SmartFlow / TechFlow in logo bars", Preferred: "Real customer names (with consent) or a 'Founder previously worked with' frame using real prior employers.", HowInAtomicsite: "logo_carousel items[].label OR logo_strip items[].alt. Real names build trust; fakes destroy it."},
			{Banned: "Title Case On Every Heading", Preferred: "Sentence case for headlines (better readability), Title Case only for product names + section eyebrows.", HowInAtomicsite: "headline + heading + eyebrow fields. Default to sentence case."},
			{Banned: "Exclamation marks in success messages and CTAs", Preferred: "Be confident, not loud. 'Audit booked.' beats 'Audit booked!'", HowInAtomicsite: "cta_text, form submit_label, FAQ answers."},
			// Behaviour
			{Banned: "AI copywriting clichés: Elevate, Seamless, Unleash, Next-Gen, Game-changer, Delve, Tapestry, In the world of...", Preferred: "Concrete verbs (replace, audit, migrate, save, ship, host, sell). Specific nouns (CRM, server, GDPR, EU, audit).", HowInAtomicsite: "All copy fields. The instant fix: read aloud, if it sounds like marketing fluff, rewrite."},
			{Banned: "Lorem ipsum or 'placeholder copy' anywhere", Preferred: "Real draft copy. The 5-minute version of real copy beats perfect Lorem.", HowInAtomicsite: "Never ship a block with Lorem text. The text field is content, not a placeholder."},
		},

		VibeArchetypes: []VibeArchetype{
			{
				Name:         "Soft Structuralism",
				BestFor:      []string{"B2B SaaS marketing", "Health/wellness", "Consumer apps", "Portfolio/agency"},
				Palette:      "Warm neutrals, bg #fafaf9 (warm white), surface-elevated #f5f5f4, text #18181b. Single accent (teal/emerald/burnt-orange).",
				Typography:   "Massive bold Grotesk for headlines (Space Grotesk 700, clamp 2.5-5rem, tracking -0.025em). Inter body. Space Mono eyebrows + brand wordmark.",
				Materiality:  "Airy, floating components. Diffused 0.05-opacity shadows. 1rem radius. No glass. No gradients.",
				BgColor:      "#fafaf9",
				PrimaryColor: "#0E7490",
				TextColor:    "#18181b",
				FontHeading:  "Space Grotesk",
				FontBody:     "Inter",
				ApplyVia:     "Default atomicsite branding maps here. Set branding.bg_color=#fafaf9, primary_color=brand-accent, text_color=#18181b. Use bg=circuit on hero for tech/security/infra; bg=image otherwise. This is the atomicsite default and the right pick for ~70% of marketing sites.",
			},
			{
				Name:         "Editorial Luxury",
				BestFor:      []string{"Lifestyle brands", "Real estate", "Creative agencies", "High-end consumer"},
				Palette:      "Warm cream #FDFBF7, muted sage / deep espresso accents. High-contrast.",
				Typography:   "Variable serif headlines (Lyon, Newsreader, Playfair, Instrument Serif) + Inter / Switzer body. Tight serif tracking -0.03em line-height 1.1.",
				Materiality:  "Subtle CSS noise/film-grain overlay (opacity-[0.03]) for paper-like physical texture. Mixed aspect ratios (4:3 next to 16:9). Asymmetric content blocks.",
				BgColor:      "#FDFBF7",
				PrimaryColor: "#5C4033",
				TextColor:    "#1F1A14",
				FontHeading:  "Playfair Display",
				FontBody:     "Inter",
				ApplyVia:     "branding.bg_color=#FDFBF7, primary_color=#5C4033 (espresso) or #6B7359 (sage), font_heading=Playfair Display. Layout's Google Fonts link must be extended to load Playfair Display, currently only Inter+Space Grotesk+Space Mono are loaded. Apply ONLY when the brand is editorial/lifestyle.",
			},
			{
				Name:         "Ethereal Glass",
				BestFor:      []string{"AI/ML products", "Premium SaaS dashboards", "Tech infrastructure", "Crypto/Web3"},
				Palette:      "Deepest OLED black #050505 background. Vantablack cards. Subtle glowing radial mesh gradients (purple/emerald/blue orbs).",
				Typography:   "Wide geometric Grotesk (Geist Sans, Plus Jakarta) for everything. Mono for stats + brand. NO serif.",
				Materiality:  "Heavy backdrop-blur-2xl on cards. Pure white/10 hairlines. True glass with 1px inner border + inset shadow.",
				BgColor:      "#050505",
				PrimaryColor: "#06B6D4",
				TextColor:    "#FAFAFA",
				FontHeading:  "Geist",
				FontBody:     "Geist",
				ApplyVia:     "branding.bg_color=#050505, primary_color=#06B6D4 (electric cyan) or #10B981 (emerald), text_color=#FAFAFA. Layout Google Fonts link must include Geist (currently only loads Inter+Space Grotesk). Use sparingly, dark sites convert worse than light for B2B; only ship this for genuine 'we build futuristic AI' brands.",
			},
			{
				Name:         "Neo-Brutalist Landing",
				BestFor:      []string{"Product launches", "Indie SaaS", "Fundraising / waitlist pages", "Fintech disruptors", "Developer tools with attitude"},
				Palette:      "Warm-white #faf9f6 base, deep charcoal #18181b text, single saturated accent picked once (electric blue #2563eb, raspberry #e11d48, mustard #ca8a04, or lime #65a30d). Brand colour gets BIG real-estate, used as full-bleed section bands, not just stroke.",
				Typography:   "Mono headlines (Space Mono 700, JetBrains Mono 700) at clamp(3rem, 7vw, 5.5rem), tight tracking -0.02em. Body in geometric grotesk (Space Grotesk or Geist Sans). No serif.",
				Materiality:  "Thick 2-3px solid borders (var(--color-text)). Hard offset shadows: 4px 4px 0 var(--color-text). NO blur, NO color-mix, NO squircle radii, 0px or 4px max. Buttons are rectangles with the same offset shadow that snaps to 2px 2px on hover.",
				BgColor:      "#faf9f6",
				PrimaryColor: "#2563eb",
				TextColor:    "#18181b",
				FontHeading:  "Space Mono",
				FontBody:     "Space Grotesk",
				ApplyVia:     "branding.bg_color=#faf9f6, primary_color=<one saturated hex>, text_color=#18181b, font_heading=Space Mono. Override the renderer's default card materiality via a site-scoped css class (`brutal-card { border: 2px solid var(--color-text); box-shadow: 4px 4px 0 var(--color-text); border-radius: 0; }`) authored through /api/agent/css-classes. Use sparingly, neo-brutalism wins attention but loses trust on enterprise B2B. Best for: 'we ship fast, we don't take ourselves too seriously' brands.",
			},
			{
				Name:         "Soft Claymorphism",
				BestFor:      []string{"Consumer apps", "Wellness", "Education / kids' products", "Onboarding flows", "Playful brands"},
				Palette:      "Warm cream #fff8f1 base, mint #d1fae5 or peach #fed7aa accents, deep charcoal #292524 text. Pastel saturation max 35%. Brand colour appears as pastel surface fills (whole cards), not stroke.",
				Typography:   "Rounded geometric grotesk (Outfit, Plus Jakarta Sans, Quicksand) at clamp(2.5rem, 5.5vw, 4.5rem). Body in same family, weight 500. Mono for tiny labels only.",
				Materiality:  "1.25-1.75rem radii (puffy, never sharp). Soft even shadows: 0 12px 24px -8px color-mix(in oklab, var(--color-text) 8%, transparent). Cards feel pressed-out, not flat. Buttons have inset highlight (inset 0 1px 0 #fff) + outer soft shadow.",
				BgColor:      "#fff8f1",
				PrimaryColor: "#10b981",
				TextColor:    "#292524",
				FontHeading:  "Outfit",
				FontBody:     "Outfit",
				ApplyVia:     "branding.bg_color=#fff8f1, primary_color=#10b981 (mint) or #f97316 (warm orange), text_color=#292524, font_heading=Outfit. Override card radius via site-scoped css class (`clay-card { border-radius: 1.5rem; box-shadow: 0 12px 24px -8px color-mix(in oklab, var(--color-text) 8%, transparent); }`). Use when the brand wants to feel friendly + approachable + premium-but-not-corporate.",
			},
		},

		Materiality: MaterialityGuidance{
			Defaults: []string{
				"Cards (.replacement-card, .pricing-tier, .feature-grid-item, .faq-item) have 1px hairline border at ~10% text-opacity, soft 0.875-1rem radius, subtle box-shadow on hover only.",
				"Buttons (.btn-primary, .btn-secondary, .btn-accent) have 0.625rem radius, 0.875rem 1.75rem padding, no shadow by default, visible from contrast not from elevation.",
				"Hero+split_hero with bg=circuit overlay an animated SVG canvas behind content (block-circuit-canvas + script tag emitted automatically).",
				"Images (.split-hero-img, .about-split-image) get 1rem radius + 0 16px 48px diffused tinted shadow.",
				"Pricing featured tier flips to dark fill (--color-text background) with brand-coloured CTA, no extra elevation needed.",
			},
			DoUse: []string{
				"Tinted shadows that carry the bg hue: color-mix(in oklab, var(--color-text) 12%, transparent). Built into renderer defaults.",
				"Hairline borders ≤ 10% opacity. Anything darker reads as a 'cheap card outline'.",
				"Inner highlight on glass-style cards: shadow-[inset_0_1px_0_rgba(255,255,255,0.1)] for top-edge refraction.",
				"Squircle radii (0.875-1.25rem). Pure pill (rounded-full) ONLY for badges, lang switchers, never on large containers.",
				"Concentric radii on nested elements: outer rounded-[1rem] + inner rounded-[calc(1rem-0.375rem)] = visual harmony.",
				"Generous padding inside cards: 1.5-2rem. Cramped cards read low-end.",
			},
			DontUse: []string{
				"shadow-md / shadow-lg / shadow-xl Tailwind defaults, too generic, untinted.",
				"Pure black box-shadows (rgba(0,0,0,0.3+)). Always at most ~12% mixed with bg hue.",
				"Neon outer glows. The 'AI fingerprint' look. Banned.",
				"Generic white cards on white bg with shadow-sm. Either remove the card OR commit to a different surface.",
				"Multiple radii in the same view (4px next to 16px next to 24px). Pick a radius family per archetype.",
				"backdrop-blur on scrolling content. Apply only to fixed/sticky elements (the navbar already has it).",
			},
			HowToApply: []string{
				"For 99% of cards, use the renderer defaults, atomicsite has all these patterns wired into the per-block CSS rules.",
				"To customise card materiality across a site, write CSS classes via PUT /api/agent/css-classes. Don't author per-block style_json overrides.",
				"For an 'expensive feel' upgrade: bump general.container_width to 'wide', use bg=circuit hero, mark a stat as featured (it auto-promotes visually).",
			},
		},

		ContentAuthenticity: ContentRules{
			BannedNames: []string{
				"John Doe", "Jane Smith", "Jane Doe", "Sarah Chan", "Jack Su", "Mike Johnson", "Test User",
			},
			BannedNumbers: []string{
				"99.99%", "100%", "50%", "$100.00", "$1000", "1234567", "$9.99", "10x faster", "100x", "999",
			},
			BannedCompanies: []string{
				"Acme Corp", "Acme Inc", "Nexus", "SmartFlow", "TechFlow", "Innovate Co", "Synergy", "ClientCo", "Example Inc",
			},
			BannedPhrases: []string{
				"Elevate", "Seamless", "Unleash", "Next-Gen", "Game-changer", "Delve", "Tapestry",
				"In the world of", "Imagine a world where", "Welcome to the future of",
				"Revolutionary", "Disruptive", "Cutting-edge", "World-class", "Best-in-class",
				"Lorem ipsum", "Coming soon", "Click here", "Learn more (as a bare CTA, be specific)",
			},
			StyleRules: []string{
				"Sentence case for headlines, not Title Case.",
				"Confident period instead of exclamation. 'Audit booked.' not 'Audit booked!'",
				"Active voice always. 'We couldn't save your changes' not 'Mistakes were made'.",
				"Specific over general: '599 Swedish law firms audited' beats '500+ projects shipped'.",
				"Concrete verbs: replace, audit, migrate, save, ship, host, sell, not Elevate, Unleash.",
				"Numbers with context: 100/100 + 'Industry average 73' is 10x stronger than just '100/100'.",
				"Phone numbers in real local format: '+46 76 297 80 35' beats '1-800-555-0100'.",
				"Email addresses on a domain you control. Never gmail/outlook for company contact.",
			},
			HowInAtomicsite: "All copy ships through the agent API: hero/split_hero text fields, stat_grid items, replacement_grid descriptions, pricing tier names, FAQ Q+A. The renderer doesn't validate copy quality, that's on the agent. Read aloud before saving: if it sounds like marketing fluff, rewrite.",
		},

		Motion: MotionGuidance{
			StackReality: "Atomicsite renders STATIC Astro. No React, no Framer Motion, no client-side reactivity beyond CookieProof + the hero circuit canvas + the visitor-hydration script. ALL animation is CSS-only. Anything that requires JS state has to register as a custom component via /api/agent/components or live in a custom block_type.",
			DoUse: []string{
				"CSS transitions on :hover and :active states (already wired on .btn-*, .replacement-card, .faq-item).",
				"CSS animations for marquees (logo_carousel uses a 30s linear infinite translate).",
				"CSS @keyframes for circuit-bg canvas (already shipped as embedded JS asset).",
				"Custom cubic-bezier timing: cubic-bezier(0.16, 1, 0.3, 1) for soft landing, cubic-bezier(0.32, 0.72, 0, 1) for premium overshoot.",
				"Transform + opacity ONLY for animated properties (GPU-accelerated).",
				"@media (prefers-reduced-motion: reduce) overrides, already applied to .logo-carousel-track. Honor it on any custom motion.",
			},
			DontUse: []string{
				"window.addEventListener('scroll'), kills mobile performance.",
				"useState magnetic buttons or hover trackers (not even available, atomicsite is server-rendered).",
				"Animating top/left/width/height, layout-thrashing, slow.",
				"GSAP, ThreeJS, Lottie unless registered as a custom component (heavy + needs CSP allow-list).",
				"Animations that fire on initial load without prefers-reduced-motion check.",
				"Continuous animations on scrolling containers (causes GPU repaints).",
				"More than ONE perpetual animation per viewport. The marquee+circuit+pulse+drift+accent-draw stack reads as 'AI-builder demo', not a real product. Pick one perpetual signal, make everything else load-once choreography.",
				"Entry-choreography staggers longer than 80ms between siblings, long staggers make the page feel slow, not premium.",
				"Parallax. Scroll-tied transforms of any kind. Sticky full-section backgrounds (only the navbar is sticky).",
				"Accent-color animations (underline draws, color shifts) replayed on hover. They fire ONCE per page load, then settle.",
			},
			Performance: []string{
				"transform + opacity only. Atomicsite's CSS is already this strict.",
				"will-change: transform sparingly, only on actively-animating elements.",
				"backdrop-blur on FIXED elements only (navbar has it; nothing else should).",
				"Avoid > 1s transitions on hover, visitors lose context.",
			},
			A11y: []string{
				"prefers-reduced-motion: reduce → set animation: none + transition: none on motion elements.",
				"Focus indicators stay visible always, never use :focus { outline: none } without :focus-visible alternative.",
				"Don't use motion as the ONLY signal for state change (e.g. don't only fade-in success, also change colour + icon).",
			},
		},

		StrategicOmissions: []OmissionItem{
			{Item: "Skip-to-content link", Importance: "WCAG required, eval check", Status: "Auto", HowApplied: "Layout emits <a href='#main' class='sr-only-focusable'> as first focusable element on every page."},
			{Item: "Custom 404 page", Importance: "Brand consistency + conversion recovery", Status: "Auto-seeded but agent should populate copy", HowApplied: "atomicsite's wizard creates /<lang>/404 pages. Agent should author hero + a couple of links to popular pages, don't ship the default."},
			{Item: "Favicon + apple-touch-icon", Importance: "SEO + brand presence in tabs/bookmarks", Status: "Auto-emitted (degrades silently if missing)", HowApplied: "Upload favicon.ico + apple-touch-icon.png to media library folder='brand'. Set branding.favicon_id."},
			{Item: "OG image (1200x630 social card)", Importance: "Conversion on shared links", Status: "Auto-emitted from sites.og_image_id with proper meta tags + width/height", HowApplied: "Upload OG image to media. Set seo.og_default_image_id."},
			{Item: "Privacy + Terms + Cookie policy pages", Importance: "Legal requirement + privacy eval check", Status: "Auto-seeded with starter content, agent populates", HowApplied: "Pages exist at /<lang>/privacy, /terms, /cookies. Edit with create_block on a text block."},
			{Item: "Cookie consent banner", Importance: "GDPR + privacy eval check", Status: "Auto when analytics.cookieproof_enabled=1", HowApplied: "bulk_upsert_settings analytics.cookieproof_enabled=1. CookieProof banner emits automatically."},
			{Item: "Form validation (HTML5 required + email + tel)", Importance: "UX + a11y eval", Status: "Renderer emits required + type=email/tel/url", HowApplied: "form block items[].required=true, items[].type=email/tel."},
			{Item: "JSON-LD Organization + FAQPage schema", Importance: "AI search + Google rich results", Status: "Auto", HowApplied: "Layout emits Organization JSON-LD. accordion_faq emits FAQPage JSON-LD inline."},
			{Item: "robots.txt + sitemap.xml + llms.txt", Importance: "SEO + AI search", Status: "Auto", HowApplied: "Builder writes all three. Agent only sets seo.canonical_base, seo.sitemap_enabled, seo.same_as."},
			{Item: "Hreflang alternates for multi-lang", Importance: "International SEO eval", Status: "Auto when general.additional_langs is set", HowApplied: "bulk_upsert_settings general.additional_langs='en,sv', general.hreflang_strategy='path'."},
			{Item: "Security headers (CSP, HSTS, X-Frame-Options, etc)", Importance: "Security eval (A grade)", Status: "Auto, with sane defaults", HowApplied: "Defaults are good. Use settings_catalog security.* keys (admin-writable) only when adding allowed domains for embeds."},
			{Item: "'Back to top' / current-page nav indicator / breadcrumbs", Importance: "Long-page UX", Status: "Manual, agent authors", HowApplied: "Add a breadcrumb section as a text block, or use the BreadcrumbList JSON-LD pattern (currently a platform gap on /en/404)."},
			{Item: "Loading + empty + error states for forms", Importance: "Form UX", Status: "HTML5 default for now (custom states need a custom component)", HowApplied: "form block uses native browser validation. For richer flows, register a component via /api/agent/components."},
			{Item: "Branded 'Powered by' or attribution", Importance: "Optional, most sites omit", Status: "Manual", HowApplied: "Footer block columns or copyright field."},
		},

		AuditChecklist: []AuditItem{
			{Check: "Exactly one hero or split_hero block, exactly one h1 on the page.", Why: "Heading hierarchy + Single H1 eval check."},
			{Check: "Hero has primary CTA (cta_text + cta_url) AND a secondary_label pointing further down the page (#anchor).", Why: "Hesitant visitors need a soft path or they bounce."},
			{Check: "stat_grid has 3-4 items with {value, label, context}, not 1-2 generic values.", Why: "AI-Friendly Formatting eval check requires ≥3 list items. Context line carries social proof."},
			{Check: "Pricing has exactly one tier with featured=true.", Why: "Decision paralysis kills conversion. The visual default has to make the choice obvious."},
			{Check: "FAQ has 5-7 items, each answer 2-4 sentences with at least one concrete number.", Why: "Search engines + AI search reward specificity. Single-sentence answers under-fill snippets."},
			{Check: "Final CTA destination matches hero CTA destination.", Why: "Two booking links to different calendars confuses; one repeated booking link reinforces."},
			{Check: "Header has brand badge + 5-9 nav items + ONE explicit CTA (cta=true).", Why: "Header is the second-most-clicked surface. Coherent nav signals competent product."},
			{Check: "Footer has 3-4 columns with legal links present, copyright with real org-nr if relevant.", Why: "Legal compliance + trust signal."},
			{Check: "Every block reads on a 375px column without horizontal scroll, broken images, or overflow.", Why: "60-70% of marketing-site traffic is mobile. The renderer's @media handles most cases, but custom blocks can break."},
			{Check: "No copy contains banned phrases (Elevate, Seamless, Unleash, etc) or fake numbers (99.99%, etc).", Why: "AI tells destroy the 'real product' feel."},
			{Check: "Brand primary colour appears in 4-7 places per page, NOT as a section background.", Why: "Brand budget rule. Primary as bg makes the page scream."},
			{Check: "Heading font is NOT Inter, it's Space Grotesk, Geist, or another premium grotesk.", Why: "Inter as the heading font is the AI-default tell."},
			{Check: "Site builds successfully on the first trigger_build call. No empty blocks, no broken anchors, no missing images.", Why: "If the first build fails, the agent's design choices haven't been tested. Iterate eval until clean."},
			{Check: "Eval grade ≥ B on all categories. Splash and 404 aside, all agent-fixable items pass.", Why: "Eval is the platform's quality scoreboard. Below B = ship blocker."},
			{Check: "OG image is a real designed image (not a screenshot of the homepage).", Why: "Social sharing previews are seen by 10x more people than the page itself."},
			{Check: "Final page count ≤ 11 (homepage + legal + 404 EN/SV pairs). More = scope creep.", Why: "Scope discipline. Sites grow on what's needed, not what's possible."},
		},

		IconPolicy: IconRules{
			UseSet:      "Lucide subset, atomicsite ships 52 curated icons in internal/builder/icons.go (Mail, Server, Lock, Workflow, FileText, MessageCircle, Shield, ChevronRight, Linkedin, Github, Sparkles, BarChart, Layers, Cloud, etc).",
			StrokeWidth: "2 (atomicsite default). Standardized across the icon set so feature_grid items don't visually clash.",
			Banned: []string{
				"Lucide rocket for 'Launch' (cliché). Use Sparkles or BarChart instead.",
				"Shield for 'Security' (cliché). Use Lock or Fingerprint variant.",
				"Cog for 'Settings' (cliché). Use Layers or Workflow.",
				"Standard SVG 'egg' avatar placeholders.",
				"Emojis in any icon position. Banned by atomicsite's voice rules + WCAG.",
			},
			Available: []string{
				"To list available icons: read internal/builder/icons.go on disk, or query graphify.",
				"Common usable icons: Mail, Phone, MapPin, Lock, Shield, Server, Cloud, Database, Workflow, Sparkles, Zap, BarChart, TrendingUp, Layers, FileText, MessageCircle, ArrowRight, Check, ChevronDown, Linkedin, Github, Twitter, Instagram.",
			},
			HowToUse: "feature_grid items[].icon = 'Mail' (case-sensitive Pascal name). Renderer looks up the SVG in icons.go and embeds it inline. Unknown icons fall back to a generic placeholder, check the icon dictionary first.",
		},

		Fonts: FontGuidance{
			Philosophy: "Self-hosted woff2 only. NEVER Google Fonts. Two reasons: (1) Google Fonts CSS doesn't carry SRI hashes, fails the Subresource Integrity eval check; (2) every font fetch leaks visitor IP to Google, which clashes with the EU-sovereignty positioning many atomicsite tenants ship. Self-hosting via the per-site upload mechanism is one POST + one branding setting away. All recommended families below are SIL OFL 1.1 licensed, free to self-host with attribution preserved in the woff2 metadata.",
			System: []string{
				"Per-site fonts live in the `site_fonts` table (id, site_id, family_name, weight, style, source_url, file_path, ...).",
				"Upload accepted formats: woff2 only (rejects woff, ttf, otf, woff2 compresses ~30% better and is universally supported).",
				"Storage: {DataDir}/fonts/{site_id}/{font_id}.woff2.",
				"Public serving: GET /atomicsite-fonts/{siteID}/{fontID}.woff2, same-origin, no CORS, no CSP exception needed.",
				"The Astro layout auto-emits @font-face rules + <link rel=preload> for every uploaded font. You don't write CSS, uploading is enough.",
				"When no fonts are uploaded, sites fall back to system-ui via the --font-heading / --font-body custom properties. Still readable, just system-styled.",
			},
			APIEndpoints: []FontEndpoint{
				{Method: "GET", Path: "/api/agent/fonts", Use: "List every font uploaded to the current site. Returns {fonts: [{id, family_name, weight, style}]}."},
				{Method: "POST", Path: "/api/agent/fonts", Use: "Upload a woff2 file. Multipart/form-data: family_name (string), weight (100-900), style (normal|italic), file (woff2 binary)."},
				{Method: "DELETE", Path: "/api/agent/fonts/{id}", Use: "Remove an uploaded font."},
				{Method: "GET", Path: "/atomicsite-fonts/{siteID}/{fontID}.woff2", Use: "Public font serving (used by the rendered site, not by the agent directly)."},
			},
			AdminUI: []string{
				"Path: /sites/{siteID}/branding (the same page where palette + colours live).",
				"Section: 'Fonts', shows currently uploaded fonts in a table, with weight + style + delete affordance.",
				"Upload affordance: 'Upload font' button → file picker for woff2, then a small form for family_name / weight / style.",
				"Setting form below the table: dropdown for `font_heading` and `font_body` populated from the uploaded fonts list + system fallbacks.",
			},
			UploadFlow: []string{
				"1. Pick families: 1 heading + 1 body + (optional) 1 mono. See `recommended` below for vetted picks.",
				"2. Download the variable woff2 files: fontsource.org or rsms.me/inter. SIL OFL 1.1, free to redistribute.",
				"3. Upload via admin UI (/sites/{id}/branding) OR via curl with multipart form to /api/agent/fonts.",
				"4. Set `branding.font_heading` and `branding.font_body` to the uploaded family_name strings (case-sensitive).",
				"5. Trigger build. Layout auto-emits @font-face + preload tags. No further wiring.",
			},
			Recommended: []FontFamily{
				{
					Name:        "Inter",
					GoodFor:     []string{"Body text on B2B SaaS sites", "Form labels", "Long-form prose", "Default fallback paired with a display heading font"},
					License:     "SIL OFL 1.1",
					DownloadFrom: "https://rsms.me/inter/download/ (Variable woff2). Or @fontsource/inter on npm.",
					Notes:       "The most-used display+body font on the web. Geometric, neutral, hyper-legible. Good default body. AVOID using as the heading font when 'premium' is the goal, Inter as h1 is the AI-default tell.",
				},
				{
					Name:        "Space Grotesk",
					GoodFor:     []string{"Display headlines on tech/SaaS marketing sites", "Soft Structuralism vibe archetype", "Pairs well with Inter body + Space Mono eyebrow"},
					License:     "SIL OFL 1.1",
					DownloadFrom: "https://github.com/floriankarsten/space-grotesk (Variable woff2). Or @fontsource-variable/space-grotesk.",
					Notes:       "Wide geometric grotesk with character. Works at heading + display sizes. Used widely on premium marketing sites.",
				},
				{
					Name:        "Space Mono",
					GoodFor:     []string{"Eyebrows (uppercase, tracking-widest)", "Brand wordmarks", "Code labels", "Tier-step tags (STEP 01)"},
					License:     "SIL OFL 1.1",
					DownloadFrom: "https://fonts.google.com/specimen/Space+Mono (download → host the woff2). Or @fontsource/space-mono.",
					Notes:       "Atomicsite's --font-mono default. Use for ANY mono surface (eyebrow, code, brand badge, footer fine print).",
				},
				{
					Name:        "Geist + Geist Mono",
					GoodFor:     []string{"Premium SaaS dashboards", "Ethereal Glass vibe archetype", "AI/ML product marketing"},
					License:     "SIL OFL 1.1",
					DownloadFrom: "https://github.com/vercel/geist-font (Variable woff2). Or @fontsource/geist-sans + @fontsource/geist-mono.",
					Notes:       "Vercel's house font. Flatter geometric grotesk than Space Grotesk, very 'tech-product'. Best paired together (sans + mono) for a unified system feel.",
				},
				{
					Name:        "Outfit",
					GoodFor:     []string{"Premium consumer brands", "Health/wellness", "Lifestyle marketing"},
					License:     "SIL OFL 1.1",
					DownloadFrom: "https://fonts.google.com/specimen/Outfit (download → host). Or @fontsource-variable/outfit.",
					Notes:       "Geometric grotesk with a softer, friendlier silhouette than Space Grotesk. Good when the brand wants to feel approachable but still premium.",
				},
				{
					Name:        "Cabinet Grotesk",
					GoodFor:     []string{"Editorial Luxury vibe archetype", "Creative agencies", "Portfolio sites"},
					License:     "SIL OFL 1.1",
					DownloadFrom: "https://www.fontshare.com/fonts/cabinet-grotesk (Variable woff2, requires free Fontshare account).",
					Notes:       "Tighter, more characterful grotesk. Premium feel without the 'tech-product' association of Geist. Used by many agency sites.",
				},
				{
					Name:        "Satoshi",
					GoodFor:     []string{"Modern SaaS marketing", "Crypto/Web3", "Premium consumer"},
					License:     "Free for commercial use (Fontshare custom)",
					DownloadFrom: "https://www.fontshare.com/fonts/satoshi (requires free Fontshare account).",
					Notes:       "Sharper, more confident grotesk. Often paired with JetBrains Mono. Works well at all sizes.",
				},
				{
					Name:        "Plus Jakarta Sans",
					GoodFor:     []string{"International brands", "Fintech / professional services", "Multilingual sites (broad Latin/Cyrillic coverage)"},
					License:     "SIL OFL 1.1",
					DownloadFrom: "https://fonts.google.com/specimen/Plus+Jakarta+Sans. Or @fontsource-variable/plus-jakarta-sans.",
					Notes:       "Geometric grotesk with strong i18n coverage. Good when the site needs to render Cyrillic / Greek / Vietnamese without FOUT.",
				},
				{
					Name:        "Newsreader",
					GoodFor:     []string{"Editorial Luxury vibe", "Long-form journalism / publishing", "Lifestyle brands"},
					License:     "SIL OFL 1.1",
					DownloadFrom: "https://fonts.google.com/specimen/Newsreader. Or @fontsource-variable/newsreader.",
					Notes:       "Variable serif with optical sizing. The serif pick when you want editorial luxury without the dated feel of Playfair Display.",
				},
				{
					Name:        "Instrument Serif",
					GoodFor:     []string{"Editorial Luxury vibe", "Brands that want a 'magazine' feel", "Hero display type only"},
					License:     "SIL OFL 1.1",
					DownloadFrom: "https://fonts.google.com/specimen/Instrument+Serif.",
					Notes:       "Display-only serif with character. Use ONLY for the hero h1 or section headings, not body text. Pair with Inter body.",
				},
				{
					Name:        "JetBrains Mono",
					GoodFor:     []string{"Code blocks on developer sites", "Stat values on data-heavy dashboards", "Alternative to Space Mono"},
					License:     "SIL OFL 1.1",
					DownloadFrom: "https://www.jetbrains.com/lp/mono/ (woff2). Or @fontsource/jetbrains-mono.",
					Notes:       "Higher legibility at small sizes than Space Mono. Pick when the site is developer-heavy.",
				},
			},
			SystemFallback: "If no font is uploaded, atomicsite emits --font-heading: 'Space Grotesk', system-ui, sans-serif (where 'Space Grotesk' is just a name in the cascade with no actual file). The browser falls through to system-ui, readable but not the intended look. The agent should ALWAYS check `GET /api/agent/fonts` first; if empty, ask the user to upload via /sites/{id}/branding before claiming the site is design-complete.",
			HowToSet: "After uploading a font with family_name='Space Grotesk', call `bulk_upsert_settings` with category='general' (NOT possible, branding fields are separate), actually use the agent branding endpoint: PATCH /api/agent/branding {font_heading: 'Space Grotesk', font_body: 'Inter'}. The family_name string must match exactly what was used during upload (case-sensitive). Verify after build by checking the rendered global.css for the @font-face rule with the matching family-name.",
		},

		StackRecommendations: StackGuidance{
			Philosophy: "Atomicsite is Astro static by default, fastest TTFB, best SEO, lowest hosting cost. Reach for client-side reactivity ONLY when the page genuinely needs it (cart, search-as-you-type, real-time status). Avoid React; prefer Svelte 5 islands when interactivity is required because (1) atomicsite's admin already runs Svelte 5 + SvelteKit so the team has the muscle memory, (2) Svelte's compiled output is smaller than React's runtime, matters for ecom CWV, (3) Astro + Svelte islands hydrate per-component, not per-page.",
			Stacks: []StackVariant{
				{
					Name:        "Static-only (default)",
					Use:         "Marketing sites, blogs, docs, landing pages, anything where every interaction is server-rendered or a plain anchor link. Covers ~80% of atomicsite use cases.",
					Composition: []string{"Astro 5 (output: static)", "Tailwind 4 + atomicsite's per-site CSS pipeline", "TypeScript only inside built-in widgets (CookieProof, hero canvas, hydration script)", "Forms POST to a worker URL or n8n webhook"},
					Constraints: []string{"No state across pages beyond cookies/localStorage", "No real-time updates", "Forms get HTML5 validation, not custom states"},
					HowApplied:  "Default. Just compose blocks via the agent API. No custom components needed.",
				},
				{
					Name:        "Astro + Svelte islands (light interactivity)",
					Use:         "Sites with one or two interactive moments: a search-as-you-type bar, a configurator, a calendar booking embed, a copy-to-clipboard button, a theme toggle that respects system preference.",
					Composition: []string{"Astro 5 static for the shell", "Svelte 5 islands registered via /api/agent/components", "shadcn-svelte primitives for component design DNA", "Tailwind 4 utilities inside Svelte components"},
					Constraints: []string{"Each island hydrates on its own, no shared state across islands without a tiny store", "Total island JS budget should stay under 50KB gzipped to preserve perf eval"},
					HowApplied:  "Register Svelte components via PUT /api/agent/components with name + props schema. Use block_type=component in pages, set data.component=<name> + data.props={...}. The renderer wires the Astro <Component client:load /> directive.",
				},
				{
					Name:        "Atomicsite first-party storefront (ecom)",
					Use:         "Selling physical or digital goods using Atomicsite's built-in catalog + checkout. Sprint 2 (2026-05-22) shipped the full stack: products / variants / inventory / discount codes / orders / Mollie checkout / webhooks / admin Orders UI.",
					Composition: []string{"Astro 5 static for catalog + product pages", "Four typed storefront blocks: product_grid, product_detail, cart_drawer, checkout_form", "One per-site vanilla JS island (_atomic-storefront.js) for cart state + checkout submit", "Cart state in localStorage (until checkout)", "Mollie hosted checkout for the transaction (NEVER PCI in our shell)", "Built-in: products / variants / inventory_adjustments / discount_codes / orders / payment_events tables", "Public endpoints: POST /api/sites/{id}/checkout, POST /api/sites/{id}/payments/mollie/webhook"},
					Constraints: []string{"Mollie is the only payment provider in v1 (Stripe / PayPal deferred)", "Single-currency per cart (mixed currencies rejected at checkout)", "VAT-inclusive prices, integer cents per ISO 4217", "VAT rates table + per-country lookup deferred to Sprint 1.5", "checkout_form.return_url must point to a real page on this site (Mollie redirects there with ?order=ATM-... appended)"},
					HowApplied:  "Set payments.mollie_api_key in site settings (Mollie test key or live). Create products + variants + (optional) discount codes via the admin Store tab. Author a catalog page with product_grid. Author a product page with product_detail (set product_slug). Author a /checkout page with checkout_form (set return_url=/thank-you). Place cart_drawer once as a global block so every page has the trigger. The storefront island is injected by the layout automatically.",
				},
				{
					Name:        "Astro + Stripe Checkout (paid digital goods, courses, subs)",
					Use:         "Selling subscriptions, courses, downloads, tickets, single product or simple catalog. Doesn't need full cart / variants / inventory.",
					Composition: []string{"Astro 5 static", "ONE Svelte island: the Buy button (hits a Go worker → Stripe Checkout session → redirects)", "Stripe-hosted Customer Portal for subscription management", "Webhooks for order fulfilment + license-key delivery"},
					Constraints: []string{"No cart needed (single-product or pick-one-of-N)", "All transactional UI is Stripe-hosted (Checkout + Customer Portal)"},
					HowApplied:  "Pricing block with cta_url=/checkout?plan=<id>. The /checkout route is a Go worker endpoint that creates the Stripe session and 302s. Saves building cart UI, variant pickers, etc.",
				},
			},
			Payments: PaymentRules{
				Philosophy: "Always use a hosted checkout (Stripe Checkout or Mollie hosted pages), never PCI inside atomicsite. Atomicsite is a static-site platform, not a payment processor; PCI compliance scope is exactly what we don't want. Both Stripe and Mollie are wired via the same pattern: Go worker creates session, redirect user, listen for webhook, update order in your data layer.",
				Providers: []PaymentProvider{
					{
						Name:       "Stripe Checkout",
						BestFor:    []string{"Global B2B / B2C", "Subscriptions (Billing)", "Card-heavy markets (US, UK, AU)", "Marketplace flows (Connect)", "Crypto opt-in"},
						Strengths:  []string{"Best developer experience by a wide margin", "Strong subscription primitives (Billing + Customer Portal)", "Handles SCA / 3DS automatically", "Apple Pay + Google Pay built-in", "Customer Portal eliminates building self-serve sub management UI", "Excellent dispute / fraud tooling (Radar)"},
						Weaknesses: []string{"~2.9% + €0.25 per EU transaction (higher than Mollie for SEPA-heavy traffic)", "iDEAL (Netherlands) and Bancontact (Belgium) cost 1.20%+, more than Mollie's flat €0.29", "Less native presence in DACH and Nordics consumer markets"},
						Geography:  "Global. Strongest in US/UK/AU. Acceptable in EU.",
						Methods:    []string{"Card (Visa/MC/Amex)", "Apple Pay", "Google Pay", "Klarna", "Afterpay/Clearpay", "iDEAL", "Bancontact", "SEPA Direct Debit", "ACH (US)"},
						Pricing:    "EU: 1.5% + €0.25 standard / 2.5% + €0.25 international. SEPA Direct Debit: 0.8%. Apple Pay / Google Pay: same as card.",
					},
					{
						Name:       "Mollie",
						BestFor:    []string{"EU-focused B2B / B2C", "NL / BE / DE / FR consumer", "Stores where iDEAL or Bancontact is the dominant method", "Brands that want to avoid US payment processors for political / sovereignty reasons"},
						Strengths:  []string{"Native EU company (Amsterdam), same compliance posture as Bright Interaction's GDPR / CLOUD-Act-free positioning", "Cheaper iDEAL (€0.29 flat) and Bancontact (€0.39 flat) than Stripe", "Cleaner invoice / B2B-style flows (Klarna Pay Later, In3)", "Better SEPA Direct Debit pricing for recurring payments", "Strong dashboard UX for non-technical operators"},
						Weaknesses: []string{"No subscription engine as polished as Stripe Billing, recurring needs more glue code", "Smaller ecosystem (fewer prebuilt integrations than Stripe)", "Customer Portal for self-serve sub management isn't first-class, you build it"},
						Geography:  "EU-first. Available globally but EU is where it's strongest.",
						Methods:    []string{"Card", "iDEAL", "Bancontact", "SEPA Direct Debit", "Klarna Pay Later", "In3", "Apple Pay", "Google Pay", "PayPal", "Belfius", "KBC", "Sofort (deprecating)"},
						Pricing:    "iDEAL: €0.29 flat. Bancontact: €0.39 flat. Card EU: 1.8% + €0.25. SEPA: €0.25. Often cheaper than Stripe for EU-only traffic.",
					},
				},
				WhenToPick: []PaymentPick{
					{IfCustomerIs: "Global SaaS subscription business", Pick: "Stripe Checkout + Stripe Billing", Why: "Best subscription primitives + Customer Portal saves building self-serve UI."},
					{IfCustomerIs: "EU-only B2C ecom (NL, BE, DE)", Pick: "Mollie", Why: "Cheaper iDEAL/Bancontact, native EU sovereignty story."},
					{IfCustomerIs: "EU B2B with international expansion plans", Pick: "BOTH, Stripe primary + Mollie for iDEAL/Bancontact local methods", Why: "Stripe's developer experience for the long tail; Mollie unblocks the Dutch/Belgian methods that Stripe charges more for."},
					{IfCustomerIs: "Bright Interaction itself or aligned 'EU sovereignty' brand", Pick: "Mollie primary + Stripe fallback", Why: "Customer-facing message is 'EU-hosted, GDPR-friendly, no US lock-in', Mollie aligns with that. Stripe stays as the edge case for non-EU customers."},
					{IfCustomerIs: "US / UK marketplace or platform with Connect-style splits", Pick: "Stripe", Why: "Mollie has nothing equivalent to Stripe Connect."},
					{IfCustomerIs: "Selling courses / digital downloads under €100", Pick: "Stripe Checkout", Why: "Faster setup, cleaner one-shot checkout, no need for Mollie's EU-method advantage on small purchases."},
				},
				HowApplied: "Add provider keys to settings: integrations.stripe_publishable_key + .stripe_secret_key + .stripe_webhook_secret, OR integrations.mollie_api_key + .mollie_webhook_secret. Buy/cart-button Svelte islands POST to a Go worker endpoint (atomicsite hosts no payment logic itself); the worker creates the Stripe Checkout session or Mollie payment, returns a redirect URL, the island redirects the browser. CSP must allow the provider's domain, set security.allowed_scripts (admin-only) for stripe.com / mollie.com if you embed their JS for Apple Pay etc. For both providers, the success/cancel return pages should be atomicsite pages with hide_global_blocks=1 for a clean conversion finish.",
			},
			WhenToPick: []StackPick{
				{IfSiteIs: "Marketing site, blog, docs, portfolio, contact page", Pick: "Static-only", Why: "No interactivity needed. Fastest TTFB, lowest hosting cost, perfect SEO."},
				{IfSiteIs: "Marketing site + a calendar booker / search bar / configurator / theme toggle", Pick: "Astro + Svelte islands (light interactivity)", Why: "Just need a single hydrated component for the interactive moment. Rest stays static."},
				{IfSiteIs: "Selling physical or digital goods with cart + variants + inventory", Pick: "Astro + Svelte + headless commerce (ecom)", Why: "Cart + variant picker + search-as-you-type + real-time stock all need client state. Static can't do that."},
				{IfSiteIs: "Selling a single course / subscription / download", Pick: "Astro + Stripe Checkout (paid digital goods)", Why: "No cart needed. One Svelte 'Buy' button is enough. Skip the ecom complexity."},
				{IfSiteIs: "Internal dashboard or admin tool", Pick: "Different platform, atomicsite is for public-facing static sites only", Why: "BrightCRM / dockyard / sentinel are the internal-tool platforms. Atomicsite would be the wrong fit (no auth, no real-time, no row-level access control)."},
			},
		},

		CopyVoice: VoiceRules{
			Tone: "Confident, specific, restrained. Like an expert who doesn't need to convince you, they're just stating what's true. Examples to match: Linear's docs, Stripe's marketing, Vercel's announcements, Resend's voice. Voices to AVOID: HubSpot's enthusiasm, Salesforce's corporate speak, generic SaaS template copy.",
			Eyebrow: []string{
				"3-4 words max. Mono font, uppercase, brand colour.",
				"Names the category the visitor is shopping for: 'OPEN SOURCE INFRASTRUCTURE', 'GDPR COMPLIANCE', 'EU HOSTING'.",
				"Never 'About Us', 'Our Services', 'Why Choose Us', that's filler.",
			},
			Headline: []string{
				"≤ 6 words. ≤ 18ch line-length. Sentence case.",
				"Transformation verb + concrete noun: 'Stop renting your business. Own it.', 'Replace SaaS with self-hosted.'",
				"Use [[bracket-accent]] on the verb that matters: 'Stop [[renting]]. Own it.'",
				"NEVER 'Welcome to X', 'Discover Y', 'The future of Z'.",
			},
			Subheading: []string{
				"≤ 2 sentences, ≤ 50ch wide.",
				"Names the painful current state + the relief atomicsite-style site delivers.",
				"Specific numbers if available: '200 EUR per employee per month' beats 'expensive software'.",
				"Avoid AI clichés (Elevate, Seamless, Unleash) at all costs.",
			},
			CTA: []string{
				"Action verb + specific outcome: 'Book audit', 'See pricing', 'Calculate savings'.",
				"NEVER 'Click here', 'Submit', 'Learn more' (alone, pair with destination), 'Get started' (vague).",
				"Primary CTA matches what the user wants to do, not what you want them to do.",
				"Secondary label often sends them lower on the page (#pricing, #faq), softer commitment.",
			},
			Forbidden: []string{
				"Marketing clichés: Elevate, Unleash, Seamless, Game-changer, Next-gen, Best-in-class, World-class.",
				"Vague nouns: solutions, offerings, experiences, journey, ecosystem.",
				"Generic openers: 'Welcome to', 'Imagine a world', 'In today's fast-paced'.",
				"Exclamation marks (!) in CTAs and success messages.",
				"Title Case On Every Heading.",
				"Passive voice: 'Mistakes were made' → 'We dropped the ball'.",
			},
		},

		HeroGraphics: []HeroGraphic{
			{
				Name:        "circuit",
				BestFor:     []string{"Tech", "Infrastructure", "Security", "DevOps", "B2B SaaS targeting engineers"},
				Description: "Animated SVG circuit-canvas drifting behind the hero content. Long-standing default for tech-leaning sites; signals 'we ship code'.",
				Materiality: "Canvas overlay at opacity ~0.6. Honors prefers-reduced-motion: reduce → static pattern.",
				Performance: "Embedded JS asset (~3kb). No layout cost. LCP unaffected (canvas draws after first paint).",
			},
			{
				Name:        "mesh",
				BestFor:     []string{"AI/ML products", "Premium SaaS", "Sites using the Ethereal Glass vibe", "Brand-led marketing pages"},
				Description: "Drifting oklab gradient mesh (color-mix of --color-primary + --color-text + --color-bg). Three orbs, 22s perpetual cycle, gated on prefers-reduced-motion.",
				Materiality: "Pure CSS, no SVG. Renders behind hero content with mask-image fade to bg at edges.",
				Performance: "~3kb of CSS. Animates transform + opacity only (GPU). LCP within budget, content paints over the mesh on first frame.",
			},
			{
				Name:        "pulse",
				BestFor:     []string{"Consumer / lifestyle", "Agencies", "Sites that want a signal of life without tech-product feel"},
				Description: "Centered radial pulse with 2.4s breathing animation in --color-primary at low opacity. Lovable-inspired signature graphic.",
				Materiality: "Single radial-gradient + box-shadow breathing. No SVG, no canvas.",
				Performance: "~1kb of CSS. Animates opacity + transform. Reduced-motion: opacity stays static.",
			},
			{
				Name:        "monogram",
				BestFor:     []string{"Editorial Luxury vibe", "Lifestyle brands", "Personal portfolios", "High-end consumer"},
				Description: "Large brand initial (configurable via hero.monogram_char, defaults to first letter of site name) in --font-heading at 18rem, set against an asymmetric off-grid position. Editorial print feel.",
				Materiality: "Pure typography. Uses var(--font-heading) at huge size, oklab color-mix tint of --color-text at 8% opacity.",
				Performance: "Zero animation. ~0.5kb CSS. Best LCP impact of the five (no compositing).",
			},
			{
				Name:        "audit-receipt",
				BestFor:     []string{"Atomicsite homepage", "Inspector / audit-style tools", "B2B SaaS selling 'we score / we grade / we audit'", "Trust-led product pages"},
				Description: "Mock browser frame showing a 100/100 inspector grade with 'Industry average 62' comparison and a CTA to run the audit. Doubles the hero as a lead-magnet surface.",
				Materiality: "Self-contained card with browser-chrome dots, monospace numerals, hairline border, tinted shadow. Fully accessible, readable as plain text by screen readers.",
				Performance: "~2kb CSS, no scripts. Real values come from hero.audit_score / hero.audit_baseline / hero.audit_label data fields.",
			},
		},

		DesignWorkflow: DesignWorkflow{
			WhenToInvoke: "BEFORE writing any custom HTML/CSS via the `custom` or `raw_astro` block_type, the agent invokes the installed design skill that maps to the chosen vibe. The skills carry years of curated rules that complement (don't duplicate) the playbook: anti-AI-slop, metric-based component architecture, hardware acceleration, agency-grade structural defaults. Skipping the skill = reverting to AI-default patterns.",
			SkillsByVibe: []SkillVibeMapping{
				{
					Vibe:        "Soft Structuralism",
					Skill:       "minimalist-ui",
					HowToInvoke: "Invoke via Claude Code Skill tool when the chosen vibe is Soft Structuralism (default for ~70% of sites). The skill teaches warm-monochrome editorial patterns that match atomicsite's render defaults.",
				},
				{
					Vibe:        "Editorial Luxury",
					Skill:       "high-end-visual-design",
					HowToInvoke: "Invoke when the vibe is Editorial Luxury OR when the brief mentions agency / portfolio / lifestyle. The skill teaches Awwwards-tier patterns: double-bezel cards, button-in-button CTAs, magnetic-feel hover (CSS-only adaptation).",
				},
				{
					Vibe:        "Ethereal Glass",
					Skill:       "taste-skill",
					HowToInvoke: "Invoke for Ethereal Glass + AI/ML products. The skill enforces anti-generic UI rules, layout variance dials, perpetual motion engine specs, the most aggressive anti-template playbook of the four.",
				},
				{
					Vibe:        "Neo-Brutalist Landing",
					Skill:       "taste-skill",
					HowToInvoke: "Invoke for Neo-Brutalist Landing. The skill's strong-stance + rule-bending guidance fits brutalist aesthetic better than the other three.",
				},
				{
					Vibe:        "Soft Claymorphism",
					Skill:       "minimalist-ui",
					HowToInvoke: "Invoke for Soft Claymorphism. Minimalist-ui's bento + flat-surface rules adapt cleanly to the rounded pastel palette; the agent reads the skill, then applies it with claymorphism-specific radii.",
				},
			},
			BeforeAuthoring: []string{
				"Read this playbook section + the AntiPatterns + the chosen vibe's row before writing any custom HTML.",
				"To extract design tokens from a reference URL (color/font/radii inventory), use the start_migration_crawl MCP tool, it bundles assets the agent can read. Atomicsite does not run computed-style extraction (chromedp not wired); manual extraction from the bundled HTML/CSS is the workflow today.",
				"For inspector-coherent custom blocks: keep hard-coded values inside the canonical token allowlist exported from builder/css.go (CanonicalRadiiRem, CanonicalShadowFormula, CanonicalBeziers). The designTokenCoherenceChecks() in critique/critique.go enforces this.",
				"Max ONE perpetual animation per page (see Motion.DontUse). Marquee + circuit + pulse + drift on the same page reads as AI-builder demo.",
			},
		},
	}
}

// defaultBlockSchemas tells the agent which JSON keys each block type
// recognises. Derived from the single source of truth in
// internal/blocks: the registry feeds both this agent surface and the
// editor's auto-rendered forms, so adding a new block_type only
// requires one Register() call. Returns the legacy BlockSchemaInfo
// shape so the agent contract stays stable.
func defaultBlockSchemas() []BlockSchemaInfo {
	all := blocks.All()
	out := make([]BlockSchemaInfo, 0, len(all))
	for _, s := range all {
		info := BlockSchemaInfo{
			BlockType: s.Type,
			Use:       s.Description,
		}
		for _, f := range s.Fields {
			label := f.Label
			if f.Help != "" {
				label = label + ", " + f.Help
			}
			field := BlockSchemaField{
				Key:       f.Key,
				Label:     label,
				Multiline: f.Kind == blocks.KindTextarea || f.Kind == blocks.KindRichtext,
			}
			switch f.Kind {
			case blocks.KindText, blocks.KindTextarea, blocks.KindRichtext:
				info.TextKeys = append(info.TextKeys, field)
			default:
				info.OtherKeys = append(info.OtherKeys, field)
			}
		}
		out = append(out, info)
	}
	return out
}


// defaultEditingModes documents the two authoring surfaces on the page
// editor. The agent uses these to phrase its responses ("toggle to Text
// mode" / "open the </> code view" / "click View source").
func defaultEditingModes() []EditingModeInfo {
	return []EditingModeInfo{
		{
			Name:        "Blocks",
			URL:         "/sites/{site_id}/pages/{page_id}",
			Description: "Canonical full editor. Drag, add, delete, expand each block to edit every field. Per-block </> toggle reveals the rendered Astro source for that block.",
		},
		{
			Name:        "Text mode",
			URL:         "/sites/{site_id}/pages/{page_id}",
			Description: "Tight inline-editable view of every text field across every visible block. Click any field, edit, blur to autosave. Use this surface when the user asks for copy or SEO edits.",
		},
		{
			Name:        "View source",
			URL:         "/sites/{site_id}/pages/{page_id}",
			Description: "Dialog showing the assembled .astro source for the whole page. Mirrors GET /api/sites/{id}/pages/{id}/preview.",
		},
		{
			Name:        "Global blocks",
			URL:         "/sites/{site_id}/global-blocks",
			Description: "Site-wide header and footer slots. Edit once, applies to every page (unless the page sets hide_global_blocks=true for landing-page suppression).",
		},
		{
			Name:        "Trusted external domains",
			URL:         "/sites/{site_id}/settings/allowed-scripts",
			Description: "CSP allowlist. Each row picks a kind: script (Stripe.js / GA), frame (cal.com / YouTube / Stripe Checkout iframes), image (Cloudinary CDN), media (Vimeo), connect (fetch-only API host), or all. Writes are admin-only to keep agents from silently widening the attack surface; the agent should point the user here when an embed is needed.",
		},
		{
			Name:        "Security headers",
			URL:         "/sites/{site_id}/settings/security",
			Description: "Edit HSTS, CSP enable + extra directives, frame-ancestors, X-Frame-Options, Referrer-Policy, Permissions-Policy, COOP/CORP/COEP. Admin-only (writes change attack surface).",
		},
	}
}

// computePersonalization builds the personalization summary surfaced to
// the agent. known_keys is the DISTINCT set of metadata keys the CRM has
// pushed for this site (read via a raw json_each query because sqlc cannot
// model SQLite's JSON1 virtual columns). Examples are static so the agent
// has shape recognition even on a fresh site.
func (b *ContextBuilder) computePersonalization(ctx context.Context, siteID string) PersonalizationInfo {
	out := PersonalizationInfo{
		IdentityMaxAgeDays: 30,
		KnownKeys:          []string{},
		ExampleConditions: []string{
			`lead_score >= 60`,
			`lifecycle == "SQL"`,
			`last_topic == "compliance"`,
			`name present`,
			`returning == "true"`,
			`last_topic in compliance,gdpr,nis2`,
		},
	}
	settings, _ := b.queries.ListSettingsBySite(ctx, siteID)
	for _, s := range settings {
		if s.Category != "analytics" {
			continue
		}
		switch s.Key {
		case "personalization_enabled":
			out.Enabled = boolFromSetting(s.Value, false)
		case "identity_max_age_days":
			if n, err := strconv.Atoi(strings.TrimSpace(s.Value)); err == nil && n > 0 {
				out.IdentityMaxAgeDays = n
			}
		}
	}
	out.KnownKeys = b.listVisitorMetadataKeys(ctx, siteID)
	return out
}

// listVisitorMetadataKeys runs a raw query that sqlc cannot model
// (SQLite's json_each returns key/value as virtual columns, which sqlc's
// static analyzer rejects). Best-effort: returns an empty list on error
// rather than aborting the whole context call.
func (b *ContextBuilder) listVisitorMetadataKeys(ctx context.Context, siteID string) []string {
	const q = `SELECT DISTINCT je.key
FROM visit_sessions vs, json_each(vs.metadata_json) je
WHERE vs.site_id = ? AND vs.metadata_json != '{}'
ORDER BY je.key`
	keys := []string{}
	rows, err := b.queries.Raw().QueryContext(ctx, q, siteID)
	if err != nil {
		return keys
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			continue
		}
		keys = append(keys, k)
	}
	return keys
}

// computePendingSetup inspects the site's profile, settings, and content to
// produce a list of configuration gaps the agent should resolve. Each task
// names a concrete endpoint so the agent does not have to guess.
//
// Failures here are non-fatal; we return whatever we managed to compute and
// fall back to an empty list if a query errors. Better to ship a partial
// pending list than to crash the entire context call.
func (b *ContextBuilder) computePendingSetup(ctx context.Context, siteID string, _ any, pages []PageInfo) []SetupTask {
	out := []SetupTask{}

	siteRow, err := b.queries.GetSiteByID(ctx, siteID)
	if err != nil {
		return out
	}

	profile, _ := b.queries.GetSiteProfile(ctx, siteID)

	settings, _ := b.queries.ListSettingsBySite(ctx, siteID)
	settingMap := map[string]string{}
	for _, s := range settings {
		settingMap[s.Category+"."+s.Key] = s.Value
	}

	push := func(t SetupTask) {
		out = append(out, t)
	}

	// --- Profile (drives Organization JSON-LD, security.txt, legal pages) ---
	if profile.BusinessName == "" {
		push(SetupTask{
			ID:       "profile.business_name",
			Category: "profile",
			Title:    "Add the business name",
			Why:      "Drives Organization JSON-LD schema, legal pages, and security.txt. Missing it costs 3 SEO/GEO eval checks.",
			Action:   "Ask the user for the legal business name, then PATCH it onto the profile.",
			Endpoint: "PATCH /api/agent/profile",
			Severity: "required",
		})
	}
	if profile.ContactEmail == "" {
		push(SetupTask{
			ID:       "profile.contact_email",
			Category: "profile",
			Title:    "Add a public contact email",
			Why:      "Required for the auto-generated imprint and security.txt. AI search engines look for it as a trust signal.",
			Action:   "Ask the user for a public-facing contact email and PATCH it onto the profile.",
			Endpoint: "PATCH /api/agent/profile",
			Severity: "required",
		})
	}
	if profile.AddressLine1 == "" || profile.City == "" {
		push(SetupTask{
			ID:       "profile.address",
			Category: "profile",
			Title:    "Add a postal address",
			Why:      "Drives Organization JSON-LD (address fields) and the auto-generated imprint page. Required for B2B, B2C-local, and ecommerce kits.",
			Action:   "Ask the user for street + city + postal code, then PATCH the profile.",
			Endpoint: "PATCH /api/agent/profile",
			Severity: "recommended",
		})
	}

	// --- SEO (drives Organization JSON-LD logo + sameAs) ---
	if siteRow.MetaTitle == "" {
		push(SetupTask{
			ID:       "seo.meta_title",
			Category: "seo",
			Title:    "Add a default meta title",
			Why:      "Falls through to every page that does not override it. Empty title fails the on-page SEO eval check.",
			Action:   "Compose a 30-60 char title for the site and PATCH it via /api/agent/branding (meta_title field).",
			Endpoint: "PATCH /api/agent/branding",
			Severity: "required",
		})
	}
	if siteRow.MetaDescription == "" {
		push(SetupTask{
			ID:       "seo.meta_description",
			Category: "seo",
			Title:    "Add a default meta description",
			Why:      "Same fall-through as meta_title. 120-160 chars. Drives SERP and AI-search snippets.",
			Action:   "Compose a 120-160 char description and PATCH it via /api/agent/branding (meta_description field).",
			Endpoint: "PATCH /api/agent/branding",
			Severity: "required",
		})
	}
	if settingMap["seo.logo_url"] == "" {
		push(SetupTask{
			ID:       "seo.logo_url",
			Category: "seo",
			Title:    "Add a logo URL",
			Why:      "Required by Organization JSON-LD (logo field) and the GEO 'Organization Schema Completeness' check.",
			Action:   "Upload or link a logo, then PATCH /api/agent/settings with category=seo, key=logo_url.",
			Endpoint: "PATCH /api/agent/settings",
			Severity: "recommended",
		})
	}
	if settingMap["seo.same_as"] == "" {
		push(SetupTask{
			ID:       "seo.same_as",
			Category: "seo",
			Title:    "Add social profile URLs",
			Why:      "Drives the Organization JSON-LD sameAs array. Strong AI-search trust signal (LinkedIn, GitHub, etc.).",
			Action:   "Ask the user for their social URLs (newline-separated) and PATCH /api/agent/settings (seo.same_as).",
			Endpoint: "PATCH /api/agent/settings",
			Severity: "recommended",
		})
	}

	// --- Branding ---
	if siteRow.PrimaryColor == "" || siteRow.PrimaryColor == "#D4AF37" {
		push(SetupTask{
			ID:       "branding.primary_color",
			Category: "branding",
			Title:    "Pick a brand primary colour",
			Why:      "The wizard default (#D4AF37) is still in place. Real brand colour is the single biggest visual change you can make.",
			Action:   "Ask the user for their brand primary, then PATCH /api/agent/branding (primary_color field).",
			Endpoint: "PATCH /api/agent/branding",
			Severity: "recommended",
		})
	}
	if siteRow.OgImageID == "" {
		push(SetupTask{
			ID:       "seo.og_image",
			Category: "media",
			Title:    "Set an Open Graph image",
			Why:      "Drives social previews and is referenced as a Schema.org logo fallback. Failing this loses 1 SEO eval check.",
			Action:   "Generate or upload a 1200x630 image via /api/agent/media (set folder=brand), then PATCH /api/agent/branding (og_image_id).",
			Endpoint: "PATCH /api/agent/branding",
			Severity: "recommended",
		})
	}
	if siteRow.FaviconID == "" {
		push(SetupTask{
			ID:       "seo.favicon",
			Category: "media",
			Title:    "Set a favicon",
			Why:      "Browsers default to a placeholder; AI agents and crawlers pick up the favicon as a brand signal.",
			Action:   "Generate or upload a square favicon via /api/agent/media (folder=brand), then PATCH /api/agent/branding (favicon_id).",
			Endpoint: "PATCH /api/agent/branding",
			Severity: "recommended",
		})
	}

	// --- Analytics + consent ---
	atomicsiteTracking := boolFromSetting(settingMap["analytics.atomicsite_tracking_enabled"], true)
	// cookieproof_enabled defaults to true (auto-A on Privacy + GDPR by default
	// for every fresh tenant, 2026-05-01). The setup task only fires when the
	// row is explicitly 0 AND tracking is on AND no custom banner is present.
	cookieproofOn := boolFromSetting(settingMap["analytics.cookieproof_enabled"], true)
	customBanner := strings.TrimSpace(settingMap["analytics.cookie_banner_snippet"]) != ""
	if atomicsiteTracking && !cookieproofOn && !customBanner {
		push(SetupTask{
			ID:       "analytics.consent",
			Category: "analytics",
			Title:    "Add a consent banner",
			Why:      "Tracking is on but no banner is in place. EU GDPR requires consent before non-essential tracking; failing this breaks the privacy eval and risks fines.",
			Action:   "Either flip on the bundled cookie banner (PATCH /api/agent/settings analytics.cookieproof_enabled=1; branding flows in automatically, copy/categories live in cookie_banner_* and cookie_cat_*) or paste a third-party snippet (analytics.cookie_banner_snippet) for Cookiebot, OneTrust, Termly, etc.",
			Endpoint: "PATCH /api/agent/settings",
			Severity: "required",
		})
	}
	if settingMap["analytics.crm_webhook_url"] == "" {
		push(SetupTask{
			ID:       "analytics.crm_webhook",
			Category: "analytics",
			Title:    "Wire a CRM webhook (optional)",
			Why:      "Without a CRM webhook, identified visitor events (form submits, opt-ins) are recorded but not forwarded anywhere. Plug in any HTTPS endpoint; we sign payloads with HMAC-SHA256.",
			Action:   "Ask the user for their CRM webhook URL (HubSpot, Salesforce, BrightCRM, n8n) and a shared secret, then PATCH /api/agent/settings (analytics.crm_webhook_url, analytics.crm_webhook_secret).",
			Endpoint: "PATCH /api/agent/settings",
			Severity: "recommended",
		})
	}

	// --- Content ---
	publishedPages := 0
	for _, p := range pages {
		if p.Status == "published" {
			publishedPages++
		}
	}
	if publishedPages == 0 {
		push(SetupTask{
			ID:       "content.publish_home",
			Category: "content",
			Title:    "Publish at least one page",
			Why:      "Every page is still draft. The site has nothing to deploy.",
			Action:   "PATCH /api/agent/pages/{slug} with status='published' once the home page is ready.",
			Endpoint: "PATCH /api/agent/pages/{slug}",
			Severity: "required",
		})
	}
	if len(pages) <= 1 {
		push(SetupTask{
			ID:       "content.add_pages",
			Category: "content",
			Title:    "Add more pages",
			Why:      "A one-page site limits SEO and conversion. Add at least Pricing, About, and Contact unless the user has explicitly chosen a one-pager.",
			Action:   "Ask the user what additional pages they want, then POST /api/agent/pages.",
			Endpoint: "POST /api/agent/pages",
			Severity: "recommended",
		})
	}

	return out
}

// boolFromSetting parses the limited bool encoding the settings table uses
// ("1" / "true" / "yes" / "on" -> true; everything else -> the default).
func boolFromSetting(v string, def bool) bool {
	if v == "" {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func (b *ContextBuilder) buildConstraints(ctx context.Context, siteID string) Constraints {
	rules, _ := b.queries.ListActiveGuardrailsBySite(ctx, siteID)

	c := Constraints{
		MaxBlocksPerPage: 50,
		MaxURLDepth:      3,
		RequiredBlocks:   make(map[string][]string),
	}

	for _, r := range rules {
		switch r.RuleType {
		case "allow_block_type":
			c.AllowedBlockTypes = append(c.AllowedBlockTypes, r.Value)
		case "forbid_pattern":
			c.ForbiddenPatterns = append(c.ForbiddenPatterns, r.Value)
		case "allowed_host":
			c.AllowedHosts = append(c.AllowedHosts, r.Value)
		case "max_blocks":
			var n int
			if err := json.Unmarshal([]byte(r.Value), &n); err == nil {
				c.MaxBlocksPerPage = n
			}
		case "require_block":
			var blocks []string
			if err := json.Unmarshal([]byte(r.Value), &blocks); err == nil {
				c.RequiredBlocks[r.Target] = blocks
			}
		}
	}

	// Add allowed scripts as allowed hosts
	scripts, _ := b.queries.ListAllowedScriptsBySite(ctx, siteID)
	for _, s := range scripts {
		if s.IsActive == 1 {
			c.AllowedHosts = append(c.AllowedHosts, s.Domain)
		}
	}

	return c
}

func parseJSONField(s string) any {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	return v
}
