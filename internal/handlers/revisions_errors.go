package handlers

import "errors"

// Typed errors the MCP tools map to JSON-RPC errors so the agent
// sees a stable error name rather than a generic Internal Error.
var (
	errInvalidEntityType   = errors.New("invalid entity_type (must be page or block)")
	errEntityNotFound      = errors.New("entity not found in site")
	errRevisionNotFound    = errors.New("revision not found")
	errSnapshotUnparseable = errors.New("snapshot not parseable")
)
