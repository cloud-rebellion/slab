package handlers

import (
	"sync"
	"sync/atomic"
	"time"
)

// consentRateLimiter is an in-process per-IP token bucket guarding the
// /t/consent receiver against abuse (spam proofs, log poisoning by way of
// volume). It's deliberately tiny and dependency-free: a sync.Map of
// (ip → bucket), refilled on demand, with a periodic sweep to evict
// stale entries.
//
// Defaults match the practical upper bound of legitimate consent traffic:
// a real visitor changes consent maybe 3-4 times per session, so 20
// proofs/minute/IP is generous for normal use and tight enough that a
// single attacker can't fill consent_records faster than the daily
// retention sweep clears it.
//
// The receiver is public + cross-origin, so we can't rely on session or
// CSRF gates. IP throttling is the right hammer here, and the key comes
// from clientIP(), which resolves r.RemoteAddr only. This comment used
// to say "CF-Connecting-IP and X-Forwarded-For are honoured via the
// existing clientIP() helper", which described the bug as if it were
// the design: honouring a caller-supplied header means the caller picks
// their own bucket and the limiter throttles nobody (audit 2026-08-11
// finding 9). The trust decision belongs to authmw.TrustedProxyRealIP,
// which is gated on the peer being a configured proxy.
type consentRateLimiter struct {
	mu          sync.Mutex
	buckets     map[string]*tokenBucket
	capacity    float64
	refillPerMs float64
	maxIdle     time.Duration
	// emptyKeyHits counts allow() calls that were short-circuited because
	// clientIP returned "" (proxy stripped headers). Audit L10: surfaced
	// via the EmptyKeyHits accessor so /api/admin/metrics can graph it.
	emptyKeyHits atomic.Uint64
}

// EmptyKeyHits returns the number of allow() calls that bypassed the
// limiter due to an empty IP key. Read by /api/admin/metrics.
func (l *consentRateLimiter) EmptyKeyHits() uint64 {
	return l.emptyKeyHits.Load()
}

type tokenBucket struct {
	tokens    float64
	lastSeenT int64 // unix-ms
}

// newConsentRateLimiter creates a limiter with `capacity` proofs as the
// burst and `refillPerSec` proofs replenished per second. Callers should
// pick the per-second value to match the desired steady-state rate; the
// burst cap absorbs the natural bunching of "open page → accept all"
// double-fires.
func newConsentRateLimiter(capacity float64, refillPerSec float64, maxIdle time.Duration) *consentRateLimiter {
	if capacity <= 0 {
		capacity = 20
	}
	if refillPerSec <= 0 {
		refillPerSec = 20.0 / 60.0 // 20/min sustained
	}
	if maxIdle <= 0 {
		maxIdle = 10 * time.Minute
	}
	return &consentRateLimiter{
		buckets:     make(map[string]*tokenBucket),
		capacity:    capacity,
		refillPerMs: refillPerSec / 1000,
		maxIdle:     maxIdle,
	}
}

// allow returns true if the given key (IP string) has tokens available
// after refill, false otherwise. Empty key always returns true so a
// proxy-misconfig that strips IPs doesn't break consent recording.
//
// Audit L10: the empty-key bypass is intentional but observable. We
// increment an atomic counter on every empty hit so the operator can
// graph it via /api/admin/metrics or a future /metrics endpoint and
// detect when their proxy is stripping IPs in bulk (the symptom: this
// counter rising fast while the limiter's `buckets` map stays small).
func (l *consentRateLimiter) allow(key string) bool {
	if key == "" {
		l.emptyKeyHits.Add(1)
		return true
	}
	now := time.Now().UnixMilli()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		// New IP: full burst available, consume one.
		l.buckets[key] = &tokenBucket{tokens: l.capacity - 1, lastSeenT: now}
		return true
	}
	elapsed := float64(now - b.lastSeenT)
	if elapsed > 0 {
		b.tokens += elapsed * l.refillPerMs
		if b.tokens > l.capacity {
			b.tokens = l.capacity
		}
	}
	b.lastSeenT = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep evicts buckets that haven't been touched within maxIdle.
// Idempotent.
//
// Audit 2026-08-11 finding 9: the previous comment here claimed this
// was "called from the manager's background goroutine". No such
// goroutine existed; the only callers were in _test.go, so buckets
// accumulated for the process lifetime. It is now driven by
// ratelimit.Sweeper, wired in TrackHandler.RegisterSweeps and started
// from cmd/server. Do not restore the claim without the wiring.
func (l *consentRateLimiter) sweep() {
	cutoff := time.Now().Add(-l.maxIdle).UnixMilli()
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.buckets {
		if b.lastSeenT < cutoff {
			delete(l.buckets, k)
		}
	}
}
