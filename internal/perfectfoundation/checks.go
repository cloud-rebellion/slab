// Package perfectfoundation maps every Site Inspector eval check to one of
// three enforcement strategies (bake / block / teach) and provides the
// auto-seeding entry point that turns a 140/140 reference site into
// the canonical baseline for every AI-built site.
//
// Owner taxonomy:
//
//   - "bake"  — the Astro/nginx output already passes the check by default.
//     Source: internal/builder/{layouts,security,nginx}.go.
//   - "block" — the agent path refuses input that would fail the check.
//     Source: internal/agent/guardrails.go.
//   - "teach" — the agent reads a knowledgebase entry that explains how to
//     pass the check. Source: kb.go in this package.
//
// CheckOwnership is the source of truth for which check is owned by which
// layer. Two tests keep it honest: TestEveryCheckHasOwner (every entry
// carries an owner) and TestOwnershipMatchesEmittedChecks (a two-way
// diff against the check names eval ACTUALLY emits, so a renamed or
// phantom entry fails the build instead of silently rotting).
package perfectfoundation

// Strategy is the enforcement strategy for a single eval check.
type Strategy string

const (
	// StrategyBake means the builder output already satisfies this check.
	StrategyBake Strategy = "bake"
	// StrategyBlock means a guardrail refuses input that would fail this check.
	StrategyBlock Strategy = "block"
	// StrategyTeach means a knowledgebase entry explains how to pass this check.
	StrategyTeach Strategy = "teach"
)

// CheckOwner names the file/function that owns enforcement for a check.
// Empty Owner means the check has no enforcement and must be added.
type CheckOwner struct {
	Category string   // security | seo | geo | performance | accessibility | privacy
	Name     string   // exact eval check name as emitted by internal/eval
	Strategy Strategy // bake | block | teach
	Owner    string   // file:function — empty if not yet owned
}

// CheckOwnership lists every eval check and where it's enforced. New eval
// checks added to internal/eval/*.go MUST be added here too — the test in
// checks_test.go fails the build otherwise.
//
// Names mirror the strings emitted by RunSecurityChecks / RunSEOChecks /
// RunGEOChecks / RunPerformanceChecks / RunAccessibilityChecks /
// RunPrivacyChecks. Keep this list alphabetical within category.
var CheckOwnership = []CheckOwner{
	// --- security (18) ---
	{"security", "Strict-Transport-Security", StrategyBake, "builder/security.go:BuildSecurityHeaders"},
	{"security", "Content-Security-Policy", StrategyBake, "builder/security.go:buildCSP"},
	{"security", "X-Content-Type-Options: nosniff", StrategyBake, "builder/security.go:BuildSecurityHeaders"},
	{"security", "X-Frame-Options", StrategyBake, "builder/security.go:BuildSecurityHeaders"},
	{"security", "Referrer-Policy", StrategyBake, "builder/security.go:BuildSecurityHeaders"},
	{"security", "Permissions-Policy", StrategyBake, "builder/security.go:BuildSecurityHeaders"},
	{"security", "Cross-Origin-Opener-Policy", StrategyBake, "builder/security.go:BuildSecurityHeaders"},
	{"security", "Cross-Origin-Resource-Policy", StrategyBake, "builder/security.go:BuildSecurityHeaders"},
	{"security", "CSP Quality", StrategyBake, "builder/security.go:buildCSP"},
	{"security", "Subresource Integrity", StrategyTeach, "kb.go:sri"},
	{"security", "No Mixed Content", StrategyBlock, "agent/guardrails.go:ValidateBlock"},
	{"security", "security.txt", StrategyBake, "builder/security.go:RenderSecurityTxt"},
	{"security", "CSP meta tag", StrategyBake, "builder/layouts.go:RenderLayouts"},
	{"security", "Headers In Deploy Artifacts", StrategyBake, "builder/security.go:BuildSecurityHeaders"},
	{"security", "HSTS max-age 1y+", StrategyBake, "builder/security.go:BuildSecurityHeaders"},
	{"security", "HSTS includeSubDomains", StrategyBake, "builder/security.go:BuildSecurityHeaders"},
	{"security", "HSTS preload directive", StrategyTeach, "kb.go:hsts-preload"},
	{"security", "CSP upgrade-insecure-requests", StrategyBake, "builder/security.go:buildCSP"},

	// --- seo (37) ---
	{"seo", "Has Title", StrategyBlock, "agent/guardrails.go:ValidatePageMeta"},
	{"seo", "Title Length 30-60", StrategyBlock, "agent/guardrails.go:ValidatePageMeta"},
	{"seo", "Has Meta Description", StrategyBlock, "agent/guardrails.go:ValidatePageMeta"},
	{"seo", "Meta Description 120-160", StrategyBlock, "agent/guardrails.go:ValidatePageMeta"},
	{"seo", "Has H1", StrategyTeach, "kb.go:headings"},
	{"seo", "Single H1", StrategyBlock, "agent/guardrails.go:ValidateBlock"},
	{"seo", "Heading Hierarchy", StrategyTeach, "kb.go:headings"},
	{"seo", "Images Have Alt Text", StrategyBlock, "agent/guardrails.go:ValidateBlock"},
	{"seo", "Canonical URL", StrategyBake, "builder/layouts.go:RenderLayouts"},
	{"seo", "Language Declared", StrategyBake, "builder/layouts.go:RenderLayouts"},
	{"seo", "Viewport Meta", StrategyBake, "builder/layouts.go:RenderLayouts"},
	{"seo", "Charset Declared", StrategyBake, "builder/layouts.go:RenderLayouts"},
	{"seo", "HTML5 Doctype", StrategyBake, "builder/layouts.go:RenderLayouts"},
	{"seo", "Open Graph Tags", StrategyBake, "builder/layouts.go:RenderLayouts"},
	{"seo", "Not Noindexed", StrategyTeach, "kb.go:robots"},
	{"seo", "URL Length < 75", StrategyBlock, "agent/guardrails.go:ValidatePageSlug"},
	{"seo", "URL Lowercase", StrategyBlock, "agent/guardrails.go:ValidatePageSlug"},
	{"seo", "URL No Underscores", StrategyBlock, "agent/guardrails.go:ValidatePageSlug"},
	{"seo", "Has Internal Links", StrategyTeach, "kb.go:internal-links"},
	{"seo", "robots.txt", StrategyBake, "builder/security.go:RenderRobotsTxt"},
	{"seo", "XML Sitemap", StrategyBake, "builder/security.go:RenderSitemap"},
	{"seo", "llms.txt", StrategyBake, "builder/security.go:RenderLLMsTxt"},
	{"seo", "Hreflang Tags", StrategyBake, "builder/layouts.go:RenderLayouts"},
	{"seo", "Title Not Truncated", StrategyBlock, "agent/guardrails.go:ValidatePageMeta"},
	{"seo", "Meta Description Has CTA", StrategyBlock, "agent/guardrails.go:ValidatePageMeta"},
	{"seo", "Content Length 300+", StrategyBlock, "agent/guardrails.go:validateBlockEvalChecks"},
	{"seo", "Descriptive Alt Text", StrategyBlock, "agent/guardrails.go:validateBlockEvalChecks"},
	{"seo", "No Empty Links", StrategyBlock, "agent/guardrails.go:validateBlockEvalChecks"},
	{"seo", "No Broken Anchor Links", StrategyTeach, "kb.go:anchor-links"},
	{"seo", "Canonical Matches URL", StrategyBake, "builder/layouts.go:RenderLayouts"},
	{"seo", "Favicon", StrategyBake, "builder/layouts.go:RenderLayouts"},
	{"seo", "Apple Touch Icon", StrategyBake, "builder/layouts.go:RenderLayouts"},
	{"seo", "No Duplicate Meta Tags", StrategyBake, "builder/layouts.go:RenderLayouts"},
	{"seo", "OG Completeness", StrategyBake, "builder/layouts.go:RenderLayouts"},
	{"seo", "OG Image Size 1200x630", StrategyBake, "builder/layouts.go:RenderLayouts"},
	{"seo", "Twitter Card", StrategyBake, "builder/layouts.go:RenderLayouts"},
	{"seo", "Twitter Image", StrategyBake, "builder/layouts.go:RenderLayouts"},
	// "No Plaintext Emails" is deliberately absent: eval emits it as
	// Info only (unscored); the real enforcement is the block-time
	// guardrail, documented at its emission site in eval/seo.go.
	{"seo", "FAQ Schema Has Visible Content", StrategyTeach, "kb.go:faq-schema"},

	// --- geo (7) ---
	{"geo", "Robots max-snippet directive", StrategyBake, "builder/layouts.go:RenderLayouts"},
	{"geo", "Robots max-image-preview directive", StrategyBake, "builder/layouts.go:RenderLayouts"},
	{"geo", "BreadcrumbList Schema", StrategyTeach, "kb.go:schema-breadcrumb"},
	{"geo", "Organization Schema Completeness", StrategyBake, "builder/layouts.go:buildOrganizationJSONLD"},
	{"geo", "Organization Schema Completeness (core)", StrategyBake, "builder/layouts.go:buildOrganizationJSONLD"},
	{"geo", "Organization Schema Completeness (entity depth)", StrategyTeach, "kb.go:schema-org"},
	{"geo", "Content Freshness Signal", StrategyTeach, "kb.go:content-freshness"},
	{"geo", "Noindex Pages Excluded From Sitemap", StrategyBake, "builder/security.go:RenderSitemap"},
	{"geo", "Noindex Pages Excluded From Sitemap (mostly clean)", StrategyBake, "builder/security.go:RenderSitemap"},
	{"geo", "Noindex Pages Excluded From Sitemap (leaks)", StrategyBake, "builder/security.go:RenderSitemap"},
	{"geo", "AI-Friendly Formatting", StrategyTeach, "kb.go:ai-formatting"},

	// --- performance (8) ---
	{"performance", "HTML Size", StrategyTeach, "kb.go:html-size"},
	{"performance", "No Render-Blocking Scripts", StrategyTeach, "kb.go:render-blocking"},
	{"performance", "Modern Image Formats", StrategyBake, "builder/media.go:GenerateVariants"},
	{"performance", "Lazy-Loaded Images", StrategyTeach, "kb.go:lazy-images"},
	{"performance", "Images Have Dimensions", StrategyBlock, "agent/guardrails.go:ValidateBlock"},
	{"performance", "Inline CSS Size", StrategyTeach, "kb.go:inline-css"},
	{"performance", "Resource Hints", StrategyBake, "builder/layouts.go:RenderLayouts"},
	{"performance", "Total HTML Size", StrategyTeach, "kb.go:html-size"},

	// --- accessibility (18) ---
	{"accessibility", "Main Landmark", StrategyTeach, "kb.go:landmarks"},
	{"accessibility", "Navigation Landmark", StrategyTeach, "kb.go:landmarks"},
	{"accessibility", "Banner Landmark", StrategyTeach, "kb.go:landmarks"},
	{"accessibility", "Contentinfo Landmark", StrategyTeach, "kb.go:landmarks"},
	{"accessibility", "Page Language", StrategyBake, "builder/layouts.go:RenderLayouts"},
	{"accessibility", "Page Has H1", StrategyTeach, "kb.go:headings"},
	{"accessibility", "Heading Order", StrategyTeach, "kb.go:headings"},
	{"accessibility", "No Empty Headings", StrategyBlock, "agent/guardrails.go:ValidateBlock"},
	{"accessibility", "Images Have Alt Text", StrategyBlock, "agent/guardrails.go:ValidateBlock"},
	{"accessibility", "Form Inputs Have Labels", StrategyTeach, "kb.go:form-labels"},
	{"accessibility", "Buttons Have Labels", StrategyBlock, "agent/guardrails.go:ValidateBlock"},
	{"accessibility", "Links Have Labels", StrategyBlock, "agent/guardrails.go:ValidateBlock"},
	{"accessibility", "No Positive Tabindex", StrategyTeach, "kb.go:tabindex"},
	{"accessibility", "Tables Have Headers", StrategyTeach, "kb.go:table-headers"},
	{"accessibility", "Skip Navigation Link", StrategyBake, "builder/layouts.go:RenderLayouts"},
	{"accessibility", "Autocomplete Attributes", StrategyTeach, "kb.go:autocomplete"},
	{"accessibility", "Focus Indicators", StrategyBake, "builder/layouts.go:RenderLayouts"},
	{"accessibility", "No Focusable in aria-hidden", StrategyTeach, "kb.go:aria-hidden"},

	// --- privacy (7) ---
	{"privacy", "Consent Banner", StrategyBake, "builder/cookieproof.go:RenderCookieProofSnippet"},
	{"privacy", "Tracker Count", StrategyTeach, "kb.go:tracker-count"},
	{"privacy", "Tracker Count (within budget)", StrategyTeach, "kb.go:tracker-count"},
	{"privacy", "Tracker Count (zero-tracker goal)", StrategyTeach, "kb.go:tracker-count"},
	{"privacy", "Pre-Consent Tracking", StrategyBake, "builder/layouts.go:RenderLayouts"},
	{"privacy", "AI Training Bots Blocked", StrategyBake, "builder/security.go:RenderRobotsTxt"},
	{"privacy", "Privacy Policy Link", StrategyBake, "agent/guardrails.go:SeedEssentialPagesWith"},
	{"privacy", "Terms / Cookie Policy Link", StrategyBake, "agent/guardrails.go:SeedEssentialPagesWith"},
	{"privacy", "Cookie Count", StrategyBake, "builder/layouts.go:RenderLayouts"},
}

// CountByStrategy returns the number of checks owned by each strategy.
func CountByStrategy() map[Strategy]int {
	out := map[Strategy]int{}
	for _, c := range CheckOwnership {
		out[c.Strategy]++
	}
	return out
}

// CountByCategory returns the number of checks per category.
func CountByCategory() map[string]int {
	out := map[string]int{}
	for _, c := range CheckOwnership {
		out[c.Category]++
	}
	return out
}
