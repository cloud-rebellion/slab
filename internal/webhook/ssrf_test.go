package webhook

import (
	"net"
	"testing"
)

// The delivery-logic tests in this package POST to httptest servers on
// loopback, which the SSRF guard would otherwise reject. Enable loopback for
// the test binary only (this init runs nowhere in production).
func init() { allowPrivateTargets = true }

func TestIsPrivateIP(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1":       true,  // loopback
		"::1":             true,  // loopback v6
		"169.254.169.254": true,  // link-local (cloud metadata)
		"10.0.0.5":        true,  // RFC1918
		"192.168.1.1":     true,  // RFC1918
		"172.16.0.1":      true,  // RFC1918
		"0.0.0.0":         true,  // unspecified
		"fd00::1":         true,  // IPv6 ULA
		"8.8.8.8":         false, // public
		"203.0.113.10":    false, // public (RFC5737 documentation range)
		"1.1.1.1":         false, // public
	}
	for s, want := range cases {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test ip %q", s)
		}
		if got := isPrivateIP(ip); got != want {
			t.Errorf("isPrivateIP(%s) = %v, want %v", s, got, want)
		}
	}
}
