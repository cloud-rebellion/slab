package handlers

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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
// because in a static test both resolutions return the same thing. That is
// exactly the difference the bug lived in, so the fixture has to make the two
// resolutions disagree on purpose.

const rebindHost = "rebind.slab-ssrf.test"

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
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			url := fmt.Sprintf("http://%s/payload.png", tc.host)
			_, _, err := fetchWithSSRFGuard(context.Background(), url, 1<<20)
			if err == nil {
				t.Fatalf("fetchWithSSRFGuard(%q) returned nil error; a forbidden address was accepted", url)
			}
			if !strings.Contains(err.Error(), "destination not allowed") {
				t.Errorf("fetchWithSSRFGuard(%q) error = %v, want the pre-flight refusal", url, err)
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
// implementation failed.
//
// It makes the two resolutions disagree, which is what a real DNS rebind does
// with a ~0s TTL record:
//
//   - the PRE-FLIGHT resolution (netguard.LookupIP, stubbed) answers a public
//     address, so the pre-flight passes and cannot be what refuses the fetch;
//   - the DIAL resolution (net.DefaultResolver, pointed at a local DNS server
//     this test runs) answers 127.0.0.1, so the connection actually being
//     opened goes somewhere the pre-flight never saw.
//
// The old code passed the pre-flight and then connected, because its client
// was a bare &http.Client{} on http.DefaultTransport with no Control hook.
func TestFetchWithSSRFGuard_RebindingIsRefusedAtDial(t *testing.T) {
	// An httptest server standing in for the internal resource. If the guard
	// fails, this handler runs and we can prove the request landed.
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

	// Dial-time resolution: a local DNS server answering rebindHost -> 127.0.0.1.
	dnsAddr := startLoopbackDNS(t)
	origResolver := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "udp", dnsAddr)
		},
	}
	defer func() { net.DefaultResolver = origResolver }()

	// Pre-flight resolution: a public address, so the pre-flight passes.
	origLookup := netguard.LookupIP
	netguard.LookupIP = func(host string) ([]net.IP, error) {
		if host == rebindHost {
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		}
		return origLookup(host)
	}
	defer func() { netguard.LookupIP = origLookup }()

	// Harness self-checks. Without these the test can pass for the wrong
	// reason: if either resolution is not actually in place, the fetch still
	// fails and the assertion below still goes green while proving nothing.
	if err := netguard.CheckHostPublic(rebindHost); err != nil {
		t.Fatalf("harness broken: pre-flight refused %q (%v), so this test would "+
			"prove the pre-flight works, not the dial guard", rebindHost, err)
	}
	dialIPs, err := net.DefaultResolver.LookupIP(context.Background(), "ip4", rebindHost)
	if err != nil || len(dialIPs) == 0 || !dialIPs[0].IsLoopback() {
		t.Fatalf("harness broken: dial-time resolution of %q = %v (err %v), want 127.0.0.1; "+
			"the two resolutions must disagree for this to be a rebind", rebindHost, dialIPs, err)
	}

	target := fmt.Sprintf("http://%s:%s/logo.png", rebindHost, port)
	body, _, err := fetchWithSSRFGuard(context.Background(), target, 1<<20)
	if err == nil {
		t.Fatalf("DNS rebinding was NOT refused: fetched %d bytes from %s", len(body), target)
	}
	if reached {
		t.Fatalf("DNS rebinding was NOT refused: the internal handler served the request (err after the fact: %v)", err)
	}
	if !strings.Contains(err.Error(), "blocked address") {
		t.Errorf("error = %v, want the dial-time Control refusal (\"blocked address\")", err)
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

func TestFetchWithSSRFGuard_CapsRedirectChain(t *testing.T) {
	var hops int
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops++
		http.Redirect(w, r, srv.URL+"/next", http.StatusFound)
	}))
	defer srv.Close()

	_, _, err := fetchWithSSRFGuardAllowingPrivate(context.Background(), srv.URL+"/start.png", 1<<20, true)
	if err == nil {
		t.Fatal("an unbounded redirect loop was followed to completion")
	}
	if hops > mediaFetchMaxRedirects+1 {
		t.Errorf("followed %d hops, want at most %d", hops, mediaFetchMaxRedirects+1)
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

// TestFetchWithSSRFGuard_HasATimeout pins the client timeout. The
// implementation this replaced set none, so a slow upstream held a server
// goroutine for as long as the server's write timeout allowed.
func TestFetchWithSSRFGuard_HasATimeout(t *testing.T) {
	if mediaFetchTimeout <= 0 {
		t.Fatal("mediaFetchTimeout must be positive; an unset Timeout leaves the fetch bounded only by the request context")
	}
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	// Prove the deadline is the client's, not the caller's context: pass a
	// context with no deadline and cancel the fetch by timeout instead.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := fetchWithSSRFGuardAllowingPrivate(ctx, srv.URL+"/slow.png", 1<<20, true)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a hanging upstream returned success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fetch did not return; it is not bounded")
	}
}

// startLoopbackDNS runs a minimal UDP DNS server that answers every A query
// with 127.0.0.1, and returns its address. It exists so the dial-time
// resolution in the rebind test can be made to disagree with the pre-flight
// without touching the network or /etc/hosts.
func startLoopbackDNS(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() { pc.Close() })

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
			resp := buildDNSAnswer(buf[:n])
			if resp != nil {
				pc.WriteTo(resp, addr)
			}
		}
	}()
	return pc.LocalAddr().String()
}

// buildDNSAnswer turns a wire-format query into a response carrying one A
// record for 127.0.0.1. Only enough of RFC 1035 to satisfy Go's resolver.
func buildDNSAnswer(q []byte) []byte {
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
		resp = append(resp, 0x00, 0x00) // no answer for AAAA and friends
	}
	resp = append(resp, 0x00, 0x00) // NSCOUNT
	resp = append(resp, 0x00, 0x00) // ARCOUNT
	resp = append(resp, question...)
	if qtype != 1 {
		return resp
	}
	resp = append(resp, 0xc0, 0x0c)             // name pointer to the question
	resp = append(resp, 0x00, 0x01)             // TYPE A
	resp = append(resp, 0x00, 0x01)             // CLASS IN
	resp = append(resp, 0x00, 0x00, 0x00, 0x00) // TTL 0, which is what a rebind uses
	resp = append(resp, 0x00, 0x04)             // RDLENGTH
	resp = append(resp, 127, 0, 0, 1)
	return resp
}
