package mcp

import (
	"sort"

	"github.com/bright-interaction/slab/internal/knowledge"
)

// AgentSurface is the operator-facing inventory of what an agent
// connecting to this MCP server can see and do. Returned by
// Server.AgentSurface() and consumed by the admin Settings -> Agent
// page (handlers/agent_admin_surface.go), so an operator can view the
// surface without speaking JSON-RPC.
//
// Distinct from atomicsite://meta/capabilities (which is agent-facing
// and includes the agent identity context). This is admin-facing and
// site-scoped: same site_id from the route, but no agent key required
// because the page is gated by the regular admin session + site
// access middleware.
type AgentSurface struct {
	Counts            AgentSurfaceCounts   `json:"counts"`
	Tools             []AgentToolMeta      `json:"tools"`
	Resources         []AgentResourceMeta  `json:"resources"`
	Prompts           []AgentPromptMeta    `json:"prompts"`
	Knowledge         []AgentKnowledgeMeta `json:"knowledge"`
	PrivacyInvariants []string             `json:"privacy_invariants"`
	Endpoint          string               `json:"endpoint"`
	ProtocolVersion   string               `json:"protocol_version"`
	Version           string               `json:"version"`
}

// AgentSurfaceCounts is the small at-a-glance summary the admin page
// renders at the top of the panel.
type AgentSurfaceCounts struct {
	Tools         int `json:"tools"`
	WriteTools    int `json:"write_tools"`
	Resources     int `json:"resources"`
	Prompts       int `json:"prompts"`
	KnowledgeDocs int `json:"knowledge_docs"`
}

// AgentToolMeta carries the public fields of one registered tool.
type AgentToolMeta struct {
	Name          string `json:"name"`
	Description   string `json:"description"`
	RequiresWrite bool   `json:"requires_write"`
}

// AgentResourceMeta carries the public fields of one resource URI.
type AgentResourceMeta struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description"`
	MimeType    string `json:"mime_type"`
}

// AgentPromptMeta carries the public fields of one prompt template.
type AgentPromptMeta struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// AgentKnowledgeMeta is one curriculum doc entry, derived from
// knowledge.Catalog().
type AgentKnowledgeMeta struct {
	Slug     string `json:"slug"`
	Title    string `json:"title"`
	Category string `json:"category"`
	Order    int    `json:"order"`
	Summary  string `json:"summary"`
}

// shieldInvariants prepends the LLM-boundary tokenization invariant
// when shield is enabled. Kept in this file so the operator-facing
// admin surface accurately reflects the live posture without re-
// reading server.go.
func shieldInvariants(enabled bool, base []string) []string {
	if !enabled {
		return base
	}
	out := make([]string, 0, len(base)+1)
	out = append(out, "Every PII field crossing this MCP boundary is tokenized: the agent reads context + metadata only, never raw values.")
	return append(out, base...)
}

// AgentSurface walks the live registries + the embedded curriculum
// and returns the inventory in deterministic order. Cheap; the slices
// have <=50 entries and are built once per request.
func (s *Server) AgentSurface() AgentSurface {
	tools := make([]AgentToolMeta, 0, len(s.tools))
	writeTools := 0
	for _, t := range s.tools {
		tools = append(tools, AgentToolMeta{
			Name:          t.Name,
			Description:   t.Description,
			RequiresWrite: t.RequiresWrite,
		})
		if t.RequiresWrite {
			writeTools++
		}
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })

	resources := make([]AgentResourceMeta, 0, len(s.resources))
	for _, r := range s.resources {
		mime := r.MimeType
		if mime == "" {
			mime = "application/json"
		}
		resources = append(resources, AgentResourceMeta{
			URI:         r.URI,
			Name:        r.Name,
			Description: r.Description,
			MimeType:    mime,
		})
	}
	sort.Slice(resources, func(i, j int) bool { return resources[i].URI < resources[j].URI })

	prompts := make([]AgentPromptMeta, 0, len(s.prompts))
	for _, p := range s.prompts {
		prompts = append(prompts, AgentPromptMeta{
			Name:        p.Name,
			Description: p.Description,
		})
	}
	sort.Slice(prompts, func(i, j int) bool { return prompts[i].Name < prompts[j].Name })

	docs := make([]AgentKnowledgeMeta, 0)
	for _, d := range knowledge.Catalog() {
		docs = append(docs, AgentKnowledgeMeta{
			Slug:     d.Slug,
			Title:    d.Title,
			Category: string(d.Category),
			Order:    d.Order,
			Summary:  d.Summary,
		})
	}

	return AgentSurface{
		Counts: AgentSurfaceCounts{
			Tools:         len(tools),
			WriteTools:    writeTools,
			Resources:     len(resources),
			Prompts:       len(prompts),
			KnowledgeDocs: len(docs),
		},
		Tools:     tools,
		Resources: resources,
		Prompts:   prompts,
		Knowledge: docs,
		PrivacyInvariants: shieldInvariants(s.ShieldEnabled(), []string{
			"No tool reads visitor metadata, sessions, fingerprints, or identified-tier records.",
			"consent/stats returns aggregates only.",
			"raw_astro and security-category writes are admin-only.",
			"search_design_corpus reads only the bundled MIT-licensed reference repos; no PII, no site state.",
			"design_critique reads only the calling site's own evaluations rows; cross-site reads are blocked by AgentAuthMiddleware.",
		}),
		Endpoint:        "/api/agent/mcp",
		ProtocolVersion: Protocol,
		Version:         "1.0.0",
	}
}
