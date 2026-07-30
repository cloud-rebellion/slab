package handlers

import (
	"net"
	"strings"
	"testing"
)

// stubScreenshotResolver replaces the DNS seam for the duration of a test.
// Hosts in override resolve to the given addresses; everything else resolves to
// a public address, which isolates the allow-list assertions from what the
// machine's resolver happens to say. Without this, the subtests below passed or
// failed depending on whether example.com answered publicly, in a sandbox, or
// not at all.
func stubScreenshotResolver(t *testing.T, override map[string][]net.IP) {
	t.Helper()
	prev := screenshotLookupIP
	public := []net.IP{net.ParseIP("93.184.216.34")}
	screenshotLookupIP = func(host string) ([]net.IP, error) {
		if ips, ok := override[host]; ok {
			return ips, nil
		}
		return public, nil
	}
	t.Cleanup(func() { screenshotLookupIP = prev })
}

func TestValidateScreenshotURL(t *testing.T) {
	// Reset before / after each subtest, since the allow-list is
	// package-level state that survives across tests.
	t.Cleanup(func() { ConfigureScreenshotAllowList() })
	stubScreenshotResolver(t, nil)

	t.Run("loopback always passes regardless of config", func(t *testing.T) {
		ConfigureScreenshotAllowList()
		for _, raw := range []string{
			"http://localhost:8080/",
			"http://127.0.0.1:5173/preview",
			"https://[::1]/",
		} {
			if err := validateScreenshotURL(raw); err != nil {
				t.Errorf("loopback %q rejected: %v", raw, err)
			}
		}
	})

	t.Run("empty allow-list refuses public hosts", func(t *testing.T) {
		ConfigureScreenshotAllowList()
		err := validateScreenshotURL("https://example.com/")
		if err == nil {
			t.Fatal("empty allow-list accepted public host")
		}
		if !strings.Contains(err.Error(), "screenshot disabled") {
			t.Errorf("expected disabled-screenshot error, got %q", err.Error())
		}
	})

	t.Run("bare domain matches apex and subdomains, not siblings", func(t *testing.T) {
		ConfigureScreenshotAllowList("example.com")
		for _, ok := range []string{
			"https://example.com/",
			"https://www.example.com/",
			"https://foo.bar.example.com/path",
		} {
			if err := validateScreenshotURL(ok); err != nil {
				t.Errorf("expected %q to pass, got %v", ok, err)
			}
		}
		for _, bad := range []string{
			"https://sister-example.com/",
			"https://attacker.com/",
			"https://example.com.attacker.com/",
		} {
			if err := validateScreenshotURL(bad); err == nil {
				t.Errorf("expected %q to fail", bad)
			}
		}
	})

	t.Run("dot-prefixed entry is suffix-only (no apex match)", func(t *testing.T) {
		ConfigureScreenshotAllowList(".tenants.example.com")
		if err := validateScreenshotURL("https://acme.tenants.example.com/"); err != nil {
			t.Errorf("expected subdomain to pass: %v", err)
		}
		// Apex of the suffix itself is not on the allow-list when the
		// entry starts with a dot, since the operator typed a wildcard
		// shape on purpose.
		if err := validateScreenshotURL("https://tenants.example.com/"); err == nil {
			t.Error("expected suffix apex to fail under dot-prefixed entry")
		}
	})

	t.Run("multiple entries OR together", func(t *testing.T) {
		ConfigureScreenshotAllowList("example.com", ".tenants.other.com")
		for _, ok := range []string{
			"https://example.com/",
			"https://acme.tenants.other.com/",
		} {
			if err := validateScreenshotURL(ok); err != nil {
				t.Errorf("expected %q to pass: %v", ok, err)
			}
		}
		if err := validateScreenshotURL("https://other.com/"); err == nil {
			t.Error("expected non-match to fail")
		}
	})

	t.Run("scheme + parse rejects", func(t *testing.T) {
		ConfigureScreenshotAllowList("example.com")
		for _, bad := range []string{
			"",
			"file:///etc/passwd",
			"javascript:alert(1)",
			"https://",
		} {
			if err := validateScreenshotURL(bad); err == nil {
				t.Errorf("expected %q to fail", bad)
			}
		}
	})

	// The DNS-rebind guard is the reason validateScreenshotURL resolves at all,
	// and until this subtest existed nothing covered it: the allow-list cases
	// exercised it only incidentally, via whatever the real resolver returned.
	// An allow-listed hostname is not a trusted address, because the tenant who
	// owns the name chooses the A record.
	t.Run("allow-listed host that resolves into the private range is refused", func(t *testing.T) {
		ConfigureScreenshotAllowList("example.com")
		for name, addr := range map[string]string{
			"RFC 1918":                   "10.0.0.5",
			"link-local metadata":        "169.254.169.254",
			"loopback via DNS":           "127.0.0.1",
			"RFC 6598 carrier / tailnet": "100.100.100.100",
		} {
			stubScreenshotResolver(t, map[string][]net.IP{
				"example.com": {net.ParseIP(addr)},
			})
			err := validateScreenshotURL("https://example.com/")
			if err == nil {
				t.Errorf("%s: allow-listed host resolving to %s was accepted; that is the rebind hole", name, addr)
				continue
			}
			if !strings.Contains(err.Error(), "non-public") {
				t.Errorf("%s: refused for the wrong reason: %v", name, err)
			}
		}
	})

	// A host that does not resolve is deliberately allowed through: navigation
	// will simply fail, and refusing on a transient DNS error would break
	// legitimate screenshots. Pinning it so the fail-open stays intentional.
	t.Run("unresolvable allow-listed host passes rather than failing closed", func(t *testing.T) {
		ConfigureScreenshotAllowList("example.com")
		prev := screenshotLookupIP
		screenshotLookupIP = func(string) ([]net.IP, error) {
			return nil, &net.DNSError{Err: "no such host", Name: "example.com", IsNotFound: true}
		}
		t.Cleanup(func() { screenshotLookupIP = prev })
		if err := validateScreenshotURL("https://example.com/"); err != nil {
			t.Errorf("unresolvable host should pass the rebind check, got %v", err)
		}
	})

	t.Run("ScreenshotAllowedSuffixes returns a copy", func(t *testing.T) {
		ConfigureScreenshotAllowList("a.com", "b.com")
		got := ScreenshotAllowedSuffixes()
		if len(got) != 2 {
			t.Fatalf("expected 2 entries, got %d", len(got))
		}
		got[0] = "mutated"
		// Mutating the returned slice must not affect future lookups.
		again := ScreenshotAllowedSuffixes()
		if again[0] == "mutated" {
			t.Error("returned slice must be a copy, not a shared reference")
		}
	})
}
