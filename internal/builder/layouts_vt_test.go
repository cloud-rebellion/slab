package builder

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderLayouts_ViewTransitionsGatedByFidelity pins the showcase
// ClientRouter contract: the router and the consent-banner re-append
// script ship ONLY in showcase layouts (balanced stays byte-free of
// them), and when CookieProof is enabled a showcase layout MUST carry
// the re-append script (consent is a fidelity invariant; without it a
// soft navigation permanently wipes the banner for unconsented
// visitors).
func TestRenderLayouts_ViewTransitionsGatedByFidelity(t *testing.T) {
	sqlDB, queries := openFXTestDB(t)
	ctx := context.Background()
	const siteID = "vtsite00000000000000001"
	if _, err := sqlDB.Exec(`INSERT INTO sites (id, name, slug) VALUES (?, 'VT', 'vt')`, siteID); err != nil {
		t.Fatalf("seed site: %v", err)
	}
	set := func(category, key, value string) {
		if _, err := sqlDB.Exec(`INSERT INTO site_settings (id, site_id, category, key, value) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(site_id, category, key) DO UPDATE SET value=excluded.value`,
			category+key, siteID, category, key, value); err != nil {
			t.Fatalf("set %s.%s: %v", category, key, err)
		}
	}
	set("analytics", "cookieproof_enabled", "1")

	render := func() string {
		ws := t.TempDir()
		if err := RenderLayouts(ctx, queries, siteID, ws); err != nil {
			t.Fatalf("RenderLayouts: %v", err)
		}
		out, err := os.ReadFile(filepath.Join(ws, "src", "layouts", "Base.astro"))
		if err != nil {
			t.Fatalf("read layout: %v", err)
		}
		return string(out)
	}

	balanced := render()
	if strings.Contains(balanced, "ClientRouter") || strings.Contains(balanced, "astro:before-swap") {
		t.Fatal("balanced layout must not ship the ClientRouter or the consent re-append script")
	}

	set("design", "fidelity", "showcase")
	showcase := render()
	if !strings.Contains(showcase, "<ClientRouter />") {
		t.Fatal("showcase layout missing the ClientRouter")
	}
	for _, want := range []string{"astro:before-swap", "astro:after-swap", "cookie-consent"} {
		if !strings.Contains(showcase, want) {
			t.Fatalf("showcase layout missing consent re-append piece %q", want)
		}
	}
}
