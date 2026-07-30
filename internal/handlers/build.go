package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bright-interaction/slab/internal/agent"
	"github.com/bright-interaction/slab/internal/builder"
	"github.com/bright-interaction/slab/internal/config"
	"github.com/bright-interaction/slab/internal/critique"
	"github.com/bright-interaction/slab/internal/deploy"
	"github.com/bright-interaction/slab/internal/eval"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/store"
)

// safeSiteIDPattern matches the format newID() produces (24-char hex). Blocks
// any path-traversal attempt via the URL param before we use it as a directory
// name in {DataDir}/workspaces/{siteID}/.
var safeSiteIDPattern = regexp.MustCompile(`^[a-fA-F0-9]{24}$`)

func isSafeSiteID(s string) bool { return safeSiteIDPattern.MatchString(s) }

// BuildHandler handles build trigger and status endpoints.
type BuildHandler struct {
	cfg     *config.Config
	queries *store.Queries
	// quota gates TriggerBuild + TriggerBuildAdmin against the
	// site's monthly build-minutes ceiling. Sprint 3 (2026-05-04).
	quota *QuotaHandler

	mu     sync.Mutex
	builds map[string]*buildState // deploymentID -> state
	// latestBySite tracks the newest tracked build per site so finalize
	// can evict the previous terminal state: without eviction every
	// build leaked a buildState (up to 1MB of log) for the process
	// lifetime. Keeping exactly the latest terminal state per site
	// preserves the Deploy handler's dist_dir lookup for the build an
	// operator would actually deploy.
	latestBySite map[string]string
	// cancels maps deployment_id -> context cancel func for the
	// in-flight goroutine. CancelBuild looks up here and calls
	// the func to interrupt bun / eval / deploy. Entries are
	// removed once the goroutine returns.
	cancels map[string]context.CancelFunc

	// siteMu guards per-site build serialization. The audit's C4 finding:
	// two simultaneous builds for the same site_id share the workspace
	// directory (`{DataDir}/workspaces/{siteID}/`); InitWorkspace removes
	// it unconditionally, so build A can wipe build B's files mid-write.
	// The siteLock map gives every active build its own mutex; only one
	// build per site_id runs at a time.
	siteMu   sync.Mutex
	siteLock map[string]*sync.Mutex

	// buildSem caps builds running concurrently across ALL sites. The per-site
	// lock only serializes same-site builds; without a global cap, N tenants
	// triggering at once spawn N bun/astro processes and OOM the host. Default
	// 3, override with ATOMICSITE_BUILD_CONCURRENCY.
	buildSem chan struct{}
}

type buildState struct {
	Status     string `json:"status"`
	BuildLog   string `json:"build_log"`
	PagesBuilt int    `json:"pages_built"`
	DurationMs int64  `json:"duration_ms"`
	Error      string `json:"error"`
	DistDir    string `json:"dist_dir"`
}

func NewBuildHandler(cfg *config.Config, queries *store.Queries, quota *QuotaHandler) *BuildHandler {
	maxConc := 3
	if v := strings.TrimSpace(os.Getenv("ATOMICSITE_BUILD_CONCURRENCY")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxConc = n
		}
	}
	return &BuildHandler{
		cfg:          cfg,
		queries:      queries,
		quota:        quota,
		builds:       make(map[string]*buildState),
		latestBySite: make(map[string]string),
		cancels:      make(map[string]context.CancelFunc),
		siteLock:     make(map[string]*sync.Mutex),
		buildSem:     make(chan struct{}, maxConc),
	}
}

// finalizeBuild writes the terminal in-memory + DB state for a build.
// Shared by all three build goroutines so the rules live once:
//   - a CancelBuild-set "cancelled" status wins over the unwinding
//     goroutine's "failed" (both in memory and in the DB row), so the
//     operator sees what actually happened;
//   - the previous terminal buildState for the site is evicted
//     (bounded memory, see latestBySite);
//   - DB writes use their own bounded context, never the build ctx:
//     on timeout/cancel the build ctx is already dead, and writing the
//     terminal status through it silently failed, stranding the row in
//     'pending' (the exact zombie the boot reaper cleans up).
func (h *BuildHandler) finalizeBuild(deployID, siteID string, result *builder.BuildResult) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status := "success"
	if !result.Success {
		status = "failed"
	}

	h.mu.Lock()
	st := h.builds[deployID]
	if st != nil {
		if st.Status == "cancelled" {
			status = "cancelled"
			if result.Error == "" {
				result.Error = st.Error
			}
		}
		st.BuildLog = result.BuildLog
		st.PagesBuilt = result.PagesBuilt
		st.DurationMs = result.DurationMs
		st.DistDir = result.DistDir
		st.Status = status
		if status != "success" {
			st.Error = result.Error
		}
	}
	if prev := h.latestBySite[siteID]; prev != "" && prev != deployID {
		delete(h.builds, prev)
	}
	h.latestBySite[siteID] = deployID
	h.mu.Unlock()

	if err := h.queries.UpdateDeploymentStatus(ctx, store.UpdateDeploymentStatusParams{
		ID:         deployID,
		Status:     status,
		BuildLog:   result.BuildLog,
		PagesBuilt: int64(result.PagesBuilt),
		DurationMs: result.DurationMs,
		Error:      result.Error,
	}); err != nil {
		slog.Warn("build: persist deployment status failed", "deploy_id", deployID, "err", err)
	}
	if err := h.queries.UpdateSiteBuildStatus(ctx, store.UpdateSiteBuildStatusParams{
		ID:              siteID,
		LastBuildStatus: status,
		LastBuildError:  result.Error,
	}); err != nil {
		slog.Warn("build: persist site build status failed", "site_id", siteID, "err", err)
	}
}

// gradeRank orders eval letter grades (higher = better) so a configured
// publish-gate threshold can be compared. N/A and unknown grades are absent
// (rank 0) and are ignored by belowPublishGrade rather than treated as failing.
var gradeRank = map[string]int{
	"A+": 13, "A": 12, "A-": 11,
	"B+": 10, "B": 9, "B-": 8,
	"C+": 7, "C": 6, "C-": 5,
	"D+": 4, "D": 3, "D-": 2,
	"F": 1,
}

// belowPublishGrade reports whether the worst category grade in reports falls
// below ATOMICSITE_MIN_PUBLISH_GRADE. Returns (false, "") when the gate is unset
// (advisory mode, the default) or the threshold/grades can't be ranked, so the
// existing unconditional-publish behavior is preserved unless an operator opts
// in. This resolves the audit's "eval grade gates nothing" finding without
// changing default behavior or risking a tenant's publish.
//
// Fidelity-aware: under showcase, the performance category compares
// against a floor three ranks below the configured minimum (the operator
// explicitly traded speed for design; blocking publish on that trade
// would fight the dial). Security, seo, accessibility, and privacy keep
// the full configured minimum in every fidelity.
func belowPublishGrade(reports []eval.CategoryReport, fidelity agent.DesignFidelity) (bool, string) {
	want, ok := gradeRank[strings.TrimSpace(os.Getenv("ATOMICSITE_MIN_PUBLISH_GRADE"))]
	if !ok {
		return false, ""
	}
	perfWant := want
	if fidelity == agent.FidelityShowcase {
		perfWant = want - 3
		if perfWant < 1 {
			perfWant = 1
		}
	}
	worst, blocked := "", false
	for _, r := range reports {
		rk, ok := gradeRank[r.Grade]
		if !ok {
			continue
		}
		threshold := want
		if r.Category == "performance" {
			threshold = perfWant
		}
		if rk < threshold {
			blocked = true
			if worst == "" || rk < gradeRank[worst] {
				worst = r.Grade
			}
		}
	}
	return blocked, worst
}

// acquireBuildSlot blocks until a global build slot is free or ctx is done.
// Returns a release func (nil + false when ctx ended first). Bounds total
// concurrent bun/astro processes regardless of how many sites build at once.
func (h *BuildHandler) acquireBuildSlot(ctx context.Context) (func(), bool) {
	select {
	case h.buildSem <- struct{}{}:
		return func() { <-h.buildSem }, true
	case <-ctx.Done():
		return nil, false
	}
}

// registerCancel stashes the cancel func for an in-flight build so
// the admin Cancel endpoint can interrupt it. clearCancel removes
// the entry once the goroutine returns.
func (h *BuildHandler) registerCancel(buildID string, cancel context.CancelFunc) {
	h.mu.Lock()
	h.cancels[buildID] = cancel
	h.mu.Unlock()
}

func (h *BuildHandler) clearCancel(buildID string) {
	h.mu.Lock()
	delete(h.cancels, buildID)
	h.mu.Unlock()
}

// CancelBuild aborts the in-flight goroutine for buildID by calling
// its derived-context cancel. Pre-existing bun / eval / rsync exec
// commands honour ctx via exec.CommandContext, so the chain unwinds
// within a few seconds. Idempotent: calling on a finished build is
// a no-op.
//
// POST /api/sites/{siteID}/builds/{buildID}/cancel  (admin-only via
// the existing siteR + RequireSiteAccess middleware on the parent
// route group)
func (h *BuildHandler) CancelBuild(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	buildID := urlParam(r, "buildID")
	// Cross-tenant guard: deployment must belong to the URL siteID.
	dep, err := h.queries.GetDeploymentByID(r.Context(), buildID)
	if err != nil || dep.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Build not found")
		return
	}
	// Read the cancel func and apply the status in ONE critical section, and
	// only when the build is not already terminal.
	//
	// These were two separate h.mu acquisitions, and clearCancel is deferred so
	// it runs AFTER the build lock is released and well after finalizeBuild has
	// written "success" to both memory and the DB. So an operator cancel landing
	// in that window found the cancel entry still present and overwrote
	// st.Status with "cancelled": the DB said success, memory said cancelled,
	// Deploy then refused the build forever, and GetBuildStatus reported a
	// cancellation that never happened.
	h.mu.Lock()
	cancel, ok := h.cancels[buildID]
	st := h.builds[buildID]
	terminal := false
	if ok && st != nil {
		switch st.Status {
		case "success", "failed", "cancelled":
			terminal = true
		default:
			st.Status = "cancelled"
			st.Error = "build cancelled by admin"
		}
	}
	h.mu.Unlock()
	if !ok {
		// Either already finished or never tracked. Returning 200 keeps
		// the cancel verb idempotent so admin scripts can fire it
		// without checking state first.
		writeJSON(w, http.StatusOK, map[string]string{"status": "not running"})
		return
	}
	if terminal {
		// The build finished while the cancel was in flight. Do NOT rewrite its
		// status: it really did succeed or fail, and saying otherwise strands a
		// good build as undeployable.
		slog.Info("build: cancel arrived after the build reached a terminal state; leaving it alone",
			"build_id", buildID, "site_id", siteID, "status", st.Status)
		writeJSON(w, http.StatusOK, map[string]string{"status": "already finished"})
		return
	}
	cancel()
	slog.Info("build: cancelled by admin", "build_id", buildID, "site_id", siteID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelling"})
}

// lockForSite returns the per-site build mutex, creating it on first
// use. Callers Lock + Unlock around the entire builder.Build pipeline.
// Closes audit C4.
func (h *BuildHandler) lockForSite(siteID string) *sync.Mutex {
	h.siteMu.Lock()
	defer h.siteMu.Unlock()
	mu, ok := h.siteLock[siteID]
	if !ok {
		mu = &sync.Mutex{}
		h.siteLock[siteID] = mu
	}
	return mu
}

// provisionCookieProof was removed 2026-04-30 when atomicsite became
// standalone for cookies. The widget bundle is now embedded in the
// atomicsite Go binary (see internal/builder/widget_embed.go) and shipped
// per-tenant as a same-origin asset. No remote CookieProof API call is
// needed at build time; the inline config blob carries everything.

// TriggerBuild starts an async build for the agent's site.
func (h *BuildHandler) TriggerBuild(w http.ResponseWriter, r *http.Request) {
	a := authmw.GetAgent(r)
	if a == nil {
		writeError(w, http.StatusUnauthorized, "Agent not authenticated")
		return
	}

	// Sprint 3 quota gate: short-circuit before we create a
	// deployment row so the over-quota site doesn't accumulate
	// orphan "queued" rows that never run.
	if h.quota != nil && h.quota.EnforceBuildMinutesQuota(w, r, a.SiteID) {
		return
	}

	// Create a deployment record
	deployID := newID()
	err := h.queries.CreateDeployment(r.Context(), store.CreateDeploymentParams{
		ID:           deployID,
		SiteID:       a.SiteID,
		DeployTarget: "local",
		DeployConfig: "{}",
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create deployment record")
		return
	}

	// Track build state
	h.mu.Lock()
	h.builds[deployID] = &buildState{Status: "building"}
	h.mu.Unlock()

	// Run build async (use background context -- request context cancels when client disconnects)
	siteID := a.SiteID
	go func() {
		// Audit H4: cap the entire build pipeline at BuildTimeout via
		// the goroutine's own derived context. Registered BEFORE the
		// per-site lock so a build queued behind a long sibling can be
		// cancelled too (the mutex wait itself is not ctx-aware, but the
		// pipeline fails fast on a dead ctx right after acquiring).
		bgCtx, cancel := context.WithTimeout(context.Background(), builder.BuildTimeout)
		defer cancel()
		h.registerCancel(deployID, cancel)
		defer h.clearCancel(deployID)
		// Audit C4: serialize per site_id so two simultaneous builds for
		// the same site don't fight over the workspace directory. Other
		// sites build in parallel without contention.
		lock := h.lockForSite(siteID)
		lock.Lock()
		defer lock.Unlock()
		// Global concurrency cap (after the per-site lock so a queued build
		// never holds a slot): bound total simultaneous bun/astro processes.
		release, ok := h.acquireBuildSlot(bgCtx)
		if !ok {
			// Finalize through the shared path so the DB row reaches a
			// terminal state too (it used to stay 'pending' forever).
			h.finalizeBuild(deployID, siteID, &builder.BuildResult{
				Error: "cancelled while waiting for a build slot",
			})
			return
		}
		defer release()
		result := builder.Build(bgCtx, h.queries, siteID, h.cfg.DataDir)

		// Run evaluation against the built dist/ if compile succeeded.
		if result.Success && result.DistDir != "" {
			if _, err := eval.Run(bgCtx, h.queries, siteID, deployID, result.DistDir); err != nil {
				// Eval failures are non-fatal; build succeeded.
				result.BuildLog += "\n=== eval ===\nfailed: " + err.Error() + "\n"
			}
			// Design critique runs after eval and persists to the same
			// evaluations table with category="design" so the existing
			// /api/sites/{id}/evaluations endpoint surfaces both. Same
			// non-fatal pattern: critique failures don't fail the build.
			if _, err := critique.Run(bgCtx, h.queries, siteID, deployID, result.DistDir); err != nil {
				result.BuildLog += "\n=== critique ===\nfailed: " + err.Error() + "\n"
			}
		}

		h.finalizeBuild(deployID, siteID, result)
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{
		"build_id": deployID,
		"status":   "building",
		"message":  "Build started. Poll GET /api/agent/build/" + deployID + "/status for progress.",
	})
}

// StartBuild creates a deployment record + kicks off an async build for
// the given site. Returns the build_id (= deployment id). Non-handler
// entry point used by the MCP server's trigger_build tool so it doesn't
// have to duplicate the deployment + goroutine + cookieproof provisioning
// logic. Mirrors TriggerBuild's body without the http.ResponseWriter.
func (h *BuildHandler) StartBuild(ctx context.Context, siteID string) (string, error) {
	deployID := newID()
	if err := h.queries.CreateDeployment(ctx, store.CreateDeploymentParams{
		ID:           deployID,
		SiteID:       siteID,
		DeployTarget: "local",
		DeployConfig: "{}",
	}); err != nil {
		return "", err
	}

	h.mu.Lock()
	h.builds[deployID] = &buildState{Status: "building"}
	h.mu.Unlock()

	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), builder.BuildTimeout)
		defer cancel()
		// Registered before the per-site lock so queued builds are
		// cancellable too.
		h.registerCancel(deployID, cancel)
		defer h.clearCancel(deployID)
		lock := h.lockForSite(siteID)
		lock.Lock()
		defer lock.Unlock()
		// Global concurrency cap (after the per-site lock so a queued build
		// never holds a slot): bound total simultaneous bun/astro processes.
		release, ok := h.acquireBuildSlot(bgCtx)
		if !ok {
			// Finalize through the shared path so the DB row reaches a
			// terminal state too (it used to stay 'pending' forever).
			h.finalizeBuild(deployID, siteID, &builder.BuildResult{
				Error: "cancelled while waiting for a build slot",
			})
			return
		}
		defer release()
		result := builder.Build(bgCtx, h.queries, siteID, h.cfg.DataDir)
		if result.Success && result.DistDir != "" {
			reports, evalErr := eval.Run(bgCtx, h.queries, siteID, deployID, result.DistDir)
			if evalErr != nil {
				result.BuildLog += "\n=== eval ===\nfailed: " + evalErr.Error() + "\n"
			}
			if _, err := critique.Run(bgCtx, h.queries, siteID, deployID, result.DistDir); err != nil {
				result.BuildLog += "\n=== critique ===\nfailed: " + err.Error() + "\n"
			}
			// Opt-in publish gate: when ATOMICSITE_MIN_PUBLISH_GRADE is set, a
			// build whose worst category grade is below it is built + graded but
			// NOT auto-published. Unset (default) keeps publish unconditional.
			// The gate fails CLOSED on a missing grade: if eval errored (reports
			// nil/empty) while the gate is configured, an ungraded build must
			// not slip past the operator's threshold.
			gateConfigured := strings.TrimSpace(os.Getenv("ATOMICSITE_MIN_PUBLISH_GRADE")) != ""
			if gateConfigured && (evalErr != nil || len(reports) == 0) {
				result.BuildLog += "\n=== deploy ===\nskipped: eval produced no grades while ATOMICSITE_MIN_PUBLISH_GRADE is set; refusing to publish ungraded\n"
			} else if blocked, worst := belowPublishGrade(reports, agent.FidelityForSite(bgCtx, h.queries, siteID)); blocked {
				result.BuildLog += "\n=== deploy ===\nskipped: worst grade " + worst + " is below ATOMICSITE_MIN_PUBLISH_GRADE; published nothing\n"
			} else if targetID, deployURL, deployErr := h.autoDeployDefault(bgCtx, siteID, result.DistDir); deployErr != nil {
				result.BuildLog += "\n=== deploy ===\nfailed: " + deployErr.Error() + "\n"
			} else if targetID != "" {
				result.BuildLog += "\n=== deploy ===\npublished to " + deployURL + "\n"
				if err := h.queries.UpdateDeploymentDeployed(bgCtx, store.UpdateDeploymentDeployedParams{
					TargetID: targetID, DeployUrl: deployURL, ID: deployID,
				}); err != nil {
					slog.Warn("build: persist deployment deploy_url failed", "deploy_id", deployID, "err", err)
				}
			}
		}
		h.finalizeBuild(deployID, siteID, result)
	}()

	return deployID, nil
}

// autoDeployDefault publishes distDir to the site's default deploy target.
// Returns ("", "", nil) when no targets are configured (silent no-op so
// brand-new sites that haven't picked a target yet still get a clean
// build response). Returns (targetID, deployURL, nil) on success.
func (h *BuildHandler) autoDeployDefault(ctx context.Context, siteID, distDir string) (string, string, error) {
	targets, err := h.queries.ListDeployTargetsBySite(ctx, siteID)
	if err != nil {
		return "", "", fmt.Errorf("list deploy targets: %w", err)
	}
	if len(targets) == 0 {
		return "", "", nil
	}
	t := targets[0] // ordered by is_default DESC, name ASC
	cfg := map[string]any{}
	if strings.TrimSpace(t.ConfigJson) != "" {
		_ = json.Unmarshal([]byte(t.ConfigJson), &cfg)
	}
	decryptDeployConfigSecrets(deployConfigCipher(h.cfg), cfg)
	// Slug lets the local deployer accept a /srv/atomicsite/{slug}/dist path
	// (the wildcard serving convention) under the cross-tenant path guard.
	var slug string
	if site, err := h.queries.GetSiteByID(ctx, siteID); err == nil {
		slug = site.Slug
	}
	target := deploy.Target{
		ID:     t.ID,
		SiteID: t.SiteID,
		Slug:   slug,
		Name:   t.Name,
		Kind:   t.Kind,
		Config: cfg,
	}
	deployer, err := deploy.New(target.Kind)
	if err != nil {
		return "", "", fmt.Errorf("deployer: %w", err)
	}
	res, err := deployer.Deploy(ctx, distDir, target)
	if err != nil {
		return "", "", fmt.Errorf("deploy: %w", err)
	}
	return t.ID, res.URL, nil
}

// GetBuildState returns the cross-source build state (in-memory if active,
// DB row if persisted). siteID is checked so MCP can't read across sites.
// Used by the MCP get_build_status + get_evaluation tools to reuse the
// same logic the BuildStatus admin endpoint exercises.
func (h *BuildHandler) GetBuildState(ctx context.Context, buildID, siteID string) (any, error) {
	deploy, err := h.queries.GetDeploymentByID(ctx, buildID)
	if err != nil {
		return nil, fmt.Errorf("build not found: %s", buildID)
	}
	if deploy.SiteID != siteID {
		return nil, fmt.Errorf("build does not belong to this site")
	}
	h.mu.Lock()
	st, ok := h.builds[buildID]
	var snapshot buildState
	if ok {
		snapshot = *st
	}
	h.mu.Unlock()
	if ok {
		return map[string]any{
			"build_id":    buildID,
			"status":      snapshot.Status,
			"build_log":   snapshot.BuildLog,
			"pages_built": snapshot.PagesBuilt,
			"duration_ms": snapshot.DurationMs,
			"error":       snapshot.Error,
			"dist_dir":    snapshot.DistDir,
			"created_at":  deploy.CreatedAt,
		}, nil
	}
	return map[string]any{
		"build_id":    buildID,
		"status":      deploy.Status,
		"build_log":   deploy.BuildLog,
		"pages_built": deploy.PagesBuilt,
		"duration_ms": deploy.DurationMs,
		"error":       deploy.Error,
		"created_at":  deploy.CreatedAt,
	}, nil
}

// BuildStatus returns the current status of a build.
func (h *BuildHandler) BuildStatus(w http.ResponseWriter, r *http.Request) {
	a := authmw.GetAgent(r)
	if a == nil {
		writeError(w, http.StatusUnauthorized, "Agent not authenticated")
		return
	}

	buildID := urlParam(r, "buildID")

	// Tenant scope FIRST: the deployment must belong to the agent's
	// site. This endpoint used to serve any tenant's build state by id
	// (its siblings GetBuildState/BuildStatusAdmin already checked).
	deploy, err := h.queries.GetDeploymentByID(r.Context(), buildID)
	if err != nil || deploy.SiteID != a.SiteID {
		writeError(w, http.StatusNotFound, "Build not found")
		return
	}

	// Prefer in-memory state for active builds. Copy under the mutex:
	// the build goroutine mutates the same struct concurrently, and
	// marshalling the shared pointer outside h.mu was a data race.
	h.mu.Lock()
	state, ok := h.builds[buildID]
	var snapshot buildState
	if ok {
		snapshot = *state
	}
	h.mu.Unlock()

	if ok {
		writeJSON(w, http.StatusOK, snapshot)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":      deploy.Status,
		"build_log":   deploy.BuildLog,
		"pages_built": deploy.PagesBuilt,
		"duration_ms": deploy.DurationMs,
		"error":       deploy.Error,
		"dist_dir":    "",
	})
}

// BuildStatusAdmin returns the status of a build for a site via admin auth.
// Mirrors BuildStatus but scoped to siteID to prevent cross-site ID guessing.
func (h *BuildHandler) BuildStatusAdmin(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	if !isSafeSiteID(siteID) {
		writeError(w, http.StatusBadRequest, "Invalid site ID")
		return
	}
	buildID := urlParam(r, "buildID")
	if buildID == "" {
		writeError(w, http.StatusBadRequest, "Invalid build ID")
		return
	}

	// Look up the deployment so we can verify it belongs to this site.
	deploy, err := h.queries.GetDeploymentByID(r.Context(), buildID)
	if err != nil {
		writeError(w, http.StatusNotFound, "Build not found")
		return
	}
	if deploy.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Build not found")
		return
	}

	// Prefer in-memory state for active builds (has dist_dir + live
	// build_log). Copy under the mutex; the build goroutine writes the
	// same struct concurrently.
	h.mu.Lock()
	state, ok := h.builds[buildID]
	var snapshot buildState
	if ok {
		snapshot = *state
	}
	h.mu.Unlock()

	if ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":      snapshot.Status,
			"build_log":   snapshot.BuildLog,
			"pages_built": snapshot.PagesBuilt,
			"duration_ms": snapshot.DurationMs,
			"error":       snapshot.Error,
			"dist_dir":    snapshot.DistDir,
			"created_at":  deploy.CreatedAt,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":      deploy.Status,
		"build_log":   deploy.BuildLog,
		"pages_built": deploy.PagesBuilt,
		"duration_ms": deploy.DurationMs,
		"error":       deploy.Error,
		"dist_dir":    "",
		"created_at":  deploy.CreatedAt,
	})
}

// deployRequest is the body for POST /api/sites/{siteID}/deploy.
type deployRequest struct {
	BuildID  string `json:"build_id"`
	TargetID string `json:"target_id"`
}

// Deploy ships a previously-built deployment to a configured deploy_target.
// Looks up the deployment row (must belong to siteID), looks up the target
// (must belong to siteID), then runs the kind-specific Deployer against the
// build's dist_dir. On success the deployment row gets target_id, deploy_url,
// deployed_at filled in.
func (h *BuildHandler) Deploy(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	if !isSafeSiteID(siteID) {
		writeError(w, http.StatusBadRequest, "Invalid site ID")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	var req deployRequest
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	req.BuildID = strings.TrimSpace(req.BuildID)
	req.TargetID = strings.TrimSpace(req.TargetID)
	if req.BuildID == "" {
		writeError(w, http.StatusBadRequest, "build_id is required")
		return
	}
	if req.TargetID == "" {
		writeError(w, http.StatusBadRequest, "target_id is required")
		return
	}

	deploymentRow, err := h.queries.GetDeploymentByID(r.Context(), req.BuildID)
	if err != nil {
		writeError(w, http.StatusNotFound, "Build not found")
		return
	}
	if deploymentRow.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Build not found")
		return
	}

	targetRow, err := h.queries.GetDeployTarget(r.Context(), req.TargetID)
	if err != nil {
		writeError(w, http.StatusNotFound, "Deploy target not found")
		return
	}
	if targetRow.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Deploy target not found")
		return
	}

	// Pull the build's dist_dir from the in-memory build state. We deliberately
	// do not persist dist paths to DB (they're ephemeral workspace artifacts),
	// so a deploy after server restart will fail loudly here. Copy under
	// the mutex; the build goroutine mutates the same struct.
	h.mu.Lock()
	statePtr, ok := h.builds[req.BuildID]
	var state buildState
	if ok && statePtr != nil {
		state = *statePtr
	}
	h.mu.Unlock()
	if !ok || state.DistDir == "" {
		writeError(w, http.StatusBadRequest, "Build dist_dir not available; trigger a fresh build before deploying")
		return
	}
	if state.Status != "success" {
		writeError(w, http.StatusBadRequest, "Build did not succeed; cannot deploy")
		return
	}

	// Hold the per-site build lock for the copy: all builds of a site
	// share one workspace dist path, so deploying without the lock can
	// ship a half-written dist while a concurrent build rewrites it.
	buildLock := h.lockForSite(siteID)
	buildLock.Lock()
	defer buildLock.Unlock()

	// RE-READ the build state now that we hold the lock. The check above ran on
	// a VALUE COPY taken before the lock, which was not enough: between the copy
	// and here, a second build of the same site can win the lock, and
	// InitWorkspace does os.RemoveAll on the workspace, which is per-SITE, so
	// every build of a site shares one DistDir string. The sequence:
	//
	//   1. Deploy(B1) copies state: success, DistDir=/data/workspaces/S/dist
	//   2. TriggerBuild(B2) wins lockForSite(S)
	//   3. B2's InitWorkspace deletes B1's dist
	//   4. Deploy(B1) blocks on the lock
	//   5. B2 finishes; finalizeBuild EVICTS B1 from h.builds, invisible to the
	//      value copy Deploy is holding
	//   6. Deploy(B1) ships B2's bytes and records the deploy against B1
	//
	// So unreviewed content publishes under a different build's identity, and if
	// B2 failed late leaving a partial tree, rsync --delete propagates the
	// truncation and deletes the good remote files. Re-reading closes the window
	// because finalizeBuild's eviction and status write both happen under h.mu.
	h.mu.Lock()
	livePtr, stillPresent := h.builds[req.BuildID]
	var live buildState
	if stillPresent && livePtr != nil {
		live = *livePtr
	}
	h.mu.Unlock()
	if !stillPresent {
		writeError(w, http.StatusConflict, "Another build for this site started while this deploy was queued; trigger a fresh build before deploying")
		return
	}
	if live.Status != "success" || live.DistDir == "" {
		writeError(w, http.StatusConflict, "This build is no longer deployable (a concurrent build replaced its workspace); trigger a fresh build")
		return
	}
	if live.DistDir != state.DistDir {
		writeError(w, http.StatusConflict, "This build's dist path changed while the deploy was queued; trigger a fresh build")
		return
	}
	state = live

	cfg := map[string]any{}
	if strings.TrimSpace(targetRow.ConfigJson) != "" {
		_ = json.Unmarshal([]byte(targetRow.ConfigJson), &cfg)
	}
	decryptDeployConfigSecrets(deployConfigCipher(h.cfg), cfg)
	target := deploy.Target{
		ID:     targetRow.ID,
		SiteID: targetRow.SiteID,
		Name:   targetRow.Name,
		Kind:   targetRow.Kind,
		Config: cfg,
	}

	deployer, err := deploy.New(target.Kind)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := deployer.Deploy(r.Context(), state.DistDir, target)
	if err != nil {
		slog.Error("deploy failed",
			"site_id", siteID,
			"build_id", req.BuildID,
			"target_id", req.TargetID,
			"kind", target.Kind,
			"err", err,
		)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	if err := h.queries.UpdateDeploymentDeployed(r.Context(), store.UpdateDeploymentDeployedParams{
		TargetID:  req.TargetID,
		DeployUrl: result.URL,
		ID:        req.BuildID,
	}); err != nil {
		slog.Error("update deployment deployed",
			"build_id", req.BuildID,
			"err", err,
		)
		writeError(w, http.StatusInternalServerError, "Deploy succeeded but DB update failed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"deployment_id": req.BuildID,
		"deploy_url":    result.URL,
		"deployed_at":   result.DeployedAt,
		"size_bytes":    result.SizeBytes,
		"file_count":    result.FileCount,
	})
}

// TriggerBuildAdmin starts a build for a site via admin auth.
func (h *BuildHandler) TriggerBuildAdmin(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	if !isSafeSiteID(siteID) {
		writeError(w, http.StatusBadRequest, "Invalid site ID")
		return
	}

	if h.quota != nil && h.quota.EnforceBuildMinutesQuota(w, r, siteID) {
		return
	}

	deployID := newID()
	err := h.queries.CreateDeployment(r.Context(), store.CreateDeploymentParams{
		ID:           deployID,
		SiteID:       siteID,
		DeployTarget: "local",
		DeployConfig: "{}",
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create deployment record")
		return
	}

	h.mu.Lock()
	h.builds[deployID] = &buildState{Status: "building"}
	h.mu.Unlock()

	adminSiteID := siteID
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), builder.BuildTimeout)
		defer cancel()
		// Same discipline as the agent paths: cancellable via the admin
		// Cancel endpoint (registered before the per-site lock so queued
		// builds are cancellable too) and bounded by the global build
		// semaphore (admin builds used to bypass both, so they could
		// neither be cancelled nor counted against the host's cap).
		h.registerCancel(deployID, cancel)
		defer h.clearCancel(deployID)
		lock := h.lockForSite(adminSiteID)
		lock.Lock()
		defer lock.Unlock()
		release, ok := h.acquireBuildSlot(bgCtx)
		if !ok {
			// Finalize through the shared path so the DB row reaches a
			// terminal state too (it used to stay 'pending' forever).
			h.finalizeBuild(deployID, siteID, &builder.BuildResult{
				Error: "cancelled while waiting for a build slot",
			})
			return
		}
		defer release()
		result := builder.Build(bgCtx, h.queries, adminSiteID, h.cfg.DataDir)
		if result.Success && result.DistDir != "" {
			if _, err := eval.Run(bgCtx, h.queries, adminSiteID, deployID, result.DistDir); err != nil {
				result.BuildLog += "\n=== eval ===\nfailed: " + err.Error() + "\n"
			}
			if _, err := critique.Run(bgCtx, h.queries, adminSiteID, deployID, result.DistDir); err != nil {
				result.BuildLog += "\n=== critique ===\nfailed: " + err.Error() + "\n"
			}
		}
		h.finalizeBuild(deployID, adminSiteID, result)
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{
		"build_id": deployID,
		"status":   "building",
	})
}
