package conversions

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/bright-interaction/slab/internal/store"
)

// Recorder writes conversion_events for goals that match an in-flight
// signal. It loads the active goals for the site, runs the matcher,
// and inserts a row per match. Failures are logged at WARN and never
// returned: a goal-record write must never break the user-facing
// pageview/form/event path.
//
// Phase 31.1, 2026-05-06.
type Recorder struct {
	queries *store.Queries
}

// NewRecorder constructs a Recorder bound to the given sqlc Queries.
func NewRecorder(queries *store.Queries) *Recorder {
	return &Recorder{queries: queries}
}

// Signal carries everything the matcher + recorder need to evaluate
// one in-flight conversion candidate. ValueOverrideCents lets a
// /t/event call carry an event-time value (e.g. cart total) that
// supersedes the goal's static value_cents; pass 0 to defer to the
// goal.
type Signal struct {
	SiteID             string
	Fingerprint        string
	SessionID          string
	Path               string
	MatchType          MatchType
	MatchValue         string
	ValueOverrideCents int64
	Properties         map[string]any
}

// Evaluate looks up the site's active goals, matches the signal
// against them, and inserts a conversion_events row for every
// matching goal. Returns the number of rows inserted (callers can
// add this to slog metadata if they want; nil error means "no
// fatal failure" not "successful insert", because best-effort).
func (r *Recorder) Evaluate(ctx context.Context, sig Signal) int {
	if r == nil || r.queries == nil {
		return 0
	}
	goals, err := r.queries.ListActiveGoalsBySite(ctx, sig.SiteID)
	if err != nil {
		slog.Warn("conversions: list goals failed", "site_id", sig.SiteID, "err", err)
		return 0
	}
	if len(goals) == 0 {
		return 0
	}
	matcher := make([]Goal, 0, len(goals))
	for _, g := range goals {
		matcher = append(matcher, Goal{
			ID:         g.ID,
			SiteID:     g.SiteID,
			Slug:       g.Slug,
			MatchType:  MatchType(g.MatchType),
			MatchValue: g.MatchValue,
		})
	}
	matched := MatchGoals(matcher, sig.MatchType, sig.MatchValue)
	if len(matched) == 0 {
		return 0
	}

	props := "{}"
	if len(sig.Properties) > 0 {
		if b, err := json.Marshal(sig.Properties); err == nil {
			props = string(b)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	count := 0
	for _, m := range matched {
		// Pull the original goal row to read value_cents/currency.
		var goalRow store.ConversionGoal
		for _, g := range goals {
			if g.ID == m.ID {
				goalRow = g
				break
			}
		}
		valueCents := goalRow.ValueCents
		if sig.ValueOverrideCents > 0 {
			valueCents = sig.ValueOverrideCents
		}
		err := r.queries.RecordConversionEvent(ctx, store.RecordConversionEventParams{
			ID:             newConversionID(),
			SiteID:         sig.SiteID,
			GoalID:         m.ID,
			Fingerprint:    sig.Fingerprint,
			SessionID:      sig.SessionID,
			Path:           sig.Path,
			Ts:             now,
			ValueCents:     valueCents,
			ValueCurrency:  goalRow.ValueCurrency,
			PropertiesJson: props,
		})
		if err != nil {
			slog.Warn("conversions: record event failed",
				"site_id", sig.SiteID, "goal_id", m.ID, "err", err)
			continue
		}
		count++
	}
	return count
}

// newConversionID returns a 24-hex random id matching the rest of
// atomicsite's id space.
func newConversionID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}
