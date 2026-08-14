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
// of a single capture and points Chromium at it, so every TCP connection the
// renderer makes (top-level, redirect, rebind, sub-resource, favicon) is
// checked against one policy at connect time and dialled to the vetted IP.
//
// SCOPE, precisely, because this was wrong once. THIS PROXY IS TCP-ONLY.
// --proxy-server is an HTTP forward proxy and carries TCP; it never sees a UDP
// datagram. Chromium's WebRTC stack opens its own UDP sockets and does its own
// DNS, so neither reaches this listener, and a CSP does not gate WebRTC in
// Chrome either. Before that channel was closed, a page could sweep internal
// ranges with one RTCPeerConnection per candidate against stun: URLs and read
// liveness off onicecandidate timing: measured as 5 STUN Binding Requests
// landing directly on an internal UDP listener (first bytes
// 00 01 00 00 21 12 a4 42) with this proxy logging nothing, and 6 under a full
// CSP. In the same runs the img and top-level-navigation probes to the same
// host WERE caught here, which is what makes it a bypass rather than a broken
// harness.
//
// UDP is therefore closed by an allocator flag, NOT by this proxy:
//
//	chromedp.Flag("webrtc-ip-handling-policy", "disable_non_proxied_udp")
//
// in captureScreenshot. Do not assume the proxy covers a channel it cannot see.
// Two traps live in that one line, both measured on this machine rather than
// taken from the internet:
//
//   - The commonly cited --force-webrtc-ip-handling-policy is a NO-OP here.
//     With it, the UDP probe still lands. The un-prefixed name is the one that
//     works. TestScreenshotEgressBlocksWebRTCUDP pins the behaviour.
//   - It must NOT be spelled as a disable-features entry. chromedp.Flag is a
//     map set, so a second disable-features silently clobbers chromedp's own
//     site-per-process,Translate,BlinkGenPropertyTrees default.
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
	"sync"
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
//	(b) netguard permits the address it actually resolves to at connect time,
//	    AND the port is 80 or 443.
//
// exemptOrigin is the capture target's own origin, and is set ONLY when that
// target is itself a loopback host, i.e. the server-minted
// http://127.0.0.1:{port}/_preview/{token} URL, or a developer's own dev server
// on an instance with no allow-list configured. It is a single exact host:port
// string computed once from the target the caller already validated, never from
// anything the rendered page says. Rule (b) can never yield a loopback
// destination, because netguard.DialControl refuses loopback, so the preview
// page cannot reach a SECOND loopback port.
//
// WHY RULE (b) IS NETGUARD AND NOT THE SCREENSHOT ALLOW-LIST. This is a
// deliberate decision, not an oversight, and it has been argued once already:
// please do not "restore" validateScreenshotURL here without reading this.
//
// HOP ZERO IS STILL ALLOW-LISTED. validateScreenshotURL continues to gate the
// URL the caller asked for, in CaptureScreenshot, before the browser starts.
// The relaxation below applies only to SUB-RESOURCES and later hops, which is
// exactly the surface that was previously unguarded.
//
// For those sub-resources the control is netguard PLUS an 80/443 port ceiling,
// and netguard refuses loopback, RFC1918, CGNAT/Tailscale, link-local including
// the cloud metadata address, ULA, NAT64 and 6to4/Teredo, behind an
// allowlist-first global-unicast check that fails closed on anything unlisted.
// That is the entire measured finding: every leak target in the SSRF matrix
// this file was written against was a private address.
//
// What is NOT kept from validateScreenshotURL is its host allow-list. Keeping
// it would buy exactly one extra property, "the page cannot reach an arbitrary
// PUBLIC host", and would cost the product its typography and CDN imagery: a
// tenant site would screenshot with its webfonts and third-party images
// silently missing. The consumer of a capture is a human or a vision-capable
// model looking at typography, layout and imagery, so a render missing those
// misrepresents the page to the only thing that reads it. And the
// exfil-to-a-public-host channel that extra property would close is one the
// draft-preview CSP already permits via img-src https: (handlers/agent.go:157),
// to a party that already holds the data.
//
// KNOWN CONSEQUENCE, now CLOSED. Dropping the host allow-list initially dropped
// the 80/443 port ceiling with it, which left "any public host on any port"
// reachable as a sub-resource. That was wider than the fidelity argument above
// supports, so the ceiling is kept explicitly in permit: real webfonts and CDNs
// are served over 80/443 without exception, so it costs the render nothing.
// Rule (b) is therefore netguard plus a port ceiling, not netguard alone.
type screenshotEgress struct {
	// exemptOrigin is the one normalized "host:port" reachable under rule (a),
	// or "" when the capture target is not a loopback host.
	exemptOrigin string

	ln   net.Listener
	srv  *http.Server
	addr string

	// tunnels tracks the conns of every live hijacked CONNECT tunnel, because
	// http.Server.Shutdown deliberately does NOT touch hijacked connections and
	// ln.Close only stops new accepts. Without this, a tunnel opened during a
	// capture keeps relaying after stop returns: measured as an established
	// tunnel still fetching from the exempt origin and getting HTTP/1.1 200
	// while the listener refused new connections.
	mu      sync.Mutex
	tunnels map[net.Conn]struct{}
	stopped bool
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
	e := &screenshotEgress{tunnels: map[net.Conn]struct{}{}}
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
		Handler: http.HandlerFunc(e.serve),
		// Bounded by the capture, which itself gets 30s in captureScreenshot.
		// Without Write/Idle timeouts a loopback client that stops reading
		// mid-response pins a goroutine and two sockets indefinitely.
		// serveConnect clears these deadlines after hijacking, so they do not
		// truncate a tunnel; see the note there.
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       35 * time.Second,
		WriteTimeout:      35 * time.Second,
		IdleTimeout:       20 * time.Second,
	}
	go func() { _ = e.srv.Serve(ln) }()
	return e, nil
}

// proxyURL is the value for the chromedp proxy-server flag.
func (e *screenshotEgress) proxyURL() string { return "http://" + e.addr }

// stop shuts the proxy down with the capture. Nothing may egress through it
// afterwards, which is why it closes hijacked CONNECT tunnels explicitly:
// Shutdown does not, and a still-relaying tunnel would outlive the capture that
// authorized it.
//
// The stopped flag is set BEFORE anything is torn down, so a CONNECT racing
// stop cannot register a tunnel that nothing will ever close.
func (e *screenshotEgress) stop() {
	e.mu.Lock()
	e.stopped = true
	live := make([]net.Conn, 0, len(e.tunnels))
	for c := range e.tunnels {
		live = append(live, c)
	}
	e.tunnels = map[net.Conn]struct{}{}
	e.mu.Unlock()
	for _, c := range live {
		_ = c.Close()
	}

	// Brief graceful pass, then a hard Close. Shutdown waits for active
	// connections to finish, so on its own it both delayed stop for as long as
	// the graceful deadline allowed and let an in-flight plain-HTTP request
	// complete after stop returned. The capture is over by this point, so
	// there is nothing worth waiting for.
	// TestScreenshotEgressStopClosesHijackedTunnels asserts stop stays under a
	// second; it currently returns in tens of microseconds.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = e.srv.Shutdown(ctx)
	_ = e.srv.Close()
	_ = e.ln.Close()
}

// registerTunnel records a hijacked tunnel's conns so stop can close them. It
// returns false when the proxy has already stopped, in which case the caller
// must close them itself rather than leaving a tunnel nothing tracks.
func (e *screenshotEgress) registerTunnel(conns ...net.Conn) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped {
		return false
	}
	for _, c := range conns {
		e.tunnels[c] = struct{}{}
	}
	return true
}

// releaseTunnel drops conns that closed on their own, so a long capture does
// not accumulate dead entries.
func (e *screenshotEgress) releaseTunnel(conns ...net.Conn) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range conns {
		delete(e.tunnels, c)
	}
}

// permit applies the single policy. It RETURNS THE NORMALIZED ORIGIN IT
// APPROVED, along with whether that origin is the exempt one (which selects the
// loopback-pinned dialer) or must go through the netguard-guarded dialer, and
// an error when neither rule allows it.
//
// Returning the origin is not a convenience. The verdict used to be a bare
// boolean and the callers re-derived the address they dialled, so serveHTTP
// could decide "exempt" about 127.0.0.1:80 and then hand 127.0.0.1:443 to a
// dialler that only asked "is this loopback", skipping netguard for ANY
// loopback address. Same lesson as the media.go rebind: never let the thing you
// checked and the thing you connect to be two different values. Callers MUST
// dial the returned string verbatim.
//
// Sub-resources are bounded by netguard, NOT by the screenshot allow-list. See
// the decision recorded on the screenshotEgress type before changing this.
//
// The literal-IP check below is an early, well-logged refusal for the common
// SSRF probe shape (the page names an address outright, which is what every
// mechanism in the leak matrix did). It is an optimisation of the log, not the
// control: netguard.DialControl in dial is authoritative, runs on the address
// actually resolved at connect time, and is what catches a HOSTNAME pointed
// inside, including one that rebinds after this check.
func (e *screenshotEgress) permit(hostPort, defaultPort string) (origin string, exempt bool, err error) {
	origin, err = normalizeScreenshotOrigin(hostPort, defaultPort)
	if err != nil {
		return "", false, err
	}
	// Rule (a): the exact origin the caller already validated, loopback-pinned
	// at dial time.
	if e.exemptOrigin != "" && origin == e.exemptOrigin {
		return origin, true, nil
	}
	// Rule (b). No DNS round trip here on purpose: a resolving pre-flight would
	// add a lookup per sub-resource and could still only refuse what DialControl
	// refuses anyway.
	host, port, serr := net.SplitHostPort(origin)
	if serr != nil {
		return "", false, serr
	}
	if ip := net.ParseIP(host); ip != nil && netguard.IsForbidden(ip) {
		return "", false, fmt.Errorf("refusing blocked address %s", ip)
	}
	// Port ceiling. Webfonts and CDNs are served over 80/443 without exception,
	// so this costs the render nothing while keeping "any public host on any
	// port" off the reachable surface. It is the one piece of
	// validateScreenshotURL that rule (b) deliberately keeps.
	if port != "80" && port != "443" {
		return "", false, fmt.Errorf("refusing sub-resource port %q (only 80/443)", port)
	}
	return origin, false, nil
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
	origin, exempt, err := e.permit(r.Host, "443")
	if err != nil {
		e.refuse(w, r.Host, err)
		return
	}
	dst, err := e.dial(r.Context(), origin, exempt)
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
	// Track before the 200, so a tunnel is never live-but-untracked. If stop
	// already ran, refuse rather than open a tunnel that outlives the capture.
	if !e.registerTunnel(src, dst) {
		_ = dst.Close()
		_ = src.Close()
		return
	}
	defer e.releaseTunnel(src, dst)
	// A hijacked conn keeps whatever deadline http.Server set from ReadTimeout
	// and WriteTimeout, which would cut a long-lived tunnel off mid-stream.
	// Clearing them is safe ONLY because the tunnel is now tracked and stop
	// closes it, and because the capture context bounds the whole capture.
	_ = src.SetDeadline(time.Time{})
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
	// The default port comes from the SCHEME, not a constant. It used to be a
	// hardcoded "80", so an https absolute-URI was judged on :80 and then
	// dialled on :443.
	origin, exempt, err := e.permit(host, defaultPortForScheme(r.URL.Scheme))
	if err != nil {
		e.refuse(w, host, err)
		return
	}
	tr := &http.Transport{
		// address is deliberately ignored: we dial the origin permit approved,
		// never the one the transport re-derived from the URL. Those two were
		// allowed to differ once, and that was the bug.
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return e.dial(ctx, origin, exempt)
		},
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: 20 * time.Second,
	}
	defer tr.CloseIdleConnections()

	outReq := r.Clone(r.Context())
	outReq.RequestURI = ""
	stripHopByHopHeaders(outReq.Header)
	resp, rerr := tr.RoundTrip(outReq)
	if rerr != nil {
		e.refuse(w, host, rerr)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	stripHopByHopHeaders(resp.Header)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// hopByHopHeaders are the RFC 9110 connection-scoped headers a proxy must not
// relay. Proxy-Authorization is the one that matters: it is credentials meant
// for THIS proxy, and forwarding it hands them to the upstream. This proxy has
// no auth today, so nothing is leaked yet; it is stripped so that adding auth
// later cannot quietly become a credential-forwarding bug.
var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Keep-Alive",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// stripHopByHopHeaders removes the connection-scoped headers, including every
// token the Connection header itself names, which is the half people forget.
func stripHopByHopHeaders(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if tok = strings.TrimSpace(tok); tok != "" {
				h.Del(tok)
			}
		}
	}
	for _, name := range hopByHopHeaders {
		h.Del(name)
	}
}

// dial opens the upstream connection under the policy decided by permit.
//
// The two dialers are deliberately distinct rather than one dialer with a flag:
// the exempt path pins the destination to a loopback address, so an
// exemptOrigin of "localhost" cannot be pointed outward by a resolver, and the
// guarded path refuses loopback outright.
// origin MUST be the exact string permit returned. dial does not re-derive or
// re-normalize it, because re-deriving is what let an exempt verdict about one
// origin authorise a connection to another.
func (e *screenshotEgress) dial(ctx context.Context, origin string, exempt bool) (net.Conn, error) {
	var d *net.Dialer
	if exempt {
		// The invariant, enforced rather than assumed: an exempt verdict
		// authorises exactly exemptOrigin and nothing else. loopbackOnlyDialControl
		// only asks "is this loopback", so without this check an exempt verdict
		// would open every loopback port on the box.
		if origin != e.exemptOrigin {
			return nil, fmt.Errorf("refusing exempt dial to %q, which is not the exempt origin %q", origin, e.exemptOrigin)
		}
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
	recordScreenshotEgressRefusal(host)
	slog.Warn("screenshot egress refused", "host", host, "err", err)
	http.Error(w, "blocked by screenshot egress policy", http.StatusForbidden)
}

// screenshotEgressRefusals records, per destination, how many times the policy
// refused it.
//
// This exists because "the internal service got zero requests" and "the page
// never tried" are the same green, and mutation testing proved it: with the
// exempt-origin rule deleted so the proxy blocked EVERYTHING, every leak subtest
// still passed, and so did replacing an attack mechanism with an HTML comment.
// A guard test has to show the attack fired AND was stopped. Counting per
// destination rather than in total is what makes it evidence: the renderer's own
// background chatter is refused too on an instance with no allow-list, so a bare
// total would be non-zero no matter what the page did.
var screenshotEgressRefusals struct {
	sync.Mutex
	byHost map[string]int
}

func recordScreenshotEgressRefusal(host string) {
	screenshotEgressRefusals.Lock()
	defer screenshotEgressRefusals.Unlock()
	if screenshotEgressRefusals.byHost == nil {
		screenshotEgressRefusals.byHost = map[string]int{}
	}
	screenshotEgressRefusals.byHost[strings.ToLower(strings.TrimSpace(host))]++
}

// screenshotEgressRefusalCount reports how many times host was refused.
func screenshotEgressRefusalCount(host string) int {
	screenshotEgressRefusals.Lock()
	defer screenshotEgressRefusals.Unlock()
	return screenshotEgressRefusals.byHost[strings.ToLower(strings.TrimSpace(host))]
}

// resetScreenshotEgressRefusals clears the ledger between captures.
func resetScreenshotEgressRefusals() {
	screenshotEgressRefusals.Lock()
	defer screenshotEgressRefusals.Unlock()
	screenshotEgressRefusals.byHost = map[string]int{}
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
