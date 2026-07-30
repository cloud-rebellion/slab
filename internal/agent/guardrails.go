package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/bright-interaction/slab/internal/personalization"
	"github.com/bright-interaction/slab/internal/store"
)

// GuardrailEngine validates agent operations against site rules.
type GuardrailEngine struct {
	queries *store.Queries
}

func NewGuardrailEngine(queries *store.Queries) *GuardrailEngine {
	return &GuardrailEngine{queries: queries}
}

// Violation represents a guardrail rule that was violated.
type Violation struct {
	Rule     string `json:"rule"`
	Message  string `json:"message"`
	Severity string `json:"severity"`
}

// guardrailLoadFailure is the fail-closed violation for a DB error while
// loading guardrail rules or page blocks: a tenant's own rules silently
// vanishing on a transient error would let exactly the writes they
// forbid through. The write is refused and the agent told to retry.
func guardrailLoadFailure(what string, err error) []Violation {
	return []Violation{{
		Rule:     "enforcement_unavailable",
		Message:  fmt.Sprintf("Could not load %s (%v); refusing the write rather than skipping enforcement. Retry shortly.", what, err),
		Severity: "error",
	}}
}

// ValidateBlock checks a block creation/update against guardrail rules.
func (g *GuardrailEngine) ValidateBlock(ctx context.Context, siteID string, blockType string, dataJSON string) []Violation {
	unescaped := unescapeJSON(dataJSON)
	rules, err := g.queries.ListActiveGuardrailsBySite(ctx, siteID)
	if err != nil {
		return guardrailLoadFailure("guardrail rules", err)
	}
	var violations []Violation

	allowedTypes := map[string]bool{}
	hasTypeRules := false

	for _, r := range rules {
		switch r.RuleType {
		case "allow_block_type":
			hasTypeRules = true
			allowedTypes[r.Value] = true

		case "forbid_pattern":
			if strings.Contains(unescaped, r.Value) || strings.Contains(dataJSON, r.Value) {
				violations = append(violations, Violation{
					Rule:     "forbid_pattern",
					Message:  fmt.Sprintf("Block data contains forbidden pattern: %q. Remove it and try again.", r.Value),
					Severity: r.Severity,
				})
			}
		}
	}

	if hasTypeRules && !allowedTypes[blockType] {
		violations = append(violations, Violation{
			Rule:     "allow_block_type",
			Message:  fmt.Sprintf("Block type %q is not allowed. Allowed types: %s", blockType, joinKeys(allowedTypes)),
			Severity: "error",
		})
	}

	// Built-in security checks (always active, regardless of rules).
	// Render-time sanitization (the builder's bluemonday pass) is the
	// authoritative defense; these input-boundary checks give the agent fast
	// feedback and reject the obvious vectors before a build. Match on a
	// lowercased haystack so mixed-case (<ScRiPt>, JavaScript:, onTOGGLE) and
	// slash-separated handlers (<svg/onload=>) can't slip past a naive
	// case-sensitive Contains.
	low := strings.ToLower(unescaped)
	// Scheme matching runs against a copy with ASCII whitespace and C0 controls
	// removed, because browsers strip tab, LF and CR from a URL BEFORE resolving
	// its scheme. Matching the raw string meant "java\tscript:alert(1)" passed
	// both this check and the renderer's escapeURL while executing fine in a
	// browser. Only the scheme scan uses this; the tag and handler checks below
	// still run on `low`, where the raw form is what matters.
	lowStripped := strings.ToLower(urlControlStripper.Replace(unescaped))
	if scriptTagRE.MatchString(low) {
		violations = append(violations, Violation{
			Rule:     "security",
			Message:  "Inline <script> tags are not allowed in block data. Use components or allowed external scripts.",
			Severity: "error",
		})
	}
	for _, scheme := range dangerousURLSchemes {
		if strings.Contains(lowStripped, scheme) {
			violations = append(violations, Violation{
				Rule:     "security",
				Message:  fmt.Sprintf("Dangerous URL scheme %q is not allowed. Use plain https:// links.", scheme),
				Severity: "error",
			})
			break
		}
	}
	if hasInsecureURL(unescaped) {
		violations = append(violations, Violation{
			Rule:     "security",
			Message:  "Mixed content: http:// URLs found. Use https:// for all external resources to avoid mixed-content blocks.",
			Severity: "error",
		})
	}
	if inlineEventHandlerRE.MatchString(low) {
		violations = append(violations, Violation{
			Rule:     "security",
			Message:  "Inline event handlers (on*= inside HTML tags) are not allowed. Use components with proper event handling.",
			Severity: "error",
		})
	}

	// Built-in eval-engine alignment checks (Site Inspector parity).
	violations = append(violations, validateBlockEvalChecks(blockType, dataJSON, unescaped)...)

	// Best practice warnings
	if strings.Contains(unescaped, "!important") {
		violations = append(violations, Violation{
			Rule:     "best_practice",
			Message:  "Avoid using !important in CSS. Use specific CSS classes instead.",
			Severity: "warning",
		})
	}
	if strings.Contains(unescaped, "style=") {
		violations = append(violations, Violation{
			Rule:     "best_practice",
			Message:  "Inline styles detected. Prefer using global CSS classes for consistency.",
			Severity: "warning",
		})
	}

	return violations
}

// ValidateBlockCount checks if adding a block would exceed the max_blocks limit.
func (g *GuardrailEngine) ValidateBlockCount(ctx context.Context, siteID string, pageID string) []Violation {
	rules, err := g.queries.ListActiveGuardrailsBySite(ctx, siteID)
	if err != nil {
		return guardrailLoadFailure("guardrail rules", err)
	}
	maxBlocks := 50 // default

	for _, r := range rules {
		if r.RuleType == "max_blocks" {
			var n int
			if err := json.Unmarshal([]byte(r.Value), &n); err == nil && n > 0 {
				maxBlocks = n
			}
		}
	}

	blocks, err := g.queries.ListBlocksByPage(ctx, pageID)
	if err != nil {
		return guardrailLoadFailure("page blocks", err)
	}
	if len(blocks) >= maxBlocks {
		return []Violation{{
			Rule:     "max_blocks",
			Message:  fmt.Sprintf("Page already has %d blocks (maximum %d). Remove blocks before adding more.", len(blocks), maxBlocks),
			Severity: "error",
		}}
	}

	return nil
}

// ValidateRequiredBlocks checks that required blocks are still present after a delete.
func (g *GuardrailEngine) ValidateRequiredBlocks(ctx context.Context, siteID string, pageSlug string, pageID string, deletingBlockID string) []Violation {
	rules, err := g.queries.ListActiveGuardrailsBySite(ctx, siteID)
	if err != nil {
		return guardrailLoadFailure("guardrail rules", err)
	}

	for _, r := range rules {
		if r.RuleType != "require_block" {
			continue
		}
		// Target is the page slug (or "*" for all pages)
		if r.Target != "*" && r.Target != pageSlug {
			continue
		}

		var requiredTypes []string
		if err := json.Unmarshal([]byte(r.Value), &requiredTypes); err != nil {
			continue
		}

		blocks, err := g.queries.ListBlocksByPage(ctx, pageID)
		if err != nil {
			return guardrailLoadFailure("page blocks", err)
		}
		remainingTypes := map[string]bool{}
		for _, b := range blocks {
			if b.ID != deletingBlockID {
				remainingTypes[b.BlockType] = true
			}
		}

		for _, rt := range requiredTypes {
			if !remainingTypes[rt] {
				return []Violation{{
					Rule:     "require_block",
					Message:  fmt.Sprintf("Page %q requires a %q block. Cannot delete the last one.", pageSlug, rt),
					Severity: "error",
				}}
			}
		}
	}

	return nil
}

// ValidatePageSlug checks a page slug against architecture constraints.
func (g *GuardrailEngine) ValidatePageSlug(ctx context.Context, siteID string, slug string) []Violation {
	var violations []Violation

	// Root slug is always valid
	if slug == "/" {
		return nil
	}

	arch, _ := g.queries.GetSiteArchitecture(ctx, siteID)
	maxDepth := int64(3)
	if arch.ID != "" {
		maxDepth = arch.MaxDepth
	}

	trimmed := strings.Trim(slug, "/")
	depth := strings.Count(trimmed, "/") + 1
	if int64(depth) > maxDepth {
		violations = append(violations, Violation{
			Rule:     "max_url_depth",
			Message:  fmt.Sprintf("URL depth %d exceeds maximum %d. Keep URLs shallow for crawl efficiency.", depth, maxDepth),
			Severity: "error",
		})
	}

	if slug != strings.ToLower(slug) {
		violations = append(violations, Violation{
			Rule:     "url_convention",
			Message:  "URLs must be lowercase.",
			Severity: "error",
		})
	}
	if strings.Contains(slug, "_") {
		violations = append(violations, Violation{
			Rule:     "url_convention",
			Message:  "Use hyphens (-) instead of underscores (_) in URLs.",
			Severity: "error",
		})
	}
	if strings.Contains(slug, " ") {
		violations = append(violations, Violation{
			Rule:     "url_convention",
			Message:  "URLs must not contain spaces.",
			Severity: "error",
		})
	}
	if len(slug) > 75 {
		violations = append(violations, Violation{
			Rule:     "url_convention",
			Message:  fmt.Sprintf("URL length %d exceeds recommended maximum of 75 characters.", len(slug)),
			Severity: "warning",
		})
	}
	// NOTE: no "must start with /" rule. Page slugs are stored WITHOUT a
	// leading slash (the human REST path stores "blog/post", and the builder's
	// slugToFilePath / pageDepth strings.Trim the slug, so that is the
	// canonical form). Requiring a leading slash here contradicted the
	// syntactic validatePageSlug (which rejects an empty leading segment) and
	// made MCP create_page impossible for any explicit slug. All the checks
	// above are leading-slash-agnostic (depth uses the trimmed form).

	return violations
}

// ShouldNoindex returns true if a page is an orphan (not in any silo) and should default to noindex.
func (g *GuardrailEngine) ShouldNoindex(ctx context.Context, siteID string, pageSlug string) bool {
	// Root page is never an orphan
	if pageSlug == "/" {
		return false
	}
	// Legal pages are never orphans
	legalPrefixes := []string{"/privacy", "/terms", "/cookie", "/imprint", "/legal"}
	for _, lp := range legalPrefixes {
		if strings.HasPrefix(pageSlug, lp) {
			return false
		}
	}

	silos, _ := g.queries.ListSilosBySite(ctx, siteID)
	if len(silos) == 0 {
		return false // No silos defined, all pages are valid
	}

	for _, s := range silos {
		if strings.HasPrefix(pageSlug, s.SlugPrefix) {
			return false // Page belongs to a silo
		}
	}

	// Top-level pages like /about, /contact are not orphans
	trimmed := strings.Trim(pageSlug, "/")
	if !strings.Contains(trimmed, "/") {
		return false
	}

	// Multi-level page not in any silo = orphan
	return true
}

// validateBlockEvalChecks enforces the input-time guardrails that mirror the
// post-build Site Inspector eval engine — alt text, image dimensions,
// descriptive anchor text, button/link labels, no empty headings. Refusing
// bad input here prevents grades from dropping after build.
func validateBlockEvalChecks(blockType, dataJSON, unescaped string) []Violation {
	var out []Violation

	// Personalization condition (Phase 18.4). Any block can carry an
	// optional `condition` string in data_json that gates its rendering on
	// per-visitor metadata. Reject anything that isn't a parseable
	// expression up front so the agent gets a fast feedback loop.
	{
		var d struct {
			Condition string `json:"condition"`
		}
		_ = json.Unmarshal([]byte(dataJSON), &d)
		if cond := strings.TrimSpace(d.Condition); cond != "" {
			if err := personalization.Validate(cond); err != nil {
				out = append(out, Violation{
					Rule:     "personalization_condition",
					Message:  fmt.Sprintf("condition: invalid expression: %s. Use 'key op value' (==, !=, >, >=, <, <=, in) or 'key present'.", err.Error()),
					Severity: "error",
				})
			}
		}
	}

	// Image-type blocks: require alt + dimensions.
	if blockType == "image" {
		var d struct {
			ImageID string `json:"image_id"`
			Alt     string `json:"alt"`
			Width   any    `json:"width"`
			Height  any    `json:"height"`
		}
		_ = json.Unmarshal([]byte(dataJSON), &d)
		if strings.TrimSpace(d.Alt) == "" {
			out = append(out, Violation{
				Rule:     "alt_text_required",
				Message:  "Image blocks must have descriptive alt text. Empty or missing alt fails accessibility + SEO checks.",
				Severity: "error",
			})
		} else if isLowQualityAlt(d.Alt) {
			out = append(out, Violation{
				Rule:     "descriptive_alt",
				Message:  fmt.Sprintf("Alt text %q is generic. Describe what's in the image (subject, action, context), not 'image' / 'photo' / 'picture'.", d.Alt),
				Severity: "error",
			})
		}
		if !hasNonZeroNumber(d.Width) || !hasNonZeroNumber(d.Height) {
			out = append(out, Violation{
				Rule:     "image_dimensions_required",
				Message:  "Image blocks must include width and height. Missing dimensions cause layout shift (CLS) and fail the perf check.",
				Severity: "error",
			})
		}
	}

	// CTA / button blocks: require non-generic label.
	if blockType == "cta" || blockType == "button" {
		var d struct {
			Label        string `json:"label"`
			PrimaryLabel string `json:"primary_label"`
			CtaLabel     string `json:"cta_label"`
		}
		_ = json.Unmarshal([]byte(dataJSON), &d)
		label := firstNonEmpty(d.Label, d.PrimaryLabel, d.CtaLabel)
		if strings.TrimSpace(label) == "" {
			out = append(out, Violation{
				Rule:     "button_label_required",
				Message:  "CTA / button blocks need a non-empty label. Empty buttons fail the a11y 'Buttons Have Labels' check.",
				Severity: "error",
			})
		} else if isLowQualityAnchor(label) {
			out = append(out, Violation{
				Rule:     "descriptive_anchor",
				Message:  fmt.Sprintf("Button label %q is generic. Describe the action (e.g. 'Book a discovery call', 'View pricing').", label),
				Severity: "warning",
			})
		}
	}

	// Hero / text blocks: forbid empty headings.
	switch blockType {
	case "hero", "text", "feature_grid":
		var d struct {
			Headline   string `json:"headline"`
			Heading    string `json:"heading"`
			Title      string `json:"title"`
			Subheading string `json:"subheading"`
		}
		_ = json.Unmarshal([]byte(dataJSON), &d)
		// If the block clearly carries a heading slot but it's whitespace-only, fail.
		if hasEmptyHeading(blockType, dataJSON, d.Headline, d.Heading, d.Title) {
			out = append(out, Violation{
				Rule:     "empty_heading",
				Message:  "Heading is whitespace-only. Either provide non-empty heading text or omit the heading field entirely.",
				Severity: "error",
			})
		}
	}

	// Generic anchor-text quality check on any block carrying links.
	for _, generic := range genericAnchors {
		// Match common JSON shapes: "label":"click here", "text":"read more"
		needle := `:"` + generic + `"`
		if strings.Contains(strings.ToLower(unescaped), needle) {
			out = append(out, Violation{
				Rule:     "descriptive_anchor",
				Message:  fmt.Sprintf("Generic anchor text %q found. Replace with descriptive text that explains the destination.", generic),
				Severity: "warning",
			})
			break
		}
	}

	// Plaintext-email rejection. Mirrors the eval-engine "No Plaintext Emails"
	// check at write time: emails that aren't wrapped in mailto: leak to
	// scrapers + bots. The page handler decides whether agent copy contains
	// an email and asks the operator to use a mailto: link or a contact form.
	if leaks := emailLeakRE.FindAllString(unescaped, -1); len(leaks) > 0 {
		// Filter emails already inside a mailto: href.
		var bare []string
		for _, e := range leaks {
			if !strings.Contains(strings.ToLower(unescaped), "mailto:"+strings.ToLower(e)) {
				bare = append(bare, e)
			}
		}
		if len(bare) > 0 {
			out = append(out, Violation{
				Rule:     "plaintext_email",
				Message:  fmt.Sprintf("Plaintext email(s) detected: %s. Wrap in <a href=\"mailto:...\"> or use a contact form. Plaintext emails get scraped.", strings.Join(bare, ", ")),
				Severity: "warning",
			})
		}
	}

	return out
}

// PageMetaInput is the subset of page fields the input-time meta guardrails
// validate. Pass a struct value populated from the create/update request.
type PageMetaInput struct {
	Title           string
	MetaTitle       string
	MetaDescription string
}

// ValidatePageMeta enforces title-length and description-length rules at the
// page CRUD boundary. Mirrors the SEO eval engine's "Title Length 30-60" and
// "Description Length 120-160" checks. Empty values are allowed (the SEO
// engine has separate "Has Title" / "Has Meta Description" checks); this
// function only catches off-range values.
func ValidatePageMeta(in PageMetaInput) []Violation {
	var out []Violation

	// Effective title: meta_title overrides the page title when set.
	title := strings.TrimSpace(in.MetaTitle)
	if title == "" {
		title = strings.TrimSpace(in.Title)
	}
	if title != "" {
		n := utf8RuneCount(title)
		if n < 30 {
			out = append(out, Violation{
				Rule:     "title_length",
				Message:  fmt.Sprintf("Title is %d characters; aim for 30 to 60. Format suggestion: 'Specific value | Brand'.", n),
				Severity: "warning",
			})
		} else if n > 60 {
			out = append(out, Violation{
				Rule:     "title_length",
				Message:  fmt.Sprintf("Title is %d characters; max 60. Search engines truncate longer titles in SERPs.", n),
				Severity: "error",
			})
		}
	}

	desc := strings.TrimSpace(in.MetaDescription)
	if desc != "" {
		n := utf8RuneCount(desc)
		if n < 120 {
			out = append(out, Violation{
				Rule:     "description_length",
				Message:  fmt.Sprintf("Meta description is %d characters; aim for 120 to 160. Include a verb / call to action.", n),
				Severity: "warning",
			})
		} else if n > 160 {
			out = append(out, Violation{
				Rule:     "description_length",
				Message:  fmt.Sprintf("Meta description is %d characters; max 160. Search engines truncate longer descriptions.", n),
				Severity: "error",
			})
		}
		// Multi-language CTA-keyword check (mirrors the eval-engine "Meta
		// Description Has CTA" check + Site Inspector's seo.js logic).
		if !ctaKeywordsRE.MatchString(desc) {
			out = append(out, Violation{
				Rule:     "description_cta",
				Message:  "Meta description has no call-to-action verbs. Add a verb like learn / discover / get / try / free / läs / prova / entdecken so the SERP snippet earns clicks.",
				Severity: "warning",
			})
		}
	}

	return out
}

// ctaKeywordsRE mirrors the regex used in internal/eval/seo.go so the agent
// gets the same answer at write time that the build-time evaluator does.
var ctaKeywordsRE = regexp.MustCompile(`(?i)\b(learn|get|discover|find|try|start|book|free|download|read|see|check|explore|calculate|compare|shop|buy|order|save|join|sign up|subscribe|request|claim|unlock|läs|hämta|hitta|prova|börja|boka|gratis|ladda|köp|handla|beställ|spara|upptäck|jämför|registrera|testa|få|erfahren|entdecken|testen|starten|buchen|kostenlos|herunterladen|lesen|vergleichen|kaufen|bestellen|sparen|registrieren|anmelden|sehen|bekommen)\b`)

// emailLeakRE matches plaintext email addresses in agent-supplied copy.
// Block-time enforcement of the eval-engine "No Plaintext Emails" check.
var emailLeakRE = regexp.MustCompile(`[\w.+-]+@[\w.-]+\.[A-Za-z]{2,}`)

// Security-filter patterns. All run against an already-lowercased haystack.
//   - scriptTagRE catches <script with optional whitespace (<  script).
//   - inlineEventHandlerRE catches an on*= handler that sits inside an HTML
//     tag, separated from the preceding token by whitespace OR a slash, so
//     both <div onload=> and <svg/onload=> match while plain prose ("x = y")
//     does not.
//   - dangerousURLSchemes are the script-capable URL schemes. Plain data:
//     is deliberately allowed (inline raster images use data:image/png); only
//     the executable variants are blocked.
var (
	scriptTagRE          = regexp.MustCompile(`<\s*script`)
	inlineEventHandlerRE = regexp.MustCompile(`<[^>]*[\s/]on[a-z]+\s*=`)
	dangerousURLSchemes  = []string{"javascript:", "vbscript:", "data:text/html"}

	// urlControlStripper mirrors the renderer's stripper in
	// internal/builder/pages.go. Both must strip the same set, or the two
	// layers disagree about what a URL says and the gap between them is
	// exactly where an injection lives.
	urlControlStripper = strings.NewReplacer(
		"\t", "", "\n", "", "\r", "", "\x00", "", "\x01", "", "\x02", "", "\x03", "",
		"\x04", "", "\x05", "", "\x06", "", "\x07", "", "\x08", "", "\x0b", "",
		"\x0c", "", "\x0e", "", "\x0f", "", "\x10", "", "\x11", "", "\x12", "",
		"\x13", "", "\x14", "", "\x15", "", "\x16", "", "\x17", "", "\x18", "",
		"\x19", "", "\x1a", "", "\x1b", "", "\x1c", "", "\x1d", "", "\x1e", "", "\x1f", "",
		" ", "",
	)
)

var genericAnchors = []string{
	"click here",
	"read more",
	"learn more",
	"this link",
	"here",
	"more",
}

func isLowQualityAlt(s string) bool {
	t := strings.ToLower(strings.TrimSpace(s))
	switch t {
	case "image", "photo", "picture", "img", "icon", "logo", "graphic":
		return true
	}
	return false
}

func isLowQualityAnchor(s string) bool {
	t := strings.ToLower(strings.TrimSpace(s))
	for _, g := range genericAnchors {
		if t == g {
			return true
		}
	}
	return false
}

func hasNonZeroNumber(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case float64:
		return x > 0
	case int:
		return x > 0
	case int64:
		return x > 0
	case string:
		return strings.TrimSpace(x) != "" && strings.TrimSpace(x) != "0"
	}
	return false
}

func hasInsecureURL(s string) bool {
	// Match http:// not preceded by https. Lowercase the haystack so we catch
	// "HTTP://" too. Skip http://localhost (dev) and http://127.0.0.1.
	low := strings.ToLower(s)
	idx := 0
	for {
		i := strings.Index(low[idx:], "http://")
		if i < 0 {
			return false
		}
		pos := idx + i
		// Skip if it's part of "https://" — should never match since we look
		// for "http://" exactly, but be defensive.
		if pos >= 1 && low[pos-1] == 's' {
			idx = pos + 7
			continue
		}
		// Allow loopback for dev / preview links.
		rest := low[pos+7:]
		if strings.HasPrefix(rest, "localhost") || strings.HasPrefix(rest, "127.0.0.1") {
			idx = pos + 7
			continue
		}
		return true
	}
}

func hasEmptyHeading(blockType, dataJSON, headline, heading, title string) bool {
	// Only fail if the block actually carries a heading-shaped key (so we
	// don't false-positive on text-only blocks that legitimately have no
	// heading slot). Detect by JSON key presence.
	hasKey := strings.Contains(dataJSON, `"headline"`) ||
		strings.Contains(dataJSON, `"heading"`) ||
		strings.Contains(dataJSON, `"title"`)
	if !hasKey {
		return false
	}
	val := firstNonEmpty(headline, heading, title)
	return strings.TrimSpace(val) == ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func utf8RuneCount(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

// ValidateCapability checks if an agent has the required capability.
func ValidateCapability(caps []string, required string) bool {
	for _, c := range caps {
		if c == required {
			return true
		}
	}
	return false
}

// KnownCapabilities is the closed set a minted agent key may carry.
//
// This exists because the capability list is an authorization set, not a
// label: the MCP dispatcher reads it to decide whether a call may proceed
// (RequiresWrite, and RequiresCapability for CSP-widening tools). The mint
// endpoint used to store whatever strings the request body contained, so a
// caller could name a capability the UI never offers and hand it to
// themselves. An unknown string must be refused at the door rather than
// stored and later trusted.
//
// "security" gates CSP allowlist mutation. It is deliberately absent from the
// key-mint UI, and minting is owner/admin-only, so reaching it takes a
// deliberate act by someone already trusted with the site's CSP.
var KnownCapabilities = []string{"read", "write", "build", "deploy", "media", "security"}

// UnknownCapabilities returns the members of caps that are not in
// KnownCapabilities, so a caller can reject the request and name what was
// wrong instead of silently dropping the entries.
func UnknownCapabilities(caps []string) []string {
	var unknown []string
	for _, c := range caps {
		if !ValidateCapability(KnownCapabilities, c) {
			unknown = append(unknown, c)
		}
	}
	return unknown
}

// HasErrors returns true if any violation has "error" severity.
func HasErrors(violations []Violation) bool {
	for _, v := range violations {
		if v.Severity == "error" {
			return true
		}
	}
	return false
}

// SeedDefaultGuardrails creates default guardrail rules for a new site
// using the engine's bound Queries handle. Use SeedDefaultGuardrailsWith
// when you need to run inside a transaction.
func (g *GuardrailEngine) SeedDefaultGuardrails(ctx context.Context, siteID string) error {
	return SeedDefaultGuardrailsWith(ctx, g.queries, siteID)
}

// SeedDefaultGuardrailsWith creates default guardrail rules using the supplied
// Queries handle, allowing callers to pass a tx-bound *store.Queries.
func SeedDefaultGuardrailsWith(ctx context.Context, q *store.Queries, siteID string) error {
	defaults := []struct {
		ruleType string
		target   string
		value    string
		severity string
	}{
		// Forbidden patterns
		{"forbid_pattern", "*", "<iframe", "warning"},

		// Block count limit
		{"max_blocks", "*", "30", "error"},

		// Required blocks on homepage
		{"require_block", "/", `["hero"]`, "warning"},
	}

	for _, d := range defaults {
		if err := q.CreateGuardrail(ctx, store.CreateGuardrailParams{
			ID:       newAgentID(),
			SiteID:   siteID,
			RuleType: d.ruleType,
			Target:   d.target,
			Value:    d.value,
			Severity: d.severity,
		}); err != nil {
			return err
		}
	}

	return nil
}

// SeedDefaultKnowledgebase creates default knowledgebase entries for a new site.
func (g *GuardrailEngine) SeedDefaultKnowledgebase(ctx context.Context, siteID string) error {
	return SeedDefaultKnowledgebaseWith(ctx, g.queries, siteID)
}

// SeedDefaultKnowledgebaseWith creates default knowledgebase entries using the
// supplied Queries handle, allowing callers to pass a tx-bound *store.Queries.
func SeedDefaultKnowledgebaseWith(ctx context.Context, q *store.Queries, siteID string) error {
	defaults := []struct {
		category string
		title    string
		content  string
	}{
		{
			"accessibility",
			"WCAG AA Compliance",
			"All pages must meet WCAG 2.1 AA standards:\n- Color contrast ratio >= 4.5:1 for normal text, >= 3:1 for large text\n- All images must have descriptive alt text (not 'image', 'photo', or empty)\n- Use semantic HTML (main, nav, header, footer landmarks)\n- Include skip navigation link\n- Form inputs must have associated labels\n- Heading hierarchy must be sequential (H1 -> H2 -> H3, no skipping)",
		},
		{
			"technical",
			"No Inline Styles or Scripts",
			"Use global CSS classes for all styling. Never use inline style= attributes or <script> tags in page content. Components should reference CSS classes by name. This ensures consistency across the site and makes theming possible.",
		},
		{
			"seo",
			"Meta Tag Requirements",
			"Every page must have:\n- Title tag: 30-60 characters, page-specific (not site name alone)\n- Meta description: 120-160 characters, includes a call to action\n- Canonical URL\n- Open Graph tags (og:title, og:description, og:image, og:type)\n- Single H1 per page\n- Heading hierarchy (H1 -> H2 -> H3)\n- At least 2 internal links per page",
		},
		{
			"seo",
			"URL Structure Rules",
			"URLs must be:\n- Lowercase only\n- Hyphens for word separation (no underscores)\n- Maximum 3 levels deep (e.g., /services/scanning/details)\n- Under 75 characters\n- Descriptive and human-readable\n- No trailing slashes except root",
		},
		{
			"security",
			"External Resource Policy",
			"Only load external resources (scripts, styles, fonts, iframes) from explicitly allowlisted domains. Never include tracking scripts without consent management. All external scripts must be loaded with the integrity attribute (SRI) when available.",
		},
	}

	for i, d := range defaults {
		if err := q.CreateKnowledgebaseEntry(ctx, store.CreateKnowledgebaseEntryParams{
			ID:        newAgentID(),
			SiteID:    siteID,
			Category:  d.category,
			Title:     d.title,
			Content:   d.content,
			SortOrder: int64(i),
		}); err != nil {
			return err
		}
	}

	return nil
}

// SeedDefaultGlobalBlocksWith creates the default header and footer global blocks
// for a new site using the supplied Queries handle. The header nav is built from
// the site's silos so wizard'd structures show up in the deployed site's menu
// (Home plus each silo as a top-level link).
func SeedDefaultGlobalBlocksWith(ctx context.Context, q *store.Queries, siteID string) error {
	type navLink struct {
		Label string `json:"label"`
		Href  string `json:"href"`
	}

	links := []navLink{{Label: "Home", Href: "/"}}

	silos, err := q.ListSilosBySite(ctx, siteID)
	if err == nil {
		for _, s := range silos {
			label := strings.TrimSpace(s.Name)
			if label == "" {
				continue
			}
			href := strings.TrimSpace(s.SlugPrefix)
			if href == "" {
				continue
			}
			href = strings.TrimPrefix(href, "/")
			links = append(links, navLink{Label: label, Href: "/" + href})
		}
	}

	headerData, _ := json.Marshal(map[string]any{"links": links})

	footerLinks := []navLink{
		{Label: "Privacy", Href: "/privacy"},
		{Label: "Terms", Href: "/terms"},
		{Label: "Cookies", Href: "/cookies"},
	}
	footerData, _ := json.Marshal(map[string]any{
		"copyright": "All rights reserved.",
		"links":     footerLinks,
	})

	defaults := []struct {
		name      string
		slot      string
		blockType string
		dataJSON  string
	}{
		{"Site Header", "header", "header", string(headerData)},
		{"Site Footer", "footer", "footer", string(footerData)},
	}

	for i, d := range defaults {
		if err := q.CreateGlobalBlock(ctx, store.CreateGlobalBlockParams{
			ID:        newAgentID(),
			SiteID:    siteID,
			Name:      d.name,
			Slot:      d.slot,
			BlockType: d.blockType,
			DataJson:  d.dataJSON,
			StyleJson: "{}",
			SortOrder: int64(i),
		}); err != nil {
			return err
		}
	}

	return nil
}

// SeedEssentialPagesWith creates the privacy, terms, cookie, and 404 placeholder
// pages for a new site using the supplied Queries handle.
func SeedEssentialPagesWith(ctx context.Context, q *store.Queries, siteID string) error {
	defaults := []struct {
		title     string
		slug      string
		layout    string
		showInNav int64
	}{
		{"Privacy Policy", "/privacy", "default", 0},
		{"Terms of Service", "/terms", "default", 0},
		{"Cookie Policy", "/cookies", "default", 0},
		{"Page Not Found", "/404", "default", 0},
	}

	for i, d := range defaults {
		if err := q.CreatePage(ctx, store.CreatePageParams{
			ID:        newAgentID(),
			SiteID:    siteID,
			Title:     d.title,
			Slug:      d.slug,
			Layout:    d.layout,
			SortOrder: int64(100 + i),
			ShowInNav: d.showInNav,
		}); err != nil {
			return err
		}
	}

	return nil
}

func unescapeJSON(s string) string {
	s = strings.ReplaceAll(s, "\\u003c", "<")
	s = strings.ReplaceAll(s, "\\u003e", ">")
	s = strings.ReplaceAll(s, "\\u0026", "&")
	return s
}

func joinKeys(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return strings.Join(keys, ", ")
}

func newAgentID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
