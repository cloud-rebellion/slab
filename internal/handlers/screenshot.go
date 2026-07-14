// Package handlers — screenshot endpoint.
//
// Closes the visual-feedback gap that's been killing iteration speed.
// Atomicsite renders agent JSON into static Astro pages, but the agent
// has historically had no way to see the rendered output — every layout
// bug had to be reported back by a human. This handler runs headless
// Chromium against the deployed page, returns base64 PNG, and lets the
// agent reason about pixels in the same turn it edits blocks.
//
// Flow:
//   1. Agent edits blocks via MCP / agent API
//   2. Agent calls trigger_build → success
//   3. Agent calls POST /api/agent/screenshot { url } → base64 PNG
//   4. Agent decodes the image and visually checks the result
//   5. Iterate, re-build, re-screenshot, until pixel-perfect
//
// The agent IS Claude (or compatible) which has vision. Sending the PNG
// back as base64 lands as an image part in the tool result on
// vision-capable models; the agent can describe what it sees and decide
// whether to keep iterating.

package handlers

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/chromedp/chromedp"
)

// screenshotAllowedSuffixes is the SSRF allow-list for screenshot URLs.
// Set once at boot via ConfigureScreenshotAllowList from the loaded
// config (cfg.PrimaryDomain + cfg.BuiltSiteSuffix). Loopback always
// passes regardless so dev environments work out of the box.
//
// atomic.Pointer keeps the lookup lock-free at request time; the slice
// is replaced wholesale on reconfigure.
var screenshotAllowedSuffixes atomic.Pointer[[]string]

// ConfigureScreenshotAllowList sets the public hostnames + suffix
// matches the screenshot tool will accept. Called from cmd/server with
// cfg.PrimaryDomain + cfg.BuiltSiteSuffix. Empty entries are ignored.
// Calling with an empty list disables non-loopback screenshots.
func ConfigureScreenshotAllowList(entries ...string) {
	cleaned := make([]string, 0, len(entries))
	for _, e := range entries {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		cleaned = append(cleaned, e)
	}
	screenshotAllowedSuffixes.Store(&cleaned)
}

// ScreenshotAllowedSuffixes returns a copy of the current allow-list
// for diagnostic / admin endpoints.
func ScreenshotAllowedSuffixes() []string {
	cur := screenshotAllowedSuffixes.Load()
	if cur == nil {
		return nil
	}
	out := make([]string, len(*cur))
	copy(out, *cur)
	return out
}

// ScreenshotHandler executes a headless Chromium navigation + screenshot
// for an agent-supplied URL. Site-scoped: the URL must belong to the
// requesting agent's site (or a known atomicsite-hosted subdomain) so
// the handler can't be turned into an open SSRF vector.
type ScreenshotHandler struct{}

func NewScreenshotHandler() *ScreenshotHandler {
	return &ScreenshotHandler{}
}

// ScreenshotRequest is the public params struct shared by the HTTP
// handler and the MCP tool wrapper. Both paths call CaptureScreenshot
// so the chromedp logic stays in one place.
type ScreenshotRequest struct {
	URL            string `json:"url"`
	ViewportWidth  int    `json:"viewport_width"`
	ViewportHeight int    `json:"viewport_height"`
	FullPage       *bool  `json:"full_page"`
	WaitMs         int    `json:"wait_ms"`
}

// ScreenshotResult is the canonical response shape for a successful
// screenshot capture. image_base64 is a base64-encoded PNG; vision-
// capable agents decode it and reason about pixels directly.
type ScreenshotResult struct {
	URL         string `json:"url"`
	ImageBase64 string `json:"image_base64"`
	Format      string `json:"format"`
	Viewport    struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"viewport"`
	FullPage  bool `json:"full_page"`
	SizeBytes int  `json:"size_bytes"`
}

// screenshotSem caps concurrent headless-Chromium launches across every
// caller (HTTP handler, MCP screenshot + preview_screenshot). Each
// launch is a full browser process; unbounded parallel agent calls
// could OOM the host, the same class the global build semaphore closed
// for bun/astro. Default 2, override with ATOMICSITE_SCREENSHOT_CONCURRENCY.
var screenshotSem = make(chan struct{}, screenshotConcurrency())

func screenshotConcurrency() int {
	if v := strings.TrimSpace(os.Getenv("ATOMICSITE_SCREENSHOT_CONCURRENCY")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 2
}

// screenshotSlotWait bounds how long a capture queues for a browser
// slot before giving up (the agent can just retry).
const screenshotSlotWait = 20 * time.Second

// errScreenshotBusy is returned when every browser slot stayed occupied
// for the full queue wait. The HTTP handler maps it to 503.
var errScreenshotBusy = errors.New("screenshot capacity saturated; retry shortly")

// CaptureScreenshot is the single chromedp execution path. Used by both
// the HTTP handler (POST /api/agent/screenshot) and the MCP tool
// wrapper (screenshot). Defaults: 1440x900 viewport, full_page=true,
// wait_ms=800. SSRF-locked via validateScreenshotURL. Bounded by
// screenshotSem.
func CaptureScreenshot(ctx context.Context, req ScreenshotRequest) (*ScreenshotResult, error) {
	target := strings.TrimSpace(req.URL)
	if target == "" {
		return nil, errors.New("url required")
	}
	if err := validateScreenshotURL(target); err != nil {
		return nil, err
	}
	return captureScreenshot(ctx, target, req)
}

// CaptureLoopbackScreenshot captures a server-minted loopback URL (the
// preview_screenshot flow, http://127.0.0.1:{port}/_preview/{token}).
// The public allow-list + 80/443 port restriction do not apply here:
// the URL is constructed by the server itself, never taken from agent
// input, and validateScreenshotURL would reject both the loopback host
// and the app port on any production instance (which silently broke
// preview_screenshot everywhere the allow-list is configured). The
// loopback-host check keeps the bypass unusable for anything else.
func CaptureLoopbackScreenshot(ctx context.Context, req ScreenshotRequest) (*ScreenshotResult, error) {
	target := strings.TrimSpace(req.URL)
	if target == "" {
		return nil, errors.New("url required")
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("malformed url: %w", err)
	}
	if !isLoopbackScreenshotHost(strings.ToLower(parsed.Hostname())) {
		return nil, errors.New("loopback capture requires a loopback host")
	}
	return captureScreenshot(ctx, target, req)
}

// captureScreenshot is the shared chromedp path behind both entry
// points. target has already passed the caller's URL policy.
func captureScreenshot(ctx context.Context, target string, req ScreenshotRequest) (*ScreenshotResult, error) {
	slotTimer := time.NewTimer(screenshotSlotWait)
	defer slotTimer.Stop()
	select {
	case screenshotSem <- struct{}{}:
		defer func() { <-screenshotSem }()
	case <-slotTimer.C:
		return nil, errScreenshotBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	vw := req.ViewportWidth
	if vw <= 0 {
		vw = 1440
	}
	if vw > 3840 {
		vw = 3840
	}
	vh := req.ViewportHeight
	if vh <= 0 {
		vh = 900
	}
	if vh > 2160 {
		vh = 2160
	}
	fullPage := true
	if req.FullPage != nil {
		fullPage = *req.FullPage
	}
	waitMs := req.WaitMs
	if waitMs <= 0 {
		waitMs = 800
	}
	if waitMs > 10000 {
		waitMs = 10000
	}

	captureCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.NoSandbox,
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("hide-scrollbars", true),
		chromedp.Flag("mute-audio", true),
		chromedp.WindowSize(vw, vh),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(captureCtx, opts...)
	defer allocCancel()
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	defer browserCancel()

	var buf []byte
	tasks := []chromedp.Action{
		chromedp.EmulateViewport(int64(vw), int64(vh)),
		chromedp.Navigate(target),
		chromedp.Sleep(time.Duration(waitMs) * time.Millisecond),
	}
	if fullPage {
		tasks = append(tasks, chromedp.FullScreenshot(&buf, 90))
	} else {
		tasks = append(tasks, chromedp.CaptureScreenshot(&buf))
	}
	if err := chromedp.Run(browserCtx, tasks...); err != nil {
		return nil, fmt.Errorf("chromedp run: %w", err)
	}

	res := &ScreenshotResult{
		URL:         target,
		ImageBase64: base64.StdEncoding.EncodeToString(buf),
		Format:      "png",
		FullPage:    fullPage,
		SizeBytes:   len(buf),
	}
	res.Viewport.Width = vw
	res.Viewport.Height = vh
	return res, nil
}

// Screenshot is the HTTP entry point. Reads JSON body, calls
// CaptureScreenshot, returns the result. Both this and the MCP tool
// wrapper hit the same shared CaptureScreenshot function.
func (h *ScreenshotHandler) Screenshot(w http.ResponseWriter, r *http.Request) {
	a := requireAgent(w, r)
	if a == nil {
		return
	}
	if !requireWrite(w, a) {
		return
	}
	var req ScreenshotRequest
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	res, err := CaptureScreenshot(r.Context(), req)
	if err != nil {
		if errors.Is(err, errScreenshotBusy) {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeError(w, http.StatusBadGateway, fmt.Sprintf("screenshot failed: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// validateScreenshotURL rejects anything that's not on the configured
// allow-list (locks the endpoint down so it can't be turned into an
// SSRF probe of internal infrastructure). The allow-list is set once
// at boot from the loaded config (PrimaryDomain + BuiltSiteSuffix);
// loopback always passes so local dev works without any env vars.
func validateScreenshotURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("malformed url: %w", err)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return errors.New("url scheme must be http or https")
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return errors.New("url missing host")
	}
	cur := screenshotAllowedSuffixes.Load()
	if cur == nil || len(*cur) == 0 {
		// No allow-list configured = local dev: permit ONLY loopback (any port,
		// so a contributor can screenshot their `make dev` server). Nothing else
		// resolves here (no public allow-list to match against).
		if isLoopbackScreenshotHost(host) {
			return nil
		}
		return errors.New("screenshot disabled: configure ATOMICSITE_PRIMARY_DOMAIN or BUILT_SITE_SUFFIX to enable public-domain screenshots")
	}

	// Production (allow-list configured). Port restriction: only the standard
	// web ports, so a tenant agent cannot screenshot http://127.0.0.1:8091 (the
	// admin API) or any internal service on an arbitrary port.
	if p := parsed.Port(); p != "" && p != "80" && p != "443" {
		return fmt.Errorf("screenshot port %q not allowed (only 80/443)", p)
	}

	// loopback is NOT auto-allowed in production (that was an SSRF hole into
	// co-located internal services). The host must be in the allow-list AND must
	// not resolve to a private/loopback/link-local address (defends
	// DNS-rebinding: an allow-listed built-site subdomain whose A record points
	// at 169.254.169.254 / 10.x).
	matched := false
	for _, entry := range *cur {
		// Suffix-style entries (".tenants.example.com") match any hostname that
		// ends with the suffix; bare entries ("example.com") match the apex AND
		// any subdomain (foo.example.com) without bleeding into siblings.
		if strings.HasPrefix(entry, ".") {
			if strings.HasSuffix(host, entry) {
				matched = true
				break
			}
			continue
		}
		if host == entry || strings.HasSuffix(host, "."+entry) {
			matched = true
			break
		}
	}
	if !matched {
		return fmt.Errorf("host %q not in screenshot allow-list", host)
	}
	// A non-resolving host is not an SSRF vector (chromedp navigation will just
	// fail), and blocking on a transient DNS hiccup would break legitimate
	// screenshots, so only reject when the host actually resolves to a
	// private/loopback/link-local address (the DNS-rebind case).
	if ips, err := net.LookupIP(host); err == nil {
		for _, ip := range ips {
			if isPrivateIP(ip) {
				return fmt.Errorf("screenshot host %q resolves to a non-public address", host)
			}
		}
	}
	return nil
}

// isLoopbackScreenshotHost returns true for localhost / 127.x / ::1.
// Lets dev contributors point the screenshot tool at their local
// `make dev` server without any env-var setup.
func isLoopbackScreenshotHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}
