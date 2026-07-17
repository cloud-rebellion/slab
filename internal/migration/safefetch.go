package migration

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// userAgent identifies our crawler so source-site operators can see it in
// their logs. Same ethos as Googlebot: explicit, includes a contact channel
// (the project home, so operators can opt out via robots.txt and look up
// what the crawler does).
const userAgent = "AtomicSite-Migrator/1.0 (+https://github.com/bright-interaction/slab)"

// FetchOptions controls the SSRF-guarded HTTP fetch. Defaults via Default*
// constants are sized for HTML pages, which is what every importer needs.
type FetchOptions struct {
	MaxBytes     int64
	MaxRedirects int
	Timeout      time.Duration
	AllowPrivate bool   // true ONLY in tests against httptest.Server
	AcceptHeader string // "*/*" if empty
	ExtraHeaders http.Header
}

const (
	DefaultMaxBytes     int64 = 10 * 1024 * 1024 // 10 MiB - typical HTML pages are far smaller; extras blocked
	DefaultMaxRedirects       = 3
	DefaultTimeout            = 30 * time.Second
)

// FetchResult is what every safe HTTP fetch returns. ContentType is taken
// from the response header; callers needing content sniffing must do it
// themselves on Body.
type FetchResult struct {
	URL         string
	Status      int
	ContentType string
	Body        []byte
}

// SafeFetch performs an SSRF-guarded HTTP GET. The check runs:
//  1. URL parses to http(s)
//  2. DNS resolves the host; every returned IP must be public
//  3. On every redirect (max MaxRedirects) the new host gets the same DNS
//     check - protects against DNS rebinding
//  4. Body is read through io.LimitReader(MaxBytes+1) and rejected if larger
//
// Returns the final URL after redirects so callers know which host actually
// served the response (matters for cross-origin asset attribution).
func SafeFetch(ctx context.Context, rawURL string, opts FetchOptions) (*FetchResult, error) {
	if opts.MaxBytes == 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.MaxRedirects == 0 {
		opts.MaxRedirects = DefaultMaxRedirects
	}
	if opts.Timeout == 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.AcceptHeader == "" {
		opts.AcceptHeader = "*/*"
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("only http/https URLs allowed (got %q)", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("URL missing host")
	}
	if !opts.AllowPrivate {
		if err := checkHostPublic(u.Hostname()); err != nil {
			return nil, err
		}
	}

	client := &http.Client{
		Timeout:   opts.Timeout,
		Transport: safeTransport(opts.AllowPrivate),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= opts.MaxRedirects {
				return fmt.Errorf("too many redirects (>%d)", opts.MaxRedirects)
			}
			if !opts.AllowPrivate {
				if err := checkHostPublic(req.URL.Hostname()); err != nil {
					return fmt.Errorf("redirect blocked: %w", err)
				}
			}
			return nil
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", opts.AcceptHeader)
	req.Header.Set("Accept-Language", "en;q=0.8")
	for k, vals := range opts.ExtraHeaders {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	limited := io.LimitReader(resp.Body, opts.MaxBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > opts.MaxBytes {
		return nil, fmt.Errorf("response exceeds max size %d bytes", opts.MaxBytes)
	}

	return &FetchResult{
		URL:         resp.Request.URL.String(),
		Status:      resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Body:        body,
	}, nil
}

// safeTransport builds an http.Transport whose dialer re-checks the ACTUAL
// resolved IP at connect time. The pre-flight checkHostPublic alone is a TOCTOU:
// the transport resolves DNS a second time when it dials, so a hostname with a
// ~0s TTL can pass the pre-flight (public IP) and then dial a private/metadata
// IP (DNS rebinding). The Control hook runs against the real dial address right
// before connect, on the initial request AND every redirect, closing that gap.
// Mirrors the estate's domains/ssrf.go ssrfDialControl shape. allowPrivate (tests
// only) disables the hook so httptest loopback servers work.
func safeTransport(allowPrivate bool) *http.Transport {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if !allowPrivate {
		d.Control = safeDialControl
	}
	return &http.Transport{
		DialContext:           d.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// safeDialControl rejects a connection whose resolved address is a
// private/loopback/link-local/metadata IP. Runs after DNS resolution, immediately
// before the socket connects, so it sees the IP actually being dialed.
func safeDialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); ip != nil && isPrivateOrLocal(ip) {
		return fmt.Errorf("refusing to connect to private address %s", ip)
	}
	return nil
}

// checkHostPublic resolves a hostname and refuses if any returned IP is in a
// private/loopback/link-local/multicast range. Same predicate logic as the
// existing handlers/media.go SSRF guard - kept in this package so the
// migration code has no cross-package dependency on a handler.
func checkHostPublic(host string) error {
	if host == "" {
		return fmt.Errorf("missing host")
	}
	// Reject literal IP forms that bypass DNS - e.g. "127.0.0.1".
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateOrLocal(ip) {
			return fmt.Errorf("private address blocked: %s", ip)
		}
		return nil
	}
	// "localhost" is special: not always in DNS, sometimes resolved by
	// nsswitch to 127.0.0.1, sometimes not. Block it explicitly.
	if strings.EqualFold(host, "localhost") {
		return fmt.Errorf("localhost blocked")
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("DNS lookup failed for %s: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("no IPs for host %s", host)
	}
	for _, ip := range ips {
		if isPrivateOrLocal(ip) {
			return fmt.Errorf("private address blocked: %s -> %s", host, ip)
		}
	}
	return nil
}

// isPrivateOrLocal returns true for any IP a public service should never be
// reachable on - RFC1918, IPv6 ULA, loopback, link-local, multicast, the
// unspecified address. Mirrors handlers/media.go isPrivateIP.
func isPrivateOrLocal(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if ip.IsPrivate() {
		return true
	}
	return false
}
