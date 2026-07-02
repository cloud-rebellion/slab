package mcp

import (
	"context"

	authmw "github.com/bright-interaction/slab/internal/middleware"
)

// registerPrompts wires reusable prompt templates the user picks from a
// dropdown in the host UI. Each prompt is a starting frame the model
// rides into the conversation; users still drive the actual work.
func (s *Server) registerPrompts() {
	register := func(p Prompt) { s.prompts[p.Name] = p }

	register(Prompt{
		Name:        "walk_through_pending_setup",
		Description: "Walk through every pending_setup item with the user, one at a time, and resolve each. Use this on first session to bring the site from wizard-defaults to launch-ready.",
		Render: func(ctx context.Context, agent *authmw.AgentIdentity, _ map[string]string) ([]PromptMessage, error) {
			return []PromptMessage{
				{Role: "user", Content: Content{Type: "text", Text: "Read atomicsite://site/pending_setup. For each item in the list, ask me what value to use, then call bulk_upsert_settings (or update_profile / update_branding) to apply it. After each one, recompute the list and continue with the next remaining item. Stop when pending_setup is empty."}},
			}, nil
		},
	})

	register(Prompt{
		Name:        "audit_seo",
		Description: "Run an SEO audit using the latest evaluation results + settings_catalog. Surfaces failing checks and suggests fixes the agent can apply directly.",
		Render: func(ctx context.Context, agent *authmw.AgentIdentity, _ map[string]string) ([]PromptMessage, error) {
			return []PromptMessage{
				{Role: "user", Content: Content{Type: "text", Text: "Trigger a build, wait for it to complete, then read the evaluation results. For every failing SEO check, propose a concrete fix (settings change, page metadata edit, block edit) and ask me to confirm before applying. Read atomicsite://site/settings_catalog when you need to know which settings keys to write."}},
			}, nil
		},
	})

	register(Prompt{
		Name:        "connect_analytics",
		Description: "Walk through analytics setup: CookieProof / GA4 / Umami / personalization. The agent configures the toggles but does not touch any visitor data.",
		Render: func(ctx context.Context, agent *authmw.AgentIdentity, _ map[string]string) ([]PromptMessage, error) {
			return []PromptMessage{
				{Role: "user", Content: Content{Type: "text", Text: "Help me wire analytics. Read atomicsite://site/settings_catalog for the analytics category. Ask which providers I want (CookieProof / GA4 / Umami / personalization), collect the IDs, and call bulk_upsert_settings to flip the toggles. Important: you have NO access to visitor data or PII; you can only configure the providers."}},
			}, nil
		},
	})

	register(Prompt{
		Name:        "add_iframe_integration",
		Description: "Playbook for embedding a third-party iframe (cal.com, YouTube, Stripe Checkout). Reads trusted_domains to detect whether the host is already whitelisted; if not, asks the human to add it via the admin URL.",
		Render: func(ctx context.Context, agent *authmw.AgentIdentity, _ map[string]string) ([]PromptMessage, error) {
			return []PromptMessage{
				{Role: "user", Content: Content{Type: "text", Text: "I want to embed an iframe. Read atomicsite://site/security_posture. Ask which host I want to embed. Check trusted_domains.frame for that host. If present, insert the iframe block. If absent, tell me the exact admin URL to visit and the kind to pick (frame), then wait for me to confirm I've added it before inserting the block."}},
			}, nil
		},
	})

	register(Prompt{
		Name:        "build_for_perfect_eval",
		Description: "Read the site state + pending_setup + settings_catalog + block_schemas, then build out the site to maximize the eval score. Treats the eval engine's checklist as the spec; iterates until the categories the agent can write hit at least 95%. Use this when the site is empty or near-empty and you want a full set-up pass in one shot. Grade targets follow the site's design.fidelity dial; under showcase the performance target is C+ against showcase budgets, not A+.",
		Render: func(ctx context.Context, agent *authmw.AgentIdentity, _ map[string]string) ([]PromptMessage, error) {
			return []PromptMessage{
				{Role: "user", Content: Content{Type: "text", Text: `Goal: get this site to its best possible eval grades without me hand-holding each step. Targets follow the design.fidelity dial: performance fidelity = A+ everywhere; balanced (default) = A+ where agent-writable; showcase = security/a11y/privacy >= A-, seo >= B, performance >= C+ (the operator traded speed for design; spend that budget on craft, read design_playbook.fidelity first).

Read atomicsite://site/context first. The payload carries everything you need to plan: pending_setup (gaps), block_schemas (what each block_type expects), settings_catalog (every settable key + writable_by_agent flag, including design.fidelity), security_posture (admin-only items you can't touch), i18n (locale + hreflang strategy), endpoints (REST mirror of MCP).

Then converge on the perfect-score checklist below. Treat each item as a hard requirement before claiming done.

== Foundation ==
1. Profile completeness via update_profile: business_name, registration_nr, country, contact_email, privacy_email, security_email, address_line1, city, postal_code. Drives Organization JSON-LD + security.txt.
2. Branding via PATCH /api/agent/branding: meta_title (30-60 chars, fallback for any page without override), meta_description (120-160 chars with action verbs like "learn / try / get / book / läs / prova"), og_image_id (1200x630 from media library, brand folder), favicon_id (square PNG/JPEG/GIF/WebP; SVG is rejected, use PNG), lang.
3. Settings via bulk_upsert_settings:
   - seo.canonical_base = absolute https URL of the production domain
   - seo.meta_title_template = "{page_title} {separator} {site_name}"
   - seo.meta_description_template = "{page_description}"
   - seo.same_as = newline-separated social URLs (LinkedIn, GitHub, Instagram, etc.; drives JSON-LD sameAs)
   - seo.sitemap_enabled = "1"
   - general.hreflang_strategy = "path" (default)
   - general.additional_langs = "en,sv,..." for every locale you'll publish
   - analytics.atomicsite_tracking_enabled = "1"
   - analytics.cookieproof_enabled = "1" to ship the bundled, same-origin cookie banner; branding colors flow in automatically. Banner copy + categories live in cookie_banner_title/_description/_position, cookie_banner_accept/_reject/_customize, cookie_cat_analytics/_marketing/_preferences. Per-cookie metadata (Cookie/Provider/Purpose/Expiry table in the extended modal) lives in cookie_declarations as a JSON array; presets for enabled trackers (GA4, language) auto-populate, user entries override on (category, name) collision.
   - Compliance + UX knobs (added 2026-05-01): cookie_expiry_days (0..365, default 365 — IMY caps at 12 months), cookie_revision (int — bump to invalidate prior consent on policy change), cookie_theme (light/dark/auto), cookie_floating_trigger (left/right/off), cookie_language_selector (0/1), ccpa_enabled (0/1) + ccpa_url (CCPA Do-Not-Sell link, required when ccpa_enabled=1).
   - Reject button always shares Accept's color (IMY 2026 equal-prominence guardrail). The consent cookie name is fixed to "as_consent" — not configurable per site.

== Structure ==
4. URL convention: pick one. Either root = default lang (e.g. / + /sv/, the typical Swedish-marketing pattern) OR symmetric (/en/ + /sv/ + a noindex splash at /). Don't mix. Hreflang emission is path-based on actually-published page slugs.
5. For every page you publish, set BOTH meta_title and meta_description on the page row via update_page (don't rely on branding fallback alone — eval flags pages with empty per-page meta on title-length / description-length checks).
6. Set no_index=0 on real content pages. The splash page should be no_index=1 + hide_global_blocks=1.
7. Each locale needs a counterpart for hreflang to emit cleanly: /en/about ↔ /sv/about, /en/privacy ↔ /sv/privacy, /en/404 ↔ /sv/404. If a counterpart is missing, eval flags the multi-language pages.

== Pages ==
8. Every content page (not splash) needs at minimum:
   - 1 hero block at sort_order=0 with: eyebrow (optional), headline (becomes h1, REQUIRED for eval Has H1), subheading, cta_text + cta_url, optional secondary_label + secondary_url, optional image_id from media library
   - 1+ feature_grid OR text block carrying real content (300+ words across the page total — eval flags pages under 300 words)
   - 1 cta block at the end with heading, text, cta_text, cta_url
9. Use feature_grid for cards (each item: title, body, icon Lucide name like Mail/Server/Lock/Workflow). 3-6 items renders cleanly.
10. Use text blocks for long-form prose. Multi-paragraph by separating with blank lines (\n\n); single \n inside a paragraph becomes <br>.
11. Don't use raw HTML blocks unless absolutely necessary; the forbidden_patterns guardrail rejects <iframe> and inline scripts.
12. No plaintext emails in block text — eval flags them as info. Use mailto: via raw HTML carefully OR refer to a contact page that uses an obfuscated address pattern.

== Global blocks ==
13. PUT /api/agent/global/header with name, block_type=header, data.links = primary nav + lang switcher + booking CTA (last link auto-styled as a primary pill).
14. PUT /api/agent/global/footer with name, block_type=footer, data.copyright + data.links pointing at /<lang>/privacy, /<lang>/terms, /<lang>/cookies, plus social URLs.

== Build verb is publish verb ==
15. trigger_build = "build static Astro output + run eval engine + auto-deploy via the configured deploy target (Dockyard, rsync, local bind mount, Vercel, Fly, or any static host)". After it returns success, the live site reflects the new content.
16. After every build, get_evaluation(build_id) returns 5 categories: accessibility (33pt), performance (12pt), privacy (14pt), security (26pt), seo (64pt). Walk failing checks, fix, re-build. Iterate until the categories you can write are above 95%.

== Block-time guardrails ==
17. Generic alt text ("image", "photo") is rejected at create_block / update_block time.
18. Empty meta_description, missing CTA verbs, slug with underscores or trailing slash are all rejected at update_page time.
19. Personalization conditions (data.condition) are validated against a small DSL; complex booleans rejected.

== Admin-only items you can't fix ==
The eval will flag these but you can't write them:
- security category settings (HSTS preload, CSP extra directives, COOP/CORP/COEP, X-Frame-Options): admin only at human_admin_url shown in settings_catalog.
- allowed-scripts (CSP allowlist for cal.com, YouTube, etc.): admin only.
- Subresource Integrity: requires layout-level SHA-384 fetch.
- Focus Indicators: requires layout-level :focus-visible CSS.
- Resource Hints: requires layout-level <link rel=preconnect> for fonts.
- Modern Image Formats: hero/image renderer doesn't yet pick WebP variants from media.variants_json.
- Image dimensions on hero: renderer doesn't emit width/height (yet).

For these, surface them to the human at the end with the exact admin URL each one points at. Don't loop trying to write them — bulk_upsert_settings rejects them with rejected_admin_only.

== Verification gate ==
Before claiming the site is "done":
- list_pages returns every page you intended.
- list_blocks for the home page returns 5+ blocks in order (sort_order increasing).
- get_site_context's pending_setup list is empty (or only admin-only items remain).
- get_evaluation shows 95%+ on agent-writable categories.
- The live URL serves correctly: curl -I returns 200, the HTML contains <h1> from the hero block, OG meta tags reference /media/<og_image_id>, headers carry HSTS + CSP.

Drive end-to-end now. Ask me only when you genuinely need a value (org-nr, social URL, copy direction); otherwise pick sensible defaults from the brand voice and keep moving.`}},
			}, nil
		},
	})

	register(Prompt{
		Name:        "create_landing_page",
		Description: "Create a high-conversion landing page that suppresses the global nav + footer (hide_global_blocks=true). Walks through the standard sections (hero, problem, solution, social proof, CTA).",
		Render: func(ctx context.Context, agent *authmw.AgentIdentity, _ map[string]string) ([]PromptMessage, error) {
			return []PromptMessage{
				{Role: "user", Content: Content{Type: "text", Text: "Create a landing page. Ask for the slug + headline + the offer. Create the page with hide_global_blocks=1. Then create blocks in order: hero, value-prop, social-proof, cta. Read atomicsite://site/structure first to see what blocks already exist on similar pages so the new page matches the site's voice."}},
			}, nil
		},
	})

	register(Prompt{
		Name:        "master_the_stack",
		Description: "Walk the curriculum that turns you into an Atomic Site stack expert: Astro + TypeScript + the CSS-variable system + the 19 block types + i18n + security + personalization + cookieproof, plus the UX/UI discipline (typography, color, spacing, motion, accessibility, performance, forms, nav, dark mode, premium-design principles). Read each doc in atomicsite://knowledge/* in order, then return a confidence summary.",
		Render: func(ctx context.Context, agent *authmw.AgentIdentity, _ map[string]string) ([]PromptMessage, error) {
			return []PromptMessage{
				{Role: "user", Content: Content{Type: "text", Text: `Become an expert on the Atomic Site stack and design discipline.

Step 1. Read atomicsite://knowledge/index. It returns a catalog of every curriculum doc in two halves: stack (how the builder emits Astro + TypeScript + custom-CSS sites) and ux (the discipline that makes output feel premium).

Step 2. Walk the stack docs in order: astro-conventions, typescript-strict, css-variable-system, block-renderer-patterns, i18n-authoring, security-authoring, personalization, cookieproof-integration. Read each via atomicsite://knowledge/<slug>. Take notes on what each teaches; do not just skim.

Step 3. Walk the ux docs in order: typography-scale, color-system, spacing-rhythm, motion-curves, accessibility-patterns, performance-budgets, forms-ux, nav-ux, dark-mode, premium-design-principles. Same depth.

Step 4. Read atomicsite://site/context to see the actual site you will be working on, and atomicsite://meta/capabilities to see exactly what tools and resources you have access to.

Step 5. Summarise back to me in 8 to 12 bullet points: what the builder writes vs what you write, which CSS variables drive the visual system, what the privacy boundaries are, which tools you can call, and which UX disciplines you are now ready to apply. End with one specific next move you would propose for this site.`}},
			}, nil
		},
	})

	register(Prompt{
		Name:        "audit_for_premium_feel",
		Description: "Run a build, read the eval scores, then layer UX/UI heuristics on top: typography, color calibration, motion, dark-mode parity, premium-design principles. Propose specific block edits to lift the site from passable to premium.",
		Render: func(ctx context.Context, agent *authmw.AgentIdentity, _ map[string]string) ([]PromptMessage, error) {
			return []PromptMessage{
				{Role: "user", Content: Content{Type: "text", Text: `Audit this site for premium feel and propose specific edits.

Step 1. Trigger a build via the trigger_build tool. Wait for it to complete via get_build_status.

Step 2. Read atomicsite://eval/latest to see the per-category scores. Note any check failing under the 90% threshold.

Step 3. Read these UX curriculum docs to layer heuristics on top of the eval rubric: atomicsite://knowledge/typography-scale, color-system, spacing-rhythm, motion-curves, premium-design-principles.

Step 4. Walk every page (list_pages then list_blocks per page). For each block, evaluate against the heuristics: is the type-scale contrast strong enough? Is whitespace generous? Are colors calibrated? Is motion present and disciplined? Is the asymmetry intentional?

Step 5. Produce a punch list: per-block before/after. Cite the specific knowledge doc that motivates each change. Do not auto-apply; show me the list and wait for sign-off.`}},
			}, nil
		},
	})

	register(Prompt{
		Name:        "build_from_figma",
		Description: "Use a Figma file as the design reference for the site: import tokens (colors, typography), seed CSS classes from the file, then build the first page using the imported palette and fonts. The agent never sees the access token; the user pastes it directly to the admin Figma import flow.",
		Render: func(ctx context.Context, agent *authmw.AgentIdentity, _ map[string]string) ([]PromptMessage, error) {
			return []PromptMessage{
				{Role: "user", Content: Content{Type: "text", Text: `Build this site using a Figma file as the design reference.

Step 1. Ask me for the Figma file URL.

Step 2. Direct me to the admin Figma import page (Settings -> Design -> Figma import). I paste the URL plus a personal access token there; the import handler fetches the file once, seeds CSS classes for every published color and text style, and updates branding's primary color and fonts. The token is never persisted and never flows through MCP.

Step 3. Once I confirm the import completed, read atomicsite://site/context to pick up the new branding tokens, list_css_classes to see the seeded classes, and list_fonts to see the new font families.

Step 4. Walk the UX curriculum docs that govern composition: atomicsite://knowledge/typography-scale, color-system, premium-design-principles.

Step 5. Propose a homepage composition using the imported tokens: hero with the brand primary, typography scaled correctly for the new font family, calibrated whitespace. Show me the block list before creating any blocks.`}},
			}, nil
		},
	})

	register(Prompt{
		Name:        "learn_from_reference",
		Description: "Register a public GitHub repo as a design reference (taste-skill, high-end-visual-design, custom), read its README and tailwind config, extract patterns, then apply them to the site without copying code.",
		Render: func(ctx context.Context, agent *authmw.AgentIdentity, _ map[string]string) ([]PromptMessage, error) {
			return []PromptMessage{
				{Role: "user", Content: Content{Type: "text", Text: `Learn from a public GitHub design reference and apply its principles to this site.

Step 1. Ask me for the GitHub URL of the reference repo (e.g. taste-skill, a component library, an agency portfolio I admire).

Step 2. Call add_design_reference with the URL. Then call refresh_design_reference with the returned id so the handler fetches a representative bundle: README, tailwind config, sample components.

Step 3. Read atomicsite://design-references and find the entry. The fetched_json field carries the bundle. Read each file: what colors does the reference use? What type-scale? What spacing? What component patterns?

Step 4. Walk these UX docs to filter what to copy vs what to leave: atomicsite://knowledge/premium-design-principles, color-system, typography-scale. Reference taste, do not imitate the surface.

Step 5. Propose a list of patterns to apply: which CSS classes to add (upsert_css_class), which knowledgebase entries to write (create_knowledgebase_entry to capture brand voice notes), which blocks to refactor. Show me the list and the citation for each ("from <reference repo>: <pattern>") before applying.`}},
			}, nil
		},
	})
}
