// Package mcp: tools_goals.go (Phase 31.1.1, 2026-05-06).
//
// MCP tools that wrap the conversion-goals admin REST surface
// (handlers/conversion_goals.go) so an agent can set up + maintain
// per-site goals and read aggregated rates without dropping to raw
// HTTP. Same site-scoped guard every other tool uses: the agent's
// SiteID is the only siteID the tool ever sees, so cross-tenant
// access is impossible at this layer.
//
// Six tools mirror the REST endpoints one-to-one:
//
//	list_goals             GET    /api/sites/{id}/goals
//	create_goal            POST   /api/sites/{id}/goals
//	update_goal            PATCH  /api/sites/{id}/goals/{goalID}
//	delete_goal            DELETE /api/sites/{id}/goals/{goalID}
//	get_goals_analytics    GET    /api/sites/{id}/analytics/goals?since=...
//	track_event            POST   /t/event (server-side equivalent of
//	                       window.atomic.track on the rendered site)
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bright-interaction/slab/internal/conversions"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"
)

func (s *Server) registerGoalTools() {
	register := func(t Tool) { s.tools[t.Name] = t }

	register(Tool{
		Name:        "list_goals",
		Description: "Lists every conversion goal for this site (active + inactive). Each goal carries id, slug, name, match_type (url_pattern | event_name | form_submit), match_value, value_cents, value_currency, active. Use this to see what currently counts as a conversion before adding new goals or asking for goal-rate aggregates via get_goals_analytics.",
		InputSchema: schema(`{"type":"object","properties":{}}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, _ json.RawMessage) (string, error) {
			rows, err := s.queries.ListAllGoalsBySite(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			out := make([]map[string]any, 0, len(rows))
			for _, g := range rows {
				out = append(out, goalToMap(g))
			}
			return mustJSON(map[string]any{"goals": out}), nil
		},
	})

	register(Tool{
		Name:        "create_goal",
		Description: "Creates a conversion goal. match_type=url_pattern with match_value='/thank-you/*' counts every pageview matching the glob (* does not cross /). match_type=event_name with match_value='signup' counts every window.atomic.track('signup') call (or track_event MCP call). match_type=form_submit with match_value=<form_id> fires when that specific form is submitted. value_cents is optional intent-value (use 4900 for a EUR 49 lead). slug must be lowercase letters/digits/underscore/hyphen, unique per site.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"slug":{"type":"string","description":"stable identifier, [a-z0-9_-], unique per site"},
				"name":{"type":"string","description":"human label shown in the Analytics tab"},
				"match_type":{"type":"string","enum":["url_pattern","event_name","form_submit"]},
				"match_value":{"type":"string","description":"glob for url_pattern, exact string for event_name, form_id for form_submit"},
				"value_cents":{"type":"number","description":"optional monetary value in minor currency units"},
				"value_currency":{"type":"string","description":"3-letter code, defaults to EUR"},
				"active":{"type":"boolean","description":"defaults to true"}
			},
			"required":["slug","name","match_type","match_value"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				Slug          string `json:"slug"`
				Name          string `json:"name"`
				MatchType     string `json:"match_type"`
				MatchValue    string `json:"match_value"`
				ValueCents    int64  `json:"value_cents"`
				ValueCurrency string `json:"value_currency"`
				Active        *bool  `json:"active"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			args.Slug = strings.TrimSpace(args.Slug)
			args.Name = strings.TrimSpace(args.Name)
			args.MatchValue = strings.TrimSpace(args.MatchValue)
			if args.Slug == "" || args.Name == "" || args.MatchValue == "" {
				return "", errors.New("slug, name, and match_value are required")
			}
			if !conversions.IsValidMatchType(args.MatchType) {
				return "", errors.New("match_type must be url_pattern, event_name, or form_submit")
			}
			if args.ValueCents < 0 {
				return "", errors.New("value_cents must be non-negative")
			}
			if args.ValueCurrency != "" && len(args.ValueCurrency) != 3 {
				return "", errors.New("value_currency must be a 3-letter code")
			}
			currency := args.ValueCurrency
			if currency == "" {
				currency = "EUR"
			}
			active := int64(1)
			if args.Active != nil && !*args.Active {
				active = 0
			}
			id := newID()
			if err := s.queries.CreateGoal(ctx, store.CreateGoalParams{
				ID:            id,
				SiteID:        agent.SiteID,
				Slug:          args.Slug,
				Name:          args.Name,
				MatchType:     args.MatchType,
				MatchValue:    args.MatchValue,
				ValueCents:    args.ValueCents,
				ValueCurrency: currency,
				Active:        active,
			}); err != nil {
				return "", fmt.Errorf("create goal failed (slug taken?): %w", err)
			}
			row, err := s.queries.GetGoalByID(ctx, id)
			if err != nil {
				return "", err
			}
			return mustJSON(goalToMap(row)), nil
		},
	})

	register(Tool{
		Name:        "update_goal",
		Description: "Patches a goal's name, match_type, match_value, value_cents, value_currency, or active flag. slug is immutable. Pass only the fields you want to change; omitted fields keep their current value (active is a tri-state via the active field on the request: true/false/omitted).",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"id":{"type":"string","description":"goal id from list_goals"},
				"name":{"type":"string"},
				"match_type":{"type":"string","enum":["url_pattern","event_name","form_submit"]},
				"match_value":{"type":"string"},
				"value_cents":{"type":"number"},
				"value_currency":{"type":"string"},
				"active":{"type":"boolean"}
			},
			"required":["id"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				ID            string  `json:"id"`
				Name          *string `json:"name"`
				MatchType     *string `json:"match_type"`
				MatchValue    *string `json:"match_value"`
				ValueCents    *int64  `json:"value_cents"`
				ValueCurrency *string `json:"value_currency"`
				Active        *bool   `json:"active"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			row, err := s.queries.GetGoalByID(ctx, args.ID)
			if err != nil || row.SiteID != agent.SiteID {
				return "", errors.New("goal not found")
			}
			name := row.Name
			if args.Name != nil {
				if strings.TrimSpace(*args.Name) == "" {
					return "", errors.New("name cannot be empty")
				}
				name = strings.TrimSpace(*args.Name)
			}
			matchType := row.MatchType
			if args.MatchType != nil {
				if !conversions.IsValidMatchType(*args.MatchType) {
					return "", errors.New("match_type must be url_pattern, event_name, or form_submit")
				}
				matchType = *args.MatchType
			}
			matchValue := row.MatchValue
			if args.MatchValue != nil {
				if strings.TrimSpace(*args.MatchValue) == "" {
					return "", errors.New("match_value cannot be empty")
				}
				matchValue = strings.TrimSpace(*args.MatchValue)
			}
			valueCents := row.ValueCents
			if args.ValueCents != nil {
				if *args.ValueCents < 0 {
					return "", errors.New("value_cents must be non-negative")
				}
				valueCents = *args.ValueCents
			}
			currency := row.ValueCurrency
			if args.ValueCurrency != nil {
				if len(*args.ValueCurrency) != 3 {
					return "", errors.New("value_currency must be a 3-letter code")
				}
				currency = *args.ValueCurrency
			}
			active := row.Active
			if args.Active != nil {
				if *args.Active {
					active = 1
				} else {
					active = 0
				}
			}
			if err := s.queries.UpdateGoal(ctx, store.UpdateGoalParams{
				ID:            args.ID,
				Name:          name,
				MatchType:     matchType,
				MatchValue:    matchValue,
				ValueCents:    valueCents,
				ValueCurrency: currency,
				Active:        active,
			}); err != nil {
				return "", err
			}
			updated, _ := s.queries.GetGoalByID(ctx, args.ID)
			return mustJSON(goalToMap(updated)), nil
		},
	})

	register(Tool{
		Name:          "delete_goal",
		Description:   "Removes a goal. Cascades to conversion_events: every recorded conversion for this goal is dropped along with the definition. Use with care -- there's no undo. The site's pageview / event stream stays intact.",
		InputSchema:   schema(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			row, err := s.queries.GetGoalByID(ctx, args.ID)
			if err != nil || row.SiteID != agent.SiteID {
				return "", errors.New("goal not found")
			}
			if err := s.queries.DeleteGoal(ctx, args.ID); err != nil {
				return "", err
			}
			return mustJSON(map[string]any{"ok": true}), nil
		},
	})

	register(Tool{
		Name:        "get_goals_analytics",
		Description: "Returns per-goal counts + rates over a since-range (1d, 7d, 30d, 90d, all; default 7d). For each goal: conversions (total events), unique_converters (distinct fingerprints), conversion_rate_pct (unique_converters / unique_visitors over the same range), total_value_cents. Plus the site's unique_visitors count for context. Use this to answer 'is this campaign / page actually converting?'",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"since":{"type":"string","enum":["1d","7d","30d","90d","all"]}
			}
		}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				Since string `json:"since"`
			}
			_ = json.Unmarshal(raw, &args)
			since := goalsSinceDuration(args.Since)
			cutoff := time.Now().UTC().Add(-since).Format(time.RFC3339)

			goals, err := s.queries.ListAllGoalsBySite(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			uniqueVisitors, err := s.queries.CountUniqueVisitorsSince(ctx, store.CountUniqueVisitorsSinceParams{
				SiteID: agent.SiteID,
				Ts:     cutoff,
			})
			if err != nil {
				uniqueVisitors = 0
			}
			out := make([]map[string]any, 0, len(goals))
			for _, g := range goals {
				conv, _ := s.queries.CountConversionsByGoal(ctx, store.CountConversionsByGoalParams{
					GoalID: g.ID,
					Ts:     cutoff,
				})
				uniq, _ := s.queries.CountUniqueConvertersByGoal(ctx, store.CountUniqueConvertersByGoalParams{
					GoalID: g.ID,
					Ts:     cutoff,
				})
				val, _ := s.queries.SumValueByGoal(ctx, store.SumValueByGoalParams{
					GoalID: g.ID,
					Ts:     cutoff,
				})
				valCents := int64(0)
				if v, ok := val.(int64); ok {
					valCents = v
				}
				rate := 0.0
				if uniqueVisitors > 0 {
					rate = float64(uniq) / float64(uniqueVisitors) * 100.0
				}
				row := goalToMap(g)
				row["conversions"] = conv
				row["unique_converters"] = uniq
				row["conversion_rate_pct"] = rate
				row["total_value_cents"] = valCents
				out = append(out, row)
			}
			return mustJSON(map[string]any{
				"since":           since.String(),
				"unique_visitors": uniqueVisitors,
				"goals":           out,
			}), nil
		},
	})

	register(Tool{
		Name:        "track_event",
		Description: "Server-side equivalent of window.atomic.track(name, props) on a rendered site. Records a conversion_event for any active event_name goal whose match_value equals name. Useful when an agent wants to fire a conversion from a back-channel (a webhook, a Slack approval flow) rather than the visitor's browser. fingerprint is required because the conversion is keyed to a visitor (use the same atomicsite_fp cookie value the rendered site uses); empty fingerprint returns an error rather than silently dropping.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"name":{"type":"string","description":"event name to match against active event_name goals"},
				"fingerprint":{"type":"string","description":"16-hex-char visitor fingerprint from atomicsite_fp cookie"},
				"path":{"type":"string","description":"optional URL path to record alongside the event"},
				"value_cents":{"type":"number","description":"optional event-time value override (otherwise the matched goal's value_cents wins)"},
				"properties":{"type":"object","description":"optional event properties stored as JSON on the conversion_events row"}
			},
			"required":["name","fingerprint"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			var args struct {
				Name        string         `json:"name"`
				Fingerprint string         `json:"fingerprint"`
				Path        string         `json:"path"`
				ValueCents  int64          `json:"value_cents"`
				Properties  map[string]any `json:"properties"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			if strings.TrimSpace(args.Name) == "" || strings.TrimSpace(args.Fingerprint) == "" {
				return "", errors.New("name and fingerprint are required")
			}
			rec := conversions.NewRecorder(s.queries)
			n := rec.Evaluate(ctx, conversions.Signal{
				SiteID:             agent.SiteID,
				Fingerprint:        args.Fingerprint,
				Path:               args.Path,
				MatchType:          conversions.MatchTypeEventName,
				MatchValue:         args.Name,
				ValueOverrideCents: args.ValueCents,
				Properties:         args.Properties,
			})
			return mustJSON(map[string]any{"recorded": n}), nil
		},
	})
}

// goalToMap turns a sqlc ConversionGoal row into the agent-friendly
// JSON shape. Keep the keys aligned with handlers.goalView so admin
// REST + MCP responses speak the same vocabulary.
func goalToMap(g store.ConversionGoal) map[string]any {
	return map[string]any{
		"id":             g.ID,
		"site_id":        g.SiteID,
		"slug":           g.Slug,
		"name":           g.Name,
		"match_type":     g.MatchType,
		"match_value":    g.MatchValue,
		"value_cents":    g.ValueCents,
		"value_currency": g.ValueCurrency,
		"active":         g.Active != 0,
		"created_at":     g.CreatedAt,
		"updated_at":     g.UpdatedAt,
	}
}

// goalsSinceDuration mirrors handlers.parseSinceRange so MCP +
// REST agree on what "30d" means. all maps to a 5-year window which
// is effectively "everything".
func goalsSinceDuration(s string) time.Duration {
	switch s {
	case "1d":
		return 24 * time.Hour
	case "30d":
		return 30 * 24 * time.Hour
	case "90d":
		return 90 * 24 * time.Hour
	case "all":
		return 5 * 365 * 24 * time.Hour
	default:
		return 7 * 24 * time.Hour
	}
}
