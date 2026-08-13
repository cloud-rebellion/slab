package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/bright-interaction/slab/internal/config"
	dbpkg "github.com/bright-interaction/slab/internal/db"
	"github.com/bright-interaction/slab/internal/storage"
	"github.com/bright-interaction/slab/internal/store"
)

// P1 finding 1 verification.
//
// config.Validate can only refuse an absent, unparseable or zero-prefix
// ATOMICSITE_TRUSTED_PROXIES value. It cannot refuse a value that is
// well-formed but matches no real peer, and that value reproduces the exact
// login DoS the guard exists to prevent: TrustedProxyRealIP trusts nobody,
// every visitor collapses onto r.RemoteAddr, and a handful of failed logins
// from anywhere locks out every user for the lockout window, silently, with
// /api/health staying green. proxyTrust (wired in Router()) is the runtime
// detector that closes that gap, ported from hephaestus's observeProxyTrust.
func newHealthTestServer(t *testing.T, trustedProxies string) *Server {
	t.Helper()
	dir := t.TempDir()
	sqlDB, err := sql.Open("sqlite", dbpkg.DSN(filepath.Join(dir, "test.sqlite"), dbpkg.DefaultBusyTimeoutMs))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if _, err := sqlDB.Exec(string(dbpkg.Schema)); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	st, err := storage.NewLocalStore(filepath.Join(dir, "media"))
	if err != nil {
		t.Fatalf("storage: %v", err)
	}

	cfg := &config.Config{
		Env:            config.EnvDevelopment,
		BaseURL:        "http://localhost:8080",
		JWTSecret:      "test-jwt-secret-that-is-long-enough-for-hs256",
		DataDir:        dir,
		MaxUploadSize:  20 << 20,
		TrustedProxies: trustedProxies,
	}
	return New(cfg, sqlDB, store.New(sqlDB), st)
}

// frontedHealthRequest fires one /api/health request that looks like it
// came through a reverse proxy: a transport peer plus the X-Forwarded-For
// header that peer would have appended.
func frontedHealthRequest(t *testing.T, r http.Handler, peer string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.RemoteAddr = peer
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /api/health: %v (body %q)", err, rec.Body.String())
	}
	return body
}

// TestHealth_WrongButValidTrustedProxyIsDetected is the BEFORE/AFTER proof
// for finding 1. BEFORE this fix, config.go:495-517 accepted this exact
// value at boot (it is a well-formed, non-zero CIDR) and nothing downstream
// ever noticed it matched no real peer: /api/health stayed
// {"status":"ok", ...} forever. AFTER the fix, the first forwarded request
// from a peer outside the configured set flips /api/health to "warning"
// with a "proxy_trust" key an operator or an uptime check can see.
func TestHealth_WrongButValidTrustedProxyIsDetected(t *testing.T) {
	// The value from the verifier's repro: well-formed, non-zero, refused by
	// nothing in config.Validate, and matches no peer that actually fronts
	// this process.
	s := newHealthTestServer(t, "172.31.99.99/32")
	r := s.Router()

	// The real peer (a docker bridge gateway, say) is NOT inside
	// 172.31.99.99/32, so before this fix landed this request's XFF header
	// was silently ignored by TrustedProxyRealIP and /api/health had no way
	// to say so.
	body := frontedHealthRequest(t, r, "172.18.0.1:54321")

	if body["sqlite"] != "ok" {
		t.Fatalf("sanity: sqlite = %v, want ok (a failing DB would confound the proxy_trust assertion below)", body["sqlite"])
	}

	pt, ok := body["proxy_trust"].(map[string]any)
	if !ok {
		t.Fatalf("AFTER: /api/health did not report the mismatch for a configured-but-wrong "+
			"ATOMICSITE_TRUSTED_PROXIES value; body=%v (this is the exact silent-green failure "+
			"the finding describes: byte-identical to the `none` control)", body)
	}
	if pt["status"] != "mismatch" {
		t.Errorf("proxy_trust.status = %v, want %q", pt["status"], "mismatch")
	}
	if pt["since"] == nil || pt["since"] == "" {
		t.Errorf("proxy_trust.since is empty, an operator cannot tell how long this has been true: %v", pt)
	}
	// The unauthenticated body must say WHAT is wrong without handing an
	// anonymous caller the network topology (the peer address, or the
	// configured prefix list).
	if detail, _ := pt["detail"].(string); detail == "" {
		t.Errorf("proxy_trust.detail is empty")
	} else {
		for _, leak := range []string{"172.18.0.1", "172.31.99.99"} {
			if strings.Contains(detail, leak) {
				t.Errorf("proxy_trust.detail leaks an address (%s) into the unauthenticated body: %q", leak, detail)
			}
		}
	}
	if raw, _ := json.Marshal(body); strings.Contains(string(raw), "172.18.0.1") || strings.Contains(string(raw), "172.31.99.99") {
		t.Errorf("the full /api/health body leaks a peer/configured address: %s", raw)
	}

	if got := body["status"]; got != "warning" {
		t.Errorf("/api/health status = %v, want \"warning\" once rate limiting is globalised", got)
	}
}

// TestHealth_CorrectTrustedProxyStaysClean is the negative control, and the
// assertion that actually proves the detector: a detector that warns
// unconditionally would also pass the test above. With the deployed value
// matching the real peer, /api/health must report clean "ok" and no
// proxy_trust key on any number of requests.
func TestHealth_CorrectTrustedProxyStaysClean(t *testing.T) {
	s := newHealthTestServer(t, "172.18.0.1/32")
	r := s.Router()

	for i := 0; i < 5; i++ {
		body := frontedHealthRequest(t, r, "172.18.0.1:54321")
		if body["status"] != "ok" {
			t.Fatalf("request %d: status = %v, want \"ok\" for a CORRECT trusted-proxy value (false positive)", i, body["status"])
		}
		if _, ok := body["proxy_trust"]; ok {
			t.Fatalf("request %d: /api/health reported proxy_trust for a CORRECT configuration: %v", i, body)
		}
	}
}

// TestHealth_UnconfiguredTrustedProxiesStaysQuiet pins the deliberate scope:
// with TrustedProxies empty ("none" or unset, both parse to zero prefixes)
// there is no proxy in front by design, so a forwarding header from an
// arbitrary caller is expected and must not degrade health. Otherwise any
// anonymous visitor could flip an unconfigured instance's own health report
// by sending X-Forwarded-For.
func TestHealth_UnconfiguredTrustedProxiesStaysQuiet(t *testing.T) {
	s := newHealthTestServer(t, "")
	r := s.Router()

	body := frontedHealthRequest(t, r, "203.0.113.77:33333")
	if _, ok := body["proxy_trust"]; ok {
		t.Errorf("/api/health reported proxy_trust with no trusted-proxy set configured: %v", body)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %v, want ok", body["status"])
	}
}
