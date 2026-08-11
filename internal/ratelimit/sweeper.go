// Package ratelimit holds the shared eviction scheduler for the
// process-local rate limiters.
//
// Why this exists (audit 2026-08-11, finding 9, second half): every
// in-process limiter in this codebase shipped with an idle-eviction
// method and a doc comment claiming a background goroutine called it.
// None of them had one. `grep -rn '\.sweep()\|\.Sweep()'` returned only
// _test.go callers for consentRateLimiter (five live instances behind
// /t/*), loginRateLimiter (two: the login lockout and the form-spam
// throttle) and middleware.AdminWriteLimiter. Their maps therefore grew
// for the life of the process, one entry per distinct key, and because
// the growth is monotonic there is no restart-correlated spike to make
// it visible in a memory graph.
//
// The eviction logic itself is genuinely different per limiter (token
// buckets vs failure counters vs sliding windows), so consolidating
// THAT would be a false abstraction. What was duplicated-by-omission is
// the schedule. This package owns the schedule and nothing else: each
// limiter registers its own eviction func, and one goroutine calls them
// all on a fixed interval.
//
// Registration is explicit and happens at route-construction time, so
// a limiter that is never registered is a visible omission at the call
// site rather than a silent leak. Add a limiter, register it here, and
// cover it in sweeper_test.go's live-registration assertion.
package ratelimit

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// DefaultInterval is how often the sweeper evicts. The limiters' own
// idle windows range from 10 minutes (/t/*) to 2 hours (form spam), so
// a 5 minute tick bounds the map at "keys seen in the idle window plus
// at most one tick" for every one of them while costing a handful of
// map walks per hour.
const DefaultInterval = 5 * time.Minute

// Sweeper runs registered eviction callbacks on a fixed interval until
// its context is cancelled. The zero value is not usable; call New.
type Sweeper struct {
	interval time.Duration

	mu      sync.Mutex
	targets []target
	started bool
}

type target struct {
	name  string
	evict func()
}

// New builds a Sweeper. A non-positive interval falls back to
// DefaultInterval so a misconfigured caller still gets eviction rather
// than a busy loop or a dead ticker.
func New(interval time.Duration) *Sweeper {
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Sweeper{interval: interval}
}

// Register adds an eviction callback. name is for logging only.
// Registering after Start is allowed: the ticker walks the slice fresh
// on every tick, so a late registration is picked up on the next one.
// A nil Sweeper or nil evict is ignored, which keeps the call sites
// free of nil checks in tests that build handlers without a sweeper.
func (s *Sweeper) Register(name string, evict func()) {
	if s == nil || evict == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.targets = append(s.targets, target{name: name, evict: evict})
}

// Len reports how many callbacks are registered.
func (s *Sweeper) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.targets)
}

// Names lists the registered callback names in registration order.
// Used by the boot log and by the test that asserts every live limiter
// actually got wired.
func (s *Sweeper) Names() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.targets))
	for _, t := range s.targets {
		out = append(out, t.name)
	}
	return out
}

// Started reports whether Start has been called. Lets a test assert
// that the boot path really launched the sweeper rather than merely
// registering limiters into one that nobody drives.
func (s *Sweeper) Started() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

// SweepNow runs every registered callback once, synchronously. Exported
// so tests can assert eviction without waiting on a ticker.
func (s *Sweeper) SweepNow() {
	if s == nil {
		return
	}
	s.mu.Lock()
	snapshot := make([]target, len(s.targets))
	copy(snapshot, s.targets)
	s.mu.Unlock()
	for _, t := range snapshot {
		t.evict()
	}
}

// Start launches the sweep goroutine. It returns immediately. Calling
// Start more than once is a no-op after the first, so a duplicated wire-up
// cannot spawn competing goroutines. The goroutine exits when ctx is done.
func (s *Sweeper) Start(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	names := make([]string, 0, len(s.targets))
	for _, t := range s.targets {
		names = append(names, t.name)
	}
	s.mu.Unlock()

	slog.Info("rate-limiter sweeper started", "interval", s.interval, "limiters", names)

	go func() {
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				slog.Info("rate-limiter sweeper stopped")
				return
			case <-ticker.C:
				s.SweepNow()
			}
		}
	}()
}
