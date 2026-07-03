package mcp

import (
	"context"
	"fmt"

	"github.com/bright-interaction/slab/internal/knowledge"
	authmw "github.com/bright-interaction/slab/internal/middleware"
)

// registerKnowledgeResources exposes the curriculum in
// internal/knowledge/docs/ as MCP resources. The catalog lives at
// atomicsite://knowledge/index; each doc is reachable at
// atomicsite://knowledge/<slug> with mimeType text/markdown.
//
// The resources are agent-readable regardless of capability bit; teaching
// the agent costs nothing and serves any session shape.
func (s *Server) registerKnowledgeResources() {
	register := func(r Resource) { s.resources[r.URI] = r }

	register(Resource{
		URI:         "atomicsite://knowledge/index",
		Name:        "Knowledge catalog",
		Description: "Catalog of every curriculum doc the agent can read: stack mastery (Astro, TypeScript, the CSS-variable system, blocks, i18n, security, personalization, cookieproof) and UX mastery (typography, color, spacing, motion, accessibility, performance, forms, nav, dark mode, premium-design principles). Bodies are stripped here; fetch atomicsite://knowledge/<slug> for the full text.",
		MimeType:    "application/json",
		Reader: func(ctx context.Context, agent *authmw.AgentIdentity) (string, error) {
			return mustJSON(map[string]any{
				"docs": knowledge.Catalog(),
				"hint": "Read atomicsite://knowledge/<slug> for full body. Use the master_the_stack prompt to walk the curriculum in reading order.",
			}), nil
		},
	})

	for _, slug := range knowledge.Slugs() {
		slug := slug
		doc, _ := knowledge.Get(slug)
		register(Resource{
			URI:         fmt.Sprintf("atomicsite://knowledge/%s", slug),
			Name:        doc.Title,
			Description: doc.Summary,
			MimeType:    "text/markdown",
			Reader: func(ctx context.Context, _ *authmw.AgentIdentity) (string, error) {
				d, ok := knowledge.Get(slug)
				if !ok {
					return "", fmt.Errorf("knowledge doc %q not found", slug)
				}
				return d.Body, nil
			},
		})
	}

	// Knowledge graph: nodes (docs, tools, resources, prompts, tags) +
	// edges (mentions, tagged, ordered_after) so the agent can navigate
	// the curriculum and the live MCP surface efficiently. Inspired by
	// the user's graphify tool which produces ~71x token reduction over
	// raw source-file reads. The graph is computed on each read against
	// the live tool/resource/prompt registries; cheap (small fixed
	// inputs) and always in sync.
	register(Resource{
		URI:         "atomicsite://meta/knowledge-graph",
		Name:        "Knowledge graph",
		Description: "Graph of every doc, tool, resource, prompt, and concept tag with edges typed by kind (mentions, tagged, ordered_after). Read this once at session start to navigate the curriculum and the MCP surface efficiently; follow up with resources/read or tools/list to fetch the picked nodes.",
		MimeType:    "application/json",
		Reader: func(ctx context.Context, _ *authmw.AgentIdentity) (string, error) {
			toolNames := make([]string, 0, len(s.tools))
			for name := range s.tools {
				toolNames = append(toolNames, name)
			}
			resourceURIs := make([]string, 0, len(s.resources))
			for uri := range s.resources {
				resourceURIs = append(resourceURIs, uri)
			}
			promptNames := make([]string, 0, len(s.prompts))
			for name := range s.prompts {
				promptNames = append(promptNames, name)
			}
			g := knowledge.BuildGraph(toolNames, resourceURIs, promptNames)
			return mustJSON(g), nil
		},
	})
}
