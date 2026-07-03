package handlers

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/bright-interaction/slab/internal/config"
	"github.com/bright-interaction/slab/internal/imaging"
	authmw "github.com/bright-interaction/slab/internal/middleware"
	"github.com/bright-interaction/slab/internal/storage"
	"github.com/bright-interaction/slab/internal/store"
)

// MediaHandler handles media CRUD + upload + public serving.
type MediaHandler struct {
	cfg     *config.Config
	queries *store.Queries
	store   storage.Store
	// quota gates uploads against the site's storage quota.
	// Sprint 3 (2026-05-04). Optional , nil keeps tests + legacy
	// callers working without quota enforcement.
	quota *QuotaHandler
}

func NewMediaHandler(cfg *config.Config, queries *store.Queries, st storage.Store, quota *QuotaHandler) *MediaHandler {
	return &MediaHandler{cfg: cfg, queries: queries, store: st, quota: quota}
}

// Extensions rejected outright (mirrors brightcrm's list, adapted for image-only).
var dangerousExts = map[string]bool{
	".html": true, ".xhtml": true, ".htm": true,
	".js": true, ".mjs": true, ".php": true, ".phtml": true,
	".svg": true, // XSS risk without sanitization
	".exe": true, ".dll": true, ".sh": true, ".bat": true,
	".jsp": true, ".asp": true, ".aspx": true,
}

// Folder name validator. Lowercase letters, digits, and hyphens. Reserved
// 'brand' is the only system name today; other reserved-looking names are
// fine since the lookup is per-site. Empty string means "unfiled".
var folderNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

const (
	systemFolderBrand   = "brand"
	folderFilterAll     = ""
	folderFilterUnfiled = "__unfiled"
)

func validFolderName(name string) bool {
	return folderNameRE.MatchString(name)
}

// ---------------- Admin endpoints ----------------

// List returns paginated media for a site. Optional ?folder= query param:
// empty (or absent) = all folders flat; "__unfiled" = items with no folder
// assigned; any other value = items in that named folder.
func (h *MediaHandler) List(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	limit := parseIntDefault(r.URL.Query().Get("limit"), 50)
	offset := parseIntDefault(r.URL.Query().Get("offset"), 0)
	folder := r.URL.Query().Get("folder")

	var (
		rows  []store.Medium
		count int64
		err   error
	)
	switch folder {
	case folderFilterAll:
		rows, err = h.queries.ListMediaBySitePaginated(r.Context(), store.ListMediaBySitePaginatedParams{
			SiteID: siteID,
			Limit:  int64(limit),
			Offset: int64(offset),
		})
		if err == nil {
			count, _ = h.queries.CountMediaBySite(r.Context(), siteID)
		}
	case folderFilterUnfiled:
		rows, err = h.queries.ListUnfiledMediaPaginated(r.Context(), store.ListUnfiledMediaPaginatedParams{
			SiteID: siteID,
			Limit:  int64(limit),
			Offset: int64(offset),
		})
		if err == nil {
			count, _ = h.queries.CountUnfiledMedia(r.Context(), siteID)
		}
	default:
		if !validFolderName(folder) {
			writeError(w, http.StatusBadRequest, "Invalid folder name")
			return
		}
		rows, err = h.queries.ListMediaInFolderPaginated(r.Context(), store.ListMediaInFolderPaginatedParams{
			SiteID: siteID,
			Folder: folder,
			Limit:  int64(limit),
			Offset: int64(offset),
		})
		if err == nil {
			count, _ = h.queries.CountMediaInFolder(r.Context(), store.CountMediaInFolderParams{
				SiteID: siteID,
				Folder: folder,
			})
		}
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list media")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"items":  rows,
		"total":  count,
		"limit":  limit,
		"offset": offset,
		"folder": folder,
	})
}

// ---------------- Folders ----------------

// ListFolders returns all folders for a site with item counts. The system
// 'brand' folder is auto-seeded and always present in the response.
func (h *MediaHandler) ListFolders(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	if err := h.queries.EnsureMediaFolder(r.Context(), store.EnsureMediaFolderParams{
		SiteID:   siteID,
		Name:     systemFolderBrand,
		IsSystem: 1,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to seed system folder")
		return
	}
	folders, err := h.queries.ListMediaFoldersBySite(r.Context(), siteID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list folders")
		return
	}
	type folderOut struct {
		Name      string `json:"name"`
		IsSystem  bool   `json:"is_system"`
		CreatedAt string `json:"created_at"`
		ItemCount int64  `json:"item_count"`
	}
	out := make([]folderOut, 0, len(folders))
	for _, f := range folders {
		c, _ := h.queries.CountMediaInFolder(r.Context(), store.CountMediaInFolderParams{
			SiteID: siteID,
			Folder: f.Name,
		})
		out = append(out, folderOut{
			Name:      f.Name,
			IsSystem:  f.IsSystem == 1,
			CreatedAt: f.CreatedAt,
			ItemCount: c,
		})
	}
	totalCount, _ := h.queries.CountMediaBySite(r.Context(), siteID)
	unfiledCount, _ := h.queries.CountUnfiledMedia(r.Context(), siteID)
	writeJSON(w, http.StatusOK, map[string]any{
		"folders":       out,
		"total_count":   totalCount,
		"unfiled_count": unfiledCount,
	})
}

// CreateFolder pre-creates an empty user folder (so it shows up in the
// sidebar before any item is moved into it).
func (h *MediaHandler) CreateFolder(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	var req struct {
		Name string `json:"name"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	name := strings.ToLower(strings.TrimSpace(req.Name))
	if !validFolderName(name) {
		writeError(w, http.StatusBadRequest, "Folder name must be 1-32 chars, lowercase letters/digits/hyphens, starting with letter or digit")
		return
	}
	if name == systemFolderBrand {
		writeError(w, http.StatusBadRequest, "'brand' is reserved")
		return
	}
	if err := h.queries.EnsureMediaFolder(r.Context(), store.EnsureMediaFolderParams{
		SiteID:   siteID,
		Name:     name,
		IsSystem: 0,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create folder")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"name":      name,
		"is_system": false,
	})
}

// DeleteFolder removes a user folder. System folders are protected. Items
// in the folder are moved back to unfiled (folder = ”).
func (h *MediaHandler) DeleteFolder(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	name := urlParam(r, "folderName")
	if !validFolderName(name) {
		writeError(w, http.StatusBadRequest, "Invalid folder name")
		return
	}
	folder, err := h.queries.GetMediaFolder(r.Context(), store.GetMediaFolderParams{
		SiteID: siteID,
		Name:   name,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "Folder not found")
		return
	}
	if folder.IsSystem == 1 {
		writeError(w, http.StatusBadRequest, "Cannot delete a system folder")
		return
	}
	if err := h.queries.ClearMediaFolder(r.Context(), store.ClearMediaFolderParams{
		SiteID: siteID,
		Folder: name,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to clear folder items")
		return
	}
	if err := h.queries.DeleteMediaFolder(r.Context(), store.DeleteMediaFolderParams{
		SiteID: siteID,
		Name:   name,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete folder")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// Get returns one media row.
func (h *MediaHandler) Get(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	id := urlParam(r, "mediaID")
	m, err := h.queries.GetMediaByID(r.Context(), id)
	if err != nil || m.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Media not found")
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// Update edits alt_text and optionally folder. Both fields are optional in
// the request body; presence is detected via pointer types so a caller can
// patch only one without clobbering the other.
func (h *MediaHandler) Update(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	id := urlParam(r, "mediaID")
	m, err := h.queries.GetMediaByID(r.Context(), id)
	if err != nil || m.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Media not found")
		return
	}
	var req struct {
		AltText *string `json:"alt_text,omitempty"`
		Folder  *string `json:"folder,omitempty"`
		FocalX  *int64  `json:"focal_x,omitempty"`
		FocalY  *int64  `json:"focal_y,omitempty"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	if req.AltText != nil {
		if err := h.queries.UpdateMedia(r.Context(), store.UpdateMediaParams{
			AltText: *req.AltText,
			ID:      id,
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to update media")
			return
		}
	}
	if req.Folder != nil {
		if status, msg := h.applyFolderChange(r.Context(), m.SiteID, id, *req.Folder); status != http.StatusOK {
			writeError(w, status, msg)
			return
		}
	}
	// Sprint 5 quick-win (2026-05-24): per-image focal point as
	// percentages of width/height. Accepted only when both are
	// supplied; either omitted keeps the existing value. Range
	// clamped to [0,100] so a sloppy slider can't poison the CSS.
	if req.FocalX != nil || req.FocalY != nil {
		x := m.FocalX
		y := m.FocalY
		if req.FocalX != nil {
			x = clampPercent(*req.FocalX)
		}
		if req.FocalY != nil {
			y = clampPercent(*req.FocalY)
		}
		if err := h.queries.UpdateMediaFocalPoint(r.Context(), store.UpdateMediaFocalPointParams{
			FocalX: x,
			FocalY: y,
			ID:     id,
			SiteID: siteID,
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to update focal point")
			return
		}
	}
	out, _ := h.queries.GetMediaByID(r.Context(), id)
	writeJSON(w, http.StatusOK, out)
}

func clampPercent(n int64) int64 {
	if n < 0 {
		return 0
	}
	if n > 100 {
		return 100
	}
	return n
}

// applyFolderChange validates and applies a folder change on a single media
// row. Empty string moves the item to unfiled. Otherwise the folder must
// match the validator and exist (or be auto-created).
func (h *MediaHandler) applyFolderChange(ctx context.Context, siteID, mediaID, folder string) (int, string) {
	folder = strings.ToLower(strings.TrimSpace(folder))
	if folder != "" {
		if !validFolderName(folder) {
			return http.StatusBadRequest, "Invalid folder name"
		}
		if err := h.queries.EnsureMediaFolder(ctx, store.EnsureMediaFolderParams{
			SiteID:   siteID,
			Name:     folder,
			IsSystem: 0,
		}); err != nil {
			return http.StatusInternalServerError, "Failed to ensure folder"
		}
	}
	if err := h.queries.UpdateMediaFolder(ctx, store.UpdateMediaFolderParams{
		Folder: folder,
		ID:     mediaID,
	}); err != nil {
		return http.StatusInternalServerError, "Failed to update folder"
	}
	return http.StatusOK, ""
}

// Delete removes media from DB and disk.
func (h *MediaHandler) Delete(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	id := urlParam(r, "mediaID")
	m, err := h.queries.GetMediaByID(r.Context(), id)
	if err != nil || m.SiteID != siteID {
		writeError(w, http.StatusNotFound, "Media not found")
		return
	}
	// Wipe entire {siteID}/{mediaID}/ dir on disk
	prefix := filepath.Join(m.SiteID, id)
	if err := h.store.DeleteDir(r.Context(), prefix); err != nil {
		slog.Warn("delete media dir failed", "prefix", prefix, "err", err)
	}
	if err := h.queries.DeleteMedia(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete media")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// Upload handles multipart upload from admin UI.
func (h *MediaHandler) Upload(w http.ResponseWriter, r *http.Request) {
	siteID := urlParam(r, "siteID")
	h.uploadMultipart(w, r, siteID)
}

// resolveUploadFolder takes a raw form value, normalises it, and ensures the
// folder row exists. Empty string -> unfiled. Returns the canonical folder
// name to persist on the new media row, or HTTP error info on validation
// failure.
func (h *MediaHandler) resolveUploadFolder(ctx context.Context, siteID, raw string) (string, int, string) {
	folder := strings.ToLower(strings.TrimSpace(raw))
	if folder == "" {
		return "", http.StatusOK, ""
	}
	if !validFolderName(folder) {
		return "", http.StatusBadRequest, "Invalid folder name"
	}
	isSystem := int64(0)
	if folder == systemFolderBrand {
		isSystem = 1
	}
	if err := h.queries.EnsureMediaFolder(ctx, store.EnsureMediaFolderParams{
		SiteID:   siteID,
		Name:     folder,
		IsSystem: isSystem,
	}); err != nil {
		return "", http.StatusInternalServerError, "Failed to ensure folder"
	}
	return folder, http.StatusOK, ""
}

// ---------------- Agent endpoints ----------------

// AgentUpload: multipart upload via agent API key.
func (h *MediaHandler) AgentUpload(w http.ResponseWriter, r *http.Request) {
	a := authmw.GetAgent(r)
	if a == nil {
		writeError(w, http.StatusUnauthorized, "Agent not authenticated")
		return
	}
	if !requireWrite(w, a) {
		return
	}
	h.uploadMultipart(w, r, a.SiteID)
}

// AgentUploadFromBase64: for AI agents that can emit base64 but not multipart.
func (h *MediaHandler) AgentUploadFromBase64(w http.ResponseWriter, r *http.Request) {
	a := authmw.GetAgent(r)
	if a == nil {
		writeError(w, http.StatusUnauthorized, "Agent not authenticated")
		return
	}
	if !requireWrite(w, a) {
		return
	}
	var req struct {
		Data     string `json:"data"`
		MimeType string `json:"mime_type"`
		Filename string `json:"filename"`
		AltText  string `json:"alt_text"`
		Folder   string `json:"folder"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	if req.Data == "" || req.Filename == "" {
		writeError(w, http.StatusBadRequest, "data and filename are required")
		return
	}
	// Accept both "data:image/jpeg;base64,..." and raw base64
	data := req.Data
	if idx := strings.Index(data, "base64,"); idx >= 0 {
		data = data[idx+len("base64,"):]
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid base64 data")
		return
	}
	if int64(len(raw)) > h.cfg.MaxUploadSize {
		writeError(w, http.StatusRequestEntityTooLarge, "File too large")
		return
	}
	if h.quota != nil && h.quota.EnforceStorageQuota(w, r, a.SiteID, int64(len(raw))) {
		return
	}
	folder, status, msg := h.resolveUploadFolder(r.Context(), a.SiteID, req.Folder)
	if status != http.StatusOK {
		writeError(w, status, msg)
		return
	}
	media, status, msg := h.processAndSave(r.Context(), a.SiteID, req.Filename, req.AltText, folder, raw)
	if status != http.StatusCreated {
		writeError(w, status, msg)
		return
	}
	writeJSON(w, http.StatusCreated, media)
}

// AgentUploadFromURL: server fetches a URL and processes. SSRF-guarded.
func (h *MediaHandler) AgentUploadFromURL(w http.ResponseWriter, r *http.Request) {
	a := authmw.GetAgent(r)
	if a == nil {
		writeError(w, http.StatusUnauthorized, "Agent not authenticated")
		return
	}
	if !requireWrite(w, a) {
		return
	}
	var req struct {
		URL      string `json:"url"`
		Filename string `json:"filename"`
		AltText  string `json:"alt_text"`
		Folder   string `json:"folder"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	if req.URL == "" {
		writeError(w, http.StatusBadRequest, "url is required")
		return
	}
	raw, finalFilename, err := fetchWithSSRFGuard(r.Context(), req.URL, h.cfg.MaxUploadSize)
	if err != nil {
		slog.Warn("agent upload from-url rejected", "url", req.URL, "err", err)
		writeError(w, http.StatusBadRequest, "Failed to fetch URL (rejected or unreachable)")
		return
	}
	if h.quota != nil && h.quota.EnforceStorageQuota(w, r, a.SiteID, int64(len(raw))) {
		return
	}
	filename := req.Filename
	if filename == "" {
		filename = finalFilename
	}
	if filename == "" {
		filename = "image"
	}
	folder, status, msg := h.resolveUploadFolder(r.Context(), a.SiteID, req.Folder)
	if status != http.StatusOK {
		writeError(w, status, msg)
		return
	}
	media, status, msg := h.processAndSave(r.Context(), a.SiteID, filename, req.AltText, folder, raw)
	if status != http.StatusCreated {
		writeError(w, status, msg)
		return
	}
	writeJSON(w, http.StatusCreated, media)
}

// AgentUpdate edits alt_text and/or folder for the agent's site.
func (h *MediaHandler) AgentUpdate(w http.ResponseWriter, r *http.Request) {
	a := authmw.GetAgent(r)
	if a == nil {
		writeError(w, http.StatusUnauthorized, "Agent not authenticated")
		return
	}
	if !requireWrite(w, a) {
		return
	}
	id := urlParam(r, "mediaID")
	m, err := h.queries.GetMediaByID(r.Context(), id)
	if err != nil || m.SiteID != a.SiteID {
		writeError(w, http.StatusNotFound, "Media not found")
		return
	}
	var req struct {
		AltText *string `json:"alt_text,omitempty"`
		Folder  *string `json:"folder,omitempty"`
	}
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	if req.AltText != nil {
		if err := h.queries.UpdateMedia(r.Context(), store.UpdateMediaParams{
			AltText: *req.AltText,
			ID:      id,
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to update media")
			return
		}
	}
	if req.Folder != nil {
		if status, msg := h.applyFolderChange(r.Context(), a.SiteID, id, *req.Folder); status != http.StatusOK {
			writeError(w, status, msg)
			return
		}
	}
	out, _ := h.queries.GetMediaByID(r.Context(), id)
	writeJSON(w, http.StatusOK, out)
}

// AgentDelete: delete media owned by the agent's site.
func (h *MediaHandler) AgentDelete(w http.ResponseWriter, r *http.Request) {
	a := authmw.GetAgent(r)
	if a == nil {
		writeError(w, http.StatusUnauthorized, "Agent not authenticated")
		return
	}
	if !requireWrite(w, a) {
		return
	}
	id := urlParam(r, "mediaID")
	m, err := h.queries.GetMediaByID(r.Context(), id)
	if err != nil || m.SiteID != a.SiteID {
		writeError(w, http.StatusNotFound, "Media not found")
		return
	}
	prefix := filepath.Join(m.SiteID, id)
	if err := h.store.DeleteDir(r.Context(), prefix); err != nil {
		slog.Warn("delete media dir failed", "prefix", prefix, "err", err)
	}
	if err := h.queries.DeleteMedia(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete media")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ---------------- Public file server ----------------

// ServePublic serves processed media files at /media/{siteID}/{mediaID}/{variant}.{ext}.
// Long-cache headers are safe because filenames are content-addressed.
func (h *MediaHandler) ServePublic(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "*")
	if key == "" || strings.Contains(key, "..") {
		writeError(w, http.StatusBadRequest, "Invalid path")
		return
	}
	reader, err := h.store.Get(r.Context(), key)
	if err != nil {
		writeError(w, http.StatusNotFound, "Not found")
		return
	}
	defer reader.Close()

	// Content-Type from extension
	ext := strings.ToLower(filepath.Ext(key))
	ct := extMimeMap[ext]
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.Copy(w, reader)
}

var extMimeMap = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",
	".gif":  "image/gif",
	".avif": "image/avif",
}

// ---------------- Internal helpers ----------------

// uploadMultipart handles the multipart form path (used by both admin and agent).
func (h *MediaHandler) uploadMultipart(w http.ResponseWriter, r *http.Request, siteID string) {
	// Enforce max size at the reader level
	r.Body = http.MaxBytesReader(w, r.Body, h.cfg.MaxUploadSize)

	if err := r.ParseMultipartForm(h.cfg.MaxUploadSize); err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "File too large or invalid form")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "Missing file field")
		return
	}
	defer file.Close()

	raw, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Failed to read upload")
		return
	}
	if int64(len(raw)) > h.cfg.MaxUploadSize {
		writeError(w, http.StatusRequestEntityTooLarge, "File too large")
		return
	}

	// Sprint 3 storage quota gate. Runs after we know the upload's
	// final byte count but before any disk writes happen, so an
	// over-quota tenant can't even start the imaging pipeline.
	if h.quota != nil && h.quota.EnforceStorageQuota(w, r, siteID, int64(len(raw))) {
		return
	}

	altText := r.FormValue("alt_text")
	folder, status, msg := h.resolveUploadFolder(r.Context(), siteID, r.FormValue("folder"))
	if status != http.StatusOK {
		writeError(w, status, msg)
		return
	}
	filename := header.Filename
	if filename == "" {
		filename = "image"
	}

	media, status, msg := h.processAndSave(r.Context(), siteID, filename, altText, folder, raw)
	if status != http.StatusCreated {
		writeError(w, status, msg)
		return
	}
	writeJSON(w, http.StatusCreated, media)
}

// processAndSave validates the raw bytes, runs the imaging pipeline, and writes
// a media row. Returns the created row + HTTP status + error message on failure.
// folder is the canonical folder name already validated by resolveUploadFolder
// (empty = unfiled).
func (h *MediaHandler) processAndSave(ctx context.Context, siteID, filename, altText, folder string, raw []byte) (*store.Medium, int, string) {
	// 1. Sniff MIME and validate
	mime := imaging.Sniff(raw)
	format := imaging.FormatFromMime(mime)
	if format == "" {
		return nil, http.StatusUnsupportedMediaType, "Unsupported image format (must be JPEG/PNG/GIF/WebP)"
	}

	// 2. Filename sanitation + extension blocklist
	safeName := filepath.Base(filename)
	safeName = strings.ReplaceAll(safeName, "\\", "_")
	safeName = strings.ReplaceAll(safeName, "/", "_")
	ext := strings.ToLower(filepath.Ext(safeName))
	if dangerousExts[ext] {
		return nil, http.StatusBadRequest, "File extension not allowed"
	}

	// 3. Generate media ID + run pipeline
	mediaID := newID()
	result, err := imaging.Process(ctx, raw, h.store, siteID, mediaID, format, h.cfg.MediaVariants)
	if err != nil {
		slog.Error("media pipeline failed", "err", err)
		return nil, http.StatusInternalServerError, "Image processing failed"
	}

	// 4. Find original path from variants
	var originalPath string
	for _, v := range result.Variants {
		if strings.Contains(v.Path, "/original.") {
			originalPath = v.Path
			break
		}
	}

	// 5. Persist
	if err := h.queries.CreateMedia(ctx, store.CreateMediaParams{
		ID:           mediaID,
		SiteID:       siteID,
		Filename:     safeName,
		AltText:      altText,
		MimeType:     mime,
		FileSize:     int64(len(raw)),
		Width:        int64(result.OriginalWidth),
		Height:       int64(result.OriginalHeight),
		OriginalPath: originalPath,
		Folder:       folder,
	}); err != nil {
		return nil, http.StatusInternalServerError, "Failed to save media record"
	}
	if err := h.queries.UpdateMediaVariants(ctx, store.UpdateMediaVariantsParams{
		Width:        int64(result.OriginalWidth),
		Height:       int64(result.OriginalHeight),
		Blurhash:     result.Blurhash,
		VariantsJson: marshalField(result.Variants),
		ID:           mediaID,
	}); err != nil {
		return nil, http.StatusInternalServerError, "Failed to save variants"
	}

	m, _ := h.queries.GetMediaByID(ctx, mediaID)
	return &m, http.StatusCreated, ""
}

// fetchWithSSRFGuard downloads a URL with strict SSRF protection.
// Blocks private IP ranges, limits body size, follows up to 3 redirects re-checking each.
func fetchWithSSRFGuard(ctx context.Context, rawURL string, maxSize int64) ([]byte, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", fmt.Errorf("invalid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, "", fmt.Errorf("only http/https URLs allowed")
	}

	// DNS resolve + block private ranges
	host := u.Hostname()
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, "", fmt.Errorf("DNS lookup failed: %s", host)
	}
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return nil, "", fmt.Errorf("private address blocked: %s", ip)
		}
	}

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("too many redirects")
			}
			// Re-check each redirect target for SSRF
			ips, err := net.LookupIP(req.URL.Hostname())
			if err != nil {
				return err
			}
			for _, ip := range ips {
				if isPrivateIP(ip) {
					return fmt.Errorf("redirect to private address blocked")
				}
			}
			return nil
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("fetch returned status %d", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, maxSize+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, "", err
	}
	if int64(len(raw)) > maxSize {
		return nil, "", fmt.Errorf("response exceeds max size")
	}

	// Extract filename from URL path
	filename := filepath.Base(u.Path)
	if filename == "" || filename == "/" || filename == "." {
		filename = "image"
	}
	return raw, filename, nil
}

func isPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	// IsPrivate covers RFC1918 and IPv6 ULA
	if ip.IsPrivate() {
		return true
	}
	return false
}

func parseIntDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// Unused but keep for future: bytes.Reader for io-based processing.
var _ = bytes.NewReader
