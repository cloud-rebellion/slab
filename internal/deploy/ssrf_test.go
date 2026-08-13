package deploy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bright-interaction/slab/internal/netguard"
)

// These tests pin audit finding 13: internal/deploy was the only
// tenant-URL-consuming package that never consulted netguard. dockyard_url and
// rsync host both come out of the deploy_targets config a tenant POSTs, and the
// only checks were "scheme is https" and "host is non-empty".
//
// Everything here must pass with the network unplugged: no test resolves a
// name that is not an IP literal or loopback.

const testServerID = "11111111-2222-3333-4444-555555555555"

// internalHosts are the addresses a tenant-supplied deploy target must never
// reach. 100.64.0.1 and 169.254.169.254 are the two that matter most here:
// carrier-grade NAT is the Tailscale range this estate runs on, and
// 169.254.169.254 is the cloud metadata endpoint.
var internalHosts = []string{
	"127.0.0.1",
	"10.0.0.5",
	"192.168.1.1",
	"172.16.0.1",
	"100.64.0.1",
	"169.254.169.254",
	"localhost",
	"[::1]",
	"[fc00::1]",
}

func TestDockyardValidate_RejectsInternalDockyardURL(t *testing.T) {
	for _, host := range internalHosts {
		t.Run(host, func(t *testing.T) {
			cfg := validDockyardConfig(t, "/var/atomicsite/site")
			cfg["dockyard_url"] = "https://" + host + "/"
			d := NewDockyardDeployer()
			err := d.Validate(Target{ID: "t1", SiteID: "s1", Name: "main", Kind: "dockyard", Config: cfg})
			if err == nil {
				t.Fatalf("Validate accepted dockyard_url pointing at %s", host)
			}
			if !strings.Contains(err.Error(), "dockyard_url") {
				t.Fatalf("error should name the offending field, got %q", err.Error())
			}
		})
	}
}

// A public hostname must still validate, and must do so without a DNS lookup:
// Validate runs on every target save, so making it network-dependent would let
// a slow resolver reject a legitimate config. dockyard.example.com does not
// resolve, which is exactly why it is the right probe.
func TestDockyardValidate_AllowsPublicHostnameOffline(t *testing.T) {
	cfg := validDockyardConfig(t, "/var/atomicsite/site")
	cfg["dockyard_url"] = "https://dockyard.example.com"
	d := NewDockyardDeployer()
	if err := d.Validate(Target{ID: "t1", SiteID: "s1", Name: "main", Kind: "dockyard", Config: cfg}); err != nil {
		t.Fatalf("Validate rejected a public hostname: %v", err)
	}
}

// The attack itself: point dockyard_url at an internal HTTP service and check
// whether the app server delivers the request. The assertion that matters is
// hits == 0, not that an error came back.
func TestDockyardClient_NeverReachesInternalAddress(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("INTERNAL-SERVICE-SECRET"))
	}))
	defer srv.Close()

	c := &httpDockyardClient{
		base: strings.TrimRight(srv.URL, "/"),
		hc:   newGuardedHTTPClient(dockyardHTTPTimeout, false),
	}
	err := c.PutProxyRoute(context.Background(), testServerID, proxyRouteRequest{Domain: "site.example.com"})
	if err == nil {
		t.Fatal("PutProxyRoute succeeded against a loopback address")
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("internal service was reached %d time(s); the guard did not hold", n)
	}
	if strings.Contains(err.Error(), "INTERNAL-SERVICE-SECRET") {
		t.Fatalf("upstream body leaked into the tenant-visible error: %q", err.Error())
	}
}

// Same for the apply call, which is the twin of PutProxyRoute inside the same
// client and was missing the same guard.
func TestDockyardClient_ApplyNeverReachesInternalAddress(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := &httpDockyardClient{
		base: strings.TrimRight(srv.URL, "/"),
		hc:   newGuardedHTTPClient(dockyardHTTPTimeout, false),
	}
	if err := c.ApplyProxy(context.Background(), testServerID); err == nil {
		t.Fatal("ApplyProxy succeeded against a loopback address")
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("internal service was reached %d time(s); the guard did not hold", n)
	}
}

// The dial-time half of the guard, tested directly so it does not depend on a
// hostname resolving anywhere. The pre-flight check is a TOCTOU on its own: the
// transport resolves again when it dials, so a record with a ~0s TTL can answer
// public for the pre-flight and private for the dial.
func TestGuardedHTTPClient_DialHookRefusesInternalAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	tr, ok := newGuardedHTTPClient(5*time.Second, false).Transport.(*http.Transport)
	if !ok {
		t.Fatal("guarded client transport is not an *http.Transport")
	}
	conn, err := tr.DialContext(context.Background(), "tcp", ln.Addr().String())
	if err == nil {
		_ = conn.Close()
		t.Fatal("dial hook allowed a connection to a loopback address")
	}
}

// Go's default is to follow ten redirects with no revalidation, so an upstream
// that passes the pre-flight can still walk the client to the metadata address.
func TestDockyardClient_BlocksRedirectToInternalAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()

	// allowPrivate lets the FIRST hop reach the loopback test server. The
	// redirect check is never relaxed, so the second hop must still be refused.
	c := &httpDockyardClient{
		base:         strings.TrimRight(srv.URL, "/"),
		hc:           newGuardedHTTPClient(dockyardHTTPTimeout, true),
		allowPrivate: true,
	}
	err := c.PutProxyRoute(context.Background(), testServerID, proxyRouteRequest{Domain: "site.example.com"})
	if err == nil {
		t.Fatal("redirect to the metadata address was followed")
	}
	if !strings.Contains(err.Error(), "redirect blocked") {
		t.Fatalf("expected a redirect-blocked error, got %q", err.Error())
	}
}

// The echo channel. handlers/build.go appends this error to the build log the
// tenant reads via get_build_status, so the upstream body must not be in it.
func TestDockyardClient_DoesNotEchoUpstreamBody(t *testing.T) {
	const secret = "INTERNAL-SERVICE-SECRET"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(secret))
	}))
	defer srv.Close()

	c := &httpDockyardClient{
		base:         strings.TrimRight(srv.URL, "/"),
		hc:           newGuardedHTTPClient(dockyardHTTPTimeout, true),
		allowPrivate: true,
	}

	routeErr := c.PutProxyRoute(context.Background(), testServerID, proxyRouteRequest{Domain: "site.example.com"})
	if routeErr == nil {
		t.Fatal("expected an error from a 404 response")
	}
	if strings.Contains(routeErr.Error(), secret) {
		t.Fatalf("PutProxyRoute echoed the upstream body: %q", routeErr.Error())
	}
	if !strings.Contains(routeErr.Error(), "404") {
		t.Fatalf("PutProxyRoute error should still carry the status, got %q", routeErr.Error())
	}

	applyErr := c.ApplyProxy(context.Background(), testServerID)
	if applyErr == nil {
		t.Fatal("expected an error from a 404 response")
	}
	if strings.Contains(applyErr.Error(), secret) {
		t.Fatalf("ApplyProxy echoed the upstream body: %q", applyErr.Error())
	}
	if !strings.Contains(applyErr.Error(), "404") {
		t.Fatalf("ApplyProxy error should still carry the status, got %q", applyErr.Error())
	}
}

// The twin surface. rsync's host is tenant-supplied too, and Deploy shells out
// to ssh with it, so an unguarded target is an SSH-shaped port probe of
// internal infrastructure whose stderr comes back in the build log.
func TestRsyncValidate_RejectsInternalHost(t *testing.T) {
	for _, host := range internalHosts {
		// hostnamePattern has always rejected bracketed IPv6 literals, so
		// those are covered by the existing hostname rule rather than here.
		if strings.HasPrefix(host, "[") {
			continue
		}
		t.Run(host, func(t *testing.T) {
			tgt := validRsyncTarget()
			tgt.Config["host"] = host
			d := NewRsyncDeployer()
			err := d.Validate(tgt)
			if err == nil {
				t.Fatalf("Validate accepted rsync host %s", host)
			}
			if !strings.Contains(err.Error(), "host") {
				t.Fatalf("error should name the offending field, got %q", err.Error())
			}
		})
	}
}

func TestRsyncDeploy_RefusesInternalHostBeforeExec(t *testing.T) {
	dist := t.TempDir()
	if err := os.WriteFile(filepath.Join(dist, "index.html"), []byte("ok"), 0o644); err != nil {
		t.Fatalf("write dist file: %v", err)
	}

	tgt := validRsyncTarget()
	tgt.Config["host"] = "169.254.169.254"
	d := NewRsyncDeployer()

	_, err := d.Deploy(context.Background(), dist, tgt)
	if err == nil {
		t.Fatal("Deploy ran rsync against the metadata address")
	}
	// "exit status" would mean rsync was actually spawned.
	if strings.Contains(err.Error(), "exit status") {
		t.Fatalf("rsync was executed before the guard ran: %q", err.Error())
	}
}

// TestRsyncDeploy_DeployTimeGuardIsTheOnlyLayerForAHostname closes the gap
// the test above leaves open. That test uses an IP literal, so
// checkHostOffline (inside Validate, which Deploy calls first) already
// refuses it via netguard.CheckHostPublic on the literal form, and the
// DEPLOY-TIME netguard.CheckHostPublic call at rsync.go:288 never has to
// fire at all; deleting it there leaves that test green.
//
// checkHostOffline deliberately never resolves DNS for a plain hostname
// (Validate runs on every target save and must not fail because a resolver
// is slow, see ssrf.go), so for a hostname target Validate has no opinion on
// where it points. The Deploy-time call is the only place in this whole
// path that ever resolves one, which is exactly the DNS-rebind case
// netguard is built for: a destination that looks fine because nothing has
// checked it yet, and turns out to resolve internal the one time something
// finally does.
//
// netguard.LookupIP is the seam, same shape as
// internal/handlers/screenshot.go's screenshotLookupIP: it lets this test
// control what the hostname resolves to without a real DNS record.
func TestRsyncDeploy_DeployTimeGuardIsTheOnlyLayerForAHostname(t *testing.T) {
	const host = "rebind.tenant-target.invalid"

	prev := netguard.LookupIP
	netguard.LookupIP = func(h string) ([]net.IP, error) {
		if h == host {
			return []net.IP{net.ParseIP("169.254.169.254")}, nil // cloud metadata
		}
		return prev(h)
	}
	t.Cleanup(func() { netguard.LookupIP = prev })

	dist := t.TempDir()
	if err := os.WriteFile(filepath.Join(dist, "index.html"), []byte("ok"), 0o644); err != nil {
		t.Fatalf("write dist file: %v", err)
	}

	tgt := validRsyncTarget()
	tgt.Config["host"] = host
	d := NewRsyncDeployer()

	// Validate alone must not catch this: it is a plain hostname, and
	// checkHostOffline is documented to leave hostname resolution to the
	// connect-time guards. If this starts failing, Validate grew a DNS
	// check and the premise of this test (Deploy-time is the ONLY layer)
	// needs re-checking, not silencing.
	if err := d.Validate(tgt); err != nil {
		t.Fatalf("Validate rejected a plain hostname target (should defer to the connect-time guard): %v", err)
	}

	_, err := d.Deploy(context.Background(), dist, tgt)
	if err == nil {
		t.Fatal("Deploy ran rsync against a hostname resolving to the metadata address")
	}
	// "exit status" would mean rsync was actually spawned, i.e. the
	// Deploy-time guard did not run.
	if strings.Contains(err.Error(), "exit status") {
		t.Fatalf("rsync was executed before the Deploy-time guard ran: %q", err.Error())
	}
	if !strings.Contains(err.Error(), host) {
		t.Fatalf("error should name the offending host, got %q", err.Error())
	}
}
