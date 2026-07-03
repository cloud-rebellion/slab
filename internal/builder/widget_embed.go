package builder

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
)

// CookieProofWidget is the inline-config widget bundle, sourced from the
// CookieProof repo (`automations/CookieProof/`) and built into a same-origin
// distribution by `bun run build` (rollup target `cookieproof.embed.esm.js`).
//
// The bundle is auto-registering .  once the script runs, `<cookie-consent>`
// is defined and the embed entry reads `window.__CCB__` to configure it.
// No runtime fetch to consent.example.com.
//
// Updated in lockstep by the atomicsite Dockerfile's "widget" stage so a
// production build always carries the freshest widget code.
//
//go:embed assets/cookieproof.embed.esm.js
var CookieProofWidget []byte

// CookieProofWidgetHash returns the first 8 hex chars of SHA-256 over the
// embedded widget bytes. Used as a cache-busting filename suffix when the
// builder writes the asset into a tenant workspace's public/ directory.
func CookieProofWidgetHash() string {
	sum := sha256.Sum256(CookieProofWidget)
	return hex.EncodeToString(sum[:])[:8]
}

// CookieProofWidgetFilename returns the filename used for the per-tenant
// widget asset. Hashes over BOTH the widget bytes AND the per-site config
// prefix that gets prepended at write time. The reason: the same widget
// bytes are shipped to every tenant, but the prefix carries siteID,
// revision, branding cssVars, copy overrides, etc. Without the prefix
// in the hash, bumping `cookie_revision` would write a different file
// body under the SAME filename and browsers would serve cached stale
// config indefinitely. Prefix in the hash means revision bumps (and any
// other per-site config change) produce a fresh filename, so caches
// miss correctly.
//
// Pass the same `prefix` bytes you'll write to disk via
// WriteCookieProofWidgetAsset / RenderCookieProofConfigPrefix.
func CookieProofWidgetFilename(prefix []byte) string {
	if len(prefix) == 0 {
		return "_ccb." + CookieProofWidgetHash() + ".js"
	}
	h := sha256.New()
	h.Write(prefix)
	h.Write(CookieProofWidget)
	return "_ccb." + hex.EncodeToString(h.Sum(nil))[:12] + ".js"
}

// CircuitBgScript is the animated circuit-board background JS, embedded so
// any block with data-bg="circuit" can opt into a ~3.5KB animated PCB
// pattern in the site's primary color. Reads --color-primary from CSS so
// it auto-tints. Inline-rendered next to the canvas element by the
// renderer; no separate asset URL.
//
//go:embed assets/circuit-bg.js
var CircuitBgScript []byte
