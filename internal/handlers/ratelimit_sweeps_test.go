package handlers

import (
	"testing"
	"time"

	"github.com/bright-interaction/slab/internal/config"
	"github.com/bright-interaction/slab/internal/ratelimit"
)

// Audit 2026-08-11 finding 9, second half. Every limiter here shipped
// with an eviction method whose doc comment named a background caller
// that did not exist; the only callers in the tree were in _test.go.
// These tests assert the registration path actually connects the
// scheduler to the real limiter maps, so a sweep evicts for real rather
// than evicting some limiter the handler does not use.

// ageBucket backdates a bucket so it falls outside maxIdle without the
// test having to wait ten minutes.
func ageBucket(l *consentRateLimiter, key string, age time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.buckets[key]; ok {
		b.lastSeenT = time.Now().Add(-age).UnixMilli()
	}
}

func bucketCount(l *consentRateLimiter) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

func TestTrackHandlerRegistersEveryLimiter(t *testing.T) {
	h := NewTrackHandler(&config.Config{}, nil, nil)
	s := ratelimit.New(time.Hour)
	h.RegisterSweeps(s)

	want := []string{
		"track.consent",
		"track.pageview",
		"track.engagement",
		"track.event",
		"track.identify",
	}
	got := s.Names()
	if len(got) != len(want) {
		t.Fatalf("registered %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("registered %v, want %v", got, want)
		}
	}
}

// The load-bearing assertion: a sweep through the registered callback
// must drain the very map that allow() writes to.
func TestTrackHandlerSweepEvictsLiveBuckets(t *testing.T) {
	h := NewTrackHandler(&config.Config{}, nil, nil)
	s := ratelimit.New(time.Hour)
	h.RegisterSweeps(s)

	limiters := map[string]*consentRateLimiter{
		"consent":    h.consentLimiter,
		"pageview":   h.pageviewLimiter,
		"engagement": h.engagementLimiter,
		"event":      h.eventLimiter,
		"identify":   h.identifyLimiter,
	}
	for name, l := range limiters {
		if !l.allow("198.51.100.7") {
			t.Fatalf("%s: first request should be allowed", name)
		}
		if bucketCount(l) != 1 {
			t.Fatalf("%s: expected 1 bucket after allow()", name)
		}
		ageBucket(l, "198.51.100.7", time.Hour)
	}

	s.SweepNow()

	for name, l := range limiters {
		if n := bucketCount(l); n != 0 {
			t.Fatalf("%s: %d buckets survived the sweep; the limiter is not wired to the sweeper", name, n)
		}
	}
}

func TestAuthAndFormHandlersRegisterLoginLimiters(t *testing.T) {
	ah := NewAuthHandler(&config.Config{}, nil)
	fh := NewFormHandler(&config.Config{}, nil, nil, nil)

	s := ratelimit.New(time.Hour)
	ah.RegisterSweeps(s)
	fh.RegisterSweeps(s)

	names := s.Names()
	if len(names) != 2 || names[0] != "auth.login" || names[1] != "forms.submit" {
		t.Fatalf("registered %v, want [auth.login forms.submit]", names)
	}

	// Age both failure counters past their idle window and prove the
	// sweep reaches them.
	for _, l := range []*loginRateLimiter{ah.loginLimiter, fh.limiter} {
		l.recordFailure("i:198.51.100.7")
		l.mu.Lock()
		f := l.failures["i:198.51.100.7"]
		f.firstFailMs = time.Now().Add(-72 * time.Hour).UnixMilli()
		f.lockedUntilMs = 0
		l.mu.Unlock()
	}

	s.SweepNow()

	for name, l := range map[string]*loginRateLimiter{"auth": ah.loginLimiter, "forms": fh.limiter} {
		l.mu.Lock()
		n := len(l.failures)
		l.mu.Unlock()
		if n != 0 {
			t.Fatalf("%s: %d failure entries survived the sweep", name, n)
		}
	}
}
