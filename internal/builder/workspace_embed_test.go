package builder

import (
	"io/fs"
	"strings"
	"testing"
)

// The skeleton is embedded with `//go:embed all:skeleton`, which has no
// exclude syntax. Anyone who runs `bun install` inside internal/builder/skeleton
// silently bakes the whole dependency tree into the server binary and into the
// public Slab mirror. Nothing else catches that, so this test does.
func TestSkeletonEmbedHasNoInstalledDeps(t *testing.T) {
	err := fs.WalkDir(skeletonFS, skeletonRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "node_modules" {
			t.Errorf("node_modules is embedded in the skeleton at %s: run `rm -rf internal/builder/skeleton/node_modules` and rebuild", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking embedded skeleton: %v", err)
	}
}

// Every tenant build runs `bun install --frozen-lockfile` first and falls back
// to an unpinned `bun install` when that fails. Without a committed lockfile
// the fallback is the ONLY path, so all 309 packages resolve live from npm on
// every build: non-reproducible, and any malicious minor release runs its
// postinstall inside the build sandbox. The lockfile must ship with the
// skeleton for the frozen path to work.
func TestSkeletonEmbedShipsLockfile(t *testing.T) {
	data, err := skeletonFS.ReadFile(skeletonRoot + "/bun.lock")
	if err != nil {
		t.Fatalf("skeleton must embed bun.lock so builds are pinned and reproducible: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("embedded bun.lock is empty")
	}
	for _, pkg := range []string{"astro", "svelte", "@astrojs/sitemap", "@astrojs/svelte", "pagefind"} {
		if !strings.Contains(string(data), `"`+pkg+`@`) {
			t.Errorf("embedded bun.lock does not pin %q; regenerate it from internal/builder/skeleton/package.json", pkg)
		}
	}
}
