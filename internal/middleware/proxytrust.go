// Package middleware - proxytrust.go.
//
// ProxyTrustDetector watches live traffic for the failure mode Validate
// cannot see at boot: ATOMICSITE_TRUSTED_PROXIES is set to a value that
// parses cleanly (a real CIDR, non-zero prefixes) but matches no peer that
// actually fronts this process. That is not hypothetical: web-proxy or
// atomicsite_default gets recreated, docker hands out a different gateway
// from its pool, and the configured value rots between deploys with no
// error and no log line. From that moment TrustedProxyRealIP trusts nobody,
// every visitor collapses onto r.RemoteAddr (the proxy), and the per-IP
// login lockout becomes a global one: five bad logins from anywhere locks
// out every user for the lockout window, unauthenticated.
//
// Ported from hephaestus/pkg/web/clientaddr.go's observeProxyTrust /
// noteProxyTrust, adapted to this package's stateless-middleware shape
// (TrustedProxyRealIP is a package func closing over a prefix list rather
// than a method on a long-lived Server). The detector holds its own state
// so the same instance can be mounted as middleware AND queried by the
// /api/health handler.
package middleware

import (
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bright-interaction/slab/internal/config"
)

// ProxyTrustMismatch is the record of the first request that arrived with a
// forwarding header from a peer OUTSIDE the configured trusted-proxy set.
type ProxyTrustMismatch struct {
	// Peer is the transport address the forwarding header arrived from.
	// Intentionally NOT exposed by /api/health (unauthenticated): only the
	// server log gets it. Handing an anonymous caller the network topology
	// would be a different bug.
	Peer string
	// Header names which forwarding header was seen, so an operator can
	// tell a real proxy apart from a client inventing one.
	Header string
	// Prefixes is the configured set, for the log line.
	Prefixes []string
	SeenAt   time.Time
}

// forwardingHeaders are the headers a reverse proxy in front of this
// process adds. TrustedProxyRealIP only ever reads X-Forwarded-For and
// X-Real-Ip; Forwarded is included here too because its presence is
// equally good evidence that something is fronting us, and evidence is
// all this is used for.
var forwardingHeaders = []string{"X-Forwarded-For", "X-Real-Ip", "Forwarded"}

func forwardingHeaderName(h http.Header) string {
	for _, name := range forwardingHeaders {
		if strings.TrimSpace(h.Get(name)) != "" {
			return name
		}
	}
	return ""
}

// ProxyTrustDetector observes live traffic for a configured-but-wrong
// trusted-proxy set. Deliberately scoped to a NON-EMPTY configured set:
// with the set empty (ATOMICSITE_TRUSTED_PROXIES=none, or unset in a
// non-production build) there is no proxy in front by design, forwarding
// headers from arbitrary clients are expected, and firing here would be a
// nuisance signal any caller could raise on demand.
type ProxyTrustDetector struct {
	prefixes   []netip.Prefix
	prefixStrs []string
	mismatch   atomic.Pointer[ProxyTrustMismatch]
}

// NewProxyTrustDetector builds a detector from the raw ATOMICSITE_TRUSTED_PROXIES
// value, using the same parser TrustedProxyRealIP and the boot-time guard
// share (config.ParseTrustedProxies), so the detector can never disagree
// with the middleware about what counts as trusted.
func NewProxyTrustDetector(trustedRaw string) *ProxyTrustDetector {
	prefixes, _ := config.ParseTrustedProxies(trustedRaw)
	strs := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		strs = append(strs, p.String())
	}
	return &ProxyTrustDetector{prefixes: prefixes, prefixStrs: strs}
}

// isTrusted reports whether addr is inside the configured set.
func (d *ProxyTrustDetector) isTrusted(addr netip.Addr) bool {
	for _, p := range d.prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// Middleware returns root-router middleware that watches every request for
// the mismatch. A no-op wrapper when the detector has zero configured
// prefixes, mirroring TrustedProxyRealIP's own empty-list passthrough so
// mounting this always costs nothing on an unconfigured or opted-out
// deployment.
func (d *ProxyTrustDetector) Middleware(next http.Handler) http.Handler {
	if d == nil || len(d.prefixes) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.observe(r)
		next.ServeHTTP(w, r)
	})
}

// observe records and logs the mismatch ONCE per process.
func (d *ProxyTrustDetector) observe(r *http.Request) {
	if r == nil {
		return
	}
	// Once, not per request: the common case is the fast pointer load.
	if d.mismatch.Load() != nil {
		return
	}
	header := forwardingHeaderName(r.Header)
	if header == "" {
		return
	}
	peerHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peerHost = r.RemoteAddr
	}
	peerHost = strings.Trim(peerHost, "[]")
	if peer, err := netip.ParseAddr(peerHost); err == nil && d.isTrusted(peer.Unmap()) {
		return
	}
	m := &ProxyTrustMismatch{
		Peer:     peerHost,
		Header:   header,
		Prefixes: d.prefixStrs,
		SeenAt:   time.Now().UTC(),
	}
	if !d.mismatch.CompareAndSwap(nil, m) {
		// Another goroutine won the race and has already logged.
		return
	}
	slog.Warn("trusted proxy set matches nothing: a forwarding header arrived from a peer OUTSIDE "+
		"ATOMICSITE_TRUSTED_PROXIES, so this process resolves every caller to the transport peer. "+
		"Consequence: every per-IP control keys on ONE address (the login lockout, the MFA throttle, "+
		"the five /t/* limiters, the form-spam gate, the admin-write bucket), so a handful of failed "+
		"logins from anywhere locks out every user for the lockout window.",
		"env", "ATOMICSITE_TRUSTED_PROXIES",
		"peer", peerHost,
		"header", header,
		"configured_prefixes", strings.Join(m.Prefixes, ","),
		"fix", "set ATOMICSITE_TRUSTED_PROXIES to the prefix containing the real proxy peer "+
			"(re-check it after the proxy container is recreated: the address changes)")
}

// Mismatch reports whether a forwarding header has been seen from an
// untrusted peer since boot, and the record if so. Consumed by
// GET /api/health so an operator can see "my rate limiting is globalised"
// without reading logs. The peer address in the returned record must stay
// out of the unauthenticated health body; callers may log it but must not
// echo it back over HTTP.
func (d *ProxyTrustDetector) Mismatch() (ProxyTrustMismatch, bool) {
	if d == nil {
		return ProxyTrustMismatch{}, false
	}
	m := d.mismatch.Load()
	if m == nil {
		return ProxyTrustMismatch{}, false
	}
	return *m, true
}
