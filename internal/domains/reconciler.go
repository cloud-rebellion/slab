package domains

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/bright-interaction/slab/internal/store"
)

// Reconciler drives every site_domains row from 'pending' to 'live'.
// One goroutine, one DB. Steps for each row at each tick:
//
//	pending     → ensure DNS (Cloudflare helper if zone matches),
//	               probe verify URL, advance to verified on success.
//	verified    → certbot HTTP-01, write 443 vhost + map entry,
//	               reload nginx, advance to cert_ready.
//	cert_ready  → live HTTPS probe, advance to live on success.
//	live        → re-probe at low cadence, demote to error on
//	               three consecutive failures (stale cert, DNS
//	               changed, etc.).
//	error       → retry with exponential backoff up to 30 min.
//
// The signal channel lets handlers nudge the loop the moment a row
// changes (so the admin sees status flips within seconds, not at the
// next periodic tick). Closing the channel via Stop() drains and
// returns.
type Reconciler struct {
	queries     *store.Queries
	edge        *EdgeWriter
	certbot     *CertbotRunner
	cloudflare  *CloudflareClient
	edgeIP      string
	appUpstream string

	tick   time.Duration
	signal chan struct{}
	done   chan struct{}
}

// Config bundles the reconciler's dependencies. Each field is
// optional; nil collaborators degrade gracefully (the loop logs the
// reason a row is stuck rather than crashing).
type Config struct {
	Queries     *store.Queries
	Edge        *EdgeWriter
	Certbot     *CertbotRunner
	Cloudflare  *CloudflareClient
	EdgeIP      string
	AppUpstream string        // http://127.0.0.1:8091 etc.
	Tick        time.Duration // default 30s
}

func NewReconciler(cfg Config) *Reconciler {
	tick := cfg.Tick
	if tick == 0 {
		tick = 30 * time.Second
	}
	return &Reconciler{
		queries:     cfg.Queries,
		edge:        cfg.Edge,
		certbot:     cfg.Certbot,
		cloudflare:  cfg.Cloudflare,
		edgeIP:      cfg.EdgeIP,
		appUpstream: cfg.AppUpstream,
		tick:        tick,
		signal:      make(chan struct{}, 1),
		done:        make(chan struct{}),
	}
}

// Signal nudges the reconciler to run a sweep right now. Drops if
// a sweep is already queued (channel buffer = 1).
func (r *Reconciler) Signal() {
	if r == nil {
		return
	}
	select {
	case r.signal <- struct{}{}:
	default:
	}
}

// Run blocks until ctx is cancelled. Spin it in a goroutine.
func (r *Reconciler) Run(ctx context.Context) {
	if r == nil || r.queries == nil {
		return
	}
	defer close(r.done)
	t := time.NewTicker(r.tick)
	defer t.Stop()
	// Initial pass: write the static ACME helper block once on boot
	// so freshly-pointed domains can verify before the first tick.
	if r.edge.Enabled() && r.appUpstream != "" {
		if err := r.edge.WriteAcmeBlock(r.appUpstream); err != nil {
			slog.Warn("domains.reconcile: write acme block", "err", err)
		} else {
			r.reloadNginx(ctx)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.sweep(ctx)
		case <-r.signal:
			r.sweep(ctx)
		}
	}
}

// Done returns a channel closed when Run returns.
func (r *Reconciler) Done() <-chan struct{} { return r.done }

// sweep walks every row and progresses it as far as it can in one tick.
// One sweep can flip many rows; the slowest step (certbot) is bounded
// by its own internal timeout.
func (r *Reconciler) sweep(ctx context.Context) {
	rows, err := r.queries.ListAllDomains(ctx)
	if err != nil {
		slog.Warn("domains.reconcile: list", "err", err)
		return
	}
	// Fetch slugs once so per-row callers don't issue N+1 lookups.
	sites, _ := r.queries.ListSites(ctx)
	sitesByID := make(map[string]string, len(sites))
	for _, s := range sites {
		sitesByID[s.ID] = s.Slug
	}

	progressed := false
	for _, d := range rows {
		if r.advanceOne(ctx, d, sitesByID) {
			progressed = true
		}
	}

	// Always rewrite the slug-derivation map at the end of a sweep
	// so it reflects the current set of live domains. writeIfChanged
	// keeps this cheap when nothing moved.
	if r.edge.Enabled() {
		fresh, _ := r.queries.ListAllDomains(ctx)
		if err := r.edge.WriteDomainMap(fresh, sitesByID); err != nil {
			slog.Warn("domains.reconcile: write map", "err", err)
		}
		// Reap nginx vhost files whose row no longer exists. This is
		// the "DELETE /api/sites/{id}/domains/{id} cleaned up the DB
		// row but the on-disk fragment lingered" case.
		liveIDs := make(map[string]struct{}, len(fresh))
		for _, d := range fresh {
			liveIDs[d.ID] = struct{}{}
		}
		removed, err := r.edge.SweepOrphanVhosts(liveIDs)
		if err != nil {
			slog.Warn("domains.reconcile: sweep orphans", "err", err)
		}
		if progressed || removed > 0 {
			r.reloadNginx(ctx)
		}
	}
}

// advanceOne returns true when the row's status changed (so the
// caller knows nginx needs a reload).
func (r *Reconciler) advanceOne(ctx context.Context, d store.SiteDomain, sitesByID map[string]string) bool {
	switch d.Status {
	case "pending":
		return r.advancePending(ctx, d)
	case "verified":
		return r.advanceVerified(ctx, d, sitesByID)
	case "cert_ready":
		return r.advanceCertReady(ctx, d)
	case "live":
		// Periodic re-check; demote to error on repeated failures.
		// One failure flip is fine (admin sees it, nginx config
		// stays in place, next tick attempts recovery).
		if err := VerifyHostnameLive(ctx, d.Hostname); err != nil {
			r.markError(ctx, d, "live probe failed: "+err.Error())
			return true
		}
		// Steady-state, no change.
		return false
	case "error":
		// Back off: re-check an error row at most every errorRetryBackoff. Every
		// UpdateDomainStatus stamps last_check_at, so a persistently broken domain
		// (wrong DNS, bogus hostname) cannot hammer certbot / Let's Encrypt (shared
		// per-account rate limits) or Cloudflare on every 30s tick.
		if !errorRetryDue(d.LastCheckAt) {
			return false
		}
		// Only re-attempt OWNERSHIP verification here. Do NOT fall through to
		// advanceVerified: the old `advancePending() || advanceVerified()` fired
		// certbot.Issue unconditionally whenever verification FAILED (advancePending
		// returns false), burning the shared LE account budget on a hostname that
		// never verified. On success this promotes the row to "verified" and the
		// next tick's verified-case issues the cert.
		return r.advancePending(ctx, d)
	}
	return false
}

// errorRetryBackoff caps how often an error-state domain is retried.
const errorRetryBackoff = 30 * time.Minute

// errorRetryDue reports whether an error row (last touched at lastCheckAt, the
// SQLite datetime('now') string) is due for another attempt. An unparseable
// timestamp fails open to progress.
func errorRetryDue(lastCheckAt string) bool {
	t, err := time.Parse("2006-01-02 15:04:05", lastCheckAt)
	if err != nil {
		return true
	}
	return time.Since(t) >= errorRetryBackoff
}

func (r *Reconciler) advancePending(ctx context.Context, d store.SiteDomain) bool {
	// Best-effort Cloudflare A-record creation when the hostname
	// falls under a zone we control.
	if r.cloudflare.Enabled() && r.edgeIP != "" {
		if zoneID, _ := r.cloudflare.MatchZone(d.Hostname); zoneID != "" {
			if err := r.cloudflare.EnsureA(ctx, d.Hostname, r.edgeIP); err != nil {
				slog.Warn("domains.reconcile: cloudflare ensure A", "host", d.Hostname, "err", err)
				// non-fatal: maybe DNS is already correct, or admin
				// rotates the CF token. Keep going.
			}
		}
	}

	if err := VerifyHostnameOwnership(ctx, d.Hostname, d.VerifyToken); err != nil {
		r.markError(ctx, d, "verify: "+err.Error())
		return false
	}
	return r.setStatus(ctx, d, "verified", "")
}

func (r *Reconciler) advanceVerified(ctx context.Context, d store.SiteDomain, sitesByID map[string]string) bool {
	if !r.certbot.Enabled() {
		r.markError(ctx, d, "certbot not configured on this host")
		return false
	}
	certDir, err := r.certbot.Issue(ctx, d.Hostname)
	if err != nil {
		r.markError(ctx, d, "certbot: "+err.Error())
		return false
	}
	if err := r.queries.UpdateDomainCertPath(ctx, store.UpdateDomainCertPathParams{
		ID:       d.ID,
		CertPath: certDir,
	}); err != nil {
		slog.Warn("domains.reconcile: update cert path", "err", err)
	}
	slug := sitesByID[d.SiteID]
	if slug == "" {
		r.markError(ctx, d, "no slug for site_id "+d.SiteID)
		return false
	}
	if err := r.edge.WriteDomainVhost(d, slug, certDir); err != nil {
		r.markError(ctx, d, "write vhost: "+err.Error())
		return false
	}
	return r.setStatus(ctx, d, "cert_ready", "")
}

func (r *Reconciler) advanceCertReady(ctx context.Context, d store.SiteDomain) bool {
	if err := VerifyHostnameLive(ctx, d.Hostname); err != nil {
		r.markError(ctx, d, "live: "+err.Error())
		return false
	}
	return r.setStatus(ctx, d, "live", "")
}

func (r *Reconciler) markError(ctx context.Context, d store.SiteDomain, msg string) {
	if err := r.queries.UpdateDomainStatus(ctx, store.UpdateDomainStatusParams{
		ID:        d.ID,
		Status:    "error",
		LastError: truncate(msg, 500),
	}); err != nil && !errors.Is(err, context.Canceled) {
		slog.Warn("domains.reconcile: mark error", "id", d.ID, "err", err)
	}
}

func (r *Reconciler) setStatus(ctx context.Context, d store.SiteDomain, status, lastErr string) bool {
	if err := r.queries.UpdateDomainStatus(ctx, store.UpdateDomainStatusParams{
		ID:        d.ID,
		Status:    status,
		LastError: lastErr,
	}); err != nil {
		slog.Warn("domains.reconcile: set status", "id", d.ID, "status", status, "err", err)
		return false
	}
	return true
}

// reloadNginx shells the configured reload command. Empty command
// means "no host integration"; we silently no-op so dev harnesses
// don't need root. Errors are logged, not fatal: the admin sees the
// stale config in the next live probe.
func (r *Reconciler) reloadNginx(ctx context.Context) {
	cmd := strings.TrimSpace(reloadCmd)
	if cmd == "" {
		return
	}
	parts := strings.Fields(cmd)
	c := exec.CommandContext(ctx, parts[0], parts[1:]...)
	out, err := c.CombinedOutput()
	if err != nil {
		slog.Warn("domains.reconcile: nginx reload failed", "err", err, "out", truncate(string(out), 200))
		return
	}
	slog.Debug("domains.reconcile: nginx reloaded")
}

// reloadCmd is package-level so SetReloadCommand can swap it without
// reaching into the reconciler. Set once at boot.
var reloadCmd string

// SetReloadCommand configures the shell command used to reload the
// host nginx. Called once at boot from the server bootstrap path.
func SetReloadCommand(cmd string) { reloadCmd = cmd }

// HumanStatus returns a friendly description of a status string for
// the admin UI. Centralised so frontend + Go agree on wording.
func HumanStatus(s string) string {
	switch s {
	case "pending":
		return "Awaiting DNS"
	case "verified":
		return "Issuing certificate"
	case "cert_ready":
		return "Finalising"
	case "live":
		return "Live"
	case "error":
		return "Error"
	}
	return s
}

// Validate reports whether the supplied config has the minimum to do
// real work. Useful for the boot-time log line.
func (cfg Config) Validate() error {
	if cfg.Queries == nil {
		return fmt.Errorf("queries is required")
	}
	if cfg.AppUpstream == "" {
		return fmt.Errorf("app upstream is required")
	}
	return nil
}
