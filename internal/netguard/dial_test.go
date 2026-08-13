package netguard

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// netguard_test.go covers IsForbidden, the PREDICATE. This file covers the
// helpers in dial.go, the two places callers actually ask it. Until this file
// existed only the predicate was pinned, so a caller could hold a correct
// verdict and still apply it at the wrong point in the request lifecycle,
// which is exactly what internal/handlers/media.go did: it ran the pre-flight
// and then dialled through a transport with no Control hook.

func withLookup(t *testing.T, fn func(string) ([]net.IP, error)) {
	t.Helper()
	orig := LookupIP
	LookupIP = fn
	t.Cleanup(func() { LookupIP = orig })
}

func TestCheckHostPublic_Literals(t *testing.T) {
	tests := []struct {
		host       string
		wantRefuse bool
	}{
		{"127.0.0.1", true},
		{"169.254.169.254", true},
		{"10.1.2.3", true},
		{"100.64.1.1", true},
		{"::1", true},
		{"::ffff:127.0.0.1", true},
		{"fd00::1", true},
		{"64:ff9b::7f00:1", true},
		{"93.184.216.34", false},
		{"2606:2800:220:1:248:1893:25c8:1946", false},
	}
	for _, tc := range tests {
		t.Run(tc.host, func(t *testing.T) {
			// No DNS should be consulted for a literal. Make it fatal if it is.
			withLookup(t, func(string) ([]net.IP, error) {
				t.Error("CheckHostPublic resolved an IP literal instead of judging it directly")
				return nil, errors.New("should not be called")
			})
			err := CheckHostPublic(tc.host)
			if tc.wantRefuse && err == nil {
				t.Errorf("CheckHostPublic(%q) = nil, want refusal", tc.host)
			}
			if !tc.wantRefuse && err != nil {
				t.Errorf("CheckHostPublic(%q) = %v, want nil", tc.host, err)
			}
		})
	}
}

func TestCheckHostPublic_LocalhostWithoutDNS(t *testing.T) {
	// localhost's presence in DNS depends on nsswitch, so resolution is not a
	// reliable way to catch it. Refuse the name itself, and prove the refusal
	// does not depend on a lookup by making a lookup fail the test.
	withLookup(t, func(string) ([]net.IP, error) {
		t.Error("CheckHostPublic resolved a localhost form instead of refusing it by name")
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	})
	for _, host := range []string{"localhost", "LocalHost", "app.localhost", "localhost."} {
		if err := CheckHostPublic(host); err == nil {
			t.Errorf("CheckHostPublic(%q) = nil, want refusal", host)
		}
	}
}

func TestCheckHostPublic_FailsClosed(t *testing.T) {
	t.Run("lookup error", func(t *testing.T) {
		withLookup(t, func(string) ([]net.IP, error) { return nil, errors.New("SERVFAIL") })
		if err := CheckHostPublic("example.com"); err == nil {
			t.Error("a failed lookup was treated as safe")
		}
	})
	t.Run("no records", func(t *testing.T) {
		withLookup(t, func(string) ([]net.IP, error) { return nil, nil })
		if err := CheckHostPublic("example.com"); err == nil {
			t.Error("a host with no records was treated as safe")
		}
	})
	t.Run("empty host", func(t *testing.T) {
		if err := CheckHostPublic(""); err == nil {
			t.Error("an empty host was treated as safe")
		}
	})
}

// TestCheckHostPublic_RefusesIfANYRecordIsForbidden pins the multi-record case.
// A host that answers with one public and one internal address is a rebind that
// needs no second query: the transport is free to pick either answer.
func TestCheckHostPublic_RefusesIfANYRecordIsForbidden(t *testing.T) {
	withLookup(t, func(string) ([]net.IP, error) {
		return []net.IP{
			net.ParseIP("93.184.216.34"),
			net.ParseIP("169.254.169.254"),
		}, nil
	})
	if err := CheckHostPublic("split.example.com"); err == nil {
		t.Fatal("a host answering with both a public and a metadata address was accepted")
	}
}

func TestDialControl(t *testing.T) {
	tests := []struct {
		name       string
		address    string
		wantRefuse bool
	}{
		{"loopback", "127.0.0.1:80", true},
		{"metadata", "169.254.169.254:80", true},
		{"cgnat", "100.64.0.1:443", true},
		{"ipv6 loopback", "[::1]:80", true},
		{"v4 mapped loopback", "[::ffff:127.0.0.1]:80", true},
		{"public", "93.184.216.34:443", false},
		{"missing port", "127.0.0.1", true},
		{"not a literal", "example.com:80", true},
		{"garbage", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := DialControl("tcp", tc.address, nil)
			if tc.wantRefuse && err == nil {
				t.Errorf("DialControl(%q) = nil, want refusal", tc.address)
			}
			if !tc.wantRefuse && err != nil {
				t.Errorf("DialControl(%q) = %v, want nil", tc.address, err)
			}
		})
	}
}

func TestCheckRedirect_CapsChain(t *testing.T) {
	check := CheckRedirect(3)
	req := &http.Request{URL: mustURL(t, "https://example.com/x")}
	withLookup(t, func(string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	})
	for n := 0; n < 3; n++ {
		if err := check(req, make([]*http.Request, n)); err != nil {
			t.Fatalf("hop %d refused early: %v", n, err)
		}
	}
	if err := check(req, make([]*http.Request, 3)); err == nil {
		t.Fatal("the redirect cap was not enforced")
	}
}

func TestCheckRedirect_ReChecksEachHopHost(t *testing.T) {
	check := CheckRedirect(3)
	req := &http.Request{URL: mustURL(t, "http://169.254.169.254/latest/meta-data/")}
	if err := check(req, nil); err == nil {
		t.Fatal("a redirect to the metadata address was allowed")
	} else if !strings.Contains(err.Error(), "redirect blocked") {
		t.Errorf("error = %v, want it labelled as a redirect refusal", err)
	}
}

// TestSafeTransport_RefusesAForbiddenDial is the integration fixture: it proves
// the Control hook is actually wired into what SafeTransport returns, rather
// than merely existing as a function. A transport that forgot to set Control
// would still satisfy every unit test above.
func TestSafeTransport_RefusesAForbiddenDial(t *testing.T) {
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	defer srv.Close()

	client := &http.Client{Transport: SafeTransport()}
	resp, err := client.Get(srv.URL) // httptest binds loopback
	if err == nil {
		resp.Body.Close()
		t.Fatal("SafeTransport connected to a loopback address")
	}
	if reached {
		t.Fatal("SafeTransport connected to a loopback address: the handler ran")
	}
	if !strings.Contains(err.Error(), "blocked address") {
		t.Errorf("error = %v, want the Control-hook refusal", err)
	}
}

func TestSafeDialer_HasControl(t *testing.T) {
	if SafeDialer().Control == nil {
		t.Fatal("SafeDialer returned a dialer with no Control hook, so every caller of it is pre-flight-only")
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}
