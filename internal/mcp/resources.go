package mcp

import (
	"context"

	authmw "github.com/bright-interaction/slab/internal/middleware"
)

// registerResources wires read-only resources accessible by URI. Same
// privacy stance as tools: visitor-metadata + identified-tier reads are
// explicitly absent. URIs use the atomicsite:// scheme so hosts can
// distinguish them from web URLs without ambiguity.
func (s *Server) registerResources() {
	register := func(r Resource) { s.resources[r.URI] = r }

	register(Resource{
		URI:         "atomicsite://site/context",
		Name:        "Site context",
		Description: "Full site context (the GET /api/agent/context payload). Read this first on every new session.",
		MimeType:    "application/json",
		Reader: func(ctx context.Context, agent *authmw.AgentIdentity) (string, error) {
			c, err := s.context.Build(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			return mustJSON(c), nil
		},
	})

	register(Resource{
		URI:         "atomicsite://site/settings_catalog",
		Name:        "Settings catalog",
		Description: "Per-key dictionary of every setting the builder consumes, with type, valid range, current value, agent-writable flag, and admin URL. The agent's source of truth for what each setting does.",
		MimeType:    "application/json",
		Reader: func(ctx context.Context, agent *authmw.AgentIdentity) (string, error) {
			c, err := s.context.Build(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			return mustJSON(c.SettingsCatalog), nil
		},
	})

	register(Resource{
		URI:         "atomicsite://site/security_posture",
		Name:        "Security posture",
		Description: "Resolved security headers + trusted-domain list (per kind) + auto-generated files inventory. Read-only mirror of the human admin's Security settings preview.",
		MimeType:    "application/json",
		Reader: func(ctx context.Context, agent *authmw.AgentIdentity) (string, error) {
			c, err := s.context.Build(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			return mustJSON(c.SecurityPosture), nil
		},
	})

	register(Resource{
		URI:         "atomicsite://site/i18n",
		Name:        "i18n configuration",
		Description: "Default lang, declared additional langs, hreflang strategy, locale roots that have published pages, meta-title/description templates, canonical base. Read this before authoring multi-locale pages.",
		MimeType:    "application/json",
		Reader: func(ctx context.Context, agent *authmw.AgentIdentity) (string, error) {
			c, err := s.context.Build(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			return mustJSON(c.I18n), nil
		},
	})

	register(Resource{
		URI:         "atomicsite://site/pending_setup",
		Name:        "Pending setup",
		Description: "Configuration gaps the agent should resolve before declaring the site 'done' (missing business name, contact email, OG image, primary color still wizard-default, etc). The first thing to walk through with a new user.",
		MimeType:    "application/json",
		Reader: func(ctx context.Context, agent *authmw.AgentIdentity) (string, error) {
			c, err := s.context.Build(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			return mustJSON(c.PendingSetup), nil
		},
	})

	register(Resource{
		URI:         "atomicsite://site/structure",
		Name:        "Site structure",
		Description: "Pages + global blocks + silos. Use this to navigate the site without calling list_pages + list_blocks per page.",
		MimeType:    "application/json",
		Reader: func(ctx context.Context, agent *authmw.AgentIdentity) (string, error) {
			c, err := s.context.Build(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			return mustJSON(c.Structure), nil
		},
	})

	// Curriculum (atomicsite://knowledge/*) and service-context
	// resources (atomicsite://eval/*, build/history, deploy/status,
	// domains, members, knowledgebase, consent/stats, retention/status,
	// integrations, design-references, meta/capabilities) live in
	// dedicated files so this one stays focused on the original
	// site-shape resources.
	s.registerKnowledgeResources()
	s.registerServiceContextResources()
}
