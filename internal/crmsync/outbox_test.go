package crmsync

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	dbpkg "github.com/bright-interaction/slab/internal/db"
	"github.com/bright-interaction/slab/internal/store"
)

func openOutboxTestDB(t *testing.T) (*sql.DB, *store.Queries) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "outbox.sqlite")
	sqlDB, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if _, err := sqlDB.Exec(string(dbpkg.Schema)); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return sqlDB, store.New(sqlDB)
}

// THE point of the outbox. SendAsync logged and dropped on failure, so a brief
// BrightCRM outage silently lost the identification while the local write
// succeeded. This drives a real outage and then a recovery.
func TestOutboxSurvivesAnOutageAndDeliversOnRecovery(t *testing.T) {
	_, q := openOutboxTestDB(t)
	ctx := context.Background()

	var up atomic.Bool
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !up.Load() {
			w.WriteHeader(http.StatusBadGateway) // CRM mid-deploy
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-secret")
	mgr := NewOutboxManager(q, client)

	if err := Enqueue(ctx, q, "obx-1", Event{
		Event: EventIdentified, SiteID: "site-1", VisitorID: "v1",
		Email: "visitor@example.com", OccurredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Outage: the row must NOT be lost, and must NOT be marked succeeded.
	n, err := mgr.DrainOnce(ctx)
	if err != nil {
		t.Fatalf("drain during outage: %v", err)
	}
	if n != 0 {
		t.Fatalf("drain reported %d delivered during an outage", n)
	}
	pending, err := q.CountPendingCRMOutbox(ctx)
	if err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 1 {
		t.Fatalf("pending = %d after a failed delivery, want 1: the event was LOST, which is the bug this replaces", pending)
	}

	// The backoff must actually hold the row back, or a long outage turns into a
	// hot retry loop against a struggling upstream.
	if n, _ := mgr.DrainOnce(ctx); n != 0 {
		t.Error("a retrying row was attempted again immediately; backoff is not applied")
	}

	// Recovery. Clear the backoff the way elapsed time would.
	up.Store(true)
	if err := q.MarkCRMOutboxRetrying(ctx, store.MarkCRMOutboxRetryingParams{
		NextAttemptAt: time.Now().UTC().Add(-time.Minute).Format("2006-01-02 15:04:05"),
		LastError:     "simulated elapsed backoff",
		ID:            "obx-1",
	}); err != nil {
		t.Fatalf("reset backoff: %v", err)
	}
	n, err = mgr.DrainOnce(ctx)
	if err != nil {
		t.Fatalf("drain after recovery: %v", err)
	}
	if n != 1 {
		t.Fatalf("delivered %d after recovery, want 1", n)
	}
	pending, _ = q.CountPendingCRMOutbox(ctx)
	if pending != 0 {
		t.Errorf("pending = %d after successful delivery, want 0", pending)
	}
	// And it must not be re-sent once succeeded.
	before := hits.Load()
	if n, _ := mgr.DrainOnce(ctx); n != 0 {
		t.Error("a succeeded row was re-delivered")
	}
	if hits.Load() != before {
		t.Error("a succeeded row hit the CRM again")
	}
}

// A permanently failing event must go terminal rather than retry forever, and
// must be LOUD, because it is a genuinely lost identification.
func TestOutboxGivesUpAfterMaxAttempts(t *testing.T) {
	_, q := openOutboxTestDB(t)
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	mgr := NewOutboxManager(q, NewClient(srv.URL, "s"))
	if err := Enqueue(ctx, q, "obx-dead", Event{Event: EventIdentified, SiteID: "s1"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Drive attempts until the row goes terminal. The backoff is cleared BEFORE
	// each drain, not after: clearing afterwards flips a just-dropped row back to
	// 'retrying' and the loop never converges, which is how the first version of
	// this test failed against correct code.
	for i := 0; i < outboxMaxAttempts+3; i++ {
		pending, err := q.CountPendingCRMOutbox(ctx)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if pending == 0 {
			break // terminal, which is what we want
		}
		if err := q.MarkCRMOutboxRetrying(ctx, store.MarkCRMOutboxRetryingParams{
			NextAttemptAt: time.Now().UTC().Add(-time.Minute).Format("2006-01-02 15:04:05"),
			LastError:     "clearing backoff for the next attempt",
			ID:            "obx-dead",
		}); err != nil {
			t.Fatalf("clear backoff: %v", err)
		}
		if _, err := mgr.DrainOnce(ctx); err != nil {
			t.Fatalf("drain %d: %v", i, err)
		}
	}
	pending, _ := q.CountPendingCRMOutbox(ctx)
	if pending != 0 {
		t.Errorf("pending = %d; a permanently failing event should be terminal, not retried forever", pending)
	}
}

// A disabled client must PARK rows, not drop them. Dropping because a secret was
// not configured yet is the same silent loss under a different cause.
func TestOutboxParksRowsWhenClientDisabled(t *testing.T) {
	_, q := openOutboxTestDB(t)
	ctx := context.Background()
	mgr := NewOutboxManager(q, NewClient("", "")) // disabled
	if err := Enqueue(ctx, q, "obx-parked", Event{Event: EventIdentified, SiteID: "s1"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if n, err := mgr.DrainOnce(ctx); err != nil || n != 0 {
		t.Fatalf("drain with disabled client: n=%d err=%v", n, err)
	}
	if pending, _ := q.CountPendingCRMOutbox(ctx); pending != 1 {
		t.Errorf("pending = %d, want 1: rows must park until the CRM is configured", pending)
	}
}
