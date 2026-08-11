// Package handlers - ratelimit_sweeps.go.
//
// One place that names every process-local limiter owned by this
// package and hands its eviction func to the shared scheduler. Audit
// 2026-08-11 finding 9: each of these had an eviction method whose doc
// comment promised a background caller that never existed, so their
// maps grew for the life of the process. Keeping the registrations
// together means the next limiter added here has an obvious empty slot
// to fill instead of inheriting the same silent omission.
//
// Limiters NOT listed here because they evict inline on every call and
// need no scheduler: WaitlistHandler's rateBucket (sweeps at most once
// a minute inside allowSubmit), formBurstTracker.last (opportunistic GC
// past 1024 entries), middleware.agentRateLimiter (walks its map on
// every allow), and crmsync.Throttler (maybeSweepLocked).
package handlers

import "github.com/bright-interaction/slab/internal/ratelimit"

// RegisterSweeps hands the five /t/* token-bucket limiters to the
// shared sweeper. Called once at route construction.
func (h *TrackHandler) RegisterSweeps(s *ratelimit.Sweeper) {
	if h == nil {
		return
	}
	for _, e := range []struct {
		name string
		l    *consentRateLimiter
	}{
		{"track.consent", h.consentLimiter},
		{"track.pageview", h.pageviewLimiter},
		{"track.engagement", h.engagementLimiter},
		{"track.event", h.eventLimiter},
		{"track.identify", h.identifyLimiter},
	} {
		if e.l == nil {
			continue
		}
		s.Register(e.name, e.l.sweep)
	}
}

// RegisterSweeps hands the login lockout counter to the shared sweeper.
// The MFA step (oidc_mfa.go) shares this same instance, so it is
// covered by this one registration.
func (h *AuthHandler) RegisterSweeps(s *ratelimit.Sweeper) {
	if h == nil || h.loginLimiter == nil {
		return
	}
	s.Register("auth.login", h.loginLimiter.sweep)
}

// RegisterSweeps hands the form-spam throttle to the shared sweeper.
func (h *FormHandler) RegisterSweeps(s *ratelimit.Sweeper) {
	if h == nil || h.limiter == nil {
		return
	}
	s.Register("forms.submit", h.limiter.sweep)
}
