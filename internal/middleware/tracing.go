package middleware

import (
	"net/http"
	"os"
	"strconv"
	"strings"

	sentry "github.com/getsentry/sentry-go"
)

// tracesSampleRate is the fraction of requests traced, tunable per service via
// FLARE_TRACES_SAMPLE_RATE (0..1). Default 0.1. A value <= 0 disables tracing
// with zero per-request overhead.
func tracesSampleRate() float64 {
	if v := strings.TrimSpace(os.Getenv("FLARE_TRACES_SAMPLE_RATE")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 && f <= 1 {
			return f
		}
	}
	return 0.1
}

// flareTracer wraps a handler in a Sentry transaction per request so the traces
// pillar populates from the same sentry-go SDK that already reports errors.
//
// Two deliberate safety choices, both learned the hard way:
//   - It does NOT wrap the ResponseWriter, so http.Flusher/Hijacker and SSE /
//     streaming handlers keep working (a wrapper that dropped those interfaces
//     would break every streaming endpoint estate-wide).
//   - isUntraceable skips Flare's own ingest routes. Tracing an ingest request
//     would emit a transaction envelope that is itself an ingest request, which
//     would be traced again: an unbounded self-amplification loop.
//
// No-op (returns next unchanged) when tracing is disabled.
func flareTracer(next http.Handler) http.Handler {
	if tracesSampleRate() <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isUntraceable(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		txn := sentry.StartTransaction(r.Context(),
			r.Method+" "+normalizeTracePath(r.URL.Path),
			sentry.ContinueFromRequest(r),
			sentry.WithOpName("http.server"),
			sentry.WithTransactionSource(sentry.SourceURL),
		)
		defer txn.Finish()
		next.ServeHTTP(w, r.WithContext(txn.Context()))
	})
}

// isUntraceable skips health/infra/static noise and, crucially, Flare's own
// ingest routes (see flareTracer). No non-Flare service serves these paths, so
// skipping them everywhere is harmless.
func isUntraceable(p string) bool {
	switch p {
	case "/health", "/healthz", "/readyz", "/livez", "/ready", "/metrics",
		"/favicon.ico", "/robots.txt", "/store", "/store/":
		return true
	}
	if strings.HasPrefix(p, "/static/") || strings.HasPrefix(p, "/assets/") ||
		strings.HasPrefix(p, "/_app/") || strings.HasPrefix(p, "/otlp") {
		return true
	}
	// Flare ingest under /api/{id}/{envelope|store|logs|traces|metrics|security}:
	// skip to break the ingest self-trace loop.
	if strings.HasPrefix(p, "/api/") {
		trimmed := strings.TrimRight(p, "/")
		for _, seg := range []string{"/envelope", "/store", "/logs", "/traces", "/metrics", "/security"} {
			if strings.HasSuffix(trimmed, seg) {
				return true
			}
		}
	}
	return false
}

// normalizeTracePath collapses high-cardinality segments (numeric ids, uuids,
// long cuid/hex tokens) to ":id" so transaction names stay low-cardinality,
// without coupling this shared package to any specific router.
func normalizeTracePath(p string) string {
	if p == "" || p == "/" {
		return p
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if idLike(s) {
			segs[i] = ":id"
		}
	}
	return strings.Join(segs, "/")
}

func idLike(s string) bool {
	if s == "" {
		return false
	}
	digits, token, hasDigit := true, true, false
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			hasDigit = true
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '-' || c == '_':
			digits = false
		default:
			digits, token = false, false
		}
	}
	if digits {
		return true // purely numeric id
	}
	// uuid / cuid / opaque token: id-charset, long, and carrying a digit.
	return token && hasDigit && len(s) >= 20
}
