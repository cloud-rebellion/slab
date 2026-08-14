package handlers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg" // registers the JPEG decoder; full_page captures are JPEG
	"image/png"    // encodes the marker image AND registers the PNG decoder
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// This file proves the egress guard in screenshot_egress.go with a REAL browser
// against a REAL "internal" service.
//
// The threat it pins: validateScreenshotURL and the loopback-host check run
// ONCE, before chromedp.Navigate. Everything the PAGE does afterwards used to
// be unguarded, and the resulting render is handed back to the caller as
// image_base64. The mechanisms below (top-level navigation, iframe, img) were
// measured leaking at 96-100% before the proxy existed and 0% after, and none
// of them needs JavaScript, which is why no CSP closes this.
//
// ABLATION: delete the two proxy flags in captureScreenshot (proxy-server and
// proxy-bypass-list) and every subtest here fails with the internal service's
// hit counter above zero and the internal marker colour filling the frame.

// internalMarker is the colour the fake internal service paints. It is chosen
// to be nothing a blocked-request error page or a blank viewport would ever
// produce.
var internalMarker = color.RGBA{R: 255, G: 0, B: 255, A: 255}

// findChromeForTest mirrors chromedp's own browser lookup. A machine without
// Chromium must SKIP with a reason, never pass quietly: a green run on a box
// with no browser proves nothing about the guard.
func findChromeForTest() string {
	var locations []string
	switch runtime.GOOS {
	case "darwin":
		locations = []string{
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		}
	case "windows":
		locations = []string{
			"chrome",
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			filepath.Join(os.Getenv("USERPROFILE"), `AppData\Local\Google\Chrome\Application\chrome.exe`),
		}
	default:
		locations = []string{
			"headless_shell", "headless-shell", "chromium", "chromium-browser",
			"google-chrome", "google-chrome-stable", "google-chrome-beta",
			"google-chrome-unstable", "/usr/bin/google-chrome",
		}
	}
	for _, l := range locations {
		if p, err := exec.LookPath(l); err == nil {
			return p
		}
	}
	return ""
}

// attackPageSentinel is the colour the ATTACK PAGE paints, distinct from the
// internal marker. Asserting it rendered is how a subtest proves the capture
// actually happened, rather than the guard blocking everything including the
// page under test. Mutation testing found that gap: with the exempt-origin rule
// deleted so the proxy refused everything, all six leak subtests still passed.
var attackPageSentinel = color.RGBA{R: 0, G: 255, B: 255, A: 255}

// requireOrSkipBrowser skips when no browser is present, EXCEPT under
// SLAB_REQUIRE_BROWSER_TESTS, where it fails instead.
//
// scripts/ci.sh runs `go test ./... -count=1` with no -v, so a skip prints
// nothing but `ok`, and the entire proof of this unit would vanish silently on
// any runner without Chromium. Measured: with the browser lookup forced empty,
// plain `go test` prints `ok ... 1.113s` and only -v reveals the SKIPs. So CI
// exports the variable and an absent browser is a CI failure, while a laptop
// without Chrome still skips cleanly.
func requireOrSkipBrowser(t *testing.T) {
	t.Helper()
	if findChromeForTest() != "" {
		return
	}
	msg := fmt.Sprintf("no Chromium/Chrome found in any location chromedp searches on %s, so the screenshot egress guard was NOT exercised", runtime.GOOS)
	if os.Getenv("SLAB_REQUIRE_BROWSER_TESTS") != "" {
		t.Fatalf("SLAB_REQUIRE_BROWSER_TESTS is set and %s. Install Chromium on this runner or unset the variable; a browser-less run must not report this guard as covered", msg)
	}
	t.Skipf("SKIPPED FOR A REASON, NOT PASSING: %s on this machine", msg)
}

// magentaPNG is the image body the fake internal service serves for <img>.
func magentaPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, internalMarker)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode marker png: %v", err)
	}
	return buf.Bytes()
}

// startInternalService stands up the service that must never be reached, on
// addr. It counts every request it receives; that counter is the primary
// assertion, because it is immune to how the browser happens to paint.
func startInternalService(t *testing.T, addr string) (base string, hits *atomic.Int64) {
	t.Helper()
	hits = &atomic.Int64{}
	imgBody := magentaPNG(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if strings.HasPrefix(r.URL.Path, "/marker.png") {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(imgBody)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><body style="margin:0;background:#ff00ff">` +
			`<h1 style="color:#ff00ff">INTERNAL-SERVICE-SECRET</h1></body></html>`))
	})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("internal service listen on %s: %v", addr, err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = ln.Close()
	})
	return "http://" + ln.Addr().String(), hits
}

// startAttackPage serves the capture target: an ordinary-looking page whose
// body is supplied per mechanism. This is the origin the caller validated, so
// it is the one origin the egress policy exempts.
// The sentinel strip sits above the mechanism element (which covers the whole
// viewport), so a mechanism that renders nothing still leaves proof that the
// attack page itself loaded.
func startAttackPage(t *testing.T, body func() string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><body style="margin:0;background:#ffffff">` +
			body() +
			`<div id="sentinel" style="position:fixed;top:0;left:0;right:0;height:25%;background:#00ffff;z-index:2147483647"></div>` +
			`</body></html>`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// markerFraction decodes the capture and reports what share of its pixels are
// the internal marker colour, within tolerance.
//
// image.Decode with a colour TOLERANCE, not png.Decode with equality: the
// full-page path emits JPEG (chromedp emits JPEG for any quality != 100), so
// png.Decode returns "invalid format" on a perfectly good capture and an
// equality check on a lossy codec never matches even when the leak is total.
// Either mistake turns this test into a permanent false pass.
func markerFraction(t *testing.T, b64 string) float64 {
	return colourFraction(t, b64, internalMarker)
}

// colourFraction is markerFraction for an arbitrary target colour, so the same
// decode path serves both the leak assertion and the attack-page-rendered
// positive control.
func colourFraction(t *testing.T, b64 string, want color.RGBA) float64 {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	img, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode image (%d bytes): %v", len(raw), err)
	}
	t.Logf("capture decoded as %s, %d bytes", format, len(raw))
	bounds := img.Bounds()
	total := bounds.Dx() * bounds.Dy()
	if total == 0 {
		t.Fatalf("capture has zero pixels")
	}
	// Per-channel tolerance rather than equality: the full-page path is JPEG,
	// so an exact match never fires even on a total leak.
	const tol = 60
	near := func(got, w uint32) bool {
		d := int(got) - int(w)
		if d < 0 {
			d = -d
		}
		return d <= tol
	}
	hits := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			if near(r>>8, uint32(want.R)) && near(g>>8, uint32(want.G)) && near(b>>8, uint32(want.B)) {
				hits++
			}
		}
	}
	return float64(hits) / float64(total)
}

// privateNonLoopbackAddr returns a private, non-loopback address this machine
// actually holds, or "" if it has none. That is the address family the original
// measurement used, and it is the one Chromium does NOT bypass a proxy for by
// default, so it exercises a different code path from the loopback cases.
func privateNonLoopbackAddr() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		ip4 := ipnet.IP.To4()
		if ip4 == nil || !ip4.IsPrivate() {
			continue
		}
		return ip4.String()
	}
	return ""
}

// TestScreenshotEgressBlocksInternalReads is the proof. Each subtest is a full
// capture through the real CaptureLoopbackScreenshot path.
func TestScreenshotEgressBlocksInternalReads(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a real headless browser; skipped under -short")
	}
	requireOrSkipBrowser(t)

	// The allow-list is package-level state other tests mutate. Empty it, which
	// is the unconfigured-dev posture: validateScreenshotURL then permits only
	// loopback, and netguard.DialControl refuses every loopback address, so the
	// ONLY reachable origin is the capture target itself.
	prev := ScreenshotAllowedSuffixes()
	ConfigureScreenshotAllowList()
	t.Cleanup(func() { ConfigureScreenshotAllowList(prev...) })
	stubScreenshotResolver(t, nil)

	targets := map[string]string{"loopback second port": "127.0.0.1:0"}
	if priv := privateNonLoopbackAddr(); priv != "" {
		targets["private non-loopback"] = net.JoinHostPort(priv, "0")
	} else {
		t.Log("no private non-loopback address on this machine; that variant is not covered here")
	}

	// navigatesAway marks a mechanism that replaces the document, so the attack
	// page's own sentinel is legitimately gone from the final frame.
	type mechanism struct {
		body          func(internal string) string
		navigatesAway bool
	}
	mechanisms := map[string]mechanism{
		"top-level navigation": {
			body: func(internal string) string {
				return `<meta http-equiv="refresh" content="0;url=` + internal + `/">`
			},
			navigatesAway: true,
		},
		"iframe": {body: func(internal string) string {
			return `<iframe src="` + internal + `/" style="position:fixed;inset:0;width:100vw;height:100vh;border:0"></iframe>`
		}},
		"img": {body: func(internal string) string {
			return `<img src="` + internal + `/marker.png" style="position:fixed;inset:0;width:100vw;height:100vh">`
		}},
	}

	for netName, bindAddr := range targets {
		for mechName, mech := range mechanisms {
			t.Run(netName+"/"+mechName, func(t *testing.T) {
				internal, hits := startInternalService(t, bindAddr)
				victimHost := strings.TrimPrefix(internal, "http://")
				page := startAttackPage(t, func() string { return mech.body(internal) })
				resetScreenshotEgressRefusals()

				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()

				res, err := CaptureLoopbackScreenshot(ctx, ScreenshotRequest{
					URL:            page.URL + "/",
					ViewportWidth:  600,
					ViewportHeight: 400,
					WaitMs:         2000,
				})
				if err != nil {
					t.Fatalf("capture failed: %v", err)
				}

				// PRIMARY: the internal service was never reached. This is the
				// assertion that carries the finding, because it is immune to
				// how the browser happens to paint.
				if got := hits.Load(); got != 0 {
					t.Errorf("EGRESS LEAK: internal service at %s received %d request(s) via %s; the renderer reached past the one-shot URL check", internal, got, mechName)
				}

				// EVIDENCE THE ATTACK FIRED. Without this, "the guard worked"
				// and "the mechanism never ran" are the same green, which
				// mutation testing demonstrated by replacing a mechanism with
				// an HTML comment and watching the subtest pass. Counting
				// refusals for THIS victim specifically, not in total: the
				// renderer's own background traffic is refused too.
				refusals := screenshotEgressRefusalCount(victimHost)
				if refusals == 0 {
					t.Errorf("NO EVIDENCE THE ATTACK FIRED: the egress proxy never refused %s via %s, so this subtest proves nothing about the guard", victimHost, mechName)
				}

				// SECONDARY: whatever rendered, it is not the internal
				// service's content. The threshold is well under the 1.71% an
				// unscaled 64x64 marker would occupy in this viewport, so a
				// partial leak cannot slip beneath it.
				frac := markerFraction(t, res.ImageBase64)
				if frac > 0.005 {
					t.Errorf("EGRESS LEAK: %.1f%% of the capture is the internal service's marker colour via %s; its content was rendered and returned to the caller", frac*100, mechName)
				}

				// POSITIVE CONTROL: the attack page itself rendered, so a
				// block-everything regression cannot masquerade as containment.
				sentinel := colourFraction(t, res.ImageBase64, attackPageSentinel)
				if !mech.navigatesAway && sentinel < 0.10 {
					t.Errorf("THE ATTACK PAGE DID NOT RENDER: only %.1f%% sentinel pixels via %s; the proxy may be blocking the capture target itself, which would make the leak assertions vacuous", sentinel*100, mechName)
				}

				t.Logf("%s via %s: internal hits=%d, refusals of %s=%d, marker pixels=%.3f%%, sentinel pixels=%.1f%%",
					netName, mechName, hits.Load(), victimHost, refusals, frac*100, sentinel*100)
			})
		}
	}
}

// TestScreenshotEgressAllowsTheCaptureTarget guards against the fix being
// "correct" by breaking the product: the exempt origin must still render.
func TestScreenshotEgressAllowsTheCaptureTarget(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a real headless browser; skipped under -short")
	}
	requireOrSkipBrowser(t)
	prev := ScreenshotAllowedSuffixes()
	ConfigureScreenshotAllowList()
	t.Cleanup(func() { ConfigureScreenshotAllowList(prev...) })
	stubScreenshotResolver(t, nil)

	// The target paints the marker colour itself. It is the exempt origin, so
	// it MUST come through.
	page := startAttackPage(t, func() string {
		return `<div style="position:fixed;inset:0;background:#ff00ff"></div>`
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := CaptureLoopbackScreenshot(ctx, ScreenshotRequest{
		URL:            page.URL + "/",
		ViewportWidth:  600,
		ViewportHeight: 400,
		WaitMs:         1200,
	})
	if err != nil {
		t.Fatalf("capture of the exempt origin failed: %v", err)
	}
	// The page paints magenta everywhere, and startAttackPage overlays a 25%
	// cyan sentinel strip, so the expected split is roughly 75/25. Asserting
	// BOTH bands rather than one keeps this honest: a blank white frame would
	// satisfy neither.
	marker := markerFraction(t, res.ImageBase64)
	sentinel := colourFraction(t, res.ImageBase64, attackPageSentinel)
	t.Logf("exempt origin rendered: marker=%.1f%% sentinel=%.1f%%", marker*100, sentinel*100)
	if marker < 0.60 {
		t.Errorf("the capture target itself did not render: only %.1f%% marker pixels; the egress proxy is blocking the origin it is supposed to allow", marker*100)
	}
	if sentinel < 0.10 {
		t.Errorf("the attack-page sentinel did not render: only %.1f%% sentinel pixels", sentinel*100)
	}
}

// TestScreenshotEgressPolicy exercises the policy without a browser, so the
// rule survives on machines that skip the browser tests.
func TestScreenshotEgressPolicy(t *testing.T) {
	prev := ScreenshotAllowedSuffixes()
	t.Cleanup(func() { ConfigureScreenshotAllowList(prev...) })
	stubScreenshotResolver(t, nil)

	t.Run("loopback target exempts exactly its own origin", func(t *testing.T) {
		ConfigureScreenshotAllowList()
		e, err := startScreenshotEgress("http://127.0.0.1:8080/_preview/tok")
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		defer e.stop()

		if _, exempt, perr := e.permit("127.0.0.1:8080", "80"); perr != nil || !exempt {
			t.Errorf("the minted preview origin must be exempt; got exempt=%v err=%v", exempt, perr)
		}
		// A SECOND loopback port is the co-located-service read this closes.
		// It is not exempt, and rule (b) cannot produce a loopback dial.
		if _, exempt, _ := e.permit("127.0.0.1:8091", "80"); exempt {
			t.Error("a different loopback port must NOT be exempt; that is the admin-API read")
		}
		if _, derr := e.dial(context.Background(), "127.0.0.1:8091", false); derr == nil {
			t.Error("the guarded dialer must refuse a loopback address")
		}
		if _, derr := e.dial(context.Background(), "10.1.2.3:80", false); derr == nil {
			t.Error("the guarded dialer must refuse a private address")
		}
	})

	t.Run("public target exempts nothing", func(t *testing.T) {
		ConfigureScreenshotAllowList("example.com")
		e, err := startScreenshotEgress("https://example.com/page")
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		defer e.stop()

		if e.exemptOrigin != "" {
			t.Errorf("a public capture target must exempt nothing; got %q", e.exemptOrigin)
		}
		if _, _, perr := e.permit("example.com:443", "443"); perr != nil {
			t.Errorf("the allow-listed host must be permitted: %v", perr)
		}
		if _, _, perr := e.permit("127.0.0.1:8091", "80"); perr == nil {
			t.Error("loopback must not be permitted when an allow-list is configured")
		}
		if _, _, perr := e.permit("169.254.169.254:80", "80"); perr == nil {
			t.Error("the cloud metadata address must not be permitted")
		}
	})

	// This subtest pins a DELIBERATE RELAXATION so a future reader can tell it
	// apart from a leak. Sub-resources are bounded by netguard, not by the
	// screenshot allow-list: an off-allow-list PUBLIC origin (a webfont host, a
	// CDN image) is reachable, because a capture that silently drops a tenant's
	// typography makes design critique and Inspector grading misjudge the exact
	// thing they exist to judge. Everything netguard calls internal stays
	// refused, which is the whole of the measured SSRF finding.
	t.Run("sub-resources are bounded by netguard, not by the allow-list", func(t *testing.T) {
		ConfigureScreenshotAllowList("example.com")
		e, err := startScreenshotEgress("https://example.com/page")
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		defer e.stop()

		// PERMITTED: public third-party sub-resource origins on 80/443, none of
		// which are on the allow-list. Hostnames defer to DialControl at
		// connect time; public literals pass outright. This is the fidelity the
		// relaxation exists to buy: webfonts and CDN imagery render.
		for _, allowed := range []string{
			"fonts.gstatic.com:443",
			"fonts.googleapis.com:443",
			"cdn.jsdelivr.net:443",
			"93.184.216.34:443",
			"cdn.jsdelivr.net:80",
		} {
			if _, _, perr := e.permit(allowed, "443"); perr != nil {
				t.Errorf("public sub-resource origin %q must be permitted: %v", allowed, perr)
			}
		}

		// STILL REFUSED: everything netguard classifies as internal. These are
		// the addresses the SSRF matrix actually reached.
		for _, blocked := range []string{
			"10.1.2.3:80",
			"172.16.5.5:80",
			"192.168.68.102:80",
			"100.64.0.1:80",      // CGNAT / Tailscale
			"169.254.169.254:80", // cloud metadata
			"127.0.0.1:8091",     // co-located admin API
			"[::1]:8091",
			"[fc00::1]:80",         // ULA
			"[64:ff9b::7f00:1]:80", // NAT64 spelling of 127.0.0.1
		} {
			if _, _, perr := e.permit(blocked, "80"); perr == nil {
				t.Errorf("internal origin %q must still be refused as a sub-resource", blocked)
			}
		}

		// ALSO REFUSED: a PUBLIC host on a non-web port. The port ceiling is
		// deliberate, and is the one piece of validateScreenshotURL that rule
		// (b) keeps. Dropping the host allow-list for render fidelity does not
		// require dropping the ceiling with it: webfonts and CDNs are served
		// over 80/443 without exception, so the ceiling costs the render
		// nothing, while without it "any public host on any port" stays
		// reachable from a rendered page (an internet-exposed :9200, say). If
		// one of these ever moves to the permitted column, that is a security
		// decision and should be made as one.
		for _, blocked := range []string{
			"cdn.example.net:8443",
			"93.184.216.34:9200",
			"fonts.gstatic.com:22",
		} {
			if _, _, perr := e.permit(blocked, "443"); perr == nil {
				t.Errorf("public origin %q on a non-web port must be refused as a sub-resource", blocked)
			}
		}

		// And the dialer stays authoritative for the hostname case.
		if _, derr := e.dial(context.Background(), "10.1.2.3:80", false); derr == nil {
			t.Error("the guarded dialer must refuse a private address")
		}
	})

	// An exempt verdict authorises EXACTLY exemptOrigin. It used to authorise
	// "any loopback address", because the verdict was a bare boolean, the
	// callers re-derived the address they dialled, and loopbackOnlyDialControl
	// only asks "is this loopback". serveHTTP judged an https absolute-URI on
	// :80 and then dialled :443 under that verdict, so netguard was skipped for
	// every loopback port on the box.
	//
	// The old test could not catch this: it pinned exemptOrigin at 8080 and
	// never once called dial with exempt=true against a different address.
	t.Run("an exempt verdict does not authorise a different loopback origin", func(t *testing.T) {
		ConfigureScreenshotAllowList()
		e, err := startScreenshotEgress("http://127.0.0.1:8080/_preview/tok")
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		defer e.stop()

		// The verdict really is exempt for the origin it was decided about.
		origin, exempt, perr := e.permit("127.0.0.1:8080", "80")
		if perr != nil || !exempt {
			t.Fatalf("expected an exempt verdict for the preview origin; got exempt=%v err=%v", exempt, perr)
		}
		if origin != "127.0.0.1:8080" {
			t.Fatalf("permit returned origin %q, want the normalized exempt origin", origin)
		}

		// Carrying that verdict to ANY other loopback origin must be refused,
		// on any port, including the scheme-default ports that the re-derivation
		// bug produced.
		for _, other := range []string{
			"127.0.0.1:443",
			"127.0.0.1:80",
			"127.0.0.1:8091",
			"127.0.0.1:5432",
			"[::1]:8080",
		} {
			if _, derr := e.dial(context.Background(), other, true); derr == nil {
				t.Errorf("EXEMPT ESCALATION: an exempt verdict for %q authorised a dial to %q", origin, other)
			}
		}
	})

	// GAP 1, and the most important case in this file: every other assertion
	// here uses an IP LITERAL, which permit refuses via netguard.IsForbidden
	// before dial is ever reached. So the GUARDED DIALER'S Control hook was
	// never under test, and a mutant that replaces netguard.DialControl with a
	// net.ParseIP pre-check on the address string plus a bare net.Dialer passed
	// the entire suite including both real-browser families. That mutant is the
	// exact fail-open shape this whole workstream started from: a hostname
	// resolving to 10.x would be fetched.
	//
	// permit does no DNS by design, so a hostname reaches dial untouched, which
	// is what puts the Control hook under test.
	//
	// Assert the GUARD'S TEXT, not merely that an error occurred: the fail-open
	// version also errors on a closed port, just for the wrong reason
	// ("connect: connection refused" instead of "refusing to connect to blocked
	// address"). An err != nil assertion here would pass under the mutant.
	t.Run("the guarded dialer refuses a HOSTNAME that resolves inside", func(t *testing.T) {
		ConfigureScreenshotAllowList()
		e, err := startScreenshotEgress("https://example.com/page")
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		defer e.stop()

		// permit must let it through: it is not an IP literal and the port is
		// on the ceiling, so nothing here can refuse it. That is the point.
		origin, exempt, perr := e.permit("localhost:80", "80")
		if perr != nil {
			t.Fatalf("permit refused the hostname before dial, so the dialler is still not under test: %v", perr)
		}
		if exempt {
			t.Fatalf("localhost must not be exempt for a public capture target")
		}

		_, derr := e.dial(context.Background(), origin, false)
		if derr == nil {
			t.Fatal("FAIL-OPEN DIALLER: the guarded dialer connected to a hostname resolving to loopback")
		}
		if !strings.Contains(derr.Error(), "refusing to connect to blocked address") {
			t.Errorf("the dial failed, but NOT because the guard refused it: %v\n"+
				"want the netguard.DialControl refusal text; a connection-refused error here means the Control hook is not wired and the guard is fail-open for hostnames", derr)
		} else {
			t.Logf("guarded dialer refused the hostname correctly: %v", derr)
		}
	})

	t.Run("origin normalization is case and trailing-dot stable", func(t *testing.T) {
		ConfigureScreenshotAllowList()
		e, err := startScreenshotEgress("http://LocalHost:8080/x")
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		defer e.stop()
		if e.exemptOrigin != "localhost:8080" {
			t.Fatalf("exempt origin %q was not lower-cased at construction", e.exemptOrigin)
		}
		// Real case variation, which the old table lacked entirely: it held
		// only {"127.0.0.1:8080", "127.0.0.1.:8080"}, so dropping ToLower from
		// normalizeScreenshotOrigin passed.
		for _, form := range []string{
			"localhost:8080",
			"LOCALHOST:8080",
			"LocalHost:8080",
			"localhost.:8080",
			"LOCALHOST.:8080",
		} {
			if _, exempt, _ := e.permit(form, "80"); !exempt {
				t.Errorf("%q should normalize onto the exempt origin %q", form, e.exemptOrigin)
			}
		}
		// Normalization is TEXTUAL, so an alternate IPv6 spelling does not
		// collapse onto the exempt origin. That fails CLOSED (refused, not
		// admitted), which is the safe direction, and is asserted here so the
		// behaviour is a recorded property rather than an assumption.
		e6, err := startScreenshotEgress("http://[::1]:8080/x")
		if err != nil {
			t.Fatalf("start v6: %v", err)
		}
		defer e6.stop()
		if _, exempt, _ := e6.permit("[::1]:8080", "80"); !exempt {
			t.Error("the canonical IPv6 loopback spelling should be exempt")
		}
		if _, exempt, _ := e6.permit("[::0001]:8080", "80"); exempt {
			t.Error("an alternate IPv6 spelling must not be treated as the exempt origin")
		}
	})
}

// TestScreenshotImageFormatMatchesTheBytes pins the label to the bytes. The
// full-page path calls chromedp.FullScreenshot(&buf, 90), and chromedp emits
// JPEG for any quality != 100, while this struct hardcoded "png".
func TestScreenshotImageFormatMatchesTheBytes(t *testing.T) {
	cases := []struct {
		name string
		buf  []byte
		want string
	}{
		{"png magic", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0d"), "png"},
		{"jpeg magic", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10}, "jpeg"},
		{"empty", nil, "unknown"},
		{"garbage", []byte("not an image at all"), "unknown"},
	}
	for _, tc := range cases {
		if got := screenshotImageFormat(tc.buf); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}

	// And the real encoder: a quality-90 full-page capture is JPEG, so the
	// reported format must be "jpeg". Proven against the actual chromedp
	// encoder settings rather than asserted from the docs.
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	if got := screenshotImageFormat(pngBuf.Bytes()); got != "png" {
		t.Errorf("real png bytes reported as %q", got)
	}
	if _, _, err := image.Decode(bytes.NewReader(pngBuf.Bytes())); err != nil {
		t.Fatalf("image.Decode rejected real png: %v", err)
	}
}

// stop() must close hijacked CONNECT tunnels. http.Server.Shutdown deliberately
// leaves hijacked conns alone and ln.Close only stops new accepts, so before the
// fix a tunnel opened during a capture kept relaying afterwards: the listener
// refused new connections while an established tunnel still fetched from the
// exempt origin and got HTTP/1.1 200. The comment on stop claimed nothing could
// egress after it returned, which was false.
//
// ABLATION: delete the registerTunnel call in serveConnect and the tunnel-close
// loop in stop, and this fails with the second request succeeding after stop().
func TestScreenshotEgressStopClosesHijackedTunnels(t *testing.T) {
	prev := ScreenshotAllowedSuffixes()
	ConfigureScreenshotAllowList()
	t.Cleanup(func() { ConfigureScreenshotAllowList(prev...) })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("UPSTREAM-OK"))
	}))
	defer upstream.Close()

	e, err := startScreenshotEgress(upstream.URL + "/")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if e.exemptOrigin == "" {
		t.Fatalf("expected the loopback upstream to be the exempt origin")
	}

	// Open a CONNECT tunnel to the exempt origin and prove it relays.
	conn, err := net.DialTimeout("tcp", e.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, werr := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", e.exemptOrigin, e.exemptOrigin); werr != nil {
		t.Fatalf("write CONNECT: %v", werr)
	}
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT refused: %q", strings.TrimSpace(status))
	}
	for { // drain to the blank line
		line, rerr := br.ReadString('\n')
		if rerr != nil {
			t.Fatalf("drain CONNECT headers: %v", rerr)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}

	// http.ReadResponse rather than hand-parsing: it consumes the ENTIRE
	// response including the body, so the next read cannot be satisfied from
	// bytes the first response left in the buffer. Reading a status line by
	// hand made the post-stop assertion pass on leftover headers, which would
	// have looked like proof while proving nothing.
	relay := func() (string, error) {
		_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
		if _, werr := fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive\r\n\r\n", e.exemptOrigin); werr != nil {
			return "", werr
		}
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		resp, rerr := http.ReadResponse(br, nil)
		if rerr != nil {
			return "", rerr
		}
		body, berr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if berr != nil {
			return "", berr
		}
		return fmt.Sprintf("%s %s", resp.Status, strings.TrimSpace(string(body))), nil
	}

	before, berr := relay()
	if berr != nil || !strings.Contains(before, "200") || !strings.Contains(before, "UPSTREAM-OK") {
		t.Fatalf("tunnel did not relay before stop: %q err=%v", before, berr)
	}
	t.Logf("before stop: tunnel relayed %q", before)

	started := time.Now()
	e.stop()
	elapsed := time.Since(started)
	t.Logf("stop() returned in %v", elapsed)
	// Finding D: stop used to cost a flat 2s whenever anything was still active.
	if elapsed > time.Second {
		t.Errorf("stop() took %v; it must not block the capture path for a whole second", elapsed)
	}

	after, aerr := relay()
	if aerr == nil && strings.Contains(after, "200") {
		t.Errorf("TUNNEL SURVIVED stop(): the hijacked CONNECT tunnel still relayed %q, so egress outlived the capture that authorised it", after)
	} else {
		t.Logf("after stop: tunnel is dead (resp=%q err=%v)", after, aerr)
	}

	// And the listener is gone too.
	if c, derr := net.DialTimeout("tcp", e.addr, time.Second); derr == nil {
		_ = c.Close()
		t.Error("the proxy listener still accepts connections after stop()")
	}
}

// The proxy must not relay connection-scoped headers. Proxy-Authorization is
// the one that matters: it is credentials meant for THIS proxy, and forwarding
// it hands them upstream. Measured before the fix: upstream received
// Proxy-Authorization, Proxy-Connection and Connection verbatim.
func TestScreenshotEgressStripsHopByHopHeaders(t *testing.T) {
	got := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	prev := ScreenshotAllowedSuffixes()
	ConfigureScreenshotAllowList()
	t.Cleanup(func() { ConfigureScreenshotAllowList(prev...) })

	e, err := startScreenshotEgress(upstream.URL + "/")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer e.stop()

	req, err := http.NewRequest(http.MethodGet, upstream.URL+"/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Proxy-Authorization", "Basic c2VjcmV0")
	req.Header.Set("X-Custom-Hop", "should-be-dropped")
	req.Header.Set("Connection", "X-Custom-Hop")
	req.Header.Set("X-Kept", "should-survive")

	proxyURL, err := url.Parse(e.proxyURL())
	if err != nil {
		t.Fatalf("parse proxy url: %v", err)
	}
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true},
		Timeout:   10 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request through proxy: %v", err)
	}
	_ = resp.Body.Close()

	select {
	case h := <-got:
		for _, banned := range []string{"Proxy-Authorization", "Proxy-Connection", "X-Custom-Hop"} {
			if v := h.Get(banned); v != "" {
				t.Errorf("upstream received hop-by-hop header %s: %q", banned, v)
			}
		}
		if h.Get("X-Kept") != "should-survive" {
			t.Errorf("an ordinary end-to-end header was dropped: X-Kept=%q", h.Get("X-Kept"))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never received the request")
	}
}

// TestScreenshotEgressBlocksWebRTCUDP is the UDP half of the containment, and
// why it exists matters as much as what it asserts.
//
// The rest of this file's probe set is HTTP-only, so the proxy-flag ablation
// could never have expressed a UDP bypass, and a WebRTC hole shipped behind a
// green suite. --proxy-server is an HTTP forward proxy carrying TCP; Chromium's
// WebRTC stack opens its own UDP sockets and does its own DNS, so neither
// reaches the proxy, and CSP connect-src does not gate WebRTC in Chrome either.
// A page could therefore sweep internal ranges with one RTCPeerConnection per
// candidate against stun: URLs and read liveness off ICE timing.
//
// ABLATION: remove ONLY the webrtc-ip-handling-policy flag in captureScreenshot
// and this fails with datagrams landing on the victim listener.
func TestScreenshotEgressBlocksWebRTCUDP(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a real headless browser; skipped under -short")
	}
	requireOrSkipBrowser(t)

	prev := ScreenshotAllowedSuffixes()
	ConfigureScreenshotAllowList()
	t.Cleanup(func() { ConfigureScreenshotAllowList(prev...) })
	stubScreenshotResolver(t, nil)

	// The victim: a UDP listener standing in for any internal service. STUN
	// probes land here as datagrams, which the TCP proxy cannot see at all.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer func() { _ = pc.Close() }()
	victim := pc.LocalAddr().String()

	var datagrams atomic.Int64
	var stunHits atomic.Int64
	go func() {
		buf := make([]byte, 2048)
		for {
			n, _, rerr := pc.ReadFrom(buf)
			if rerr != nil {
				return
			}
			datagrams.Add(1)
			// STUN Binding Request: type 0x0001, magic cookie 0x2112A442.
			if n >= 8 && buf[0] == 0x00 && buf[1] == 0x01 &&
				buf[4] == 0x21 && buf[5] == 0x12 && buf[6] == 0xA4 && buf[7] == 0x42 {
				stunHits.Add(1)
			}
		}
	}()

	page := startAttackPage(t, func() string {
		return `<div style="position:fixed;inset:0;background:#ffffff"></div>
<script>
(function () {
  for (var i = 0; i < 4; i++) {
    try {
      var pc = new RTCPeerConnection({
        iceServers: [{ urls: "stun:` + victim + `" }],
        iceCandidatePoolSize: 4
      });
      pc.createDataChannel("probe" + i);
      pc.createOffer().then(function (o) { return pc.setLocalDescription(o); }).catch(function () {});
    } catch (e) {}
  }
})();
</script>`
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, cerr := CaptureLoopbackScreenshot(ctx, ScreenshotRequest{
		URL:            page.URL + "/",
		ViewportWidth:  400,
		ViewportHeight: 300,
		WaitMs:         4000,
	}); cerr != nil {
		t.Fatalf("capture failed: %v", cerr)
	}
	// ICE gathering is asynchronous; give any in-flight datagram time to land
	// rather than winning the assertion on a race.
	time.Sleep(1500 * time.Millisecond)

	d, s := datagrams.Load(), stunHits.Load()
	t.Logf("webrtc probe at %s: datagrams=%d stun_binding_requests=%d", victim, d, s)
	if d != 0 {
		t.Errorf("WEBRTC UDP LEAK: %d datagram(s) (%d STUN Binding Requests) reached the internal UDP listener at %s; the TCP proxy cannot see this channel, so webrtc-ip-handling-policy is what has to close it", d, s, victim)
	}
}
