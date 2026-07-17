package migration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

// VerifyOptions tunes the post-launch crawl. Defaults are conservative:
// 4 parallel checks, 200ms politeness delay, 3 redirect hops max.
type VerifyOptions struct {
	Concurrency     int
	PolitenessDelay time.Duration
	MaxHops         int
	Timeout         time.Duration
	AllowPrivate    bool // tests only

	// OnResult fires once per URL as soon as its result is known. Sprint
	// 4 (2026-05-06) async verify-live uses this to flush per-URL rows
	// into migration_verifications and bump the verify_jobs counters
	// during a long crawl. Optional; nil disables the callback so the
	// synchronous test fixture stays unchanged. Implementers must keep
	// the callback fast - it runs on the worker goroutine and any delay
	// here adds to the verify wall time.
	OnResult func(VerifyResult)
}

const (
	verifyDefaultConcurrency = 4
	verifyDefaultDelay       = 200 * time.Millisecond
	verifyDefaultMaxHops     = 3
	verifyDefaultTimeout     = 15 * time.Second
)

// VerifyResult is what VerifyLive produces per source URL. The deployed
// atomicsite is expected to either serve the URL directly (final 200) or
// 301 it through to a new live URL. Anything that ends in 4xx/5xx, loops,
// or fails to connect counts as "ok=false" and the operator gets to fix it.
type VerifyResult struct {
	SourceURL  string `json:"source_url"`
	StatusCode int    `json:"status_code"`
	FinalURL   string `json:"final_url"`
	Hops       int    `json:"hops"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
}

// VerifyLive crawls every source URL through the deployed atomicsite, follows
// up to MaxHops redirects, and reports per-URL outcome. The "deployedDomain"
// is what the operator's DNS now points at; we substitute that host into
// each source URL before fetching so the crawl exercises the redirect chain
// the live nginx config emits.
//
// Errors fetching individual URLs are non-fatal: each becomes a VerifyResult
// with OK=false and Error set. The function only returns an error if the
// inputs themselves are invalid.
func VerifyLive(ctx context.Context, sourceURLs []string, deployedDomain string, opts VerifyOptions) ([]VerifyResult, error) {
	deployedDomain = strings.TrimSpace(deployedDomain)
	if deployedDomain == "" {
		return nil, errors.New("deployedDomain is required")
	}
	if len(sourceURLs) == 0 {
		return nil, errors.New("sourceURLs is required")
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = verifyDefaultConcurrency
	}
	if opts.PolitenessDelay == 0 {
		opts.PolitenessDelay = verifyDefaultDelay
	}
	if opts.MaxHops <= 0 {
		opts.MaxHops = verifyDefaultMaxHops
	}
	if opts.Timeout == 0 {
		opts.Timeout = verifyDefaultTimeout
	}

	results := make([]VerifyResult, len(sourceURLs))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(opts.Concurrency)
	var hostMu sync.Mutex // serialise per-host so we don't burst the deployed site

	for i, raw := range sourceURLs {
		i, raw := i, raw
		g.Go(func() error {
			r := verifyOne(gctx, raw, deployedDomain, opts, &hostMu)
			results[i] = r
			if opts.OnResult != nil {
				opts.OnResult(r)
			}
			return nil
		})
	}
	_ = g.Wait()
	return results, nil
}

// verifyOne is the per-URL probe. Substitutes deployedDomain into the
// source URL's host, follows up to MaxHops redirects, and records the
// terminal status. The redirect-walk is manual (no http.Client follow)
// so we can count hops and capture the chain.
func verifyOne(ctx context.Context, raw, deployedDomain string, opts VerifyOptions, mu *sync.Mutex) VerifyResult {
	res := VerifyResult{SourceURL: raw}
	target, err := rewriteHostToDeployed(raw, deployedDomain)
	if err != nil {
		res.Error = err.Error()
		return res
	}

	mu.Lock()
	defer func() {
		if opts.PolitenessDelay > 0 {
			select {
			case <-time.After(opts.PolitenessDelay):
			case <-ctx.Done():
			}
		}
		mu.Unlock()
	}()

	if !opts.AllowPrivate {
		if u, err := url.Parse(target); err == nil {
			if err := checkHostPublic(u.Hostname()); err != nil {
				res.Error = fmt.Sprintf("blocked: %v", err)
				return res
			}
		}
	}

	client := &http.Client{
		Timeout:   opts.Timeout,
		Transport: safeTransport(opts.AllowPrivate),
		// Disable auto-follow; we count + log hops manually.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	currentURL := target
	for hop := 0; hop <= opts.MaxHops; hop++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, currentURL, nil)
		if err != nil {
			res.Error = err.Error()
			return res
		}
		req.Header.Set("User-Agent", userAgent)
		resp, err := client.Do(req)
		if err != nil {
			res.Error = err.Error()
			return res
		}
		_ = resp.Body.Close()
		res.StatusCode = resp.StatusCode
		res.FinalURL = currentURL
		res.Hops = hop

		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc := resp.Header.Get("Location")
			if loc == "" {
				res.Error = fmt.Sprintf("redirect %d with empty Location", resp.StatusCode)
				return res
			}
			next, err := resolveRedirect(currentURL, loc)
			if err != nil {
				res.Error = fmt.Sprintf("resolve Location: %v", err)
				return res
			}
			if !opts.AllowPrivate {
				if u, err := url.Parse(next); err == nil {
					if err := checkHostPublic(u.Hostname()); err != nil {
						res.Error = fmt.Sprintf("redirect blocked: %v", err)
						return res
					}
				}
			}
			currentURL = next
			continue
		}
		// Terminal response (2xx, 4xx, 5xx).
		res.OK = resp.StatusCode >= 200 && resp.StatusCode < 300
		if !res.OK {
			res.Error = fmt.Sprintf("terminal status %d", resp.StatusCode)
		}
		return res
	}
	// Hop budget exhausted without a terminal response.
	res.Error = fmt.Sprintf("redirect chain exceeded %d hops", opts.MaxHops)
	return res
}

// rewriteHostToDeployed swaps the host in a source URL with the deployed
// atomicsite domain so the crawl exercises the new edge. Path + query +
// fragment are preserved verbatim. Bare paths (e.g. "/about") are
// promoted to "https://{deployed}/about".
func rewriteHostToDeployed(raw, deployedDomain string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("empty URL")
	}
	if strings.HasPrefix(raw, "/") {
		return urlForDeployed(deployedDomain, raw), nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		// Bare host or path - try a fallback.
		if u, err := url.Parse("https://" + raw); err == nil && u.Host != "" {
			return urlForDeployed(deployedDomain, u.Path+queryFragment(u)), nil
		}
		return "", fmt.Errorf("malformed URL: %s", raw)
	}
	pathPart := u.Path
	if pathPart == "" {
		pathPart = "/"
	}
	return urlForDeployed(deployedDomain, pathPart+queryFragment(u)), nil
}

func urlForDeployed(deployedDomain, pathPart string) string {
	scheme := "https"
	if strings.Contains(deployedDomain, "://") {
		// Tests pass "http://127.0.0.1:NNNN" form; preserve as-is.
		return strings.TrimRight(deployedDomain, "/") + pathPart
	}
	return scheme + "://" + deployedDomain + pathPart
}

func queryFragment(u *url.URL) string {
	out := ""
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		out += "#" + u.Fragment
	}
	return out
}

// resolveRedirect handles relative + absolute Location headers. nginx
// emits absolute URLs by default but plenty of CMS-generated redirect
// rules use relative paths.
func resolveRedirect(currentURL, location string) (string, error) {
	if strings.TrimSpace(location) == "" {
		return "", errors.New("empty Location")
	}
	cur, err := url.Parse(currentURL)
	if err != nil {
		return "", err
	}
	loc, err := url.Parse(location)
	if err != nil {
		return "", err
	}
	return cur.ResolveReference(loc).String(), nil
}
