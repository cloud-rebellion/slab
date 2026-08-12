// Package middleware - realip.go.
//
// TrustedProxyRealIP returns a chi-style middleware that rewrites
// r.RemoteAddr to the upstream client IP when (and only when) the
// immediate TCP peer is in the configured trusted-proxy list. Every
// downstream consumer (audit logs, rate limiters, IP-throttle keys)
// reads r.RemoteAddr, so this is the single hop where XFF / X-Real-IP
// gets either honored or discarded.
//
// Why not chi.middleware.RealIP: chi's stock RealIP honours
// X-Forwarded-For unconditionally, which lets any direct caller spoof
// the audit-log IP by setting the header. The fix is to make trust
// boundary configuration explicit (ATOMICSITE_TRUSTED_PROXIES) and to
// fall back to r.RemoteAddr when the peer isn't trusted.
//
// XFF parsing walks right-to-left: the rightmost entry is the proxy
// closest to us; we strip every trusted hop and report the first
// untrusted IP as the client. This handles CDN -> LB -> app chains
// correctly. If every entry is trusted (e.g. a bug in the proxy
// chain), the leftmost is used as a best-guess fallback.

package middleware

import (
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/bright-interaction/slab/internal/config"
)

// TrustedProxyRealIP builds the middleware. trustedRaw is the raw
// comma-separated CIDR/IP list from config; parsing happens once here.
// Empty string disables the middleware entirely (returns a no-op).
func TrustedProxyRealIP(trustedRaw string) func(http.Handler) http.Handler {
	prefixes := parseTrustedProxies(trustedRaw)
	if len(prefixes) == 0 {
		// No trust configured: pass-through. r.RemoteAddr stays as the
		// actual TCP peer; XFF / X-Real-IP are ignored downstream.
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			peer := remoteAddrIP(r.RemoteAddr)
			if peer.IsValid() && isTrusted(peer, prefixes) {
				if client := resolveClientIP(r, prefixes); client != "" {
					r.RemoteAddr = client
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// parseTrustedProxies delegates to config.ParseTrustedProxies, which is
// the single parser shared with the boot-time guard in config.Validate.
// Unparseable entries are dropped here (they never trusted anything);
// production refuses to start on them, so the runtime only ever sees a
// list an operator already had to get right.
func parseTrustedProxies(raw string) []netip.Prefix {
	prefixes, _ := config.ParseTrustedProxies(raw)
	return prefixes
}

func isTrusted(addr netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// remoteAddrIP parses r.RemoteAddr into a netip.Addr. RemoteAddr is
// always "host:port" for TCP and net/http; SplitHostPort strips the
// port, then ParseAddr handles bare v4 / v6.
func remoteAddrIP(remote string) netip.Addr {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return addr.Unmap()
}

// resolveClientIP walks XFF right-to-left to find the leftmost
// untrusted hop. Falls back to X-Real-IP if XFF is absent. Returns the
// resolved IP as "ip:port" so net/http callers downstream parse it
// uniformly (port is "0" since we don't know the original client port).
//
// Multiple X-Forwarded-For / X-Real-IP headers in a single request are
// treated as one combined chain (concatenated with comma separators)
// per RFC 7230. Go's net/http already canonicalizes folded headers
// this way via Header.Values + Get, but we use Values explicitly so a
// client that sends `X-Forwarded-For: a` + `X-Forwarded-For: b` can't
// hide a hop from the trust-walk. Same defense for X-Real-IP.
func resolveClientIP(r *http.Request, trusted []netip.Prefix) string {
	xff := strings.TrimSpace(joinHeaderValues(r.Header.Values("X-Forwarded-For")))
	if xff != "" {
		entries := strings.Split(xff, ",")
		for i := len(entries) - 1; i >= 0; i-- {
			candidate := strings.TrimSpace(entries[i])
			if candidate == "" {
				continue
			}
			addr, err := netip.ParseAddr(candidate)
			if err != nil {
				continue
			}
			if !isTrusted(addr.Unmap(), trusted) {
				return net.JoinHostPort(candidate, "0")
			}
		}
		// Every hop trusted: take the leftmost as best-effort.
		if first := strings.TrimSpace(entries[0]); first != "" {
			if _, err := netip.ParseAddr(first); err == nil {
				return net.JoinHostPort(first, "0")
			}
		}
	}
	// X-Real-IP fallback. Multiple values get joined the same way; a
	// well-behaved proxy sets one value, but we don't trust the client
	// to be well-behaved.
	realVals := r.Header.Values("X-Real-IP")
	for _, v := range realVals {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, err := netip.ParseAddr(v); err == nil {
			return net.JoinHostPort(v, "0")
		}
	}
	return ""
}

// joinHeaderValues concatenates multiple header values with ", " so
// downstream comma-split sees every value, even when a client sent
// the same header multiple times rather than as a folded list.
func joinHeaderValues(values []string) string {
	switch len(values) {
	case 0:
		return ""
	case 1:
		return values[0]
	default:
		return strings.Join(values, ", ")
	}
}
