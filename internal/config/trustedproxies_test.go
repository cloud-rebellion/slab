package config

import (
	"net/netip"
	"strings"
	"testing"
)

// P1 verification 2026-08-13, blocker 2.
//
// The 2026-08-11 fix made clientIP read r.RemoteAddr only, which is
// correct: the forwarding headers were caller-chosen and every per-IP
// control in the product keyed off them. What shipped with it was a
// production topology where nothing sets ATOMICSITE_TRUSTED_PROXIES:
// compose binds 127.0.0.1, the deploy did not seed it, .env.example
// leaves it commented and the default is empty. Behind the host nginx
// that makes r.RemoteAddr the docker bridge gateway for EVERY visitor,
// so 500 distinct visitors become one limiter key and one attacker's
// failed logins lock out everyone for the whole lockout window. The
// header-spoofing bug traded for an unauthenticated global login DoS.
//
// The deploy now seeds the gateway (hephaestus/userworkflows/
// deploy_atomicsite.go, ensure-trusted-proxies). This is the guard that
// makes the seed non-optional: a production deployment that never
// declares its trust boundary refuses to start instead of collapsing
// every visitor into one bucket and finding out in the incident channel.

// prodBase is a Config that passes every OTHER production guard, so a
// failure here is unambiguously about the trusted-proxy boundary.
func prodBase() *Config {
	return &Config{
		Env:           EnvProduction,
		BaseURL:       "https://app.example.com",
		JWTSecret:     "this-is-a-strong-32-byte-secret-value-here",
		AnalyticsSalt: "this-is-a-random-32-byte-fingerprint-salt-value",
		ShieldKey:     "0123456789abcdef0123456789abcdef",
		PrimaryDomain: "example.com",
	}
}

func TestTrustedProxies_ProductionMustDeclareTheBoundary(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"unset is refused", "", true},
		{"whitespace only is refused", "   ", true},
		{"explicit none is accepted", "none", false},
		{"none is case insensitive", "  NONE  ", false},
		{"a bridge gateway is accepted", "172.18.0.1/32", false},
		{"several gateways are accepted", "172.18.0.1/32,10.0.6.1/32", false},
		{"a bare IP is accepted", "10.0.6.1", false},
		{"an IPv6 gateway is accepted", "fd00::1/128", false},
		// A typo has to be as loud as an absence: it parses to zero
		// prefixes, the middleware then trusts nobody, and the runtime
		// behaviour is byte-for-byte the collapsed-bucket outage.
		{"an unparseable entry is refused", "172.16.0.0/12x", true},
		{"a quoted value is refused", `"172.18.0.1/32"`, true},
		{"none mixed into a list is refused", "none,172.18.0.1/32", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := prodBase()
			cfg.TrustedProxies = tc.value
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("ATOMICSITE_TRUSTED_PROXIES=%q must refuse to start in production; got nil. "+
					"Every visitor behind the proxy would share one rate-limit key and one failed-login "+
					"burst would lock out every user for the lockout window", tc.value)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ATOMICSITE_TRUSTED_PROXIES=%q must be accepted; got %v", tc.value, err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "ATOMICSITE_TRUSTED_PROXIES") {
				t.Fatalf("refusal must name the variable so the operator knows what to set; got %v", err)
			}
		})
	}
}

// Local dev has to stay a one-liner. ATOMICSITE_ENV=development opts out
// of this guard the same way it opts out of the secret guards; a
// contributor running `make dev` or `go test` sets nothing.
func TestTrustedProxies_DevelopmentStaysErgonomic(t *testing.T) {
	cfg := &Config{Env: EnvDevelopment}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("development with nothing set must validate; got %v", err)
	}
}

// The collapse itself, demonstrated rather than asserted from a comment.
// This is the verifier's probe: 500 distinct visitors arriving through
// one reverse proxy. With no trusted-proxy list the peer address is the
// only thing left after the clientIP fix, so they share one key; with
// the gateway declared, the forwarding header resolves and they get 500.
//
// The test does not need the limiter to prove it: the number of distinct
// keys IS the property, and one key for 500 visitors is the outage.
func TestTrustedProxies_UnsetCollapsesEveryVisitorIntoOneKey(t *testing.T) {
	const visitors = 500
	gateway := netip.MustParseAddr("172.18.0.1")

	// resolve mimics the production chain: TrustedProxyRealIP only
	// rewrites RemoteAddr when the peer is trusted, and handlers.clientIP
	// then reads RemoteAddr and nothing else.
	resolve := func(trustedRaw, forwardedFor string) string {
		prefixes, _ := ParseTrustedProxies(trustedRaw)
		for _, p := range prefixes {
			if p.Contains(gateway) {
				return forwardedFor
			}
		}
		return gateway.String()
	}

	count := func(trustedRaw string) int {
		keys := map[string]bool{}
		for i := 0; i < visitors; i++ {
			// One distinct public visitor address per iteration, the way
			// nginx would present it in X-Forwarded-For.
			visitor := netip.AddrFrom4([4]byte{198, 51, byte(i >> 8), byte(i)}).String()
			keys[resolve(trustedRaw, visitor)] = true
		}
		return len(keys)
	}

	if got := count(""); got != 1 {
		t.Fatalf("unset trusted proxies: %d distinct keys for %d visitors, want 1 "+
			"(the premise of the guard has changed; re-check the fix)", got, visitors)
	}
	if got := count("172.18.0.1/32"); got != visitors {
		t.Fatalf("gateway declared: %d distinct keys for %d visitors, want %d", got, visitors, visitors)
	}

	// And the configuration that produces the collapse cannot ship.
	cfg := prodBase()
	cfg.TrustedProxies = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("the collapsing configuration booted; production must refuse it")
	}
}

// The runtime parser and the boot guard have to agree. They are one
// function for that reason: a second copy is how a value the guard
// accepts and the middleware silently discards gets shipped.
func TestParseTrustedProxies(t *testing.T) {
	cases := []struct {
		raw       string
		wantCount int
		wantBad   int
	}{
		{"", 0, 0},
		{"none", 0, 0},
		{"10.0.6.1", 1, 0},
		{"10.0.6.1/32, 172.18.0.1/32", 2, 0},
		{"::1", 1, 0},
		{"10.0.6.1,,10.0.6.2", 2, 0},
		{"garbage", 0, 1},
		{"10.0.6.1, garbage", 1, 1},
	}
	for _, tc := range cases {
		prefixes, bad := ParseTrustedProxies(tc.raw)
		if len(prefixes) != tc.wantCount || len(bad) != tc.wantBad {
			t.Errorf("ParseTrustedProxies(%q) = %d prefixes, %d bad; want %d, %d",
				tc.raw, len(prefixes), len(bad), tc.wantCount, tc.wantBad)
		}
	}
	// Bare IPs widen to a host route, not a network.
	prefixes, _ := ParseTrustedProxies("10.0.6.1")
	if got := prefixes[0].Bits(); got != 32 {
		t.Errorf("bare IPv4 widened to /%d, want /32", got)
	}
	if !prefixes[0].Contains(netip.MustParseAddr("10.0.6.1")) {
		t.Error("bare IPv4 prefix does not contain its own address")
	}
	if prefixes[0].Contains(netip.MustParseAddr("10.0.6.2")) {
		t.Error("bare IPv4 prefix must not contain a neighbour; that would trust other containers")
	}
}
