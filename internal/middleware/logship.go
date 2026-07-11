package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
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
	h := &flareSlogHandler{next: slog.Default().Handler(), shipper: sh, minLvl: logShipLevel()}
	slog.SetDefault(slog.New(h))
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
			attrs = b
		}
	}
	h.shipper.enqueue(nativeLogLine{
		Severity:   strings.ToLower(r.Level.String()),
		Body:       r.Message,
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
