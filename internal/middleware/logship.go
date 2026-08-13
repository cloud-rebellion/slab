package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// logShipMinDefault is the default floor for shipping a log line to Flare.
// warn+ keeps volume sane (info/debug stay local stderr); override with
// FLARE_LOG_LEVEL=debug|info|warn|error (or off to disable).
const logShipMinDefault = slog.LevelWarn

const (
	logShipBuffer    = 512             // records buffered before drop-on-full
	logShipBatch     = 50              // flush at this many records
	logShipFlushEach = 3 * time.Second // or at least this often
	logShipMaxPerMin = 300             // hard per-minute cap: bounds any storm or loop
	logShipMaxAttrs  = 8 << 10         // drop a record's attrs beyond this many bytes
)

var logShipOnce sync.Once

// installLogShipper wraps the current default slog handler so warn+ records are
// also shipped to Flare's native logs endpoint, giving the estate a real logs
// pillar without a new dependency. Best-effort: the app's own stderr logging is
// untouched, a full buffer drops rather than blocks, and a per-minute cap bounds
// any storm. Called once from InitFlare after sentry.Init; no-op when FLARE_DSN
// is unset/unparseable or FLARE_LOG_LEVEL=off.
func installLogShipper(service string) {
	logShipOnce.Do(func() { installLogShipperOnce(service) })
}

func installLogShipperOnce(service string) {
	if strings.EqualFold(os.Getenv("FLARE_LOG_LEVEL"), "off") {
		return
	}
	// The flare service IS the ingest endpoint; shipping its own warn+ logs back
	// to itself risks a self-amplification loop (a batch that 401s on a stale key
	// logs a warn, which ships, which 401s...). Flare reports its own errors to
	// its project via sentry already, so never HTTP self-ship.
	if service == "flare" {
		return
	}
	base, key, ok := parseDSNForLogs(os.Getenv("FLARE_DSN"))
	if !ok {
		return
	}
	sh := &logShipper{
		endpoint: base,
		key:      key,
		service:  service,
		ch:       make(chan nativeLogLine, logShipBuffer),
		client:   &http.Client{Timeout: 5 * time.Second},
	}
	go sh.run()
	// slog.SetDefault re-routes the standard log package through the new default
	// handler. slog's built-in defaultHandler itself writes VIA the standard log
	// package, so wrapping it and then SetDefault-ing the wrapper forms a cycle
	// (wrapper -> defaultHandler -> std log -> wrapper -> ...) that self-deadlocks
	// the non-reentrant std log mutex on the first log line after install. A service
	// that never installed a concrete slog handler (bare default) then hangs at
	// startup before it binds its port. Break the cycle: when the current handler is
	// the built-in default, pass through to a concrete stderr handler instead (it
	// writes straight to os.Stderr, never back through the std log package). A
	// concrete handler the app already installed is safe to wrap as-is.
	next := slog.Default().Handler()
	if fmt.Sprintf("%T", next) == "*slog.defaultHandler" {
		next = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	}
	h := &flareSlogHandler{next: next, shipper: sh, minLvl: logShipLevel()}
	slog.SetDefault(slog.New(h))
}

// sensitiveLogKeyParts are case-insensitive substrings whose attribute values
// are redacted before shipping a log record to the shared Flare logs store.
// Bare "key" is intentionally excluded (too broad: keyboard, monkey, ...).
var sensitiveLogKeyParts = []string{
	"password", "passwd", "secret", "token", "authorization", "bearer",
	"cookie", "credential", "api_key", "apikey", "access_key", "accesskey",
	"private_key", "privatekey", "vault_key", "new_value", "jwt", "session_id", "dsn",
}

func isSensitiveLogKey(key string) bool {
	k := strings.ToLower(key)
	for _, s := range sensitiveLogKeyParts {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

func logShipLevel() slog.Level {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("FLARE_LOG_LEVEL"))) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "error":
		return slog.LevelError
	case "warn", "warning":
		return slog.LevelWarn
	}
	return logShipMinDefault
}

// parseDSNForLogs turns a Sentry-style DSN ({scheme}://{key}@{host}/{dsnID})
// into the native-logs endpoint URL and the ingest key.
func parseDSNForLogs(dsn string) (endpoint, key string, ok bool) {
	if dsn == "" {
		return "", "", false
	}
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil || u.Host == "" {
		return "", "", false
	}
	k := u.User.Username()
	id := strings.Trim(u.Path, "/")
	if k == "" || id == "" {
		return "", "", false
	}
	return u.Scheme + "://" + u.Host + "/api/" + id + "/logs", k, true
}

type nativeLogLine struct {
	Severity   string          `json:"severity"`
	Body       string          `json:"body"`
	Attributes json.RawMessage `json:"attributes,omitempty"`
	TraceID    string          `json:"trace_id,omitempty"`
	Timestamp  string          `json:"timestamp"`
}

type logShipper struct {
	endpoint string
	key      string
	service  string
	ch       chan nativeLogLine
	dropped  atomic.Int64
	client   *http.Client

	mu          sync.Mutex
	windowStart time.Time
	windowCount int
}

// allow admits at most logShipMaxPerMin records per fixed minute window,
// bounding any warn storm or self-referential loop before it does work.
func (s *logShipper) allow() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if now.Sub(s.windowStart) >= time.Minute {
		s.windowStart = now
		s.windowCount = 0
	}
	if s.windowCount >= logShipMaxPerMin {
		return false
	}
	s.windowCount++
	return true
}

// enqueue is non-blocking: a full buffer drops the line so logging never stalls
// the app on a slow or unreachable Flare.
func (s *logShipper) enqueue(l nativeLogLine) {
	select {
	case s.ch <- l:
	default:
		s.dropped.Add(1)
	}
}

func (s *logShipper) run() {
	t := time.NewTicker(logShipFlushEach)
	defer t.Stop()
	batch := make([]nativeLogLine, 0, logShipBatch)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		body, err := json.Marshal(batch)
		batch = batch[:0]
		if err != nil {
			return
		}
		req, err := http.NewRequest(http.MethodPost, s.endpoint, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Flare-Key", s.key)
		if resp, err := s.client.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body) // drain for keep-alive reuse
			_ = resp.Body.Close()
		}
	}
	for {
		select {
		case l := <-s.ch:
			batch = append(batch, l)
			if len(batch) >= logShipBatch {
				flush()
			}
		case <-t.C:
			flush()
		}
	}
}

// flareSlogHandler tees warn+ records to the log shipper while passing every
// record through to the wrapped handler (stderr).
type flareSlogHandler struct {
	next    slog.Handler
	shipper *logShipper
	minLvl  slog.Level
	attrs   []slog.Attr
	groups  []string
}

func (h *flareSlogHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *flareSlogHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= h.minLvl && h.shipper != nil {
		h.ship(r)
	}
	return h.next.Handle(ctx, r)
}

func (h *flareSlogHandler) ship(r slog.Record) {
	// Rate-cap first, before any allocation: an over-cap storm/loop pays nothing.
	if !h.shipper.allow() {
		h.shipper.dropped.Add(1)
		return
	}
	m := make(map[string]any)
	traceID := ""
	prefix := ""
	if len(h.groups) > 0 {
		prefix = strings.Join(h.groups, ".") + "."
	}
	var addAttr func(pfx string, a slog.Attr)
	addAttr = func(pfx string, a slog.Attr) {
		if a.Value.Kind() == slog.KindGroup {
			gp := pfx
			if a.Key != "" {
				gp = pfx + a.Key + "."
			}
			for _, ga := range a.Value.Group() {
				addAttr(gp, ga)
			}
			return
		}
		key := pfx + a.Key
		if key == "trace_id" {
			traceID = a.Value.String()
			return
		}
		// Redact sensitive attribute values before they leave the process for the
		// shared Flare logs store. Log records are an egress boundary just like the
		// sentry event path (which BeforeSend already scrubs); a stray
		// slog.Error("...", "token", t) must not ship the token. Key-based so it
		// stays cheap and never touches legit values.
		if isSensitiveLogKey(key) {
			m[key] = "[redacted]"
			return
		}
		m[key] = a.Value.Any()
	}
	for _, a := range h.attrs {
		addAttr(prefix, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		addAttr(prefix, a)
		return true
	})
	var attrs json.RawMessage
	if len(m) > 0 {
		if b, err := json.Marshal(m); err == nil && len(b) <= logShipMaxAttrs {
			attrs = scrubLogJSON(b)
		}
	}
	h.shipper.enqueue(nativeLogLine{
		Severity:   strings.ToLower(r.Level.String()),
		Body:       scrubLogText(r.Message),
		Attributes: attrs,
		TraceID:    traceID,
		Timestamp:  r.Time.UTC().Format(time.RFC3339),
	})
}

func (h *flareSlogHandler) WithAttrs(as []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(h.attrs)+len(as))
	merged = append(merged, h.attrs...)
	merged = append(merged, as...)
	return &flareSlogHandler{next: h.next.WithAttrs(as), shipper: h.shipper, minLvl: h.minLvl, attrs: merged, groups: h.groups}
}

func (h *flareSlogHandler) WithGroup(name string) slog.Handler {
	groups := append(append([]string{}, h.groups...), name)
	return &flareSlogHandler{next: h.next.WithGroup(name), shipper: h.shipper, minLvl: h.minLvl, attrs: h.attrs, groups: groups}
}

// The value-level egress scrub.
//
// isSensitiveLogKey above gates on the attribute KEY, and that is not enough on
// its own. The key used everywhere in Go error logging is "error", which is not
// and must not be a sensitive key, yet the value under it is routinely a
// *url.Error whose exported URL field carries the endpoint and its ?token=
// query. So the single most common line in the estate,
//
//	slog.Error("webhook delivery failed", "error", err)
//
// shipped credentials to the shared Flare logs store in cleartext. That store is
// a different trust boundary from the process that logged them. The same applies
// to the record's own message, which is free text an author formats by hand.
//
// This is deliberately self-contained: logship.go is a vendored copy in a dozen
// separate Go modules with no shared package between them, and a fix that
// depends on one project's helper is a fix the other eleven cannot take.
var (
	scrubLogPEM = regexp.MustCompile(`(?s)-----BEGIN [^-]*PRIVATE KEY-----.*?-----END [^-]*PRIVATE KEY-----`)
	// Userinfo in a URL: scheme://user:password@host
	scrubLogURLCreds = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/\s:@"]+:[^/\s@"]+@`)
	// A credential-bearing query parameter, the shape *url.Error hands over.
	scrubLogQuery = regexp.MustCompile(`(?i)([?&](?:token|key|api[_-]?key|apikey|secret|password|passwd|pwd|sig|signature|access[_-]?token|refresh[_-]?token|auth|session|code)=)[^&\s"']+`)
	// Bearer / Basic / Token authorization values.
	scrubLogBearer = regexp.MustCompile(`(?i)\b(bearer|basic|token)\s+[A-Za-z0-9._\-+/=]{8,}`)
	// key=value / key: value assignments with a credential-looking name.
	scrubLogAssign = regexp.MustCompile(`(?i)\b(token|api[_-]?key|apikey|secret|password|passwd|pwd|access[_-]?token|refresh[_-]?token|authorization)\b(\s*[:=]\s*)["']?[^\s"',;&)}]+`)
)

// scrubLogText redacts credential shapes from one free-text value.
//
// Redaction, not destruction: the host and path of a failing URL survive, so an
// operator can still tell which call broke. Only the secret is replaced.
func scrubLogText(s string) string {
	if s == "" {
		return s
	}
	s = scrubLogPEM.ReplaceAllString(s, "[private-key]")
	s = scrubLogURLCreds.ReplaceAllString(s, "${1}[redacted]@")
	s = scrubLogQuery.ReplaceAllString(s, "${1}[secret]")
	s = scrubLogBearer.ReplaceAllString(s, "${1} [secret]")
	s = scrubLogAssign.ReplaceAllString(s, "${1}${2}[secret]")
	return s
}

// scrubLogJSON walks a marshalled attribute document and applies scrubLogText to
// STRING LEAVES ONLY, then re-marshals.
//
// Never run the text scrub over serialized JSON directly. The assignment rule
// matches `"password":"x"` shapes across the quoting, and a text rewrite of a
// serialized document can leave it unparseable; walking the structure cannot.
// Numbers are decoded as literal text, because decoding into float64 silently
// rounds every integer above 2^53, so a 19-digit id would reach the operator as
// 1.7123456789012346e+18.
func scrubLogJSON(raw []byte) []byte {
	if len(raw) == 0 {
		return raw
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		// Not JSON. Scrub as text and return a JSON string, still valid to embed.
		if out, merr := json.Marshal(scrubLogText(string(raw))); merr == nil {
			return out
		}
		return []byte(`"[unserializable]"`)
	}
	out, err := json.Marshal(scrubLogValue(v))
	if err != nil {
		// Fail closed: dropping the attrs beats shipping an unscrubbed document.
		return []byte(`{"_scrub_error":"redacted"}`)
	}
	return out
}

// scrubLogValue rewrites string leaves. Object KEYS are left alone: they are
// attribute names, isSensitiveLogKey already handled the sensitive ones, and
// rewriting a key would change what the operator is searching on.
func scrubLogValue(v any) any {
	switch t := v.(type) {
	case string:
		return scrubLogText(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = scrubLogValue(val)
		}
		return out
	case []any:
		for i := range t {
			t[i] = scrubLogValue(t[i])
		}
		return t
	default:
		return v
	}
}
