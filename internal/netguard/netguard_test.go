package netguard

import (
	"net"
	"testing"
)

func TestIsForbidden(t *testing.T) {
	tests := []struct {
		ip     string
		want   bool
		reason string
	}{
		// The hole this package exists to close. These two are the estate's
		// real tailnet addresses (they are in scripts/mirror-redactions.txt
		// for exactly that reason) and the old predicate allowed both.
		{"100.100.100.100", true, "estate tailnet host"},
		{"100.100.100.101", true, "estate tailnet host"},
		{"100.64.0.1", true, "CGNAT range start"},
		{"100.127.255.254", true, "CGNAT range end"},
		{"100.63.255.255", false, "just below CGNAT, public"},
		{"100.128.0.1", false, "just above CGNAT, public"},

		// NAT64: another spelling of loopback the old predicate allowed.
		{"64:ff9b::7f00:1", true, "NAT64 wrapping 127.0.0.1"},
		{"64:ff9b::a00:1", true, "NAT64 wrapping 10.0.0.1"},

		// Ranges the old predicate did catch, which must stay caught.
		{"127.0.0.1", true, "loopback"},
		{"::1", true, "IPv6 loopback"},
		{"::ffff:127.0.0.1", true, "v4-mapped loopback"},
		{"10.0.0.1", true, "RFC1918"},
		{"172.16.0.1", true, "RFC1918"},
		{"192.168.1.1", true, "RFC1918"},
		{"169.254.169.254", true, "cloud metadata"},
		{"0.0.0.0", true, "unspecified"},
		{"::", true, "IPv6 unspecified"},
		{"fd00::1", true, "IPv6 unique local"},
		{"fe80::1", true, "IPv6 link-local"},
		{"224.0.0.1", true, "multicast"},
		{"ff02::1", true, "IPv6 multicast"},

		// Tunnel encapsulations that can carry an arbitrary inner target.
		{"2002::1", true, "6to4"},
		{"2001::1", true, "Teredo"},

		// Special-purpose ranges that are never a real destination.
		{"192.0.0.1", true, "IETF protocol assignments"},
		{"198.18.0.1", true, "benchmarking"},

		// Documentation ranges stay ALLOWED on purpose: they reach nothing
		// internal, and they are how tests across this repo spell "a public
		// address" (webhook/ssrf_test.go uses 203.0.113.10). Denying them
		// would break those tests and buy no security.
		{"192.0.2.1", false, "documentation, allowed by design"},
		{"203.0.113.10", false, "documentation, used as public in webhook tests"},
		{"2001:db8::1", false, "IPv6 documentation, allowed by design"},

		// Genuine public addresses must still be reachable, or the guard has
		// simply broken the feature.
		{"1.1.1.1", false, "public DNS"},
		{"8.8.8.8", false, "public DNS"},
		{"93.184.216.34", false, "public web host"},
		{"2606:4700:4700::1111", false, "public IPv6 DNS"},
	}
	for _, tt := range tests {
		ip := net.ParseIP(tt.ip)
		if ip == nil {
			t.Fatalf("test bug: %q did not parse", tt.ip)
		}
		if got := IsForbidden(ip); got != tt.want {
			t.Errorf("IsForbidden(%s) = %v, want %v (%s)", tt.ip, got, tt.want, tt.reason)
		}
	}
}

// Fail closed on anything we cannot interpret, rather than treating an
// unparseable address as safe.
func TestIsForbiddenFailsClosedOnGarbage(t *testing.T) {
	if !IsForbidden(nil) {
		t.Error("nil IP was allowed")
	}
	if !IsForbidden(net.IP{1, 2, 3}) {
		t.Error("a malformed 3-byte IP was allowed")
	}
	if !IsForbidden(net.IP{}) {
		t.Error("an empty IP was allowed")
	}
}
