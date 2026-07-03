package domains

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// CertbotRunner orchestrates Let's Encrypt cert issuance via the host
// certbot binary in webroot mode. We deliberately do NOT use the
// nginx plugin because that would have certbot rewrite our nginx
// vhosts; we own those files and want certbot to stay in its lane.
//
// Webroot flow:
//  1. atomicsite-acme.conf (written by EdgeWriter) serves
//     /.well-known/acme-challenge/ from AcmeWebrootDir on port 80.
//  2. We exec `certbot certonly --webroot -w <dir> -d <host>`.
//  3. certbot drops a challenge file in
//     AcmeWebrootDir/.well-known/acme-challenge/, hits the LE API,
//     LE fetches the file, certbot writes the cert to
//     /etc/letsencrypt/live/<host>/.
type CertbotRunner struct {
	BinaryPath     string // /usr/bin/certbot or empty to disable
	WebrootDir     string
	LiveDir        string // /etc/letsencrypt/live  (set explicitly so tests can override)
	AdminEmail     string // reused as the LE registration email (any valid email works)
	NonInteractive bool
	// Timeout caps the total certbot exec. LE rate limits + retries
	// can push real runs past 60s; 5 minutes is safe.
	Timeout time.Duration
}

// Enabled reports whether certbot is wired up.
func (c *CertbotRunner) Enabled() bool {
	return c != nil && c.BinaryPath != "" && c.WebrootDir != ""
}

// CertDir returns the directory the live cert lives in for hostname.
// /etc/letsencrypt/live/<host>/ is certbot's canonical layout; the
// nginx vhost references {dir}/fullchain.pem + {dir}/privkey.pem.
func (c *CertbotRunner) CertDir(hostname string) string {
	if c.LiveDir == "" {
		return filepath.Join("/etc/letsencrypt/live", hostname)
	}
	return filepath.Join(c.LiveDir, hostname)
}

// HasCert reports whether a cert is already on disk for hostname.
// Used to short-circuit certbot when the operator has manually issued
// a cert (or a previous reconcile already did).
func (c *CertbotRunner) HasCert(hostname string) bool {
	dir := c.CertDir(hostname)
	if _, err := os.Stat(filepath.Join(dir, "fullchain.pem")); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "privkey.pem")); err != nil {
		return false
	}
	return true
}

// Issue runs certbot for one hostname. Returns the live cert dir on
// success. Idempotent: certbot detects an existing cert and skips
// re-issuance unless the cert is within 30 days of expiry.
func (c *CertbotRunner) Issue(ctx context.Context, hostname string) (string, error) {
	if !c.Enabled() {
		return "", fmt.Errorf("certbot not configured")
	}
	if hostname == "" {
		return "", fmt.Errorf("hostname is empty")
	}

	timeout := c.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{
		"certonly",
		"--webroot",
		"-w", c.WebrootDir,
		"-d", hostname,
		"--keep-until-expiring",
		"--no-eff-email",
		"--agree-tos",
	}
	if c.AdminEmail != "" {
		args = append(args, "-m", c.AdminEmail)
	}
	if c.NonInteractive {
		args = append(args, "--non-interactive")
	}

	cmd := exec.CommandContext(ctx, c.BinaryPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("certbot %s: %w (output: %s)", hostname, err, truncate(strings.TrimSpace(string(out)), 400))
	}
	dir := c.CertDir(hostname)
	if !c.HasCert(hostname) {
		return "", fmt.Errorf("certbot reported success but cert files missing at %s", dir)
	}
	return dir, nil
}
