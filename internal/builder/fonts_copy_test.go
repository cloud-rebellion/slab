package builder

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bright-interaction/slab/internal/store"
)

func TestEmitFontFiles(t *testing.T) {
	srcDir := t.TempDir()
	wsDir := t.TempDir()

	good := filepath.Join(srcDir, "aaa.woff2")
	if err := os.WriteFile(good, []byte("woff2-bytes"), 0o644); err != nil {
		t.Fatalf("write src font: %v", err)
	}

	rows := []store.SiteFont{
		{ID: "aaa111", SiteID: "site1", FilePath: good},
		{ID: "bbb222", SiteID: "site1", FilePath: filepath.Join(srcDir, "missing.woff2")},
		{ID: "ccc333", SiteID: "site1", FilePath: ""},
	}

	if err := emitFontFiles(rows, "site1", wsDir); err != nil {
		t.Fatalf("emitFontFiles: %v", err)
	}

	emitted := filepath.Join(wsDir, "public", "atomicsite-fonts", "site1", "aaa111.woff2")
	got, err := os.ReadFile(emitted)
	if err != nil {
		t.Fatalf("expected emitted font at %s: %v", emitted, err)
	}
	if string(got) != "woff2-bytes" {
		t.Fatalf("emitted font content mismatch: %q", got)
	}

	for _, id := range []string{"bbb222", "ccc333"} {
		p := filepath.Join(wsDir, "public", "atomicsite-fonts", "site1", id+".woff2")
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("degenerate row %s should not emit a file (err=%v)", id, err)
		}
	}
}
