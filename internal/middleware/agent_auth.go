package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bright-interaction/slab/internal/store"
)

const AgentContextKey contextKey = "agent_key"

// AgentIdentity is the request-scoped agent after successful API key auth.
type AgentIdentity struct {
	KeyID        string
	SiteID       string
	Name         string
	Capabilities []string
}

// GetAgent retrieves the authenticated agent from the request context.
func GetAgent(r *http.Request) *AgentIdentity {
	a, _ := r.Context().Value(AgentContextKey).(*AgentIdentity)
	return a
}

// HashAgentKey produces a SHA-256 hash of the raw API key for storage.
func HashAgentKey(rawKey string) string {
	h := sha256.Sum256([]byte(rawKey))
	return hex.EncodeToString(h[:])
}

// agentBucket is a single token-bucket entry per agent key. Refills
// at rate tokens/second up to capacity.
type agentBucket struct {
	tokens     float64
	capacity   float64
	rate       float64 // tokens per second
	lastMs     int64
	lastSeenMs int64
}

// agentRateLimiter throttles per-agent-key request volume. Same
// shape as the consentRateLimiter used for /t/* but keyed on the
// agent KeyID instead of an IP. We pick conservative values:
// burst 60, refill 60/min sustained , covers any realistic agent
// workload (a build cycle is a handful of writes plus a build) and
// blocks runaway loops that wedge the server.
type agentRateLimiter struct {
	mu        sync.Mutex
	buckets   map[string]*agentBucket
	capacity  float64
	rate      float64
	idleAfter time.Duration
}

func newAgentRateLimiter(capacity int, ratePerSec float64, idleAfter time.Duration) *agentRateLimiter {
	return &agentRateLimiter{
		buckets:   make(map[string]*agentBucket),
		capacity:  float64(capacity),
		rate:      ratePerSec,
		idleAfter: idleAfter,
	}
}

// allow consumes one token for keyID. Returns (allowed,
// retryAfterSeconds). Empty keyID is allowed unconditionally to
// keep the unauth code-path functional for tests.
func (l *agentRateLimiter) allow(keyID string) (bool, int) {
	if keyID == "" {
		return true, 0
	}
	now := time.Now().UnixMilli()
	l.mu.Lock()
	defer l.mu.Unlock()

	// Lazy GC: drop buckets the agent hasn't touched in idleAfter
	// so a long-lived process doesn't accumulate one entry per
	// rotated key.
	cutoff := now - l.idleAfter.Milliseconds()
	for k, b := range l.buckets {
		if b.lastSeenMs < cutoff {
			delete(l.buckets, k)
		}
	}

	b, ok := l.buckets[keyID]
	if !ok {
		b = &agentBucket{
			tokens:   l.capacity - 1,
			capacity: l.capacity,
			rate:     l.rate,
			lastMs:   now,
		}
		b.lastSeenMs = now
		l.buckets[keyID] = b
		return true, 0
	}
	// Refill since lastMs.
	elapsedSec := float64(now-b.lastMs) / 1000.0
	b.tokens += elapsedSec * b.rate
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	b.lastMs = now
	b.lastSeenMs = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	// Compute seconds to next free token.
	missing := 1 - b.tokens
	wait := missing / b.rate
	if wait < 1 {
		wait = 1
	}
	return false, int(wait)
}

// AgentAuthMiddleware authenticates requests via X-Agent-Key header.
type AgentAuthMiddleware struct {
	queries *store.Queries
	// limiter throttles per-key request volume. Hardcoded to a
	// reasonable default (60 burst, 60/min sustained) , explicit
	// tuning lives in NewAgentAuthMiddleware.
	limiter *agentRateLimiter
}

func NewAgentAuthMiddleware(queries *store.Queries) *AgentAuthMiddleware {
	return &AgentAuthMiddleware{
		queries: queries,
		limiter: newAgentRateLimiter(60, 60.0/60.0, 30*time.Minute),
	}
}

func (m *AgentAuthMiddleware) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent, err := m.authenticate(r)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}

		// Per-key rate limit. Runs after authentication so unknown
		// keys don't churn the bucket map. The limiter capacity (60)
		// matches a generous agent workload; legitimate burst
		// activity (a build cycle is ~10 writes + 1 build) sits well
		// below it. Runaway loops trip 429 + Retry-After.
		if m.limiter != nil {
			if ok, retry := m.limiter.allow(agent.KeyID); !ok {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", strconv.Itoa(retry))
				w.WriteHeader(http.StatusTooManyRequests)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"error": "Too many requests for this agent key; slow down.",
				})
				return
			}
		}

		// Update last_used_at in background
		go func() {
			_ = m.queries.UpdateAgentKeyLastUsed(context.Background(), agent.KeyID)
		}()

		ctx := context.WithValue(r.Context(), AgentContextKey, agent)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (m *AgentAuthMiddleware) authenticate(r *http.Request) (*AgentIdentity, error) {
	raw := r.Header.Get("X-Agent-Key")
	if raw == "" {
		// Also accept Bearer token in Authorization header
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			raw = strings.TrimPrefix(auth, "Bearer ")
		}
	}
	if raw == "" {
		return nil, errAgentNoKey
	}

	keyHash := HashAgentKey(raw)
	// Revocation enforcement: GetAgentKeyByHash already filters
	// WHERE is_active = 1 at the SQL level, so a revoked key (set
	// is_active = 0 by RevokeAgentKey) returns sql.ErrNoRows here
	// and the request is rejected with errAgentInvalidKey. The test
	// in agent_auth_test.go pins this contract.
	row, err := m.queries.GetAgentKeyByHash(r.Context(), keyHash)
	if err != nil {
		return nil, errAgentInvalidKey
	}

	var caps []string
	if err := json.Unmarshal([]byte(row.Capabilities), &caps); err != nil {
		caps = []string{"read"}
	}

	return &AgentIdentity{
		KeyID:        row.ID,
		SiteID:       row.SiteID,
		Name:         row.Name,
		Capabilities: caps,
	}, nil
}

const (
	errAgentNoKey      authError = "API key required. Set X-Agent-Key header or Bearer token."
	errAgentInvalidKey authError = "Invalid or deactivated API key."
)
