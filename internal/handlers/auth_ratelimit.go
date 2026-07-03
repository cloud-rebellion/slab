package handlers

import (
	"strings"
	"sync"
	"time"
)

// loginRateLimiter throttles failed logins on two axes: per-email AND
// per-IP. Either axis hitting the threshold blocks subsequent attempts
// for `lockoutFor`. Closes audit finding H3.
//
// Why two axes:
//   - per-email defeats a brute force against a single account
//     ("admin@example.com + password1, password2, password3...") even
//     when the attacker rotates IPs.
//   - per-IP defeats a credential-spray against many accounts from one
//     attacker host ("admin@a.com:foo, admin@b.com:foo, ...").
//
// Successful logins reset BOTH counters for the (email, ip) pair, so a
// legitimate user mistyping their password a few times is not punished
// after they get it right. Internally each axis is its own counter; a
// pure email-spray attacker gets locked on per-IP first and never reaches
// the per-email threshold for any one account, which is the correct
// outcome.
type loginRateLimiter struct {
	mu             sync.Mutex
	failures       map[string]*loginFailureCount
	threshold      int
	lockoutFor     time.Duration
	maxIdleEntries time.Duration
}

type loginFailureCount struct {
	count         int
	firstFailMs   int64
	lockedUntilMs int64
}

// newLoginRateLimiter constructs a limiter. threshold = number of failed
// attempts allowed within the windowing implied by lockoutFor before the
// next attempt is rejected with 429. maxIdleEntries controls the
// background sweep so the map doesn't grow unbounded.
func newLoginRateLimiter(threshold int, lockoutFor, maxIdleEntries time.Duration) *loginRateLimiter {
	if threshold <= 0 {
		threshold = 5
	}
	if lockoutFor <= 0 {
		lockoutFor = 15 * time.Minute
	}
	if maxIdleEntries <= 0 {
		maxIdleEntries = time.Hour
	}
	return &loginRateLimiter{
		failures:       make(map[string]*loginFailureCount),
		threshold:      threshold,
		lockoutFor:     lockoutFor,
		maxIdleEntries: maxIdleEntries,
	}
}

// allow returns (allowed, retryAfterSeconds). The caller must check
// allowed before doing the bcrypt compare; if false, return 429 with the
// Retry-After header set to retryAfterSeconds.
//
// Empty key is allowed (proxy-strips-IP doesn't lock anyone out — same
// pattern as consentRateLimiter; tracked as audit finding L10).
func (l *loginRateLimiter) allow(key string) (bool, int) {
	if strings.TrimSpace(key) == "" {
		return true, 0
	}
	now := time.Now().UnixMilli()
	l.mu.Lock()
	defer l.mu.Unlock()
	f, ok := l.failures[key]
	if !ok {
		return true, 0
	}
	if f.lockedUntilMs > now {
		return false, int((f.lockedUntilMs - now) / 1000)
	}
	// Lockout expired: reset the counter on next failure.
	if f.lockedUntilMs > 0 && f.lockedUntilMs <= now {
		delete(l.failures, key)
	}
	return true, 0
}

// recordFailure increments the failure count for the key. When the
// threshold is reached, sets lockedUntilMs so subsequent allow() calls
// return false until the lockout expires.
func (l *loginRateLimiter) recordFailure(key string) {
	if strings.TrimSpace(key) == "" {
		return
	}
	now := time.Now().UnixMilli()
	l.mu.Lock()
	defer l.mu.Unlock()
	f, ok := l.failures[key]
	if !ok {
		f = &loginFailureCount{firstFailMs: now}
		l.failures[key] = f
	}
	// Slide the window: if the first fail was longer ago than the
	// lockout duration, treat this as a fresh series.
	if now-f.firstFailMs > l.lockoutFor.Milliseconds() {
		f.count = 0
		f.firstFailMs = now
		f.lockedUntilMs = 0
	}
	f.count++
	if f.count >= l.threshold {
		f.lockedUntilMs = now + l.lockoutFor.Milliseconds()
	}
}

// recordSuccess clears the failure counter for the key. Called on a
// successful login so an honest user who mistyped a few times gets a
// clean slate.
func (l *loginRateLimiter) recordSuccess(key string) {
	if strings.TrimSpace(key) == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, key)
}

// sweep evicts entries that haven't been touched within maxIdleEntries.
// Idempotent; designed to be called from a periodic goroutine.
func (l *loginRateLimiter) sweep() {
	cutoff := time.Now().Add(-l.maxIdleEntries).UnixMilli()
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, f := range l.failures {
		mostRecent := f.firstFailMs
		if f.lockedUntilMs > mostRecent {
			mostRecent = f.lockedUntilMs
		}
		if mostRecent < cutoff {
			delete(l.failures, k)
		}
	}
}
