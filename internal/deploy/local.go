package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LocalDeployer copies the built dist directory to a target path on the
// local filesystem with an atomic swap so the served content is never in a
// half-written state.
type LocalDeployer struct{}

// NewLocalDeployer returns a configured LocalDeployer.
func NewLocalDeployer() *LocalDeployer { return &LocalDeployer{} }

// Kind reports the deployer kind.
func (d *LocalDeployer) Kind() string { return "local" }

// localConfig is the parsed view of target.Config for local targets.
type localConfig struct {
	Path      string
	PublicURL string
}

func parseLocalConfig(cfg map[string]any) localConfig {
	if cfg == nil {
		return localConfig{}
	}
	return localConfig{
		Path:      strings.TrimSpace(stringFrom(cfg["path"])),
		PublicURL: strings.TrimSpace(stringFrom(cfg["public_url"])),
	}
}

// Validate enforces structural rules on a local target before we touch disk.
// The grandparent of the target path must already exist (so writes stay scoped
// to a known data dir); the parent itself is created on demand because users
// pick a leaf slug per site and shouldn't have to ssh in to mkdir it.
func (d *LocalDeployer) Validate(target Target) error {
	cfg := parseLocalConfig(target.Config)
	if cfg.Path == "" {
		return errors.New("local deploy: path is required")
	}
	if !filepath.IsAbs(cfg.Path) {
		return fmt.Errorf("local deploy: path %q must be absolute", cfg.Path)
	}
	cleaned := filepath.Clean(cfg.Path)
	if cleaned == "/" {
		return errors.New("local deploy: path must not be \"/\"")
	}
	// Cross-tenant isolation: a local target must write inside the site's own
	// directory so one tenant cannot point `path` at /srv/atomicsite/<other>/dist
	// and overwrite another tenant's served files. The wildcard host serves a
	// tenant from /srv/atomicsite/{slug}/dist, so the legitimate path contains
	// the site's slug; some setups use the site id instead. Accept either as a
	// full path segment. Both are set from the authenticated site, so a member
	// of site A can only ever supply A's slug/id here.
	if target.SiteID != "" || target.Slug != "" {
		if !pathHasSegment(cleaned, target.SiteID) && !(target.Slug != "" && pathHasSegment(cleaned, target.Slug)) {
			want := target.Slug
			if want == "" {
				want = target.SiteID
			}
			return fmt.Errorf("local deploy: path %q must be scoped to this site (include %q as a path segment)", cleaned, want)
		}
	}
	parent := filepath.Dir(cleaned)
	if parent == "/" {
		return errors.New("local deploy: parent dir must not be \"/\"")
	}
	grandparent := filepath.Dir(parent)
	info, err := os.Stat(grandparent)
	if err != nil {
		return fmt.Errorf("local deploy: grandparent dir %q: %w", grandparent, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("local deploy: grandparent %q is not a directory", grandparent)
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("local deploy: ensure parent dir %q: %w", parent, err)
	}
	return nil
}

// pathHasSegment reports whether seg appears as a full path component of p
// (so "/srv/atomicsite/<id>/dist" matches <id>, but "/srv/<id>extra" does not).
func pathHasSegment(p, seg string) bool {
	for _, s := range strings.Split(p, string(filepath.Separator)) {
		if s == seg {
			return true
		}
	}
	return false
}

// Deploy copies distDir to {path}.next, then atomically swaps it in:
//
//  1. copy distDir to path+".next"
//  2. rename existing path -> path+".prev" (skipped if no existing path)
//  3. rename path+".next" -> path
//  4. RemoveAll path+".prev" on success
//
// On any error the .next staging dir is removed and the swap is rolled back
// where possible.
func (d *LocalDeployer) Deploy(ctx context.Context, distDir string, target Target) (Result, error) {
	if err := d.Validate(target); err != nil {
		return Result{}, err
	}
	cfg := parseLocalConfig(target.Config)

	info, err := os.Stat(distDir)
	if err != nil {
		return Result{}, fmt.Errorf("local deploy: dist dir: %w", err)
	}
	if !info.IsDir() {
		return Result{}, fmt.Errorf("local deploy: dist path %q is not a directory", distDir)
	}

	dest := filepath.Clean(cfg.Path)
	nextPath := dest + ".next"
	prevPath := dest + ".prev"

	// Defensive cleanup: a previous failed run could have left .next or .prev around.
	_ = os.RemoveAll(nextPath)
	_ = os.RemoveAll(prevPath)

	if err := copyTree(ctx, distDir, nextPath); err != nil {
		_ = os.RemoveAll(nextPath)
		return Result{}, fmt.Errorf("local deploy: copy: %w", err)
	}

	// Move the existing dest aside (if it exists) so the next rename is atomic.
	hadExisting := false
	if _, err := os.Lstat(dest); err == nil {
		if err := os.Rename(dest, prevPath); err != nil {
			_ = os.RemoveAll(nextPath)
			return Result{}, fmt.Errorf("local deploy: stash existing: %w", err)
		}
		hadExisting = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		_ = os.RemoveAll(nextPath)
		return Result{}, fmt.Errorf("local deploy: stat dest: %w", err)
	}

	if err := os.Rename(nextPath, dest); err != nil {
		// Swap failed. Try to restore the previous tree so the site keeps serving.
		if hadExisting {
			if rerr := os.Rename(prevPath, dest); rerr != nil {
				slog.Error("local deploy: restore prev failed",
					"target_id", target.ID,
					"site_id", target.SiteID,
					"err", rerr,
				)
			}
		}
		_ = os.RemoveAll(nextPath)
		return Result{}, fmt.Errorf("local deploy: swap: %w", err)
	}

	if hadExisting {
		if err := os.RemoveAll(prevPath); err != nil {
			slog.Warn("local deploy: cleanup prev failed",
				"target_id", target.ID,
				"site_id", target.SiteID,
				"path", prevPath,
				"err", err,
			)
		}
	}

	size, count, err := walkDist(dest)
	if err != nil {
		slog.Warn("local deploy: walk for stats failed", "err", err, "dest", dest)
	}

	return Result{
		URL:        cfg.PublicURL,
		DeployedAt: time.Now(),
		SizeBytes:  size,
		FileCount:  count,
	}, nil
}

// copyTree copies src directory contents into dst (which must not exist).
// It honors ctx cancellation between files.
func copyTree(ctx context.Context, src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(target, info.Mode().Perm())
		}
		// Symlinks: skip silently. The site bundle is plain files.
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
