package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// dockyardHTTPTimeout caps a single HTTP call to the Dockyard admin API.
// Caller's context still wins if it's tighter.
const dockyardHTTPTimeout = 30 * time.Second

// uuidPattern is a permissive UUID-ish matcher: hex segments separated by
// dashes. Dockyard server IDs are UUIDs, but we don't insist on the exact
// 8-4-4-4-12 layout in case the format ever shifts.
var uuidPattern = regexp.MustCompile(`^[a-fA-F0-9]{8}-?[a-fA-F0-9]{4}-?[a-fA-F0-9]{4}-?[a-fA-F0-9]{4}-?[a-fA-F0-9]{12}$`)

// DockyardDeployer pushes a built site to a Dockyard-managed server. It runs
// in two stages: rsync the dist tree to the host filesystem, then register a
// Caddy route via the Dockyard admin API so traffic for the site's domain is
// served from that path. Idempotent: re-running on the same target = no-op
// rsync + 409-on-route-already-exists, both treated as success.
type DockyardDeployer struct {
	httpClient *http.Client
	rsync      *RsyncDeployer
	client     dockyardClient
	// allowPrivate disables the SSRF guards on the tenant-supplied
	// dockyard_url and rsync host. TESTS ONLY: httptest servers bind
	// loopback. It is unexported and set nowhere in production, so a
	// deployer built by any real path fails closed. See ssrf.go.
	allowPrivate bool
}

// NewDockyardDeployer returns a configured DockyardDeployer with sensible
// defaults. Tests can swap dockyardClient via SetClient.
func NewDockyardDeployer() *DockyardDeployer {
	return &DockyardDeployer{
		httpClient: newGuardedHTTPClient(dockyardHTTPTimeout, false),
		rsync:      NewRsyncDeployer(),
	}
}

// Kind reports the deployer kind.
func (d *DockyardDeployer) Kind() string { return "dockyard" }

// SetClient overrides the Dockyard API client. Used by tests.
func (d *DockyardDeployer) SetClient(c dockyardClient) { d.client = c }

// dockyardConfig is the parsed view of target.Config for dockyard targets.
type dockyardConfig struct {
	DockyardURL   string
	APIKey        string
	ServerID      string
	Domain        string
	Host          string
	User          string
	Port          int
	PrivateKeyPEM string
	HostPath      string
}

// dockyardClient is the narrow surface the deployer uses to talk to Dockyard.
// Everything routes through here so tests can fake responses without wiring
// up a real httptest server.
type dockyardClient interface {
	PutProxyRoute(ctx context.Context, serverID string, body proxyRouteRequest) error
	ApplyProxy(ctx context.Context, serverID string) error
}

// proxyRouteRequest mirrors the body Dockyard's POST /api/servers/{id}/proxy/routes
// expects. Dockyard has no native "static files" upstream kind, so we emit a
// snippet of Caddy custom_config that does `root` + `file_server` against the
// host path the rsync step just wrote to. UpstreamTarget still has to be set
// since Dockyard validates it as required; the file_server directive in
// custom_config takes precedence at request time.
type proxyRouteRequest struct {
	Domain          string `json:"domain"`
	UpstreamTarget  string `json:"upstream_target"`
	SSLMode         string `json:"ssl_mode"`
	ListenInterface string `json:"listen_interface"`
	CustomConfig    string `json:"custom_config"`
}

// parseDockyardConfig pulls the dockyard-specific fields from a Target.Config
// map. Permissive about port type to match RsyncDeployer.
func parseDockyardConfig(cfg map[string]any) (dockyardConfig, error) {
	out := dockyardConfig{Port: 22}
	if cfg == nil {
		return out, errors.New("dockyard deploy: config is empty")
	}

	out.DockyardURL = strings.TrimSpace(stringFrom(cfg["dockyard_url"]))
	out.APIKey = stringFrom(cfg["api_key"])
	out.ServerID = strings.TrimSpace(stringFrom(cfg["server_id"]))
	out.Domain = strings.TrimSpace(stringFrom(cfg["domain"]))
	out.Host = strings.TrimSpace(stringFrom(cfg["host"]))
	out.User = strings.TrimSpace(stringFrom(cfg["user"]))
	out.PrivateKeyPEM = stringFrom(cfg["private_key_pem"])
	out.HostPath = strings.TrimSpace(stringFrom(cfg["host_path"]))

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
			return out, fmt.Errorf("dockyard deploy: port has unsupported type %T", v)
		}
	}

	return out, nil
}

// rsyncTargetFor builds a synthetic rsync Target so the embedded
// RsyncDeployer can be reused without re-implementing Validate/Deploy.
func rsyncTargetFor(target Target, cfg dockyardConfig) Target {
	return Target{
		ID:     target.ID,
		SiteID: target.SiteID,
		Name:   target.Name,
		Kind:   "rsync",
		Config: map[string]any{
			"host":            cfg.Host,
			"user":            cfg.User,
			"port":            cfg.Port,
			"path":            cfg.HostPath,
			"private_key_pem": cfg.PrivateKeyPEM,
			"public_url":      "https://" + cfg.Domain,
		},
	}
}

// Validate enforces structural rules on a dockyard target before any network
// call. Delegates the SSH/rsync subset to RsyncDeployer so the rules stay in
// one place.
func (d *DockyardDeployer) Validate(target Target) error {
	cfg, err := parseDockyardConfig(target.Config)
	if err != nil {
		return err
	}

	if cfg.DockyardURL == "" {
		return errors.New("dockyard deploy: dockyard_url is required")
	}
	u, err := url.Parse(cfg.DockyardURL)
	if err != nil {
		return fmt.Errorf("dockyard deploy: dockyard_url %q is invalid: %w", cfg.DockyardURL, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("dockyard deploy: dockyard_url must use https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("dockyard deploy: dockyard_url has no host")
	}
	// SSRF: dockyard_url is tenant-supplied and this deployer POSTs to it.
	// https + non-empty host was the whole check, so 169.254.169.254 and the
	// Tailscale range both sailed through. Offline forms are refused here at
	// save time; the resolving guards run at request time in ssrf.go.
	if !d.allowPrivate {
		if err := checkTenantURLOffline(cfg.DockyardURL); err != nil {
			return fmt.Errorf("dockyard deploy: dockyard_url %q is not an allowed destination: %w", cfg.DockyardURL, err)
		}
	}
	if cfg.APIKey == "" {
		return errors.New("dockyard deploy: api_key is required")
	}
	if cfg.ServerID == "" {
		return errors.New("dockyard deploy: server_id is required")
	}
	if !uuidPattern.MatchString(cfg.ServerID) {
		return fmt.Errorf("dockyard deploy: server_id %q is not a valid UUID", cfg.ServerID)
	}
	if cfg.Domain == "" {
		return errors.New("dockyard deploy: domain is required")
	}
	if !hostnamePattern.MatchString(cfg.Domain) {
		return fmt.Errorf("dockyard deploy: domain %q is not a valid hostname", cfg.Domain)
	}
	if cfg.HostPath == "" {
		return errors.New("dockyard deploy: host_path is required")
	}
	if !strings.HasPrefix(cfg.HostPath, "/") {
		return fmt.Errorf("dockyard deploy: host_path %q must be absolute", cfg.HostPath)
	}

	// Reuse rsync validation for the SSH subset.
	rsyncT := rsyncTargetFor(target, cfg)
	if err := d.rsyncValidator().Validate(rsyncT); err != nil {
		return err
	}
	return nil
}

// rsyncValidator returns the embedded RsyncDeployer, lazily constructing one
// if NewDockyardDeployer wasn't used (e.g. via deploy.New).
func (d *DockyardDeployer) rsyncValidator() *RsyncDeployer {
	if d.rsync == nil {
		d.rsync = NewRsyncDeployer()
	}
	// The dockyard deploy delegates its whole SSH half to this deployer, so
	// the test-only escape hatch has to reach it too. Never true in production.
	d.rsync.allowPrivate = d.allowPrivate
	return d.rsync
}

// Deploy syncs the dist tree to the Dockyard-managed host then registers a
// Caddy proxy route so the configured domain serves those files.
//
// Order matters: rsync first, then route. If the route call fails after a
// successful sync the dist is up but unreachable; we surface that as an error
// so the caller can retry. Retry is safe because rsync re-syncs idempotently
// and a duplicate route returns 409, which we treat as success.
func (d *DockyardDeployer) Deploy(ctx context.Context, distDir string, target Target) (Result, error) {
	if err := d.Validate(target); err != nil {
		return Result{}, err
	}

	cfg, err := parseDockyardConfig(target.Config)
	if err != nil {
		return Result{}, err
	}

	// Step 1: rsync the dist tree.
	rsyncT := rsyncTargetFor(target, cfg)
	rsyncResult, err := d.rsyncValidator().Deploy(ctx, distDir, rsyncT)
	if err != nil {
		return Result{}, fmt.Errorf("dockyard deploy: rsync: %w", err)
	}

	// Step 2: register the Caddy proxy route + apply.
	client := d.dockyardClientFor(cfg)

	customConfig := fmt.Sprintf("root * %s\nfile_server", strings.TrimRight(cfg.HostPath, "/"))
	body := proxyRouteRequest{
		Domain:          cfg.Domain,
		UpstreamTarget:  "static-files",
		SSLMode:         "auto",
		ListenInterface: "public",
		CustomConfig:    customConfig,
	}

	if err := client.PutProxyRoute(ctx, cfg.ServerID, body); err != nil {
		slog.Warn("dockyard deploy: rsync succeeded but proxy route registration failed",
			"target_id", target.ID,
			"site_id", target.SiteID,
			"server_id", cfg.ServerID,
			"domain", cfg.Domain,
			"err", err,
		)
		return Result{}, fmt.Errorf("dockyard deploy: register proxy route: %w", err)
	}

	if err := client.ApplyProxy(ctx, cfg.ServerID); err != nil {
		slog.Warn("dockyard deploy: proxy route registered but apply failed",
			"target_id", target.ID,
			"site_id", target.SiteID,
			"server_id", cfg.ServerID,
			"err", err,
		)
		return Result{}, fmt.Errorf("dockyard deploy: apply proxy: %w", err)
	}

	return Result{
		URL:        "https://" + cfg.Domain,
		DeployedAt: time.Now(),
		SizeBytes:  rsyncResult.SizeBytes,
		FileCount:  rsyncResult.FileCount,
	}, nil
}

// dockyardClientFor returns the configured client or a default HTTP-backed
// one bound to this target's URL + API key.
func (d *DockyardDeployer) dockyardClientFor(cfg dockyardConfig) dockyardClient {
	if d.client != nil {
		return d.client
	}
	hc := d.httpClient
	if hc == nil {
		hc = newGuardedHTTPClient(dockyardHTTPTimeout, d.allowPrivate)
	}
	return &httpDockyardClient{
		base:         strings.TrimRight(cfg.DockyardURL, "/"),
		apiKey:       cfg.APIKey,
		hc:           hc,
		allowPrivate: d.allowPrivate,
	}
}

// httpDockyardClient is the production implementation of dockyardClient.
type httpDockyardClient struct {
	base   string
	apiKey string
	hc     *http.Client
	// allowPrivate skips the pre-flight host check. Tests only; see ssrf.go.
	allowPrivate bool
}

// dockyardMaxLoggedBody caps how much of an unexpected upstream response is
// kept for the server-side log. It is a log budget, not a tenant budget: none
// of it goes into the returned error.
const dockyardMaxLoggedBody = 4096

// post performs one guarded POST to the Dockyard admin API.
//
// Both call sites route through here so the SSRF pre-flight and the
// body-handling rule cannot be applied to one and forgotten on the other,
// which is how PutProxyRoute and ApplyProxy drifted apart in the first place.
//
// The upstream response body NEVER appears in the returned error. That error
// reaches the tenant: handlers/build.go appends it to the build log, which the
// tenant reads back via get_build_status. Returning the body there turned a
// blind request into a read primitive. It is logged server-side instead, where
// an operator debugging a real Dockyard failure still has it.
func (c *httpDockyardClient) post(ctx context.Context, op, endpoint string, payload []byte, accept func(int) bool) error {
	if !c.allowPrivate {
		if err := checkTenantURLPublic(endpoint); err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
	}

	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	defer resp.Body.Close()

	if accept(resp.StatusCode) {
		return nil
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, dockyardMaxLoggedBody))
	slog.Warn("dockyard admin API returned an unexpected status",
		"op", op,
		"status", resp.StatusCode,
		"body", strings.TrimSpace(string(respBody)),
	)
	return fmt.Errorf("%s: status %d", op, resp.StatusCode)
}

// PutProxyRoute POSTs to /api/servers/{serverID}/proxy/routes. 200/201/409
// are treated as success (409 means the route already exists).
func (c *httpDockyardClient) PutProxyRoute(ctx context.Context, serverID string, body proxyRouteRequest) error {
	endpoint := fmt.Sprintf("%s/api/servers/%s/proxy/routes", c.base, url.PathEscape(serverID))
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode body: %w", err)
	}

	return c.post(ctx, "post proxy route", endpoint, payload, func(status int) bool {
		return status == http.StatusOK || status == http.StatusCreated || status == http.StatusConflict
	})
}

// ApplyProxy POSTs to /api/servers/{serverID}/proxy/apply.
func (c *httpDockyardClient) ApplyProxy(ctx context.Context, serverID string) error {
	endpoint := fmt.Sprintf("%s/api/servers/%s/proxy/apply", c.base, url.PathEscape(serverID))

	return c.post(ctx, "post proxy apply", endpoint, nil, func(status int) bool {
		return status >= 200 && status < 300
	})
}
