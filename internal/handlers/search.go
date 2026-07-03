// Package handlers - search.go.
//
// Global content search via SQLite FTS5. Three pieces:
//
//  1. SearchIndexer - stateless helpers that upsert/delete a row in
//     the search_index virtual table. Handler-side mutation paths
//     (page Create/Update/Delete, collection item, knowledgebase)
//     call the indexer after their own write so the FTS table stays
//     in sync. Idempotent: re-upserting the same resource_id wipes
//     the old row before inserting.
//
//  2. ReindexSite - drops every search_index row for a site and
//     repopulates by walking the live tables. Used on first deploy
//     after this lands, after a migration, or whenever the operator
//     wants to recover from an out-of-sync state.
//
//  3. SearchHandler - GET /api/sites/{siteID}/search?q=...&kind=...
//     and POST /api/sites/{siteID}/search/reindex. Both site-access-
//     gated.
//
// Indexed kinds: page, collection_item, knowledgebase, site. The
// "site" kind covers the shell (name + slug) so a search for the
// site name lands somewhere meaningful even before content exists.
//
// FTS5 isn't expressed in sqlc-generated code (sqlc doesn't model
// virtual tables), so this file uses raw *sql.DB queries.

package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/bright-interaction/slab/internal/store"
)

// SearchKind is one of the supported indexable resource types.
type SearchKind string

const (
	SearchKindPage SearchKind = "page"
	SearchKindItem SearchKind = "collection_item"
	SearchKindKB   SearchKind = "knowledgebase"
	SearchKindSite SearchKind = "site"
)

// validSearchKinds is the closed set the handler will accept on the
// query-side `?kind=` filter. Unknown kinds become an empty filter
// (search every kind) rather than an error so the URL is forgiving.
var validSearchKinds = map[string]bool{
	string(SearchKindPage): true,
	string(SearchKindItem): true,
	string(SearchKindKB):   true,
	string(SearchKindSite): true,
}

// searchMaxLimit caps the number of results in a single response.
// FTS5 ranking gets noisy past this; the dashboard's UI scrolls a
// few result groups, not a thousand.
const searchMaxLimit = 100

// SearchHandler exposes the search + reindex endpoints. Construct
// with the *sql.DB because FTS5 sits outside sqlc; the *store.Queries
// handle is used for the live table walk during reindex.
type SearchHandler struct {
	db      *sql.DB
	queries *store.Queries
}

func NewSearchHandler(db *sql.DB, queries *store.Queries) *SearchHandler {
	return &SearchHandler{db: db, queries: queries}
}

// SearchResult is the JSON shape returned to the SPA. excerpt is
// FTS5's snippet() output (HTML-safe; mark tags wrap matches).
type SearchResult struct {
	SiteID     string  `json:"site_id"`
	Kind       string  `json:"kind"`
	ResourceID string  `json:"resource_id"`
	Title      string  `json:"title"`
	Excerpt    string  `json:"excerpt"`
	Rank       float64 `json:"rank"`
}

// Search runs the FTS5 MATCH query and returns ranked results.
//
// GET /api/sites/{siteID}/search?q=<query>&kind=<kind>&limit=<n>
func (h *SearchHandler) Search(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	if !isSafeSiteID(siteID) {
		writeError(w, http.StatusBadRequest, "Invalid siteID")
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusOK, map[string]any{"results": []any{}})
		return
	}
	if len(q) > 200 {
		// FTS5 query strings of unbounded length are a soft DoS vector;
		// 200 chars is generous for any real search.
		writeError(w, http.StatusBadRequest, "Search query too long (max 200 chars)")
		return
	}
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	limit := 25
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > searchMaxLimit {
				n = searchMaxLimit
			}
			limit = n
		}
	}

	// Sanitize the user query for FTS5: wrap each token in quotes so
	// reserved syntax (NEAR, AND, OR, NOT, "*", parentheses) is
	// treated as literal text. Operators using advanced syntax can
	// still hit the raw FTS5 surface via a separate tool; the public
	// search API stays safe-by-default.
	safeQuery := buildFTS5Query(q)
	if safeQuery == "" {
		writeJSON(w, http.StatusOK, map[string]any{"results": []any{}})
		return
	}

	args := []any{siteID, safeQuery}
	sql := `SELECT site_id, kind, resource_id, title,
		    snippet(search_index, 4, '<mark>', '</mark>', '...', 32) AS excerpt,
		    rank
		FROM search_index
		WHERE site_id = ? AND search_index MATCH ?`
	if validSearchKinds[kind] {
		sql += ` AND kind = ?`
		args = append(args, kind)
	}
	sql += ` ORDER BY rank LIMIT ?`
	args = append(args, limit)

	rows, err := h.db.QueryContext(r.Context(), sql, args...)
	if err != nil {
		// FTS5 throws a syntax error on broken queries; we already
		// quote everything so this should be rare. 500 with a
		// generic message keeps internals out of the response.
		writeError(w, http.StatusInternalServerError, "Search failed")
		return
	}
	defer rows.Close()

	out := []SearchResult{}
	for rows.Next() {
		var rsLine SearchResult
		if err := rows.Scan(&rsLine.SiteID, &rsLine.Kind, &rsLine.ResourceID, &rsLine.Title, &rsLine.Excerpt, &rsLine.Rank); err != nil {
			continue
		}
		out = append(out, rsLine)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": out, "query": q})
}

// Reindex drops every search_index row for the site and repopulates
// from the live tables. Admin-grade operation; the route mount
// already restricts to site members + admin global-role bypass.
//
// POST /api/sites/{siteID}/search/reindex
func (h *SearchHandler) Reindex(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	if !isSafeSiteID(siteID) {
		writeError(w, http.StatusBadRequest, "Invalid siteID")
		return
	}
	count, err := h.ReindexSite(r.Context(), siteID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Reindex failed: "+err.Error())
		return
	}
	AuditLog(r.Context(), h.queries, r,
		siteID, AuditActionUpdate, "search_index_rebuilt", siteID,
		map[string]any{"rows_indexed": count})
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "rows_indexed": count})
}

// ReindexSite is the package-internal helper for both the HTTP route
// and any background-job invocation (e.g. boot-time). Reads live
// data through h.queries first, THEN opens the FTS write
// transaction; SQLite with SetMaxOpenConns(1) deadlocks if a read
// from h.queries is attempted while a tx already holds the only
// connection.
func (h *SearchHandler) ReindexSite(ctx context.Context, siteID string) (int, error) {
	// Phase 1: gather every row we'll write. None of these reads
	// share a connection with the FTS tx that follows.
	site, _ := h.queries.GetSiteByID(ctx, siteID)
	pages, _ := h.queries.ListPagesBySite(ctx, siteID)
	cols, _ := h.queries.ListCollectionsBySite(ctx, siteID)
	type itemTuple struct {
		id, title, body string
	}
	itemRows := make([]itemTuple, 0)
	for _, c := range cols {
		items, _ := h.queries.ListItemsByCollection(ctx, c.ID)
		for _, it := range items {
			itemRows = append(itemRows, itemTuple{it.ID, it.Title, flattenItemBody(it.DataJson)})
		}
	}
	kbs, _ := h.queries.ListKnowledgebaseBySite(ctx, siteID)

	// Phase 2: drop + insert in a single tx so a partial failure
	// rolls back to the previous state.
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM search_index WHERE site_id = ?`, siteID); err != nil {
		return 0, err
	}

	count := 0
	if site.ID != "" {
		if err := txInsertSearchRow(ctx, tx, siteID, string(SearchKindSite), site.ID, site.Name, site.Slug); err != nil {
			return 0, err
		}
		count++
	}
	for _, p := range pages {
		if err := txInsertSearchRow(ctx, tx, siteID, string(SearchKindPage), p.ID, p.Title, p.MetaDescription+"\n"+p.Slug); err != nil {
			return 0, err
		}
		count++
	}
	for _, it := range itemRows {
		if err := txInsertSearchRow(ctx, tx, siteID, string(SearchKindItem), it.id, it.title, it.body); err != nil {
			return 0, err
		}
		count++
	}
	for _, k := range kbs {
		if err := txInsertSearchRow(ctx, tx, siteID, string(SearchKindKB), k.ID, k.Title, k.Content); err != nil {
			return 0, err
		}
		count++
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

// SearchUpsert wipes any prior search_index row matching (site_id,
// kind, resource_id) and inserts the new title + body. Called from
// the page / item / kb mutation handlers after a successful write.
// Errors are non-fatal: a search miss is recoverable, a refused
// content write is not, so we don't abort the calling handler.
func SearchUpsert(ctx context.Context, db *sql.DB, siteID string, kind SearchKind, resourceID, title, body string) {
	if db == nil || siteID == "" || resourceID == "" {
		return
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM search_index WHERE site_id = ? AND kind = ? AND resource_id = ?`,
		siteID, string(kind), resourceID); err != nil {
		return
	}
	_, _ = db.ExecContext(ctx,
		`INSERT INTO search_index (site_id, kind, resource_id, title, body) VALUES (?, ?, ?, ?, ?)`,
		siteID, string(kind), resourceID, title, body)
}

// SearchDelete removes a row from the search_index. Called from the
// content delete handlers; ignores missing rows.
func SearchDelete(ctx context.Context, db *sql.DB, siteID string, kind SearchKind, resourceID string) {
	if db == nil || siteID == "" || resourceID == "" {
		return
	}
	_, _ = db.ExecContext(ctx,
		`DELETE FROM search_index WHERE site_id = ? AND kind = ? AND resource_id = ?`,
		siteID, string(kind), resourceID)
}

func txInsertSearchRow(ctx context.Context, tx *sql.Tx, siteID, kind, resourceID, title, body string) error {
	if siteID == "" || resourceID == "" {
		return errors.New("site_id and resource_id required")
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO search_index (site_id, kind, resource_id, title, body) VALUES (?, ?, ?, ?, ?)`,
		siteID, kind, resourceID, title, body)
	return err
}

// flattenItemBody pulls every string field out of a collection item's
// data_json so the FTS body sees the searchable text. Ignores
// non-string fields (numbers, booleans, refs) which carry no search
// signal.
func flattenItemBody(dataJSON string) string {
	if strings.TrimSpace(dataJSON) == "" {
		return ""
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(dataJSON), &data); err != nil {
		return ""
	}
	var b strings.Builder
	for _, v := range data {
		walkAndAppendStrings(v, &b)
	}
	return b.String()
}

func walkAndAppendStrings(v any, b *strings.Builder) {
	switch t := v.(type) {
	case string:
		if t != "" {
			b.WriteString(t)
			b.WriteByte('\n')
		}
	case map[string]any:
		for _, vv := range t {
			walkAndAppendStrings(vv, b)
		}
	case []any:
		for _, vv := range t {
			walkAndAppendStrings(vv, b)
		}
	}
}

// buildFTS5Query takes the raw user query and returns an FTS5 MATCH
// expression that treats every token as a quoted literal phrase. This
// neutralizes FTS5's reserved syntax (NEAR, OR, NOT, AND, "*",
// parentheses, "...") so user input can't break the parser. Empty
// input returns "" so the caller can short-circuit.
//
// Algorithm: split on whitespace, drop tokens shorter than 1 char,
// wrap each in double quotes (escaping internal double-quotes per
// FTS5 docs by doubling them), join with " AND " for an
// implicit-AND search. Tokens shorter than 2 chars are kept (single-
// letter unicode characters carry meaning in many languages).
func buildFTS5Query(q string) string {
	parts := strings.Fields(q)
	if len(parts) == 0 {
		return ""
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		// Strip FTS5-syntax-relevant characters from the token. We
		// keep alphanumerics + apostrophes + hyphens + slashes which
		// commonly appear inside legitimate words / paths.
		p = sanitizeFTS5Token(p)
		if p == "" {
			continue
		}
		// Double-quote escaping: FTS5 uses "" inside a quoted phrase.
		escaped := strings.ReplaceAll(p, `"`, `""`)
		out = append(out, `"`+escaped+`"`)
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, " AND ")
}

// sanitizeFTS5Token drops characters that have meaning in FTS5
// query grammar (parentheses, NEAR, asterisks, colons, etc.) but
// keeps everything that's normal text content.
func sanitizeFTS5Token(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '(', ')', ':', '*', '^':
			continue
		}
		out = append(out, c)
	}
	return string(out)
}
