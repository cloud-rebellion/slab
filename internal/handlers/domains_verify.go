// /.well-known/atomic-verify/{token}
//
// Public, no-auth endpoint mounted alongside the static-asset handlers.
// Answers HTTP-01-style ownership probes for site_domains. Verification
// flow:
//
//  1. Admin creates a site_domains row; we mint a verify_token.
//  2. Admin points DNS at our edge IP and returns to the admin UI.
//  3. The reconciler (or admin clicking "Refresh") fetches
//     http://{hostname}/.well-known/atomic-verify/{token} via plain
//     HTTP. nginx routes the bare-port-80 request to a `location
//     /.well-known/atomic-verify/` block that proxies HERE. If we
//     respond with the matching token, ownership is proven and the
//     row flips to status='verified'.
//
// Why HTTP-01-style instead of TXT records: TXT requires a second DNS
// round-trip and can be cached for hours; the plain-HTTP probe falls
// out of the same nginx that already terminates the wildcard, so the
// "DNS is wired correctly" signal is identical to "your site can be
// served from this hostname". This is what Let's Encrypt itself uses.
package handlers

import (
	"net/http"
	"strings"
)

// VerifyToken serves the verification token for the matching domain row.
// Returns 404 (and never the token) for any unmatched URL so the
// endpoint does not double as a domain-enumeration tool.
//
// GET /.well-known/atomic-verify/{token}
func (h *DomainHandler) VerifyToken(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(urlParam(r, "token"))
	if token == "" {
		http.NotFound(w, r)
		return
	}
	row, err := h.queries.GetDomainByVerifyToken(r.Context(), token)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(row.VerifyToken))
}
