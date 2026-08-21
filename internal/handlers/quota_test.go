package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	_ "modernc.org/sqlite"

	"github.com/bright-interaction/slab/internal/store"
)

// newQuotaHandlerForTest spins up an in-memory SQLite DB with the
// canonical schema and a single seed site whose quota defaults to
// 1 GiB / 600 minutes from the schema. Returns the handler + queries
// + the seeded site_id (24-char hex so isSafeSiteID accepts it).
func newQuotaHandlerForTest(t *testing.T) (*QuotaHandler, *store.Queries, string) {
	t.Helper()
	sqlDB, _ := newSchemaDB(t, "quota.db")
	q := store.New(sqlDB)
	siteID := "abcdef0123456789abcdef01"
	if err := q.CreateSite(context.Background(), store.CreateSiteParams{
		ID: siteID, Name: "T", Slug: "t", Domain: "", PrimaryColor: "#000",
		SecondaryColor: "#000", BgColor: "#FFF", TextColor: "#111",
		FontHeading: "Inter", FontBody: "Inter", Lang: "en",
	}); err != nil {
		t.Fatalf("create site: %v", err)
	}
	return NewQuotaHandler(q), q, siteID
}

func withChiSiteID(siteID string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("siteID", siteID)
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
		h(w, r)
	}
}

func seedMediaRow(t *testing.T, q *store.Queries, siteID, id string, bytes int64) {
	t.Helper()
	if err := q.CreateMedia(context.Background(), store.CreateMediaParams{
		ID: id, SiteID: siteID, Filename: "f.jpg", AltText: "",
		MimeType: "image/jpeg", FileSize: bytes, Width: 1, Height: 1,
		OriginalPath: "/dev/null", Folder: "",
	}); err != nil {
		t.Fatalf("create media: %v", err)
	}
}

func seedCompletedDeployment(t *testing.T, q *store.Queries, siteID, id string, durationMs int64) {
	t.Helper()
	if err := q.CreateDeployment(context.Background(), store.CreateDeploymentParams{
		ID: id, SiteID: siteID, DeployTarget: "local", DeployConfig: "{}",
	}); err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	if err := q.UpdateDeploymentStatus(context.Background(), store.UpdateDeploymentStatusParams{
		ID: id, Status: "success", BuildLog: "", PagesBuilt: 1,
		DurationMs: durationMs, Error: "",
	}); err != nil {
		t.Fatalf("update deployment status: %v", err)
	}
}

func TestCheckStorageQuota_DefaultsAllowSmallUpload(t *testing.T) {
	h, q, siteID := newQuotaHandlerForTest(t)
	seedMediaRow(t, q, siteID, "m1", 1024)
	check, err := h.CheckStorageQuota(context.Background(), siteID, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if !check.Allowed {
		t.Errorf("expected small upload to be allowed under 1 GiB default, got Used=%d Limit=%d", check.Used, check.Limit)
	}
	if check.Used != 2048 {
		t.Errorf("expected Used=2048 (existing 1024 + additional 1024), got %d", check.Used)
	}
}

func TestCheckStorageQuota_BlocksWhenQuotaExceeded(t *testing.T) {
	h, q, siteID := newQuotaHandlerForTest(t)
	if err := q.UpdateSiteQuota(context.Background(), store.UpdateSiteQuotaParams{
		StorageQuotaBytes: 100, BuildMinutesQuota: 600, QuotaOverageBlocked: 1, ID: siteID,
	}); err != nil {
		t.Fatal(err)
	}
	seedMediaRow(t, q, siteID, "m1", 50)
	check, err := h.CheckStorageQuota(context.Background(), siteID, 60)
	if err != nil {
		t.Fatal(err)
	}
	if check.Allowed {
		t.Error("expected over-quota upload to be denied")
	}
	if !check.Blocked {
		t.Error("expected blocked=true when quota_overage_blocked=1")
	}
}

func TestCheckStorageQuota_OverageWarnOnlyWhenBlockedFalse(t *testing.T) {
	h, q, siteID := newQuotaHandlerForTest(t)
	if err := q.UpdateSiteQuota(context.Background(), store.UpdateSiteQuotaParams{
		StorageQuotaBytes: 100, BuildMinutesQuota: 600, QuotaOverageBlocked: 0, ID: siteID,
	}); err != nil {
		t.Fatal(err)
	}
	seedMediaRow(t, q, siteID, "m1", 200)
	check, err := h.CheckStorageQuota(context.Background(), siteID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if check.Allowed {
		t.Error("expected over-quota check to report Allowed=false")
	}
	if check.Blocked {
		t.Error("expected blocked=false when quota_overage_blocked=0 (warn-only)")
	}
}

func TestCheckBuildMinutesQuota_BlocksAfterQuotaConsumed(t *testing.T) {
	h, q, siteID := newQuotaHandlerForTest(t)
	if err := q.UpdateSiteQuota(context.Background(), store.UpdateSiteQuotaParams{
		StorageQuotaBytes: 1073741824, BuildMinutesQuota: 5, QuotaOverageBlocked: 1, ID: siteID,
	}); err != nil {
		t.Fatal(err)
	}
	// Seed 6 minutes of build duration: 6*60s*1000ms = 360000ms.
	seedCompletedDeployment(t, q, siteID, "d1", 360000)
	check, err := h.CheckBuildMinutesQuota(context.Background(), siteID)
	if err != nil {
		t.Fatal(err)
	}
	if check.Allowed {
		t.Errorf("expected over-quota build-minutes check to deny, got Used=%d Limit=%d", check.Used, check.Limit)
	}
	if check.Used < 5 {
		t.Errorf("expected Used >= 5 minutes from 360000ms seed, got %d", check.Used)
	}
}

func TestEnforceStorageQuota_429WhenBlocked(t *testing.T) {
	h, q, siteID := newQuotaHandlerForTest(t)
	if err := q.UpdateSiteQuota(context.Background(), store.UpdateSiteQuotaParams{
		StorageQuotaBytes: 10, BuildMinutesQuota: 600, QuotaOverageBlocked: 1, ID: siteID,
	}); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/upload", nil)
	short := h.EnforceStorageQuota(rr, req, siteID, 100)
	if !short {
		t.Fatal("expected EnforceStorageQuota to short-circuit on overage with blocked=true")
	}
	if rr.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", rr.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["kind"] != "storage" {
		t.Errorf("expected kind=storage, got %v", body["kind"])
	}
}

func TestEnforceStorageQuota_WarnHeaderWhenNotBlocked(t *testing.T) {
	h, q, siteID := newQuotaHandlerForTest(t)
	if err := q.UpdateSiteQuota(context.Background(), store.UpdateSiteQuotaParams{
		StorageQuotaBytes: 10, BuildMinutesQuota: 600, QuotaOverageBlocked: 0, ID: siteID,
	}); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/upload", nil)
	short := h.EnforceStorageQuota(rr, req, siteID, 100)
	if short {
		t.Error("expected EnforceStorageQuota to NOT short-circuit when warn-only")
	}
	if got := rr.Header().Get("X-Quota-Warning"); got != "storage" {
		t.Errorf("expected X-Quota-Warning=storage, got %q", got)
	}
}

func TestGetQuota_DefaultsForFreshSite(t *testing.T) {
	h, _, siteID := newQuotaHandlerForTest(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/sites/"+siteID+"/quota", nil)
	withChiSiteID(siteID, h.GetQuota)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	storage, _ := body["storage"].(map[string]any)
	if storage == nil || storage["quota_bytes"].(float64) != 1073741824 {
		t.Errorf("expected default 1 GiB quota, got %v", body["storage"])
	}
	mins, _ := body["build_minutes"].(map[string]any)
	if mins == nil || mins["quota"].(float64) != 600 {
		t.Errorf("expected default 600 build minutes, got %v", body["build_minutes"])
	}
}

func TestPatchAdminQuota_UpdatesAndAuditsRow(t *testing.T) {
	h, q, siteID := newQuotaHandlerForTest(t)
	body := map[string]any{
		"storage_quota_bytes":   2147483648, // 2 GiB
		"build_minutes_quota":   1200,
		"quota_overage_blocked": false,
	}
	b, _ := json.Marshal(body)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/admin/sites/"+siteID+"/quota", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	withChiSiteID(siteID, h.PatchAdminQuota)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	got, err := q.GetSiteQuota(context.Background(), siteID)
	if err != nil {
		t.Fatal(err)
	}
	if got.StorageQuotaBytes != 2147483648 {
		t.Errorf("storage_quota_bytes = %d, want 2147483648", got.StorageQuotaBytes)
	}
	if got.BuildMinutesQuota != 1200 {
		t.Errorf("build_minutes_quota = %d, want 1200", got.BuildMinutesQuota)
	}
	if got.QuotaOverageBlocked != 0 {
		t.Errorf("quota_overage_blocked = %d, want 0", got.QuotaOverageBlocked)
	}
}

func TestAdminQuotaSummary_IncludesPerSiteUsage(t *testing.T) {
	h, q, siteID := newQuotaHandlerForTest(t)
	seedMediaRow(t, q, siteID, "m1", 500)
	seedCompletedDeployment(t, q, siteID, "d1", 120000) // 2 min
	rows, err := h.AdminQuotaSummary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 site row, got %d", len(rows))
	}
	r := rows[0]
	if r["site_id"] != siteID {
		t.Errorf("site_id = %v, want %s", r["site_id"], siteID)
	}
	if r["storage_used_bytes"].(int64) != 500 {
		t.Errorf("storage_used_bytes = %v, want 500", r["storage_used_bytes"])
	}
	if r["build_minutes_used"].(int64) != 2 {
		t.Errorf("build_minutes_used = %v, want 2", r["build_minutes_used"])
	}
}
