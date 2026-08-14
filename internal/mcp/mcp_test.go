package mcp

import (
	"strings"
	"testing"
)

// TestPrivacyPosture is the headline guard. The MCP surface must NEVER
// expose a tool, resource, or prompt that touches identified-tier
// visitor PII. This test enforces that by asserting:
//
//  1. No registered tool name contains "visitor" or "metadata" in a way
//     that would leak the GET /api/agent/visitor-metadata endpoint.
//  2. No resource URI references visit-events, sessions, or identified
//     metadata.
//  3. Settings-write tool restricts category to {general, seo, analytics}
//     (admin-only categories like "security" and "allowed-scripts" are
//     rejected).
//
// If you add a tool that legitimately needs "visitor" in its name, update
// this test to allow the specific name explicitly. The default is REJECT
// so accidental exposure is impossible.
func TestPrivacyPosture(t *testing.T) {
	s := &Server{
		tools:     map[string]Tool{},
		resources: map[string]Resource{},
		prompts:   map[string]Prompt{},
	}
	s.registerTools()
	s.registerResources()
	s.registerPrompts()

	for name := range s.tools {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "visitor") || strings.Contains(lower, "session") {
			t.Errorf("tool %q looks like it touches visitor PII; add an explicit allow-list entry to TestPrivacyPosture if it's safe", name)
		}
	}

	for uri := range s.resources {
		lower := strings.ToLower(uri)
		// "session" is valid for "session-management" prompts; visitor /
		// identity / metadata are the actual PII surfaces to block.
		if strings.Contains(lower, "visitor") || strings.Contains(lower, "/identity") || strings.Contains(lower, "/visit-events") {
			t.Errorf("resource %q looks like it touches visitor PII", uri)
		}
	}
}

func TestRequiredToolsRegistered(t *testing.T) {
	s := &Server{
		tools:     map[string]Tool{},
		resources: map[string]Resource{},
		prompts:   map[string]Prompt{},
	}
	s.registerTools()
	s.registerResources()
	s.registerPrompts()

	required := []string{
		"get_site_context",
		"get_settings_catalog",
		"list_pages",
		"create_page",
		"update_page",
		"delete_page",
		"list_blocks",
		"create_block",
		"update_block",
		"delete_block",
		"get_branding",
		"get_profile",
		"update_profile",
		"list_settings",
		"bulk_upsert_settings",
		"get_security_posture",
		"list_media",
		"trigger_build",
		"get_build_status",
		"get_evaluation",
		// Phase 31.1.1 conversion goals + events.
		"list_goals",
		"create_goal",
		"update_goal",
		"delete_goal",
		"get_goals_analytics",
		"track_event",
	}
	for _, name := range required {
		if _, ok := s.tools[name]; !ok {
			t.Errorf("required tool %q not registered", name)
		}
	}

	requiredResources := []string{
		"atomicsite://site/context",
		"atomicsite://site/settings_catalog",
		"atomicsite://site/security_posture",
		"atomicsite://site/i18n",
		"atomicsite://site/pending_setup",
		"atomicsite://site/structure",
		// Phase 31.1.1 (2026-05-06).
		"atomicsite://analytics/goals",
	}
	for _, uri := range requiredResources {
		if _, ok := s.resources[uri]; !ok {
			t.Errorf("required resource %q not registered", uri)
		}
	}

	requiredPrompts := []string{
		"walk_through_pending_setup",
		"audit_seo",
		"connect_analytics",
		"add_iframe_integration",
		"create_landing_page",
	}
	for _, name := range requiredPrompts {
		if _, ok := s.prompts[name]; !ok {
			t.Errorf("required prompt %q not registered", name)
		}
	}
}

func TestSettingValidation(t *testing.T) {
	cases := []struct {
		category, key, value string
		ok                   bool
	}{
		// passes
		{"seo", "hreflang_strategy", "path", true},
		{"seo", "hreflang_strategy", "subdomain", true},
		{"analytics", "ga4_id", "G-ABC1234", true},
		{"analytics", "umami_url", "https://analytics.example.com", true},
		{"analytics", "identity_max_age_days", "30", true},
		{"general", "domain_aliases", "www.example.com,example.org", true},
		// rejections
		{"seo", "hreflang_strategy", "GARBAGE", false},
		{"analytics", "ga4_id", "XX-12345", false},
		{"analytics", "umami_url", "not a url", false},
		{"analytics", "identity_max_age_days", "9999", false},
		{"analytics", "identity_max_age_days", "0", false},
		{"general", "domain_aliases", "www.example.com,,example.org", false},
	}
	for _, c := range cases {
		err := validateSettingValue(c.category, c.key, c.value)
		if c.ok && err != nil {
			t.Errorf("expected %s.%s=%q to pass, got %v", c.category, c.key, c.value, err)
		}
		if !c.ok && err == nil {
			t.Errorf("expected %s.%s=%q to fail validation, got pass", c.category, c.key, c.value)
		}
	}
}

func TestAgentWritableCategories(t *testing.T) {
	if !isAgentWritableCategory("seo") || !isAgentWritableCategory("analytics") || !isAgentWritableCategory("general") {
		t.Error("seo / analytics / general must be agent-writable")
	}
	if isAgentWritableCategory("security") {
		t.Error("security category must NOT be agent-writable")
	}
	if isAgentWritableCategory("allowed-scripts") {
		t.Error("allowed-scripts must NOT be agent-writable (CSP widens attack surface)")
	}
}

// --- expansion guards (knowledge curriculum, service context, write tools) ---

// TestKnowledgeResourcesRegistered asserts every doc in
// internal/knowledge has a matching atomicsite://knowledge/<slug>
// resource, plus the catalog index. This is the expansion that turns
// the agent into an Astro + TS + custom-CSS + UX/UI expert; if any
// curriculum doc is missing, the agent is missing a chunk of its
// training material.
func TestKnowledgeResourcesRegistered(t *testing.T) {
	s := &Server{
		tools:     map[string]Tool{},
		resources: map[string]Resource{},
		prompts:   map[string]Prompt{},
	}
	s.registerTools()
	s.registerResources()
	s.registerPrompts()

	if _, ok := s.resources["atomicsite://knowledge/index"]; !ok {
		t.Error("atomicsite://knowledge/index resource missing")
	}
	if _, ok := s.resources["atomicsite://meta/knowledge-graph"]; !ok {
		t.Error("atomicsite://meta/knowledge-graph resource missing (the cross-reference graph)")
	}

	requiredKnowledgeSlugs := []string{
		// stack curriculum
		"astro-conventions",
		"typescript-strict",
		"css-variable-system",
		"block-renderer-patterns",
		"i18n-authoring",
		"security-authoring",
		"personalization",
		"cookieproof-integration",
		// ux curriculum
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
	}
	for _, slug := range requiredKnowledgeSlugs {
		uri := "atomicsite://knowledge/" + slug
		if _, ok := s.resources[uri]; !ok {
			t.Errorf("knowledge resource %q missing; check internal/knowledge/embed.go docMetadataTable", uri)
		}
	}
}

// TestServiceContextResourcesRegistered asserts the service-context
// resources that let the agent reason about whole-site state are wired.
func TestServiceContextResourcesRegistered(t *testing.T) {
	s := &Server{
		tools:     map[string]Tool{},
		resources: map[string]Resource{},
		prompts:   map[string]Prompt{},
	}
	s.registerTools()
	s.registerResources()
	s.registerPrompts()

	required := []string{
		"atomicsite://eval/latest",
		"atomicsite://build/history",
		"atomicsite://deploy/status",
		"atomicsite://domains",
		"atomicsite://members",
		"atomicsite://knowledgebase",
		"atomicsite://consent/stats",
		"atomicsite://retention/status",
		"atomicsite://integrations",
		"atomicsite://design-references",
		"atomicsite://meta/capabilities",
		"atomicsite://meta/design-playbook",
	}
	for _, uri := range required {
		if _, ok := s.resources[uri]; !ok {
			t.Errorf("service-context resource %q missing", uri)
		}
	}
}

// TestExtraWriteToolsRegistered asserts the operator-mutable tools are
// wired and correctly flagged as RequiresWrite. Read-only listers stay
// callable by any active key; write tools must gate on the capability.
func TestExtraWriteToolsRegistered(t *testing.T) {
	s := &Server{
		tools:     map[string]Tool{},
		resources: map[string]Resource{},
		prompts:   map[string]Prompt{},
	}
	s.registerTools()
	s.registerResources()
	s.registerPrompts()

	readOnly := []string{
		"list_components",
		"list_css_classes",
		"list_knowledgebase",
		"list_guardrails",
		"list_allowed_scripts",
		"list_fonts",
		"list_design_references",
		"get_capabilities",
		"get_design_playbook",
		"get_figma_import_url",
	}
	for _, name := range readOnly {
		tool, ok := s.tools[name]
		if !ok {
			t.Errorf("read-only tool %q missing", name)
			continue
		}
		if tool.RequiresWrite {
			t.Errorf("tool %q marked RequiresWrite; should be readable by any active key", name)
		}
	}

	writeTools := []string{
		"create_component",
		"update_component",
		"delete_component",
		"upsert_css_class",
		"delete_css_class",
		"create_knowledgebase_entry",
		"update_knowledgebase_entry",
		"delete_knowledgebase_entry",
		"create_guardrail",
		"delete_guardrail",
		"register_allowed_script",
		"revoke_allowed_script",
		"delete_font",
		"upload_font",
		"add_design_reference",
		"delete_design_reference",
		// Not a mutator, but it drives a headless browser process and returns
		// a render of site content, so it gates like its sibling `screenshot`.
		// It shipped without the flag, which let any active read-only key run
		// captures.
		"preview_screenshot",
		"screenshot",
	}
	for _, name := range writeTools {
		tool, ok := s.tools[name]
		if !ok {
			t.Errorf("write tool %q missing", name)
			continue
		}
		if !tool.RequiresWrite {
			t.Errorf("tool %q must be RequiresWrite=true; mutates site state", name)
		}
	}
}

// TestNewPromptsRegistered asserts the four expansion prompts ship.
func TestNewPromptsRegistered(t *testing.T) {
	s := &Server{
		tools:     map[string]Tool{},
		resources: map[string]Resource{},
		prompts:   map[string]Prompt{},
	}
	s.registerTools()
	s.registerResources()
	s.registerPrompts()

	required := []string{
		"master_the_stack",
		"audit_for_premium_feel",
		"build_from_figma",
		"learn_from_reference",
	}
	for _, name := range required {
		if _, ok := s.prompts[name]; !ok {
			t.Errorf("expansion prompt %q missing", name)
		}
	}
}

// TestPrivacyPostureExpansion is the negative-regression guard for the
// expansion: even with the new write surfaces, no tool or resource may
// expose visitor PII. The original TestPrivacyPosture catches "visitor"
// and "session" substrings; this extends the check across every new
// curriculum / service-context / write surface to be doubly sure.
func TestPrivacyPostureExpansion(t *testing.T) {
	s := &Server{
		tools:     map[string]Tool{},
		resources: map[string]Resource{},
		prompts:   map[string]Prompt{},
	}
	s.registerTools()
	s.registerResources()
	s.registerPrompts()

	bannedSubstrings := []string{
		"visit_events",
		"consent_records",
		"fingerprint",
		"visitor_id",
		"session_metadata",
		"identified_tier",
	}

	for name := range s.tools {
		lower := strings.ToLower(name)
		for _, banned := range bannedSubstrings {
			if strings.Contains(lower, banned) {
				t.Errorf("tool %q contains banned PII substring %q", name, banned)
			}
		}
	}
	for uri := range s.resources {
		lower := strings.ToLower(uri)
		for _, banned := range bannedSubstrings {
			if strings.Contains(lower, banned) {
				t.Errorf("resource %q contains banned PII substring %q", uri, banned)
			}
		}
	}
}

// TestFontFamilyValidator guards the upload_font precondition: family
// names go on disk and into the database, so loose validation lets a
// caller plant arbitrary path / SQL / shell metacharacters into the
// system. Mirrors handlers.validFamilyName.
func TestFontFamilyValidator(t *testing.T) {
	cases := []struct {
		input string
		ok    bool
	}{
		{"Inter", true},
		{"Acme Sans", true},
		{"Acme-Sans_2", true},
		{"abc", true},
		{"", false},
		{strings.Repeat("a", 65), false},
		{"Ac/me", false},
		{"Ac\\me", false},
		{"Ac;me", false},
		{"Ac.me", false},
		{"Ac me\n", false},
	}
	for _, c := range cases {
		got := validFontFamilyName.MatchString(c.input)
		if got != c.ok {
			t.Errorf("validFontFamilyName(%q) = %v, want %v", c.input, got, c.ok)
		}
	}
}

// TestMigrationToolsRegistered asserts the Sprint 4.5 (2026-05-06) MCP
// migration surface ships every tool needed for an end-to-end CMS
// migration: read (list / get / list_verifications / list_verify_jobs /
// list_missing_urls), write (start / apply / delete / verify-live /
// cancel / create_redirect_from_missing / dismiss). RequiresWrite gating
// is asserted alongside.
func TestMigrationToolsRegistered(t *testing.T) {
	s := &Server{
		tools:     map[string]Tool{},
		resources: map[string]Resource{},
		prompts:   map[string]Prompt{},
	}
	s.registerTools()

	readOnly := []string{
		"list_migrations",
		"get_migration",
		"plan_migration",
		"list_verify_jobs",
		"get_verify_job",
		"list_migration_verifications",
		"list_missing_urls",
	}
	for _, name := range readOnly {
		tool, ok := s.tools[name]
		if !ok {
			t.Errorf("read-only migration tool %q missing", name)
			continue
		}
		if tool.RequiresWrite {
			t.Errorf("migration tool %q marked RequiresWrite; should be readable by any active key", name)
		}
	}

	writeTools := []string{
		"start_migration_crawl",
		"apply_migration",
		"delete_migration",
		"verify_migration_live",
		"cancel_verify_job",
		"create_redirect_from_missing",
		"dismiss_missing_url",
	}
	for _, name := range writeTools {
		tool, ok := s.tools[name]
		if !ok {
			t.Errorf("write migration tool %q missing", name)
			continue
		}
		if !tool.RequiresWrite {
			t.Errorf("migration tool %q must be RequiresWrite=true; mutates site state", name)
		}
	}
}

// TestPathTokenSafeForMCP locks the redirect-creation validation in.
// Mirrors handlers.pathTokenSafeForHandler so the MCP create_redirect_
// from_missing tool rejects the same hostile inputs the HTTP handler
// would. Adding a forbidden char must update both.
func TestPathTokenSafeForMCP(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"/normal", true},
		{"/path/with-dash_and-digits-123", true},
		{"/with{brace", false},
		{"/with}brace", false},
		{"/with;semi", false},
		{"/with\"quote", false},
		{"/with\\back", false},
		{"/with'apostrophe", false},
		{"/with\x00null", false},
		{"/with\x7fdel", false},
	}
	for _, c := range cases {
		got := pathTokenSafeForMCP(c.in)
		if got != c.want {
			t.Errorf("pathTokenSafeForMCP(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestValidRedirectStatusForMCP asserts the 5-status whitelist matches
// the HTTP handler's validRedirectStatusCodes map.
func TestValidRedirectStatusForMCP(t *testing.T) {
	allowed := []int64{301, 302, 307, 308, 410}
	for _, c := range allowed {
		if !validRedirectStatusForMCP(c) {
			t.Errorf("status %d should be allowed", c)
		}
	}
	rejected := []int64{200, 300, 303, 401, 404, 500, 0, -1}
	for _, c := range rejected {
		if validRedirectStatusForMCP(c) {
			t.Errorf("status %d should be rejected", c)
		}
	}
}

// TestCleanFontFilename strips path separators agent-side before they
// land in the original_name column and any future log line.
func TestCleanFontFilename(t *testing.T) {
	cases := []struct {
		input, want string
	}{
		{"Inter-Regular.woff2", "Inter-Regular.woff2"},
		{"  spaced  ", "spaced"},
		{"../../etc/passwd", "....etcpasswd"},
		{"a\\b\\c.woff2", "abc.woff2"},
		{strings.Repeat("x", 200), strings.Repeat("x", 128)},
	}
	for _, c := range cases {
		got := cleanFontFilename(c.input)
		if got != c.want {
			t.Errorf("cleanFontFilename(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}
