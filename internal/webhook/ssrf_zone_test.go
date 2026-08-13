package webhook

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSSRFClient_RefusesZonedLiteral pins the bypass that
// `ip != nil && isPrivateIP(ip)` allowed.
//
// net.ParseIP returns nil for any address carrying a zone suffix, so the old
// hook's first conjunct was false and the whole guard evaluated to "allow".
// Measured before the fix, against an ordinary IPv4 loopback httptest server:
//
//	http://127.0.0.1:PORT/                  -> refused
//	http://[::ffff:127.0.0.1%25lo0]:PORT/   -> 200, handler ran
//
// The two forms reach the same socket. netguard.DialControl refuses anything
// it cannot parse as a literal, which is the fail-closed shape.
//
// NOTE ON THE HARNESS: ssrf_test.go has a package-level
// `func init() { allowPrivateTargets = true }`, which disables this guard for
// every test in the package. A test written the obvious way here passes
// whether or not the guard works, because the guard is not running. Turning it
// back on explicitly is what makes this test able to fail.
func TestSSRFClient_RefusesZonedLiteral(t *testing.T) {
	orig := allowPrivateTargets
	allowPrivateTargets = false
	defer func() { allowPrivateTargets = orig }()

	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.Write([]byte("INTERNAL"))
	}))
	defer srv.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("parse httptest addr: %v", err)
	}

	// Control: the unzoned form must be refused. If this stops failing, the
	// guard is off and every case below is vacuous.
	if _, err := ssrfSafeClient(5 * time.Second).Get(fmt.Sprintf("http://127.0.0.1:%s/", port)); err == nil {
		t.Fatal("harness broken: plain loopback was allowed, so the guard is not running and this test proves nothing")
	}

	zoned := []string{
		fmt.Sprintf("http://[::ffff:127.0.0.1%%25lo0]:%s/", port),
		fmt.Sprintf("http://[::1%%25lo0]:%s/", port),
		fmt.Sprintf("http://[fe80::1%%25lo0]:%s/", port),
	}
	for _, target := range zoned {
		t.Run(target, func(t *testing.T) {
			reached = false
			resp, err := ssrfSafeClient(5 * time.Second).Get(target)
			if err == nil {
				resp.Body.Close()
				t.Fatalf("zoned literal was dialled: %s returned %d", target, resp.StatusCode)
			}
			if reached {
				t.Fatalf("zoned literal reached the server: %s", target)
			}
			// The refusal must come from the guard, not from the connection
			// happening to fail. "connect: connection refused" would mean the
			// dial was attempted and the guard never spoke.
			if !strings.Contains(err.Error(), "refusing to connect") {
				t.Errorf("%s refused by the network, not by the guard: %v", target, err)
			}
		})
	}
}
