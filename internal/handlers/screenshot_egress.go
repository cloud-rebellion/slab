// Package handlers: filtering egress proxy for the headless screenshot path.
//
// WHY THIS FILE EXISTS
//
// validateScreenshotURL (and the loopback-host check on the preview path) run
// exactly ONCE, before chromedp.Navigate. Whatever the PAGE does after that is
// unguarded: Chromium does its own DNS and its own native networking, so the
// Go-side netguard dial guard never sees a single renderer fetch. A page that
// top-level-navigates, meta-refreshes, opens an iframe, clicks an anchor, calls
// window.open, submits a form, loads an <img> or sets a CSS background pointed
// at an internal address gets that response RENDERED, and the render is handed
// straight back to the caller as image_base64. Five of those mechanisms need no
// JavaScript at all, so a Content-Security-Policy cannot close this: CSP has no
// directive that bounds top-level navigation while allowing scripts
// (navigate-to was dropped from CSP3 and never shipped in Chrome).
//
// The only control that sees every one of those mechanisms is the socket. This
// file runs an in-process filtering forward proxy on loopback for the lifetime
// of a single capture and points Chromium at it, so EVERY connection the
// renderer makes (top-level, redirect, rebind, sub-resource, favicon) is
// checked against one policy at connect time and dialled to the vetted IP.
//
// The proxy-bypass-list "<-loopback>" token on the allocator is load-bearing and
// not optional: Chromium bypasses a configured proxy for loopback destinations
// BY DEFAULT, so without it every co-located 127.0.0.1 service (the admin API,
// the database's HTTP surface, anything else on the box) stays directly
// reachable and the proxy is decorative.
//
// The same file already exists, for the same reason, at
// svar/engine/internal/browser/egress_proxy.go. See the note at the bottom of
// this file about why the two are not yet one shared package.

package handlers

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/bright-interaction/slab/internal/netguard"
)

// screenshotEgressDialTimeout bounds a single upstream connect attempt made on
// behalf of the renderer.
const screenshotEgressDialTimeout = 15 * time.Second

// screenshotEgress is the per-capture filtering forward proxy.
//
// ONE policy serves BOTH entry points, which is the point: the loopback preview
// capture (CaptureLoopbackScreenshot) and the public capture (CaptureScreenshot)
// differ only in what exemptOrigin is set to.
//
// An origin is reachable if and only if:
//
//	(a) it equals exemptOrigin exactly, and resolves to a loopback address; or
//	(b) validateScreenshotURL permits it AND netguard.DialControl permits the
//	    address it actually resolves to at connect time.
//
// exemptOrigin is the capture target's own origin, and is set ONLY when that
// target is itself a loopback host, i.e. the server-minted
// http://127.0.0.1:{port}/_preview/{token} URL, or a developer's own dev server
// on an instance with no allow-list configured. It is a single exact host:port
// string computed once from the target the caller already validated, never from
// anything the rendered page says. Rule (b) can never yield a loopback
// destination, because netguard.DialControl refuses loopback, so the preview
// page cannot reach a SECOND loopback port.
type screenshotEgress struct {
	// exemptOrigin is the one normalized "host:port" reachable under rule (a),
	// or "" when the capture target is not a loopback host.
	exemptOrigin string

	ln   net.Listener
	srv  *http.Server
	addr string
}

// startScreenshotEgress binds a filtering forward proxy on 127.0.0.1:0 for one
// capture of target and returns it. The caller MUST call stop when the capture
// ends.
//
// It fails closed: if the listener cannot be created the capture must not run,
// because a capture without this proxy is the unguarded full-read SSRF this
// file exists to close.
func startScreenshotEgress(target string) (*screenshotEgress, error) {
	parsed, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("egress proxy: malformed target: %w", err)
	}
	e := &screenshotEgress{}
	if isLoopbackScreenshotHost(strings.ToLower(parsed.Hostname())) {
		origin, oerr := normalizeScreenshotOrigin(parsed.Host, defaultPortForScheme(parsed.Scheme))
		if oerr != nil {
			return nil, fmt.Errorf("egress proxy: unusable target origin: %w", oerr)
		}
		e.exemptOrigin = origin
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("egress proxy: listen: %w", err)
	}
	e.ln = ln
	e.addr = ln.Addr().String()
	e.srv = &http.Server{
		Handler:           http.HandlerFunc(e.serve),
		ReadHeaderTimeout: 15 * time.Second,
	}
	go func() { _ = e.srv.Serve(ln) }()
	return e, nil
}

// proxyURL is the value for the chromedp proxy-server flag.
func (e *screenshotEgress) proxyURL() string { return "http://" + e.addr }

// stop shuts the proxy down with the capture. Nothing may egress through it
// afterwards.
func (e *screenshotEgress) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = e.srv.Shutdown(ctx)
	_ = e.ln.Close()
}

// permit applies the single policy. It returns whether the destination is the
// exempt origin (which selects the loopback-pinned dialer) or must go through
// the netguard-guarded dialer, and an error when neither rule allows it.
func (e *screenshotEgress) permit(hostPort, defaultPort string) (exempt bool, err error) {
	origin, err := normalizeScreenshotOrigin(hostPort, defaultPort)
	if err != nil {
		return false, err
	}
	// Rule (a): the exact origin the caller already validated, loopback-pinned
	// at dial time.
	if e.exemptOrigin != "" && origin == e.exemptOrigin {
		return true, nil
	}
	// Rule (b): the public allow-list the screenshot tool already enforces.
	// Synthesizing the URL rather than re-implementing the check keeps this
	// from drifting away from validateScreenshotURL, which also carries the
	// 80/443 port ceiling and the DNS-rebind pre-flight.
	scheme := "http"
	if defaultPort == "443" {
		scheme = "https"
	}
	if verr := validateScreenshotURL(scheme + "://" + origin + "/"); verr != nil {
		return false, verr
	}
	return false, nil
}

// serve is the proxy. It handles CONNECT (every https fetch the renderer makes)
// and absolute-URI plain HTTP.
func (e *screenshotEgress) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		e.serveConnect(w, r)
		return
	}
	e.serveHTTP(w, r)
}

func (e *screenshotEgress) serveConnect(w http.ResponseWriter, r *http.Request) {
	// CONNECT always carries an explicit port in practice; 443 is the default
	// only as a belt-and-braces normalization.
	exempt, err := e.permit(r.Host, "443")
	if err != nil {
		e.refuse(w, r.Host, err)
		return
	}
	dst, err := e.dial(r.Context(), r.Host, "443", exempt)
	if err != nil {
		e.refuse(w, r.Host, err)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = dst.Close()
		http.Error(w, "proxy cannot hijack", http.StatusInternalServerError)
		return
	}
	src, _, herr := hj.Hijack()
	if herr != nil {
		_ = dst.Close()
		return
	}
	if _, werr := src.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); werr != nil {
		_ = dst.Close()
		_ = src.Close()
		return
	}
	go func() {
		_, _ = io.Copy(dst, src)
		_ = dst.Close()
	}()
	_, _ = io.Copy(src, dst)
	_ = src.Close()
}

func (e *screenshotEgress) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL == nil || r.URL.Host == "" {
		http.Error(w, "proxy expects an absolute URI", http.StatusBadRequest)
		return
	}
	host := r.URL.Host
	exempt, err := e.permit(host, "80")
	if err != nil {
		e.refuse(w, host, err)
		return
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return e.dial(ctx, address, "80", exempt)
		},
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: 20 * time.Second,
	}
	defer tr.CloseIdleConnections()

	outReq := r.Clone(r.Context())
	outReq.RequestURI = ""
	resp, rerr := tr.RoundTrip(outReq)
	if rerr != nil {
		e.refuse(w, host, rerr)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// dial opens the upstream connection under the policy decided by permit.
//
// The two dialers are deliberately distinct rather than one dialer with a flag:
// the exempt path pins the destination to a loopback address, so an
// exemptOrigin of "localhost" cannot be pointed outward by a resolver, and the
// guarded path refuses loopback outright.
func (e *screenshotEgress) dial(ctx context.Context, hostPort, defaultPort string, exempt bool) (net.Conn, error) {
	origin, err := normalizeScreenshotOrigin(hostPort, defaultPort)
	if err != nil {
		return nil, err
	}
	var d *net.Dialer
	if exempt {
		d = &net.Dialer{Timeout: screenshotEgressDialTimeout, Control: loopbackOnlyDialControl}
	} else {
		d = &net.Dialer{Timeout: screenshotEgressDialTimeout, Control: netguard.DialControl}
	}
	return d.DialContext(ctx, "tcp", origin)
}

// loopbackOnlyDialControl is the mirror image of netguard.DialControl: it
// permits nothing but loopback. It guards the exempt origin so that the one
// allowance this proxy makes cannot be redirected off-box by DNS.
func loopbackOnlyDialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("refusing to connect to unparseable address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("refusing to connect to non-literal address %q", host)
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("refusing to connect to non-loopback address %s on the preview-origin path", ip)
	}
	return nil
}

// refuse is what the renderer sees instead of the internal response. The body
// is deliberately generic: it is going to be rendered into a screenshot handed
// back to the caller, so it must not describe the destination.
func (e *screenshotEgress) refuse(w http.ResponseWriter, host string, err error) {
	slog.Warn("screenshot egress refused", "host", host, "err", err)
	http.Error(w, "blocked by screenshot egress policy", http.StatusForbidden)
}

// normalizeScreenshotOrigin turns whatever form the renderer used into a single
// comparable "host:port". Hosts are lower-cased and a trailing root dot is
// stripped, so "Internal.Example.Com." and "internal.example.com" cannot be
// treated as two different origins by the policy and one by the dialler.
func normalizeScreenshotOrigin(hostPort, defaultPort string) (string, error) {
	hostPort = strings.TrimSpace(hostPort)
	if hostPort == "" {
		return "", fmt.Errorf("missing host")
	}
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		host, port = hostPort, defaultPort
	}
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if host == "" {
		return "", fmt.Errorf("missing host in %q", hostPort)
	}
	if port == "" {
		port = defaultPort
	}
	return net.JoinHostPort(host, port), nil
}

// defaultPortForScheme maps the target URL's scheme to the port to assume when
// the URL carried none.
func defaultPortForScheme(scheme string) string {
	if strings.EqualFold(scheme, "https") {
		return "443"
	}
	return "80"
}

// NOTE ON THE TWIN. svar/engine/internal/browser/egress_proxy.go is the same
// control for the same reason, and svar got it first (2026-07-17 audit,
// "Chromium renderer bypasses the SSRF guard"). The two are NOT shared code
// because svar's policy is "any public address" while this one additionally
// carries the screenshot allow-list and the single loopback preview exemption,
// and because the two products are separate Go modules with separate netguard
// packages. If a THIRD product needs a filtering renderer proxy, extract a
// shared package rather than writing a third copy.
