package handlers

import "testing"

func TestConsentDomainMatches_ExactAndCase(t *testing.T) {
	if !consentDomainMatches("Example.com", "example.com", "") {
		t.Error("case-insensitive exact match should pass")
	}
	if !consentDomainMatches("example.com.", "example.com", "") {
		t.Error("trailing dot should be ignored")
	}
	if !consentDomainMatches("example.com:443", "example.com", "") {
		t.Error("port should be stripped")
	}
}

func TestConsentDomainMatches_WWWAndApex(t *testing.T) {
	if !consentDomainMatches("www.example.com", "example.com", "") {
		t.Error("www should match apex")
	}
	if !consentDomainMatches("example.com", "www.example.com", "") {
		t.Error("apex should match www")
	}
}

func TestConsentDomainMatches_Subdomain(t *testing.T) {
	if !consentDomainMatches("blog.example.com", "example.com", "") {
		t.Error("subdomain should match site domain")
	}
	if !consentDomainMatches("a.b.example.com", "example.com", "") {
		t.Error("nested subdomain should match")
	}
}

func TestConsentDomainMatches_CookieproofOverride(t *testing.T) {
	if !consentDomainMatches("foo.example.com", "", "example.com") {
		t.Error("cookieproof_domain should accept matching subdomain")
	}
}

func TestConsentDomainMatches_RejectMismatch(t *testing.T) {
	if consentDomainMatches("evil.com", "example.com", "") {
		t.Error("unrelated domain must be rejected")
	}
	if consentDomainMatches("example.com.evil.com", "example.com", "") {
		t.Error("substring trick must be rejected")
	}
	if consentDomainMatches("", "example.com", "") {
		t.Error("empty target must be rejected")
	}
}

func TestNormaliseDomain_StripsScheme(t *testing.T) {
	cases := map[string]string{
		"https://example.com":     "example.com",
		"HTTP://EXAMPLE.COM/path": "example.com",
		"  example.com  ":         "example.com",
		"example.com:8080":        "example.com",
		"example.com/foo?bar":     "example.com",
	}
	for in, want := range cases {
		if got := normaliseDomain(in); got != want {
			t.Errorf("normaliseDomain(%q) = %q want %q", in, got, want)
		}
	}
}
