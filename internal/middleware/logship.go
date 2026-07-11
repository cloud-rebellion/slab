package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// logShipMinDefault is the default floor for shipping a log line to Flare.
// warn+ keeps volume sane (info/debug stay local stderr); override with
// FLARE_LOG_LEVEL=debug|info|warn|error.
const logShipMinDefault = slog.LevelWarn

const (
	logShipBuffer    = 512             // records buffered before drop-on-full
	logShipBatch     = 50              // flush at this many records
	logShipFlushEach = 3 * time.Second // or at least this often
)

// installLogShipper wraps the current default slog handler so that warn+ records
// are also shipped to Flare's native logs endpoint, giving the estate a real
// logs pillar without a new dependency. Best-effort: the app's own stderr
// logging is untouched, and a full buffer drops rather than blocks. Called from
// InitFlare after sentry.Init succeeds; no-op when FLARE_DSN is unset or
// unparseable, or FLARE_LOG_LEVEL=off.
func installLogShipper(service string) {
	if strings.EqualFold(os.Getenv("FLARE_LOG_LEVEL"), "off") {
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
	id := strings.Trim(u.Path, "/")
	if id == "" {
		return "", "", false
	}
	return u.Scheme + "://" + u.Host + "/api/" + id + "/logs", u.User.Username(), true
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
}

// enqueue is non-blocking: a full buffer drops the line so logging never stalls
// the app on a slow/unreachable Flare.
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
	m := make(map[string]any)
	traceID := ""
	add := func(a slog.Attr) bool {
		if a.Key == "trace_id" {
			traceID = a.Value.String()
			return true
		}
		m[a.Key] = a.Value.Any()
		return true
	}
	for _, a := range h.attrs {
		add(a)
	}
	r.Attrs(func(a slog.Attr) bool { return add(a) })
	var attrs json.RawMessage
	if len(m) > 0 {
		if b, err := json.Marshal(m); err == nil {
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
	return &flareSlogHandler{next: h.next.WithAttrs(as), shipper: h.shipper, minLvl: h.minLvl, attrs: merged}
}

func (h *flareSlogHandler) WithGroup(name string) slog.Handler {
	return &flareSlogHandler{next: h.next.WithGroup(name), shipper: h.shipper, minLvl: h.minLvl, attrs: h.attrs}
}
