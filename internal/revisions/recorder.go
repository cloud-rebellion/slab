// Package revisions writes per-entity history snapshots so dashboards
// and MCP tools can list, inspect, and restore prior states.
//
// Sprint 1 of the WP/Webflow replacement roadmap (see
// atomicsite/docs/ROADMAP.md): trust foundation. Operators edit more
// aggressively when undo is one click away.
//
// Scope: pages + blocks. Future sprints add global_blocks,
// collection_items, site_branding via the same recorder.
//
// Semantics: BEFORE every entity mutation (Update / Delete), the
// handler calls Record with a snapshot of the existing row. Restore is
// itself a normal Update path which therefore writes its own revision,
// so history is append-only. Retention is 50 revisions per entity;
// older rows are pruned on insert so storage stays bounded.
package revisions

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/bright-interaction/slab/internal/store"
)

// Entity types stored in entity_revisions.entity_type. Must match the
// schema CHECK constraint in internal/db/schema.sql.
const (
	EntityTypePage        = "page"
	EntityTypeBlock       = "block"
	EntityTypePageLocale  = "page_locale"
	EntityTypeBlockLocale = "block_locale"
)

// MaxPerEntity is the retention cap. PruneEntityRevisionsOverLimit
// trims older rows on every insert so the table stays bounded.
const MaxPerEntity = 50

// Recorder writes a snapshot row to entity_revisions and prunes the
// older tail. Constructed once at server boot and shared across
// handlers; safe for concurrent use because every call goes through
// the underlying *sql.DB which serialises SQLite writes.
type Recorder struct {
	queries *store.Queries
}

// New returns a Recorder backed by the given store. A nil store
// returns a nil Recorder so callers can wire optional revision
// recording without changing existing signatures.
func New(q *store.Queries) *Recorder {
	if q == nil {
		return nil
	}
	return &Recorder{queries: q}
}

// RecordParams is the input shape for Record. Snapshot is marshalled
// to JSON via encoding/json so any serialisable struct works. The
// caller is responsible for marshalling pointer types appropriately
// (e.g. pass the dereferenced value, not the pointer).
type RecordParams struct {
	SiteID        string
	EntityType    string
	EntityID      string
	Snapshot      any
	ChangeSummary string
	CreatedBy     string
}

// Record writes a single revision row for the given entity. Validates
// entity_type against the schema CHECK constraint, marshals Snapshot
// to JSON, inserts the row (version_number computed atomically inside
// the INSERT, see entity_revisions.sql), then prunes rows past
// MaxPerEntity. All errors are returned; callers typically log +
// continue because revision recording must never block a real update.
func (r *Recorder) Record(ctx context.Context, p RecordParams) error {
	if r == nil {
		return errors.New("revisions: recorder is nil")
	}
	if p.SiteID == "" {
		return errors.New("revisions: site_id required")
	}
	if p.EntityID == "" {
		return errors.New("revisions: entity_id required")
	}
	switch p.EntityType {
	case EntityTypePage, EntityTypeBlock, EntityTypePageLocale, EntityTypeBlockLocale:
	default:
		return fmt.Errorf("revisions: unsupported entity_type %q", p.EntityType)
	}
	if p.Snapshot == nil {
		return errors.New("revisions: snapshot required")
	}
	snap, err := json.Marshal(p.Snapshot)
	if err != nil {
		return fmt.Errorf("revisions: marshal snapshot: %w", err)
	}

	if _, err := r.queries.CreateEntityRevision(ctx, store.CreateEntityRevisionParams{
		ID:            newID(),
		SiteID:        p.SiteID,
		EntityType:    p.EntityType,
		EntityID:      p.EntityID,
		SnapshotJson:  string(snap),
		ChangeSummary: p.ChangeSummary,
		CreatedBy:     p.CreatedBy,
	}); err != nil {
		return fmt.Errorf("revisions: insert: %w", err)
	}

	// Prune over-limit rows. Best-effort: if prune fails the older
	// rows just stick around, which is harmless until the next insert.
	_ = r.queries.PruneEntityRevisionsOverLimit(ctx, store.PruneEntityRevisionsOverLimitParams{
		SiteID:     p.SiteID,
		EntityType: p.EntityType,
		EntityID:   p.EntityID,
		Limit:      MaxPerEntity,
	})
	return nil
}

// newID returns a random 12-byte hex string. Mirrors
// internal/handlers.newID() rather than importing the handlers
// package (which would create a cycle once handlers depends on this
// package).
func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
