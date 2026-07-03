package handlers

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/bright-interaction/slab/internal/config"
	"github.com/bright-interaction/slab/internal/store"
)

// RedirectHandler implements the per-site CRUD + bulk import + pre-launch
// verify surface for the redirects table. The auto-301 created on slug change
// (agent.go) writes directly to the same store; this handler is what humans
// and the agent use for everything else - migrations, bulk CSVs from Search
// Console, manual cleanup of the long tail of 404s.
//
// Cross-tenant safety: every mutation re-fetches by ID and matches SiteID
// before acting (mirrors CollectionHandler). Mounting under siteR in
// internal/server/server.go provides the outer auth gate.
type RedirectHandler struct {
	cfg     *config.Config
	queries *store.Queries
}

func NewRedirectHandler(cfg *config.Config, queries *store.Queries) *RedirectHandler {
	return &RedirectHandler{cfg: cfg, queries: queries}
}

// redirectMaxPerSite caps the total number of stored redirects per site.
// 50k covers the largest WordPress migrations we expect (5-15k typical) with
// headroom; beyond this nginx config size becomes a real concern and the
// operator probably needs regex consolidation instead of more rows.
const redirectMaxPerSite = 50_000

// redirectBulkMaxPerRequest limits a single bulk import payload. Higher than
// collections.BulkImport (1k items) because redirect rows are tiny and Search
// Console exports for big sites can run 10k+.
const redirectBulkMaxPerRequest = 10_000

var validRedirectStatusCodes = map[int64]bool{
	301: true, 302: true, 307: true, 308: true, 410: true,
}

// redirectInput is the public request shape for create / update. is_auto is
// always 0 for human creates - the slug-edit path in agent.go writes its own
// rows with is_auto=1 directly via store.CreateRedirect.
type redirectInput struct {
	FromPath   string `json:"from_path"`
	ToPath     string `json:"to_path"`
	StatusCode int64  `json:"status_code"`
}

// ----- CRUD -----

func (h *RedirectHandler) List(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	rows, err := h.queries.ListRedirectsBySite(r.Context(), siteID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list redirects")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, rr := range rows {
		out = append(out, redirectToMap(rr))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *RedirectHandler) Get(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	id := urlParam(r, "redirectID")
	rr, err := h.queries.GetRedirectByID(r.Context(), id)
	if err != nil || rr.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Redirect not found")
		return
	}
	writeJSON(w, http.StatusOK, redirectToMap(rr))
}

func (h *RedirectHandler) Create(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	var req redirectInput
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	if err := h.validateRedirectInput(r.Context(), siteID, req, ""); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	count, _ := h.queries.CountRedirectsBySite(r.Context(), siteID)
	if count >= redirectMaxPerSite {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("Site already at the %d-redirect cap; consolidate via regex rules", redirectMaxPerSite))
		return
	}
	if existing, err := h.queries.GetRedirectByPath(r.Context(), store.GetRedirectByPathParams{
		SiteID: siteID, FromPath: req.FromPath,
	}); err == nil && existing.ID != "" {
		writeError(w, http.StatusConflict, "A redirect from that path already exists")
		return
	}

	id := newID()
	if err := h.queries.CreateRedirect(r.Context(), store.CreateRedirectParams{
		ID:         id,
		SiteID:     siteID,
		FromPath:   req.FromPath,
		ToPath:     req.ToPath,
		StatusCode: req.StatusCode,
		IsAuto:     0,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create redirect")
		return
	}
	rr, _ := h.queries.GetRedirectByID(r.Context(), id)
	AuditLog(r.Context(), h.queries, r, siteID, AuditActionCreate, "redirect", id, map[string]any{
		"from": rr.FromPath, "to": rr.ToPath, "status": rr.StatusCode,
	})
	writeJSON(w, http.StatusCreated, redirectToMap(rr))
}

func (h *RedirectHandler) Update(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	id := urlParam(r, "redirectID")
	existing, err := h.queries.GetRedirectByID(r.Context(), id)
	if err != nil || existing.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Redirect not found")
		return
	}
	var req redirectInput
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	if err := h.validateRedirectInput(r.Context(), siteID, req, existing.FromPath); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	// If the from_path is changing, ensure the new path doesn't collide
	// with another existing rule.
	if req.FromPath != existing.FromPath {
		if other, err := h.queries.GetRedirectByPath(r.Context(), store.GetRedirectByPathParams{
			SiteID: siteID, FromPath: req.FromPath,
		}); err == nil && other.ID != "" && other.ID != id {
			writeError(w, http.StatusConflict, "Another redirect already covers that from_path")
			return
		}
	}
	if err := h.queries.UpdateRedirect(r.Context(), store.UpdateRedirectParams{
		ID: id, FromPath: req.FromPath, ToPath: req.ToPath, StatusCode: req.StatusCode,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to update redirect")
		return
	}
	rr, _ := h.queries.GetRedirectByID(r.Context(), id)
	AuditLog(r.Context(), h.queries, r, siteID, AuditActionUpdate, "redirect", id, map[string]any{
		"from": rr.FromPath, "to": rr.ToPath, "status": rr.StatusCode,
	})
	writeJSON(w, http.StatusOK, redirectToMap(rr))
}

func (h *RedirectHandler) Delete(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	id := urlParam(r, "redirectID")
	existing, err := h.queries.GetRedirectByID(r.Context(), id)
	if err != nil || existing.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Redirect not found")
		return
	}
	if err := h.queries.DeleteRedirect(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete redirect")
		return
	}
	AuditLog(r.Context(), h.queries, r, siteID, AuditActionDelete, "redirect", id, map[string]any{
		"from": existing.FromPath, "to": existing.ToPath,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ----- Bulk import -----

// BulkImport accepts either:
//   - application/json: {"items": [{"from_path": "...", "to_path": "...", "status_code": 301}, ...]}
//   - text/csv: header row optional; columns are from,to,status (status defaults to 301)
//
// Validation runs over the entire batch BEFORE any insert; on partial insert
// failure all rows in the batch are deleted. Matches collections.BulkImport
// rollback contract so the agent can retry safely.
func (h *RedirectHandler) BulkImport(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")

	items, err := h.parseBulkImportBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(items) == 0 {
		writeError(w, http.StatusBadRequest, "items array is required")
		return
	}
	if len(items) > redirectBulkMaxPerRequest {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("Bulk import capped at %d rows per request", redirectBulkMaxPerRequest))
		return
	}

	// Cap check up front so we don't insert N rows then fail row N+1.
	count, _ := h.queries.CountRedirectsBySite(r.Context(), siteID)
	if count+int64(len(items)) > redirectMaxPerSite {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("Importing %d rows would exceed site cap of %d", len(items), redirectMaxPerSite))
		return
	}
	// Sprint 3 (2026-05-06): plan-quota gate. Workspace plan caps are
	// stricter than the global redirectMaxPerSite for paid tiers
	// (solo=500, studio=2500, agency=10000) so the bulk import path
	// has to consult the plan before inserting. OSS / unlimited plans
	// fall through to the global 50k cap above. Auth required: an
	// anonymous bulk import returns 401 here, matching the migration
	// Apply contract.
	if status, msg := h.checkRedirectsQuotaForImport(r, siteID, count, int64(len(items))); status != 0 {
		writeError(w, status, msg)
		return
	}

	// Validate every row before touching the DB. Includes intra-batch
	// collision detection so a single bad batch is rejected as a unit.
	seenFrom := map[string]int{}
	for i, it := range items {
		if err := h.validateRedirectInput(r.Context(), siteID, it, ""); err != nil {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("Row %d: %s", i+1, err.Error()))
			return
		}
		if prev, ok := seenFrom[it.FromPath]; ok {
			writeError(w, http.StatusUnprocessableEntity,
				fmt.Sprintf("Row %d: from_path %q duplicates row %d in this batch", i+1, it.FromPath, prev+1))
			return
		}
		seenFrom[it.FromPath] = i
		if existing, err := h.queries.GetRedirectByPath(r.Context(), store.GetRedirectByPathParams{
			SiteID: siteID, FromPath: it.FromPath,
		}); err == nil && existing.ID != "" {
			writeError(w, http.StatusConflict,
				fmt.Sprintf("Row %d: a redirect from %q already exists", i+1, it.FromPath))
			return
		}
	}

	createdIDs := make([]string, 0, len(items))
	for _, it := range items {
		id := newID()
		if err := h.queries.CreateRedirect(r.Context(), store.CreateRedirectParams{
			ID: id, SiteID: siteID, FromPath: it.FromPath, ToPath: it.ToPath,
			StatusCode: it.StatusCode, IsAuto: 0,
		}); err != nil {
			for _, cid := range createdIDs {
				_ = h.queries.DeleteRedirect(r.Context(), cid)
			}
			writeError(w, http.StatusInternalServerError, "Bulk import failed mid-batch; rolled back")
			return
		}
		createdIDs = append(createdIDs, id)
	}
	AuditLog(r.Context(), h.queries, r, siteID, AuditActionCreate, "redirect_bulk", "", map[string]any{
		"count": len(createdIDs),
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"created": len(createdIDs),
		"ids":     createdIDs,
	})
}

// parseBulkImportBody reads either JSON or CSV depending on Content-Type.
// Both forms produce []redirectInput. Default status_code is 301 when omitted.
func (h *RedirectHandler) parseBulkImportBody(r *http.Request) ([]redirectInput, error) {
	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if strings.HasPrefix(ct, "text/csv") || strings.HasPrefix(ct, "application/csv") {
		return parseRedirectCSV(r.Body)
	}
	var req struct {
		Items []redirectInput `json:"items"`
	}
	if err := parseJSON(r, &req); err != nil {
		return nil, errors.New("Invalid JSON body")
	}
	for i := range req.Items {
		if req.Items[i].StatusCode == 0 {
			req.Items[i].StatusCode = 301
		}
	}
	return req.Items, nil
}

// parseRedirectCSV reads from,to,status rows. Header is auto-detected: if the
// first row's third column doesn't parse as an integer, that row is treated
// as a header and skipped. Supports two-column rows (status defaults to 301).
func parseRedirectCSV(body io.Reader) ([]redirectInput, error) {
	cr := csv.NewReader(body)
	cr.FieldsPerRecord = -1 // tolerate ragged rows; we validate per-row below
	records, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("CSV parse error: %v", err)
	}
	out := make([]redirectInput, 0, len(records))
	for i, rec := range records {
		if len(rec) < 2 {
			return nil, fmt.Errorf("Row %d: need at least from,to columns", i+1)
		}
		from := strings.TrimSpace(rec[0])
		to := strings.TrimSpace(rec[1])
		status := int64(301)
		if len(rec) >= 3 {
			s := strings.TrimSpace(rec[2])
			if s != "" {
				n, perr := strconv.ParseInt(s, 10, 64)
				if perr != nil {
					if i == 0 {
						// Header row - skip it.
						continue
					}
					return nil, fmt.Errorf("Row %d: status %q is not an integer", i+1, s)
				}
				status = n
			}
		}
		out = append(out, redirectInput{FromPath: from, ToPath: to, StatusCode: status})
	}
	return out, nil
}

// ----- Pre-launch verify -----

// VerifyRequest is the input shape for the verify endpoint.
type VerifyRequest struct {
	URLs []string `json:"urls"`
}

// VerifyResponse buckets every input URL into one of three outcomes so the
// migration UI can surface "X URLs will 404 after launch" before the operator
// flips DNS.
type VerifyResponse struct {
	Will200   []string `json:"will_200"`  // URL has a live page at the same slug
	Will301   []string `json:"will_301"`  // URL is covered by a redirect rule (exact or regex)
	Will410   []string `json:"will_410"`  // URL is explicitly marked gone
	Will404   []string `json:"will_404"`  // unmapped - SEO leak risk
	Malformed []string `json:"malformed"` // input we couldn't parse
}

// Verify takes a list of source-site URLs (from a Search Console export, the
// site's sitemap, or a crawl) and reports what each one will resolve to on
// the new atomicsite domain. The output drives the "block deploy if will_404
// is non-empty" check in the migration apply flow (Layer 5).
func (h *RedirectHandler) Verify(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	var req VerifyRequest
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	if len(req.URLs) == 0 {
		writeError(w, http.StatusBadRequest, "urls array is required")
		return
	}
	if len(req.URLs) > 100_000 {
		writeError(w, http.StatusRequestEntityTooLarge, "Verify capped at 100,000 URLs per request")
		return
	}

	// Pull the data we'll match against once.
	pages, _ := h.queries.ListPagesBySite(r.Context(), siteID)
	pageSlugs := make(map[string]bool, len(pages))
	for _, p := range pages {
		pageSlugs[normalizeVerifyPath(p.Slug)] = true
	}
	redirects, _ := h.queries.ListRedirectsBySite(r.Context(), siteID)
	exact := make(map[string]store.Redirect, len(redirects))
	regexes := make([]compiledRegexRedirect, 0, 8)
	for _, rr := range redirects {
		if strings.HasPrefix(rr.FromPath, "~") {
			pat := strings.TrimSpace(strings.TrimPrefix(rr.FromPath, "~"))
			if re, err := regexp.Compile(pat); err == nil {
				regexes = append(regexes, compiledRegexRedirect{re: re, target: rr})
			}
			continue
		}
		exact[normalizeVerifyPath(rr.FromPath)] = rr
	}

	resp := VerifyResponse{
		Will200:   []string{},
		Will301:   []string{},
		Will410:   []string{},
		Will404:   []string{},
		Malformed: []string{},
	}
	for _, raw := range req.URLs {
		path, ok := extractURLPath(raw)
		if !ok {
			resp.Malformed = append(resp.Malformed, raw)
			continue
		}
		key := normalizeVerifyPath(path)
		if pageSlugs[key] {
			resp.Will200 = append(resp.Will200, raw)
			continue
		}
		if rr, ok := exact[key]; ok {
			if rr.StatusCode == 410 {
				resp.Will410 = append(resp.Will410, raw)
			} else {
				resp.Will301 = append(resp.Will301, raw)
			}
			continue
		}
		matched := false
		for _, rx := range regexes {
			if rx.re.MatchString(path) {
				if rx.target.StatusCode == 410 {
					resp.Will410 = append(resp.Will410, raw)
				} else {
					resp.Will301 = append(resp.Will301, raw)
				}
				matched = true
				break
			}
		}
		if !matched {
			resp.Will404 = append(resp.Will404, raw)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

type compiledRegexRedirect struct {
	re     *regexp.Regexp
	target store.Redirect
}

// extractURLPath pulls the path out of any input the operator might paste:
// `https://old-site.com/about`, `/about`, `old-site.com/about?ref=foo`, etc.
// Returns false for empty / clearly-not-a-URL input.
func extractURLPath(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}
	if strings.HasPrefix(s, "/") {
		// Already a path. Strip query string & fragment.
		if i := strings.IndexAny(s, "?#"); i >= 0 {
			s = s[:i]
		}
		return s, true
	}
	// Try to parse as URL. url.Parse is lenient enough to accept
	// `example.com/about` too, but we need a path component for that
	// to be useful.
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Path == "" {
		return "", false
	}
	return u.Path, true
}

// normalizeVerifyPath strips trailing slashes (except root) so `/about` and
// `/about/` collapse to the same key. Page slugs and redirect from_paths get
// the same treatment so lookups match regardless of the operator's habits.
func normalizeVerifyPath(p string) string {
	p = strings.TrimSpace(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if p == "/" {
		return p
	}
	return strings.TrimRight(p, "/")
}

// ----- validation -----

// validateRedirectInput is the single source of truth for what humans (and the
// agent) can write into the redirects table. The builder package re-validates
// at emit time as defense-in-depth, but every error here is a UX win - we
// surface the actual rule that was broken instead of silently dropping rows.
//
// excludeFromPath is the from_path of the row being updated (skip the
// "duplicate" check against itself). Empty for create.
func (h *RedirectHandler) validateRedirectInput(ctx context.Context, siteID string, in redirectInput, excludeFromPath string) error {
	from := strings.TrimSpace(in.FromPath)
	to := strings.TrimSpace(in.ToPath)

	if from == "" {
		return errors.New("from_path is required")
	}
	if to == "" {
		return errors.New("to_path is required")
	}
	if len(from) > 2048 || len(to) > 2048 {
		return errors.New("paths capped at 2048 characters")
	}
	if !validRedirectStatusCodes[in.StatusCode] {
		return fmt.Errorf("status_code must be one of 301, 302, 307, 308, 410 (got %d)", in.StatusCode)
	}

	// from_path: regex form vs exact form.
	if strings.HasPrefix(from, "~") {
		pat := strings.TrimSpace(strings.TrimPrefix(from, "~"))
		if pat == "" {
			return errors.New("regex from_path requires a non-empty pattern after ~")
		}
		if _, err := regexp.Compile(pat); err != nil {
			return fmt.Errorf("regex from_path does not compile: %v", err)
		}
	} else {
		if !strings.HasPrefix(from, "/") {
			return errors.New("from_path must start with / (or ~ for regex)")
		}
		if strings.ContainsAny(from, "?#") {
			return errors.New("from_path must be path-only (no query string or fragment)")
		}
	}
	if !pathTokenSafeForHandler(from) {
		return errors.New("from_path contains forbidden characters ({, }, ;, quotes, control chars)")
	}

	// to_path: path-only in this layer; off-site redirects belong in a later
	// flag we haven't shipped. Reject protocol-relative `//` (open-redirect
	// vector) and protocol-bearing strings.
	if !strings.HasPrefix(to, "/") {
		return errors.New("to_path must start with / (off-site redirects not yet supported)")
	}
	if strings.HasPrefix(to, "//") {
		return errors.New("to_path cannot start with // (open-redirect vector)")
	}
	if !pathTokenSafeForHandler(to) {
		return errors.New("to_path contains forbidden characters ({, }, ;, quotes, control chars)")
	}

	if from == to {
		return errors.New("from_path and to_path are identical (no-op redirect)")
	}

	// Page-slug collision: if a live page is already serving this path,
	// the redirect is dead weight (the page would still serve from the
	// static dist/, never the redirect). Refuse and surface the conflict
	// so the operator can rename one or the other.
	if !strings.HasPrefix(from, "~") && from != excludeFromPath {
		// Pages store slug WITHOUT a leading slash (e.g. "about-us"),
		// so strip the / before lookup.
		slug := strings.TrimPrefix(from, "/")
		if page, err := h.queries.GetPageBySiteAndSlug(ctx, store.GetPageBySiteAndSlugParams{
			SiteID: siteID, Slug: slug,
		}); err == nil && page.ID != "" {
			return fmt.Errorf("page %q already exists at this path; rename the page or pick another from_path", page.Slug)
		}
	}

	return nil
}

// pathTokenSafeForHandler mirrors builder.nginxTokenSafe but lives in handlers
// to avoid an import cycle. Same intent: reject anything that could break out
// of an `add_header` or `location { ... }` block in the rendered nginx.conf.
func pathTokenSafeForHandler(s string) bool {
	for _, r := range s {
		if r == '{' || r == '}' || r == ';' || r == '"' || r == '\\' || r == '\'' {
			return false
		}
		if r < 0x20 || r == 0x7f {
			return false
		}
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// redirectToMap is the canonical wire shape for a redirect row.
func redirectToMap(r store.Redirect) map[string]any {
	return map[string]any{
		"id":          r.ID,
		"site_id":     r.SiteID,
		"from_path":   r.FromPath,
		"to_path":     r.ToPath,
		"status_code": r.StatusCode,
		"is_auto":     r.IsAuto == 1,
		"created_at":  r.CreatedAt,
	}
}

// checkRedirectsQuotaForImport enforces the workspace plan's
// max_redirects_per_site cap before bulk import inserts new rows.
// Returns (0, "") on pass; (status, message) on refuse. OSS / unlimited
// tiers return -1 from the EE provider so they bypass the gate and
// fall through to the global redirectMaxPerSite ceiling. Auth required.
func (h *RedirectHandler) checkRedirectsQuotaForImport(r *http.Request, siteID string, current, projected int64) (int, string) {
	workspaceID, err := resolveUserWorkspace(r, h.queries)
	if err != nil {
		return http.StatusUnauthorized, err.Error()
	}
	limit := planLimitForRequest(r.Context(), h.queries, workspaceID, "max_redirects_per_site")
	if limit < 0 {
		return 0, ""
	}
	if current+projected > limit {
		AuditLog(r.Context(), h.queries, r, siteID, AuditActionUpdate, "redirect_import_blocked", "", map[string]any{
			"reason":       "max_redirects_per_site",
			"current":      current,
			"projected":    projected,
			"limit":        limit,
			"workspace_id": workspaceID,
		})
		return http.StatusPaymentRequired, quotaRedirectsError(current, projected, limit)
	}
	return 0, ""
}
