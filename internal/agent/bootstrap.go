package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/bright-interaction/slab/internal/store"
)

// RenderCLAUDEMD generates a personalised CLAUDE.md for an agent session.
// It is the same content the docs page shows as a static template, but with
// the site name + base URL + (optionally) the raw key substituted in, plus a
// snapshot of the current pending_setup so the agent has a starting checklist
// even before the first /api/agent/context call.
//
// rawKey may be empty: the admin bootstrap endpoint passes the just-issued
// key (visible once); the agent bootstrap endpoint omits it because the
// caller already has it.
func RenderCLAUDEMD(ctx context.Context, q *store.Queries, siteID, baseURL, rawKey string) string {
	site, err := q.GetSiteByID(ctx, siteID)
	if err != nil {
		return ""
	}
	siteName := site.Name
	if siteName == "" {
		siteName = "this Atomic Site"
	}
	siteDomain := site.Domain
	if siteDomain == "" {
		siteDomain = "(domain not set)"
	}

	// Snapshot pending_setup so the agent reads a real checklist on first
	// open even if it has not called /api/agent/context yet.
	pending := []SetupTask{}
	if cb := NewContextBuilder(q); cb != nil {
		// Reuse the same logic the agent context endpoint exposes; pages
		// param is best-effort (empty list is fine for the snapshot).
		pending = cb.computePendingSetup(ctx, siteID, nil, nil)
	}

	keyLine := "$ATOMICSITE_KEY"
	if rawKey != "" {
		keyLine = rawKey
	}
	urlLine := "$ATOMICSITE_API"
	if baseURL != "" {
		urlLine = baseURL
	}

	var b strings.Builder
	b.WriteString("# Working on ")
	b.WriteString(siteName)
	b.WriteString("\n\n")
	b.WriteString("You are an AI agent connected to an Atomic Site instance. Your job is to build\nand edit this website by calling the agent API.\n\n")

	b.WriteString("## Site\n")
	b.WriteString("- Name: ")
	b.WriteString(siteName)
	b.WriteString("\n- Domain: ")
	b.WriteString(siteDomain)
	b.WriteString("\n- Site ID: ")
	b.WriteString(siteID)
	b.WriteString("\n\n")

	b.WriteString("## Setup\n")
	b.WriteString("- Base URL: ")
	b.WriteString(urlLine)
	b.WriteString("\n- Auth: X-Agent-Key: ")
	b.WriteString(keyLine)
	b.WriteString("\n\n")

	b.WriteString("## First-call workflow (do this BEFORE anything else)\n")
	b.WriteString("1. Call `GET /api/agent/context`.\n")
	b.WriteString("2. Inspect the `pending_setup` array in the response.\n")
	b.WriteString("3. For every item in pending_setup, walk the user through it before\n   touching content:\n   - Read the `why` to the user; ask only what you need.\n   - Call the listed `endpoint` with their answer.\n   - Re-fetch /api/agent/context and verify the item is gone.\n4. Only after pending_setup is empty (or the user explicitly defers an\n   item) move on to content work.\n\n")

	if len(pending) > 0 {
		b.WriteString("## Pending setup (snapshot at the time this file was generated)\n\n")
		b.WriteString("This list will be re-computed on every /api/agent/context call. Treat\nthis section as a starting checklist, not a contract.\n\n")
		for _, t := range pending {
			b.WriteString("- **[")
			b.WriteString(t.Severity)
			b.WriteString("] ")
			b.WriteString(t.Title)
			b.WriteString("** (")
			b.WriteString(t.Category)
			b.WriteString(")\n")
			b.WriteString("  - Why: ")
			b.WriteString(t.Why)
			b.WriteString("\n")
			b.WriteString("  - Action: ")
			b.WriteString(t.Action)
			b.WriteString("\n")
			b.WriteString("  - Endpoint: `")
			b.WriteString(t.Endpoint)
			b.WriteString("`\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("## Editing workflow\n")
	b.WriteString("1. Make edits via the relevant CRUD endpoints. Every write goes through the\n   guardrails engine. Read the validation errors and fix them; do not work\n   around them.\n")
	b.WriteString("2. After a meaningful set of edits, trigger a build:\n   `POST /api/agent/build`\n")
	b.WriteString("3. Poll status: `GET /api/agent/build/{buildID}/status` until done.\n")
	b.WriteString("4. Fetch the evaluation: `GET /api/agent/evaluation/{buildID}`. The evals\n   are the source of truth for quality. Fix every failing check before\n   declaring the task done.\n\n")

	b.WriteString("## Endpoints you will use most\n")
	b.WriteString("- `GET /api/agent/context` -> full site state + pending_setup\n")
	b.WriteString("- `GET /api/agent/bootstrap` -> re-fetch this CLAUDE.md any time\n")
	b.WriteString("- `PATCH /api/agent/profile` -> business name, address, contact emails\n")
	b.WriteString("- `PATCH /api/agent/branding` -> colours, fonts, meta_title, meta_description, og_image_id, favicon_id\n")
	b.WriteString("- `PATCH /api/agent/settings` -> analytics + seo + general categories\n")
	b.WriteString("- `POST /api/agent/pages` and `PATCH /api/agent/pages/{slug}`\n")
	b.WriteString("- `POST /api/agent/pages/{slug}/blocks`\n")
	b.WriteString("- `POST /api/agent/build` and `GET /api/agent/evaluation/{buildID}`\n\n")

	fidelity := FidelityForSite(ctx, q, siteID)
	b.WriteString("## Design fidelity\n")
	b.WriteString(fmt.Sprintf("- Active dial: **%s** (settings category=design key=fidelity; agent-writable).\n", string(fidelity)))
	switch fidelity {
	case FidelityPerformance:
		b.WriteString("- Contract: perfect scores. Static hero graphics, zero perpetual motion,\n  every category at A+. See design_playbook.fidelity for the full contract.\n\n")
	case FidelityShowcase:
		b.WriteString("- Contract: jaw-dropping design, measured speed trade. fx-* utilities,\n  scroll-driven animation, view transitions, bespoke tokens, 4-signal motion\n  budget. Targets: security/a11y/privacy >= A-, seo >= B, perf >= C+ against\n  showcase budgets. Read design_playbook.fidelity FIRST, then invoke the\n  stitch-design skill to generate the site's DESIGN.md before authoring.\n\n")
	default:
		b.WriteString("- Contract: the standard taste rulebook and budgets. Flip to `performance`\n  for perfect scores or `showcase` for expressive freedom (the playbook and\n  grading rubric adapt automatically; re-read them after changing it).\n\n")
	}

	b.WriteString("## Guardrails (these will reject your writes if you violate them)\n")
	b.WriteString("- Pages: title 30-60 chars, description 120-160 chars\n")
	b.WriteString("- Blocks: image blocks need alt + dimensions; CTAs need a label;\n  no generic anchor text (\"click here\", \"read more\"); no mixed-content\n  http:// URLs in https sites\n")
	b.WriteString("- URL depth: max 3 levels\n")
	b.WriteString("- Media: alt text required; SVG rejected for safety; SSRF-guarded\n  from-url ingestion\n\n")

	b.WriteString("## Bring-your-own integrations\n")
	b.WriteString("- CRM: any HTTPS webhook URL + shared secret. Set via\n  `PATCH /api/agent/settings` with category=analytics, keys\n  crm_webhook_url and crm_webhook_secret. Payloads HMAC-signed\n  (X-Atomicsite-Signature, SHA-256 hex).\n")
	b.WriteString("- Cookie banner: paste any HTML/JS into analytics.cookie_banner_snippet\n  (Cookiebot, OneTrust, Termly, Iubenda, your own). Or flip\n  analytics.cookieproof_enabled=1 for the bundled CookieProof.\n")

	return b.String()
}

// RenderEnvFile returns a shell-ready .env content with the base URL and key
// pre-filled. Intended as a download companion to the CLAUDE.md.
func RenderEnvFile(baseURL, rawKey string) string {
	return fmt.Sprintf(`# Atomic Site agent credentials.
# Source this file (or copy into your shell profile) before invoking your agent.

export ATOMICSITE_API=%q
export ATOMICSITE_KEY=%q
`, baseURL, rawKey)
}

// RenderSmokeTest returns a one-liner curl command the user can paste to
// verify the key works. Wraps the URL in single quotes so shells with $ in
// the key are safe.
func RenderSmokeTest(baseURL string) string {
	return fmt.Sprintf("curl -sH \"X-Agent-Key: $ATOMICSITE_KEY\" '%s/api/agent/context' | jq .site\n", baseURL)
}
