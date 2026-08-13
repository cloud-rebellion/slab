package handlers

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bright-interaction/slab/internal/netguard"
)

// This file is the guard for /api/agent/media/from-url, which is reachable by
// any write-scoped agent key and had zero test coverage of any kind while it
// carried a live DNS-rebinding SSRF.
//
// The important case is TestFetchWithSSRFGuard_RebindingIsRefusedAtDial. Every
// other SSRF test in this repo asserts a static denylist: it hands the guard an
// IP literal or "localhost" and checks the refusal. A denylist assertion cannot
// distinguish a guard that checks the address it is about to dial from one that
// checks a DIFFERENT address and then dials whatever DNS says the second time,
// because in a static test both resolutions return the same thing.

const rebindHost = "rebind.slab-ssrf.test"

const publicIP = "93.184.216.34"

func TestFetchWithSSRFGuard_BlocksForbiddenLiterals(t *testing.T) {
	tests := []struct {
		name string
		host string
	}{
		{"loopback", "127.0.0.1"},
		{"loopback other octet", "127.9.9.9"},
		{"cloud metadata", "169.254.169.254"},
		{"rfc1918 ten", "10.0.0.1"},
		{"rfc1918 172", "172.16.0.1"},
		{"rfc1918 192", "192.168.1.1"},
		{"tailscale cgnat", "100.64.0.1"},
		{"ipv6 loopback", "[::1]"},
		{"ipv4 mapped loopback", "[::ffff:127.0.0.1]"},
		{"ipv6 ula", "[fd00::1]"},
		{"nat64 loopback", "[64:ff9b::7f00:1]"},
		{"unspecified", "0.0.0.0"},
		// Zoned forms: net.ParseIP returns nil for these, which is what made
		// the sibling copies of this guard in internal/webhook and
		// internal/domains fail open. netguard refuses a non-literal.
		{"zoned v4-mapped loopback", "[::ffff:127.0.0.1%25lo0]"},
		{"zoned ipv6 loopback", "[::1%25lo0]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			url := fmt.Sprintf("http://%s/payload.png", tc.host)
			_, _, err := fetchWithSSRFGuard(context.Background(), url, 1<<20)
			if err == nil {
				t.Fatalf("fetchWithSSRFGuard(%q) returned nil error; a forbidden address was accepted", url)
			}
		})
	}
}

func TestFetchWithSSRFGuard_BlocksLocalhost(t *testing.T) {
	// localhost is not reliably in DNS, so resolving is not a reliable way to
	// catch it. netguard rejects the name itself; assert that reaches here.
	for _, host := range []string{"localhost", "LOCALHOST", "foo.localhost"} {
		_, _, err := fetchWithSSRFGuard(context.Background(), "http://"+host+"/x.png", 1<<20)
		if err == nil {
			t.Fatalf("fetchWithSSRFGuard accepted %q", host)
		}
	}
}

func TestFetchWithSSRFGuard_RejectsNonHTTPScheme(t *testing.T) {
	for _, raw := range []string{"file:///etc/passwd", "gopher://example.com/x", "ftp://example.com/x"} {
		_, _, err := fetchWithSSRFGuard(context.Background(), raw, 1<<20)
		if err == nil {
			t.Fatalf("fetchWithSSRFGuard accepted %q", raw)
		}
	}
}

// TestFetchWithSSRFGuard_RebindingIsRefusedAtDial is the case the old
// implementation failed, and it is deliberately built on ONE seam.
//
// The only thing this test controls is what DNS answers, via a local server
// net.DefaultResolver is pointed at. It answers the FIRST query for the host
// with a public address and every later query with 127.0.0.1, which is what a
// ~0s TTL record does. So:
//
//   - whatever the implementation uses for its pre-flight, seam or raw
//     net.LookupIP, it sees the public address and lets the fetch proceed;
//   - the transport resolves again when it dials and gets loopback.
//
// An earlier version of this test also stubbed netguard.LookupIP to return the
// public address. That made it exactly one mutant wide: an implementation that
// ran its pre-flight through raw net.LookupIP instead of the seam would have
// seen loopback, refused at the pre-flight, and passed this test while still
// having no dial-time guard at all. Do not reintroduce that stub here.
func TestFetchWithSSRFGuard_RebindingIsRefusedAtDial(t *testing.T) {
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.Write([]byte("internal-secret"))
	}))
	defer srv.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("parse httptest addr: %v", err)
	}

	dns := startRebindDNS(t)
	restore := useResolver(t, dns.addr)
	defer restore()

	// Harness self-check, in "always public" mode so it consumes no rebind
	// budget: if the fake resolver is not actually in the path, everything
	// below would fail for reasons unrelated to the guard.
	dns.setMode(dnsAlwaysPublic)
	if err := netguard.CheckHostPublic(rebindHost); err != nil {
		t.Fatalf("harness broken: %q did not resolve public through the fake resolver (%v)", rebindHost, err)
	}

	// Now the rebind: first answer public, everything after it loopback.
	dns.setMode(dnsRebind)
	target := fmt.Sprintf("http://%s:%s/logo.png", rebindHost, port)
	body, _, err := fetchWithSSRFGuard(context.Background(), target, 1<<20)
	if err == nil {
		t.Fatalf("DNS rebinding was NOT refused: fetched %d bytes from %s", len(body), target)
	}
	if reached {
		t.Fatalf("DNS rebinding was NOT refused: the internal handler served the request (err after the fact: %v)", err)
	}
	// The refusal has to come from the DIAL. "destination not allowed" is the
	// pre-flight's wording; if that is what came back, the pre-flight consumed
	// the loopback answer and this test proved the pre-flight, not the dial
	// guard, which is the failure mode the single-seam design exists to expose.
	if strings.Contains(err.Error(), "destination not allowed") {
		t.Fatalf("refused at the PRE-FLIGHT, not at the dial: %v; the rebind did not materialise", err)
	}
	if !strings.Contains(err.Error(), "refusing to connect") {
		t.Errorf("error = %v, want the dial-time Control refusal (\"refusing to connect to blocked address\")", err)
	}
}

func TestFetchWithSSRFGuard_BlocksRedirectToPrivate(t *testing.T) {
	// allowPrivate lets the first hop reach an httptest server on loopback.
	// The redirect policy is deliberately NOT relaxed by that flag, because a
	// redirect target is chosen by the upstream, not by the caller.
	var secondHopReached bool
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHopReached = true
		w.Write([]byte("internal-secret"))
	}))
	defer internal.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/secret", http.StatusFound)
	}))
	defer redirector.Close()

	_, _, err := fetchWithSSRFGuardAllowingPrivate(context.Background(), redirector.URL+"/start.png", 1<<20, true)
	if err == nil {
		t.Fatal("redirect to a private address was followed")
	}
	if secondHopReached {
		t.Fatal("redirect to a private address was followed: the internal handler served the request")
	}
	if !strings.Contains(err.Error(), "redirect blocked") {
		t.Errorf("error = %v, want \"redirect blocked\"", err)
	}
}

// TestFetchWithSSRFGuard_CapsRedirectChain proves the cap this function passes
// to netguard.CheckRedirect is actually enforced on a real chain.
//
// It needs every hop to LOOK public to the redirect check while actually
// landing on the loopback test server, so unlike the rebind test above it
// stubs both resolution paths on purpose. That is sound here because this test
// is about hop counting, not about the two resolutions agreeing.
func TestFetchWithSSRFGuard_CapsRedirectChain(t *testing.T) {
	var mu sync.Mutex
	var hops int
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hops++
		mu.Unlock()
		_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
		http.Redirect(w, r, fmt.Sprintf("http://%s:%s/next", rebindHost, port), http.StatusFound)
	}))
	defer srv.Close()

	dns := startRebindDNS(t)
	dns.setMode(dnsAlwaysLoopback) // every dial lands on the test server
	restore := useResolver(t, dns.addr)
	defer restore()

	// Redirect-time host checks must pass, so the chain can actually run.
	origLookup := netguard.LookupIP
	netguard.LookupIP = func(host string) ([]net.IP, error) {
		if host == rebindHost {
			return []net.IP{net.ParseIP(publicIP)}, nil
		}
		return origLookup(host)
	}
	defer func() { netguard.LookupIP = origLookup }()

	_, _, err := fetchWithSSRFGuardAllowingPrivate(context.Background(), srv.URL+"/start.png", 1<<20, true)
	if err == nil {
		t.Fatal("an unbounded redirect chain was followed to completion")
	}
	if !strings.Contains(err.Error(), "too many redirects") {
		t.Fatalf("chain ended for a reason other than the cap: %v", err)
	}
	mu.Lock()
	got := hops
	mu.Unlock()
	// netguard.CheckRedirect refuses once len(via) reaches the cap, and via
	// holds the requests already made. So the chain stops on the Nth request
	// rather than after the Nth redirect: the server sees exactly
	// mediaFetchMaxRedirects requests and follows one fewer redirect.
	if got != mediaFetchMaxRedirects {
		t.Errorf("server saw %d requests, want %d", got, mediaFetchMaxRedirects)
	}
}

func TestFetchWithSSRFGuard_BodySizeCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, 4096))
	}))
	defer srv.Close()

	_, _, err := fetchWithSSRFGuardAllowingPrivate(context.Background(), srv.URL+"/big.png", 1024, true)
	if err == nil {
		t.Fatal("a response larger than maxSize was accepted")
	}
	if !strings.Contains(err.Error(), "exceeds max size") {
		t.Errorf("error = %v, want the size-cap refusal", err)
	}
}

func TestFetchWithSSRFGuard_RejectsNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	_, _, err := fetchWithSSRFGuardAllowingPrivate(context.Background(), srv.URL+"/x.png", 1<<20, true)
	if err == nil {
		t.Fatal("a non-200 response was accepted")
	}
}

func TestFetchWithSSRFGuard_HappyPath(t *testing.T) {
	payload := []byte("\x89PNG\r\n\x1a\nfake-image-bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()

	tests := []struct {
		path     string
		wantName string
	}{
		{"/hero.png", "hero.png"},
		{"/a/b/c/logo.jpg", "logo.jpg"},
		{"/", "image"},
		{"", "image"},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			raw, name, err := fetchWithSSRFGuardAllowingPrivate(context.Background(), srv.URL+tc.path, 1<<20, true)
			if err != nil {
				t.Fatalf("fetchWithSSRFGuardAllowingPrivate: %v", err)
			}
			if string(raw) != string(payload) {
				t.Errorf("body = %q, want %q", raw, payload)
			}
			if name != tc.wantName {
				t.Errorf("filename = %q, want %q", name, tc.wantName)
			}
		})
	}
}

// TestFetchWithSSRFGuard_ClientTimeoutFires proves the CLIENT's deadline, not
// the caller's context deadline.
//
// The context passed in deliberately has no deadline, so the only thing that
// can end this fetch is http.Client.Timeout. Written the other way round, with
// a 2s context against a 30s client timeout, the test passes with the Timeout
// field deleted, because the context is what fires. That is what the first
// version of this test did.
func TestFetchWithSSRFGuard_ClientTimeoutFires(t *testing.T) {
	orig := mediaFetchTimeout
	mediaFetchTimeout = 300 * time.Millisecond
	defer func() { mediaFetchTimeout = orig }()

	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	done := make(chan error, 1)
	go func() {
		_, _, err := fetchWithSSRFGuardAllowingPrivate(context.Background(), srv.URL+"/slow.png", 1<<20, true)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a hanging upstream returned success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fetch did not return: http.Client.Timeout is not set, so only the caller's context bounds this")
	}
}

// --- fake resolver plumbing ---------------------------------------------

type dnsMode int

const (
	dnsAlwaysPublic dnsMode = iota
	dnsAlwaysLoopback
	// dnsRebind answers the first query public and every later one loopback,
	// which is what a record with a ~0s TTL does.
	dnsRebind
)

type rebindDNS struct {
	addr string
	mu   sync.Mutex
	mode dnsMode
	seen int
}

func (d *rebindDNS) setMode(m dnsMode) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.mode = m
	d.seen = 0
}

// answer picks the address for the next A query.
func (d *rebindDNS) answer() net.IP {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen++
	switch d.mode {
	case dnsAlwaysLoopback:
		return net.IPv4(127, 0, 0, 1)
	case dnsRebind:
		if d.seen == 1 {
			return net.ParseIP(publicIP)
		}
		return net.IPv4(127, 0, 0, 1)
	default:
		return net.ParseIP(publicIP)
	}
}

// useResolver points net.DefaultResolver at addr and returns a restore func.
func useResolver(t *testing.T, addr string) func() {
	t.Helper()
	orig := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "udp", addr)
		},
	}
	restore := func() { net.DefaultResolver = orig }
	t.Cleanup(restore)
	return restore
}

// startRebindDNS runs a minimal UDP DNS server and returns it. It answers A
// queries per its mode and answers everything else as NODATA, so Go's resolver
// does not stall waiting for the AAAA half of a dual query.
func startRebindDNS(t *testing.T) *rebindDNS {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() { pc.Close() })
	d := &rebindDNS{addr: pc.LocalAddr().String()}

	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return // closed
			}
			if n < 12 {
				continue
			}
			req := make([]byte, n)
			copy(req, buf[:n])
			if resp := buildDNSAnswer(req, d); resp != nil {
				pc.WriteTo(resp, addr)
			}
		}
	}()
	return d
}

// buildDNSAnswer turns a wire-format query into a response carrying one A
// record. Only enough of RFC 1035 to satisfy Go's resolver.
func buildDNSAnswer(q []byte, d *rebindDNS) []byte {
	// Walk the question's QNAME to find where the question section ends.
	i := 12
	for i < len(q) {
		l := int(q[i])
		if l == 0 {
			i++
			break
		}
		if l >= 0xc0 { // compression pointer, not expected in a query
			return nil
		}
		i += l + 1
	}
	if i+4 > len(q) {
		return nil
	}
	qtype := int(q[i])<<8 | int(q[i+1])
	i += 4 // QTYPE + QCLASS
	question := q[12:i]

	resp := make([]byte, 0, len(q)+16)
	resp = append(resp, q[0], q[1]) // transaction ID
	resp = append(resp, 0x81, 0x80) // response, recursion available, no error
	resp = append(resp, 0x00, 0x01) // QDCOUNT
	if qtype == 1 {                 // A
		resp = append(resp, 0x00, 0x01) // ANCOUNT
	} else {
		resp = append(resp, 0x00, 0x00) // NODATA for AAAA and friends
	}
	resp = append(resp, 0x00, 0x00) // NSCOUNT
	resp = append(resp, 0x00, 0x00) // ARCOUNT
	resp = append(resp, question...)
	if qtype != 1 {
		return resp
	}
	ip := d.answer().To4()
	resp = append(resp, 0xc0, 0x0c)             // name pointer to the question
	resp = append(resp, 0x00, 0x01)             // TYPE A
	resp = append(resp, 0x00, 0x01)             // CLASS IN
	resp = append(resp, 0x00, 0x00, 0x00, 0x00) // TTL 0, which is what a rebind uses
	resp = append(resp, 0x00, 0x04)             // RDLENGTH
	resp = append(resp, ip...)
	return resp
}
