package crmsync

// Durable delivery for identified-visitor events.
//
// The package doc promises identified events bypass the throttle "so we never
// lose the moment a visitor links to a contact", and then delivered them with
// SendAsync, which logs and drops on failure: no retry, no backoff, no dead
// letter. A 30-second BrightCRM deploy window silently lost every
// identification inside it, and because the local DB write succeeded and
// nothing recorded an outstanding dispatch, the only way to notice was
// hand-diffing two systems.
//
// The fix is a record of the obligation, enqueued in the same transaction as the
// visit_sessions write, plus a drain loop. Deliberately modelled on the existing
// webhook_deliveries manager: same status vocabulary, same backoff shape, same
// terminal 'dropped'. One outbox pattern in this codebase, not two.
//
// Best-effort analytics pings still use SendAsync. This is only for events whose
// loss is silently wrong.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/bright-interaction/slab/internal/store"
)

const (
	// outboxMaxAttempts bounds retries before a row goes terminal. Six attempts
	// on the backoff below spans roughly half an hour, which covers a deploy or
	// a short outage without retrying a genuinely bad payload forever.
	outboxMaxAttempts = 6
	// outboxDrainInterval is how often the loop looks for due rows. Identified
	// events are not latency-critical (the CRM is a system of record, not a live
	// surface), and a tight loop would add write contention on the single SQLite
	// writer for no benefit.
	outboxDrainInterval = 30 * time.Second
	// outboxBatch bounds one pass so a large backlog cannot monopolise the
	// writer or hold a long transaction.
	outboxBatch = 50
)

// outboxBackoff returns the delay before attempt n+1. Exponential with a cap,
// so a long outage does not push the next attempt beyond a useful horizon.
func outboxBackoff(attempts int64) time.Duration {
	d := time.Duration(1<<uint(attempts)) * time.Minute
	if d > 15*time.Minute {
		d = 15 * time.Minute
	}
	return d
}

// Enqueuer is the subset of store.Queries the enqueue path needs, so a caller
// can pass either the Queries handle or a transaction-scoped one. Enqueuing
// inside the caller's tx is the whole point: the local "this visitor is
// identified" write and the obligation to tell the CRM must land together.
type Enqueuer interface {
	EnqueueCRMOutbox(ctx context.Context, arg store.EnqueueCRMOutboxParams) error
}

// Enqueue records an event for durable delivery. Pass a tx-scoped Queries to
// tie it to the surrounding write.
func Enqueue(ctx context.Context, q Enqueuer, id string, event Event) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("crmsync: marshal outbox event: %w", err)
	}
	return q.EnqueueCRMOutbox(ctx, store.EnqueueCRMOutboxParams{
		ID:          id,
		SiteID:      event.SiteID,
		EventType:   string(event.Event),
		PayloadJson: string(payload),
	})
}

// OutboxManager drains the outbox. One goroutine, same shape as the webhook
// delivery manager.
type OutboxManager struct {
	queries *store.Queries
	client  *Client
	done    chan struct{}
}

func NewOutboxManager(queries *store.Queries, client *Client) *OutboxManager {
	return &OutboxManager{queries: queries, client: client, done: make(chan struct{})}
}

// Start runs the drain loop until ctx is done. Safe to call when the client is
// disabled: rows accumulate and drain once a secret is configured, which is
// better than dropping them because config was late.
func (m *OutboxManager) Start(ctx context.Context) {
	if m == nil || m.queries == nil {
		return
	}
	go func() {
		defer close(m.done)
		t := time.NewTicker(outboxDrainInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n, err := m.DrainOnce(ctx); err != nil {
					slog.Warn("crmsync outbox: drain", "err", err)
				} else if n > 0 {
					slog.Info("crmsync outbox: delivered", "count", n)
				}
			}
		}
	}()
}

// Done is closed when the drain loop exits.
func (m *OutboxManager) Done() <-chan struct{} { return m.done }

// DrainOnce attempts every due row once and returns how many were delivered.
// Exported so a test can drive one pass deterministically instead of waiting on
// the ticker.
func (m *OutboxManager) DrainOnce(ctx context.Context) (int, error) {
	if m.client == nil || !m.client.Enabled() {
		// Not an error: leave the rows for when the CRM is configured. Dropping
		// them because a secret was not set yet is the failure this replaces.
		return 0, nil
	}
	rows, err := m.queries.ListDueCRMOutbox(ctx, outboxBatch)
	if err != nil {
		return 0, err
	}

	delivered := 0
	for _, row := range rows {
		if ctx.Err() != nil {
			return delivered, ctx.Err()
		}
		var event Event
		if uerr := json.Unmarshal([]byte(row.PayloadJson), &event); uerr != nil {
			// Unparseable payload will never succeed, so retrying it is pure
			// noise. Terminal immediately, and kept so an operator can see it.
			if merr := m.queries.MarkCRMOutboxDropped(ctx, store.MarkCRMOutboxDroppedParams{
				LastError: "unparseable payload: " + uerr.Error(),
				ID:        row.ID,
			}); merr != nil {
				slog.Warn("crmsync outbox: mark dropped", "id", row.ID, "err", merr)
			}
			continue
		}

		sendErr := m.client.Send(ctx, event)
		if sendErr == nil {
			if merr := m.queries.MarkCRMOutboxSucceeded(ctx, row.ID); merr != nil {
				// The send DID happen. Failing to record it means the next pass
				// re-sends, which is why the receiver must treat these as
				// idempotent; log loudly rather than pretend.
				slog.Error("crmsync outbox: delivered but could not mark succeeded; it will be re-sent",
					"id", row.ID, "err", merr)
			}
			delivered++
			continue
		}
		if errors.Is(sendErr, context.Canceled) || errors.Is(sendErr, context.DeadlineExceeded) {
			// Shutdown or timeout: leave the row due and try next pass rather
			// than burning an attempt on our own cancellation.
			return delivered, nil
		}

		if row.Attempts+1 >= outboxMaxAttempts {
			if merr := m.queries.MarkCRMOutboxDropped(ctx, store.MarkCRMOutboxDroppedParams{
				LastError: sendErr.Error(),
				ID:        row.ID,
			}); merr != nil {
				slog.Warn("crmsync outbox: mark dropped", "id", row.ID, "err", merr)
			}
			// Loud: this is a permanently lost identification, which is the
			// exact event the old code lost silently every time.
			slog.Error("crmsync outbox: giving up on an identified event after max attempts",
				"id", row.ID, "site_id", row.SiteID, "event", row.EventType,
				"attempts", row.Attempts+1, "err", sendErr)
			continue
		}
		next := time.Now().UTC().Add(outboxBackoff(row.Attempts)).Format("2006-01-02 15:04:05")
		if merr := m.queries.MarkCRMOutboxRetrying(ctx, store.MarkCRMOutboxRetryingParams{
			NextAttemptAt: next,
			LastError:     sendErr.Error(),
			ID:            row.ID,
		}); merr != nil {
			slog.Warn("crmsync outbox: mark retrying", "id", row.ID, "err", merr)
		}
	}
	return delivered, nil
}
