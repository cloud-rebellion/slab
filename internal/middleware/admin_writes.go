// Package middleware - admin_writes.go.
//
// AdminWriteRateLimit gates POST / PUT / PATCH / DELETE traffic on the
// authenticated admin surface. The login throttle covers credential
// brute force; this covers everything else: a logged-in user (or
// stolen session) hammering /api/sites/{id}/builds, /media,
// /collections/items, etc.
//
// Per-(user, peer-IP) keying so a shared NAT can't lock the whole
// office out, while a single user from one IP gets throttled
// uniformly. Token bucket with capacity 20, refill 1/sec (= 60/min
// sustained, 20-burst). That covers interactive batch-editing and
// reorder operations comfortably and stops a runaway script at ~60
// writes per user per minute. Returns 429 with Retry-After.
//
// The limiter is in-process; horizontal scale-out moves this to Redis
// via the same `Limiter` interface. For atomicsite's single-binary
// shape, in-process is the right scope.

package middleware

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// AdminWriteLimiter is a per-key token bucket. Exported so the server
// constructor can plumb a custom instance for tests.
type AdminWriteLimiter struct {
	mu          sync.Mutex
	buckets     map[string]*adminWriteBucket
	capacity    float64
	refillPerMs float64
	maxIdle     time.Duration
}

type adminWriteBucket struct {
	tokens    float64
	lastSeenT int64 // unix-ms
}

// NewAdminWriteLimiter builds a limiter. capacity is the burst
// allowance per key; refillPerSec is the steady-state replenishment
// rate. maxIdle is how long a bucket can sit untouched before sweep
// evicts it.
func NewAdminWriteLimiter(capacity, refillPerSec float64, maxIdle time.Duration) *AdminWriteLimiter {
	if capacity <= 0 {
		capacity = 20
	}
	if refillPerSec <= 0 {
		refillPerSec = 1 // 60/min
	}
	if maxIdle <= 0 {
		maxIdle = 30 * time.Minute
	}
	return &AdminWriteLimiter{
		buckets:     make(map[string]*adminWriteBucket),
		capacity:    capacity,
		refillPerMs: refillPerSec / 1000,
		maxIdle:     maxIdle,
	}
}

// allow returns (ok, retryAfterSeconds). retryAfterSeconds is non-zero
// only when ok is false; it's the seconds the caller should wait
// before retrying so the response can set a Retry-After header.
func (l *AdminWriteLimiter) allow(key string) (bool, int) {
	if key == "" {
		return true, 0
	}
	now := time.Now().UnixMilli()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		l.buckets[key] = &adminWriteBucket{tokens: l.capacity - 1, lastSeenT: now}
		return true, 0
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
		// Round up to the next whole second the caller should wait
		// before one token will be available.
		need := 1 - b.tokens
		retryMs := need / l.refillPerMs
		retrySec := int(retryMs/1000) + 1
		return false, retrySec
	}
	b.tokens--
	return true, 0
}

// Sweep evicts buckets idle longer than maxIdle.
//
// Audit 2026-08-11 finding 9, third twin: "the manager calls it
// periodically to bound memory" described a manager that did not
// exist, same as consentRateLimiter.sweep and loginRateLimiter.sweep.
// The buckets map grew one entry per (user, peer-IP) pair for the
// process lifetime. It is now registered with ratelimit.Sweeper in
// Server.Router and started from cmd/server.
func (l *AdminWriteLimiter) Sweep() {
	cutoff := time.Now().Add(-l.maxIdle).UnixMilli()
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.buckets {
		if b.lastSeenT < cutoff {
			delete(l.buckets, k)
		}
	}
}

// AdminWriteRateLimit returns the chi-style middleware. Mounts after
// AuthMiddleware so GetUser(r) is populated; the user's ID is half
// the bucket key.
func AdminWriteRateLimit(l *AdminWriteLimiter) func(http.Handler) http.Handler {
	if l == nil {
		// Disabled: pass-through.
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isWriteMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			key := adminWriteKey(r)
			if ok, retryAfter := l.allow(key); !ok {
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				writeAuthError(w, http.StatusTooManyRequests, "Too many write requests; slow down")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func isWriteMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// adminWriteKey returns the bucket key. Combines user ID (when the
// auth middleware has populated it) with peer IP. Falls back to
// IP-only for unauthenticated requests (those still pass through if
// AdminWriteRateLimit is mounted before auth, but the typical mount
// point is after auth so user is always set).
func adminWriteKey(r *http.Request) string {
	user := GetUser(r)
	ip := remoteAddrIPString(r.RemoteAddr)
	if user == nil {
		return "anon|" + ip
	}
	return user.ID + "|" + ip
}

func remoteAddrIPString(remote string) string {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return remote
	}
	return host
}
