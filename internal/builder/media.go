package builder

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/bright-interaction/slab/internal/store"
)

// CopyMedia hardlinks (or copies) the site's processed media into the Astro
// workspace's public/media/ so that Astro bundles them into dist/. URLs stored
// in blocks as /media/{mediaID}/... resolve both during dev-serve and in the
// built static site.
//
// Disk layout transformation:
//
//	src: {dataDir}/media/{siteID}/{mediaID}/{variant}.{ext}
//	dst: {wsDir}/public/media/{mediaID}/{variant}.{ext}
//
// The siteID segment is dropped because the workspace is always single-site.
func CopyMedia(ctx context.Context, queries *store.Queries, siteID, dataDir, wsDir string) error {
	srcDir := filepath.Join(dataDir, "media", siteID)
	dstDir := filepath.Join(wsDir, "public", "media")

	// If the site has no media, that's fine.
	if _, err := os.Stat(srcDir); os.IsNotExist(err) {
		return nil
	}

	// Ensure dst exists (parent of each mediaID dir)
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dstDir, err)
	}

	return filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dstDir, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return atomicLinkOrCopy(path, target)
	})
}

// CopyBrandIcons emits /favicon.ico and /apple-touch-icon.png into the
// workspace public/ root from the site's branding favicon media.
//
// layouts.go links both paths unconditionally on every page ("for
// site-inspector parity"), but nothing ever placed the files in dist, so
// every built site 404'd on them even with branding.favicon_id set
// (browsers showed the broken-tab icon; release hard checks failed).
//
// Degenerate states (no favicon_id, media row missing or cross-site,
// source file gone) are non-fatal by design: the build degrades exactly
// like before, link tags without files. Only a real IO failure while
// writing into the workspace errors.
//
// PNG bytes behind the favicon.ico name are fine: browsers sniff the
// icon payload rather than trusting the extension or the link type.
func CopyBrandIcons(ctx context.Context, queries *store.Queries, siteID, dataDir, wsDir string) error {
	site, err := queries.GetSiteByID(ctx, siteID)
	if err != nil || site.FaviconID == "" {
		return nil
	}
	m, err := queries.GetMediaByID(ctx, site.FaviconID)
	if err != nil || m.SiteID != siteID {
		slog.Warn("build: brand icons skipped (favicon media row missing)",
			"site_id", siteID, "favicon_id", site.FaviconID)
		return nil
	}
	// Same variant resolution as resolveOgImageURL: CopyMedia hardlinks
	// original.<ext> for every processed image; OriginalPath's basename
	// wins when set.
	base := "original" + extOf(m.Filename)
	if m.OriginalPath != "" {
		base = filepath.Base(m.OriginalPath)
	}
	src := filepath.Join(dataDir, "media", siteID, m.ID, base)
	if _, err := os.Stat(src); err != nil {
		slog.Warn("build: brand icons skipped (favicon file missing on disk)",
			"site_id", siteID, "src", src)
		return nil
	}
	return writeBrandIcons(src, wsDir)
}

// writeBrandIcons copies src into {wsDir}/public/ as favicon.ico and
// apple-touch-icon.png so the Astro build carries both into dist/.
func writeBrandIcons(src, wsDir string) error {
	pubDir := filepath.Join(wsDir, "public")
	if err := os.MkdirAll(pubDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", pubDir, err)
	}
	for _, name := range []string{"favicon.ico", "apple-touch-icon.png"} {
		if err := atomicLinkOrCopy(src, filepath.Join(pubDir, name)); err != nil {
			return fmt.Errorf("emit %s: %w", name, err)
		}
	}
	return nil
}

// CopyFonts emits every uploaded site font into the workspace public/ tree at
// /atomicsite-fonts/{siteID}/{fontID}.woff2, the exact URL the @font-face
// blocks in layouts.go reference on every page.
//
// The runtime route (FontsHandler.Serve) only exists on the admin-app origin;
// static deploys served from dist/ (the *.atomicsite wildcard host) 404'd on
// every font and silently fell back to system type. Same shape as
// CopyBrandIcons: degenerate states (no fonts, file missing on disk) are
// non-fatal, only a real IO failure while writing into the workspace errors.
func CopyFonts(ctx context.Context, queries *store.Queries, siteID, wsDir string) error {
	rows, err := queries.ListSiteFonts(ctx, siteID)
	if err != nil || len(rows) == 0 {
		return nil
	}
	return emitFontFiles(rows, siteID, wsDir)
}

// emitFontFiles is the store-free half of CopyFonts, split out for testing.
func emitFontFiles(rows []store.SiteFont, siteID, wsDir string) error {
	dstDir := filepath.Join(wsDir, "public", "atomicsite-fonts", siteID)
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dstDir, err)
	}
	for _, f := range rows {
		if f.FilePath == "" {
			continue
		}
		if _, err := os.Stat(f.FilePath); err != nil {
			slog.Warn("build: font skipped (file missing on disk)",
				"site_id", siteID, "font_id", f.ID, "src", f.FilePath)
			continue
		}
		if err := atomicLinkOrCopy(f.FilePath, filepath.Join(dstDir, f.ID+".woff2")); err != nil {
			return fmt.Errorf("emit font %s: %w", f.ID, err)
		}
	}
	return nil
}

// atomicLinkOrCopy writes src to dst via a temp name + rename, avoiding the
// TOCTOU window between Remove and Link. Tries hardlink first (same filesystem),
// falls back to a byte copy across filesystems.
func atomicLinkOrCopy(src, dst string) error {
	tmp := dst + ".tmp-" + randomSuffix()
	if err := os.Link(src, tmp); err != nil {
		// Hardlink failed (likely cross-filesystem). Fall back to copy.
		if err := copyFile(src, tmp); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	// rename is atomic on POSIX; overwrites target if it exists.
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func randomSuffix() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
