package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bright-interaction/slab/internal/store"
)

// consentSaltCache caches the day -> salt lookup for the current process so
// every consent POST doesn't hit SQLite. The salt rotates daily at UTC
// midnight; cache misses regenerate from the DB (or insert a new salt for
// today when none exists yet).
//
// Salts older than the retention window (default 30 days) are pruned by
// PruneConsentSalts so historical hashes can never be linked back to an IP.
type consentSaltCache struct {
	mu   sync.RWMutex
	day  string // YYYY-MM-DD UTC
	salt string
}

var saltCache = &consentSaltCache{}

// todayUTC returns the current UTC date as YYYY-MM-DD.
func todayUTC() string {
	return time.Now().UTC().Format("2006-01-02")
}

// getOrCreateConsentSalt returns the salt for the current UTC day. If the
// daily salt doesn't exist yet, it generates a 32-byte random salt and
// upserts it. Read-mostly path, so the cache short-circuits 99.9% of calls.
func getOrCreateConsentSalt(ctx context.Context, queries *store.Queries) (string, error) {
	day := todayUTC()

	saltCache.mu.RLock()
	if saltCache.day == day && saltCache.salt != "" {
		s := saltCache.salt
		saltCache.mu.RUnlock()
		return s, nil
	}
	saltCache.mu.RUnlock()

	// Cache miss: try DB.
	saltCache.mu.Lock()
	defer saltCache.mu.Unlock()
	// Re-check after acquiring write lock (another goroutine may have populated).
	if saltCache.day == day && saltCache.salt != "" {
		return saltCache.salt, nil
	}

	salt, err := queries.GetConsentSalt(ctx, day)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("get consent salt: %w", err)
	}
	if errors.Is(err, sql.ErrNoRows) || salt == "" {
		// Mint a new salt.
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("rand read: %w", err)
		}
		salt = hex.EncodeToString(buf)
		if err := queries.UpsertConsentSalt(ctx, store.UpsertConsentSaltParams{
			DayUtc: day,
			Salt:   salt,
		}); err != nil {
			return "", fmt.Errorf("upsert consent salt: %w", err)
		}
		// Audit M3: re-read after upsert. Two goroutines that both hit
		// the cache miss can both call UpsertConsentSalt; the upsert
		// uses ON CONFLICT DO NOTHING, so only one row's salt wins.
		// Without the re-read the loser's local `salt` variable
		// diverges from the persisted row, hashing IPs differently
		// for ~1ms at midnight. Re-fetching the persisted row
		// guarantees both goroutines cache the same value.
		persisted, perr := queries.GetConsentSalt(ctx, day)
		if perr == nil && persisted != "" {
			salt = persisted
		}
	}

	saltCache.day = day
	saltCache.salt = salt
	return salt, nil
}

// hashIPWithSalt returns SHA-256(ip || salt) hex-encoded. ip is normalised by
// trimming whitespace and lowercasing IPv6 zones. Empty ip yields empty
// hash (caller may store "" when fingerprint already provides uniqueness).
func hashIPWithSalt(ip, salt, siteID string) string {
	ip = strings.TrimSpace(strings.ToLower(ip))
	if ip == "" {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(ip))
	h.Write([]byte(":"))
	h.Write([]byte(salt))
	// Per-tenant: mixing the site id means the same IP hashes differently on
	// each site, so a query that ever forgets its site_id filter still cannot
	// link a visitor across tenants (the daily consent salt is otherwise global).
	h.Write([]byte(":"))
	h.Write([]byte(siteID))
	return hex.EncodeToString(h.Sum(nil))
}
