package domains

import (
	"net"
	"syscall"

	"github.com/bright-interaction/slab/internal/netguard"
)

// isPrivateIP reports whether ip is loopback, link-local, multicast,
// unspecified, or in a private (RFC1918 / IPv6 ULA) range. A custom-domain
// probe must never be allowed to reach such an address: a tenant could point
// their domain's A record at 127.0.0.1 or 169.254.169.254 (cloud metadata) and
// turn the verifier into an internal-resource fetcher (SSRF).
// isPrivateIP delegates to netguard, which is the single definition of "an
// address a tenant-supplied URL must not reach". Four packages used to carry
// their own copy of this predicate, and all four shared one hole: net.IP's
// IsPrivate covers RFC1918 and fc00::/7 but NOT RFC 6598 carrier-grade NAT,
// 100.64.0.0/10, which is the Tailscale range this estate actually runs on.
// Keep this a thin wrapper; do not reintroduce a local range list.
func isPrivateIP(ip net.IP) bool {
	return netguard.IsForbidden(ip)
}

// ssrfDialControl is a net.Dialer Control callback that refuses to connect to a
// private/loopback/link-local address. It runs against the ACTUAL resolved IP
// immediately before connect, so it defends DNS rebinding: a hostname whose A
// record resolves to an internal IP is rejected at connect time, not just at a
// pre-flight validation step. Mirrors webhook.ssrfSafeClient and
// handlers.fetchWithSSRFGuard so every tenant-controlled fetch shares one shape.
// It delegates to netguard.DialControl rather than carrying the check. The
// copy this replaced was `ip != nil && isPrivateIP(ip)`, which FAILS OPEN on
// any address net.ParseIP cannot parse, and a zone suffix is exactly that:
// ParseIP("::ffff:127.0.0.1%lo0") is nil. Measured in the sibling copy in
// internal/webhook: http://[::ffff:127.0.0.1%25lo0]:PORT/ returned 200 from an
// ordinary IPv4 loopback service while plain 127.0.0.1 was refused. Keep this
// a thin wrapper; do not reintroduce a local ParseIP check.
func ssrfDialControl(network string, address string, rc syscall.RawConn) error {
	return netguard.DialControl(network, address, rc)
}
