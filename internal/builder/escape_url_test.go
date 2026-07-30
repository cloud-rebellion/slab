package builder

import (
	"strings"
	"testing"
)

// escapeURL's scheme check used TrimSpace, which removes LEADING whitespace but
// nothing embedded, so "java\tscript:alert(1)" did not match the "javascript:"
// prefix. Browsers strip ASCII tab, LF and CR from a URL BEFORE resolving its
// scheme, so the stripped form is what executes. 21 call sites feed this.
func TestEscapeURLBlocksSchemesHiddenByControlCharacters(t *testing.T) {
	blocked := []string{
		"javascript:alert(1)",
		"JaVaScRiPt:alert(1)",
		"  javascript:alert(1)",
		"java\tscript:alert(1)",
		"java\nscript:alert(1)",
		"java\rscript:alert(1)",
		"java script:alert(1)",
		"\x01javascript:alert(1)",
		"\x00javascript:alert(1)",
		"\x0bjavascript:alert(1)",
		"j\ta\nv\ra script:alert(1)",
		"vbscript:msgbox(1)",
		"data:text/html;base64,PHNjcmlwdD4=",
		"blob:https://evil.tld/x",
	}
	for _, in := range blocked {
		if got := escapeURL(in); got != "#" {
			t.Errorf("escapeURL(%q) = %q, want \"#\": the scheme is reachable once the browser strips controls", in, got)
		}
	}

	// Legitimate URLs must survive, or every link on every site breaks.
	for _, in := range []string{
		"https://example.com/a?b=c#d",
		"http://example.com",
		"/relative/path",
		"mailto:someone@example.com",
		"tel:+46701234567",
		"#anchor",
		"",
	} {
		if got := escapeURL(in); got == "#" && in != "#anchor" && in != "" {
			t.Errorf("escapeURL(%q) = %q: a legitimate URL was blocked", in, got)
		}
	}

	// Attribute escaping still applies on the pass-through path.
	if got := escapeURL(`https://example.com/"onmouseover="alert(1)`); strings.Contains(got, `"`) {
		t.Errorf("escapeURL left a raw quote in an href: %q", got)
	}
}
