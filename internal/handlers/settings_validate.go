package handlers

import "github.com/bright-interaction/slab/internal/settingspolicy"

// validateSetting checks one {category, key, value} write. The actual
// validators live in internal/settingspolicy, the single source of
// truth shared with the MCP surface (they used to be hand-synced copies
// and drifted). This wrapper keeps the handler-local name every caller
// in this package already uses.
func validateSetting(category, key, value string) error {
	return settingspolicy.ValidateSetting(category, key, value)
}
