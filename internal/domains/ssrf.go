package domains

import (
	"fmt"
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
func ssrfDialControl(_ /*network*/ string, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip != nil && isPrivateIP(ip) {
		return fmt.Errorf("refusing to connect to private address %s", ip)
	}
	return nil
}
