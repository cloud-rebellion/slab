package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	_ "modernc.org/sqlite"

	"github.com/bright-interaction/slab/internal/config"
	dbpkg "github.com/bright-interaction/slab/internal/db"
	"github.com/bright-interaction/slab/internal/identify"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"
)

type capturingMail struct {
	mu    sync.Mutex
	calls []capturedFormCall
}
type capturedFormCall struct {
	To, SiteName, FormName string
	Fields                 map[string]string
}

func (c *capturingMail) SendPasswordResetLink(_ context.Context, _, _ string) error { return nil }
func (c *capturingMail) SendFormNotification(_ context.Context, to, siteName, formName string, fields map[string]string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make(map[string]string, len(fields))
	for k, v := range fields {
		cp[k] = v
	}
	c.calls = append(c.calls, capturedFormCall{To: to, SiteName: siteName, FormName: formName, Fields: cp})
	return nil
}

func newFormHandlerForTest(t *testing.T, mail MailSender) (*FormHandler, *store.Queries, string, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "forms.db")
	sqlDB, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	if _, err := sqlDB.Exec(string(dbpkg.Schema)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	q := store.New(sqlDB)
	siteID := "abcdef0123456789abcdef01"
	if err := q.CreateSite(context.Background(), store.CreateSiteParams{
		ID: siteID, Name: "T", Slug: "t", PrimaryColor: "#000",
		SecondaryColor: "#000", BgColor: "#FFF", TextColor: "#111",
		FontHeading: "Inter", FontBody: "Inter", Lang: "en",
	}); err != nil {
		t.Fatalf("create site: %v", err)
	}
	formID := "form_001"
	if err := q.CreateForm(context.Background(), store.CreateFormParams{
		ID: formID, SiteID: siteID, Name: "Contact",
		FieldsJson:   `[{"name":"email","type":"email","required":true},{"name":"message","type":"textarea"}]`,
		Action:       "store",
		ActionConfig: `{"notify_email":"owner@example.com"}`,
	}); err != nil {
		t.Fatalf("create form: %v", err)
	}
	cfg := &config.Config{BaseURL: "http://localhost:8080", AnalyticsSalt: "test-salt"}
	return NewFormHandler(cfg, q, mail, nil), q, siteID, formID
}

func withChiFormParams(siteID, formID string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("siteID", siteID)
		rctx.URLParams.Add("formID", formID)
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
		h(w, r)
	}
}

func postFormSubmit(handler http.HandlerFunc, formID, ip, ct, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/t/forms/"+formID+"/submit", strings.NewReader(body))
	req.Header.Set("Content-Type", ct)
	req.RemoteAddr = ip + ":54321"
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("formID", formID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()
	handler(rr, req)
	return rr
}

func TestFormSubmit_HappyPath(t *testing.T) {
	h, q, siteID, formID := newFormHandlerForTest(t, nil)
	body := url.Values{"email": {"a@b.com"}, "message": {"hi"}}.Encode()
	rr := postFormSubmit(h.Submit, formID, "203.0.113.4", "application/x-www-form-urlencoded", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	subs, err := q.ListFormSubmissionsBySite(context.Background(), store.ListFormSubmissionsBySiteParams{SiteID: siteID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 row, got %d", len(subs))
	}
	if !strings.Contains(subs[0].DataJson, "a@b.com") {
		t.Errorf("data_json missing email: %s", subs[0].DataJson)
	}
	if subs[0].IpHash == "" {
		t.Error("expected ip_hash to be populated")
	}
}

func TestFormSubmit_HoneypotDropsSilently(t *testing.T) {
	h, q, _, formID := newFormHandlerForTest(t, nil)
	body := url.Values{"email": {"a@b.com"}, "website": {"http://spam.example"}}.Encode()
	rr := postFormSubmit(h.Submit, formID, "203.0.113.5", "application/x-www-form-urlencoded", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected silent 200, got %d", rr.Code)
	}
	subs, _ := q.ListFormSubmissions(context.Background(), formID)
	if len(subs) != 0 {
		t.Errorf("expected no row written when honeypot triggered, got %d", len(subs))
	}
}

func TestFormSubmit_UnknownFormReturns404(t *testing.T) {
	h, _, _, _ := newFormHandlerForTest(t, nil)
	body := url.Values{"email": {"a@b.com"}}.Encode()
	rr := postFormSubmit(h.Submit, "does-not-exist", "203.0.113.6", "application/x-www-form-urlencoded", body)
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}
}

func TestFormSubmit_BurstWithin3SecondsReturns429(t *testing.T) {
	h, _, _, formID := newFormHandlerForTest(t, nil)
	ip := "203.0.113.7"
	body := url.Values{"email": {"a@b.com"}}.Encode()
	first := postFormSubmit(h.Submit, formID, ip, "application/x-www-form-urlencoded", body)
	if first.Code != http.StatusOK {
		t.Fatalf("first should succeed, got %d", first.Code)
	}
	second := postFormSubmit(h.Submit, formID, ip, "application/x-www-form-urlencoded", body)
	if second.Code != http.StatusTooManyRequests {
		t.Errorf("second within burst window should 429, got %d", second.Code)
	}
}

func TestFormSubmit_PerIPRateLimit(t *testing.T) {
	h, _, _, formID := newFormHandlerForTest(t, nil)
	ip := "203.0.113.8"
	body := url.Values{"email": {"a@b.com"}}.Encode()
	// Bypass burst by varying formID? we only have one form. Wait
	// 5ms between attempts is too slow for the 3s window; instead
	// we shrink burst by simulating different forms via direct
	// limiter calls. Easier: just send to the burst tracker once
	// then poke the limiter directly to drive it past threshold.
	for i := 0; i < 11; i++ {
		h.limiter.recordFailure("f:" + ip)
	}
	// 12th call should be blocked.
	rr := postFormSubmit(h.Submit, formID, ip, "application/x-www-form-urlencoded", body)
	if rr.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 after threshold reached, got %d", rr.Code)
	}
}

func TestFormSubmit_CrossSitePreservesFormSiteID(t *testing.T) {
	h, q, siteID, formID := newFormHandlerForTest(t, nil)
	// Even if the body included a site_id, the handler must ignore
	// it and write the form's own site_id.
	body := url.Values{"email": {"a@b.com"}, "site_id": {"FAKE"}}.Encode()
	rr := postFormSubmit(h.Submit, formID, "203.0.113.9", "application/x-www-form-urlencoded", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	subs, _ := q.ListFormSubmissions(context.Background(), formID)
	if len(subs) != 1 {
		t.Fatalf("expected 1 row, got %d", len(subs))
	}
	if subs[0].SiteID != siteID {
		t.Errorf("site_id leaked from body, got %s want %s", subs[0].SiteID, siteID)
	}
}

func TestFormSubmit_NotificationEmailSentWhenConfigured(t *testing.T) {
	mail := &capturingMail{}
	h, _, _, formID := newFormHandlerForTest(t, mail)
	body := url.Values{"email": {"a@b.com"}, "message": {"hi there"}}.Encode()
	rr := postFormSubmit(h.Submit, formID, "203.0.113.10", "application/x-www-form-urlencoded", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	// notification is fire-and-forget; wait briefly.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mail.mu.Lock()
		n := len(mail.calls)
		mail.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mail.mu.Lock()
	defer mail.mu.Unlock()
	if len(mail.calls) != 1 {
		t.Fatalf("expected 1 notification call, got %d", len(mail.calls))
	}
	if mail.calls[0].To != "owner@example.com" {
		t.Errorf("notify_email mismatch: %q", mail.calls[0].To)
	}
	if mail.calls[0].Fields["email"] != "a@b.com" {
		t.Errorf("expected email=a@b.com in fields, got %v", mail.calls[0].Fields)
	}
}

func TestFormSubmit_OversizedFieldGetsTruncated(t *testing.T) {
	h, q, _, formID := newFormHandlerForTest(t, nil)
	huge := strings.Repeat("x", 8000)
	body := url.Values{"message": {huge}, "email": {"a@b.com"}}.Encode()
	rr := postFormSubmit(h.Submit, formID, "203.0.113.11", "application/x-www-form-urlencoded", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	subs, _ := q.ListFormSubmissions(context.Background(), formID)
	if len(subs) != 1 {
		t.Fatalf("expected 1 row, got %d", len(subs))
	}
	var data map[string]string
	if err := json.Unmarshal([]byte(subs[0].DataJson), &data); err != nil {
		t.Fatalf("unmarshal data_json: %v", err)
	}
	if len(data["message"]) != maxFieldValueBytes {
		t.Errorf("expected message truncated to %d, got %d", maxFieldValueBytes, len(data["message"]))
	}
}

func TestFormSubmit_AdminListAndDelete(t *testing.T) {
	h, q, siteID, formID := newFormHandlerForTest(t, nil)
	body := url.Values{"email": {"a@b.com"}}.Encode()
	if rr := postFormSubmit(h.Submit, formID, "203.0.113.12", "application/x-www-form-urlencoded", body); rr.Code != http.StatusOK {
		t.Fatalf("seed submit failed: %d", rr.Code)
	}
	// List
	listReq := httptest.NewRequest(http.MethodGet, "/api/sites/"+siteID+"/forms/"+formID+"/submissions", nil)
	listRR := httptest.NewRecorder()
	withChiFormParams(siteID, formID, h.ListAdminSubmissions)(listRR, listReq)
	if listRR.Code != http.StatusOK {
		t.Fatalf("list expected 200, got %d", listRR.Code)
	}
	var rows []store.FormSubmission
	if err := json.Unmarshal(listRR.Body.Bytes(), &rows); err != nil {
		t.Fatalf("list decode: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("list expected 1 row, got %d", len(rows))
	}
	subID := rows[0].ID
	// Delete
	delReq := httptest.NewRequest(http.MethodDelete, "/api/sites/"+siteID+"/forms/"+formID+"/submissions/"+subID, nil)
	delRR := httptest.NewRecorder()
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("siteID", siteID)
	rctx.URLParams.Add("formID", formID)
	rctx.URLParams.Add("submissionID", subID)
	delReq = delReq.WithContext(context.WithValue(delReq.Context(), chi.RouteCtxKey, rctx))
	h.DeleteAdminSubmission(delRR, delReq)
	if delRR.Code != http.StatusOK {
		t.Fatalf("delete expected 200, got %d", delRR.Code)
	}
	final, _ := q.ListFormSubmissions(context.Background(), formID)
	if len(final) != 0 {
		t.Errorf("expected 0 rows after delete, got %d", len(final))
	}
}

// TestFormSubmit_AutoIdentifiesVisitor (Phase 31.3, 2026-05-06)
// Locks the auto-identify path: when a form carries an email-shaped
// value AND a visit_session exists for the visitor's fingerprint,
// FormHandler.Submit pulls the email out of the submitted fields and
// stamps it onto the session via the shared identify.Recorder.
func TestFormSubmit_AutoIdentifiesVisitor(t *testing.T) {
	// Standalone setup: we need the FormHandler to carry an
	// identify.Recorder, which the shared newFormHandlerForTest
	// constructs with nil. So we do the open-DB + create-site +
	// create-form + construct-handler dance inline with the
	// recorder wired.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "forms.db")
	sqlDB, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	if _, err := sqlDB.Exec(string(dbpkg.Schema)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	q := store.New(sqlDB)
	siteID := "abcdef0123456789abcdef02"
	if err := q.CreateSite(context.Background(), store.CreateSiteParams{
		ID: siteID, Name: "T", Slug: "t-id", PrimaryColor: "#000",
		SecondaryColor: "#000", BgColor: "#FFF", TextColor: "#111",
		FontHeading: "Inter", FontBody: "Inter", Lang: "en",
	}); err != nil {
		t.Fatalf("create site: %v", err)
	}
	formID := "form_id_auto"
	if err := q.CreateForm(context.Background(), store.CreateFormParams{
		ID: formID, SiteID: siteID, Name: "Contact",
		FieldsJson:   `[{"name":"email","type":"email"}]`,
		Action:       "store",
		ActionConfig: `{}`,
	}); err != nil {
		t.Fatalf("create form: %v", err)
	}

	// Compute the deterministic fingerprint the request will carry,
	// then seed a visit_session for it.
	salt := "test-salt"
	ip := "203.0.113.99"
	ua := "Go-http-client/1.1"
	lang := ""
	fp := authmw.DeriveFingerprint(ip, ua, lang, salt)
	now := time.Now().UTC().Format(time.RFC3339)
	if err := q.UpsertVisitSession(context.Background(), store.UpsertVisitSessionParams{
		ID:          "sess_id_auto",
		SiteID:      siteID,
		Fingerprint: fp,
		StartedAt:   now,
		LastSeenAt:  now,
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	cfg := &config.Config{BaseURL: "http://localhost:8080", AnalyticsSalt: salt}
	idRec := identify.NewRecorder(q, nil) // nil crmClient = best-effort no-op
	h := NewFormHandler(cfg, q, nil, idRec)

	// Build a request with the matching headers + a manually-set
	// fingerprint cookie so the FingerprintMiddleware-derived value
	// matches our seed. The simpler path: bypass the middleware and
	// set the fingerprint in context directly.
	body := url.Values{"email": {"identified@example.com"}, "message": {"hello"}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/t/forms/"+formID+"/submit", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", ua)
	req.RemoteAddr = ip + ":54321"
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("formID", formID)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	// Stash the fingerprint as the FingerprintMiddleware would.
	ctx = authmw.WithFingerprintForTest(ctx, fp)
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	h.Submit(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("submit: expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	// The visit_session should now have the form's email.
	sess, err := q.GetSessionByFingerprint(context.Background(), store.GetSessionByFingerprintParams{
		SiteID: siteID, Fingerprint: fp,
	})
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.Email != "identified@example.com" {
		t.Errorf("session email = %q, want identified@example.com", sess.Email)
	}
	if sess.IdentifiedAt == "" {
		t.Errorf("identified_at should be stamped")
	}
}
