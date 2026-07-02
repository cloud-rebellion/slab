package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/bright-interaction/slab/internal/agent"
	"github.com/bright-interaction/slab/internal/critique"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"
)

// strictDesignLintEnabled reads the per-site `design.strict_lint` setting.
// Default ON (returns true) so new sites get design enforcement out of the
// box; operators flip it via bulk_upsert_settings to "0"/"false"/"off" for a
// relaxed mode. A missing row / DB error keeps the default ON: strict mode is
// the documented default and we do not silently downgrade enforcement.
func strictDesignLintEnabled(ctx context.Context, queries *store.Queries, siteID string) bool {
	row, err := queries.GetSetting(ctx, store.GetSettingParams{
		SiteID:   siteID,
		Category: "design",
		Key:      "strict_lint",
	})
	if err != nil {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(row.Value)) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// siteFidelity resolves the design.fidelity dial for lint gating. Named
// helper (rather than calling agent.FidelityForSite inline) because the
// tool handlers shadow the agent package with their identity parameter.
func siteFidelity(ctx context.Context, queries *store.Queries, siteID string) agent.DesignFidelity {
	return agent.FidelityForSite(ctx, queries, siteID)
}

// runBlockLint is the in-process bridge create_block + update_block
// use to run the synchronous design lint (gap 4 of 6, 2026-05-21).
// Takes whatever shape json.Unmarshal handed back (map[string]any |
// nil | scalar) and normalises to the map the linter expects.
//
// Why this lives in the MCP package: keeps the cross-package
// boundary tight (critique.LintBlockData only deals in map[string]any
// + DesignPlaybookInfo, no MCP framework types) and lets future
// linters fan out from a single hook.
func runBlockLint(blockType, archetype string, raw any, fidelity agent.DesignFidelity) []critique.LintFinding {
	if raw == nil {
		return nil
	}
	data, ok := raw.(map[string]any)
	if !ok {
		// data was authored as a non-object (string, array). Skip lint;
		// the renderer will surface the type error on build.
		return nil
	}
	return critique.LintBlockDataWithArchetype(blockType, data, archetype, agent.DesignPlaybookFor(fidelity))
}

// runBlockLintFromJSON unmarshals a stored DataJson string and lints
// the result. Used by update_block where the post-mutation state comes
// from the DB as a JSON string rather than the just-decoded map.
func runBlockLintFromJSON(blockType, archetype, dataJSON string, fidelity agent.DesignFidelity) []critique.LintFinding {
	dataJSON = strings.TrimSpace(dataJSON)
	if dataJSON == "" || dataJSON == "{}" || dataJSON == "null" {
		return nil
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(dataJSON), &data); err != nil {
		return nil
	}
	return critique.LintBlockDataWithArchetype(blockType, data, archetype, agent.DesignPlaybookFor(fidelity))
}

// registerLintTools wires the standalone lint_block tool. Lets the
// agent run the same lint create_block embeds without actually
// committing the block: useful for the "try a few variants, pick the
// cleanest one" workflow before any DB write.
func (s *Server) registerLintTools() {
	register := func(t Tool) { s.tools[t.Name] = t }

	register(Tool{
		Name:        "lint_block",
		Description: "Runs the synchronous block-level design lint without persisting. Same rule set create_block + update_block embed in their responses: hero_quality (plain text hero with no hero_graphic / bg=circuit / image_id), headline_length (>12 words), custom_block_duplicate_eyebrow, slop_term / slop_name / slop_company / slop_number, archetype_drift (when archetype is supplied and the hero_graphic does not fit the archetype). Input: block_type + data (the JSON the agent is about to commit), optional archetype (one of mesh|pulse|monogram|audit-receipt). Output: design_warnings + design_inspiration. Use this to vet a variant before calling create_block.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"block_type":{"type":"string"},
				"data":{"type":"object"},
				"archetype":{"type":"string"}
			},
			"required":["block_type","data"]
		}`),
		Handler: func(ctx context.Context, identity *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				BlockType string         `json:"block_type"`
				Data      map[string]any `json:"data"`
				Archetype string         `json:"archetype"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			if strings.TrimSpace(args.BlockType) == "" {
				return "", errors.New("block_type required")
			}
			fidelity := siteFidelity(ctx, s.queries, identity.SiteID)
			findings := critique.LintBlockDataWithArchetype(args.BlockType, args.Data, args.Archetype, agent.DesignPlaybookFor(fidelity))
			return mustJSON(map[string]any{
				"design_warnings":    findings,
				"count":              len(findings),
				"design_fidelity":    string(fidelity),
				"would_block":        critique.FilterBlockingFor(findings, fidelity),
				"design_inspiration": critique.InspirationsFor(args.BlockType),
				"hint":               "Each finding has name, severity (warning|info), field, message, fix. Zero findings = the block clears the synchronous design lint; the Inspector still grades the rendered HTML after the next build (against this site's design.fidelity rubric, see design_fidelity). would_block lists the findings that would refuse a strict-mode write. design_inspiration is the curated 2-3 design-corpus references for this block_type (gap 5). Pass archetype to enable the archetype_drift check (gap 6).",
			}), nil
		},
	})
}
