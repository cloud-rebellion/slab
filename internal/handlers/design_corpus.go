// Package handlers: design_corpus.go exposes a read-only search
// over the bundled MIT-licensed reference repos in
// $DESIGN_REFERENCES_DIR (astrowind, astro-paper, starlight,
// shadcn-svelte, shadcn-ui sparse). Surfaced via the
// search_design_corpus MCP tool.
//
// Implementation is pure Go (no ripgrep dependency) using
// filepath.WalkDir + a per-file scanner so the binary stays self
// contained and the runtime container needs no extra packages. Bounded
// by file-count, file-size, and total-result caps so a pathological
// query can't burn the build pipeline.
package handlers

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bright-interaction/slab/internal/config"
)

// SearchDesignCorpusRequest is the MCP tool input shape.
type SearchDesignCorpusRequest struct {
	Query      string   `json:"query"`
	Kind       string   `json:"kind"`
	Repos      []string `json:"repos"`
	MaxResults int      `json:"max_results"`
}

// CorpusMatch is one match in the search results.
type CorpusMatch struct {
	Repo    string   `json:"repo"`
	Path    string   `json:"path"`
	Line    int      `json:"line"`
	Snippet string   `json:"snippet"`
	Context []string `json:"context"`
}

// SearchDesignCorpusResponse is the MCP tool output shape.
type SearchDesignCorpusResponse struct {
	Query     string        `json:"query"`
	Kind      string        `json:"kind"`
	Repos     []string      `json:"repos"`
	Matches   []CorpusMatch `json:"matches"`
	Truncated bool          `json:"truncated"`
}

// DefaultMaxResults caps the response. The MCP layer enforces a hard
// cap on top so an agent can't bypass this with max_results=999.
const DefaultMaxResults = 20

// HardMaxResults is the absolute ceiling.
const HardMaxResults = 50

// MaxFileBytes skips any file larger than this. shadcn-ui has some
// generated bundles that aren't worth scanning.
const MaxFileBytes = 256 * 1024

// MaxFilesScanned bounds the walk so a malformed corpus dir can't
// stall the pipeline.
const MaxFilesScanned = 8000

// extensions per kind. Glob-style; matched against lowercase suffix.
var extsByKind = map[string][]string{
	"code":   {".svelte", ".astro", ".tsx", ".ts", ".jsx", ".js", ".vue"},
	"css":    {".css", ".scss"},
	"tokens": {".json", ".cjs", ".mjs"},
	"all":    {".svelte", ".astro", ".tsx", ".ts", ".jsx", ".js", ".vue", ".css", ".scss", ".json", ".cjs", ".mjs", ".md"},
}

// SearchDesignCorpus searches the configured corpus dir for matches.
// Returns ErrCorpusNotConfigured when DESIGN_REFERENCES_DIR is empty
// or the dir doesn't exist; the MCP tool surfaces that as a clean
// error to the caller.
var ErrCorpusNotConfigured = errors.New("design references corpus not configured (set DESIGN_REFERENCES_DIR)")

func SearchDesignCorpus(ctx context.Context, cfg *config.Config, req SearchDesignCorpusRequest) (*SearchDesignCorpusResponse, error) {
	if cfg == nil || cfg.DesignReferencesDir == "" {
		return nil, ErrCorpusNotConfigured
	}
	root := cfg.DesignReferencesDir
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return nil, ErrCorpusNotConfigured
	}

	if strings.TrimSpace(req.Query) == "" {
		return nil, errors.New("query is required")
	}

	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	if kind == "" {
		kind = "all"
	}
	exts, ok := extsByKind[kind]
	if !ok {
		return nil, fmt.Errorf("unknown kind %q, valid: code|css|tokens|all", kind)
	}

	max := req.MaxResults
	if max <= 0 {
		max = DefaultMaxResults
	}
	if max > HardMaxResults {
		max = HardMaxResults
	}

	// Build a regex from the query. Treat as literal substring unless
	// the query starts with "re:" which switches to raw regex mode.
	var pattern *regexp.Regexp
	var err error
	q := req.Query
	if strings.HasPrefix(q, "re:") {
		pattern, err = regexp.Compile("(?i)" + strings.TrimPrefix(q, "re:"))
	} else {
		pattern, err = regexp.Compile("(?i)" + regexp.QuoteMeta(q))
	}
	if err != nil {
		return nil, fmt.Errorf("invalid regex: %w", err)
	}

	// Repo filter (case-insensitive directory name match).
	repoFilter := map[string]bool{}
	for _, r := range req.Repos {
		repoFilter[strings.ToLower(strings.TrimSpace(r))] = true
	}

	var (
		matches   []CorpusMatch
		filesSeen int
		truncated bool
		extSet    = make(map[string]bool, len(exts))
	)
	for _, e := range exts {
		extSet[e] = true
	}

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // skip unreadable paths, don't abort
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			// Skip nested git/node_modules.
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "dist" || name == "build" || name == ".next" {
				return filepath.SkipDir
			}
			return nil
		}
		filesSeen++
		if filesSeen > MaxFilesScanned {
			truncated = true
			return filepath.SkipAll
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if !extSet[ext] {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		repo := repoFromRel(rel)
		if len(repoFilter) > 0 && !repoFilter[strings.ToLower(repo)] {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > MaxFileBytes {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		lines := strings.Split(string(body), "\n")
		for i, line := range lines {
			if !pattern.MatchString(line) {
				continue
			}
			matches = append(matches, CorpusMatch{
				Repo:    repo,
				Path:    rel,
				Line:    i + 1,
				Snippet: trimLine(line),
				Context: surroundingLines(lines, i),
			})
			if len(matches) >= max {
				truncated = true
				return filepath.SkipAll
			}
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, filepath.SkipAll) {
		return nil, walkErr
	}

	return &SearchDesignCorpusResponse{
		Query:     req.Query,
		Kind:      kind,
		Repos:     req.Repos,
		Matches:   matches,
		Truncated: truncated,
	}, nil
}

// repoFromRel extracts the top-level reference repo name from a
// path relative to the corpus root.
func repoFromRel(rel string) string {
	if rel == "" {
		return ""
	}
	idx := strings.IndexAny(rel, string(filepath.Separator)+"/")
	if idx <= 0 {
		return rel
	}
	return rel[:idx]
}

// trimLine drops leading/trailing whitespace and caps the displayed
// snippet so a 4000-char minified line doesn't blow up the response.
func trimLine(line string) string {
	t := strings.TrimSpace(line)
	if len(t) > 240 {
		return t[:240] + "."
	}
	return t
}

// surroundingLines returns up to 3 lines of context (the matching line
// plus one before and one after) so the agent has enough to read.
func surroundingLines(lines []string, i int) []string {
	start := i - 1
	if start < 0 {
		start = 0
	}
	end := i + 2
	if end > len(lines) {
		end = len(lines)
	}
	out := make([]string, 0, end-start)
	for j := start; j < end; j++ {
		out = append(out, trimLine(lines[j]))
	}
	return out
}
