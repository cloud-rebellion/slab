package builder

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

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

// llmsLinkRe pulls the URL out of each "- [Title](URL): desc" line RenderLLMsTxt
// emits for a page.
var llmsLinkRe = regexp.MustCompile(`(?m)^- \[[^\]]*\]\(([^)]*)\)`)

// TestRenderLLMsTxtEmitsWellFormedURLs is the guard the defect actually needed.
//
// TestPageURLPath and TestPageURLPathJoinsCleanly only exercise the helper, and
// the helper was never the thing that broke: RenderLLMsTxt's call site was. Both
// of those tests stay green when the caller is reverted to the old
// `domain + p.Slug` concatenation, which is the exact shape of the bug and the
// exact failure mode the audit warned about, a test agreeing with a doc while
// both disagree with reality. This one reads the file RenderLLMsTxt actually
// writes and parses every URL in it, so a regression at the call site fails here
// no matter how the path is spelled.
func TestRenderLLMsTxtEmitsWellFormedURLs(t *testing.T) {
	sqlDB, q := openSecurityTestDB(t)
	const siteID = "abcdef0123456789abcdef02"
	const host = "demo.slab.example.com"

	if _, err := sqlDB.Exec(
		`INSERT INTO sites (id, name, slug, domain, status) VALUES (?, ?, ?, ?, 'published')`,
		siteID, "Demo", "demo", host,
	); err != nil {
		t.Fatalf("seed site: %v", err)
	}

	// The homepage is stored as the slug "index"; the rest carry no leading
	// slash. Both storage shapes are what production holds today.
	pages := []struct{ id, title, slug, wantPath string }{
		{"p0000000000000000000001", "Hem", "index", "/"},
		{"p0000000000000000000002", "Vara plastkort", "our-products", "/our-products/"},
		{"p0000000000000000000003", "Kontakt", "/kontakt", "/kontakt/"},
		{"p0000000000000000000004", "Insights", "sv/insights/n8n-vs-zapier", "/sv/insights/n8n-vs-zapier/"},
	}
	for i, p := range pages {
		if _, err := sqlDB.Exec(
			`INSERT INTO pages (id, site_id, title, slug, status, sort_order) VALUES (?, ?, ?, ?, 'published', ?)`,
			p.id, siteID, p.title, p.slug, i,
		); err != nil {
			t.Fatalf("seed page %q: %v", p.slug, err)
		}
	}

	wsDir := t.TempDir()
	if err := RenderLLMsTxt(context.Background(), q, siteID, wsDir); err != nil {
		t.Fatalf("RenderLLMsTxt: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(wsDir, "public", "llms.txt"))
	if err != nil {
		t.Fatalf("read llms.txt: %v", err)
	}
	body := string(raw)

	matches := llmsLinkRe.FindAllStringSubmatch(body, -1)
	if len(matches) != len(pages) {
		t.Fatalf("llms.txt lists %d page links, want %d\n---\n%s", len(matches), len(pages), body)
	}

	// Anti-vacuity: if the file ever stops carrying the host at all, the
	// per-URL assertions below would pass over an empty claim.
	if !strings.Contains(body, host) {
		t.Fatalf("llms.txt never mentions the site host %q; the assertions below would be vacuous\n---\n%s", host, body)
	}

	for i, m := range matches {
		raw := m[1]
		want := pages[i].wantPath

		u, err := url.Parse(raw)
		if err != nil {
			t.Errorf("page %q: llms.txt URL %q does not parse: %v", pages[i].slug, raw, err)
			continue
		}
		if u.Scheme != "https" {
			t.Errorf("page %q: llms.txt URL %q has scheme %q, want https", pages[i].slug, raw, u.Scheme)
		}
		// This is the assertion the bug fails: "https://" + host + "index"
		// parses as host "demo.slab.example.comindex" with an
		// empty path, so the host check catches it even before the path check.
		if u.Host != host {
			t.Errorf("page %q: llms.txt URL %q has host %q, want %q (the slug has fused into the host)", pages[i].slug, raw, u.Host, host)
		}
		if u.Path != want {
			t.Errorf("page %q: llms.txt URL %q has path %q, want %q", pages[i].slug, raw, u.Path, want)
		}
	}
}
