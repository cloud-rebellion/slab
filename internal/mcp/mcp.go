// Package mcp implements a Model Context Protocol server for atomicsite.
//
// MCP is a JSON-RPC 2.0 dialect that lets AI hosts (Claude Desktop, Cursor,
// Cline, Windsurf, n8n's MCP node, future Claude Code remote-MCP support)
// discover and call capabilities from external "MCP servers." The protocol
// defines three primitive types we expose:
//
//   - tools     callable functions (verb-shaped: create_page, update_block)
//   - resources read-only data accessible by URI (atomicsite://site/context)
//   - prompts   reusable templates the user picks from a dropdown
//
// Architecture: the MCP server is a thin protocol translator. tools/call →
// existing store.Queries (same logic the REST handlers run); resources/read
// → same handlers' GET path. No business logic is duplicated.
//
// Privacy posture (the key design decision): the server is allow-list
// driven. Adding a new endpoint to /api/agent/* does NOT auto-expose it
// via MCP. To expose a tool, someone must explicitly add it to the
// registry in tools.go. This means future PII-touching endpoints
// (visitor metadata, raw consent records, identified-tier data) cannot
// accidentally leak. The user's stated rule is: "the agent can SET UP
// analytics but never TOUCH PII the analytics produces." We honour that
// by exposing settings-write tools (gating analytics-category writes
// through the same validateSetting() the REST path uses) while NEVER
// exposing visitor-data reads.
//
// Auth: same X-Agent-Key as REST. The /mcp route mounts behind the
// existing AgentAuthMiddleware, so every request arrives with a resolved
// AgentIdentity (KeyID, SiteID, Capabilities). Tools that mutate require
// the "write" capability; tools that read accept any active key.
//
// Transport: Streamable HTTP (2025-03-26 spec). Single endpoint accepting
// POST for client→server messages. Response is JSON for non-streamed
// methods. SSE upgrade is supported for long-running tool calls but not
// strictly required by the spec; this implementation answers everything
// inline today and can grow into SSE without breaking existing clients.
package mcp

import (
	"context"
	"encoding/json"
	"net/http"

	authmw "github.com/bright-interaction/slab/internal/middleware"
)

// Protocol is the version of MCP we speak.
//
// 2025-03-26 is the Streamable HTTP transport spec; we negotiate this in
// initialize. Older clients that send 2024-11-05 (the SSE-only spec) are
// answered with the same version they offer; the server stays
// backward-compatible because the handful of methods we use (initialize,
// tools/list, tools/call, resources/list, resources/read, prompts/list,
// ping) are stable across both versions.
const Protocol = "2025-03-26"

// JSONRPCVersion is fixed by the JSON-RPC 2.0 spec.
const JSONRPCVersion = "2.0"

// Request is one inbound JSON-RPC request. Notifications (Method set, ID
// absent) are decoded into the same shape; the server detects them by
// checking RawID == nil before sending a response.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is one outbound JSON-RPC response.
//
// Exactly one of Result and Error is set on a successful round trip. ID
// must mirror the request's ID per the spec (we treat any input shape as
// opaque pass-through to avoid number-vs-string surprises across clients).
type Response struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id,omitempty"`
	Result  any            `json:"result,omitempty"`
	Error   *ResponseError `json:"error,omitempty"`
}

// ResponseError is one JSON-RPC error envelope. Code values follow the
// JSON-RPC 2.0 reservations:
//
//	-32700 parse error    (malformed JSON)
//	-32600 invalid request (jsonrpc/method missing)
//	-32601 method not found
//	-32602 invalid params
//	-32603 internal error  (uncaught panic / DB failure)
//
// Application errors (validation rejections, missing pages, etc.) ride
// inside a successful result with isError=true on the tool content shape;
// they are NOT signalled via the JSON-RPC error envelope. This lets the
// agent see the human-readable error and decide what to do next without
// the host swallowing the message.
type ResponseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Standard JSON-RPC error codes.
const (
	ErrParse          = -32700
	ErrInvalidRequest = -32600
	ErrMethodNotFound = -32601
	ErrInvalidParams  = -32602
	ErrInternal       = -32603
)

// ServerInfo is what we send in the initialize response. The
// protocolVersion echo lets the client decide whether we're compatible
// with the version it offered.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Capabilities is what we advertise during initialize. Only the
// primitives we actually implement are listed; clients use this to
// decide which UI affordances to show.
type Capabilities struct {
	Tools     *struct{} `json:"tools,omitempty"`
	Resources *struct{} `json:"resources,omitempty"`
	Prompts   *struct{} `json:"prompts,omitempty"`
}

// InitializeResult is the body of an initialize response.
type InitializeResult struct {
	ProtocolVersion string       `json:"protocolVersion"`
	ServerInfo      ServerInfo   `json:"serverInfo"`
	Capabilities    Capabilities `json:"capabilities"`
	// Instructions are surfaced by some clients (Claude Desktop) to the
	// model as system context on first connect. Keep terse; the heavy
	// lifting belongs in the resources/prompts surface, not here.
	Instructions string `json:"instructions"`
}

// Content is the wire shape for tool call results + resource reads + prompt
// messages. The MCP spec defines a small set of content types:
//
//	"text"     literal string (most common; we use this for JSON-encoded payloads too)
//	"image"    base64 PNG/JPEG (out of scope for atomicsite today)
//	"resource" reference to a resource URI
//
// We standardise on "text" for tool results: the body is the JSON-encoded
// store row or list, which most hosts render as a code block in the chat.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// ToolCallResult is what tools/call returns to the client. IsError=true
// signals an application-level failure (e.g. validation rejected, page
// not found) without hiding it inside a JSON-RPC error.
type ToolCallResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

// ToolHandler is the function signature a registered tool implements.
// Returns the JSON-encoded result string OR an application error message;
// the dispatcher wraps either into a ToolCallResult.
type ToolHandler func(ctx context.Context, agent *authmw.AgentIdentity, args json.RawMessage) (string, error)

// Tool is one registered tool: schema + handler.
//
// InputSchema is a JSON Schema (draft-2020-12) that hosts surface to the
// model so it knows what arguments to send. We hand-write the schemas
// rather than reflect from Go structs because the request shapes are
// small (≤8 fields each) and the descriptions matter for model behaviour.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`

	// RequiresWrite gates the call on the "write" capability bit on the
	// agent key. Read-only tools (list, get, search) can be called by
	// any active key; mutate tools fail with a clean error when the key
	// is read-only.
	RequiresWrite bool `json:"-"`

	// RequiresCapability, when set, gates the call on an ADDITIONAL named
	// capability beyond "write". Used for security-sensitive surfaces that
	// are admin-only on the REST side (e.g. CSP allowlist mutation, which
	// widens XSS exposure): a plain write key must not reach them. Empty =
	// no extra capability required.
	RequiresCapability string `json:"-"`

	Handler ToolHandler `json:"-"`
}

// Resource is one registered read-only resource.
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description"`
	MimeType    string `json:"mimeType,omitempty"`

	// Reader produces the body of the resource. The body is a JSON-encoded
	// payload mirroring the corresponding GET endpoint response.
	Reader func(ctx context.Context, agent *authmw.AgentIdentity) (string, error) `json:"-"`
}

// Prompt is one registered reusable prompt template surfaced in the host
// UI. We ship a few playbooks (walk_through_pending_setup, audit_seo, etc.)
// so users get the right starting frame.
type Prompt struct {
	Name        string `json:"name"`
	Description string `json:"description"`

	// Render produces the message stream the host sends to the model when
	// the user picks this prompt.
	Render func(ctx context.Context, agent *authmw.AgentIdentity, args map[string]string) ([]PromptMessage, error) `json:"-"`
}

// PromptMessage is one entry in a rendered prompt's message stream.
type PromptMessage struct {
	Role    string  `json:"role"` // "user" | "assistant" | "system"
	Content Content `json:"content"`
}

// writeJSON sends a JSON response with the given status code. Mirrors the
// helper in internal/handlers without taking a dependency on it.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
