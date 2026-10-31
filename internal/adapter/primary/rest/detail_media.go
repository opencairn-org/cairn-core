package rest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/activityexport"
	"github.com/opencairn-org/cairn-core/internal/usecase/attachment"
	"github.com/opencairn-org/cairn-core/internal/usecase/content"
	"github.com/opencairn-org/cairn-core/internal/usecase/mapimage"
	"github.com/opencairn-org/cairn-core/internal/usecase/sharing"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// detailMapCacheControl: short private TTL + ETag revalidation, so a
// regenerated snapshot shows on the next page load.
const detailMapCacheControl = "private, max-age=300"

// detailServePNG writes a course image with its ETag, honouring If-None-Match.
func detailServePNG(w http.ResponseWriter, r *http.Request, data []byte) {
	sum := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", detailMapCacheControl)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(data) //nolint:gosec // G705: rendered PNG bytes served as image/png
}

// detailVisible loads the {id} activity when the caller (or the ?share= token)
// may see cat of it; owners always may. On failure it has written 400 or 404.
func detailVisible(w http.ResponseWriter, r *http.Request, auth *Auth, access *view.ActivityAccess, shares *sharing.Links,
	cat domain.Category, badID string) (domain.Activity, view.Viewer, bool) {
	id, err := domain.ParseUUID[domain.ActivityID](r.PathValue("id"))
	if err != nil {
		http.Error(w, badID, http.StatusBadRequest)
		return domain.Activity{}, view.Viewer{}, false
	}
	hasLink := shares.Grants(r.Context(), r.URL.Query().Get("share"), id)
	act, vw, err := access.Visible(r.Context(), id, auth.Viewer(r), hasLink, cat)
	if err != nil {
		http.NotFound(w, r)
		return domain.Activity{}, view.Viewer{}, false
	}
	return act, vw, true
}

// MapImageHandlers serve the static PNG of an activity's course. The owner
// gets the full track; a non-owner gets it only when the map category is
// granted, privacy-zone-trimmed.
//
//	GET  /api/activities/{id}/map.png
//	POST /api/activities/{id}/map/regenerate   owner: drop the cache and re-render
type MapImageHandlers struct {
	Auth   *Auth
	Access *view.ActivityAccess
	Shares *sharing.Links
	Images *mapimage.Images
}

// Mount registers the routes.
func (h *MapImageHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/activities/{id}/map.png", h.serve)
	mux.HandleFunc("POST /api/activities/{id}/map/regenerate", h.regenerate)
}

func (h *MapImageHandlers) serve(w http.ResponseWriter, r *http.Request) {
	act, vw, ok := detailVisible(w, r, h.Auth, h.Access, h.Shares, domain.CategoryMap, "bad id")
	if !ok {
		return
	}
	data, ok := h.Images.Load(r.Context(), act, !vw.IsOwner)
	if !ok {
		http.NotFound(w, r) // no renderable GPS track
		return
	}
	detailServePNG(w, r, data)
}

// regenerate is for when the track changed under the cache (re-import,
// source pinned, privacy zone edited) or a render cached nothing.
func (h *MapImageHandlers) regenerate(w http.ResponseWriter, r *http.Request) {
	me, ok := h.Auth.requireUser(w, r)
	if !ok {
		return
	}
	id, err := domain.ParseUUID[domain.ActivityID](r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	act, err := h.Access.Owned(r.Context(), me, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !h.Images.Regenerate(r.Context(), act) {
		writeJSON(w, http.StatusOK, map[string]any{
			"regenerated": false,
			"warnings":    []string{"no renderable GPS track"},
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"regenerated": true})
}

// detailAttachmentMaxUpload caps an attachment file; the body adds headroom
// for multipart framing and caption.
const (
	detailAttachmentMaxUpload = 32 << 20
	detailAttachmentMaxBody   = detailAttachmentMaxUpload + 1<<20
)

// AttachmentHandlers serve activity photos.
//
//	GET    /api/activities/{id}/attachments              list metadata
//	POST   /api/activities/{id}/attachments              upload (multipart "file")
//	GET    /api/activities/{id}/attachments/{aid}/raw    stream the image bytes
//	DELETE /api/activities/{id}/attachments/{aid}        remove
//
// Reads are gated on the photos category for non-owners (like map.png);
// writes are owner-only. Bytes are proxied on /raw so the storage endpoint
// never leaks to the browser.
type AttachmentHandlers struct {
	Auth   *Auth
	Access *view.ActivityAccess
	Shares *sharing.Links
	Photos *attachment.Photos
}

// Mount registers the routes.
func (h *AttachmentHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/activities/{id}/attachments", h.list)
	mux.HandleFunc("GET /api/activities/{id}/attachments/{aid}/raw", h.raw)
	mux.HandleFunc("POST /api/activities/{id}/attachments", h.upload)
	mux.HandleFunc("DELETE /api/activities/{id}/attachments/{aid}", h.remove)
}

func (h *AttachmentHandlers) visible(w http.ResponseWriter, r *http.Request) (domain.Activity, bool, bool) {
	act, vw, ok := detailVisible(w, r, h.Auth, h.Access, h.Shares, domain.CategoryPhotos, "invalid activity id")
	return act, vw.IsOwner, ok
}

// attachmentID parses {aid} or writes 400.
func (h *AttachmentHandlers) attachmentID(w http.ResponseWriter, r *http.Request) (domain.AttachmentID, bool) {
	id, err := domain.ParseUUID[domain.AttachmentID](r.PathValue("aid"))
	if err != nil {
		http.Error(w, "invalid attachment id", http.StatusBadRequest)
		return id, false
	}
	return id, true
}

func (h *AttachmentHandlers) list(w http.ResponseWriter, r *http.Request) {
	act, isOwner, ok := h.visible(w, r)
	if !ok {
		return
	}
	atts, err := h.Photos.List(r.Context(), act.ID)
	if err != nil {
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	// The raw links point back at a link-gated route; visible already checked it.
	share := ""
	if t := r.URL.Query().Get("share"); t != "" {
		share = "?share=" + url.QueryEscape(t)
	}
	out := make([]map[string]any, 0, len(atts))
	for _, a := range atts {
		item := map[string]any{
			"id":           a.ID.String(),
			"url":          fmt.Sprintf("/api/activities/%s/attachments/%s/raw", act.ID.String(), a.ID.String()) + share,
			"caption":      a.Caption,
			"content_type": a.ContentType,
			"width":        a.Width,
			"height":       a.Height,
			"blurhash":     a.Blurhash,
		}
		// Signed per-variant URLs (as in the Connect Attachment message); "url"
		// stays for the web until it migrates.
		if urls := h.Photos.URLs(r.Context(), a, isOwner); len(urls) > 0 {
			signed := make(map[string]any, len(urls))
			for name, u := range urls {
				signed[name] = map[string]any{"url": u.URL, "expires_at": u.ExpiresAt.UTC().Format(time.RFC3339)}
			}
			item["urls"] = signed
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"attachments": out})
}

func (h *AttachmentHandlers) raw(w http.ResponseWriter, r *http.Request) {
	act, isOwner, ok := h.visible(w, r)
	if !ok {
		return
	}
	if !h.Photos.HasStorage() {
		http.Error(w, "storage not configured", http.StatusServiceUnavailable)
		return
	}
	attID, ok := h.attachmentID(w, r)
	if !ok {
		return
	}
	att, err := h.Photos.Get(r.Context(), act.ID, attID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	data, ct, err := h.Photos.Bytes(r.Context(), att, isOwner)
	if errors.Is(err, domain.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeActionError(w, err, "blob unavailable")
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = w.Write(data) //nolint:gosec // G705: image bytes served under an image content type
}

// readUpload parses the multipart body: the image bytes and sniffed type.
func (h *AttachmentHandlers) readUpload(w http.ResponseWriter, r *http.Request) ([]byte, string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, detailAttachmentMaxBody)
	if err := r.ParseMultipartForm(detailAttachmentMaxUpload); err != nil { //nolint:gosec // G120: body capped by MaxBytesReader above
		http.Error(w, "bad multipart form", http.StatusBadRequest)
		return nil, "", false
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file field", http.StatusBadRequest)
		return nil, "", false
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, detailAttachmentMaxUpload))
	if err != nil {
		http.Error(w, "read file failed", http.StatusBadRequest)
		return nil, "", false
	}
	ct := http.DetectContentType(raw)
	if !attachment.IsImageContentType(ct) {
		http.Error(w, "only image uploads are supported", http.StatusBadRequest)
		return nil, "", false
	}
	return raw, ct, true
}

func (h *AttachmentHandlers) upload(w http.ResponseWriter, r *http.Request) {
	act, ok := detailOwned(w, r, h.Auth, h.Access, "invalid activity id")
	if !ok {
		return
	}
	if !h.Photos.HasStorage() {
		http.Error(w, "storage not configured", http.StatusServiceUnavailable)
		return
	}
	raw, ct, ok := h.readUpload(w, r)
	if !ok {
		return
	}
	if err := h.Photos.Upload(r.Context(), act.UserID, act.ID, raw, ct, r.FormValue("caption")); err != nil {
		writeActionError(w, err, "save failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *AttachmentHandlers) remove(w http.ResponseWriter, r *http.Request) {
	act, ok := detailOwned(w, r, h.Auth, h.Access, "invalid activity id")
	if !ok {
		return
	}
	attID, ok := h.attachmentID(w, r)
	if !ok {
		return
	}
	if err := h.Photos.Remove(r.Context(), act.ID, attID); err != nil {
		writeActionError(w, err, "delete failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ContentHandlers serve the signed content route; the token is the whole
// authorization.
//
//	GET /content/{token}   stored object | course image (rendered on demand) | export (rendered on GET)
//	PUT /content/{token}   client file upload into the blob store
type ContentHandlers struct {
	Signer  *content.Signer
	Access  *view.ActivityAccess
	Images  *mapimage.Images
	Exports *activityexport.Exports
	Log     *slog.Logger
}

// Mount registers the routes.
func (h *ContentHandlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /content/{token}", h.get)
	mux.HandleFunc("PUT /content/{token}", h.put)
}

// claims verifies the token and its kind; every failure is a bare 404.
func (h *ContentHandlers) claims(w http.ResponseWriter, r *http.Request, kinds ...string) (content.Claims, bool) {
	cl, err := h.Signer.Codec.Decode(r.PathValue("token"))
	if err == nil {
		for _, k := range kinds {
			if cl.Kind == k {
				return cl, true
			}
		}
	}
	http.NotFound(w, r)
	return content.Claims{}, false
}

// cacheHeaders: private (the URL is per viewer) for the token's remaining life.
func (h *ContentHandlers) cacheHeaders(w http.ResponseWriter, cl content.Claims) {
	w.Header().Set("Cache-Control", "private, max-age="+strconv.FormatInt(h.Signer.Codec.Remaining(cl), 10))
}

func (h *ContentHandlers) get(w http.ResponseWriter, r *http.Request) {
	cl, ok := h.claims(w, r, content.KindBlob, content.KindMap, content.KindExport)
	if !ok {
		return
	}
	switch cl.Kind {
	case content.KindBlob:
		h.serveBlob(w, r, cl)
	case content.KindMap:
		h.serveMap(w, r, cl)
	case content.KindExport:
		h.serveExport(w, r, cl)
	}
}

func (h *ContentHandlers) serveBlob(w http.ResponseWriter, r *http.Request, cl content.Claims) {
	if !h.Signer.HasStorage() {
		http.Error(w, "storage not configured", http.StatusServiceUnavailable)
		return
	}
	data, ct, err := h.Signer.Read(r.Context(), cl)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	if cl.Filename != "" {
		w.Header().Set("Content-Disposition", "attachment; filename=\""+cl.Filename+"\"")
	}
	h.cacheHeaders(w, cl)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data) //nolint:gosec // G705: stored object bytes served under their own content type
}

// live is the token's activity unless it is gone.
func (h *ContentHandlers) live(w http.ResponseWriter, r *http.Request, cl content.Claims) (domain.Activity, bool) {
	id, err := domain.ParseUUID[domain.ActivityID](cl.ActivityID)
	if err != nil {
		http.NotFound(w, r)
		return domain.Activity{}, false
	}
	act, err := h.Access.Live(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return domain.Activity{}, false
	}
	return act, true
}

func (h *ContentHandlers) serveMap(w http.ResponseWriter, r *http.Request, cl content.Claims) {
	act, ok := h.live(w, r, cl)
	if !ok {
		return
	}
	data, ok := h.Images.Load(r.Context(), act, cl.Trimmed)
	if !ok {
		http.NotFound(w, r)
		return
	}
	detailServePNG(w, r, data)
}

func (h *ContentHandlers) serveExport(w http.ResponseWriter, r *http.Request, cl content.Claims) {
	act, ok := h.live(w, r, cl)
	if !ok {
		return
	}
	file, err := h.Exports.Render(r.Context(), act, cl.Format, cl.Exclude)
	if err != nil {
		writeActionError(w, err, "export failed")
		return
	}
	h.cacheHeaders(w, cl)
	detailWriteFile(w, file)
}

func (h *ContentHandlers) put(w http.ResponseWriter, r *http.Request) {
	cl, ok := h.claims(w, r, content.KindUpload)
	if !ok {
		return
	}
	if !h.Signer.HasStorage() {
		http.Error(w, "storage not configured", http.StatusServiceUnavailable)
		return
	}
	limit := cl.MaxBytes
	if limit <= 0 {
		limit = port.MaxUploadBytes
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		http.Error(w, fmt.Sprintf("body exceeds %d bytes", limit), http.StatusRequestEntityTooLarge)
		return
	}
	if len(raw) == 0 {
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}
	if err := h.Signer.Write(r.Context(), cl, raw, r.Header.Get("Content-Type")); err != nil {
		h.Log.Error("content upload: blob put failed", "key", cl.Key, "error", err)
		http.Error(w, "store failed", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
