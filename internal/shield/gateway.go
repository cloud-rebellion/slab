package shield

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Gateway is the process-wide Shield chokepoint: one object holding the
// store, the AES-256 key, the TTL and the hint level, from which EVERY
// agent-facing transport resolves its per-request Session.
//
// Why this type exists at all: Shield used to live inside internal/mcp,
// with the store + key as private fields of mcp.Server. That made the
// PII boundary a property of one transport rather than of the product.
// The 2026-08-11 audit found the predictable result: ContextBuilder.Build
// had ten callers, the nine inside internal/mcp were shielded and the
// tenth (GET /api/agent/context, same X-Agent-Key, same tenant data) was
// not, and GET /api/agent/visitor-metadata returned a visitor's raw email
// plus the whole CRM metadata map to the model while the MCP contract
// advertised that exact data as never exposed.
//
// The rule this type enforces: a new agent-facing transport gets its
// Session from a Gateway or it does not ship. Do NOT reintroduce
// per-transport store/key fields.
//
// A nil *Gateway, and one built with no store or a wrong-length key, is
// a disabled Gateway: Begin returns (nil, nil) and callers behave exactly
// as they did before Shield was configured. That keeps dev installs and
// unit tests without an ATOMICSITE_SHIELD_KEY working.
type Gateway struct {
	mu sync.RWMutex
	// key is guarded by mu so /admin/reload-secrets can hot-swap it
	// under live traffic. Everything else is write-once at construction.
	key       []byte
	store     Store
	enabled   bool
	ttl       time.Duration
	hintLevel HintLevel
}

// DefaultTTL is the session lifetime used when the caller passes zero.
// 24 hours lets a real Claude Code / agent conversation span a full
// working day; the TTL is housekeeping, not auth. X-Agent-Key validation
// upstream is the security boundary.
const DefaultTTL = 24 * time.Hour

// NewGateway builds the shared Shield chokepoint. store must be non-nil
// and key must be exactly 32 bytes, or the returned Gateway is disabled
// (this mirrors the old mcp.Server.WithShield contract, which silently
// no-opped on a bad key rather than refusing to boot; config.Validate is
// the layer that refuses a malformed production configuration).
func NewGateway(store Store, key []byte, ttl time.Duration, hintLevel HintLevel) *Gateway {
	g := &Gateway{}
	if store == nil || len(key) != 32 {
		return g
	}
	// Defensive copy: the caller may reuse or zero its buffer.
	dup := make([]byte, 32)
	copy(dup, key)
	g.store = store
	g.key = dup
	g.enabled = true
	g.hintLevel = hintLevel
	if ttl > 0 {
		g.ttl = ttl
	} else {
		g.ttl = DefaultTTL
	}
	return g
}

// Enabled reports whether tokenization is active. Nil-safe so callers can
// hold a *Gateway that was never configured.
func (g *Gateway) Enabled() bool {
	if g == nil {
		return false
	}
	return g.enabled
}

// UpdateKey hot-swaps the in-memory AES-256 key handed to every NEW
// Session. Existing Sessions hold their key by value (snapshot at
// NewSession time) and keep working until they expire, so PII tokenized
// with the prior key stays resolvable for that session's lifetime.
//
// Returns an error when Shield was never enabled or the key is the wrong
// length. A byte-identical value is a no-op rather than an error, because
// the rotation engine's dual-phase delivery sends the new value twice.
func (g *Gateway) UpdateKey(newKey []byte) error {
	if !g.Enabled() {
		return fmt.Errorf("shield: not enabled, cannot rotate key")
	}
	if len(newKey) != 32 {
		return fmt.Errorf("shield: key must be 32 bytes, got %d", len(newKey))
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.key) == 32 && string(g.key) == string(newKey) {
		return nil
	}
	dup := make([]byte, 32)
	copy(dup, newKey)
	g.key = dup
	return nil
}

// SessionID is the deterministic identifier for a (site, key, sub-scope)
// triple. Deterministic on purpose: the SAME agent hitting MCP and the
// REST agent API lands in the same tokenization context, so a marker
// minted on one transport resolves on the other. That is what makes the
// chokepoint one boundary rather than two that happen to run the same
// code.
//
// The sub-scope can only ever narrow within the caller's own (site, key)
// namespace, never across it: it is mixed into the hash, not substituted
// for the identity.
func SessionID(siteID, keyID, sub string) string {
	if sub == "" {
		sub = "default"
	}
	sum := sha256.Sum256([]byte(siteID + "\x00" + keyID + "\x00" + sub))
	return "s_" + hex.EncodeToString(sum[:16])
}

// Begin returns the per-request Session for a SERVER-resolved agent
// identity. Returns (nil, nil) when Shield is disabled, which every
// caller must treat as "pass data through unchanged".
//
// An empty siteID or keyID is an error, never a silent pass: a Session
// that is not tenant-scoped would pool one tenant's tokens with another's.
func (g *Gateway) Begin(ctx context.Context, siteID, keyID, sub string) (*Session, error) {
	if !g.Enabled() {
		return nil, nil
	}
	if siteID == "" || keyID == "" {
		return nil, errors.New("shield: no authenticated agent identity")
	}
	g.mu.RLock()
	key := g.key
	g.mu.RUnlock()
	return NewSession(ctx, g.store, SessionID(siteID, keyID, sub), key, g.ttl, g.hintLevel)
}
