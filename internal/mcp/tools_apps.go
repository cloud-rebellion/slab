package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	authmw "github.com/bright-interaction/slab/internal/middleware"
)

// registerAppsTools wires the Sprint 4 apps platform MCP surface.
// Three tools today:
//
//   - list_apps_marketplace (Slice A): cross-tenant catalogue
//   - list_installed_apps   (Slice A): per-site installed list
//   - use_app               (Slice B): JSON-RPC proxy that dials
//     the publisher's MCP endpoint with the stored credentials
//     and forwards a tools/call envelope.
//
// list_* tools are RequiresWrite=false (read-only). use_app is
// RequiresWrite=true because the upstream tool call might mutate
// state on the publisher's side; the capability gate is
// pessimistic by design.
func (s *Server) registerAppsTools() {
	register := func(t Tool) { s.tools[t.Name] = t }

	register(Tool{
		Name:        "list_apps_marketplace",
		Description: "Lists every active app in the cross-tenant Atomicsite Apps marketplace. Returns app_id, slug, name, description, category, publisher, version, icon_url, docs_url, plus the credential field schema each app needs at install. Use this to discover what third-party integrations are available before recommending an install to the human. Read-only: install happens via the admin UI in slice A; slice B will gate agent-driven installs behind a per-site flag.",
		InputSchema: schema(`{"type":"object","properties":{}}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			if s.apps == nil {
				return "", errors.New("list_apps_marketplace not configured on this server (apps handler unset)")
			}
			out, err := s.apps.ListAgentMarketplace(ctx)
			if err != nil {
				return "", err
			}
			return mustJSON(map[string]any{"apps": out, "count": len(out)}), nil
		},
	})

	register(Tool{
		Name:        "list_installed_apps",
		Description: "Lists the apps installed on this site. Returns app_id, slug, name, category, status (active|revoked), credentials_set (the FIELD KEYS that have a value stored; values themselves are never exposed via MCP), and last_used_at. Use this to plan multi-step flows: e.g. 'I see Stripe is installed with secret_key set, so I can create a checkout link via the use_app proxy'.",
		InputSchema: schema(`{"type":"object","properties":{}}`),
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			if s.apps == nil {
				return "", errors.New("list_installed_apps not configured on this server (apps handler unset)")
			}
			out, err := s.apps.ListAgentInstalls(ctx, agent.SiteID)
			if err != nil {
				return "", err
			}
			return mustJSON(map[string]any{"installs": out, "count": len(out)}), nil
		},
	})

	register(Tool{
		Name:        "use_app",
		Description: "Proxies one JSON-RPC tools/call to an installed app's upstream MCP server using the stored install credentials. Required: app_slug (e.g. 'stripe', 'calcom', 'brevo'), tool (the upstream tool name; call list_installed_apps + the publisher's docs to discover the surface). Optional: arguments (JSON object passed verbatim to the upstream tool). Atomicsite injects the primary secret as a Bearer token and any extra credentials as X-Atomic-Cred-<key> headers; the agent never sees the values. Returns the upstream JSON-RPC result on success or an error envelope (with http_status / error_code / error_message) when the publisher refuses or fails. Updates last_used_at on every dispatch, including upstream errors. Refuses when the app is not installed, not active, or not in the marketplace. Upstream timeout is 30 seconds; responses larger than 256 KB are rejected.",
		InputSchema: schema(`{
			"type":"object",
			"properties":{
				"app_slug":{"type":"string","description":"Marketplace slug of an installed app (e.g. stripe, calcom, brevo)."},
				"tool":{"type":"string","description":"Upstream MCP tool name to invoke."},
				"arguments":{"type":"object","description":"Arguments object forwarded verbatim to the upstream tool."}
			},
			"required":["app_slug","tool"]
		}`),
		RequiresWrite: true,
		Handler: func(ctx context.Context, agent *authmw.AgentIdentity, raw json.RawMessage) (string, error) {
			if s.apps == nil {
				return "", errors.New("use_app not configured on this server (apps handler unset)")
			}
			var in struct {
				AppSlug   string         `json:"app_slug"`
				Tool      string         `json:"tool"`
				Arguments map[string]any `json:"arguments"`
			}
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &in); err != nil {
					return "", fmt.Errorf("invalid arguments JSON: %w", err)
				}
			}
			if in.AppSlug == "" {
				return "", errors.New("app_slug is required")
			}
			if in.Tool == "" {
				return "", errors.New("tool is required")
			}
			res, err := s.apps.CallAppForAgent(ctx, agent.SiteID, in.AppSlug, in.Tool, in.Arguments)
			if err != nil {
				return "", err
			}
			return mustJSON(map[string]any{
				"app_slug":      in.AppSlug,
				"tool":          in.Tool,
				"http_status":   res.HTTPStatus,
				"result":        res.ResultJSON,
				"error_code":    res.ErrorCode,
				"error_message": res.ErrorMessage,
			}), nil
		},
	})
}
