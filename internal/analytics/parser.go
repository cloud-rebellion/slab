// Package analytics tails per-site Nginx JSON access logs, hashes IPs into
// stable fingerprints, and writes visit_events + visit_sessions rows.
//
// IPs are NEVER persisted in plain form. The fingerprint hash is the only
// IP-derived value stored; salt is per-site so cross-site correlation is
// not possible from the database alone.
package analytics

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/bright-interaction/slab/internal/store"
)

// newID generates a random 12-byte hex ID, matching the convention used by
// the rest of the atomicsite codebase (internal/handlers.newID).
func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// pollInterval governs how often the parser re-checks the log file when there
// is nothing new to read. Short enough to feel real-time, long enough that
// idle sites don't waste CPU.
const pollInterval = 250 * time.Millisecond

// logLine matches the JSON fields emitted by the nginx log_format we generate
// in internal/builder/nginx.go. Unknown fields are ignored.
type logLine struct {
	TS      string  `json:"ts"`
	SiteID  string  `json:"site_id"`
	IP      string  `json:"ip"`
	UA      string  `json:"ua"`
	AL      string  `json:"al"`
	Path    string  `json:"path"`
	Status  int     `json:"status"`
	MS      float64 `json:"ms"`
	Ref     string  `json:"ref"`
	Country string  `json:"country"` // CF-IPCountry (Cloudflare). Empty when not behind CF.
}

// Parser tails one site's JSON access log and persists analytics rows.
// One Parser per site; the Manager owns lifecycle.
type Parser struct {
	siteID  string
	logPath string
	salt    string
	queries *store.Queries
	logger  *slog.Logger
	stop    chan struct{}
}

// NewParser builds a Parser. Salt must be non-empty (caller derives a per-site
// salt from cfg.AnalyticsSalt + site_id; see Manager).
func NewParser(siteID, logPath, salt string, queries *store.Queries) *Parser {
	return &Parser{
		siteID:  siteID,
		logPath: logPath,
		salt:    salt,
		queries: queries,
		logger:  slog.With("component", "analytics.parser", "site_id", siteID),
		stop:    make(chan struct{}),
	}
}

// Stop signals Run to exit. Safe to call once.
func (p *Parser) Stop() {
	select {
	case <-p.stop:
		// already closed
	default:
		close(p.stop)
	}
}

// Run tails the log file forever (or until ctx/Stop). It handles:
//   - file not yet existing (waits for nginx to create it)
//   - log rotation (file replaced on disk: detect via stat inode/size shrink)
//   - partial trailing line (buffer until \n arrives)
//
// Pure Go, no shell exec. Errors are logged and the loop continues so a
// transient failure doesn't take the site offline.
func (p *Parser) Run(ctx context.Context) {
	var (
		f      *os.File
		buf    []byte
		offset int64
		curIno uint64
	)

	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()

	open := func() bool {
		nf, err := os.Open(p.logPath)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				p.logger.Warn("open log", "error", err, "path", p.logPath)
			}
			return false
		}
		// Start at current end-of-file: we don't replay history.
		end, err := nf.Seek(0, io.SeekEnd)
		if err != nil {
			p.logger.Warn("seek end", "error", err)
			_ = nf.Close()
			return false
		}
		ino, _ := statInode(nf)
		if f != nil {
			_ = f.Close()
		}
		f = nf
		offset = end
		curIno = ino
		buf = buf[:0]
		return true
	}

	// Open immediately so we don't miss events written between Run() entry and
	// the first ticker fire. open() seeks to end-of-file so we don't replay
	// existing history.
	open()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	step := func() {
		if f == nil {
			if !open() {
				return
			}
		}

		// Detect rotation: file shrank, or the inode on disk changed.
		st, err := os.Stat(p.logPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				_ = f.Close()
				f = nil
				buf = buf[:0]
			}
			return
		}
		if st.Size() < offset || statInodeFromInfo(st) != curIno {
			p.logger.Info("log rotated, reopening", "path", p.logPath)
			if !open() {
				return
			}
			// After reopen, restart from beginning of the new file (its content
			// is by definition fresh, so no history-replay risk).
			offset = 0
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				p.logger.Warn("seek-after-rotate", "error", err)
				return
			}
			st, _ = os.Stat(p.logPath)
		}

		if st.Size() == offset {
			return
		}

		// Some filesystems don't reliably return new data when the file handle
		// has previously hit EOF. Explicitly seek to our tracked offset.
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			p.logger.Warn("seek", "error", err)
			return
		}

		chunk := make([]byte, 32*1024)
		for {
			n, err := f.Read(chunk)
			if n > 0 {
				offset += int64(n)
				buf = append(buf, chunk[:n]...)
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					p.logger.Warn("read log", "error", err)
				}
				break
			}
		}

		for {
			i := indexByte(buf, '\n')
			if i < 0 {
				break
			}
			line := buf[:i]
			buf = buf[i+1:]
			line = trimCR(line)
			if len(line) == 0 {
				continue
			}
			p.processLine(ctx, line)
		}
	}

	// Run one step immediately so freshly-written lines that landed before
	// the first tick aren't delayed by a full poll interval.
	step()

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stop:
			return
		case <-ticker.C:
			step()
		}
	}
}

// processLine parses one JSON line and records it. Bad lines are logged + skipped.
func (p *Parser) processLine(ctx context.Context, raw []byte) {
	var ll logLine
	if err := json.Unmarshal(raw, &ll); err != nil {
		p.logger.Debug("malformed log line", "error", err)
		return
	}
	if ll.Path == "" {
		return
	}

	fp := fingerprint(ll.IP, ll.UA, ll.AL, p.salt)
	ts := ll.TS
	if ts == "" {
		ts = time.Now().UTC().Format(time.RFC3339)
	}

	browser, os, device := ParseUA(ll.UA)
	lang := ParsePrimaryLanguage(ll.AL)
	utmSource, utmMedium, utmCampaign := ParseUTMFromPath(ll.Path)
	country := normaliseCountry(ll.Country)

	if err := p.queries.RecordVisitEvent(ctx, store.RecordVisitEventParams{
		ID:          newID(),
		SiteID:      p.siteID,
		Fingerprint: fp,
		SessionID:   "",
		Path:        ll.Path,
		Referer:     ll.Ref,
		Status:      int64(ll.Status),
		Ms:          int64(ll.MS * 1000),
		Ts:          ts,
		Browser:     browser,
		Os:          os,
		Device:      device,
		Country:     country,
		Lang:        lang,
		UtmSource:   utmSource,
		UtmMedium:   utmMedium,
		UtmCampaign: utmCampaign,
	}); err != nil {
		p.logger.Warn("record visit", "error", err)
		return
	}

	if err := p.queries.UpsertVisitSession(ctx, store.UpsertVisitSessionParams{
		ID:          newID(),
		SiteID:      p.siteID,
		Fingerprint: fp,
		StartedAt:   ts,
		LastSeenAt:  ts,
	}); err != nil {
		p.logger.Warn("upsert session", "error", err)
	}

	// 404 capture (Layer 5 / Sprint 2 of the migration system, 2026-05-06).
	// Aggregate hits by (site_id, path) so the migration dashboard can show
	// "top forgotten URLs" and offer one-click redirect creation. Wrapped
	// in a fast-path 404 check so the hot loop stays cheap for sites with
	// healthy traffic patterns.
	if ll.Status == 404 {
		// Strip query string + fragment so hits to /old?ref=foo and
		// /old?ref=bar coalesce into one row keyed on /old. The redirect
		// rule the operator creates will fire regardless of query string
		// because nginx exact-match ignores the query.
		path := stripQueryFragment(ll.Path)
		if path == "" {
			return
		}
		if err := p.queries.UpsertMissingURL(ctx, store.UpsertMissingURLParams{
			ID:      newID(),
			SiteID:  p.siteID,
			Path:    path,
			Referer: ll.Ref,
		}); err != nil {
			p.logger.Warn("upsert missing url", "error", err, "path", path)
		}
	}
}

// stripQueryFragment returns the path component without ?query or #fragment.
// 404 aggregation keys on path-only because that's what the redirect rule
// the operator creates will match against (nginx exact-match ignores
// the query string).
func stripQueryFragment(p string) string {
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		return p[:i]
	}
	return p
}

// normaliseCountry coerces the CF-IPCountry value into a clean ISO 3166-1
// alpha-2 code, or empty string. Cloudflare emits "XX" for unknown and
// "T1" for Tor exits — we drop both rather than poisoning aggregations.
func normaliseCountry(s string) string {
	if len(s) != 2 {
		return ""
	}
	upper := ""
	for _, r := range s {
		if r >= 'a' && r <= 'z' {
			r -= 32
		}
		if r < 'A' || r > 'Z' {
			return ""
		}
		upper += string(r)
	}
	if upper == "XX" || upper == "T1" {
		return ""
	}
	return upper
}

// fingerprint hashes IP + UA + Accept-Language + per-site salt with SHA-256
// and returns the first 16 lowercase hex chars. The IP itself is never stored.
func fingerprint(ip, ua, accLang, salt string) string {
	h := sha256.New()
	// Use newline separators to avoid trivial collisions when one input ends
	// with a substring of the next.
	_, _ = io.WriteString(h, ip)
	_, _ = h.Write([]byte{'\n'})
	_, _ = io.WriteString(h, ua)
	_, _ = h.Write([]byte{'\n'})
	_, _ = io.WriteString(h, accLang)
	_, _ = h.Write([]byte{'\n'})
	_, _ = io.WriteString(h, salt)
	sum := h.Sum(nil)
	return hex.EncodeToString(sum)[:16]
}

// --- helpers ---

func indexByte(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}

func trimCR(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\r' {
		return b[:len(b)-1]
	}
	return b
}
