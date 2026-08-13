package builder

import "testing"

// TestPageURLPath covers the join that produced malformed URLs in every
// llms.txt this builder has ever written.
//
// Page slugs are stored without a leading slash and the canonical domain is
// stored without a trailing one, so the old "domain + slug" concatenation
// emitted "https://example.comour-products". Every entry in the file was
// affected, including the homepage, whose slug "index" has to resolve to "/"
// because Astro serves src/pages/index.astro at the site root.
func TestPageURLPath(t *testing.T) {
	tests := []struct {
		name string
		slug string
		want string
	}{
		{"homepage slug", "index", "/"},
		{"homepage slug with slashes", "/index/", "/"},
		{"empty slug", "", "/"},
		{"root slug", "/", "/"},
		{"plain slug", "our-products", "/our-products/"},
		{"slug with leading slash", "/kontakt", "/kontakt/"},
		{"slug with trailing slash", "om-oss/", "/om-oss/"},
		{"slug with both slashes", "/miljo/", "/miljo/"},
		{"nested slug", "sv/insights/n8n-vs-zapier", "/sv/insights/n8n-vs-zapier/"},
		{"whitespace padded", "  recycling-programme  ", "/recycling-programme/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pageURLPath(tt.slug); got != tt.want {
				t.Errorf("pageURLPath(%q) = %q, want %q", tt.slug, got, tt.want)
			}
		})
	}
}

// TestPageURLPathJoinsCleanly is the regression the bug actually needs: the
// concatenation the caller performs must never fuse the host into the path.
func TestPageURLPathJoinsCleanly(t *testing.T) {
	const domain = "https://demo.example.com"
	for _, slug := range []string{"index", "our-products", "/kontakt", "om-oss/"} {
		got := domain + pageURLPath(slug)
		if len(got) <= len(domain) || got[len(domain)] != '/' {
			t.Errorf("domain+pageURLPath(%q) = %q, want a %q-rooted path after the host", slug, got, "/")
		}
	}
}
