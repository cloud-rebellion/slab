package netguard

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// This file is the reusable half of netguard: the IP predicate in netguard.go
// answers "may this address be dialled", and the helpers here are the two
// places every caller has to ask it.
//
// A tenant-supplied URL needs BOTH, and neither is sufficient alone:
//
//   - CheckHostPublic is the pre-flight. It catches a literal private IP and
//     "localhost" without a DNS round trip, and rejects a hostname whose A/AAAA
//     records point inside. On its own it is a TOCTOU: the transport resolves
//     DNS again when it dials, so a record with a ~0s TTL can answer public for
//     the pre-flight and private for the dial (DNS rebinding).
//   - DialControl closes that gap. net.Dialer.Control runs after resolution and
//     immediately before connect, against the address actually being dialled,
//     on the initial request AND on every redirect hop.
//
// migration/safefetch.go, webhook/ssrf.go, domains/ssrf.go and handlers/media.go
// each grew their own copy of this pair before this file existed. New callers
// should use these instead of writing a fifth.

// dialTimeout and keepAlive match the estate's other guarded transports.
const (
	dialTimeout = 10 * time.Second
	keepAlive   = 30 * time.Second
)

// CheckHostPublic reports whether host is safe to send a tenant-supplied
// request to. It fails closed: a host it cannot resolve or cannot make sense
// of is refused.
//
// host is a hostname or IP literal WITHOUT a port, i.e. url.URL.Hostname().
func CheckHostPublic(host string) error {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if host == "" {
		return fmt.Errorf("missing host")
	}
	// Literal IP forms bypass DNS entirely, so check them directly.
	if ip := net.ParseIP(host); ip != nil {
		if IsForbidden(ip) {
			return fmt.Errorf("blocked address %s", ip)
		}
		return nil
	}
	// "localhost" is special: whether it is in DNS at all depends on
	// nsswitch, so resolution is not a reliable way to catch it. Same for
	// the RFC 6761 .localhost suffix, which is reserved to mean loopback.
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return fmt.Errorf("blocked host %q", host)
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("DNS lookup failed for %s: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("no IPs for host %s", host)
	}
	for _, ip := range ips {
		if IsForbidden(ip) {
			return fmt.Errorf("blocked address %s -> %s", host, ip)
		}
	}
	return nil
}

// DialControl is a net.Dialer.Control hook that refuses a connection whose
// resolved address must not be reached. Assign it to net.Dialer.Control.
func DialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("refusing to connect to unparseable address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Control always receives a resolved literal. Anything else is
		// something we do not understand, so refuse it.
		return fmt.Errorf("refusing to connect to non-literal address %q", host)
	}
	if IsForbidden(ip) {
		return fmt.Errorf("refusing to connect to blocked address %s", ip)
	}
	return nil
}

// SafeDialer returns a net.Dialer whose Control hook is DialControl.
func SafeDialer() *net.Dialer {
	return &net.Dialer{Timeout: dialTimeout, KeepAlive: keepAlive, Control: DialControl}
}

// SafeTransport returns an http.Transport that dials through SafeDialer, so
// every connection it opens is checked against the real IP at connect time.
func SafeTransport() *http.Transport {
	return &http.Transport{
		DialContext:           SafeDialer().DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// CheckRedirect returns an http.Client CheckRedirect function that caps the
// redirect chain at maxRedirects and re-runs CheckHostPublic on every hop.
// Without one, http.Client follows up to ten redirects to wherever the
// upstream points, which hands the redirect target the same reach the
// original URL was checked for.
func CheckRedirect(maxRedirects int) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("too many redirects (>%d)", maxRedirects)
		}
		if err := CheckHostPublic(req.URL.Hostname()); err != nil {
			return fmt.Errorf("redirect blocked: %w", err)
		}
		return nil
	}
}
