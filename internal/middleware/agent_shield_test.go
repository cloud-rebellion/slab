package middleware

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/slab/internal/shield"
)

const shieldTestKey = "0123456789abcdef0123456789abcdef" // 32 bytes

func testGateway(t *testing.T) *shield.Gateway {
	t.Helper()
	return shield.NewGateway(shield.NewMemoryStore(), []byte(shieldTestKey), time.Hour, shield.HintFull)
}

// withIdentity wraps h so the shield middleware sees an authenticated agent,
// standing in for AgentAuthMiddleware.
func withIdentity(h http.Handler, id *AgentIdentity) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id == nil {
			h.ServeHTTP(w, r)
			return
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), AgentContextKey, id)))
	})
}

var testIdentity = &AgentIdentity{KeyID: "ak_1", SiteID: "site_1", Capabilities: []string{"read", "write"}}

// TestAgentShieldRedactsJSONResponses is the core of audit finding 12: a REST
// handler that never calls Shield still cannot put a raw email in front of the
// model.
func TestAgentShieldRedactsJSONResponses(t *testing.T) {
	h := AgentShield(testGateway(t))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"email":"anna@andersson-law.se","customer_name":"Anna Andersson"}`))
	}))

	rec := httptest.NewRecorder()
	withIdentity(h, testIdentity).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agent/whatever", nil))

	body := rec.Body.String()
	if strings.Contains(body, "anna@andersson-law.se") {
		t.Errorf("email leaked: %s", body)
	}
	if strings.Contains(body, "Anna Andersson") {
		t.Errorf("customer_name leaked: %s", body)
	}
	if !strings.Contains(body, "[shield:email:") || !strings.Contains(body, "[shield:name:") {
		t.Errorf("expected markers for both fields: %s", body)
	}
}

// TestAgentShieldResolvesRequestMarkers pins the write direction: a marker the
// agent echoes back must reach the handler as plaintext, never as a literal.
func TestAgentShieldResolvesRequestMarkers(t *testing.T) {
	gw := testGateway(t)
	sess, err := gw.Begin(context.Background(), "site_1", "ak_1", "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	marker, err := sess.Tokenize(context.Background(), shield.KindEmail, "anna@andersson-law.se", "domain=andersson-law.se")
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}

	var seen string
	h := AgentShield(gw)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Email string `json:"email"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		seen = body.Email
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	req := httptest.NewRequest(http.MethodPatch, "/api/agent/profile", strings.NewReader(`{"email":"`+marker+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	withIdentity(h, testIdentity).ServeHTTP(rec, req)

	if seen != "anna@andersson-law.se" {
		t.Errorf("handler saw %q, want the resolved plaintext", seen)
	}
}

// TestAgentShieldFailsClosedOnRedactionError: when tokenization cannot run,
// the response is withheld rather than forwarded raw. This is the invariant
// the MCP prompts/get path was violating.
func TestAgentShieldFailsClosedOnRedactionError(t *testing.T) {
	gw := shield.NewGateway(&brokenTokenStore{MemoryStore: shield.NewMemoryStore()}, []byte(shieldTestKey), time.Hour, shield.HintFull)
	h := AgentShield(gw)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"email":"anna@andersson-law.se"}`))
	}))

	rec := httptest.NewRecorder()
	withIdentity(h, testIdentity).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agent/whatever", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "anna@andersson-law.se") {
		t.Errorf("PII forwarded on a redaction error: %s", rec.Body.String())
	}
}

// TestAgentShieldFailsClosedWithoutIdentity: registered ahead of the auth
// middleware by mistake, it must refuse rather than silently pass PII.
func TestAgentShieldFailsClosedWithoutIdentity(t *testing.T) {
	h := AgentShield(testGateway(t))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"email":"anna@andersson-law.se"}`))
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agent/whatever", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "anna@andersson-law.se") {
		t.Errorf("PII forwarded with no identity: %s", rec.Body.String())
	}
}

// TestAgentShieldExemptsOnlyMCP pins the single carve-out. The MCP mount does
// its own per-method shielding off the SAME Gateway; every other path on the
// agent surface goes through this middleware. If a second exemption ever gets
// added, this test is where it has to be argued for.
func TestAgentShieldExemptsOnlyMCP(t *testing.T) {
	exempt := []string{"/mcp", "/mcp/", "/mcp/anything", "/api/agent/mcp"}
	covered := []string{
		"/api/agent/context", "/api/agent/visitor-metadata", "/api/agent/profile",
		"/api/agent/settings", "/api/agent/branding", "/api/agent/media",
		"/api/agent/bootstrap", "/api/agent/mcpfoo", "/api/agent/orders",
	}
	h := AgentShield(testGateway(t))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"email":"anna@andersson-law.se"}`))
	}))
	for _, p := range exempt {
		rec := httptest.NewRecorder()
		withIdentity(h, testIdentity).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if !strings.Contains(rec.Body.String(), "anna@andersson-law.se") {
			t.Errorf("%s: expected passthrough (MCP shields itself), got %s", p, rec.Body.String())
		}
	}
	for _, p := range covered {
		rec := httptest.NewRecorder()
		withIdentity(h, testIdentity).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if strings.Contains(rec.Body.String(), "anna@andersson-law.se") {
			t.Errorf("%s: PII LEAK, path not covered by the shield chokepoint: %s", p, rec.Body.String())
		}
	}
}

// TestAgentShieldRedactsMarkdown covers GET /api/agent/bootstrap, which is
// text/markdown rather than JSON.
func TestAgentShieldRedactsMarkdown(t *testing.T) {
	h := AgentShield(testGateway(t))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write([]byte("# Site\n\n- Contact: anna@andersson-law.se\n"))
	}))
	rec := httptest.NewRecorder()
	withIdentity(h, testIdentity).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agent/bootstrap", nil))
	if strings.Contains(rec.Body.String(), "anna@andersson-law.se") {
		t.Errorf("markdown body leaked an email: %s", rec.Body.String())
	}
}

// TestAgentShieldLeavesBase64Intact: POST /api/agent/screenshot returns a
// base64 PNG. The regex bank's phone pattern matches '+' followed by digits,
// which base64 emits constantly, so without the encoded-blob guard the
// chokepoint would hand the agent an undecodable image.
func TestAgentShieldLeavesBase64Intact(t *testing.T) {
	raw := make([]byte, 64*1024)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	b64 := base64.StdEncoding.EncodeToString(raw)
	payload, err := json.Marshal(map[string]any{"image_base64": b64, "format": "png"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	h := AgentShield(testGateway(t))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	rec := httptest.NewRecorder()
	withIdentity(h, testIdentity).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/agent/screenshot", nil))

	var out struct {
		ImageBase64 string `json:"image_base64"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ImageBase64 != b64 {
		t.Errorf("base64 payload corrupted by the redact pass: %d bytes in, %d out", len(b64), len(out.ImageBase64))
	}
}

// TestAgentShieldIsNoOpWhenDisabled: an install with no ATOMICSITE_SHIELD_KEY
// must behave exactly as before.
func TestAgentShieldIsNoOpWhenDisabled(t *testing.T) {
	h := AgentShield(shield.NewGateway(nil, nil, 0, shield.HintFull))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"email":"anna@andersson-law.se"}`))
	}))
	rec := httptest.NewRecorder()
	withIdentity(h, testIdentity).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agent/profile", nil))
	if !strings.Contains(rec.Body.String(), "anna@andersson-law.se") {
		t.Errorf("disabled gateway redacted anyway: %s", rec.Body.String())
	}
}

// TestAgentShieldPreservesStatusCode: a handler's 404 must stay a 404.
func TestAgentShieldPreservesStatusCode(t *testing.T) {
	h := AgentShield(testGateway(t))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}))
	rec := httptest.NewRecorder()
	withIdentity(h, testIdentity).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agent/profile", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// brokenTokenStore fails every token write, which is how a redaction error is
// produced without reaching for a real driver failure.
type brokenTokenStore struct {
	*shield.MemoryStore
}

func (b *brokenTokenStore) InsertToken(ctx context.Context, sessionID, token, kind, hint, ciphertext string) error {
	return errors.New("store unavailable")
}
