package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bright-interaction/slab/internal/store"
)

// Production opens one SQLite connection. A guardrail read through the pool
// while BulkSave owns that connection cannot finish before its client cancels.
// Exercise the real handler and database, including refusal and rollback paths.
type bulkTransactionFixture struct {
	db      *sql.DB
	queries *store.Queries
	handler *BlockHandler
	siteID  string
	pageID  string
	oldID   string
}

func newBulkTransactionFixture(t *testing.T) bulkTransactionFixture {
	t.Helper()
	db, q := setupDeployTestDB(t)
	if got := db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("fixture pool has %d connections; production requires one", got)
	}
	f := bulkTransactionFixture{db: db, queries: q, handler: NewBlockHandler(nil, q, db), siteID: "bulk-tx-site", pageID: "bulk-tx-page", oldID: "bulk-tx-original"}
	seedSite(t, q, f.siteID)
	ctx := context.Background()
	if err := q.CreatePage(ctx, store.CreatePageParams{ID: f.pageID, SiteID: f.siteID, Title: "Original", Slug: "original", Layout: "default"}); err != nil {
		t.Fatalf("seed page: %v", err)
	}
	if err := q.CreateBlock(ctx, store.CreateBlockParams{ID: f.oldID, PageID: f.pageID, BlockType: "text", Name: "Original", DataJson: `{"content":"before"}`, StyleJson: "{}", IsVisible: 1}); err != nil {
		t.Fatalf("seed block: %v", err)
	}
	return f
}

func (f bulkTransactionFixture) request(t *testing.T, blocks []map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req := jsonReq(http.MethodPut, map[string]any{"blocks": blocks}).WithContext(ctx)
	rr := httptest.NewRecorder()
	withParams(f.handler.BulkSave, "siteID", f.siteID, "pageID", f.pageID)(rr, req)
	if inUse := f.db.Stats().InUse; inUse != 0 {
		t.Fatalf("handler leaked %d database connections", inUse)
	}
	return rr
}

func bulkTransactionBlock(id, content string, order int) map[string]any {
	return map[string]any{"id": id, "block_type": "text", "data": map[string]any{"content": content}, "style": map[string]any{}, "sort_order": order, "is_visible": true}
}

func (f bulkTransactionFixture) assertOriginal(t *testing.T) {
	t.Helper()
	rows, err := f.queries.ListBlocksByPage(context.Background(), f.pageID)
	if err != nil {
		t.Fatalf("read after refusal: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != f.oldID || rows[0].DataJson != `{"content":"before"}` {
		t.Fatal("bulk refusal or insertion failure changed the original page blocks")
	}
}

func assertBulkTransactionRule(t *testing.T, rr *httptest.ResponseRecorder, want string) {
	t.Helper()
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d, want guardrail refusal 422", rr.Code)
	}
	var body struct {
		Violations []struct {
			Rule string `json:"rule"`
		} `json:"violations"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode violation response: %v", err)
	}
	for _, v := range body.Violations {
		if v.Rule == want {
			return
		}
	}
	t.Fatalf("expected guardrail rule %q was not reached", want)
}

func TestBulkSaveTransactionReplacesBlocksWithSingleConnection(t *testing.T) {
	f := newBulkTransactionFixture(t)
	rr := f.request(t, []map[string]any{bulkTransactionBlock("first", "first", 0), bulkTransactionBlock("second", "second", 1)})
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 without waiting for request cancellation", rr.Code)
	}
	rows, err := f.queries.ListBlocksByPage(context.Background(), f.pageID)
	if err != nil {
		t.Fatalf("read replaced page: %v", err)
	}
	if len(rows) != 2 || rows[0].ID != "first" || rows[1].ID != "second" {
		t.Fatal("bulk save did not replace the page with both requested blocks")
	}
}

func TestBulkSaveTransactionRejectsSecurityViolationBeforeReplacement(t *testing.T) {
	f := newBulkTransactionFixture(t)
	rr := f.request(t, []map[string]any{bulkTransactionBlock("first", "safe", 0), bulkTransactionBlock("second", "<script>unsafe</script>", 1)})
	assertBulkTransactionRule(t, rr, "security")
	f.assertOriginal(t)
}

func TestBulkSaveTransactionHonorsSiteGuardrailBeforeReplacement(t *testing.T) {
	f := newBulkTransactionFixture(t)
	if err := f.queries.CreateGuardrail(context.Background(), store.CreateGuardrailParams{ID: "bulk-site-rule", SiteID: f.siteID, RuleType: "forbid_pattern", Target: "*", Value: "forbidden-fixture", Severity: "error"}); err != nil {
		t.Fatalf("seed site guardrail: %v", err)
	}
	rr := f.request(t, []map[string]any{bulkTransactionBlock("first", "safe", 0), bulkTransactionBlock("second", "forbidden-fixture", 1)})
	assertBulkTransactionRule(t, rr, "forbid_pattern")
	f.assertOriginal(t)
}

func TestBulkSaveTransactionPreservesNilEngineFailClosed(t *testing.T) {
	f := newBulkTransactionFixture(t)
	f.handler.guardrails = nil
	rr := f.request(t, []map[string]any{bulkTransactionBlock("first", "safe", 0)})
	assertBulkTransactionRule(t, rr, "enforcement_unavailable")
	f.assertOriginal(t)
}

func TestBulkSaveTransactionRollsBackInsertionFailure(t *testing.T) {
	f := newBulkTransactionFixture(t)
	// The second insert violates the primary key after the old page was
	// deleted and the first insert succeeded. The deferred rollback must
	// recover the complete original page, not its partially replaced state.
	rr := f.request(t, []map[string]any{bulkTransactionBlock("duplicate", "first", 0), bulkTransactionBlock("duplicate", "second", 1)})
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want insertion failure 500", rr.Code)
	}
	f.assertOriginal(t)
}
