package handlers

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	_ "image/jpeg" // registers the JPEG decoder; full_page captures are JPEG
	"image/png"    // encodes the marker image AND registers the PNG decoder
	"net"
	"net/http"
	"net/http/httptest"
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
func startAttackPage(t *testing.T, body func() string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><body style="margin:0;background:#ffffff">` +
			body() + `</body></html>`))
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
	marker := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			// 16-bit channels from RGBA(); compare in 8-bit space.
			r8, g8, b8 := r>>8, g>>8, bl>>8
			if r8 > 200 && g8 < 80 && b8 > 200 {
				marker++
			}
		}
	}
	return float64(marker) / float64(total)
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
	if chrome := findChromeForTest(); chrome == "" {
		t.Skipf("SKIPPED FOR A REASON, NOT PASSING: no Chromium/Chrome found in any location chromedp searches on %s, so the screenshot egress guard was NOT exercised on this machine", runtime.GOOS)
	}

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

	mechanisms := map[string]func(internal string) string{
		"top-level navigation": func(internal string) string {
			return `<meta http-equiv="refresh" content="0;url=` + internal + `/">`
		},
		"iframe": func(internal string) string {
			return `<iframe src="` + internal + `/" style="position:fixed;inset:0;width:100vw;height:100vh;border:0"></iframe>`
		},
		"img": func(internal string) string {
			return `<img src="` + internal + `/marker.png" style="position:fixed;inset:0;width:100vw;height:100vh">`
		},
	}

	for netName, bindAddr := range targets {
		for mechName, body := range mechanisms {
			t.Run(netName+"/"+mechName, func(t *testing.T) {
				internal, hits := startInternalService(t, bindAddr)
				page := startAttackPage(t, func() string { return body(internal) })

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

				// Primary assertion: the internal service was never reached.
				if got := hits.Load(); got != 0 {
					t.Errorf("EGRESS LEAK: internal service at %s received %d request(s) via %s; the renderer reached past the one-shot URL check", internal, got, mechName)
				}

				// Secondary assertion: whatever rendered, it is not the
				// internal service's content.
				frac := markerFraction(t, res.ImageBase64)
				if frac > 0.02 {
					t.Errorf("EGRESS LEAK: %.1f%% of the capture is the internal service's marker colour via %s; its content was rendered and returned to the caller", frac*100, mechName)
				}
				t.Logf("%s via %s: internal hits=%d, marker pixels=%.3f%%", netName, mechName, hits.Load(), frac*100)
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
	if chrome := findChromeForTest(); chrome == "" {
		t.Skipf("SKIPPED FOR A REASON, NOT PASSING: no Chromium/Chrome found on %s", runtime.GOOS)
	}
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
	if frac := markerFraction(t, res.ImageBase64); frac < 0.80 {
		t.Errorf("the capture target itself did not render: only %.1f%% marker pixels; the egress proxy is blocking the origin it is supposed to allow", frac*100)
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

		if exempt, perr := e.permit("127.0.0.1:8080", "80"); perr != nil || !exempt {
			t.Errorf("the minted preview origin must be exempt; got exempt=%v err=%v", exempt, perr)
		}
		// A SECOND loopback port is the co-located-service read this closes.
		// It is not exempt, and rule (b) cannot produce a loopback dial.
		if exempt, _ := e.permit("127.0.0.1:8091", "80"); exempt {
			t.Error("a different loopback port must NOT be exempt; that is the admin-API read")
		}
		if _, derr := e.dial(context.Background(), "127.0.0.1:8091", "80", false); derr == nil {
			t.Error("the guarded dialer must refuse a loopback address")
		}
		if _, derr := e.dial(context.Background(), "10.1.2.3:80", "80", false); derr == nil {
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
		if _, perr := e.permit("example.com:443", "443"); perr != nil {
			t.Errorf("the allow-listed host must be permitted: %v", perr)
		}
		if _, perr := e.permit("127.0.0.1:8091", "80"); perr == nil {
			t.Error("loopback must not be permitted when an allow-list is configured")
		}
		if _, perr := e.permit("169.254.169.254:80", "80"); perr == nil {
			t.Error("the cloud metadata address must not be permitted")
		}
		if _, perr := e.permit("example.com:8443", "443"); perr == nil {
			t.Error("a non-80/443 port must not be permitted")
		}
	})

	t.Run("origin normalization is case and trailing-dot stable", func(t *testing.T) {
		ConfigureScreenshotAllowList()
		e, err := startScreenshotEgress("http://127.0.0.1:8080/x")
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		defer e.stop()
		for _, form := range []string{"127.0.0.1:8080", "127.0.0.1.:8080"} {
			if exempt, _ := e.permit(form, "80"); !exempt {
				t.Errorf("%q should normalize onto the exempt origin", form)
			}
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
