// Public-GitHub design references (Phase 12.8). Site owners paste a public
// repo URL; atomicsite pre-fetches a representative bundle (package.json,
// README, tailwind.config, global stylesheet, a few component files) and
// surfaces it via the agent context endpoint as "design vocabulary the
// user wants atomicsite to feel like." Read-only pattern reference, not
// code copy.
//
// Public repos only. Auth-bearing PAT support is a follow-up.

package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bright-interaction/slab/internal/config"
	"github.com/bright-interaction/slab/internal/store"
)

const (
	maxRefBytes        = 32 * 1024  // 32 KB per file
	maxRefBundleBytes  = 200 * 1024 // 200 KB total per reference
	githubFetchTimeout = 10 * time.Second
)

// candidatePaths is the list we try to fetch from a repo. Order matters:
// the JS/TS variants of the tailwind config are tried in succession, the
// first hit wins for that slot.
var candidatePaths = []string{
	"package.json",
	"README.md",
	"tailwind.config.ts",
	"tailwind.config.js",
	"tailwind.config.mjs",
	"tailwind.config.cjs",
	"src/app.css",
	"src/styles/global.css",
	"src/styles/globals.css",
	"app/globals.css",
	"styles/globals.css",
	// A handful of representative component paths. The tree is opinionated,
	// but these cover the common layouts.
	"src/components/ui/button.tsx",
	"src/lib/components/ui/Button.svelte",
	"components/ui/button.tsx",
	"src/components/Button.tsx",
	"src/components/Button.svelte",
}

type DesignReferencesHandler struct {
	cfg     *config.Config
	queries *store.Queries
}

func NewDesignReferencesHandler(cfg *config.Config, queries *store.Queries) *DesignReferencesHandler {
	return &DesignReferencesHandler{cfg: cfg, queries: queries}
}

type fetchedFile struct {
	Path  string `json:"path"`
	Bytes int    `json:"bytes"`
	Body  string `json:"body"`
}

type fetchedBundle struct {
	Owner  string        `json:"owner"`
	Repo   string        `json:"repo"`
	Branch string        `json:"branch"`
	Files  []fetchedFile `json:"files"`
	Notes  string        `json:"notes,omitempty"`
}

// Create handles POST /api/sites/{siteID}/design-references.
// Body: { url: "https://github.com/owner/repo", label?: string, ref_type?: "design-system|component-library|reference-site" }
// On success, the row carries the fetched_json bundle for the agent to read.
func (h *DesignReferencesHandler) Create(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	if _, err := h.queries.GetSiteByID(r.Context(), siteID); err != nil {
		writeError(w, http.StatusNotFound, "Site not found")
		return
	}

	var req struct {
		URL     string `json:"url"`
		Label   string `json:"label"`
		RefType string `json:"ref_type"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	owner, repo, branch, ok := parseGitHubURL(req.URL)
	if !ok {
		writeError(w, http.StatusBadRequest, "url must be a public GitHub repo URL (https://github.com/owner/repo)")
		return
	}
	if req.RefType == "" {
		req.RefType = "design-system"
	}
	switch req.RefType {
	case "design-system", "component-library", "reference-site":
		// allowed
	default:
		writeError(w, http.StatusBadRequest, "ref_type must be design-system, component-library, or reference-site")
		return
	}

	bundle, err := fetchGitHubBundle(r.Context(), owner, repo, branch)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("GitHub fetch failed: %v", err))
		return
	}
	bundleBytes, _ := json.Marshal(bundle)
	now := time.Now().UTC().Format(time.RFC3339)

	id := newRefID()
	if err := h.queries.CreateDesignReference(r.Context(), store.CreateDesignReferenceParams{
		ID:          id,
		SiteID:      siteID,
		Url:         fmt.Sprintf("https://github.com/%s/%s", owner, repo),
		Label:       strings.TrimSpace(req.Label),
		RefType:     req.RefType,
		FetchedJson: string(bundleBytes),
		FetchedAt:   now,
	}); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeError(w, http.StatusConflict, "This repo is already a design reference for the site")
			return
		}
		writeError(w, http.StatusInternalServerError, "Failed to record design reference")
		return
	}
	row, _ := h.queries.GetDesignReference(r.Context(), store.GetDesignReferenceParams{ID: id, SiteID: siteID})
	writeJSON(w, http.StatusCreated, row)
}

// List returns all design references for the site.
// GET /api/sites/{siteID}/design-references
func (h *DesignReferencesHandler) List(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	rows, err := h.queries.ListDesignReferences(r.Context(), siteID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list design references")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"references": rows})
}

// RefreshDesignReferenceBundle re-fetches a design reference's GitHub bundle
// and updates its fetched_json, org/site-scoped by refID+siteID. Shared by the
// REST Refresh handler and the MCP refresh_design_reference tool so the fetch
// path lives in one place. Returns a caller-safe error.
func RefreshDesignReferenceBundle(ctx context.Context, q *store.Queries, siteID, refID string) error {
	row, err := q.GetDesignReference(ctx, store.GetDesignReferenceParams{ID: refID, SiteID: siteID})
	if err != nil {
		return errors.New("design reference not found")
	}
	owner, repo, branch, ok := parseGitHubURL(row.Url)
	if !ok {
		return errors.New("stored URL is not a valid GitHub repo URL")
	}
	bundle, err := fetchGitHubBundle(ctx, owner, repo, branch)
	if err != nil {
		return fmt.Errorf("github fetch failed: %w", err)
	}
	bundleBytes, _ := json.Marshal(bundle)
	now := time.Now().UTC().Format(time.RFC3339)
	return q.UpdateDesignReferenceFetched(ctx, store.UpdateDesignReferenceFetchedParams{
		FetchedJson: string(bundleBytes),
		FetchedAt:   now,
		ID:          refID,
		SiteID:      siteID,
	})
}

// Refresh re-fetches the bundle. Useful when the upstream repo changed.
// POST /api/sites/{siteID}/design-references/{refID}/refresh
func (h *DesignReferencesHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	refID := urlParam(r, "refID")
	if err := RefreshDesignReferenceBundle(r.Context(), h.queries, siteID, refID); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	updated, _ := h.queries.GetDesignReference(r.Context(), store.GetDesignReferenceParams{ID: refID, SiteID: siteID})
	writeJSON(w, http.StatusOK, updated)
}

// Delete removes a design reference.
// DELETE /api/sites/{siteID}/design-references/{refID}
func (h *DesignReferencesHandler) Delete(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	refID := urlParam(r, "refID")
	if _, err := h.queries.GetDesignReference(r.Context(), store.GetDesignReferenceParams{ID: refID, SiteID: siteID}); err != nil {
		writeError(w, http.StatusNotFound, "Design reference not found")
		return
	}
	if err := h.queries.DeleteDesignReference(r.Context(), store.DeleteDesignReferenceParams{ID: refID, SiteID: siteID}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete reference")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// parseGitHubURL accepts:
//
//	https://github.com/owner/repo
//	https://github.com/owner/repo/
//	https://github.com/owner/repo.git
//	https://github.com/owner/repo/tree/<branch>
//
// and returns owner, repo, branch (default "main"), ok.
func parseGitHubURL(rawURL string) (owner, repo, branch string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host != "github.com" {
		return "", "", "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return "", "", "", false
	}
	owner = parts[0]
	repo = strings.TrimSuffix(parts[1], ".git")
	branch = "main"
	if len(parts) >= 4 && parts[2] == "tree" {
		branch = parts[3]
	}
	if owner == "" || repo == "" {
		return "", "", "", false
	}
	return owner, repo, branch, true
}

// fetchGitHubBundle pulls the candidate paths from the public raw URL.
// Tolerant of 404s (most repos won't have every path); returns whatever
// hits succeed. Total bundle size is capped at maxRefBundleBytes.
func fetchGitHubBundle(ctx context.Context, owner, repo, branch string) (*fetchedBundle, error) {
	client := &http.Client{Timeout: githubFetchTimeout}
	bundle := &fetchedBundle{Owner: owner, Repo: repo, Branch: branch, Files: []fetchedFile{}}
	totalBytes := 0
	branches := []string{branch}
	if branch != "main" {
		// "main" is the modern default; also try as fallback so paste-a-tree-URL works.
		branches = append(branches, "main")
	} else {
		// "main" first, fall back to "master" for older repos.
		branches = append(branches, "master")
	}

	for _, p := range candidatePaths {
		if totalBytes >= maxRefBundleBytes {
			break
		}
		var body []byte
		var hitBranch string
		for _, br := range branches {
			rawURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s", owner, repo, br, p)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
			if err != nil {
				continue
			}
			resp, err := client.Do(req)
			if err != nil {
				continue
			}
			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				continue
			}
			limited := io.LimitReader(resp.Body, int64(maxRefBytes)+1)
			b, err := io.ReadAll(limited)
			resp.Body.Close()
			if err != nil {
				continue
			}
			if len(b) > maxRefBytes {
				b = b[:maxRefBytes]
			}
			body = b
			hitBranch = br
			break
		}
		if body == nil {
			continue
		}
		bundle.Files = append(bundle.Files, fetchedFile{
			Path:  p,
			Bytes: len(body),
			Body:  string(body),
		})
		totalBytes += len(body)
		if hitBranch != "" && hitBranch != bundle.Branch {
			bundle.Branch = hitBranch
		}
	}

	if len(bundle.Files) == 0 {
		bundle.Notes = "No representative files matched. The repo may use a non-standard layout. Add file paths via a future advanced settings UI."
	}
	return bundle, nil
}

func newRefID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
