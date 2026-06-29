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
		cfg:      cfg,
		queries:  queries,
		quota:    quota,
		builds:   make(map[string]*buildState),
		cancels:  make(map[string]context.CancelFunc),
		siteLock: make(map[string]*sync.Mutex),
		buildSem: make(chan struct{}, maxConc),
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
func belowPublishGrade(reports []eval.CategoryReport) (bool, string) {
	want, ok := gradeRank[strings.TrimSpace(os.Getenv("ATOMICSITE_MIN_PUBLISH_GRADE"))]
	if !ok {
		return false, ""
	}
	worst, worstRank := "", 1<<30
	for _, r := range reports {
		if rk, ok := gradeRank[r.Grade]; ok && rk < worstRank {
			worst, worstRank = r.Grade, rk
		}
	}
	if worst == "" {
		return false, ""
	}
	return worstRank < want, worst
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
	h.mu.Lock()
	cancel, ok := h.cancels[buildID]
	st, _ := h.builds[buildID]
	h.mu.Unlock()
	if !ok {
		// Either already finished or never tracked. Returning 200 keeps
		// the cancel verb idempotent so admin scripts can fire it
		// without checking state first.
		writeJSON(w, http.StatusOK, map[string]string{"status": "not running"})
		return
	}
	cancel()
	if st != nil {
		h.mu.Lock()
		st.Status = "cancelled"
		st.Error = "build cancelled by admin"
		h.mu.Unlock()
	}
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
		// Audit C4: serialize per site_id so two simultaneous builds for
		// the same site don't fight over the workspace directory. Other
		// sites build in parallel without contention.
		lock := h.lockForSite(siteID)
		lock.Lock()
		defer lock.Unlock()
		// Audit H4: cap the entire build pipeline at BuildTimeout via
		// the goroutine's own derived context. The bun child processes
		// inside Compile honour ctx via exec.CommandContext.
		bgCtx, cancel := context.WithTimeout(context.Background(), builder.BuildTimeout)
		defer cancel()
		// Register the cancel func so the admin Cancel endpoint can
		// interrupt the goroutine before BuildTimeout elapses.
		h.registerCancel(deployID, cancel)
		defer h.clearCancel(deployID)
		// Global concurrency cap (after the per-site lock so a queued build
		// never holds a slot): bound total simultaneous bun/astro processes.
		release, ok := h.acquireBuildSlot(bgCtx)
		if !ok {
			h.mu.Lock()
			if st := h.builds[deployID]; st != nil {
				st.Status = "failed"
				st.Error = "cancelled while waiting for a build slot"
			}
			h.mu.Unlock()
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

		h.mu.Lock()
		state := h.builds[deployID]
		state.BuildLog = result.BuildLog
		state.PagesBuilt = result.PagesBuilt
		state.DurationMs = result.DurationMs
		state.DistDir = result.DistDir
		if result.Success {
			state.Status = "success"
		} else {
			state.Status = "failed"
			state.Error = result.Error
		}
		h.mu.Unlock()

		// Update DB record
		status := "success"
		if !result.Success {
			status = "failed"
		}
		if err := h.queries.UpdateDeploymentStatus(bgCtx, store.UpdateDeploymentStatusParams{
			ID:         deployID,
			Status:     status,
			BuildLog:   result.BuildLog,
			PagesBuilt: int64(result.PagesBuilt),
			DurationMs: result.DurationMs,
			Error:      result.Error,
		}); err != nil {
			slog.Warn("build: persist deployment status failed", "deploy_id", deployID, "err", err)
		}

		if err := h.queries.UpdateSiteBuildStatus(bgCtx, store.UpdateSiteBuildStatusParams{
			ID:              siteID,
			LastBuildStatus: status,
			LastBuildError:  result.Error,
		}); err != nil {
			slog.Warn("build: persist site build status failed", "site_id", siteID, "err", err)
		}
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
		lock := h.lockForSite(siteID)
		lock.Lock()
		defer lock.Unlock()
		bgCtx, cancel := context.WithTimeout(context.Background(), builder.BuildTimeout)
		defer cancel()
		h.registerCancel(deployID, cancel)
		defer h.clearCancel(deployID)
		// Global concurrency cap (after the per-site lock so a queued build
		// never holds a slot): bound total simultaneous bun/astro processes.
		release, ok := h.acquireBuildSlot(bgCtx)
		if !ok {
			h.mu.Lock()
			if st := h.builds[deployID]; st != nil {
				st.Status = "failed"
				st.Error = "cancelled while waiting for a build slot"
			}
			h.mu.Unlock()
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
			if blocked, worst := belowPublishGrade(reports); blocked {
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
		h.mu.Lock()
		st := h.builds[deployID]
		st.BuildLog = result.BuildLog
		st.PagesBuilt = result.PagesBuilt
		st.DurationMs = result.DurationMs
		st.DistDir = result.DistDir
		if result.Success {
			st.Status = "success"
		} else {
			st.Status = "failed"
			st.Error = result.Error
		}
		h.mu.Unlock()

		status := "success"
		if !result.Success {
			status = "failed"
		}
		if err := h.queries.UpdateDeploymentStatus(bgCtx, store.UpdateDeploymentStatusParams{
			ID: deployID, Status: status, BuildLog: result.BuildLog,
			PagesBuilt: int64(result.PagesBuilt), DurationMs: result.DurationMs, Error: result.Error,
		}); err != nil {
			slog.Warn("build: persist deployment status failed", "deploy_id", deployID, "err", err)
		}
		if err := h.queries.UpdateSiteBuildStatus(bgCtx, store.UpdateSiteBuildStatusParams{
			ID: siteID, LastBuildStatus: status, LastBuildError: result.Error,
		}); err != nil {
			slog.Warn("build: persist site build status failed", "site_id", siteID, "err", err)
		}
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
	h.mu.Unlock()
	if ok {
		return map[string]any{
			"build_id":    buildID,
			"status":      st.Status,
			"build_log":   st.BuildLog,
			"pages_built": st.PagesBuilt,
			"duration_ms": st.DurationMs,
			"error":       st.Error,
			"dist_dir":    st.DistDir,
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

	// Check in-memory state first (for active builds)
	h.mu.Lock()
	state, ok := h.builds[buildID]
	h.mu.Unlock()

	if ok {
		writeJSON(w, http.StatusOK, state)
		return
	}

	// Fall back to DB
	deploy, err := h.queries.GetDeploymentByID(r.Context(), buildID)
	if err != nil {
		writeError(w, http.StatusNotFound, "Build not found")
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

	// Prefer in-memory state for active builds (has dist_dir + live build_log).
	h.mu.Lock()
	state, ok := h.builds[buildID]
	h.mu.Unlock()

	if ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":      state.Status,
			"build_log":   state.BuildLog,
			"pages_built": state.PagesBuilt,
			"duration_ms": state.DurationMs,
			"error":       state.Error,
			"dist_dir":    state.DistDir,
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
	// so a deploy after server restart will fail loudly here.
	h.mu.Lock()
	state, ok := h.builds[req.BuildID]
	h.mu.Unlock()
	if !ok || state == nil || state.DistDir == "" {
		writeError(w, http.StatusBadRequest, "Build dist_dir not available; trigger a fresh build before deploying")
		return
	}
	if state.Status != "success" {
		writeError(w, http.StatusBadRequest, "Build did not succeed; cannot deploy")
		return
	}

	cfg := map[string]any{}
	if strings.TrimSpace(targetRow.ConfigJson) != "" {
		_ = json.Unmarshal([]byte(targetRow.ConfigJson), &cfg)
	}
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
		lock := h.lockForSite(adminSiteID)
		lock.Lock()
		defer lock.Unlock()
		bgCtx, cancel := context.WithTimeout(context.Background(), builder.BuildTimeout)
		defer cancel()
		result := builder.Build(bgCtx, h.queries, adminSiteID, h.cfg.DataDir)
		if result.Success && result.DistDir != "" {
			if _, err := eval.Run(bgCtx, h.queries, adminSiteID, deployID, result.DistDir); err != nil {
				result.BuildLog += "\n=== eval ===\nfailed: " + err.Error() + "\n"
			}
			if _, err := critique.Run(bgCtx, h.queries, adminSiteID, deployID, result.DistDir); err != nil {
				result.BuildLog += "\n=== critique ===\nfailed: " + err.Error() + "\n"
			}
		}

		h.mu.Lock()
		state := h.builds[deployID]
		state.BuildLog = result.BuildLog
		state.PagesBuilt = result.PagesBuilt
		state.DurationMs = result.DurationMs
		state.DistDir = result.DistDir
		if result.Success {
			state.Status = "success"
		} else {
			state.Status = "failed"
			state.Error = result.Error
		}
		h.mu.Unlock()

		status := "success"
		if !result.Success {
			status = "failed"
		}
		if err := h.queries.UpdateDeploymentStatus(bgCtx, store.UpdateDeploymentStatusParams{
			ID:         deployID,
			Status:     status,
			BuildLog:   result.BuildLog,
			PagesBuilt: int64(result.PagesBuilt),
			DurationMs: result.DurationMs,
			Error:      result.Error,
		}); err != nil {
			slog.Warn("build: persist deployment status failed", "deploy_id", deployID, "err", err)
		}
		if err := h.queries.UpdateSiteBuildStatus(bgCtx, store.UpdateSiteBuildStatusParams{
			ID:              adminSiteID,
			LastBuildStatus: status,
			LastBuildError:  result.Error,
		}); err != nil {
			slog.Warn("build: persist site build status failed", "site_id", adminSiteID, "err", err)
		}
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{
		"build_id": deployID,
		"status":   "building",
	})
}
