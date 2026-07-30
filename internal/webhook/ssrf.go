package webhook

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"

	"github.com/bright-interaction/slab/internal/netguard"
)

// allowPrivateTargets disables the private-IP rejection. It is false in
// production and set to true only by the in-package tests (which deliver to
// httptest servers on loopback). Never flipped outside _test.go.
var allowPrivateTargets = false

// isPrivateIP reports whether ip is loopback, link-local, multicast,
// unspecified, or in a private (RFC1918 / IPv6 ULA) range. Such addresses must
// never be the target of a tenant-controlled webhook delivery.
// isPrivateIP delegates to netguard, which is the single definition of "an
// address a tenant-supplied URL must not reach". Four packages used to carry
// their own copy of this predicate, and all four shared one hole: net.IP's
// IsPrivate covers RFC1918 and fc00::/7 but NOT RFC 6598 carrier-grade NAT,
// 100.64.0.0/10, which is the Tailscale range this estate actually runs on.
// Keep this a thin wrapper; do not reintroduce a local range list.
func isPrivateIP(ip net.IP) bool {
	return netguard.IsForbidden(ip)
}

// ssrfSafeClient returns an http.Client whose dialer refuses to connect to a
// private/loopback/link-local address. The check runs in the dialer Control
// callback, which receives the ACTUAL resolved IP immediately before connect,
// so it defends DNS-rebinding (a tenant webhook target whose A record resolves
// to 169.254.169.254 / 10.x is rejected at connect time, not just at validation).
// Redirects are capped at 3 and re-validated by the same dialer on each hop.
func ssrfSafeClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip != nil && !allowPrivateTargets && isPrivateIP(ip) {
				return fmt.Errorf("refusing to connect to private address %s", ip)
			}
			return nil
		},
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{DialContext: dialer.DialContext},
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("stopped after 3 redirects")
			}
			return nil
		},
	}
}
