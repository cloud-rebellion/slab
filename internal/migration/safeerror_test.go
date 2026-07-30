package migration

import (
	"errors"
	"strings"
	"testing"
)

// The real SSRF refusals and dial failures, verbatim from safefetch.go and the
// net package. None of these may reach a tenant-readable column: a tenant can
// point a hostname they control at an internal address, let the guard correctly
// refuse, then read back exactly what it resolved to.
func TestSafeErrorTextDoesNotDiscloseAddresses(t *testing.T) {
	leaky := []string{
		"refusing to connect to private address 10.73.42.8",
		"private address blocked: 100.100.100.100",
		"refusing to connect to private address 169.254.169.254",
		`Get "http://internal.svc:8080/sitemap.xml": dial tcp 10.0.0.5:8080: connect: connection refused`,
		"dial tcp [fd00::1]:443: connect: connection refused",
		"lookup internal.corp on 10.0.0.53:53: no such host",
		`Get "https://x/": context deadline exceeded`,
		"x509: certificate signed by unknown authority for 192.168.1.10",
	}
	for _, in := range leaky {
		got := SafeErrorText(errors.New(in))
		if ipLikeRE.MatchString(got) {
			t.Errorf("SafeErrorText(%q) still contains an address: %q", in, got)
		}
		// Nor may it echo the raw text wholesale.
		if got == in {
			t.Errorf("SafeErrorText passed %q through unchanged", in)
		}
		if got == "" {
			t.Errorf("SafeErrorText(%q) returned empty; the tenant gets no signal at all", in)
		}
	}
}

// Fail closed: an error shape nobody has audited must not be passed through,
// because that is precisely the one whose contents are unknown.
func TestSafeErrorTextFailsClosedOnUnknownShapes(t *testing.T) {
	got := SafeErrorText(errors.New("some brand new driver error mentioning ~/secret and 10.1.2.3"))
	if strings.Contains(got, "tom.isgren") || ipLikeRE.MatchString(got) {
		t.Errorf("an unrecognised error leaked detail: %q", got)
	}
	if SafeErrorText(nil) != "" {
		t.Error("nil error should produce an empty string")
	}
}

// The tenant still needs to know what to DO, or they file a support ticket for
// every typo. A guard that destroys all signal gets removed.
func TestSafeErrorTextKeepsActionableSignal(t *testing.T) {
	cases := map[string]string{
		"refusing to connect to private address 10.0.0.1": "publicly reachable",
		"dial tcp 1.2.3.4:80: connect: connection refused": "could not reach",
		"unsupported scheme ftp":                           "absolute http",
	}
	for in, want := range cases {
		if got := SafeErrorText(errors.New(in)); !strings.Contains(got, want) {
			t.Errorf("SafeErrorText(%q) = %q, want it to mention %q", in, got, want)
		}
	}
}

func TestSafeWarningsRedactsAddressesInPlace(t *testing.T) {
	in := []string{
		"page /about failed: dial tcp 10.0.0.5:8080: connect: connection refused",
		"skipped /contact: no image",
	}
	out := SafeWarnings(in)
	if len(out) != len(in) {
		t.Fatalf("SafeWarnings dropped warnings: %d -> %d", len(in), len(out))
	}
	if ipLikeRE.MatchString(out[0]) {
		t.Errorf("address survived in %q", out[0])
	}
	// The useful part must remain, so the tenant still knows which page.
	if !strings.Contains(out[0], "/about") {
		t.Errorf("redaction destroyed the actionable part: %q", out[0])
	}
	if out[1] != in[1] {
		t.Errorf("a clean warning was altered: %q -> %q", in[1], out[1])
	}
}
