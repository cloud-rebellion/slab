package handlers

import "net/http"

// DraftPreviewCSP is the response policy for every route that serves
// builder.RenderPageDraft output.
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
// `sandbox allow-scripts` puts the document in an OPAQUE origin at the
// response level. Measured in real headless Chrome (see the ablation in the
// fix report), with the session cookie set:
//
//	no CSP            -> GET /api/admin/pages WITH_SESSION_COOKIE, body read
//	                     back by script; POST also arrives with the cookie
//	sandbox alone     -> window.origin "null", cookie NOT sent, read blocked,
//	                     but a no-cors POST STILL REACHES the server
//	sandbox + this    -> the request is never sent at all
//
// That last row is why the resource directives stay. `default-src 'none'`
// covers connect-src, so fetch/XHR/WebSocket are refused before a packet
// leaves the browser. Sandbox alone would leave an unauthenticated request
// generator pointed at the loopback interface. Two independent controls:
// opaque origin for authorization, connect-src for reachability.
//
// script-src is 'unsafe-inline' rather than 'none' because the component
// animation is a locked-in feature. This is a deliberate relaxation of the
// agent route's previous script-src 'none'; it is safe here only because the
// opaque origin and connect-src 'none' both hold. Do not drop either one and
// keep this.
//
// 'self' is kept in img-src/style-src/font-src. It was NOT obvious that it
// still matches once the document origin is opaque, so it was measured: in
// Chrome a relative /media/ image and the inline <style> both still load
// under sandbox, and control policies of img-src 'none' / style-src 'none'
// were confirmed to break them, so those are real results and not a probe
// that cannot fail. /media/* and /atomicsite-fonts/* are public routes with
// no auth, so losing the cookie on subresource loads costs nothing.
const DraftPreviewCSP = "sandbox allow-scripts; " +
	"default-src 'none'; img-src 'self' data: https:; style-src 'self' 'unsafe-inline'; " +
	"font-src 'self' https: data:; script-src 'unsafe-inline'; base-uri 'none'; " +
	"form-action 'none'; frame-ancestors 'self'"

// serveDraftPreview is the ONLY sanctioned way to write builder.RenderPageDraft
// bytes to a response.
//
// The bug this exists to prevent: the CSP was set on 1 of the 3 routes that
// serve these identical bytes. When the control lives in a response header,
// enumerating renderers is not enough, because the header is attached by
// whoever writes the body. Centralising the write means a fourth route gets
// the header by construction, and TestEveryRenderPageDraftCallerServesThroughHelper
// fails the build if a new caller writes the bytes itself instead.
func serveDraftPreview(w http.ResponseWriter, html string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", DraftPreviewCSP)
	_, _ = w.Write([]byte(html))
}
