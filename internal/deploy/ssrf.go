package deploy

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bright-interaction/slab/internal/netguard"
)

// This file is the SSRF boundary for the deploy package.
//
// Every field this package dials is tenant-supplied: dockyard_url and host
// come straight out of the deploy_targets config a tenant POSTs to
// /api/sites/{id}/deploy-targets. Until this file existed the only checks were
// "scheme is https" and "host is non-empty", so a target pointed at
// 169.254.169.254 or at the Tailscale range was dialled by the app server on
// the tenant's behalf, and up to 4096 bytes of the reply came back in the
// error string that handlers/build.go appends to the build log the tenant
// reads via get_build_status.
//
// The guard has three parts because no single one is sufficient:
//
//  1. checkHostOffline, run from Validate at target-save time. Deterministic
//     and DNS-free, so a target is never rejected because DNS was flaky.
//  2. netguard.CheckHostPublic, run immediately before the request/exec.
//     Resolves and rejects a hostname pointing inside.
//  3. netguard.SafeTransport's dial-time Control hook, which sees the address
//     actually being connected to and therefore survives DNS rebinding, plus
//     netguard.CheckRedirect, because without a redirect policy Go follows ten
//     hops to wherever the upstream points.

// deployMaxRedirects caps the redirect chain on a call to a tenant-supplied
// deploy endpoint. Dockyard's admin API does not redirect at all; three hops
// is slack for a proxy in front of it, not a feature.
const deployMaxRedirects = 3

// newGuardedHTTPClient builds the http.Client used for calls to a
// tenant-supplied deploy endpoint.
//
// allowPrivate is a test-only escape hatch: httptest servers bind loopback,
// which the dial hook would refuse. It relaxes the dial hook only. The
// redirect host check stays on unconditionally, because a redirect target is
// chosen by the upstream rather than by the caller, so there is no legitimate
// reason to ever relax it, and the redirect cap is what stops an upstream
// walking us somewhere we never checked.
func newGuardedHTTPClient(timeout time.Duration, allowPrivate bool) *http.Client {
	c := &http.Client{
		Timeout:       timeout,
		Transport:     netguard.SafeTransport(),
		CheckRedirect: netguard.CheckRedirect(deployMaxRedirects),
	}
	if allowPrivate {
		c.Transport = &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
	}
	return c
}

// checkHostOffline rejects the host forms that can be judged without touching
// the network: an IP literal in a range a tenant-supplied URL must not reach,
// and localhost (which is not reliably in DNS, so resolving is not a reliable
// way to catch it).
//
// A plain hostname is deliberately left to the connect-time guards. Validate
// runs on every target save and must not fail because a DNS server was slow,
// and must not turn a config API into a DNS oracle.
//
// The verdict itself comes from netguard, which is the single definition of
// "an address a tenant-supplied URL must not reach". Do not reintroduce a
// local range list here.
func checkHostOffline(host string) error {
	h := strings.TrimSuffix(strings.TrimSpace(host), ".")
	if h == "" {
		return fmt.Errorf("missing host")
	}
	isLocalhost := strings.EqualFold(h, "localhost") || strings.HasSuffix(strings.ToLower(h), ".localhost")
	if net.ParseIP(h) == nil && !isLocalhost {
		return nil
	}
	// Both remaining branches are the no-DNS paths inside CheckHostPublic.
	return netguard.CheckHostPublic(h)
}

// checkTenantURLOffline is checkHostOffline for a whole URL.
func checkTenantURLOffline(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	return checkHostOffline(u.Hostname())
}

// checkTenantURLPublic is the request-time pre-flight: it resolves the URL's
// host and refuses if any answer is an address a tenant-supplied URL must not
// reach. Run this immediately before the request, not at save time.
func checkTenantURLPublic(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if err := netguard.CheckHostPublic(u.Hostname()); err != nil {
		return fmt.Errorf("destination not allowed: %w", err)
	}
	return nil
}
