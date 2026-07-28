package builder

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/slab/internal/store"
)

func TestBuildRedirectBlock_Empty(t *testing.T) {
	if got := BuildRedirectBlock(nil); got != "" {
		t.Errorf("expected empty output for nil input, got %q", got)
	}
	if got := BuildRedirectBlock([]store.Redirect{}); got != "" {
		t.Errorf("expected empty output for empty input, got %q", got)
	}
}

func TestBuildRedirectBlock_ExactMatchEmitsBothSlashVariants(t *testing.T) {
	got := BuildRedirectBlock([]store.Redirect{
		{FromPath: "/about-us", ToPath: "/about", StatusCode: 301},
	})
	wantLines := []string{
		"location = /about-us { return 301 /about; }",
		"location = /about-us/ { return 301 /about; }",
	}
	for _, w := range wantLines {
		if !strings.Contains(got, w) {
			t.Errorf("missing line %q in output:\n%s", w, got)
		}
	}
}

func TestBuildRedirectBlock_ChainCollapseSingleHop(t *testing.T) {
	// A -> B -> C should emit A -> C and B -> C (one hop max).
	got := BuildRedirectBlock([]store.Redirect{
		{FromPath: "/a", ToPath: "/b", StatusCode: 301},
		{FromPath: "/b", ToPath: "/c", StatusCode: 301},
	})
	if !strings.Contains(got, "location = /a { return 301 /c; }") {
		t.Errorf("chain A->B->C did not collapse to A->C:\n%s", got)
	}
	if !strings.Contains(got, "location = /b { return 301 /c; }") {
		t.Errorf("middle hop B->C missing:\n%s", got)
	}
	if strings.Contains(got, "return 301 /b;") {
		t.Errorf("uncollapsed A->B should not appear:\n%s", got)
	}
}

func TestBuildRedirectBlock_LoopDropped(t *testing.T) {
	// A -> B -> A is unservable; both rows must drop.
	got := BuildRedirectBlock([]store.Redirect{
		{FromPath: "/a", ToPath: "/b", StatusCode: 301},
		{FromPath: "/b", ToPath: "/a", StatusCode: 301},
	})
	if strings.Contains(got, "location = /a") || strings.Contains(got, "location = /b") {
		t.Errorf("looping redirects should be dropped, got:\n%s", got)
	}
}

func TestBuildRedirectBlock_RegexPassthrough(t *testing.T) {
	got := BuildRedirectBlock([]store.Redirect{
		{FromPath: "~ ^/blog/2020/(.*)$", ToPath: "/blog/$1", StatusCode: 301},
	})
	want := "location ~  ^/blog/2020/(.*)$ { return 301 /blog/$1; }"
	if !strings.Contains(got, want) {
		t.Errorf("regex redirect missing or wrong, want %q in:\n%s", want, got)
	}
}

func TestBuildRedirectBlock_StatusCodes(t *testing.T) {
	cases := []struct {
		stored int64
		want   string
	}{
		{301, "return 301"},
		{302, "return 302"},
		{307, "return 307"},
		{308, "return 308"},
		{999, "return 301"}, // invalid -> safe default
	}
	for _, c := range cases {
		got := BuildRedirectBlock([]store.Redirect{
			{FromPath: "/x", ToPath: "/y", StatusCode: c.stored},
		})
		if !strings.Contains(got, c.want) {
			t.Errorf("status %d: want %q in %q", c.stored, c.want, got)
		}
	}
}

func TestBuildRedirectBlock_410NoLocation(t *testing.T) {
	got := BuildRedirectBlock([]store.Redirect{
		{FromPath: "/dead", ToPath: "/ignored", StatusCode: 410},
	})
	if !strings.Contains(got, "location = /dead { return 410; }") {
		t.Errorf("410 should emit `return 410;` with no Location, got:\n%s", got)
	}
	if strings.Contains(got, "/ignored") {
		t.Errorf("410 must not include a target URL, got:\n%s", got)
	}
}

func TestBuildRedirectBlock_DeterministicOrder(t *testing.T) {
	// Two passes with input in different orders must produce identical output.
	in1 := []store.Redirect{
		{FromPath: "/c", ToPath: "/x", StatusCode: 301},
		{FromPath: "/a", ToPath: "/x", StatusCode: 301},
		{FromPath: "/b", ToPath: "/x", StatusCode: 301},
	}
	in2 := []store.Redirect{
		{FromPath: "/a", ToPath: "/x", StatusCode: 301},
		{FromPath: "/b", ToPath: "/x", StatusCode: 301},
		{FromPath: "/c", ToPath: "/x", StatusCode: 301},
	}
	if BuildRedirectBlock(in1) != BuildRedirectBlock(in2) {
		t.Errorf("output not deterministic across input orders")
	}
}

func TestBuildRedirectBlock_RootPathPreservesSlash(t *testing.T) {
	got := BuildRedirectBlock([]store.Redirect{
		{FromPath: "/", ToPath: "/home", StatusCode: 301},
	})
	if !strings.Contains(got, "location = / { return 301 /home; }") {
		t.Errorf("root path should emit single `=` location, got:\n%s", got)
	}
	// Should NOT emit a duplicate "location = //" line.
	if strings.Contains(got, "location = //") {
		t.Errorf("root path should not produce //, got:\n%s", got)
	}
}

func TestBuildRedirectBlock_ChainAcrossRegexBreaks(t *testing.T) {
	// Chain walking must not hop into a regex entry (we don't try to
	// pattern-match for chain detection - too risky).
	got := BuildRedirectBlock([]store.Redirect{
		{FromPath: "/a", ToPath: "/b", StatusCode: 301},
		{FromPath: "~ ^/b$", ToPath: "/c", StatusCode: 301},
	})
	// /a should still resolve to /b (not /c) because the next hop is regex.
	if !strings.Contains(got, "location = /a { return 301 /b; }") {
		t.Errorf("chain should stop at regex boundary, got:\n%s", got)
	}
}

// TestBuildRedirectBlock_HostileInputsDropped is the security-critical test:
// any path containing nginx structural chars (`{`, `}`, `;`, `"`, `\`,
// newlines, control chars) MUST be silently dropped. A `}` in from_path
// would otherwise close the location block and let the next characters
// inject arbitrary directives.
func TestBuildRedirectBlock_HostileInputsDropped(t *testing.T) {
	hostile := []store.Redirect{
		{FromPath: "/a}return 500;{", ToPath: "/x", StatusCode: 301},
		{FromPath: "/b\nreturn 500;", ToPath: "/x", StatusCode: 301},
		{FromPath: "/c;return 500;", ToPath: "/x", StatusCode: 301},
		{FromPath: "/d\"injected\"", ToPath: "/x", StatusCode: 301},
		{FromPath: "/e", ToPath: "/x}return 500;{", StatusCode: 301},
		{FromPath: "/f", ToPath: "/x\nreturn 500;", StatusCode: 301},
		{FromPath: "/g", ToPath: "//evil.com/", StatusCode: 301},       // not / prefix once normalized
		{FromPath: "no-slash", ToPath: "/x", StatusCode: 301},          // exact must start with /
		{FromPath: "/h", ToPath: "https://evil.com/", StatusCode: 301}, // off-site not allowed in L1
		{FromPath: "", ToPath: "/x", StatusCode: 301},
		{FromPath: "/i", ToPath: "", StatusCode: 301},
		{FromPath: "~", ToPath: "/x", StatusCode: 301}, // empty regex pattern
		{FromPath: "/j\x00", ToPath: "/x", StatusCode: 301},
	}
	// Mix in one safe row so the block is non-empty.
	all := append([]store.Redirect{
		{FromPath: "/safe", ToPath: "/dest", StatusCode: 301},
	}, hostile...)

	got := BuildRedirectBlock(all)

	for _, bad := range []string{
		"return 500", "evil.com", "injected", "\x00", "no-slash",
	} {
		if strings.Contains(got, bad) {
			t.Errorf("hostile content %q leaked into output:\n%s", bad, got)
		}
	}
	// Multi-line `}return 500;{` would split across lines if not dropped:
	if strings.Count(got, "}\n") != 0 && !strings.HasSuffix(strings.TrimSpace(got), "}") {
		// Above check is a safety net; the real assertions are the substring drops above.
		_ = got
	}
	if !strings.Contains(got, "location = /safe { return 301 /dest; }") {
		t.Errorf("safe row should still emit, got:\n%s", got)
	}
}

// TestBuildRedirectBlock_PrintableUnicodeAllowed verifies we don't over-block
// legitimate non-ASCII paths (Swedish, German, etc.) that real WordPress
// installs slug. Only structural and control chars should be rejected.
func TestBuildRedirectBlock_PrintableUnicodeAllowed(t *testing.T) {
	got := BuildRedirectBlock([]store.Redirect{
		{FromPath: "/blog/förändring", ToPath: "/blog/forandring", StatusCode: 301},
	})
	if !strings.Contains(got, "/blog/förändring") {
		t.Errorf("printable unicode path should pass through, got:\n%s", got)
	}
}

// TestRenderNginxConfig_EmitsRedirects is the end-to-end integration test:
// open in-memory SQLite, seed a site, insert real redirect rows via the
// store, run RenderNginxConfig, and verify the produced nginx.conf has the
// expected `location = / { return 301 /...; }` lines. This covers the wiring
// between RenderNginxConfig -> ListRedirectsBySite -> BuildRedirectBlock.
func TestRenderNginxConfig_EmitsRedirects(t *testing.T) {
	sqlDB, q := openSecurityTestDB(t)
	siteID := "abcdef0123456789abcdef10"
	seedSecuritySite(t, sqlDB, siteID)

	// Insert two redirects: one auto-from-slug-edit, one manual hostile row
	// that must be dropped by the safety filter.
	for _, r := range []store.CreateRedirectParams{
		{ID: "r1", SiteID: siteID, FromPath: "/old-about", ToPath: "/about", StatusCode: 301, IsAuto: 1},
		{ID: "r2", SiteID: siteID, FromPath: "/legacy", ToPath: "/new", StatusCode: 301, IsAuto: 0},
		{ID: "r3", SiteID: siteID, FromPath: "/evil}return 500;{", ToPath: "/x", StatusCode: 301, IsAuto: 0},
	} {
		if err := q.CreateRedirect(context.Background(), r); err != nil {
			t.Fatalf("seed redirect %s: %v", r.ID, err)
		}
	}

	dir := t.TempDir()
	if err := RenderNginxConfig(context.Background(), q, siteID, dir); err != nil {
		t.Fatalf("RenderNginxConfig: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "nginx.conf"))
	if err != nil {
		t.Fatalf("read nginx.conf: %v", err)
	}
	out := string(body)

	for _, want := range []string{
		"location = /old-about { return 301 /about; }",
		"location = /legacy { return 301 /new; }",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("nginx.conf missing %q in:\n%s", want, out)
		}
	}
	for _, bad := range []string{"return 500", "/evil"} {
		if strings.Contains(out, bad) {
			t.Errorf("hostile content %q leaked into nginx.conf:\n%s", bad, out)
		}
	}
}

// TestRenderNginxConfig_NoRedirectsKeepsOutputClean confirms zero redirects
// produces no leftover redirect comment block - byte-stable for sites that
// haven't migrated anything.
func TestRenderNginxConfig_NoRedirectsKeepsOutputClean(t *testing.T) {
	sqlDB, q := openSecurityTestDB(t)
	siteID := "abcdef0123456789abcdef11"
	seedSecuritySite(t, sqlDB, siteID)

	dir := t.TempDir()
	if err := RenderNginxConfig(context.Background(), q, siteID, dir); err != nil {
		t.Fatalf("RenderNginxConfig: %v", err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "nginx.conf"))
	if strings.Contains(string(body), "Redirects") {
		t.Errorf("redirect block should be absent when no rows exist")
	}
}

// TestRenderNginxConfig_NginxSyntaxValidates wraps the emitted snippet in a
// minimal valid nginx.conf and runs `nginx -t -c <file>`. This is the gold
// standard - if real nginx accepts it, the syntax is correct end to end.
// Skipped when nginx is not on PATH (e.g. CI without nginx installed).
func TestRenderNginxConfig_NginxSyntaxValidates(t *testing.T) {
	nginxBin, err := exec.LookPath("nginx")
	if err != nil {
		t.Skip("nginx not installed; skipping syntax validation")
	}

	sqlDB, q := openSecurityTestDB(t)
	siteID := "abcdef0123456789abcdef12"
	seedSecuritySite(t, sqlDB, siteID)

	for _, r := range []store.CreateRedirectParams{
		{ID: "n1", SiteID: siteID, FromPath: "/old", ToPath: "/new", StatusCode: 301, IsAuto: 1},
		{ID: "n2", SiteID: siteID, FromPath: "/blog/2020", ToPath: "/blog", StatusCode: 301, IsAuto: 0},
		{ID: "n3", SiteID: siteID, FromPath: "~ ^/archive/(.*)$", ToPath: "/$1", StatusCode: 301, IsAuto: 0},
		{ID: "n4", SiteID: siteID, FromPath: "/dead", ToPath: "/ignored", StatusCode: 410, IsAuto: 0},
	} {
		if err := q.CreateRedirect(context.Background(), r); err != nil {
			t.Fatalf("seed redirect: %v", err)
		}
	}

	dir := t.TempDir()
	if err := RenderNginxConfig(context.Background(), q, siteID, dir); err != nil {
		t.Fatalf("RenderNginxConfig: %v", err)
	}
	snippet, err := os.ReadFile(filepath.Join(dir, "nginx.conf"))
	if err != nil {
		t.Fatalf("read snippet: %v", err)
	}

	// Wrap snippet in a minimal full conf nginx will accept. Strip the
	// security-headers `add_header` lines that reference inline log_format
	// macros only valid inside http{} - those go through their own block
	// in production; here we test the redirect block syntax specifically.
	wrapped := buildTestableNginxConf(string(snippet), dir)
	confPath := filepath.Join(dir, "test.conf")
	if err := os.WriteFile(confPath, []byte(wrapped), 0o644); err != nil {
		t.Fatalf("write wrapped conf: %v", err)
	}

	// Use a writable prefix so nginx can create temp dirs under TempDir.
	prefix := filepath.Join(dir, "nginx-prefix")
	_ = os.MkdirAll(prefix, 0o755)
	cmd := exec.Command(nginxBin, "-t", "-p", prefix+"/", "-c", confPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("nginx -t failed:\n%s\n\nwrapped conf:\n%s", out, wrapped)
	}
	if !strings.Contains(string(out), "syntax is ok") || !strings.Contains(string(out), "test is successful") {
		t.Errorf("nginx -t did not affirm success:\n%s", out)
	}
}

// buildTestableNginxConf wraps the per-site snippet in the surrounding
// http{} / server{} scaffolding nginx needs. The snippet itself contains
// `add_header`, `location`, `gzip`, etc - all valid inside server{}.
// We strip the `log_format` and `access_log` lines because log_format must
// be at http{} level and the test path may not be writable; analytics is
// covered by its own tests.
func buildTestableNginxConf(snippet, prefixDir string) string {
	var kept []string
	skipBlock := false
	for _, line := range strings.Split(snippet, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "log_format ") {
			skipBlock = true
			continue
		}
		if skipBlock {
			if strings.HasSuffix(trimmed, "';") || trimmed == "" {
				skipBlock = false
			}
			continue
		}
		if strings.HasPrefix(trimmed, "access_log ") {
			continue
		}
		kept = append(kept, line)
	}
	// Point every runtime path nginx wants to open at the test's own temp dir.
	// `nginx -t` validates the config AND opens the pid file, so as an
	// unprivileged user it reports "syntax is ok" and then fails anyway with
	// open() "/run/nginx.pid" failed (13: Permission denied). That is the test
	// tripping over a path it does not care about, not a bad config: it failed
	// the slab CI run on 2026-07-28 with the syntax line reading ok directly
	// above the error. GitHub's ubuntu-latest ships nginx, so this only shows up
	// where the test actually runs rather than skipping.
	return "pid " + filepath.Join(prefixDir, "nginx.pid") + ";\n" +
		"error_log " + filepath.Join(prefixDir, "error.log") + ";\n" +
		"events {}\n" +
		"http {\n" +
		"  client_body_temp_path " + filepath.Join(prefixDir, "client_body") + ";\n" +
		"  proxy_temp_path " + filepath.Join(prefixDir, "proxy") + ";\n" +
		"  fastcgi_temp_path " + filepath.Join(prefixDir, "fastcgi") + ";\n" +
		"  uwsgi_temp_path " + filepath.Join(prefixDir, "uwsgi") + ";\n" +
		"  scgi_temp_path " + filepath.Join(prefixDir, "scgi") + ";\n" +
		"  server {\n" +
		"    listen 8080;\n" +
		"    server_name _;\n" +
		strings.Join(kept, "\n") + "\n" +
		"  }\n" +
		"}\n"
}
