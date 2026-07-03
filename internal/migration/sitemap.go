package migration

import (
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/sync/errgroup"
)

// CrawlOptions tunes the sitemap crawler. Defaults are conservative on
// purpose: real WordPress installs rate-limit aggressive crawlers and we
// would rather ship an importer that takes 10 minutes than one that gets
// the source site's IP banned mid-migration.
type CrawlOptions struct {
	Concurrency     int           // parallel fetches; default 8
	PolitenessDelay time.Duration // pause between requests to same host; default 200ms
	MaxURLs         int           // hard cap on URLs touched per crawl; default 5000
	FetchOpts       FetchOptions
}

const (
	defaultConcurrency     = 8
	defaultPolitenessDelay = 200 * time.Millisecond
	defaultMaxURLs         = 5000
)

// urlsetXML and sitemapindexXML are the two top-level shapes the sitemaps.org
// schema allows. We accept either at the top level; sitemap-index recursion
// then pulls <urlset> children one level deeper.
type urlsetXML struct {
	XMLName xml.Name     `xml:"urlset"`
	URLs    []sitemapURL `xml:"url"`
}

type sitemapindexXML struct {
	XMLName  xml.Name       `xml:"sitemapindex"`
	Sitemaps []sitemapEntry `xml:"sitemap"`
}

type sitemapURL struct {
	Loc        string `xml:"loc"`
	LastMod    string `xml:"lastmod,omitempty"`
	ChangeFreq string `xml:"changefreq,omitempty"`
	Priority   string `xml:"priority,omitempty"`
}

type sitemapEntry struct {
	Loc     string `xml:"loc"`
	LastMod string `xml:"lastmod,omitempty"`
}

// ParseSitemap takes a fetched sitemap XML body and returns the page URLs.
// If it's a sitemap-index, the caller is expected to recursively fetch each
// child sitemap (CrawlSitemap orchestrates that).
func ParseSitemap(body []byte) (urls []string, indexed []string, err error) {
	// Try urlset first - it's the common case.
	var us urlsetXML
	if err := xml.Unmarshal(body, &us); err == nil && us.XMLName.Local == "urlset" {
		out := make([]string, 0, len(us.URLs))
		for _, u := range us.URLs {
			loc := strings.TrimSpace(u.Loc)
			if loc != "" {
				out = append(out, loc)
			}
		}
		return out, nil, nil
	}
	var idx sitemapindexXML
	if err := xml.Unmarshal(body, &idx); err == nil && idx.XMLName.Local == "sitemapindex" {
		out := make([]string, 0, len(idx.Sitemaps))
		for _, s := range idx.Sitemaps {
			loc := strings.TrimSpace(s.Loc)
			if loc != "" {
				out = append(out, loc)
			}
		}
		return nil, out, nil
	}
	return nil, nil, fmt.Errorf("body is neither <urlset> nor <sitemapindex>")
}

// CrawlSitemap fetches the sitemap at sitemapURL, recurses into any
// sitemap-index entries (one level deep is enough for every CMS we've seen),
// dedupes, then fetches every URL in parallel and extracts a MigrationPage
// for each. Returns the assembled MigrationManifest.
//
// Errors fetching individual pages are non-fatal: they're recorded in
// Stats.URLsFailed and Manifest.Warnings, never returned. The crawl as a
// whole only fails if the sitemap itself is unfetchable or unparseable.
func CrawlSitemap(ctx context.Context, sitemapURL string, opts CrawlOptions) (*MigrationManifest, error) {
	if opts.Concurrency == 0 {
		opts.Concurrency = defaultConcurrency
	}
	if opts.PolitenessDelay == 0 {
		opts.PolitenessDelay = defaultPolitenessDelay
	}
	if opts.MaxURLs == 0 {
		opts.MaxURLs = defaultMaxURLs
	}

	manifest := &MigrationManifest{
		SourceURL:  sitemapURL,
		SourceType: SourceSitemap,
		FetchedAt:  time.Now().UTC(),
	}
	if u, err := url.Parse(sitemapURL); err == nil {
		manifest.SourceDomain = u.Host
	}

	// Fetch root sitemap.
	root, err := SafeFetch(ctx, sitemapURL, opts.FetchOpts)
	if err != nil {
		return nil, fmt.Errorf("fetch sitemap: %w", err)
	}
	if root.Status != 200 {
		return nil, fmt.Errorf("sitemap returned status %d", root.Status)
	}
	pageURLs, indexedURLs, err := ParseSitemap(root.Body)
	if err != nil {
		return nil, fmt.Errorf("parse sitemap: %w", err)
	}

	// Recurse one level into any sitemap-index entries.
	if len(indexedURLs) > 0 {
		for _, sub := range indexedURLs {
			subResp, err := SafeFetch(ctx, sub, opts.FetchOpts)
			if err != nil {
				manifest.Warnings = append(manifest.Warnings, fmt.Sprintf("fetch child sitemap %s: %v", sub, err))
				continue
			}
			if subResp.Status != 200 {
				manifest.Warnings = append(manifest.Warnings, fmt.Sprintf("child sitemap %s status %d", sub, subResp.Status))
				continue
			}
			childPages, _, err := ParseSitemap(subResp.Body)
			if err != nil {
				manifest.Warnings = append(manifest.Warnings, fmt.Sprintf("parse child sitemap %s: %v", sub, err))
				continue
			}
			pageURLs = append(pageURLs, childPages...)
			// Cap: if we've already hit MaxURLs, stop recursing.
			if len(pageURLs) >= opts.MaxURLs {
				break
			}
		}
	}

	// Dedupe + cap.
	seen := make(map[string]bool, len(pageURLs))
	deduped := make([]string, 0, len(pageURLs))
	for _, u := range pageURLs {
		if seen[u] {
			continue
		}
		seen[u] = true
		deduped = append(deduped, u)
		if len(deduped) >= opts.MaxURLs {
			break
		}
	}
	pageURLs = deduped
	manifest.Stats.URLsRequested = len(pageURLs)

	// Parallel fetch with politeness delay per host.
	type fetchedPage struct {
		page MigrationPage
		err  error
	}
	results := make([]fetchedPage, len(pageURLs))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(opts.Concurrency)
	var hostMu sync.Map // host -> *sync.Mutex for politeness serialization

	for i, raw := range pageURLs {
		i, raw := i, raw // capture
		g.Go(func() error {
			// Per-host politeness: serialize requests to the same host so
			// we never burst N parallel hits at one server.
			h := hostOf(raw)
			muIface, _ := hostMu.LoadOrStore(h, &sync.Mutex{})
			mu := muIface.(*sync.Mutex)
			mu.Lock()
			defer mu.Unlock()

			page, err := FetchAndExtractPage(gctx, raw, opts.FetchOpts)
			results[i] = fetchedPage{page: page, err: err}

			// Politeness delay AFTER fetch so the next caller for this
			// host pauses before its own request.
			if opts.PolitenessDelay > 0 {
				select {
				case <-time.After(opts.PolitenessDelay):
				case <-gctx.Done():
					return gctx.Err()
				}
			}
			return nil
		})
	}
	_ = g.Wait()

	// Assemble results.
	mediaSet := map[string]MigrationMediaRef{}
	for _, r := range results {
		if r.err != nil {
			manifest.Stats.URLsFailed++
			manifest.Warnings = append(manifest.Warnings, fmt.Sprintf("fetch %s: %v", r.page.SourceURL, r.err))
			continue
		}
		manifest.Stats.URLsFetched++
		manifest.Pages = append(manifest.Pages, r.page)
		for _, img := range r.page.ImageURLs {
			if _, ok := mediaSet[img]; ok {
				continue
			}
			mediaSet[img] = MigrationMediaRef{SourceURL: img, OnPage: r.page.SourcePath}
		}
		for _, link := range r.page.InternalLinks {
			manifest.Links = append(manifest.Links, MigrationLink{From: r.page.SourcePath, To: link})
		}
	}
	manifest.Media = make([]MigrationMediaRef, 0, len(mediaSet))
	for _, m := range mediaSet {
		manifest.Media = append(manifest.Media, m)
	}
	manifest.Stats.PagesFound = len(manifest.Pages)
	manifest.Stats.MediaFound = len(manifest.Media)
	manifest.Stats.LinksFound = len(manifest.Links)

	slog.Info("sitemap crawl complete",
		"source", sitemapURL,
		"requested", manifest.Stats.URLsRequested,
		"fetched", manifest.Stats.URLsFetched,
		"failed", manifest.Stats.URLsFailed,
		"warnings", len(manifest.Warnings))

	return manifest, nil
}

// FetchAndExtractPage downloads one HTML page and turns it into a
// MigrationPage. Errors propagate; the crawler bumps URLsFailed in that case.
func FetchAndExtractPage(ctx context.Context, pageURL string, opts FetchOptions) (MigrationPage, error) {
	if opts.AcceptHeader == "" {
		opts.AcceptHeader = "text/html,application/xhtml+xml,*/*;q=0.8"
	}
	resp, err := SafeFetch(ctx, pageURL, opts)
	if err != nil {
		return MigrationPage{SourceURL: pageURL}, err
	}
	if resp.Status != 200 {
		return MigrationPage{SourceURL: pageURL}, fmt.Errorf("status %d", resp.Status)
	}
	if !strings.Contains(strings.ToLower(resp.ContentType), "html") &&
		!strings.Contains(strings.ToLower(resp.ContentType), "xml") {
		return MigrationPage{SourceURL: pageURL}, fmt.Errorf("unexpected content-type %q", resp.ContentType)
	}

	doc, err := html.Parse(strings.NewReader(string(resp.Body)))
	if err != nil {
		return MigrationPage{SourceURL: pageURL}, fmt.Errorf("html parse: %w", err)
	}
	return extractPage(pageURL, doc), nil
}

// extractPage walks the parsed HTML once, pulling out everything the agent
// or the porter needs. Single-pass keeps it cheap on large sites.
func extractPage(pageURL string, doc *html.Node) MigrationPage {
	page := MigrationPage{SourceURL: pageURL}
	if u, err := url.Parse(pageURL); err == nil {
		page.SourcePath = u.Path
		if page.SourcePath == "" {
			page.SourcePath = "/"
		}
	}
	pageHost := hostOf(pageURL)

	imageSet := map[string]bool{}
	linkSet := map[string]bool{}

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch strings.ToLower(n.Data) {
			case "html":
				if v := attr(n, "lang"); v != "" {
					page.Lang = v
				}
			case "title":
				if page.Title == "" {
					page.Title = strings.TrimSpace(textContent(n))
				}
			case "meta":
				name := strings.ToLower(attr(n, "name"))
				prop := strings.ToLower(attr(n, "property"))
				content := attr(n, "content")
				switch {
				case name == "description":
					page.MetaDescription = content
				case name == "robots":
					if strings.Contains(strings.ToLower(content), "noindex") {
						page.NoIndex = true
					}
				case prop == "og:image":
					page.OGImageURL = absURL(pageURL, content)
				}
			case "link":
				if strings.EqualFold(attr(n, "rel"), "canonical") {
					page.CanonicalURL = absURL(pageURL, attr(n, "href"))
				}
			case "h1":
				if page.H1 == "" {
					page.H1 = strings.TrimSpace(textContent(n))
				}
			case "img":
				if src := absURL(pageURL, attr(n, "src")); src != "" && !imageSet[src] {
					imageSet[src] = true
					page.ImageURLs = append(page.ImageURLs, src)
				}
			case "a":
				href := strings.TrimSpace(attr(n, "href"))
				if href == "" || strings.HasPrefix(href, "#") ||
					strings.HasPrefix(href, "mailto:") || strings.HasPrefix(href, "tel:") ||
					strings.HasPrefix(href, "javascript:") {
					break
				}
				abs := absURL(pageURL, href)
				if abs == "" {
					break
				}
				if hostOf(abs) == pageHost {
					if u, err := url.Parse(abs); err == nil {
						p := u.Path
						if p == "" {
							p = "/"
						}
						if !linkSet[p] {
							linkSet[p] = true
							page.InternalLinks = append(page.InternalLinks, p)
						}
					}
				}
			case "script":
				if strings.EqualFold(attr(n, "type"), "application/ld+json") {
					if t := strings.TrimSpace(textContent(n)); t != "" {
						page.JSONLD = append(page.JSONLD, t)
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return page
}

// ----- HTML node helpers -----

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func textContent(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

func absURL(base, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	bu, err := url.Parse(base)
	if err != nil {
		return ref
	}
	ru, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return bu.ResolveReference(ru).String()
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}
