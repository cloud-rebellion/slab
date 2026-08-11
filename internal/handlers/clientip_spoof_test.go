package handlers

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"
)

// Audit 2026-08-11 finding 9 regression guard.
//
// handlers.clientIP used to prefer CF-Connecting-IP, then
// X-Forwarded-For, and only fell through to r.RemoteAddr when both were
// absent. Cloudflare is DNS-only in front of the production host, so
// nothing strips a client-set CF-Connecting-IP and every key derived
// from this function was chosen by the caller: the login lockout, the
// MFA limiter, the waitlist cap, the form-spam throttle, all five /t/*
// limiters, and the ip_hash written into the GDPR consent proof.
//
// authmw.TrustedProxyRealIP is the one hop allowed to honour those
// headers, and it does so only when the immediate TCP peer is trusted.
// These tests assert the handlers-side helper never second-guesses it.

// TestClientIPIgnoresSpoofableHeaders is the direct assertion: whatever
// the caller sends, the answer is the peer.
func TestClientIPIgnoresSpoofableHeaders(t *testing.T) {
	const peer = "203.0.113.9"

	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"no headers", nil, peer},
		{"cf-connecting-ip", map[string]string{"CF-Connecting-IP": "10.1.2.3"}, peer},
		{"x-forwarded-for", map[string]string{"X-Forwarded-For": "10.1.2.3"}, peer},
		{"xff chain", map[string]string{"X-Forwarded-For": "10.1.2.3, 10.4.5.6"}, peer},
		{"x-real-ip", map[string]string{"X-Real-IP": "10.1.2.3"}, peer},
		{"all at once", map[string]string{
			"CF-Connecting-IP": "10.1.2.3",
			"X-Forwarded-For":  "10.4.5.6",
			"X-Real-IP":        "10.7.8.9",
		}, peer},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/t/consent", nil)
			r.RemoteAddr = peer + ":44444"
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			if got := clientIP(r); got != tc.want {
				t.Fatalf("clientIP() = %q, want %q (headers must not be trusted here; "+
					"authmw.TrustedProxyRealIP already resolved RemoteAddr)", got, tc.want)
			}
			// The password-reset forensic IP is the twin of the same
			// helper and carried the identical header chain.
			if got := clientIPForReset(r); got != tc.want {
				t.Fatalf("clientIPForReset() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClientIPParsesIPv6Peer guards the port-stripping, which the old
// implementation did with a naive LastIndex(":") that would have cut an
// IPv6 literal in the middle had it ever reached that branch.
func TestClientIPParsesIPv6Peer(t *testing.T) {
	r := httptest.NewRequest("POST", "/t/consent", nil)
	r.RemoteAddr = "[2001:db8::1]:44444"
	if got, want := clientIP(r), "2001:db8::1"; got != want {
		t.Fatalf("clientIP() = %q, want %q", got, want)
	}
}

// TestSpoofedHeaderCannotEvadeRateLimiter is the audit's executed repro
// turned into an assertion. Before the fix this printed
// "requests=50000 denied=0 buckets_retained=50000": a credential spray
// or consent flood from one host never tripped the per-IP bucket and
// left one permanent map entry per forged header value.
//
// After the fix every request from the same peer shares one bucket, so
// the limiter denies everything past the burst and the map holds
// exactly one key.
func TestSpoofedHeaderCannotEvadeRateLimiter(t *testing.T) {
	const (
		requests = 50000
		capacity = 20
	)
	rl := newConsentRateLimiter(capacity, 20.0/60.0, 10*time.Minute)

	denied := 0
	for i := 0; i < requests; i++ {
		r := httptest.NewRequest("POST", "/t/consent", nil)
		r.RemoteAddr = "203.0.113.9:44444"
		// A rotating attacker-supplied header, one distinct value per
		// request. This is free to send and needs no infrastructure.
		r.Header.Set("CF-Connecting-IP", fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255))
		if !rl.allow(clientIP(r)) {
			denied++
		}
	}

	rl.mu.Lock()
	buckets := len(rl.buckets)
	rl.mu.Unlock()

	t.Logf("requests=%d denied=%d buckets_retained=%d", requests, denied, buckets)

	if denied == 0 {
		t.Fatalf("rotating CF-Connecting-IP evaded the limiter entirely: %d requests, 0 denied", requests)
	}
	// The burst plus whatever refilled over the loop's wall time; the
	// overwhelming majority must be rejected.
	if denied < requests-1000 {
		t.Fatalf("only %d of %d requests denied; the header still splits the bucket", denied, requests)
	}
	if buckets != 1 {
		t.Fatalf("buckets_retained = %d, want 1; a caller-chosen key is still creating map entries", buckets)
	}
}
