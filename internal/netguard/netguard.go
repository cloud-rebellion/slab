// Package netguard decides whether an IP address is safe to let a
// tenant-supplied URL reach.
//
// It exists because four packages had grown their own copy of the same
// predicate (migration/safefetch.go, webhook/ssrf.go, domains/ssrf.go and
// handlers/media.go), all four spelled:
//
//	ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
//	    ip.IsMulticast() || ip.IsUnspecified() || ip.IsPrivate()
//
// and all four therefore shared one hole: net.IP.IsPrivate reports only
// RFC1918 and fc00::/7. It does NOT cover RFC 6598 carrier-grade NAT,
// 100.64.0.0/10, which is the Tailscale range. This estate runs Tailscale, so
// that range is live internal infrastructure and every one of those guards
// waved it through. Nor did they cover NAT64 (64:ff9b::/96), where
// 64:ff9b::7f00:1 is a spelling of 127.0.0.1.
//
// The shape is deliberately inverted from what it replaced. Those predicates
// were denylists that had to enumerate every bad range, and a missing entry
// failed open. This requires a global unicast address FIRST and then applies
// an explicit deny table, so an address family or special-purpose range
// nobody thought about is refused by default rather than permitted by default.
package netguard

import (
	"net"
	"net/netip"
)

// deniedPrefixes are ranges that are never a legitimate destination for a
// tenant-supplied URL. IsGlobalUnicast already rejects loopback, link-local,
// multicast and the unspecified address; these are the ranges it does not.
var deniedPrefixes = []netip.Prefix{
	// RFC1918 private
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	// RFC 6598 carrier-grade NAT. This is the Tailscale range and the hole
	// that motivated this package.
	netip.MustParsePrefix("100.64.0.0/10"),
	// Loopback and "this host" (belt and braces alongside IsGlobalUnicast)
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("0.0.0.0/8"),
	// Link-local, including the cloud metadata address 169.254.169.254
	netip.MustParsePrefix("169.254.0.0/16"),
	// IETF protocol assignments and benchmarking
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	// Deliberately NOT denied: the RFC 5737 / RFC 3849 documentation ranges
	// (192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24, 2001:db8::/32). They
	// reach no internal infrastructure, so denying them buys no security, and
	// they are the conventional way to spell "some public address" in a test.
	// Denying them just breaks those tests for nothing.
	// IPv6
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),  // unique local
	netip.MustParsePrefix("fe80::/10"), // link-local
	// NAT64: 64:ff9b::7f00:1 is 127.0.0.1 wearing a different hat
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	// 6to4 and Teredo can encapsulate an arbitrary inner destination
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("2001::/32"),
}

// IsForbidden reports whether ip must not be dialled for a tenant-supplied
// URL. It fails closed: an address it cannot make sense of is forbidden.
func IsForbidden(ip net.IP) bool {
	if ip == nil {
		return true
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	return IsForbiddenAddr(addr)
}

// IsForbiddenAddr is IsForbidden for a netip.Addr.
func IsForbiddenAddr(addr netip.Addr) bool {
	if !addr.IsValid() {
		return true
	}
	// Collapse ::ffff:127.0.0.1 to 127.0.0.1 so a v4-mapped address cannot
	// dodge the v4 prefixes below.
	addr = addr.Unmap()
	// Allowlist first: anything that is not global unicast is out. This is
	// what makes an unlisted special-purpose range fail closed.
	if !addr.IsGlobalUnicast() {
		return true
	}
	// IsGlobalUnicast is true for ULA and for 100.64/10, so the deny table
	// still has work to do.
	for _, p := range deniedPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
