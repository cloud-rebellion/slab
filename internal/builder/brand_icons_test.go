package builder

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteBrandIcons_EmitsBothRootIcons(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "original.png")
	payload := []byte("\x89PNG\r\n\x1a\nfake-favicon-bytes")
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	wsDir := filepath.Join(tmp, "ws")

	if err := writeBrandIcons(src, wsDir); err != nil {
		t.Fatalf("writeBrandIcons: %v", err)
	}

	for _, name := range []string{"favicon.ico", "apple-touch-icon.png"} {
		got, err := os.ReadFile(filepath.Join(wsDir, "public", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != string(payload) {
			t.Errorf("%s: content mismatch (got %d bytes, want %d)", name, len(got), len(payload))
		}
	}
}

func TestWriteBrandIcons_OverwritesExisting(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "original.png")
	if err := os.WriteFile(src, []byte("new-icon"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	wsDir := filepath.Join(tmp, "ws")
	pubDir := filepath.Join(wsDir, "public")
	if err := os.MkdirAll(pubDir, 0o755); err != nil {
		t.Fatalf("mkdir public: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pubDir, "favicon.ico"), []byte("stale"), 0o644); err != nil {
		t.Fatalf("seed stale favicon: %v", err)
	}

	if err := writeBrandIcons(src, wsDir); err != nil {
		t.Fatalf("writeBrandIcons: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(pubDir, "favicon.ico"))
	if err != nil {
		t.Fatalf("read favicon.ico: %v", err)
	}
	if string(got) != "new-icon" {
		t.Errorf("favicon.ico not overwritten: got %q", got)
	}
}

func TestWriteBrandIcons_MissingSourceErrors(t *testing.T) {
	tmp := t.TempDir()
	if err := writeBrandIcons(filepath.Join(tmp, "nope.png"), filepath.Join(tmp, "ws")); err == nil {
		t.Fatal("expected error for missing source file")
	}
}
