// Package eval runs static-evaluable security/SEO/accessibility/privacy/performance
// checks against a built site's dist/ directory and persists results to the
// evaluations table. Inspired by site-inspector's check set, ported to Go.
//
// Checks operate on:
//   - Parsed HTML (per page) via golang.org/x/net/html
//   - Computed security headers from BuildSecurityHeaders (since we know what
//     we'd emit at deploy time)
//   - Static files (robots.txt, sitemap.xml, llms.txt, security.txt) inside dist/
//
// Performance checks that need a real browser/network are excluded.
package eval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/bright-interaction/slab/internal/agent"
	"github.com/bright-interaction/slab/internal/builder"
	"github.com/bright-interaction/slab/internal/store"
)

// MaxHTMLFileSize bounds memory for any single page. Prevents OOM on a
// pathologically large file in dist/.
const MaxHTMLFileSize = 50 * 1024 * 1024 // 50 MB

// EvalTimeout caps total evaluation time per build.
const EvalTimeout = 30 * time.Second

// Severity classifies a failed check.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
	SeverityInfo    Severity = "info"
)

// CheckResult is one rule's outcome.
type CheckResult struct {
	Name           string   `json:"name"`
	Section        string   `json:"section"`
	Passed         *bool    `json:"passed"` // nil = informational, no scoring
	Weight         int      `json:"weight"`
	Severity       Severity `json:"severity,omitempty"`
	Detail         string   `json:"detail,omitempty"`
	Recommendation string   `json:"recommendation,omitempty"`
	Page           string   `json:"page,omitempty"`
}

// CategoryReport groups checks within a category and computes a score + grade.
type CategoryReport struct {
	Category string        `json:"category"`
	Score    int           `json:"score"`
	MaxScore int           `json:"max_score"`
	Grade    string        `json:"grade"`
	Checks   []CheckResult `json:"checks"`
}

// PageContext is everything a check needs about one page in dist/.
type PageContext struct {
	URL      string            // path relative to dist root, e.g. "/about/index.html"
	Slug     string            // logical slug, e.g. "/about"
	HTML     string            // raw html
	Doc      *html.Node        // parsed
	Headers  map[string]string // synthesized from build settings
	FileSize int64
}

// SiteContext is the site-wide static state.
type SiteContext struct {
	DistDir      string
	Pages        []PageContext
	RobotsTxt    string
	SitemapXML   string
	LLMsTxt      string
	SecurityTxt  string
	HeadersFile  string // _headers content
	NginxConf    string
	Headers      map[string]string // computed security headers
	AllowedHosts []string
	// Fidelity is the site's design.fidelity dial. Performance budgets
	// and a handful of SEO/GEO taste checks grade against it; security,
	// privacy, and accessibility checks ignore it by construction.
	// Zero value = balanced.
	Fidelity agent.DesignFidelity
}

// Pass / Fail / Info constructors -- pointer-bool dance for nil = info.
func Pass(name, section string, weight int, detail string) CheckResult {
	t := true
	return CheckResult{Name: name, Section: section, Passed: &t, Weight: weight, Detail: detail}
}
func Fail(name, section string, weight int, sev Severity, detail, rec string) CheckResult {
	f := false
	return CheckResult{Name: name, Section: section, Passed: &f, Weight: weight, Severity: sev, Detail: detail, Recommendation: rec}
}
func Info(name, section, detail string) CheckResult {
	return CheckResult{Name: name, Section: section, Passed: nil, Detail: detail}
}

// siteWideCheck is the site-wide counterpart of perPageCheck: it runs fn only
// when there is markup to reason about, and returns Info on an empty dist/.
// Site-wide checks that skipped this convention handed a build with zero pages
// full marks for markup that does not exist (and, on the mirror-image branch,
// failed it for the same reason), while perPageCheck beside them correctly
// returned Info.
func siteWideCheck(name, section string, site *SiteContext, fn func() CheckResult) CheckResult {
	if len(site.Pages) == 0 {
		return Info(name, section, "no pages to evaluate")
	}
	return fn()
}

// siteWideCheckMulti is siteWideCheck for a check that emits a partial-credit
// split (a Pass plus a Fail sharing one denominator).
func siteWideCheckMulti(name, section string, site *SiteContext, fn func() []CheckResult) []CheckResult {
	if len(site.Pages) == 0 {
		return []CheckResult{Info(name, section, "no pages to evaluate")}
	}
	return fn()
}

// Run executes all categories and persists results. Returns the per-category
// reports. distDir is the path to the built static site. Bounded by EvalTimeout.
func Run(ctx context.Context, queries *store.Queries, siteID, buildID, distDir string) ([]CategoryReport, error) {
	ctx, cancel := context.WithTimeout(ctx, EvalTimeout)
	defer cancel()

	// Resolve any symlinks so the walker stays inside the real distDir.
	resolved, err := filepath.EvalSymlinks(distDir)
	if err != nil {
		return nil, fmt.Errorf("resolve dist dir: %w", err)
	}
	distDir = resolved

	// Re-eval is idempotent: clear any prior rows for this buildID. A failed
	// delete must abort: continuing would either duplicate every category row
	// or (when the insert then fails too) leave the build graded by the
	// PREVIOUS eval while this run reports success.
	if err := queries.DeleteEvaluationsByBuild(ctx, buildID); err != nil {
		return nil, fmt.Errorf("clear previous evaluations: %w", err)
	}

	site, err := LoadSiteContext(ctx, queries, siteID, distDir)
	if err != nil {
		return nil, fmt.Errorf("load site context: %w", err)
	}

	// The design.fidelity dial selects the performance budgets (and a
	// few SEO/GEO taste checks). Security / privacy / accessibility
	// never read it. Missing row = balanced = today's grading.
	site.Fidelity = agent.FidelityForSite(ctx, queries, siteID)

	perfChecks := RunPerformanceChecks(site)
	perfChecks = append(perfChecks, RunCWVChecks(ctx, queries, siteID, site.Fidelity)...)

	reports := []CategoryReport{
		{Category: "security", Checks: RunSecurityChecks(site)},
		{Category: "seo", Checks: RunSEOChecks(site)},
		{Category: "accessibility", Checks: RunAccessibilityChecks(site)},
		{Category: "privacy", Checks: RunPrivacyChecks(site)},
		{Category: "performance", Checks: perfChecks},
	}

	var persistErrs []string
	for i := range reports {
		reports[i].Score, reports[i].MaxScore = ComputeScore(reports[i].Checks)
		reports[i].Grade = ScoreToGrade(reports[i].Score, reports[i].MaxScore)

		// Persist per-category to the evaluations table
		checksJSON, err := json.Marshal(reports[i].Checks)
		if err != nil {
			slog.Error("eval: encode checks failed", "category", reports[i].Category, "err", err)
			persistErrs = append(persistErrs, reports[i].Category+": encode: "+err.Error())
			continue
		}
		if err := queries.CreateEvaluation(ctx, store.CreateEvaluationParams{
			ID:         newID(),
			BuildID:    buildID,
			SiteID:     siteID,
			Category:   reports[i].Category,
			Score:      int64(reports[i].Score),
			MaxScore:   int64(reports[i].MaxScore),
			Grade:      reports[i].Grade,
			ChecksJson: string(checksJSON),
			Profile:    string(site.Fidelity),
		}); err != nil {
			slog.Error("eval: persist failed", "category", reports[i].Category, "err", err)
			persistErrs = append(persistErrs, reports[i].Category+": "+err.Error())
		}
	}

	slog.Info("eval: complete", "site_id", siteID, "build_id", buildID,
		"security", reports[0].Grade, "seo", reports[1].Grade,
		"a11y", reports[2].Grade, "privacy", reports[3].Grade, "perf", reports[4].Grade)

	// A run that deleted the old rows and stored none of the new ones has
	// left the tenant with NO grades at all, so it must not report success:
	// the publish gate reads the returned error and fails closed.
	if len(persistErrs) > 0 {
		return reports, fmt.Errorf("persist evaluations: %s", strings.Join(persistErrs, "; "))
	}

	return reports, nil
}

// LoadSiteContext walks dist/ + computes headers + reads aux files.
func LoadSiteContext(ctx context.Context, queries *store.Queries, siteID, distDir string) (*SiteContext, error) {
	site := &SiteContext{DistDir: distDir, Headers: map[string]string{}}

	// Compute security headers as if deployed (matches what _headers/nginx.conf emit).
	// Swallowing this error left site.Headers EMPTY, which every header check
	// reads as "header missing": a transient settings read failure fabricated a
	// security F for a correctly configured site and (with
	// ATOMICSITE_MIN_PUBLISH_GRADE set) blocked its publish. The callee was
	// hardened against exactly this, see builder/security.go:40-46.
	hdrs, err := builder.BuildSecurityHeaders(ctx, queries, siteID)
	if err != nil {
		return nil, fmt.Errorf("compute security headers: %w", err)
	}
	for name, value := range map[string]string{
		"Content-Security-Policy":      hdrs.CSP,
		"Strict-Transport-Security":    hdrs.HSTS,
		"X-Frame-Options":              hdrs.XFrameOptions,
		"X-Content-Type-Options":       hdrs.XContentTypeOptions,
		"Referrer-Policy":              hdrs.ReferrerPolicy,
		"Permissions-Policy":           hdrs.PermissionsPolicy,
		"Cross-Origin-Opener-Policy":   hdrs.COOP,
		"Cross-Origin-Resource-Policy": hdrs.CORP,
		"Cross-Origin-Embedder-Policy": hdrs.COEP,
	} {
		if value != "" {
			site.Headers[name] = value
		}
	}

	// Allowed scripts for trust-domain checks
	scripts, _ := queries.ListAllowedScriptsBySite(ctx, siteID)
	for _, s := range scripts {
		if s.IsActive == 1 {
			site.AllowedHosts = append(site.AllowedHosts, s.Domain)
		}
	}

	// Static files
	site.RobotsTxt = readFileSafe(filepath.Join(distDir, "robots.txt"))
	site.SitemapXML = readFileSafe(filepath.Join(distDir, "sitemap-index.xml"))
	if site.SitemapXML == "" {
		site.SitemapXML = readFileSafe(filepath.Join(distDir, "sitemap.xml"))
	}
	site.LLMsTxt = readFileSafe(filepath.Join(distDir, "llms.txt"))
	site.SecurityTxt = readFileSafe(filepath.Join(distDir, ".well-known", "security.txt"))
	site.HeadersFile = readFileSafe(filepath.Join(distDir, "_headers"))

	// nginx.conf is at workspace root, one level above dist/
	nginxPath := filepath.Join(filepath.Dir(distDir), "nginx.conf")
	site.NginxConf = readFileSafe(nginxPath)

	// Walk all html files. Symlinks are NOT followed (default WalkDir behavior).
	absBase, err := filepath.Abs(distDir)
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(distDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			slog.Debug("eval: walk error", "path", path, "err", err)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		// Reject symlinks defensively (WalkDir doesn't follow them, but if
		// someone replaces a generated file with a symlink mid-walk, skip it).
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		// Verify path stays under distDir
		absPath, err := filepath.Abs(path)
		if err != nil || !strings.HasPrefix(absPath, absBase+string(filepath.Separator)) {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(path), ".html") {
			return nil
		}
		stat, err := os.Stat(path)
		if err != nil {
			return nil
		}
		if stat.Size() > MaxHTMLFileSize {
			slog.Warn("eval: skipping oversized HTML", "path", path, "size", stat.Size())
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		doc, err := html.Parse(strings.NewReader(string(data)))
		if err != nil {
			slog.Debug("eval: parse failure", "path", path, "err", err)
			return nil
		}

		rel, _ := filepath.Rel(distDir, path)
		slug := htmlPathToSlug(rel)
		site.Pages = append(site.Pages, PageContext{
			URL:      "/" + filepath.ToSlash(rel),
			Slug:     slug,
			HTML:     string(data),
			Doc:      doc,
			Headers:  site.Headers,
			FileSize: stat.Size(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	return site, nil
}

// htmlPathToSlug maps "about/index.html" -> "/about", "index.html" -> "/".
func htmlPathToSlug(rel string) string {
	rel = filepath.ToSlash(rel)
	if rel == "index.html" {
		return "/"
	}
	if strings.HasSuffix(rel, "/index.html") {
		return "/" + strings.TrimSuffix(rel, "/index.html")
	}
	if strings.HasSuffix(rel, ".html") {
		return "/" + strings.TrimSuffix(rel, ".html")
	}
	return "/" + rel
}

// ComputeScore sums weights of passed checks vs total weight.
func ComputeScore(checks []CheckResult) (score, max int) {
	for _, c := range checks {
		if c.Passed == nil {
			continue // info-only
		}
		max += c.Weight
		if *c.Passed {
			score += c.Weight
		}
	}
	return score, max
}

func readFileSafe(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// newID generates a random hex ID for evaluation rows.
func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
