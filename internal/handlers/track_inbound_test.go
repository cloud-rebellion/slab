package handlers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/bright-interaction/slab/internal/config"
	"github.com/bright-interaction/slab/internal/store"
)

// inboundRouter mirrors the production wiring for /t/inbound. The handler
// reads the body itself (not via FingerprintMiddleware), so we bind it
// directly without the /t group's middleware.
func inboundRouter(h *TrackHandler) chi.Router {
	r := chi.NewRouter()
	r.Post("/t/inbound", h.Inbound)
	return r
}

// signedInboundReq builds a signed POST against /t/inbound. Returns the
// raw body bytes and the resulting response so callers can inspect both.
func signedInboundReq(t *testing.T, r chi.Router, secret string, body any, headerOverrides map[string]string) (*http.Response, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	sig := hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/t/inbound", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Atomicsite-Signature", sig)
	for k, v := range headerOverrides {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Result(), raw
}

// seedVisitorWithID inserts a visit_sessions row with the given site_id +
// fingerprint, then attaches the visitor_id so /t/inbound can resolve it.
func seedVisitorWithID(t *testing.T, q *store.Queries, siteID, fingerprint, visitorID string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if err := q.UpsertVisitSession(context.Background(), store.UpsertVisitSessionParams{
		ID:          "session-" + visitorID,
		SiteID:      siteID,
		Fingerprint: fingerprint,
		StartedAt:   now,
		LastSeenAt:  now,
	}); err != nil {
		t.Fatalf("upsert visit session: %v", err)
	}
	if err := q.IdentifyVisitSession(context.Background(), store.IdentifyVisitSessionParams{
		VisitorID:             visitorID,
		Email:                 "",
		ConsentMethod:         "accept-all",
		ConsentCategoriesJson: "{}",
		IdentifiedAt:          now,
		LastSeenAt:            now,
		SiteID:                siteID,
		Fingerprint:           fingerprint,
	}); err != nil {
		t.Fatalf("identify visit session: %v", err)
	}
}

// TestInbound_RejectsWithoutSecret verifies 503 when BRIGHTCRM_WEBHOOK_SECRET
// is not configured. Operators must opt in.
func TestInbound_RejectsWithoutSecret(t *testing.T) {
	sqlDB, q := setupDeployTestDB(t)
	cfg := &config.Config{} // empty secret
	h := NewTrackHandler(cfg, q, sqlDB)
	r := inboundRouter(h)

	resp, _ := signedInboundReq(t, r, "anything", map[string]any{
		"site_id":    testSiteID,
		"visitor_id": "v_1",
	}, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}
}

// TestInbound_RejectsMissingSignature ensures the handler refuses requests
// that arrive without the X-Atomicsite-Signature header.
func TestInbound_RejectsMissingSignature(t *testing.T) {
	sqlDB, q := setupDeployTestDB(t)
	cfg := &config.Config{BrightCRMWebhookSecret: "shared-secret"}
	h := NewTrackHandler(cfg, q, sqlDB)
	r := inboundRouter(h)

	resp, _ := signedInboundReq(t, r, "shared-secret", map[string]any{
		"site_id":    testSiteID,
		"visitor_id": "v_1",
	}, map[string]string{"X-Atomicsite-Signature": ""})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

// TestInbound_RejectsBadSignature ensures HMAC verification actually compares.
func TestInbound_RejectsBadSignature(t *testing.T) {
	sqlDB, q := setupDeployTestDB(t)
	cfg := &config.Config{BrightCRMWebhookSecret: "real-secret"}
	h := NewTrackHandler(cfg, q, sqlDB)
	r := inboundRouter(h)

	// Sign with the wrong secret on purpose.
	resp, _ := signedInboundReq(t, r, "wrong-secret", map[string]any{
		"site_id":    testSiteID,
		"visitor_id": "v_1",
	}, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

// TestInbound_RejectsMalformedJSON returns 400 when the body parses badly
// after passing HMAC verification.
func TestInbound_RejectsMalformedJSON(t *testing.T) {
	sqlDB, q := setupDeployTestDB(t)
	cfg := &config.Config{BrightCRMWebhookSecret: "shared-secret"}
	h := NewTrackHandler(cfg, q, sqlDB)
	r := inboundRouter(h)

	body := []byte("{not json")
	mac := hmac.New(sha256.New, []byte("shared-secret"))
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))
	req := httptest.NewRequest(http.MethodPost, "/t/inbound", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Atomicsite-Signature", sig)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// TestInbound_RejectsInvalidSiteID rejects payloads whose site_id fails the
// safety regex (this is the path our smoke test "smoke-test" hits).
func TestInbound_RejectsInvalidSiteID(t *testing.T) {
	sqlDB, q := setupDeployTestDB(t)
	cfg := &config.Config{BrightCRMWebhookSecret: "shared-secret"}
	h := NewTrackHandler(cfg, q, sqlDB)
	r := inboundRouter(h)

	resp, _ := signedInboundReq(t, r, "shared-secret", map[string]any{
		"site_id":    "smoke-test", // not isSafeSiteID
		"visitor_id": "v_1",
	}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

// TestInbound_RejectsUnknownSite rejects payloads whose site_id passes the
// safety check but doesn't match any row in the sites table.
func TestInbound_RejectsUnknownSite(t *testing.T) {
	sqlDB, q := setupDeployTestDB(t)
	cfg := &config.Config{BrightCRMWebhookSecret: "shared-secret"}
	h := NewTrackHandler(cfg, q, sqlDB)
	r := inboundRouter(h)

	resp, _ := signedInboundReq(t, r, "shared-secret", map[string]any{
		"site_id":    "bbbbbbbbbbbbbbbbbbbbbbbb", // 24-char hex placeholder, no row
		"visitor_id": "v_1",
	}, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// TestInbound_RejectsMissingIdentifiers requires at least one of
// visitor_id or fingerprint.
func TestInbound_RejectsMissingIdentifiers(t *testing.T) {
	sqlDB, q := setupDeployTestDB(t)
	seedSiteWithSafeID(t, q, testSiteID)

	cfg := &config.Config{BrightCRMWebhookSecret: "shared-secret"}
	h := NewTrackHandler(cfg, q, sqlDB)
	r := inboundRouter(h)

	resp, _ := signedInboundReq(t, r, "shared-secret", map[string]any{
		"site_id": testSiteID,
		// no visitor_id, no fingerprint
	}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

// TestInbound_404OnUnknownVisitor returns 404 when the site is known but no
// matching visit_sessions row exists. BrightCRM's outbound client tolerates
// this (visitor not yet seen on the destination site).
func TestInbound_404OnUnknownVisitor(t *testing.T) {
	sqlDB, q := setupDeployTestDB(t)
	seedSiteWithSafeID(t, q, testSiteID)

	cfg := &config.Config{BrightCRMWebhookSecret: "shared-secret"}
	h := NewTrackHandler(cfg, q, sqlDB)
	r := inboundRouter(h)

	resp, _ := signedInboundReq(t, r, "shared-secret", map[string]any{
		"site_id":    testSiteID,
		"visitor_id": "v_unknown",
	}, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// TestInbound_HappyPath is the full round-trip: a known visitor exists on
// a known site, signed payload arrives, metadata is upserted, 204 returned.
func TestInbound_HappyPath(t *testing.T) {
	sqlDB, q := setupDeployTestDB(t)
	seedSiteWithSafeID(t, q, testSiteID)
	seedVisitorWithID(t, q, testSiteID, "fp_abc", "v_known")

	cfg := &config.Config{BrightCRMWebhookSecret: "shared-secret"}
	h := NewTrackHandler(cfg, q, sqlDB)
	r := inboundRouter(h)

	resp, _ := signedInboundReq(t, r, "shared-secret", map[string]any{
		"site_id":               testSiteID,
		"visitor_id":            "v_known",
		"email":                 "alice@example.com",
		"identity_confirmed_at": time.Now().UTC().Format(time.RFC3339),
		"metadata": map[string]any{
			"lifecycle":         "WARM",
			"lead_score":        42,
			"last_topic":        "Pricing page",
			"consent_marketing": true,
			"consent_analytics": true,
		},
	}, nil)

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}

	// Metadata should now be visible. Read back by fingerprint (the only
	// reader query exposed) to confirm the upsert wrote what we sent.
	row, err := q.GetVisitorMetadataByFingerprint(context.Background(), store.GetVisitorMetadataByFingerprintParams{
		SiteID:      testSiteID,
		Fingerprint: "fp_abc",
	})
	if err != nil {
		t.Fatalf("read back metadata: %v", err)
	}
	if !bytes.Contains([]byte(row.MetadataJson), []byte("WARM")) {
		t.Fatalf("expected metadata to contain WARM, got %q", row.MetadataJson)
	}
}
