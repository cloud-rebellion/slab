package deploy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeDist(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
}

func TestLocalDeployer_Validate(t *testing.T) {
	parentOK := t.TempDir()
	cases := []struct {
		name    string
		config  map[string]any
		wantErr string
	}{
		{
			name:    "missing path",
			config:  map[string]any{},
			wantErr: "path is required",
		},
		{
			name:    "relative path",
			config:  map[string]any{"path": "relative/site"},
			wantErr: "must be absolute",
		},
		{
			name:    "root path",
			config:  map[string]any{"path": "/"},
			wantErr: "must not be \"/\"",
		},
		{
			name:    "parent missing",
			config:  map[string]any{"path": "/this/parent/does/not/exist/site"},
			wantErr: "parent dir",
		},
		{
			name:   "ok",
			config: map[string]any{"path": filepath.Join(parentOK, "site")},
		},
	}
	d := &LocalDeployer{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := d.Validate(Target{Kind: "local", Config: tc.config})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestLocalDeployer_Validate_AcceptsSlugPath covers the wildcard serving
// convention: a tenant is served from /srv/atomicsite/{slug}/dist, so a path
// carrying the site's slug (not its 24-hex id) must validate. Also confirms a
// path scoped to neither slug nor id is rejected (cross-tenant guard).
func TestLocalDeployer_Validate_AcceptsSlugPath(t *testing.T) {
	parent := t.TempDir()
	d := &LocalDeployer{}

	okSlug := Target{
		Kind: "local", SiteID: "517e0000000000000000000d", Slug: "demo-1700000000",
		Config: map[string]any{"path": filepath.Join(parent, "demo-1700000000", "dist")},
	}
	if err := d.Validate(okSlug); err != nil {
		t.Fatalf("slug-scoped path should validate, got: %v", err)
	}

	wrong := Target{
		Kind: "local", SiteID: "517e0000000000000000000d", Slug: "demo-1700000000",
		Config: map[string]any{"path": filepath.Join(parent, "someone-elses-site", "dist")},
	}
	if err := d.Validate(wrong); err == nil {
		t.Fatal("path scoped to neither slug nor site id must be rejected")
	}
}

func TestLocalDeployer_Deploy_HappyPath(t *testing.T) {
	dist := t.TempDir()
	writeDist(t, dist, map[string]string{
		"index.html":            "<h1>hello</h1>",
		"about/index.html":      "<h1>about</h1>",
		"_app/immutable/x.css":  "body{}",
	})

	parent := t.TempDir()
	dest := filepath.Join(parent, "s1", "site")

	d := &LocalDeployer{}
	res, err := d.Deploy(context.Background(), dist, Target{
		ID: "t1", SiteID: "s1", Kind: "local",
		Config: map[string]any{"path": dest, "public_url": "https://example.com"},
	})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if res.URL != "https://example.com" {
		t.Fatalf("url: got %q", res.URL)
	}
	if res.FileCount != 3 {
		t.Fatalf("file count: got %d want 3", res.FileCount)
	}
	if res.SizeBytes <= 0 {
		t.Fatalf("size bytes: got %d", res.SizeBytes)
	}

	// Files exist at dest.
	for _, rel := range []string{"index.html", "about/index.html", "_app/immutable/x.css"} {
		if _, err := os.Stat(filepath.Join(dest, rel)); err != nil {
			t.Fatalf("missing %s: %v", rel, err)
		}
	}

	// .next and .prev are gone.
	for _, suffix := range []string{".next", ".prev"} {
		if _, err := os.Stat(dest + suffix); !os.IsNotExist(err) {
			t.Fatalf("expected %s gone, stat err: %v", dest+suffix, err)
		}
	}
}

func TestLocalDeployer_Deploy_AtomicSwap(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, "s1", "site")

	// Pre-existing tree with stale content + a sentinel file that should disappear.
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("mkdir dest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dest, "stale.txt"), []byte("old"), 0o644); err != nil {
		t.Fatalf("write stale: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dest, "index.html"), []byte("old"), 0o644); err != nil {
		t.Fatalf("write old index: %v", err)
	}

	dist := t.TempDir()
	writeDist(t, dist, map[string]string{
		"index.html": "<h1>new</h1>",
		"new.txt":    "new",
	})

	d := &LocalDeployer{}
	if _, err := d.Deploy(context.Background(), dist, Target{
		ID: "t2", SiteID: "s1", Kind: "local",
		Config: map[string]any{"path": dest},
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	// New content present.
	body, err := os.ReadFile(filepath.Join(dest, "index.html"))
	if err != nil {
		t.Fatalf("read new index: %v", err)
	}
	if string(body) != "<h1>new</h1>" {
		t.Fatalf("index.html body: got %q", string(body))
	}
	if _, err := os.Stat(filepath.Join(dest, "new.txt")); err != nil {
		t.Fatalf("missing new.txt: %v", err)
	}

	// Stale file gone.
	if _, err := os.Stat(filepath.Join(dest, "stale.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected stale.txt gone, stat err: %v", err)
	}

	// .next and .prev cleaned up.
	for _, suffix := range []string{".next", ".prev"} {
		if _, err := os.Stat(dest + suffix); !os.IsNotExist(err) {
			t.Fatalf("expected %s gone, stat err: %v", dest+suffix, err)
		}
	}
}

func TestNew_KnownKinds(t *testing.T) {
	for _, kind := range []string{"local", "rsync", "dockyard"} {
		got, err := New(kind)
		if err != nil {
			t.Fatalf("New(%q): %v", kind, err)
		}
		if got.Kind() != kind {
			t.Fatalf("New(%q).Kind() = %q", kind, got.Kind())
		}
	}
	if _, err := New("nope"); err == nil {
		t.Fatalf("New(\"nope\"): want error")
	}
}
