package analytics

import "testing"

func TestParseUA(t *testing.T) {
	tests := []struct {
		name    string
		ua      string
		browser string
		os      string
		device  string
	}{
		{
			"chrome-macos",
			"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
			"Chrome", "macOS", "desktop",
		},
		{
			"safari-macos",
			"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15",
			"Safari", "macOS", "desktop",
		},
		{
			"firefox-windows",
			"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:121.0) Gecko/20100101 Firefox/121.0",
			"Firefox", "Windows", "desktop",
		},
		{
			"edge-windows",
			"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36 Edg/120.0.2210.144",
			"Edge", "Windows", "desktop",
		},
		{
			"chrome-android-mobile",
			"Mozilla/5.0 (Linux; Android 13; Pixel 7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36",
			"Chrome", "Android", "mobile",
		},
		{
			"safari-iphone",
			"Mozilla/5.0 (iPhone; CPU iPhone OS 17_2 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.2 Mobile/15E148 Safari/604.1",
			"Safari", "iOS", "mobile",
		},
		{
			"safari-ipad",
			"Mozilla/5.0 (iPad; CPU OS 17_2 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.2 Mobile/15E148 Safari/604.1",
			"Safari", "iPadOS", "tablet",
		},
		{
			"googlebot",
			"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
			"Googlebot", "Bot", "bot",
		},
		{
			"gptbot",
			"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko); compatible; GPTBot/1.0; +https://openai.com/gptbot",
			"GPTBot", "Bot", "bot",
		},
		{
			"claudebot",
			"Mozilla/5.0 (compatible; ClaudeBot/1.0; +claudebot@anthropic.com)",
			"ClaudeBot", "Bot", "bot",
		},
		{
			"empty",
			"",
			"", "", "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, o, d := ParseUA(tc.ua)
			if b != tc.browser {
				t.Errorf("browser: got %q want %q", b, tc.browser)
			}
			if o != tc.os {
				t.Errorf("os: got %q want %q", o, tc.os)
			}
			if d != tc.device {
				t.Errorf("device: got %q want %q", d, tc.device)
			}
		})
	}
}

func TestParsePrimaryLanguage(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"en-US,en;q=0.9", "en-US"},
		{"sv-SE,sv;q=0.9,en;q=0.8", "sv-SE"},
		{"en", "en"},
		{"*", ""},
		{"", ""},
		{"  fr-FR  ,en;q=0.5", "fr-FR"},
	}
	for _, tc := range tests {
		got := ParsePrimaryLanguage(tc.in)
		if got != tc.want {
			t.Errorf("ParsePrimaryLanguage(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseUTMFromPath(t *testing.T) {
	tests := []struct {
		in      string
		s, m, c string
	}{
		{"/?utm_source=newsletter&utm_medium=email&utm_campaign=spring", "newsletter", "email", "spring"},
		{"/blog/post?utm_source=twitter", "twitter", "", ""},
		{"/no-query", "", "", ""},
		{"/?other=val", "", "", ""},
		{"/?utm_source=", "", "", ""},
	}
	for _, tc := range tests {
		s, m, c := ParseUTMFromPath(tc.in)
		if s != tc.s || m != tc.m || c != tc.c {
			t.Errorf("ParseUTMFromPath(%q) = (%q,%q,%q), want (%q,%q,%q)", tc.in, s, m, c, tc.s, tc.m, tc.c)
		}
	}
}
