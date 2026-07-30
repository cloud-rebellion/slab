package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// hostnamePattern matches a hostname or IP literal. Permits dots, digits,
// letters, and hyphens. Length capped at 253 like RFC 1035.
var hostnamePattern = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9\-\.]{0,251}[a-zA-Z0-9])?$`)

// rsyncUserPattern + rsyncPathPattern keep shell metacharacters out of the
// remote-shell spec. Even with --protect-args (which stops rsync expanding the
// remote path), validating here is defence in depth: a user/path with `;`, `$`,
// backticks or spaces never reaches the ssh argv.
var (
	rsyncUserPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)
	rsyncPathPattern = regexp.MustCompile(`^/[a-zA-Z0-9._/-]{0,255}$`)
)

// rsyncTimeout caps a single rsync invocation. The context the caller passes
// in still wins if it's tighter.
const rsyncTimeout = 5 * time.Minute

// RsyncDeployer pushes a built site to a remote server over SSH using rsync.
//
// private_key_pem is encrypted at rest inside targets.config_json via
// internal/atrest (AES-256-GCM under ATOMICSITE_SHIELD_KEY, enc:v1:
// prefix); the handlers decrypt it before constructing the Target this
// deployer receives, so this package always sees a real PEM. Legacy
// plaintext rows keep working and upgrade on their next save.
type RsyncDeployer struct{}

// NewRsyncDeployer returns a configured RsyncDeployer. Reserved as a
// constructor in case we later thread in dependencies (clock, logger, etc).
func NewRsyncDeployer() *RsyncDeployer { return &RsyncDeployer{} }

// Kind reports the deployer kind that target rows reference.
func (d *RsyncDeployer) Kind() string { return "rsync" }

// rsyncConfig is the parsed view of target.Config for rsync targets.
type rsyncConfig struct {
	Host          string
	User          string
	Port          int
	Path          string
	PrivateKeyPEM string
	PublicURL     string
	// HostKey is the optional expected SSH host public key line (e.g.
	// "ssh-ed25519 AAAA..."). When set, the host key is pinned
	// (StrictHostKeyChecking=yes against a known_hosts file). When empty we
	// fall back to accept-new (trust-on-first-use) instead of the old
	// blanket-ignore (StrictHostKeyChecking=no + /dev/null), which accepted
	// any key on every connect and gave zero MITM protection.
	HostKey string
}

// parseRsyncConfig pulls and normalises the rsync-specific fields from a
// generic Target.Config map. It is permissive about types: callers may have
// stored port as a JSON number (float64) or a string.
func parseRsyncConfig(cfg map[string]any) (rsyncConfig, error) {
	out := rsyncConfig{Port: 22}
	if cfg == nil {
		return out, errors.New("rsync deploy: config is empty")
	}

	out.Host = strings.TrimSpace(stringFrom(cfg["host"]))
	out.User = strings.TrimSpace(stringFrom(cfg["user"]))
	out.Path = strings.TrimSpace(stringFrom(cfg["path"]))
	out.PrivateKeyPEM = stringFrom(cfg["private_key_pem"])
	out.PublicURL = strings.TrimSpace(stringFrom(cfg["public_url"]))
	out.HostKey = strings.TrimSpace(stringFrom(cfg["host_key"]))

	if v, ok := cfg["port"]; ok && v != nil {
		switch n := v.(type) {
		case float64:
			out.Port = int(n)
		case int:
			out.Port = n
		case int64:
			out.Port = int(n)
		case string:
			if n == "" {
				break
			}
			parsed, err := parsePort(n)
			if err != nil {
				return out, err
			}
			out.Port = parsed
		default:
			return out, fmt.Errorf("rsync deploy: port has unsupported type %T", v)
		}
	}

	return out, nil
}

func stringFrom(v any) string {
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

func parsePort(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	if err != nil {
		return 0, fmt.Errorf("rsync deploy: invalid port %q: %w", s, err)
	}
	return n, nil
}

// Validate enforces the structural rules on a target before we ever touch
// the network. Anything caught here surfaces as a useful error in the UI
// instead of an opaque rsync exit code later.
func (d *RsyncDeployer) Validate(target Target) error {
	cfg, err := parseRsyncConfig(target.Config)
	if err != nil {
		return err
	}

	if cfg.Host == "" {
		return errors.New("rsync deploy: host is required")
	}
	if !hostnamePattern.MatchString(cfg.Host) {
		return fmt.Errorf("rsync deploy: host %q is not a valid hostname", cfg.Host)
	}
	if cfg.User == "" {
		return errors.New("rsync deploy: user is required")
	}
	if !rsyncUserPattern.MatchString(cfg.User) {
		return fmt.Errorf("rsync deploy: user %q contains invalid characters (allowed: letters, digits, . _ -)", cfg.User)
	}
	if cfg.Path == "" {
		return errors.New("rsync deploy: path is required")
	}
	if !rsyncPathPattern.MatchString(cfg.Path) {
		return fmt.Errorf("rsync deploy: path %q must be absolute and free of shell metacharacters", cfg.Path)
	}
	if cfg.HostKey != "" && !strings.HasPrefix(cfg.HostKey, "ssh-") && !strings.HasPrefix(cfg.HostKey, "ecdsa-") {
		return errors.New("rsync deploy: host_key must be an SSH host public key line (e.g. \"ssh-ed25519 AAAA...\")")
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return fmt.Errorf("rsync deploy: port %d is out of range (1..65535)", cfg.Port)
	}
	if cfg.PrivateKeyPEM == "" {
		return errors.New("rsync deploy: private_key_pem is required")
	}
	if !strings.HasPrefix(strings.TrimSpace(cfg.PrivateKeyPEM), "-----BEGIN") {
		return errors.New("rsync deploy: private_key_pem does not look like a PEM block")
	}
	return nil
}

// buildRsyncArgs returns the argv for the rsync invocation. Pulled out so
// unit tests can verify the command shape without spawning a process.
//
// Host-key handling: when cfg.HostKey is set the key is pinned
// (StrictHostKeyChecking=yes against knownHostsFile); otherwise accept-new
// (trust-on-first-use) replaces the old blanket-ignore. --protect-args stops
// rsync handing the remote path to a remote shell (injection guard);
// --delay-updates stages the new tree and swaps it into place at the end so a
// half-synced site is never served.
func buildRsyncArgs(cfg rsyncConfig, distDir, keyfile, knownHostsFile string) []string {
	strict := "accept-new"
	if cfg.HostKey != "" {
		strict = "yes"
	}
	sshCmd := fmt.Sprintf(
		"ssh -i %s -p %d -o StrictHostKeyChecking=%s -o UserKnownHostsFile=%s",
		keyfile, cfg.Port, strict, knownHostsFile,
	)
	src := strings.TrimRight(distDir, "/") + "/"
	dst := fmt.Sprintf("%s@%s:%s/", cfg.User, cfg.Host, strings.TrimRight(cfg.Path, "/"))
	return []string{
		"-avz",
		"--delete",
		"--delay-updates",
		"--protect-args",
		"-e", sshCmd,
		src,
		dst,
	}
}

// writeKnownHosts drops a 0600 known_hosts file into its own tempdir. When
// cfg.HostKey is set the file pins that key for the host (and [host]:port for a
// non-standard port); otherwise it is empty and accept-new fills it on first
// connect. Returns the path plus a cleanup closure.
func writeKnownHosts(cfg rsyncConfig) (string, func(), error) {
	dir, err := os.MkdirTemp("", "atomicsite-rsync-kh-")
	if err != nil {
		return "", func() {}, fmt.Errorf("rsync deploy: mkdir known_hosts temp: %w", err)
	}
	cleanup := func() {
		if err := os.RemoveAll(dir); err != nil {
			slog.Warn("rsync deploy: cleanup known_hosts tempdir failed", "dir", dir, "err", err)
		}
	}
	var content string
	if cfg.HostKey != "" {
		hostspec := cfg.Host
		if cfg.Port != 22 {
			hostspec = fmt.Sprintf("[%s]:%d", cfg.Host, cfg.Port)
		}
		content = hostspec + " " + strings.TrimSpace(cfg.HostKey) + "\n"
	}
	path := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("rsync deploy: write known_hosts: %w", err)
	}
	return path, cleanup, nil
}

// Deploy syncs distDir to the remote target using rsync over SSH.
func (d *RsyncDeployer) Deploy(ctx context.Context, distDir string, target Target) (Result, error) {
	if err := d.Validate(target); err != nil {
		return Result{}, err
	}

	cfg, err := parseRsyncConfig(target.Config)
	if err != nil {
		return Result{}, err
	}

	info, err := os.Stat(distDir)
	if err != nil {
		return Result{}, fmt.Errorf("rsync deploy: dist dir: %w", err)
	}
	if !info.IsDir() {
		return Result{}, fmt.Errorf("rsync deploy: dist path %q is not a directory", distDir)
	}
	// Refuse an EMPTY dist. This deploy runs rsync with --delete, so syncing an
	// empty or index-less tree does not just publish nothing, it DELETES the
	// live site's files. The stat above only caught a MISSING dist, and a build
	// that exits 0 without emitting anything, or a workspace another build
	// deleted mid-deploy, both produce a present-but-empty directory. There is
	// no legitimate reason to --delete-sync an empty tree, so refusing costs
	// nothing and the failure mode it prevents is unrecoverable without a
	// restore.
	entries, err := os.ReadDir(distDir)
	if err != nil {
		return Result{}, fmt.Errorf("rsync deploy: read dist dir: %w", err)
	}
	if len(entries) == 0 {
		return Result{}, fmt.Errorf("rsync deploy: dist dir %q is empty; refusing to --delete-sync an empty tree over the live site", distDir)
	}

	keyfile, cleanup, err := writeKeyfile(cfg.PrivateKeyPEM)
	if err != nil {
		return Result{}, err
	}
	defer cleanup()

	knownHosts, khCleanup, err := writeKnownHosts(cfg)
	if err != nil {
		return Result{}, err
	}
	defer khCleanup()

	cmdCtx, cancel := context.WithTimeout(ctx, rsyncTimeout)
	defer cancel()

	args := buildRsyncArgs(cfg, distDir, keyfile, knownHosts)
	cmd := exec.CommandContext(cmdCtx, "rsync", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		slog.Error("rsync deploy failed",
			"target_id", target.ID,
			"site_id", target.SiteID,
			"host", cfg.Host,
			"err", err,
			"stderr", strings.TrimSpace(stderr.String()),
		)
		return Result{}, fmt.Errorf("rsync deploy: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	size, count, err := walkDist(distDir)
	if err != nil {
		slog.Warn("rsync deploy: walk dist for stats failed", "err", err, "dist_dir", distDir)
	}

	return Result{
		URL:        cfg.PublicURL,
		DeployedAt: time.Now(),
		SizeBytes:  size,
		FileCount:  count,
	}, nil
}

// writeKeyfile drops a private key PEM into a 0600 tempfile and returns
// the path plus a cleanup closure. Parent dir is created via MkdirTemp so
// even the directory itself is private to the current user.
func writeKeyfile(pem string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "atomicsite-rsync-key-")
	if err != nil {
		return "", func() {}, fmt.Errorf("rsync deploy: mkdir temp: %w", err)
	}
	cleanup := func() {
		if err := os.RemoveAll(dir); err != nil {
			slog.Warn("rsync deploy: cleanup tempdir failed", "dir", dir, "err", err)
		}
	}

	path := filepath.Join(dir, "id_key.pem")
	if err := os.WriteFile(path, []byte(pem), 0o600); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("rsync deploy: write key: %w", err)
	}
	return path, cleanup, nil
}

// walkDist sums file sizes and counts regular files under root.
func walkDist(root string) (int64, int, error) {
	var total int64
	var count int
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
			count++
		}
		return nil
	})
	return total, count, err
}
