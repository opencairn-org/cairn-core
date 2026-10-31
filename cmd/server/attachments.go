package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// attachmentsMaxUpload caps an attachment file; attachmentsMaxBody adds headroom for multipart framing and caption.
const (
	attachmentsMaxUpload = 32 << 20
	attachmentsMaxBody   = attachmentsMaxUpload + 1<<20
)

// attachmentsHandlers holds the dependencies of the attachment routes.
type attachmentsHandlers struct {
	app    *App
	logger *slog.Logger
}

// mountAttachmentEndpoints registers activity attachment (photo) routes:
//
//	GET    /api/activities/{id}/attachments              list metadata
//	POST   /api/activities/{id}/attachments              upload (multipart "file")
//	GET    /api/activities/{id}/attachments/{aid}/raw    stream the image bytes
//	DELETE /api/activities/{id}/attachments/{aid}        remove
//
// Reads (list, raw) are visibility-gated: the owner always sees them; a
// non-owner sees them when the CategoryPhotos gate is open (like map.png).
// Writes (upload, delete) are owner-only. Bytes live in the BlobStore; the
// server proxies them on /raw so the storage endpoint never leaks to the browser.
func mountAttachmentEndpoints(mux *http.ServeMux, app *App, logger *slog.Logger) {
	if app.Attachments == nil {
		return
	}
	h := &attachmentsHandlers{app: app, logger: logger}
	mux.HandleFunc("GET /api/activities/{id}/attachments", h.list)
	mux.HandleFunc("GET /api/activities/{id}/attachments/{aid}/raw", h.raw)
	mux.HandleFunc("POST /api/activities/{id}/attachments", h.upload)
	mux.HandleFunc("DELETE /api/activities/{id}/attachments/{aid}", h.remove)

	logger.Info("attachment endpoints mounted", "path", "/api/activities/{id}/attachments")
}

// authOwner: the activity must exist and belong to the caller (writes).
func (h *attachmentsHandlers) authOwner(w http.ResponseWriter, r *http.Request) (domain.ActivityID, bool) {
	userID, ok := resolveSessionUser(r, h.app)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return domain.ActivityID{}, false
	}
	aid, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid activity id", http.StatusBadRequest)
		return domain.ActivityID{}, false
	}
	act, err := h.app.Activities.GetActivity(r.Context(), domain.ActivityID(aid))
	if err != nil || act.UserID != userID || act.IsDeleted() {
		http.Error(w, "not found", http.StatusNotFound)
		return domain.ActivityID{}, false
	}
	return domain.ActivityID(aid), true
}

// authView: owner sees photos; a non-owner sees them only when the activity
// is visible and the CategoryPhotos gate is open for them (mirrors the
// map.png visibility re-check). Reads only.
func (h *attachmentsHandlers) authView(w http.ResponseWriter, r *http.Request) (domain.ActivityID, bool, bool) {
	aid, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid activity id", http.StatusBadRequest)
		return domain.ActivityID{}, false, false
	}
	act, err := h.app.Activities.GetActivity(r.Context(), domain.ActivityID(aid))
	if err != nil {
		http.NotFound(w, r)
		return domain.ActivityID{}, false, false
	}
	var viewer *domain.UserID
	if v, ok := optionalSessionUser(r, h.app); ok {
		viewer = &v
	}
	vw, ok := h.app.Visibility2.Resolve(r.Context(), act, viewer, shareTokenFor(r, h.app, act.ID))
	if !ok || (!vw.IsOwner && !vw.Cats.Has(domain.CategoryPhotos)) {
		http.NotFound(w, r)
		return domain.ActivityID{}, false, false
	}
	return act.ID, vw.IsOwner, true
}

// ownedAttachment loads the {aid} attachment and checks it belongs to activityID.
func (h *attachmentsHandlers) ownedAttachment(w http.ResponseWriter, r *http.Request, activityID domain.ActivityID) (domain.AttachmentID, domain.Attachment, bool) {
	aid, err := uuid.Parse(r.PathValue("aid"))
	if err != nil {
		http.Error(w, "invalid attachment id", http.StatusBadRequest)
		return domain.AttachmentID{}, domain.Attachment{}, false
	}
	att, err := h.app.Attachments.Get(r.Context(), domain.AttachmentID(aid))
	if err != nil || att.ActivityID != activityID {
		http.Error(w, "not found", http.StatusNotFound)
		return domain.AttachmentID{}, domain.Attachment{}, false
	}
	return domain.AttachmentID(aid), att, true
}

func (h *attachmentsHandlers) list(w http.ResponseWriter, r *http.Request) {
	activityID, isOwner, ok := h.authView(w, r)
	if !ok {
		return
	}
	atts, err := h.app.Attachments.ListForActivity(r.Context(), activityID)
	if err != nil {
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(atts))
	for _, a := range atts {
		item := map[string]any{
			"id":           a.ID.String(),
			"url":          fmt.Sprintf("/api/activities/%s/attachments/%s/raw", activityID.String(), a.ID.String()) + shareSuffix(r),
			"caption":      a.Caption,
			"content_type": a.ContentType,
			"width":        a.Width,
			"height":       a.Height,
			"blurhash":     a.Blurhash,
		}
		// Signed per-variant URLs (same mechanism as the Connect Attachment
		// message); "url" stays for the web until it migrates.
		if urls := attachmentContentURLs(r.Context(), h.app, a, isOwner); len(urls) > 0 {
			item["urls"] = urls
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"attachments": out})
}

// shareSuffix carries a ?share= token onto links that point back at a
// link-gated endpoint; authView already checked it.
func shareSuffix(r *http.Request) string {
	if t := r.URL.Query().Get("share"); t != "" {
		return "?share=" + url.QueryEscape(t)
	}
	return ""
}

// attachmentContentURLs signs the derived variants that exist, plus the
// original (which keeps its EXIF) for the owner.
func attachmentContentURLs(ctx context.Context, app *App, a domain.Attachment, isOwner bool) map[string]any {
	if app.Content == nil {
		return nil
	}
	original := ""
	if isOwner {
		original = a.BlobID
	}
	out := map[string]any{}
	for name, key := range map[string]string{"original": original, "medium": a.MediumBlobID, "thumb": a.ThumbBlobID} {
		if key == "" {
			continue
		}
		ct := "image/jpeg"
		if name == "original" {
			ct = a.ContentType
		}
		if u, err := app.Content.Blob(ctx, key, ct, ""); err == nil {
			out[name] = map[string]any{"url": u.URL, "expires_at": u.ExpiresAt.UTC().Format(time.RFC3339)}
		}
	}
	return out
}

func (h *attachmentsHandlers) raw(w http.ResponseWriter, r *http.Request) {
	activityID, isOwner, ok := h.authView(w, r)
	if !ok {
		return
	}
	if h.app.BlobStore == nil {
		http.Error(w, "storage not configured", http.StatusServiceUnavailable)
		return
	}
	_, att, ok := h.ownedAttachment(w, r, activityID)
	if !ok {
		return
	}
	// Non-owners only get a re-encoded variant: the original keeps EXIF GPS.
	key, wantCT := att.BlobID, att.ContentType
	if !isOwner {
		key, wantCT = att.DisplayBlobID(), "image/jpeg"
		if key == "" {
			http.NotFound(w, r)
			return
		}
	}
	data, ct, err := h.app.BlobStore.Get(r.Context(), key)
	if err != nil {
		http.Error(w, "blob unavailable", http.StatusBadGateway)
		return
	}
	if wantCT != "" {
		ct = wantCT
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = w.Write(data)
}

// readUpload parses the multipart body and returns the image bytes and sniffed content type.
func (h *attachmentsHandlers) readUpload(w http.ResponseWriter, r *http.Request) ([]byte, string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, attachmentsMaxBody)
	if err := r.ParseMultipartForm(attachmentsMaxUpload); err != nil { //nolint:gosec // G120: body capped by MaxBytesReader above
		http.Error(w, "bad multipart form", http.StatusBadRequest)
		return nil, "", false
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file field", http.StatusBadRequest)
		return nil, "", false
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, attachmentsMaxUpload))
	if err != nil {
		http.Error(w, "read file failed", http.StatusBadRequest)
		return nil, "", false
	}
	ct := http.DetectContentType(raw)
	if !strings.HasPrefix(ct, "image/") {
		http.Error(w, "only image uploads are supported", http.StatusBadRequest)
		return nil, "", false
	}
	return raw, ct, true
}

func (h *attachmentsHandlers) upload(w http.ResponseWriter, r *http.Request) {
	activityID, ok := h.authOwner(w, r)
	if !ok {
		return
	}
	userID, _ := resolveSessionUser(r, h.app)
	if h.app.BlobStore == nil {
		http.Error(w, "storage not configured", http.StatusServiceUnavailable)
		return
	}
	raw, ct, ok := h.readUpload(w, r)
	if !ok {
		return
	}
	key := fmt.Sprintf("attachments/%s/%s/%s", userID.String(), activityID.String(), uuid.NewString())
	if err := h.app.BlobStore.Put(r.Context(), key, raw, ct); err != nil {
		h.logger.Error("attachment upload: blob put", "error", err)
		http.Error(w, "store failed", http.StatusInternalServerError)
		return
	}
	att := domain.Attachment{
		ActivityID:  activityID,
		UserID:      userID,
		BlobID:      key,
		ContentType: ct,
		Caption:     strings.TrimSpace(r.FormValue("caption")),
	}
	// Variants are best-effort: an undecodable image is still stored as is.
	if derived, err := h.app.AttachmentVariants.Derive(r.Context(), att, raw); err == nil {
		att = derived
	} else {
		h.logger.Warn("attachment upload: variants failed", "key", key, "error", err)
	}
	if err := h.app.Attachments.Add(r.Context(), att); err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *attachmentsHandlers) remove(w http.ResponseWriter, r *http.Request) {
	activityID, ok := h.authOwner(w, r)
	if !ok {
		return
	}
	attID, att, ok := h.ownedAttachment(w, r, activityID)
	if !ok {
		return
	}
	blobID, err := h.app.Attachments.Delete(r.Context(), attID)
	if err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	if h.app.BlobStore != nil {
		for _, key := range []string{blobID, att.ThumbBlobID, att.MediumBlobID} {
			if key == "" {
				continue
			}
			if err := h.app.BlobStore.Delete(r.Context(), key); err != nil {
				h.logger.Warn("attachment delete: blob drop failed", "blob_id", key, "error", err)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
