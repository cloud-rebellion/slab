package domains

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/slab/internal/store"
)

func tempEdge(t *testing.T) *EdgeWriter {
	t.Helper()
	dir := t.TempDir()
	confDir := filepath.Join(dir, "conf.d")
	sitesDir := filepath.Join(dir, "sites-enabled")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sitesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return &EdgeWriter{
		NginxConfDir:   confDir,
		NginxSitesDir:  sitesDir,
		AcmeWebrootDir: "/var/www/acme",
		SlugSuffix:     ".atomicsite.example.com",
		HeadersSnippet: "/etc/nginx/snippets/atomicsite-headers.conf",
		CaddyUpstream:  "127.0.0.1:8080",
	}
}

func TestEdgeWriter_DisabledIsNoOp(t *testing.T) {
	t.Parallel()
	e := &EdgeWriter{} // empty: Enabled() == false
	if e.Enabled() {
		t.Fatal("empty writer should not be Enabled")
	}
	if err := e.WriteAcmeBlock("http://127.0.0.1:8091"); err != nil {
		t.Errorf("disabled WriteAcmeBlock returned err: %v", err)
	}
	if err := e.WriteDomainMap(nil, nil); err != nil {
		t.Errorf("disabled WriteDomainMap returned err: %v", err)
	}
	d := store.SiteDomain{ID: "x", Hostname: "h.example.com"}
	if err := e.WriteDomainVhost(d, "slug", "/etc/letsencrypt/live/h.example.com"); err != nil {
		t.Errorf("disabled WriteDomainVhost returned err: %v", err)
	}
}

func TestEdgeWriter_WriteAcmeBlock(t *testing.T) {
	t.Parallel()
	e := tempEdge(t)
	if err := e.WriteAcmeBlock("http://127.0.0.1:8091"); err != nil {
		t.Fatalf("WriteAcmeBlock: %v", err)
	}
	body, err := os.ReadFile(e.AcmeBlockPath())
	if err != nil {
		t.Fatalf("read acme block: %v", err)
	}
	s := string(body)
	for _, want := range []string{
		"listen 80 default_server",
		"/.well-known/acme-challenge/",
		"/.well-known/atomic-verify/",
		e.AcmeWebrootDir,
		"http://127.0.0.1:8091",
		"return 301 https://$host$request_uri",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("acme block missing %q", want)
		}
	}
	// Idempotency: a second write with the same upstream is a no-op
	// in terms of bytes. Capture mtime to confirm.
	info1, _ := os.Stat(e.AcmeBlockPath())
	if err := e.WriteAcmeBlock("http://127.0.0.1:8091"); err != nil {
		t.Fatal(err)
	}
	info2, _ := os.Stat(e.AcmeBlockPath())
	if info1.ModTime() != info2.ModTime() {
		t.Errorf("idempotent write changed mtime: was %s, now %s", info1.ModTime(), info2.ModTime())
	}
}

func TestEdgeWriter_WriteDomainMap_FiltersByStatus(t *testing.T) {
	t.Parallel()
	e := tempEdge(t)
	domains := []store.SiteDomain{
		{ID: "1", SiteID: "s1", Hostname: "live.example.com", Status: "live"},
		{ID: "2", SiteID: "s2", Hostname: "ready.example.com", Status: "cert_ready"},
		{ID: "3", SiteID: "s1", Hostname: "pending.example.com", Status: "pending"},
		{ID: "4", SiteID: "s3", Hostname: "errored.example.com", Status: "error"},
	}
	sites := map[string]string{"s1": "tenant-one", "s2": "tenant-two", "s3": "tenant-three"}
	if err := e.WriteDomainMap(domains, sites); err != nil {
		t.Fatalf("WriteDomainMap: %v", err)
	}
	body, _ := os.ReadFile(e.DomainMapPath())
	s := string(body)
	if !strings.Contains(s, `"live.example.com" "tenant-one"`) {
		t.Errorf("expected live row in map: %s", s)
	}
	if !strings.Contains(s, `"ready.example.com" "tenant-two"`) {
		t.Errorf("expected cert_ready row in map: %s", s)
	}
	if strings.Contains(s, "pending.example.com") {
		t.Errorf("pending domain leaked into map: %s", s)
	}
	if strings.Contains(s, "errored.example.com") {
		t.Errorf("errored domain leaked into map: %s", s)
	}
	if !strings.Contains(s, "default \"\";") {
		t.Errorf("expected default fallback in map")
	}
}

func TestEdgeWriter_WriteDomainVhost(t *testing.T) {
	t.Parallel()
	e := tempEdge(t)
	d := store.SiteDomain{
		ID:       "abc123",
		SiteID:   "site1",
		Hostname: "shop.example.org",
		Status:   "verified",
	}
	if err := e.WriteDomainVhost(d, "tenant-shop", "/etc/letsencrypt/live/shop.example.org"); err != nil {
		t.Fatalf("WriteDomainVhost: %v", err)
	}
	body, _ := os.ReadFile(e.VhostPath(d.ID))
	s := string(body)
	for _, want := range []string{
		"server_name shop.example.org",
		"listen 443 ssl",
		"include /etc/nginx/snippets/atomicsite-headers.conf",
		"ssl_certificate /etc/letsencrypt/live/shop.example.org/fullchain.pem",
		"ssl_certificate_key /etc/letsencrypt/live/shop.example.org/privkey.pem",
		"proxy_pass http://127.0.0.1:8080",
		"proxy_set_header Host tenant-shop.atomicsite.example.com",
		"X-Atomicsite-Custom-Host $host",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("vhost missing %q\n--- block ---\n%s", want, s)
		}
	}
}

func TestEdgeWriter_SweepOrphanVhosts(t *testing.T) {
	t.Parallel()
	e := tempEdge(t)

	// Write three vhosts.
	for _, id := range []string{"alpha", "beta", "gamma"} {
		d := store.SiteDomain{ID: id, Hostname: id + ".example.com"}
		if err := e.WriteDomainVhost(d, "slug-"+id, "/etc/letsencrypt/live/"+id+".example.com"); err != nil {
			t.Fatal(err)
		}
	}
	// Drop a non-atomicsite file in the same dir to make sure we
	// only sweep our own files.
	other := filepath.Join(e.NginxSitesDir, "other.conf")
	if err := os.WriteFile(other, []byte("# unrelated"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Live set contains only beta. alpha + gamma should disappear.
	live := map[string]struct{}{"beta": {}}
	removed, err := e.SweepOrphanVhosts(live)
	if err != nil {
		t.Fatalf("SweepOrphanVhosts: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}

	if _, err := os.Stat(e.VhostPath("alpha")); !os.IsNotExist(err) {
		t.Error("alpha vhost should have been removed")
	}
	if _, err := os.Stat(e.VhostPath("beta")); err != nil {
		t.Error("beta vhost should still exist")
	}
	if _, err := os.Stat(e.VhostPath("gamma")); !os.IsNotExist(err) {
		t.Error("gamma vhost should have been removed")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("unrelated file should not have been swept")
	}
}

func TestQuoteForMap(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"example.com":        `"example.com"`,
		"a-b.c.d":            `"a-b.c.d"`,
		`weird"hostname.com`: `"weird\"hostname.com"`,
	}
	for in, want := range cases {
		if got := quoteForMap(in); got != want {
			t.Errorf("quoteForMap(%q) = %q, want %q", in, got, want)
		}
	}
}
