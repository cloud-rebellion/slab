// Package handlers: design_critique.go is a thin read wrapper that
// returns the persisted "design" category evaluations row(s) for a
// given build. Surfaced via the design_critique MCP tool.
//
// Critique itself runs from internal/critique inside the post-build
// goroutine in build.go (alongside eval.Run). This handler just reads
// what was stored, mirroring how /api/agent/evaluation/{buildID}
// surfaces the security/SEO/perf/a11y/privacy categories.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/bright-interaction/slab/internal/store"
)

// DesignCritiqueRequest is the MCP tool input shape.
type DesignCritiqueRequest struct {
	BuildID string `json:"build_id"`
}

// DesignCritiqueCheck is one persisted critique finding (mirrors
// eval.CheckResult fields the caller cares about).
type DesignCritiqueCheck struct {
	Name           string `json:"name"`
	Section        string `json:"section"`
	Passed         *bool  `json:"passed"`
	Severity       string `json:"severity,omitempty"`
	Detail         string `json:"detail,omitempty"`
	Recommendation string `json:"recommendation,omitempty"`
}

// DesignCritiqueResponse is the MCP tool output shape.
type DesignCritiqueResponse struct {
	BuildID string                `json:"build_id"`
	Score   int64                 `json:"score"`
	Max     int64                 `json:"max_score"`
	Grade   string                `json:"grade"`
	Checks  []DesignCritiqueCheck `json:"checks"`
}

// RunDesignCritique fetches the persisted critique for the given
// build. If buildID is empty, falls back to the most recent
// successful build for the site. Returns a clean error when no
// critique row exists (build hasn't run yet, or build pre-dates the
// critique feature).
func RunDesignCritique(ctx context.Context, queries *store.Queries, siteID string, req DesignCritiqueRequest) (*DesignCritiqueResponse, error) {
	if siteID == "" {
		return nil, errors.New("site_id is required")
	}
	buildID := req.BuildID
	if buildID == "" {
		recent, err := queries.ListRecentDeploymentsBySite(ctx, store.ListRecentDeploymentsBySiteParams{
			SiteID: siteID,
			Limit:  10,
		})
		if err != nil {
			return nil, fmt.Errorf("list recent deployments: %w", err)
		}
		for _, d := range recent {
			if d.Status == "success" {
				buildID = d.ID
				break
			}
		}
		if buildID == "" {
			return nil, errors.New("no successful build found for site; run a build first")
		}
	}

	rows, err := queries.ListEvaluationsByBuild(ctx, buildID)
	if err != nil {
		return nil, fmt.Errorf("list evaluations: %w", err)
	}

	for _, row := range rows {
		if row.Category != "design" {
			continue
		}
		var raw []json.RawMessage
		_ = json.Unmarshal([]byte(row.ChecksJson), &raw)
		checks := make([]DesignCritiqueCheck, 0, len(raw))
		for _, m := range raw {
			var c DesignCritiqueCheck
			if err := json.Unmarshal(m, &c); err != nil {
				continue
			}
			checks = append(checks, c)
		}
		sort.SliceStable(checks, func(i, j int) bool {
			// Fails first, then info, then passes. Stable so playbook
			// order is preserved within each band.
			ai, aj := bandFor(checks[i].Passed), bandFor(checks[j].Passed)
			return ai < aj
		})
		return &DesignCritiqueResponse{
			BuildID: buildID,
			Score:   row.Score,
			Max:     row.MaxScore,
			Grade:   row.Grade,
			Checks:  checks,
		}, nil
	}

	return nil, fmt.Errorf("no design critique recorded for build %s; rebuild to generate one", buildID)
}

// bandFor groups checks: 0 = fails, 1 = info, 2 = passes.
func bandFor(passed *bool) int {
	if passed == nil {
		return 1
	}
	if !*passed {
		return 0
	}
	return 2
}
