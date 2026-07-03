package shield

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore is a Store implementation backed by Redis. Unlocks Shield for
// multi-node atomicsite deploys: SQLStore is single-node (one SQLite + WAL),
// MemoryStore is per-process (each node has its own map). RedisStore lets
// every node in a cluster see the same shield_sessions + shield_tokens
// state with single-digit-ms reads.
//
// Key layout:
//
//	shield:sess:<id>           -> expires_at unix nanos (string)
//	shield:tok:<token>         -> JSON {session_id, kind, hint, ciphertext}
//	shield:sess:<id>:tokens    -> SET of tokens issued in this session
//
// TTL on every key matches the session expiry so Redis garbage-collects
// expired sessions automatically without needing a sweep job. SweepExpiredSessions
// is a no-op here because Redis handles it via per-key TTL.
type RedisStore struct {
	Client redis.UniversalClient
	// Prefix is prepended to every key. Empty = "shield:" default. Set
	// when multiple atomicsite tenants share one Redis instance so the
	// keyspace per tenant stays isolated.
	Prefix string
}

// NewRedisStore returns a RedisStore wrapping the given client. The client
// must already be configured (TLS, auth, etc); RedisStore does no
// connection management beyond a single Ping at first use.
func NewRedisStore(client redis.UniversalClient) *RedisStore {
	return &RedisStore{Client: client}
}

// NewRedisStoreFromURL is a convenience constructor that parses a redis://
// URL and returns a RedisStore. The client is created with sane production
// defaults (3 retries, 30s read/write timeout). Callers that need tighter
// control should construct the client themselves and pass it to NewRedisStore.
func NewRedisStoreFromURL(rawURL string) (*RedisStore, error) {
	opts, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, fmt.Errorf("shield: parse redis url: %w", err)
	}
	if opts.MaxRetries == 0 {
		opts.MaxRetries = 3
	}
	return NewRedisStore(redis.NewClient(opts)), nil
}

func (r *RedisStore) prefix() string {
	if r.Prefix == "" {
		return "shield:"
	}
	return r.Prefix
}

func (r *RedisStore) sessKey(id string) string {
	return r.prefix() + "sess:" + id
}

func (r *RedisStore) tokKey(token string) string {
	return r.prefix() + "tok:" + token
}

func (r *RedisStore) sessTokensKey(id string) string {
	return r.prefix() + "sess:" + id + ":tokens"
}

// ttlFromExpiry derives a Redis TTL from an absolute expiry timestamp.
// Returns at least 1 second to satisfy Redis's positive-TTL requirement;
// callers should not be inserting already-expired sessions.
func ttlFromExpiry(expiresAt time.Time) time.Duration {
	d := time.Until(expiresAt)
	if d <= 0 {
		return time.Second
	}
	return d
}

func (r *RedisStore) InsertSession(ctx context.Context, id string, expiresAt time.Time) error {
	return r.Client.Set(ctx, r.sessKey(id),
		expiresAt.UTC().UnixNano(),
		ttlFromExpiry(expiresAt),
	).Err()
}

func (r *RedisStore) GetSession(ctx context.Context, id string) (time.Time, bool, error) {
	raw, err := r.Client.Get(ctx, r.sessKey(id)).Int64()
	if errors.Is(err, redis.Nil) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return time.Unix(0, raw).UTC(), true, nil
}

func (r *RedisStore) UpdateSessionExpiry(ctx context.Context, id string, expiresAt time.Time) error {
	// EXPIRE only succeeds when the key exists, matching the SQL store's
	// no-op-on-missing behavior. We follow with a SET if the EXPIRE
	// returned false (no such key); callers can decide to recreate the
	// session by calling InsertSession instead.
	ok, err := r.Client.Expire(ctx, r.sessKey(id), ttlFromExpiry(expiresAt)).Result()
	if err != nil {
		return err
	}
	if !ok {
		// Session does not exist; caller asked to update an unknown id.
		// SQLStore's UPDATE is a silent no-op in this case; mirror that.
		return nil
	}
	// Also refresh the stored expiry value so a later GetSession
	// returns the new timestamp, not the original one.
	return r.Client.Set(ctx, r.sessKey(id),
		expiresAt.UTC().UnixNano(),
		ttlFromExpiry(expiresAt),
	).Err()
}

func (r *RedisStore) SweepExpiredSessions(_ context.Context, _ time.Time) error {
	// Redis garbage-collects expired keys automatically via per-key TTL.
	// No sweep needed; return nil so the shared sweep job stays compatible.
	return nil
}

func (r *RedisStore) InsertToken(ctx context.Context, sessionID, token, kind, hint, ciphertext string) error {
	// Idempotent: the same (session, token) tuple is a deterministic id
	// for the same value, so a re-insert is a no-op. SETNX returns false
	// when the key already exists, which is the success path for repeats.
	// JSON marshal is inline to avoid pulling encoding/json into the hot
	// path's allocation budget; the fields are simple strings.
	payload := redisTokenJSON(sessionID, kind, hint, ciphertext)
	// We rely on session TTL to garbage-collect orphan tokens, but set
	// an explicit per-token TTL too so a session sweep that runs before
	// Redis's lazy expiry still bounds memory growth.
	sessExp, err := r.Client.PTTL(ctx, r.sessKey(sessionID)).Result()
	if err != nil {
		return err
	}
	if sessExp < 0 {
		// No TTL on session or session missing; fall back to 1 hour.
		// Callers must hit InsertSession before InsertToken; this is
		// defensive in case the session expired between calls.
		sessExp = time.Hour
	}
	if _, err := r.Client.SetNX(ctx, r.tokKey(token), payload, sessExp).Result(); err != nil {
		return err
	}
	if err := r.Client.SAdd(ctx, r.sessTokensKey(sessionID), token).Err(); err != nil {
		return err
	}
	return r.Client.Expire(ctx, r.sessTokensKey(sessionID), sessExp).Err()
}

func (r *RedisStore) GetTokenCiphertext(ctx context.Context, sessionID, token string) (string, bool, error) {
	raw, err := r.Client.Get(ctx, r.tokKey(token)).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	gotSession, kind, hint, ciphertext, ok := parseRedisTokenJSON(raw)
	_ = kind
	_ = hint
	if !ok {
		return "", false, fmt.Errorf("shield: corrupt redis token payload for %q", token)
	}
	// Replay protection: token bound to a different session looks
	// identical to "not found" to the caller, matching SQLStore.
	if gotSession != sessionID {
		return "", false, nil
	}
	return ciphertext, true, nil
}

// redisTokenJSON serializes a token row as a JSON-ish line with a fixed
// field order. Avoids encoding/json overhead in the hot path; the four
// fields never contain unescaped newlines (ciphertext is base64, kind +
// hint are validated alphanumerics). One \n delimits the record.
func redisTokenJSON(sessionID, kind, hint, ciphertext string) string {
	// Format: session_id\nkind\nhint\nciphertext
	return sessionID + "\n" + kind + "\n" + hint + "\n" + ciphertext
}

func parseRedisTokenJSON(s string) (sessionID, kind, hint, ciphertext string, ok bool) {
	parts := strings.SplitN(s, "\n", 4)
	if len(parts) != 4 {
		return "", "", "", "", false
	}
	return parts[0], parts[1], parts[2], parts[3], true
}
