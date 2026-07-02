package agent

import (
	"context"
	"strings"

	"github.com/bright-interaction/slab/internal/store"
)

// DesignFidelity is the per-site design-freedom dial. It decides which
// taste rules the playbook teaches, which critique detectors score vs
// advise, and which eval budgets grade the build. Security, privacy,
// accessibility and content-authenticity rules are identical across
// every fidelity; the dial trades page weight and motion budget for
// expressiveness, never safety.
//
// Stored as the site_settings row category="design" key="fidelity".
// A missing row, empty value, or unknown value resolves to
// FidelityBalanced so existing sites keep grading byte-identically and
// enforcement is never silently downgraded (same fail-safe posture as
// design.strict_lint).
type DesignFidelity string

const (
	// FidelityPerformance targets perfect scores: the playbook steers
	// the agent away from decorative weight (canvas heroes, perpetual
	// motion, heavy imagery) so every category can hit A+. Grading
	// budgets are the same as balanced; the difference is guidance.
	FidelityPerformance DesignFidelity = "performance"
	// FidelityBalanced is today's behavior and the default.
	FidelityBalanced DesignFidelity = "balanced"
	// FidelityShowcase unlocks expressive design: layered ambient
	// motion, scroll-driven animations, gradients, bespoke tokens,
	// view transitions, larger page budgets. The eval engine grades
	// against showcase budgets and records the fidelity on the
	// evaluation row so an A in showcase is never confused with an A
	// in performance.
	FidelityShowcase DesignFidelity = "showcase"
)

// FidelitySettingCategory / FidelitySettingKey name the settings row the
// dial lives in. Shared so the catalog, validators, builder, eval and
// critique engines can't drift on the spelling.
const (
	FidelitySettingCategory = "design"
	FidelitySettingKey      = "fidelity"
)

// FidelityValues enumerates the valid setting values, in dial order.
var FidelityValues = []string{
	string(FidelityPerformance),
	string(FidelityBalanced),
	string(FidelityShowcase),
}

// ParseFidelity normalises a raw setting value to a DesignFidelity.
// Unknown, empty, or garbage input resolves to balanced: never silently
// relax (showcase) and never silently regrade existing sites
// (performance).
func ParseFidelity(raw string) DesignFidelity {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(FidelityPerformance):
		return FidelityPerformance
	case string(FidelityShowcase):
		return FidelityShowcase
	default:
		return FidelityBalanced
	}
}

// FidelityFromSettings reads the dial out of an already-loaded
// "category.key" settings map (the shape ContextBuilder.Build and the
// builder stages assemble).
func FidelityFromSettings(settingsMap map[string]string) DesignFidelity {
	return ParseFidelity(settingsMap[FidelitySettingCategory+"."+FidelitySettingKey])
}

// FidelityForSite resolves the dial with a single settings read. Missing
// row or DB error resolves to balanced (fail-safe, mirrors
// strictDesignLintEnabled's posture).
func FidelityForSite(ctx context.Context, queries *store.Queries, siteID string) DesignFidelity {
	if queries == nil || siteID == "" {
		return FidelityBalanced
	}
	row, err := queries.GetSetting(ctx, store.GetSettingParams{
		SiteID:   siteID,
		Category: FidelitySettingCategory,
		Key:      FidelitySettingKey,
	})
	if err != nil {
		return FidelityBalanced
	}
	return ParseFidelity(row.Value)
}
