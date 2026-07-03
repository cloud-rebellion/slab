package handlers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/bright-interaction/slab/internal/config"
	"github.com/bright-interaction/slab/internal/conversions"
	"github.com/bright-interaction/slab/internal/identify"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"
)

// FormHandler serves the public form-submission endpoint and the
// admin CSV export + per-row delete endpoints. The submission flow:
//
//  1. Look up the form by ID (denies fast for unknown form IDs).
//  2. Honeypot check: a bot that fills the hidden "website" field
//     gets a silent 200 with no DB write. Real users never see it.
//  3. Per-IP rate limit (10/hour) via formRateLimiter; reuses the
//     loginRateLimiter token-bucket pattern.
//  4. Per-form burst limit: same form + same IP twice within 3s
//     returns 429.
//  5. Parse + cap field values at 5KB to defang giant payloads.
//  6. Persist with site_id derived from the form row (NOT the
//     client). Cross-site spoofing is impossible at this layer.
//  7. If the form has action_config.notify_email and a MailSender is
//     configured, fire-and-forget a notification email.
type FormHandler struct {
	cfg        *config.Config
	queries    *store.Queries
	mail       MailSender
	limiter    *loginRateLimiter
	burst      *formBurstTracker
	recorder   *conversions.Recorder
	idRecorder *identify.Recorder
}

func NewFormHandler(cfg *config.Config, queries *store.Queries, mail MailSender, idRecorder *identify.Recorder) *FormHandler {
	return &FormHandler{
		cfg:     cfg,
		queries: queries,
		mail:    mail,
		// 10 submissions per IP per rolling hour. Permissive enough
		// for the legitimate "filled it twice because the network
		// blipped" case, restrictive enough to make spam costly.
		limiter:    newLoginRateLimiter(10, time.Hour, 2*time.Hour),
		burst:      newFormBurstTracker(),
		recorder:   conversions.NewRecorder(queries),
		idRecorder: idRecorder,
	}
}

// formBurstTracker rejects a second submission to the same form from
// the same IP within burstWindow. The state is process-local; an
// attacker that rotates source IPs evades it but the per-IP rate
// limiter catches the single-IP burst case.
type formBurstTracker struct {
	mu   sync.Mutex
	last map[string]time.Time
}

const burstWindow = 3 * time.Second

func newFormBurstTracker() *formBurstTracker {
	return &formBurstTracker{last: make(map[string]time.Time)}
}

func (b *formBurstTracker) allow(formID, ip string) bool {
	if formID == "" {
		return true
	}
	key := formID + "|" + ip
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if last, ok := b.last[key]; ok && now.Sub(last) < burstWindow {
		return false
	}
	b.last[key] = now
	// Opportunistic GC: drop entries older than 10x the window so
	// the map doesn't grow unbounded for high-traffic sites.
	if len(b.last) > 1024 {
		cutoff := now.Add(-10 * burstWindow)
		for k, t := range b.last {
			if t.Before(cutoff) {
				delete(b.last, k)
			}
		}
	}
	return true
}

// formActionConfig is the optional JSON shape stored on
// forms.action_config. Empty / missing keys are fine; only
// notify_email influences runtime behavior today.
type formActionConfig struct {
	NotifyEmail string `json:"notify_email"`
	RedirectURL string `json:"redirect_url"`
	SuccessMsg  string `json:"success_message"`
}

const (
	maxFieldValueBytes = 5000
	honeypotFieldName  = "website"
)

// Submit handles POST /t/forms/{formID}/submit from the built site.
// Accepts both application/json and application/x-www-form-urlencoded
// so the no-JS POST fallback works.
//
// CORS is handled by the existing built-site allowance in
// server.go's isAllowedOriginForPath; this handler does not need to
// echo CORS headers itself.
func (h *FormHandler) Submit(w http.ResponseWriter, r *http.Request) {
	formID := chi.URLParam(r, "formID")
	if formID == "" {
		writeError(w, http.StatusBadRequest, "Missing form id")
		return
	}
	form, err := h.queries.GetFormByID(r.Context(), formID)
	if err != nil {
		writeError(w, http.StatusNotFound, "Form not found")
		return
	}

	fields, honeypotSet, err := parseSubmissionBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	// Honeypot first, before any rate-limit recording. Bots get a
	// silent 200; we don't tell them they were caught.
	if honeypotSet {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	ip := clientIP(r)
	if allowed, retry := h.limiter.allow("f:" + ip); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		writeError(w, http.StatusTooManyRequests, "Too many submissions; try again later")
		return
	}
	if !h.burst.allow(formID, ip) {
		w.Header().Set("Retry-After", "3")
		writeError(w, http.StatusTooManyRequests, "Slow down")
		return
	}

	// Sanitize: cap each field value to maxFieldValueBytes. This
	// prevents an attacker from posting megabytes of text and
	// blowing up data_json, the email body, or the CSV export.
	for k, v := range fields {
		if len(v) > maxFieldValueBytes {
			fields[k] = v[:maxFieldValueBytes]
		}
		// Strip the honeypot from stored values (already handled
		// above, but defensive).
		if k == honeypotFieldName {
			delete(fields, k)
		}
	}
	dataJSON, err := json.Marshal(fields)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to encode submission")
		return
	}

	subID := newID()
	ipHash := hashIPWithSalt(ip, h.cfg.AnalyticsSalt, form.SiteID)
	if err := h.queries.CreateFormSubmission(r.Context(), store.CreateFormSubmissionParams{
		ID:       subID,
		FormID:   form.ID,
		SiteID:   form.SiteID,
		DataJson: string(dataJSON),
		IpHash:   ipHash,
	}); err != nil {
		slog.Error("form submission write failed", "form_id", form.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "Failed to save submission")
		return
	}

	// Reset the per-IP counter on success so a legitimate user that
	// submits two forms in a row doesn't hit the cap.
	h.limiter.recordSuccess("f:" + ip)

	fp := authmw.GetFingerprint(r)

	// Conversion goal evaluation (Phase 31.1). form_submit goals
	// fire here so a successful submission produces both a
	// form_submissions row and any matching conversion_event rows.
	// Best-effort: a failed eval never fails the submit.
	if h.recorder != nil {
		h.recorder.Evaluate(r.Context(), conversions.Signal{
			SiteID:      form.SiteID,
			Fingerprint: fp,
			Path:        r.Header.Get("Referer"),
			MatchType:   conversions.MatchTypeFormSubmit,
			MatchValue:  form.ID,
			Properties:  map[string]any{"submission_id": subID},
		})
	}

	// Visitor identification (Phase 31.3, 2026-05-06). When the form
	// carries an email-shaped value, link it to the visitor's
	// session so atomicsite's identified-sessions panel + per-email
	// drill-down work without a separate /t/identify ping. Best-
	// effort: a missing fingerprint or a no-session-for-this-fp
	// returns 0 rows and we move on; the form submission itself is
	// already persisted in form_submissions and is the source of
	// truth for the email regardless.
	if h.idRecorder != nil && fp != "" {
		if email := identify.PickEmail(fields); email != "" {
			_, _ = h.idRecorder.Identify(r.Context(), form.SiteID, fp, email, identify.Page{
				URL:      r.Header.Get("Referer"),
				Path:     r.Header.Get("Referer"),
				Referrer: r.Header.Get("Referer"),
			})
		}
	}

	// Optional notification email (fire-and-forget, errors logged).
	cfg := parseFormActionConfig(form.ActionConfig)
	if cfg.NotifyEmail != "" {
		if h.mail != nil {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := h.mail.SendFormNotification(ctx, cfg.NotifyEmail, form.SiteID, form.Name, fields); err != nil {
					slog.Warn("form notification email failed", "form_id", form.ID, "err", err)
				}
			}()
		} else {
			slog.Info("form submission received (no MailSender configured)",
				"form_id", form.ID,
				"site_id", form.SiteID,
				"notify_email", cfg.NotifyEmail,
			)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"redirect":        cfg.RedirectURL,
		"success_message": cfg.SuccessMsg,
	})
}

// parseSubmissionBody supports both JSON and URL-encoded form posts.
// Returns the parsed fields and a flag indicating whether the
// honeypot field was filled (drop signal for the caller).
func parseSubmissionBody(r *http.Request) (map[string]string, bool, error) {
	const maxBodyBytes = int64(64 * 1024)
	r.Body = http.MaxBytesReader(nil, r.Body, maxBodyBytes)
	ct := r.Header.Get("Content-Type")
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(strings.ToLower(ct))
	out := make(map[string]string)
	if ct == "application/json" {
		var raw map[string]any
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			return nil, false, err
		}
		for k, v := range raw {
			out[k] = stringifyValue(v)
		}
	} else {
		if err := r.ParseForm(); err != nil {
			return nil, false, err
		}
		for k, vals := range r.PostForm {
			if len(vals) == 0 {
				continue
			}
			out[k] = vals[0]
		}
	}
	honeypot := strings.TrimSpace(out[honeypotFieldName]) != ""
	return out, honeypot, nil
}

func stringifyValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func parseFormActionConfig(raw string) formActionConfig {
	var cfg formActionConfig
	if raw == "" {
		return cfg
	}
	_ = json.Unmarshal([]byte(raw), &cfg)
	return cfg
}

// ListAdminForms returns the forms defined for the site, gated by
// RequireSiteAccess. Each row includes a submissions count so the
// admin UI can show "Contact form (12 submissions)" without a second
// round-trip per row.
//
// GET /api/sites/{siteID}/forms
func (h *FormHandler) ListAdminForms(w http.ResponseWriter, r *http.Request) {
	siteID := chi.URLParam(r, "siteID")
	forms, err := h.queries.ListFormsBySite(r.Context(), siteID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load forms")
		return
	}
	out := make([]map[string]any, 0, len(forms))
	for _, f := range forms {
		subs, _ := h.queries.ListFormSubmissions(r.Context(), f.ID)
		out = append(out, map[string]any{
			"id":               f.ID,
			"name":             f.Name,
			"action":           f.Action,
			"action_config":    f.ActionConfig,
			"fields_json":      f.FieldsJson,
			"created_at":       f.CreatedAt,
			"updated_at":       f.UpdatedAt,
			"submission_count": len(subs),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// ListAdminSubmissions returns the submissions for one form, gated
// by RequireSiteAccess at the route level. Cross-site isolation is
// belt-and-suspenders: we re-check that the form's site_id matches
// the URL's site_id.
//
// GET /api/sites/{siteID}/forms/{formID}/submissions
func (h *FormHandler) ListAdminSubmissions(w http.ResponseWriter, r *http.Request) {
	siteID := chi.URLParam(r, "siteID")
	formID := chi.URLParam(r, "formID")
	form, err := h.queries.GetFormByID(r.Context(), formID)
	if err != nil || form.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Form not found")
		return
	}
	rows, err := h.queries.ListFormSubmissions(r.Context(), formID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load submissions")
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// ExportAdminSubmissionsCSV streams the submissions as CSV. Capped
// at MaxCSVExportRows to prevent memory blow-ups; reuse the limit
// from the S2 audit work.
//
// GET /api/sites/{siteID}/forms/{formID}/submissions.csv
func (h *FormHandler) ExportAdminSubmissionsCSV(w http.ResponseWriter, r *http.Request) {
	siteID := chi.URLParam(r, "siteID")
	formID := chi.URLParam(r, "formID")
	form, err := h.queries.GetFormByID(r.Context(), formID)
	if err != nil || form.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Form not found")
		return
	}
	rows, err := h.queries.ListFormSubmissions(r.Context(), formID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load submissions")
		return
	}
	if len(rows) > MaxCSVExportRows {
		writeError(w, http.StatusRequestEntityTooLarge, "Too many rows; narrow the window")
		return
	}
	// Collect every field key across submissions for a stable header.
	keys := map[string]struct{}{}
	parsed := make([]map[string]string, len(rows))
	for i, r := range rows {
		var m map[string]any
		_ = json.Unmarshal([]byte(r.DataJson), &m)
		flat := make(map[string]string, len(m))
		for k, v := range m {
			flat[k] = stringifyValue(v)
			keys[k] = struct{}{}
		}
		parsed[i] = flat
	}
	header := []string{"submission_id", "created_at", "ip_hash"}
	for k := range keys {
		header = append(header, k)
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=submissions-"+formID+".csv")
	writeCSVRow(w, header)
	for i, r := range rows {
		row := []string{r.ID, r.CreatedAt, r.IpHash}
		for _, k := range header[3:] {
			row = append(row, parsed[i][k])
		}
		writeCSVRow(w, row)
	}
}

func writeCSVRow(w http.ResponseWriter, row []string) {
	for i, c := range row {
		if i > 0 {
			_, _ = w.Write([]byte{','})
		}
		_, _ = w.Write([]byte(csvEscape(c)))
	}
	_, _ = w.Write([]byte("\r\n"))
}

func csvEscape(s string) string {
	if !strings.ContainsAny(s, `,"`+"\n\r") {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// DeleteAdminSubmission removes a single row. Site-access gated.
//
// DELETE /api/sites/{siteID}/forms/{formID}/submissions/{submissionID}
func (h *FormHandler) DeleteAdminSubmission(w http.ResponseWriter, r *http.Request) {
	siteID := chi.URLParam(r, "siteID")
	formID := chi.URLParam(r, "formID")
	subID := chi.URLParam(r, "submissionID")
	form, err := h.queries.GetFormByID(r.Context(), formID)
	if err != nil || form.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Form not found")
		return
	}
	if err := h.queries.DeleteFormSubmission(r.Context(), subID); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete submission")
		return
	}
	AuditLog(r.Context(), h.queries, r, siteID, AuditActionDelete, "form_submission", subID, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
