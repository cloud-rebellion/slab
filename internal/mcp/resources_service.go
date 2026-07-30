package mcp

import (
	"context"
	"database/sql"
	"sort"
	"time"

	agentpkg "github.com/bright-interaction/slab/internal/agent"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"
)

// registerServiceContextResources adds the surface that lets an agent
// reason about the WHOLE site state, not just its pages and blocks.
// Every resource here is a thin pass-through to existing sqlc queries
// or in-memory snapshots. Visitor-PII boundaries are preserved:
// consent/stats returns aggregates only; identified-tier records are
// never exposed.
func (s *Server) registerServiceContextResources() {
	register := func(r Resource) { s.resources[r.URI] = r }

	register(Resource{
		URI:         "atomicsite://eval/latest",
		Name:        "Latest evaluation scores",
		Description: "Per-category eval scores from the most recent build (security, seo, accessibility, privacy, performance, geo). Returns empty list when no builds have run yet. Read this after trigger_build to see what to fix.",
		MimeType:    "application/json",
		Reader: func(ctx context.Context, agent *authmw.AgentIdentity) (string, error) {
			rows, err := s.queries.ListEvaluationsBySite(ctx, store.ListEvaluationsBySiteParams{
				SiteID: agent.SiteID,
				Limit:  20,
			})
			if err != nil {
				return "", err
			}
			latest := latestEvaluationsByCategory(rows)
			return mustJSON(map[string]any{
				"site_id":     agent.SiteID,
				"by_category": latest,
				"hint":        "Each entry has score/max_score/grade and checks_json with per-check detail. Categories: security, seo, accessibility, privacy, performance, geo.",
			}), nil
		},
	})

	register(Resource{
		URI:         "atomicsite://build/history",
		Name:        "Recent builds",
		Description: "Last 10 deployments for this site: id, status, duration_ms, error, deploy_url, target. Useful for diagnosing flaky builds and avoiding repeating failed deployments.",
		MimeType:    "application/json",
		Reader: func(ctx context.Context, agent *authmw.AgentIdentity) (string, error) {
			rows, err := s.queries.ListRecentDeploymentsBySite(ctx, store.ListRecentDeploymentsBySiteParams{
				SiteID: agent.SiteID,
				Limit:  10,
			})
			if err != nil {
				return "", err
			}
			return mustJSON(map[string]any{"deployments": rows}), nil
		},
	})

	register(Resource{
		URI:         "atomicsite://deploy/status",
		Name:        "Current deploy status",
		Description: "Most recent deployment record: status (building/success/failed), deploy_url, target. Read before calling screenshot to confirm the build succeeded and a URL is live.",
		MimeType:    "application/json",
		Reader: func(ctx context.Context, agent *authmw.AgentIdentity) (string, error) {
			rows, err := s.queries.ListRecentDeploymentsBySite(ctx, store.ListRecentDeploymentsBySiteParams{
				SiteID: agent.SiteID,
				Limit:  1,
			})
			if err != nil {
				return "", err
			}
			if len(rows) == 0 {
				return mustJSON(map[string]any{
					"status": "no_builds_yet",
					"hint":   "Call the trigger_build tool to start the first deployment.",
				}), nil
			}
			d := rows[0]
			return mustJSON(map[string]any{
				"status":        d.Status,
				"deploy_url":    d.DeployUrl,
				"deploy_target": d.DeployTarget,
				"duration_ms":   d.DurationMs,
				"pages_built":   d.PagesBuilt,
				"error":         d.Error,
				"deployed_at":   d.DeployedAt,
				"created_at":    d.CreatedAt,
				"completed_at":  d.CompletedAt,
			}), nil
		},
	})

	register(Resource{
		URI:         "atomicsite://domains",
		Name:        "Custom domains",
		Description: "Every custom hostname on this site, with reconciler state (pending/verified/cert_ready/live/error), canonical flag, last error. The agent uses this to confirm the live URL before reporting completion.",
		MimeType:    "application/json",
		Reader: func(ctx context.Context, agent *authmw.AgentIdentity) (string, error) {
			rows, err := s.queries.ListDomainsBySite(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			return mustJSON(map[string]any{"domains": rows}), nil
		},
	})

	register(Resource{
		URI:         "atomicsite://members",
		Name:        "Site members",
		Description: "Workspace users with access to this site: user_id, role (owner/admin/member), email, name. Useful when the agent needs to confirm collaboration shape before destructive operations.",
		MimeType:    "application/json",
		Reader: func(ctx context.Context, agent *authmw.AgentIdentity) (string, error) {
			rows, err := s.queries.ListSiteMembers(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			// Project rather than emitting the row verbatim. The raw column is
			// `name`, and the shield's key-aware pass keys on a fixed map that
			// deliberately excludes bare "name" (or every product and page name
			// in the estate would be tokenized). There is also no name regex in
			// the bank, because a name is not pattern-matchable. So the raw row
			// sent every collaborator's real name to the model in the clear,
			// while the sibling order resource tokenized the same value under
			// customer_name. member_name/member_email are in piiFieldKinds.
			out := make([]map[string]any, 0, len(rows))
			for _, r := range rows {
				out = append(out, map[string]any{
					"user_id":      r.UserID,
					"role":         r.Role,
					"created_at":   r.CreatedAt,
					"member_name":  r.Name,
					"member_email": r.Email,
				})
			}
			return mustJSON(map[string]any{"members": out}), nil
		},
	})

	register(Resource{
		URI:         "atomicsite://knowledgebase",
		Name:        "Site knowledgebase",
		Description: "Per-site KB entries: brand voice, product positioning, FAQ source, anything operators want the agent to remember when authoring. Use this before writing copy so the voice matches.",
		MimeType:    "application/json",
		Reader: func(ctx context.Context, agent *authmw.AgentIdentity) (string, error) {
			rows, err := s.queries.ListKnowledgebaseBySite(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			return mustJSON(map[string]any{"entries": rows}), nil
		},
	})

	register(Resource{
		URI:         "atomicsite://consent/stats",
		Name:        "Consent aggregates (no individual records)",
		Description: "Aggregate consent stats over the last 30 days: total banners shown, accept-all rate, reject-all rate, custom-categories rate, GPC signals received, do-not-sell-count. Aggregates only; individual consent records are never exposed via MCP.",
		MimeType:    "application/json",
		Reader: func(ctx context.Context, agent *authmw.AgentIdentity) (string, error) {
			now := time.Now().UTC().Unix()
			thirtyDaysAgo := now - 30*24*3600
			row, err := s.queries.ConsentStatsBySite(ctx, store.ConsentStatsBySiteParams{
				SiteID:      agent.SiteID,
				CreatedAt:   thirtyDaysAgo,
				CreatedAt_2: now,
			})
			if err != nil {
				return "", err
			}
			return mustJSON(map[string]any{
				"window_days": 30,
				"total":       row.Total,
				"accepts":     nullToFloat(row.Accepts),
				"rejects":     nullToFloat(row.Rejects),
				"customs":     nullToFloat(row.Customs),
				"gpc_signals": nullToFloat(row.Gpcs),
				"dns_signals": nullToFloat(row.DnsCount),
				"do_not_sell": nullToFloat(row.DoNotSellCount),
				"hint":        "Aggregates only. Individual consent rows are not exposed via MCP per privacy policy.",
			}), nil
		},
	})

	register(Resource{
		URI:         "atomicsite://retention/status",
		Name:        "Retention manager status",
		Description: "Configuration of the GDPR retention sweep: configured retention days for analytics/consent/engagement, plus an instruction on where to read the actual sweep result (admin /api/admin/metrics).",
		MimeType:    "application/json",
		Reader: func(ctx context.Context, agent *authmw.AgentIdentity) (string, error) {
			settings, err := s.queries.ListSettingsBySite(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			retention := map[string]string{}
			for _, st := range settings {
				if st.Category != "general" {
					continue
				}
				switch st.Key {
				case "analytics_retention_days", "consent_retention_days", "engagement_retention_days":
					retention[st.Key] = st.Value
				}
			}
			return mustJSON(map[string]any{
				"site_id":               agent.SiteID,
				"retention_settings":    retention,
				"sweep_status_endpoint": "/api/admin/metrics",
				"hint":                  "Sweep run timestamps + rows reaped live in the admin metrics endpoint, not exposed via MCP yet.",
			}), nil
		},
	})

	register(Resource{
		URI:         "atomicsite://integrations",
		Name:        "Wired integrations",
		Description: "Snapshot of which third-party services are configured for this site: GA4, Umami, CRM webhook, Figma (via design refs or token import), GitHub design references count, deploy targets count.",
		MimeType:    "application/json",
		Reader: func(ctx context.Context, agent *authmw.AgentIdentity) (string, error) {
			settings, err := s.queries.ListSettingsBySite(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			has := func(key string) bool {
				for _, st := range settings {
					if st.Category == "analytics" && st.Key == key && st.Value != "" && st.Value != "0" {
						return true
					}
				}
				return false
			}
			refs, _ := s.queries.ListDesignReferences(ctx, agent.SiteID)
			targets, _ := s.queries.ListDeployTargetsBySite(ctx, agent.SiteID)
			return mustJSON(map[string]any{
				"ga4":               map[string]any{"enabled": has("ga4_enabled")},
				"umami":             map[string]any{"enabled": has("umami_enabled")},
				"cookieproof":       map[string]any{"enabled": has("cookieproof_enabled")},
				"personalization":   map[string]any{"enabled": has("personalization_enabled")},
				"crm_webhook":       map[string]any{"configured": settingValue(settings, "analytics", "crm_webhook_url") != ""},
				"design_references": map[string]any{"count": len(refs)},
				"deploy_targets":    map[string]any{"count": len(targets)},
			}), nil
		},
	})

	register(Resource{
		URI:         "atomicsite://design-references",
		Name:        "Design references",
		Description: "External GitHub repos the operator registered as design vocabulary (taste-skill, high-end-visual-design, custom). Each ref carries a fetched bundle: README, tailwind config when present, sample components. Read this before composing pages so the agent can match the operator's preferred design language.",
		MimeType:    "application/json",
		Reader: func(ctx context.Context, agent *authmw.AgentIdentity) (string, error) {
			rows, err := s.queries.ListDesignReferences(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			return mustJSON(map[string]any{"references": rows}), nil
		},
	})

	// Phase 31.1.1 (2026-05-06). Read-only aggregate snapshot of every
	// active conversion goal with counts + rates over the last 7 days.
	// Companion to the get_goals_analytics tool: agents that prefer
	// resources/read get a quick "what counts as a conversion here, and
	// is it firing?" answer in one fetch. Aggregates only; identified-
	// tier emails are never exposed.
	register(Resource{
		URI:         "atomicsite://analytics/goals",
		Name:        "Conversion goals + rates (7-day)",
		Description: "Per-site conversion goals with conversions / unique_converters / conversion_rate_pct / total_value_cents over the last 7 days. Use this to learn what counts as a conversion before suggesting goal changes; use the get_goals_analytics tool when you need a different time range.",
		MimeType:    "application/json",
		Reader: func(ctx context.Context, agent *authmw.AgentIdentity) (string, error) {
			cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour).Format(time.RFC3339)
			goals, err := s.queries.ListAllGoalsBySite(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			uniqueVisitors, _ := s.queries.CountUniqueVisitorsSince(ctx, store.CountUniqueVisitorsSinceParams{
				SiteID: agent.SiteID,
				Ts:     cutoff,
			})
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
				"window":          "7d",
				"unique_visitors": uniqueVisitors,
				"goals":           out,
			}), nil
		},
	})

	register(Resource{
		URI:         "atomicsite://meta/capabilities",
		Name:        "Agent capabilities",
		Description: "Self-describing snapshot the agent can read to know what it can do right now: identity (key id, site id, capability bits), tool/resource/prompt counts, write capability, eval thresholds. Read this once on session start to plan what is in reach.",
		MimeType:    "application/json",
		Reader: func(ctx context.Context, agent *authmw.AgentIdentity) (string, error) {
			return mustJSON(s.capabilitiesSnapshot(agent)), nil
		},
	})

	register(Resource{
		URI:         "atomicsite://meta/design-playbook",
		Name:        "Design playbook (rubric the Inspector grades against)",
		Description: "THE site's design DNA, adapted to its design.fidelity dial (performance / balanced / showcase): the fidelity contract (what is unlocked, how grading changes, what is never relaxed), principles, page archetypes, anti-patterns (AI tells), vibe archetypes, materiality, content authenticity (banned slop terms), motion budget, copy voice, font system, hero graphics catalog, one-shot recipe. The Inspector grades against THIS rubric, so read it BEFORE authoring blocks. Re-read after changing design.fidelity.",
		MimeType:    "application/json",
		Reader: func(ctx context.Context, agent *authmw.AgentIdentity) (string, error) {
			fidelity := agentpkg.FidelityForSite(ctx, s.queries, agent.SiteID)
			return mustJSON(agentpkg.DesignPlaybookFor(fidelity)), nil
		},
	})
}

// capabilitiesSnapshot is the payload behind atomicsite://meta/capabilities
// AND the get_capabilities tool. Same shape so clients can pick the
// surface they prefer (resources/read or tools/call).
func (s *Server) capabilitiesSnapshot(agent *authmw.AgentIdentity) map[string]any {
	toolNames := make([]string, 0, len(s.tools))
	writeTools := 0
	for name, t := range s.tools {
		toolNames = append(toolNames, name)
		if t.RequiresWrite {
			writeTools++
		}
	}
	sort.Strings(toolNames)

	resourceURIs := make([]string, 0, len(s.resources))
	for uri := range s.resources {
		resourceURIs = append(resourceURIs, uri)
	}
	sort.Strings(resourceURIs)

	promptNames := make([]string, 0, len(s.prompts))
	for name := range s.prompts {
		promptNames = append(promptNames, name)
	}
	sort.Strings(promptNames)

	hasWrite := false
	for _, c := range agent.Capabilities {
		if c == "write" {
			hasWrite = true
			break
		}
	}

	return map[string]any{
		"identity": map[string]any{
			"key_id":       agent.KeyID,
			"site_id":      agent.SiteID,
			"capabilities": agent.Capabilities,
			"can_write":    hasWrite,
		},
		"server": map[string]any{
			"name":             "atomicsite",
			"version":          "1.0.0",
			"protocol_version": Protocol,
		},
		"surface": map[string]any{
			"tools":             toolNames,
			"tools_count":       len(toolNames),
			"write_tools_count": writeTools,
			"resources":         resourceURIs,
			"resources_count":   len(resourceURIs),
			"prompts":           promptNames,
			"prompts_count":     len(promptNames),
		},
		"eval_thresholds": map[string]any{
			"a_plus": ">=95%",
			"a":      "90-95%",
			"b":      "80-90%",
			"hint":   "Grade targets follow the site's design.fidelity dial (settings category=design key=fidelity). performance fidelity: A+ everywhere. balanced (default): A or higher across security/seo/accessibility/privacy/performance. showcase: security/a11y/privacy >= A-, seo >= B, performance >= C+ against showcase budgets. Read design_playbook.fidelity for the full contract; every evaluation row records the fidelity it was graded under.",
		},
		"privacy_invariants": []string{
			"No tool reads visitor metadata, sessions, fingerprints, or identified-tier records.",
			"consent/stats returns aggregates only.",
			"raw_astro and security-category writes are admin-only.",
		},
	}
}

// latestEvaluationsByCategory returns the freshest row per category from a
// site-wide eval list (already ordered DESC). The eval list spans many
// builds; the agent typically wants only the most recent score per
// category.
func latestEvaluationsByCategory(rows []store.Evaluation) map[string]store.Evaluation {
	out := map[string]store.Evaluation{}
	for _, r := range rows {
		if _, seen := out[r.Category]; !seen {
			out[r.Category] = r
		}
	}
	return out
}

// nullToFloat unwraps a sql.NullFloat64 for the consent aggregate
// payload. JSON-encoding NullFloat64 directly emits an awkward
// {Float64,Valid} pair; we want plain numbers (or zero when null).
func nullToFloat(n sql.NullFloat64) float64 {
	if !n.Valid {
		return 0
	}
	return n.Float64
}

// settingValue scans a settings list for one (category, key) value.
// Empty string when absent.
func settingValue(rows []store.SiteSetting, category, key string) string {
	for _, r := range rows {
		if r.Category == category && r.Key == key {
			return r.Value
		}
	}
	return ""
}
