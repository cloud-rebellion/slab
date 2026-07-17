package apps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Sprint 4 Slice B (2026-05-24): upstream MCP client. The use_app
// MCP tool dials a publisher-operated MCP endpoint via JSON-RPC POST,
// carrying the install's stored credentials in the Authorization
// header (primary bearer) plus X-Atomic-Cred-<key> extras for any
// non-bearer credential fields (Algolia application_id, etc).
//
// Slice A shipped marketplace + install ledger; Slice B closes the
// loop so an agent connected to atomicsite can fan out to every
// installed app's tool surface under one namespace.

const (
	defaultUpstreamTimeout   = 30 * time.Second
	maxUpstreamResponseBytes = 256 * 1024
)

// UpstreamCall is one tools/call dispatch to an upstream MCP server.
type UpstreamCall struct {
	URL         string
	ToolName    string
	Arguments   map[string]any
	Credentials map[string]string
	Schema      []CredentialField
	// HTTPClient lets tests inject a mock transport. Nil means
	// http.DefaultClient.
	HTTPClient *http.Client
}

// UpstreamResult is the JSON-RPC outcome of one upstream dispatch.
// Either ResultJSON or ErrorMessage is populated; never both.
type UpstreamResult struct {
	HTTPStatus   int             `json:"http_status"`
	ResultJSON   json.RawMessage `json:"result,omitempty"`
	ErrorMessage string          `json:"error_message,omitempty"`
	ErrorCode    int             `json:"error_code,omitempty"`
}

// CallUpstreamMCP sends a JSON-RPC tools/call envelope to the upstream
// MCP server URL described by call.URL. Returns an UpstreamResult that
// distinguishes transport failures (returned as Go errors) from
// upstream-level errors (returned in UpstreamResult.ErrorMessage with
// HTTPStatus / ErrorCode set).
func CallUpstreamMCP(ctx context.Context, call UpstreamCall) (*UpstreamResult, error) {
	if call.URL == "" {
		return nil, errors.New("upstream MCP URL is empty")
	}
	if call.ToolName == "" {
		return nil, errors.New("upstream tool name is empty")
	}
	args := call.Arguments
	if args == nil {
		args = map[string]any{}
	}
	envelope := map[string]any{
		"jsonrpc": "2.0",
		"id":      "atomic-use-app",
		"method":  "tools/call",
		"params": map[string]any{
			"name":      call.ToolName,
			"arguments": args,
		},
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("marshal upstream envelope: %w", err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, defaultUpstreamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, call.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "atomicsite-use-app/1.0")
	bearer := PrimaryBearer(call.Schema, call.Credentials)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for _, f := range call.Schema {
		v, ok := call.Credentials[f.Key]
		if !ok {
			continue
		}
		trimmed := strings.TrimSpace(v)
		if trimmed == "" || trimmed == bearer {
			continue
		}
		req.Header.Set("X-Atomic-Cred-"+normalizeHeaderKey(f.Key), trimmed)
	}
	client := call.HTTPClient
	if client == nil {
		// A legitimate MCP tools/call endpoint has no reason to 302, and Go copies
		// custom headers (the X-Atomic-Cred-* secrets set above) to a cross-host
		// redirect target. Forbid redirects so a compromised/redirecting upstream
		// cannot exfiltrate those credentials or steer the request to an internal IP.
		client = &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream MCP dial failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read upstream response: %w", err)
	}
	out := &UpstreamResult{HTTPStatus: resp.StatusCode}
	if len(raw) > maxUpstreamResponseBytes {
		out.ErrorMessage = fmt.Sprintf("upstream response exceeded %d bytes; refused", maxUpstreamResponseBytes)
		return out, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		out.ErrorMessage = fmt.Sprintf("upstream MCP returned HTTP %d: %s", resp.StatusCode, truncateString(string(raw), 512))
		return out, nil
	}
	if len(raw) == 0 {
		out.ErrorMessage = "upstream returned empty body"
		return out, nil
	}
	var rpc struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      any             `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    any    `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &rpc); err != nil {
		out.ErrorMessage = "upstream did not return valid JSON-RPC: " + err.Error()
		return out, nil
	}
	if rpc.Error != nil {
		out.ErrorCode = rpc.Error.Code
		out.ErrorMessage = rpc.Error.Message
		return out, nil
	}
	out.ResultJSON = rpc.Result
	return out, nil
}

// PrimaryBearer picks the bearer-token credential. Heuristic: prefer
// well-known secret field names in this order:
//
//	api_key, secret_key, private_app_token, bot_token, admin_api_key,
//	access_token, token
//
// Falls back to the first kind=secret schema field with a value.
// Returns empty string when no bearer-shaped credential is set; the
// caller proceeds without an Authorization header (some MCP servers
// authenticate via custom headers only).
func PrimaryBearer(schema []CredentialField, creds map[string]string) string {
	preferred := []string{
		"api_key", "secret_key", "private_app_token",
		"bot_token", "admin_api_key", "access_token", "token",
	}
	for _, key := range preferred {
		if v, ok := creds[key]; ok {
			trimmed := strings.TrimSpace(v)
			if trimmed != "" {
				return trimmed
			}
		}
	}
	for _, f := range schema {
		if f.Kind != "secret" {
			continue
		}
		if v, ok := creds[f.Key]; ok {
			trimmed := strings.TrimSpace(v)
			if trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func normalizeHeaderKey(key string) string {
	return strings.ReplaceAll(key, "_", "-")
}

func truncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
