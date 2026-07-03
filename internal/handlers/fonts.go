// Self-hosted woff2 font uploads (Phase 12.8). Storage path:
//   {DataDir}/fonts/{site_id}/{id}.woff2
// Public serving path on the built site is /_atomicsite/fonts/{id}.woff2,
// emitted as @font-face by the Astro Layout generator.

package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/bright-interaction/slab/internal/config"
	"github.com/bright-interaction/slab/internal/store"
)

// woff2Magic is the signature at offset 0 of every woff2 file.
// We deliberately reject woff (older format) since woff2 is universally
// supported in modern browsers and compresses ~30% better.
var woff2Magic = []byte{'w', 'O', 'F', '2'}

// fontUploadMaxBytes caps a single font upload at 2 MB. Real-world woff2
// subsets land around 30-200 KB; anything larger is either an unsubsetted
// upload (waste) or malicious.
const fontUploadMaxBytes = 2 << 20

// validFamilyName accepts letters / digits / spaces / hyphens / underscores
// up to 64 chars. Stricter than CSS allows so the dropdown stays clean and
// the @font-face emission can't break out of quotes.
var validFamilyName = regexp.MustCompile(`^[A-Za-z0-9 _-]{1,64}$`)

// validSubsets is the allowlist of Unicode-range subset names accepted at
// upload time. Maps to a deterministic unicode-range descriptor in
// builder.layouts so the browser only fetches the woff2 when the page
// actually contains characters in the declared range. The "all" value
// emits no unicode-range so the font loads on every page.
var validSubsets = map[string]struct{}{
	"latin":      {},
	"latin-ext":  {},
	"cyrillic":   {},
	"greek":      {},
	"vietnamese": {},
	"arabic":     {},
	"hebrew":     {},
	"all":        {},
}

// normalizeSubset coerces an operator-supplied subset string into one of the
// allowed values; empty or unknown falls back to "latin". Default matches the
// EU/UK target market where Latin-only is the common case.
func normalizeSubset(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if _, ok := validSubsets[s]; ok {
		return s
	}
	return "latin"
}

type FontsHandler struct {
	cfg     *config.Config
	queries *store.Queries
}

func NewFontsHandler(cfg *config.Config, queries *store.Queries) *FontsHandler {
	return &FontsHandler{cfg: cfg, queries: queries}
}

// List returns every uploaded font for the site.
// GET /api/sites/{siteID}/fonts
func (h *FontsHandler) List(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	rows, err := h.queries.ListSiteFonts(r.Context(), siteID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list fonts")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"fonts": rows})
}

// Upload accepts a multipart/form-data with fields:
//
//	file:        the woff2 binary
//	family_name: human-readable family ("Acme Sans")
//	weight:      100..900 (default 400)
//	style:       "normal" | "italic" (default "normal")
//
// POST /api/sites/{siteID}/fonts
func (h *FontsHandler) Upload(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	if _, err := h.queries.GetSiteByID(r.Context(), siteID); err != nil {
		writeError(w, http.StatusNotFound, "Site not found")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, fontUploadMaxBytes+8192) // headroom for form fields
	if err := r.ParseMultipartForm(fontUploadMaxBytes); err != nil {
		writeError(w, http.StatusBadRequest, "Upload too large or malformed multipart body")
		return
	}

	familyName := strings.TrimSpace(r.FormValue("family_name"))
	if !validFamilyName.MatchString(familyName) {
		writeError(w, http.StatusBadRequest, "family_name must be 1 to 64 chars: letters, digits, spaces, hyphen, underscore")
		return
	}

	weight := 400
	if v := strings.TrimSpace(r.FormValue("weight")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 100 && n <= 900 {
			weight = n
		}
	}
	style := strings.TrimSpace(r.FormValue("style"))
	if style != "italic" {
		style = "normal"
	}
	subset := normalizeSubset(r.FormValue("subset"))

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "file field required")
		return
	}
	defer file.Close()

	// Read entire file into memory (capped at 2 MB by MaxBytesReader).
	body, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Failed to read uploaded file")
		return
	}
	if len(body) < 4 || !startsWith(body, woff2Magic) {
		writeError(w, http.StatusBadRequest, "Only woff2 fonts are accepted (signature 'wOF2' at offset 0)")
		return
	}

	id := newFontID()
	siteFontsDir := filepath.Join(h.cfg.FontsDir, siteID)
	if err := os.MkdirAll(siteFontsDir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to prepare fonts directory")
		return
	}
	outPath := filepath.Join(siteFontsDir, id+".woff2")
	if err := os.WriteFile(outPath, body, 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to write font file")
		return
	}

	if err := h.queries.CreateSiteFont(r.Context(), store.CreateSiteFontParams{
		ID:           id,
		SiteID:       siteID,
		FamilyName:   familyName,
		Weight:       int64(weight),
		Style:        style,
		FilePath:     outPath,
		FileSize:     int64(len(body)),
		OriginalName: cleanFilename(header.Filename),
		Subset:       subset,
	}); err != nil {
		// Roll back the file on insert failure so we don't leak orphans.
		_ = os.Remove(outPath)
		if strings.Contains(err.Error(), "UNIQUE") {
			writeError(w, http.StatusConflict, "A font with this family/weight/style already exists for this site")
			return
		}
		writeError(w, http.StatusInternalServerError, "Failed to record font")
		return
	}

	row, _ := h.queries.GetSiteFont(r.Context(), store.GetSiteFontParams{ID: id, SiteID: siteID})
	writeJSON(w, http.StatusCreated, row)
}

// Delete removes a font row and its on-disk file.
// DELETE /api/sites/{siteID}/fonts/{fontID}
func (h *FontsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	fontID := urlParam(r, "fontID")
	row, err := h.queries.GetSiteFont(r.Context(), store.GetSiteFontParams{ID: fontID, SiteID: siteID})
	if err != nil {
		writeError(w, http.StatusNotFound, "Font not found")
		return
	}
	if err := h.queries.DeleteSiteFont(r.Context(), store.DeleteSiteFontParams{ID: fontID, SiteID: siteID}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete font row")
		return
	}
	if row.FilePath != "" {
		_ = os.Remove(row.FilePath)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// Serve streams a font file at its public URL. Long cache because the
// content addresses by id (immutable per upload).
// GET /atomicsite-fonts/{siteID}/{fontID}.woff2
func (h *FontsHandler) Serve(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	fontID := urlParam(r, "fontID")
	row, err := h.queries.GetSiteFont(r.Context(), store.GetSiteFontParams{ID: fontID, SiteID: siteID})
	if err != nil {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(row.FilePath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "font/woff2")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Access-Control-Allow-Origin", "*") // allow built sites to fetch cross-origin
	if _, err := io.Copy(w, f); err != nil {
		// Best-effort streaming; the client will see a truncated response.
		return
	}
}

func startsWith(haystack, needle []byte) bool {
	if len(haystack) < len(needle) {
		return false
	}
	for i, b := range needle {
		if haystack[i] != b {
			return false
		}
	}
	return true
}

func cleanFilename(s string) string {
	s = filepath.Base(s)
	if len(s) > 128 {
		s = s[:128]
	}
	return s
}

func newFontID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// EmitFontFace returns a CSS @font-face block for a stored font, suitable
// for inlining into the Astro Layout. The src points at the public font
// URL served by Serve(). Used by the builder.
func (h *FontsHandler) EmitFontFace(siteID string, row store.SiteFont) string {
	src := fmt.Sprintf("/atomicsite-fonts/%s/%s.woff2", siteID, row.ID)
	return fmt.Sprintf(
		"@font-face { font-family: %q; font-style: %s; font-weight: %d; font-display: swap; src: url(%q) format('woff2'); }\n",
		row.FamilyName, row.Style, row.Weight, src,
	)
}
