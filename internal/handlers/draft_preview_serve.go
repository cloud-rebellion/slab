package handlers

import (
	"context"
	"net/http"
	"strings"

	"github.com/bright-interaction/slab/internal/builder"
	"github.com/bright-interaction/slab/internal/store"
)

// draftPreviewCSPBase is the site-independent part of the draft-preview
// policy. The per-site media-src and frame-src are appended by
// draftPreviewCSP; see DraftPreviewCSP for the full rationale.
//
// Directive order and spelling are load-bearing for the guards in
// draft_preview_csp_test.go, which parse this into directive -> sources and
// assert EXACT source lists plus a known-directive whitelist. Adding a
// directive here without adding it there is a test failure, deliberately:
// an unpinned directive is how a policy gets widened without anyone noticing.
const draftPreviewCSPBase = "sandbox allow-scripts; " +
	"default-src 'none'; img-src 'self' data: https:; style-src 'self' 'unsafe-inline'; " +
	"font-src 'self' https: data:; script-src 'unsafe-inline'; base-uri 'none'; " +
	"form-action 'none'; frame-ancestors 'self'"

// DraftPreviewCSP is the policy for a site with no allow-listed domains. It
// is the base plus the closed-but-explicit media-src/frame-src a bare site
// gets. Kept exported because it is the value the route tests compare against
// for the no-allowlist case.
//
// Why the sandbox directive and not script-src 'none'
//
// The draft preview emits a stored component template body verbatim: the
// document is assembled with a strings.Builder, there is no html/template, and
// only prop substitutions are escaped. That is deliberate. A registered
// component IS Astro source, so its <style> and <script> are the feature, not
// an injection: draft_preview_inline_test.go locks in that the component
// animation actually runs in the preview. Sanitizing that sink would break the
// feature, so the containment has to come from the response instead.
//
// A write-scoped agent key can store that template (MCP create_component,
// or the REST agent components route), and the admin app opens the preview
// with window.open on a relative URL. So the payload runs as a TOP-LEVEL
// document on the admin origin. The session cookie is HttpOnly + SameSite=
// Strict, which stops token theft but is inert same-origin: a credentialed
// fetch to the admin API is same-origin, same-site, and fully authorized.
//
// What `sandbox allow-scripts` buys, measured in real headless Chrome with a
// live session cookie and a server logging what arrived:
//
//	no CSP        -> GET /api/admin/pages WITH_SESSION_COOKIE, body read by
//	                 script; POST also arrives with the cookie
//	sandbox alone -> window.origin "null", cookie NOT sent, read CORS-blocked,
//	                 but a no-cors POST STILL REACHES the server
//	sandbox+base  -> the connect-src family is dead, so fetch/XHR/WebSocket
//	                 never leave the browser
//
// WHAT THIS POLICY DOES NOT DO. An earlier version of this comment claimed
// "the request is never sent at all". That is false and worth stating plainly,
// because someone will drop a directive on the strength of a sentence like
// that. Measured: 14 request generators still egress under the full policy,
// including markup and constructed <img> against both 'self' and the https:
// wildcard, <link rel=stylesheet>, CSS @import, @font-face, rel=prefetch and
// rel=preload, and one of them can carry attacker-chosen data to an external
// host. Top-level navigation also still egresses. What actually holds is
// narrower and sufficient for THIS threat: the connect-src family is dead, the
// opaque origin holds so nothing is credentialed or readable, and form-action
// 'none' stops a form POST leaving the browser. Resource loads are uncredentialed
// and unreadable, which is why the escalation is closed even though egress is not.
//
// script-src is 'unsafe-inline' rather than 'none' because the component
// animation is a locked-in feature. This is a deliberate relaxation of the
// agent route's previous script-src 'none'; it is safe here only because the
// opaque origin and the dead connect-src family both hold. Do not drop either
// one and keep this.
//
// PREVIEW DIVERGES FROM PRODUCTION ON script-src, and an agent will hit this.
// The built site sends `script-src 'self'` plus the tenant allowlist
// (builder/security.go). Here it is 'unsafe-inline' with no host sources, so
// eval, new Function and WASM compilation all THROW in preview and work in
// production, and an external script the built site allows is refused in
// preview. No shipped starter component uses those, but components are
// agent-authored, so an agent can hit a preview-only failure and "fix" code
// that was already correct. Do not read "the animation is a locked-in feature"
// as meaning every script that works in production works here.
//
// 'self' is kept in img-src/style-src/font-src. It was NOT obvious that it
// still matches once the document origin is opaque, so it was measured: in
// Chrome a relative /media/ image and the inline <style> both still load
// under sandbox, and control policies of img-src 'none' / style-src 'none'
// were confirmed to break them, so those are real results and not a probe
// that cannot fail. /media/* and /atomicsite-fonts/* are public routes with
// no auth, so losing the cookie on subresource loads costs nothing.
var DraftPreviewCSP = draftPreviewCSP(builder.AllowedDomains{})

// draftPreviewCSP appends the per-site media-src and frame-src to the base.
//
// These two exist because `default-src 'none'` swallows every directive that
// is not named, and media-src and frame-src were not named. The built site
// falls back to `default-src 'self'`, so self-hosted video and same-origin
// frames play there; the preview fell back to 'none' and they did not. That
// shipped as a regression on the two routes that previously had no CSP at
// all. Measured in Chrome against the markup builder/blocks_extra.go emits:
// the mp4 request never left the browser, and neither did the iframe's.
//
// The host lists come from the SAME allowlist the built site uses
// (builder.LoadAllowedDomains), so the preview tells the truth about what
// production will do rather than inventing a policy of its own. A tenant that
// has not approved a third-party embed sees it blocked in preview because it
// is also blocked in production.
//
// frame-src 'self' is new attack surface and was measured before shipping,
// because it lets a sandboxed preview iframe a same-origin admin page. It does
// NOT reopen the escalation: the sandbox propagates to the child frame, so the
// child gets its own opaque origin, and site-for-cookies is null for both. In
// Chrome, with a live session cookie, the framed and <video>-sourced requests
// to /api/admin/pages both arrived NO_COOKIE and the parent read back
// FRAME_OPAQUE_OR_EMPTY rather than the admin body. Uncredentialed and
// unreadable, which is the same category as the other resource loads above.
func draftPreviewCSP(allow builder.AllowedDomains) string {
	media := append([]string{"'self'"}, allow.Media...)
	frame := append([]string{"'self'"}, allow.Frame...)
	return draftPreviewCSPBase +
		"; media-src " + strings.Join(media, " ") +
		"; frame-src " + strings.Join(frame, " ")
}

// draftPreviewCSPForSite resolves the policy for one site, falling back to the
// closed base if the allowlist cannot be read.
//
// Fail-closed on purpose: a transient DB error must not silently widen the
// policy, and must not block the preview either. The tenant's extra hosts are
// the only thing lost, and they are additive.
func draftPreviewCSPForSite(ctx context.Context, queries *store.Queries, siteID string) string {
	allow, err := builder.LoadAllowedDomains(ctx, queries, siteID)
	if err != nil {
		return DraftPreviewCSP
	}
	return draftPreviewCSP(allow)
}

// serveDraftPreview is the ONLY sanctioned way to write builder.RenderPageDraft
// bytes to a response.
//
// The bug this exists to prevent: the CSP was set on 1 of the 3 routes that
// serve these identical bytes. When the control lives in a response header,
// enumerating renderers is not enough, because the header is attached by
// whoever writes the body. Centralising the write means a fourth route gets
// the header by construction, and TestEveryRenderPageDraftCallerServesThroughHelper
// fails the build if a new caller writes the bytes itself instead.
func serveDraftPreview(w http.ResponseWriter, r *http.Request, queries *store.Queries, siteID, html string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", draftPreviewCSPForSite(r.Context(), queries, siteID))
	_, _ = w.Write([]byte(html))
}
