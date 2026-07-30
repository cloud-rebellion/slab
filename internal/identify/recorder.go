// Package identify: Phase 31.3 (2026-05-06) -- shared visitor-identify
// flow used by both POST /t/identify (explicit SDK call from the
// rendered site) and FormHandler.Submit (auto-identify when a form
// submission carries an email-shaped value).
//
// One Recorder is constructed at server boot and shared by both
// handlers so identify semantics are one place: validate the email
// shape, update visit_sessions.email + identified_at, fire a
// crmsync "identified" event so BrightCRM gets the freshly-named
// contact along with their full UTM / page history.
package identify

import (
	"context"
	"errors"
	"log/slog"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/bright-interaction/slab/internal/crmsync"
	"github.com/bright-interaction/slab/internal/store"
)

// Recorder owns the queries + crmsync handles. nil-safe: a nil
// Recorder no-ops every call so unit tests don't need to plumb
// the dependency.
type Recorder struct {
	queries   *store.Queries
	crmClient *crmsync.Client
}

// NewRecorder builds a Recorder. Pass a disabled crmsync client
// (empty webhookURL/secret) to skip CRM dispatch -- the DB update
// still runs.
func NewRecorder(queries *store.Queries, crmClient *crmsync.Client) *Recorder {
	return &Recorder{queries: queries, crmClient: crmClient}
}

// Page captures URL context from the call site. Same shape as
// crmsync.Page so the forwarded event body matches the existing
// /t/inbound contract on the BrightCRM side.
type Page struct {
	URL      string
	Path     string
	Title    string
	Referrer string
}

// Outcome reports what happened. Callers use it for response shaping
// (200 vs 404 when no session exists) and for slog metadata.
type Outcome struct {
	Updated   bool
	VisitorID string
}

// ErrInvalidEmail is returned when the email argument is not a valid
// RFC 5322 address. The handler turns this into a 400.
var ErrInvalidEmail = errors.New("identify: invalid email")

// Identify validates the email shape, updates visit_sessions.email +
// identified_at if a session exists for (siteID, fingerprint), and
// fires a best-effort crmsync identified event so the CRM matches
// the freshly-named visitor to their behavioural history.
//
// The DB update is the source of truth; the CRM dispatch is async
// (SendAsync) so a slow webhook never blocks the caller. If no
// session row exists the call returns Outcome{Updated:false} with
// no error -- the caller decides whether to treat that as 404 or
// to upsert a fresh session first.
func (r *Recorder) Identify(ctx context.Context, siteID, fingerprint, email string, page Page) (Outcome, error) {
	if r == nil || r.queries == nil {
		return Outcome{}, nil
	}
	email = strings.TrimSpace(email)
	if email == "" || len(email) > 320 {
		return Outcome{}, ErrInvalidEmail
	}
	if _, err := mail.ParseAddress(email); err != nil {
		return Outcome{}, ErrInvalidEmail
	}

	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := r.queries.SetVisitSessionEmail(ctx, store.SetVisitSessionEmailParams{
		Email:        email,
		IdentifiedAt: now,
		LastSeenAt:   now,
		SiteID:       siteID,
		Fingerprint:  fingerprint,
	})
	if err != nil {
		slog.Warn("identify: set email failed",
			"site_id", siteID, "fingerprint", fingerprint, "err", err)
		return Outcome{}, err
	}
	if rows == 0 {
		return Outcome{Updated: false}, nil
	}

	// Pull visitor_id for the CRM payload.
	sess, err := r.queries.GetSessionByFingerprint(ctx, store.GetSessionByFingerprintParams{
		SiteID:      siteID,
		Fingerprint: fingerprint,
	})
	visitorID := ""
	if err == nil {
		visitorID = sess.VisitorID
	}

	r.dispatchCRM(ctx, siteID, visitorID, email, page)
	return Outcome{Updated: true, VisitorID: visitorID}, nil
}

// dispatchCRM is split out so unit tests can verify the call without
// standing up an HTTP server: a fake Client passed via NewRecorder
// records the SendAsync invocation.
func (r *Recorder) dispatchCRM(ctx context.Context, siteID, visitorID, email string, page Page) {
	if r.crmClient == nil || !r.crmClient.Enabled() {
		return
	}
	event := crmsync.Event{
		Event:      crmsync.EventIdentified,
		SiteID:     siteID,
		VisitorID:  visitorID,
		Email:      email,
		OccurredAt: time.Now().UTC(),
		Page: crmsync.Page{
			URL:      page.URL,
			Path:     page.Path,
			Title:    page.Title,
			Referrer: page.Referrer,
		},
		Metadata: map[string]any{
			"source": "form_or_identify",
		},
	}
	// Durable, not fire-and-forget. This event carries the visitor's EMAIL and
	// is the moment a fingerprint links to a real contact, which the package doc
	// calls the thing we must "never lose". SendAsync logged and dropped on
	// failure, so a brief CRM outage lost it while the visit_sessions write
	// succeeded: Slab believed the visitor identified, BrightCRM never heard,
	// and nothing recorded an outstanding dispatch.
	if err := crmsync.Enqueue(ctx, r.queries, newOutboxID(), event); err != nil {
		slog.Error("identify: could not enqueue identified event, falling back to best-effort send",
			"site_id", siteID, "err", err)
		r.crmClient.SendAsync(event)
	}
}

// newOutboxID mints an outbox row id. Local so this package does not depend on
// the handlers' id helper.
func newOutboxID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is not recoverable here; a time-based fallback
		// keeps the event deliverable rather than dropping it.
		return fmt.Sprintf("obx-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// emailFieldRE looks for plausibly-named email fields in form
// submissions. Used by FormHandler.Submit to pick the visitor's
// email out of the submitted JSON without requiring a fixed schema.
var emailFieldRE = regexp.MustCompile(`(?i)^(email|e_mail|e-mail|emailaddress|email_address|user_email|your[-_]email|contact[-_]email)$`)

// emailValueRE is a minimal email-shape fallback. We use it only as
// a tiebreaker when a field's name doesn't match emailFieldRE -- a
// last-resort scan of values in case the form labels its email
// field something exotic ("contact me at"). RFC 5322 has thousands
// of edge cases; we don't try to be exhaustive.
var emailValueRE = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

// PickEmail scans form-field key/value pairs and returns the most
// likely email address. Preference order: keys matching emailFieldRE
// (the labeled email field) first, then any value that looks like
// an email by emailValueRE. Returns "" when nothing plausible is
// found.
func PickEmail(fields map[string]string) string {
	// Pass 1: labelled email field.
	for k, v := range fields {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if emailFieldRE.MatchString(k) && emailValueRE.MatchString(v) {
			return v
		}
	}
	// Pass 2: any value that looks like an email.
	for _, v := range fields {
		v = strings.TrimSpace(v)
		if emailValueRE.MatchString(v) {
			return v
		}
	}
	return ""
}
