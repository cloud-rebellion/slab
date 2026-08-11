package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/bright-interaction/slab/internal/shield"
)

// AgentShield puts the WHOLE REST agent surface behind the same PII
// boundary the MCP transport has, by wrapping the response writer instead
// of asking each handler to remember a call.
//
// Why it is a middleware and not another per-handler call. Shield used to
// live entirely inside internal/mcp. The 2026-08-11 audit found the shape
// that always follows from that: agent.ContextBuilder.Build had ten
// callers, the nine in internal/mcp ran their result through
// Session.ShieldJSON and the tenth, GET /api/agent/context, did not, even
// though both transports authenticate with the same X-Agent-Key and serve
// the same tenant. GET /api/agent/visitor-metadata was worse still,
// returning a visitor's raw email and the entire CRM metadata map while
// the MCP contract advertises visitor data and identified-tier PII as
// deliberately not exposed. Adding a second per-transport ShieldJSON call
// would have set up the third one to be missed, so the boundary moved to
// the one place every response on this surface has to pass through.
//
// It does BOTH directions, exactly like mcp.handleToolsCall:
//
//   - Request: a JSON body is run through Session.UnshieldJSON, so a
//     marker the agent echoes back into a write is resolved to plaintext
//     before the handler sees it. Without this, adding response shielding
//     would make the REST surface store literal "[shield:...]" strings in
//     tenant content, which is a known recorded failure of this exact
//     boundary.
//   - Response: a JSON body is run through Session.ShieldJSON, and text
//     bodies through Session.RedactString. On a redaction error the
//     response is WITHHELD, never forwarded raw.
//
// Ordering: this must be registered AFTER the agent auth middleware, so
// the identity is server-resolved. It fails closed if it is not.
//
// It is a no-op when the Gateway is disabled (no ATOMICSITE_SHIELD_KEY),
// which is the default for dev installs and unit tests.
func AgentShield(gw *shield.Gateway) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if !gw.Enabled() {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The MCP JSON-RPC endpoint is mounted inside the same
			// authenticated group but resolves its Session from the SAME
			// Gateway and shields per JSON-RPC method, because its errors
			// have to come back as JSON-RPC errors rather than as REST
			// bodies, and because it unshields tool ARGUMENTS specifically
			// rather than the envelope. Running both would double-process
			// every MCP response. This is the only exemption, and
			// agent_shield_test.go pins that.
			if isMCPPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			identity := GetAgent(r)
			if identity == nil {
				// Unreachable behind AgentAuthMiddleware, which 401s first.
				// Fail closed anyway: if this middleware is ever registered
				// ahead of auth, silently skipping redaction is the exact
				// outcome the boundary exists to prevent.
				slog.Error("shield: agent shield middleware ran without an authenticated identity", "path", r.URL.Path)
				writeShieldError(w, http.StatusInternalServerError, "shield: no authenticated agent identity")
				return
			}

			session, err := gw.Begin(r.Context(), identity.SiteID, identity.KeyID, "")
			if err != nil {
				slog.Error("shield: session begin failed", "err", err, "site_id", identity.SiteID)
				writeShieldError(w, http.StatusInternalServerError, "shield session error; request refused")
				return
			}
			if session == nil {
				next.ServeHTTP(w, r)
				return
			}

			ctx := shield.WithSession(r.Context(), session)
			r = r.WithContext(ctx)

			// --- request direction ---------------------------------------
			if isJSONContentType(r.Header.Get("Content-Type")) && r.Body != nil {
				raw, rerr := io.ReadAll(r.Body)
				_ = r.Body.Close()
				if rerr != nil {
					// Body-cap overrun or a broken connection. The handler
					// would have failed on the same read; answer cleanly.
					writeShieldError(w, http.StatusBadRequest, "could not read request body")
					return
				}
				// Fast path: a body with no marker in it needs no resolution,
				// and skipping it means the overwhelming majority of write
				// payloads are never unmarshaled and re-marshaled. That is a
				// correctness guard as much as a speed one: a round-trip
				// through map[string]any reorders keys and pushes every
				// number through float64.
				if bytes.Contains(raw, []byte("[shield:")) && json.Valid(raw) {
					resolved, uerr := session.UnshieldJSON(ctx, raw)
					if uerr != nil {
						// A marker from an expired or foreign session. Say so
						// rather than writing the literal token into content.
						writeShieldError(w, http.StatusBadRequest, "shield: failed to resolve request tokens")
						return
					}
					raw = resolved
				}
				// Malformed JSON is passed through untouched so the handler
				// emits its own "Invalid JSON body" error, unchanged.
				r.Body = io.NopCloser(bytes.NewReader(raw))
				r.ContentLength = int64(len(raw))
			}

			// --- response direction --------------------------------------
			sw := &shieldWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)

			body := sw.buf.Bytes()
			ct := w.Header().Get("Content-Type")
			switch {
			case len(body) == 0:
			case isJSONContentType(ct):
				shielded, serr := session.ShieldJSON(ctx, body)
				if serr != nil {
					if json.Valid(body) {
						// Fail CLOSED. A JSON body whose redaction errored may
						// carry PII the handler never tagged.
						slog.Error("shield: response redaction failed; withheld", "err", serr, "path", r.URL.Path)
						writeShieldError(w, http.StatusInternalServerError, "shield redaction failed; response withheld")
						return
					}
					// Content-Type lied and the body is not JSON. Nothing to
					// unmarshal means nothing was redacted, so withhold too.
					slog.Error("shield: non-JSON body on a JSON content type; withheld", "path", r.URL.Path)
					writeShieldError(w, http.StatusInternalServerError, "shield redaction failed; response withheld")
					return
				}
				body = shielded
			case isRedactableTextContentType(ct):
				redacted, rerr := session.RedactString(ctx, string(body))
				if rerr != nil {
					slog.Error("shield: text response redaction failed; withheld", "err", rerr, "path", r.URL.Path)
					writeShieldError(w, http.StatusInternalServerError, "shield redaction failed; response withheld")
					return
				}
				body = []byte(redacted)
			}

			// Content-Length was computed by the handler over the pre-shield
			// body, if it set one at all; markers change the length.
			if w.Header().Get("Content-Length") != "" {
				w.Header().Del("Content-Length")
			}
			w.WriteHeader(sw.status)
			if _, werr := w.Write(body); werr != nil {
				slog.Debug("shield: response write", "err", werr)
			}
		})
	}
}

// isMCPPath reports whether the request targets the JSON-RPC MCP mount.
// Both the historical /api/agent/mcp path and the current /mcp mount are
// matched so a route move cannot silently start double-shielding.
func isMCPPath(p string) bool {
	return p == "/mcp" || strings.HasPrefix(p, "/mcp/") ||
		p == "/api/agent/mcp" || strings.HasPrefix(p, "/api/agent/mcp/")
}

// isJSONContentType reports whether a Content-Type header names JSON.
func isJSONContentType(ct string) bool {
	base := strings.TrimSpace(strings.ToLower(ct))
	if i := strings.IndexByte(base, ';'); i >= 0 {
		base = strings.TrimSpace(base[:i])
	}
	return base == "application/json" || strings.HasSuffix(base, "+json")
}

// isRedactableTextContentType reports whether a text response should go
// through the free-form regex redact pass.
//
// text/html is deliberately excluded: the only HTML this surface serves is
// GET /api/agent/pages/{slug}/draft-preview, whose entire purpose is that
// the agent inspects the rendered layout. It renders tenant-authored page
// blocks, not visitor records, and rewriting strings inside markup would
// destroy the thing the endpoint exists to show.
func isRedactableTextContentType(ct string) bool {
	base := strings.TrimSpace(strings.ToLower(ct))
	if i := strings.IndexByte(base, ';'); i >= 0 {
		base = strings.TrimSpace(base[:i])
	}
	return base == "text/markdown" || base == "text/plain"
}

func writeShieldError(w http.ResponseWriter, status int, msg string) {
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// shieldWriter buffers a handler's response so the shield pass can run
// over the complete body before a single byte reaches the client. Once
// bytes are on the wire there is no withholding them, which is why this
// buffers rather than streams.
//
// It deliberately does NOT implement http.Flusher, http.Hijacker or
// Unwrap: a handler that feature-detects one of those would bypass the
// buffer and stream unredacted bytes past the boundary. Nothing on the
// agent surface streams today, and if something needs to, it needs a
// decision here first rather than a silent bypass.
type shieldWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	buf         bytes.Buffer
}

func (w *shieldWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.status = code
	w.wroteHeader = true
}

func (w *shieldWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.buf.Write(p)
}
