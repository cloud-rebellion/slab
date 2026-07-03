// Package figma imports design tokens (colors, typography, spacing) from a
// Figma file via the public REST API. The agent / wizard then has real brand
// tokens to compose with instead of guessing.
//
// Scope: tokens only (option 1 from the design memo). We deliberately do NOT
// try to convert frames into components — Figma autolayout to clean Astro is
// a known-bad mapping. The agent treats Figma frames as visual reference
// when they're attached as media; the import here only extracts the tokens.
package figma

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Client is a tiny Figma REST API client.
type Client struct {
	HTTP    *http.Client
	BaseURL string // override for tests; defaults to https://api.figma.com
	Token   string // X-Figma-Token (personal access token)
}

// NewClient returns a Client with the given personal access token.
func NewClient(token string) *Client {
	return &Client{
		HTTP:    &http.Client{Timeout: 15 * time.Second},
		BaseURL: "https://api.figma.com",
		Token:   token,
	}
}

// FileKeyFromURL extracts the file key from a Figma file URL.
//
// Accepts both /file/{key}/{slug} and /design/{key}/{slug} URL shapes.
// Returns an empty string if no key can be parsed.
func FileKeyFromURL(figmaURL string) string {
	u, err := url.Parse(figmaURL)
	if err != nil {
		return ""
	}
	// Path looks like /file/abc123/Name or /design/abc123/Name.
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, p := range parts {
		if (p == "file" || p == "design") && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// File is the trimmed shape of a Figma file response. We only request what
// we need to extract tokens (the full response is multi-MB).
type File struct {
	Name     string                  `json:"name"`
	Document json.RawMessage         `json:"document,omitempty"` // ignored for token import
	Styles   map[string]PublishedRef `json:"styles"`
}

// PublishedRef is the metadata Figma returns for each published style.
type PublishedRef struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	StyleType   string `json:"styleType"` // FILL | TEXT | EFFECT | GRID
	Description string `json:"description"`
}

// FetchFileMeta fetches the file with depth=1 — enough for the published
// styles map without paying for the full document tree.
func (c *Client) FetchFileMeta(ctx context.Context, fileKey string) (*File, error) {
	if c.Token == "" {
		return nil, errors.New("figma: personal access token required")
	}
	u := c.BaseURL + "/v1/files/" + url.PathEscape(fileKey) + "?depth=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Figma-Token", c.Token)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("figma: %d %s: %s", resp.StatusCode, resp.Status, strings.TrimSpace(string(body)))
	}

	var f File
	if err := json.NewDecoder(resp.Body).Decode(&f); err != nil {
		return nil, fmt.Errorf("figma: decode file: %w", err)
	}
	return &f, nil
}

// StyleNode is the trimmed shape returned by /v1/files/{key}/nodes for a
// published style. We pull the nested `style` block for text styles and the
// `fills` array for color styles.
type StyleNode struct {
	Document struct {
		ID    string         `json:"id"`
		Name  string         `json:"name"`
		Type  string         `json:"type"`
		Style *TextStyleData `json:"style,omitempty"`
		Fills []Fill         `json:"fills,omitempty"`
	} `json:"document"`
}

// TextStyleData mirrors the Figma TypeStyle shape.
type TextStyleData struct {
	FontFamily     string  `json:"fontFamily"`
	FontPostScript string  `json:"fontPostScriptName"`
	FontWeight     float64 `json:"fontWeight"`
	FontSize       float64 `json:"fontSize"`
	LetterSpacing  float64 `json:"letterSpacing"`
	LineHeightPx   float64 `json:"lineHeightPx"`
	LineHeightUnit string  `json:"lineHeightUnit"`
	TextDecoration string  `json:"textDecoration"`
	TextCase       string  `json:"textCase"`
}

// Fill is a Figma SolidColor / Gradient entry. We only handle SolidColor.
type Fill struct {
	Type    string `json:"type"`
	Color   *RGBA  `json:"color,omitempty"`
	Visible *bool  `json:"visible,omitempty"`
}

// RGBA is Figma's float-channel color.
type RGBA struct {
	R float64 `json:"r"`
	G float64 `json:"g"`
	B float64 `json:"b"`
	A float64 `json:"a"`
}

// FetchStyleNodes fetches the full node data for a batch of style node IDs.
// Figma allows up to 50 IDs per request; we batch into chunks of 50.
func (c *Client) FetchStyleNodes(ctx context.Context, fileKey string, nodeIDs []string) (map[string]StyleNode, error) {
	if len(nodeIDs) == 0 {
		return map[string]StyleNode{}, nil
	}
	out := map[string]StyleNode{}
	for i := 0; i < len(nodeIDs); i += 50 {
		end := i + 50
		if end > len(nodeIDs) {
			end = len(nodeIDs)
		}
		batch, err := c.fetchStyleNodesBatch(ctx, fileKey, nodeIDs[i:end])
		if err != nil {
			return nil, err
		}
		for k, v := range batch {
			out[k] = v
		}
	}
	return out, nil
}

func (c *Client) fetchStyleNodesBatch(ctx context.Context, fileKey string, ids []string) (map[string]StyleNode, error) {
	u := c.BaseURL + "/v1/files/" + url.PathEscape(fileKey) + "/nodes?ids=" + url.QueryEscape(strings.Join(ids, ","))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Figma-Token", c.Token)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("figma: %d %s: %s", resp.StatusCode, resp.Status, strings.TrimSpace(string(body)))
	}

	var wrap struct {
		Nodes map[string]StyleNode `json:"nodes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrap); err != nil {
		return nil, fmt.Errorf("figma: decode nodes: %w", err)
	}
	return wrap.Nodes, nil
}

// HexFromRGBA renders Figma's float-channel color as a #RRGGBB string. The
// alpha channel is ignored (atomicsite stores opaque colors only at the
// site-settings level — gradients/transparency live in CSS classes).
func HexFromRGBA(c RGBA) string {
	to := func(f float64) int {
		if f < 0 {
			f = 0
		}
		if f > 1 {
			f = 1
		}
		return int(f*255 + 0.5)
	}
	return fmt.Sprintf("#%02x%02x%02x", to(c.R), to(c.G), to(c.B))
}

// SlugifyName converts a Figma style name like "Brand / Primary 600" into a
// URL-safe slug like "brand-primary-600". Used as the CSS class suffix.
var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func SlugifyName(name string) string {
	low := strings.ToLower(strings.TrimSpace(name))
	low = slugRe.ReplaceAllString(low, "-")
	return strings.Trim(low, "-")
}
