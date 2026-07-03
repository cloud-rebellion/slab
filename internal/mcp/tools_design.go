// Package mcp: tools_design.go wires two design-skill MCP tools that
// expose atomicsite's existing design surfaces to agents:
//
//   - search_design_corpus  : ripgrep-style search over the bundled
//     MIT-licensed reference repos
//     (astrowind, astro-paper, starlight,
//     shadcn-svelte, shadcn-ui sparse).
//   - design_critique       : returns the persisted "design" category
//     evaluations for a build, derived from
//     DesignPlaybook anti-patterns + slop-term
//     rules. Read-only; the critique itself
//     runs in the post-build hook.
//
// Both tools are read-only, no PII, no site-state mutation. The corpus
// is public MIT code; the critique reads only the site's own
// previously-stored evaluations rows. Both gated by the existing
// AgentAuthMiddleware so an unauthenticated caller can't enumerate.
package mcp

import (
	"context"
	"encoding/json"

	"github.com/bright-interaction/slab/internal/config"
	"github.com/bright-interaction/slab/internal/handlers"
	authmw "github.com/bright-interaction/slab/internal/middleware"
)

func (s *Server) registerDesignTools() {
	register := func(t Tool) { s.tools[t.Name] = t }

	register(Tool{
		Name:        "search_design_corpus",
		Description: "Searches the bundled MIT-licensed reference repos (astrowind, astro-paper, starlight, shadcn-svelte, shadcn-ui) for a pattern. Returns file paths plus 3-line snippets. Use this BEFORE writing a new component to see how 5 well-designed repos solve the same problem. Inputs: query (required, literal substring; prefix with 're:' for regex), kind (code|css|tokens|all, default all), repos (string[], filter to specific reference repos), max_results (1-50, default 20).",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"query":{"type":"string"},
				"kind":{"type":"string","enum":["code","css","tokens","all"]},
				"repos":{"type":"array","items":{"type":"string"}},
				"max_results":{"type":"integer","minimum":1,"maximum":50}
			},
			"required":["query"]
		}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var req handlers.SearchDesignCorpusRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				return "", err
			}
			cfg := &config.Config{DesignReferencesDir: s.designReferencesDir}
			res, err := handlers.SearchDesignCorpus(ctx, cfg, req)
			if err != nil {
				return "", err
			}
			return mustJSON(res), nil
		},
	})

	register(Tool{
		Name:        "design_critique",
		Description: "Returns the design critique findings for a build, derived from DesignPlaybook anti-patterns and slop-term rules. Findings are graded (Score, MaxScore, Grade) and per-check categorised as fail / info / pass. Inputs: build_id (optional; defaults to the latest successful build for this site). Run this after a build to catch generic AI tells before shipping.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"build_id":{"type":"string"}
			}
		}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var req handlers.DesignCritiqueRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				return "", err
			}
			res, err := handlers.RunDesignCritique(ctx, s.queries, agent.SiteID, req)
			if err != nil {
				return "", err
			}
			return mustJSON(res), nil
		},
	})
}
