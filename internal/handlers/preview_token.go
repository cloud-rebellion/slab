// Package handlers: preview-token manager.
//
// Backs the preview_screenshot MCP tool. The tool needs to drive headless
// Chromium against the admin draft-preview HTML, but that endpoint sits
// behind admin session auth which chromedp cannot easily replay. Solution:
// mint a single-use, short-lived, loopback-only token, point chromedp at
// http://127.0.0.1:{port}/_preview/{token}, consume on first hit, expire
// after 60s. Fonts resolve via the same loopback origin (the existing
// public /atomicsite-fonts/{site}/{font}.woff2 route).
//
// Security guarantees:
//   - Tokens are 32 bytes from crypto/rand, hex-encoded (64 chars).
//   - Single-use: the second request for the same token returns 404.
//   - 60s TTL: stale tickets are garbage-collected on every mint + serve.
//   - Loopback-only: requests where r.Host's hostname is not loopback
//     return 404 (defense-in-depth against the route accidentally being
//     proxied to the public internet).
//   - The map is in-memory only; tokens never persist to disk or
//     cross-process boundary.

package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bright-interaction/slab/internal/builder"
	"github.com/bright-interaction/slab/internal/store"
)

// PreviewTokenTTL is how long a minted token stays valid before being
// garbage-collected. Long enough to start chromedp + navigate; short
// enough that a leaked token is uninteresting.
const PreviewTokenTTL = 60 * time.Second

type previewTicket struct {
	siteID  string
	pageID  string
	expires time.Time
}

// PreviewTokenManager mints + redeems single-use tokens for the
// preview_screenshot tool. The MCP tool calls Mint in-process; chromedp
// hits Serve over loopback to redeem.
type PreviewTokenManager struct {
	queries *store.Queries

	mu      sync.Mutex
	tickets map[string]previewTicket
}

func NewPreviewTokenManager(queries *store.Queries) *PreviewTokenManager {
	return &PreviewTokenManager{
		queries: queries,
		tickets: make(map[string]previewTicket),
	}
}

// Mint registers a new single-use preview ticket for (siteID, pageID)
// and returns the opaque token. Caller is expected to embed the token in
// a loopback URL and pass that URL to chromedp. The token is consumed by
// the first request to Serve and unrecoverable after that.
func (m *PreviewTokenManager) Mint(siteID, pageID string) (string, error) {
	if m == nil {
		return "", nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(buf)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gcLocked(time.Now())
	m.tickets[tok] = previewTicket{
		siteID:  siteID,
		pageID:  pageID,
		expires: time.Now().Add(PreviewTokenTTL),
	}
	return tok, nil
}

// Serve consumes a token and streams the rendered draft HTML for the
// associated page. Single-use: a second request for the same token
// returns 404. Loopback-only: requests whose source isn't loopback are
// rejected with 404 (NOT 403, so we don't leak that the route exists).
func (m *PreviewTokenManager) Serve(w http.ResponseWriter, r *http.Request) {
	if !requestFromLoopback(r) {
		http.NotFound(w, r)
		return
	}

	tok := strings.TrimPrefix(r.URL.Path, "/_preview/")
	if tok == "" || strings.ContainsAny(tok, "/?#") {
		http.NotFound(w, r)
		return
	}

	m.mu.Lock()
	ticket, ok := m.tickets[tok]
	if ok {
		delete(m.tickets, tok)
	}
	m.gcLocked(time.Now())
	m.mu.Unlock()
	if !ok || time.Now().After(ticket.expires) {
		http.NotFound(w, r)
		return
	}

	html, err := builder.RenderPageDraft(r.Context(), m.queries, ticket.siteID, ticket.pageID)
	if err != nil {
		http.Error(w, "preview render failed", http.StatusInternalServerError)
		return
	}

	// Headers, including the CSP that contains this document, are set in
	// serveDraftPreview so all three RenderPageDraft routes share one policy.
	// This route previously set no CSP at all.
	serveDraftPreview(w, html)
}

// MintAndCount is a hook used in tests to assert ticket lifecycle.
func (m *PreviewTokenManager) ticketCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.tickets)
}

func (m *PreviewTokenManager) gcLocked(now time.Time) {
	for k, t := range m.tickets {
		if now.After(t.expires) {
			delete(m.tickets, k)
		}
	}
}

// requestFromLoopback returns true when the connecting peer is the
// local machine. Belt-and-braces gate so the route can't be turned into
// a draft-preview leak via a reverse-proxy misconfiguration.
func requestFromLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

// EnsureValidCtx is a no-op hook letting future iterations add quota /
// retention checks without churning the call signature. Kept exported so
// the MCP layer can call it before minting a token if we ever need to
// rate-limit preview captures.
func (m *PreviewTokenManager) EnsureValidCtx(_ context.Context, _ *store.Queries) error {
	return nil
}
