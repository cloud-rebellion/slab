package figma

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/bright-interaction/slab/internal/builder"
)

// ImportResult is what the handler returns to the client after a successful
// design-tokens import.
type ImportResult struct {
	FileName       string       `json:"file_name"`
	Colors         []ColorToken `json:"colors"`
	Typography     []TextToken  `json:"typography"`
	PrimarySuggest string       `json:"primary_suggest,omitempty"` // hex
	HeadingSuggest string       `json:"heading_suggest,omitempty"` // font family
	BodySuggest    string       `json:"body_suggest,omitempty"`    // font family
	SkippedStyles  []string     `json:"skipped,omitempty"`
}

// ColorToken is one extracted Figma color style ready to seed as a CSS class.
type ColorToken struct {
	Name      string `json:"name"`       // raw Figma name, e.g. "Brand / Primary 600"
	ClassName string `json:"class_name"` // ".brand-primary-600"
	Hex       string `json:"hex"`        // "#1e3a8a"
	Property  string `json:"property"`   // "color" | "background-color"
}

// TextToken is one extracted Figma text style.
type TextToken struct {
	Name       string `json:"name"`
	ClassName  string `json:"class_name"`
	FontFamily string `json:"font_family"`
	FontSize   int    `json:"font_size_px"`
	FontWeight int    `json:"font_weight"`
	LineHeight string `json:"line_height"` // CSS-ready: "1.4" or "24px"
	CSS        string `json:"css"`         // body of the class, e.g. "font-family: Inter; font-size: 18px; ..."
}

// ExtractTokens fetches the file metadata + style nodes and produces
// ImportResult ready for the handler to seed into atomicsite.
func ExtractTokens(ctx context.Context, c *Client, fileKey string) (*ImportResult, error) {
	meta, err := c.FetchFileMeta(ctx, fileKey)
	if err != nil {
		return nil, err
	}

	// Group style refs by type.
	var fillIDs, textIDs []string
	idToRef := map[string]PublishedRef{}
	for nodeID, ref := range meta.Styles {
		idToRef[nodeID] = ref
		switch strings.ToUpper(ref.StyleType) {
		case "FILL":
			fillIDs = append(fillIDs, nodeID)
		case "TEXT":
			textIDs = append(textIDs, nodeID)
		}
	}

	allIDs := append(append([]string{}, fillIDs...), textIDs...)
	nodes, err := c.FetchStyleNodes(ctx, fileKey, allIDs)
	if err != nil {
		return nil, err
	}

	out := &ImportResult{FileName: meta.Name}

	// Colors.
	for _, id := range fillIDs {
		ref, n := idToRef[id], nodes[id]
		if len(n.Document.Fills) == 0 {
			out.SkippedStyles = append(out.SkippedStyles, fmt.Sprintf("color %q: no fills", ref.Name))
			continue
		}
		fill := pickSolidFill(n.Document.Fills)
		if fill == nil {
			out.SkippedStyles = append(out.SkippedStyles, fmt.Sprintf("color %q: only gradient/pattern fills (skipped)", ref.Name))
			continue
		}
		slug := SlugifyName(ref.Name)
		if slug == "" {
			continue
		}
		hex := HexFromRGBA(*fill.Color)
		out.Colors = append(out.Colors, ColorToken{
			Name:      ref.Name,
			ClassName: ".color-" + slug,
			Hex:       hex,
			Property:  "color",
		})
	}

	// Typography.
	for _, id := range textIDs {
		ref, n := idToRef[id], nodes[id]
		if n.Document.Style == nil {
			out.SkippedStyles = append(out.SkippedStyles, fmt.Sprintf("text %q: missing style block", ref.Name))
			continue
		}
		t := *n.Document.Style
		slug := SlugifyName(ref.Name)
		if slug == "" {
			continue
		}
		size := int(t.FontSize)
		if size == 0 {
			size = 16
		}
		weight := int(t.FontWeight)
		if weight == 0 {
			weight = 400
		}
		lineHeight := "1.4"
		if t.LineHeightUnit == "PIXELS" && t.LineHeightPx > 0 {
			lineHeight = fmt.Sprintf("%dpx", int(t.LineHeightPx))
		}
		// builder.CSSFontFamilyDecl allowlists the family name instead of merely
		// quoting it. The name comes from a shared Figma file, i.e. a third
		// party, and this is the helper written for exactly that case.
		cssBody := fmt.Sprintf("font-family: %s; font-size: %dpx; font-weight: %d; line-height: %s;",
			builder.CSSFontFamilyDecl(t.FontFamily), size, weight, lineHeight)
		out.Typography = append(out.Typography, TextToken{
			Name:       ref.Name,
			ClassName:  ".type-" + slug,
			FontFamily: t.FontFamily,
			FontSize:   size,
			FontWeight: weight,
			LineHeight: lineHeight,
			CSS:        cssBody,
		})
	}

	out.PrimarySuggest = pickPrimaryColor(out.Colors)
	out.HeadingSuggest, out.BodySuggest = pickFontPair(out.Typography)

	// Stable order so successive imports diff cleanly.
	sort.Slice(out.Colors, func(i, j int) bool { return out.Colors[i].ClassName < out.Colors[j].ClassName })
	sort.Slice(out.Typography, func(i, j int) bool { return out.Typography[i].ClassName < out.Typography[j].ClassName })

	return out, nil
}

func pickSolidFill(fills []Fill) *Fill {
	for i := range fills {
		f := &fills[i]
		if strings.EqualFold(f.Type, "SOLID") && f.Color != nil {
			if f.Visible != nil && !*f.Visible {
				continue
			}
			return f
		}
	}
	return nil
}

// pickPrimaryColor heuristically picks the brand "primary" by name match
// (e.g. "Primary", "Brand / Primary", "Primary 500"). Falls back to the
// first color in the list.
func pickPrimaryColor(colors []ColorToken) string {
	for _, c := range colors {
		low := strings.ToLower(c.Name)
		if strings.Contains(low, "primary") && !strings.Contains(low, "secondary") {
			return c.Hex
		}
	}
	for _, c := range colors {
		low := strings.ToLower(c.Name)
		if strings.Contains(low, "brand") {
			return c.Hex
		}
	}
	if len(colors) > 0 {
		return colors[0].Hex
	}
	return ""
}

// pickFontPair returns the (heading, body) font families. Heading is the
// largest unique font; body is the most-frequent body-sized text style.
func pickFontPair(tokens []TextToken) (heading, body string) {
	if len(tokens) == 0 {
		return "", ""
	}
	largest := tokens[0]
	for _, t := range tokens {
		if t.FontSize > largest.FontSize {
			largest = t
		}
	}
	heading = largest.FontFamily

	freq := map[string]int{}
	for _, t := range tokens {
		if t.FontSize >= 14 && t.FontSize <= 18 {
			freq[t.FontFamily]++
		}
	}
	bestN := 0
	for fam, n := range freq {
		if n > bestN {
			body = fam
			bestN = n
		}
	}
	if body == "" {
		body = heading
	}
	return heading, body
}
