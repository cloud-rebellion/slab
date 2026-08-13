// Package handlers: the single guardrail choke point for block payload writes.
//
// The 2026-08-11 audit counted six writers that put a tenant-authored payload
// into blocks.data_json, global_blocks.data_json or block_locales.data_json,
// and only three of them ran the guardrail engine. Global blocks were the worst
// of the three misses: they render in the header and footer of EVERY page, so a
// <script> payload refused by create_block was accepted by
// PUT /api/agent/global/{slot} and shipped site-wide.
//
// Every one of those misses had the same shape: a new writer was added, nobody
// remembered the rule, and no test could tell. So the rule now lives in one
// function with one name, and block_payload_coverage_test.go derives the caller
// list from the AST and fails on any writer that does not call it. Adding a
// seventh writer is covered on the day it is written.

package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/bright-interaction/slab/internal/agent"
)

// validateBlockWrite runs the site's guardrail rules plus the built-in security
// checks against a block payload. Returns the violations and whether the write
// must be refused.
//
// A nil engine fails CLOSED. Every constructor in this package builds one from
// its *store.Queries, so nil means a new handler was wired without the guard,
// which is exactly the omission this file exists to stop. Skipping enforcement
// on a mis-wired handler is how writers 4, 5 and 6 stayed invisible.
func validateBlockWrite(ctx context.Context, g *agent.GuardrailEngine, siteID, blockType, dataJSON string) ([]agent.Violation, bool) {
	if g == nil {
		return []agent.Violation{{
			Rule:     "enforcement_unavailable",
			Message:  "Guardrail engine is not wired on this handler; refusing the write rather than skipping enforcement.",
			Severity: "error",
		}}, true
	}
	v := g.ValidateBlock(ctx, siteID, blockType, dataJSON)
	return v, agent.HasErrors(v)
}

// writeGuardrailViolations emits the 422 the agent and admin surfaces already
// return for a refused block write, so a client switching between REST, MCP and
// the admin SPA sees one response shape.
func writeGuardrailViolations(w http.ResponseWriter, violations []agent.Violation) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"error":      "Guardrail violations",
		"violations": violations,
	})
}

// guardrailViolationError is the non-HTTP form, for the agent-facing helpers
// that return an error instead of writing a response.
func guardrailViolationError(violations []agent.Violation) error {
	msgs := make([]string, 0, len(violations))
	for _, v := range violations {
		if v.Severity == "error" {
			msgs = append(msgs, v.Rule+": "+v.Message)
		}
	}
	return errors.New("guardrail violations: " + strings.Join(msgs, "; "))
}
