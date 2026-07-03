package mcp

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/bright-interaction/slab/internal/settingspolicy"
)

// newID mirrors handlers.newID() so the MCP package doesn't import the
// handlers package (which would create a cycle: handlers imports the mcp
// server in server.go via the route wiring).
func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// isAgentWritableCategory + validateSettingValue delegate to
// internal/settingspolicy, the single source of truth shared with the
// REST handlers. The MCP surface used to keep hand-synced copies that
// drifted (ten missing analytics validators, missing search category);
// policy changes now land in one package and both surfaces follow.
func isAgentWritableCategory(category string) bool {
	return settingspolicy.AgentWritableCategory(category)
}

func validateSettingValue(category, key, value string) error {
	return settingspolicy.ValidateSetting(category, key, value)
}
