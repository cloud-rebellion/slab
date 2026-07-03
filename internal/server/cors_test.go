package server

import (
	"testing"

	"github.com/bright-interaction/slab/internal/config"
)

func prodCfg() *config.Config {
	return &config.Config{
		BaseURL:         "https://atomicsite.example.com",
		BuiltSiteSuffix: ".atomicsite.example.com",
	}
}

// TestCORS_AdminRoutesRejectBuiltSiteOrigins is the audit C2 regression
// guard: a built-tenant subdomain must NOT be allowed credentialed CORS
// to admin routes. Pre-fix this returned true.
func TestCORS_AdminRoutesRejectBuiltSiteOrigins(t *testing.T) {
	cfg := prodCfg()
	got := isAllowedOriginForPath(cfg, "https://malicious-tenant.atomicsite.example.com", "/api/sites/abc/blocks")
	if got {
		t.Error("admin route /api/sites/... must NOT accept built-site subdomain origins (C2 regression)")
	}
}

func TestCORS_PublicVisitorRoutesAllowBuiltSiteOrigins(t *testing.T) {
	cfg := prodCfg()
	for _, path := range []string{"/t/consent", "/t/visitor", "/t/inbound", "/t/pageview"} {
		got := isAllowedOriginForPath(cfg, "https://tenant-foo.atomicsite.example.com", path)
		if !got {
			t.Errorf("public visitor route %s must accept built-site subdomain", path)
		}
	}
}

func TestCORS_BaseURLAlwaysAccepted(t *testing.T) {
	cfg := prodCfg()
	for _, path := range []string{"/api/sites/abc", "/t/consent", "/api/health"} {
		if !isAllowedOriginForPath(cfg, cfg.BaseURL, path) {
			t.Errorf("BaseURL %q must be accepted on every path; failed at %s", cfg.BaseURL, path)
		}
	}
}

func TestCORS_RandomOriginsRejected(t *testing.T) {
	cfg := prodCfg()
	for _, origin := range []string{
		"https://attacker.example.com",
		"https://atomicsite.example.com.attacker.com", // suffix trick
		"http://atomicsite.example.com",               // wrong scheme
		"https://otherproduct.com",
	} {
		for _, path := range []string{"/api/sites/abc", "/t/consent"} {
			if isAllowedOriginForPath(cfg, origin, path) {
				t.Errorf("origin %q must be rejected on %s", origin, path)
			}
		}
	}
}

func TestCORS_LocalhostInDev(t *testing.T) {
	cfg := &config.Config{
		BaseURL:         "http://localhost:8080",
		BuiltSiteSuffix: ".atomicsite.example.com",
	}
	for _, origin := range []string{
		"http://localhost:5173", // Vite dev server
		"http://localhost:5174",
		"http://127.0.0.1:5173",
	} {
		if !isAllowedOriginForPath(cfg, origin, "/api/sites/abc") {
			t.Errorf("dev mode must accept localhost origin %q on admin routes", origin)
		}
	}
}

func TestStripScheme(t *testing.T) {
	cases := map[string]string{
		"https://foo.example":          "foo.example",
		"https://foo.example:443":      "foo.example",
		"https://foo.example/path":     "foo.example",
		"http://foo.example:8080/path": "foo.example",
		"foo.example":                  "foo.example",
	}
	for in, want := range cases {
		if got := stripScheme(in); got != want {
			t.Errorf("stripScheme(%q) = %q, want %q", in, got, want)
		}
	}
}
