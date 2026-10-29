package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/config"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/staticmap"
)

// content.go delivers binary content (photos, static course images, exports,
// client uploads) under short-lived signed URLs, so image loaders and share
// sheets fetch with a plain GET and no credentials.
//
//	GET /content/{token}   stored object | course image (rendered on demand) | export (rendered on GET)
//	PUT /content/{token}   client file upload into the blob store
//
// A stored object is presigned on the public object-store endpoint instead
// when CAIRN_STORAGE_PUBLIC_ENDPOINT is set; rendered content always goes
// through this route. The token is an HMAC-signed, base64url JSON claim set
// bound to one object and an expiry; it is the whole authorization.

// contentKinds of a token.
const (
	contentKindBlob   = "blob"
	contentKindMap    = "map"
	contentKindExport = "export"
	contentKindUpload = "upload"
)

// contentClaims is the signed payload of a content token.
type contentClaims struct {
	Kind        string `json:"k"`
	Key         string `json:"b,omitempty"` // blob key (blob, upload)
	ActivityID  string `json:"a,omitempty"` // map, export
	Trimmed     bool   `json:"tr,omitempty"`
	Format      string `json:"f,omitempty"` // export
	Exclude     string `json:"x,omitempty"` // export, comma list
	ContentType string `json:"t,omitempty"`
	Filename    string `json:"n,omitempty"`
	MaxBytes    int64  `json:"m,omitempty"` // upload
	Expires     int64  `json:"e"`           // unix seconds
}

// contentCodec signs and verifies tokens with a key derived from the session
// secret (domain-separated, so a token can never pass as a session).
type contentCodec struct {
	key []byte
	now func() time.Time
}

func newContentCodec(sessionSecret string, now func() time.Time) *contentCodec {
	mac := hmac.New(sha256.New, []byte(sessionSecret))
	mac.Write([]byte("cairn-content-url-v1"))
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &contentCodec{key: mac.Sum(nil), now: now}
}

// encode returns payload.signature in base64url.
func (c *contentCodec) encode(cl contentClaims) (string, error) {
	payload, err := json.Marshal(cl)
	if err != nil {
		return "", err
	}
	p := base64.RawURLEncoding.EncodeToString(payload)
	return p + "." + c.sign(p), nil
}

func (c *contentCodec) sign(payload string) string {
	mac := hmac.New(sha256.New, c.key)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// errContentToken covers every verification failure; the handler answers 404
// for all of them so the route leaks nothing about why.
var errContentToken = errors.New("content: invalid token")

// decode verifies the signature and expiry.
func (c *contentCodec) decode(token string) (contentClaims, error) {
	p, sig, ok := strings.Cut(token, ".")
	if !ok || p == "" || sig == "" {
		return contentClaims{}, errContentToken
	}
	if !hmac.Equal([]byte(c.sign(p)), []byte(sig)) {
		return contentClaims{}, errContentToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return contentClaims{}, errContentToken
	}
	var cl contentClaims
	if err := json.Unmarshal(raw, &cl); err != nil {
		return contentClaims{}, errContentToken
	}
	if cl.Expires <= c.now().Unix() {
		return contentClaims{}, errContentToken
	}
	return cl, nil
}

// contentSigner implements port.ContentSigner over the codec and, when the
// store has a public endpoint, S3 presigning for stored objects.
type contentSigner struct {
	codec   *contentCodec
	baseURL string
	ttl     time.Duration
	blobs   port.BlobStore // nil when storage is not configured
}

var _ port.ContentSigner = (*contentSigner)(nil)

func newContentSigner(app *App, cfg *config.Config) *contentSigner {
	ttl := cfg.Storage.PresignTTL
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	return &contentSigner{
		codec:   newContentCodec(cfg.Auth.SessionSecret, nil),
		baseURL: strings.TrimRight(cfg.HTTP.PublicBaseURL, "/"),
		ttl:     ttl,
		blobs:   app.BlobStore,
	}
}

func (s *contentSigner) route(cl contentClaims) (port.ContentURL, error) {
	exp := s.codec.now().Add(s.ttl)
	cl.Expires = exp.Unix()
	tok, err := s.codec.encode(cl)
	if err != nil {
		return port.ContentURL{}, err
	}
	return port.ContentURL{URL: s.baseURL + "/content/" + tok, ExpiresAt: exp}, nil
}

func (s *contentSigner) Blob(ctx context.Context, key, contentType, filename string) (port.ContentURL, error) {
	if key == "" {
		return port.ContentURL{}, errors.New("content: empty blob key")
	}
	if s.blobs != nil {
		opts := port.PresignDownloadOpts{Expiry: s.ttl, ResponseContentType: contentType}
		if filename != "" {
			opts.ResponseContentDisposition = "attachment; filename=\"" + filename + "\""
		}
		signed, err := s.blobs.PresignPublicDownload(ctx, key, opts)
		if err == nil {
			return port.ContentURL{URL: signed.URL, ExpiresAt: signed.ExpiresAt}, nil
		}
		if !errors.Is(err, port.ErrNoPublicEndpoint) {
			return port.ContentURL{}, err
		}
	}
	return s.route(contentClaims{Kind: contentKindBlob, Key: key, ContentType: contentType, Filename: filename})
}

func (s *contentSigner) MapImage(_ context.Context, activityID domain.ActivityID, trimmed bool) (port.ContentURL, error) {
	return s.route(contentClaims{Kind: contentKindMap, ActivityID: activityID.String(), Trimmed: trimmed})
}

func (s *contentSigner) Export(_ context.Context, activityID domain.ActivityID, format string, exclude []string) (port.ContentURL, error) {
	return s.route(contentClaims{Kind: contentKindExport, ActivityID: activityID.String(), Format: format, Exclude: strings.Join(exclude, ",")})
}

func (s *contentSigner) Upload(ctx context.Context, key, contentType string, maxBytes int64) (port.PresignedURL, error) {
	if s.blobs == nil {
		return port.PresignedURL{}, port.NewActionError(port.ActionErrorUnavailable, "object storage is not configured")
	}
	signed, err := s.blobs.PresignPublicUpload(ctx, key, port.PresignUploadOpts{
		ContentType: contentType, ContentLengthMin: 1, ContentLengthMax: maxBytes, Expiry: s.ttl,
	})
	if err == nil {
		return signed, nil
	}
	if !errors.Is(err, port.ErrNoPublicEndpoint) {
		return port.PresignedURL{}, err
	}
	u, err := s.route(contentClaims{Kind: contentKindUpload, Key: key, ContentType: contentType, MaxBytes: maxBytes})
	if err != nil {
		return port.PresignedURL{}, err
	}
	return port.PresignedURL{
		URL: u.URL, Method: http.MethodPut,
		RequiredHeaders: map[string]string{"Content-Type": contentType},
		ExpiresAt:       u.ExpiresAt,
	}, nil
}

// contentHandlers serve /content/{token}.
type contentHandlers struct {
	app       *App
	codec     *contentCodec
	logger    *slog.Logger
	fetchTile staticmap.TileFetcher
}

func mountContent(mux *http.ServeMux, app *App, cfg *config.Config, logger *slog.Logger) {
	signer := newContentSigner(app, cfg)
	app.Content = signer
	h := &contentHandlers{app: app, codec: signer.codec, logger: logger, fetchTile: newTileFetcher(cfg.Map)}
	mux.HandleFunc("GET /content/{token}", h.get)
	mux.HandleFunc("PUT /content/{token}", h.put)
	logger.Info("signed content route mounted", "path", "/content/{token}", "public_endpoint", cfg.Storage.PublicEndpoint != "")
}

func (h *contentHandlers) claims(w http.ResponseWriter, r *http.Request, kinds ...string) (contentClaims, bool) {
	cl, err := h.codec.decode(r.PathValue("token"))
	if err != nil {
		http.NotFound(w, r)
		return contentClaims{}, false
	}
	for _, k := range kinds {
		if cl.Kind == k {
			return cl, true
		}
	}
	http.NotFound(w, r)
	return contentClaims{}, false
}

// cacheHeaders: private (the URL is per viewer) for the token's remaining life.
func (h *contentHandlers) cacheHeaders(w http.ResponseWriter, cl contentClaims) {
	remaining := max(0, cl.Expires-h.codec.now().Unix())
	w.Header().Set("Cache-Control", "private, max-age="+strconv.FormatInt(remaining, 10))
}

func (h *contentHandlers) get(w http.ResponseWriter, r *http.Request) {
	cl, ok := h.claims(w, r, contentKindBlob, contentKindMap, contentKindExport)
	if !ok {
		return
	}
	switch cl.Kind {
	case contentKindBlob:
		h.serveBlob(w, r, cl)
	case contentKindMap:
		h.serveMap(w, r, cl)
	case contentKindExport:
		h.serveExport(w, r, cl)
	}
}

func (h *contentHandlers) serveBlob(w http.ResponseWriter, r *http.Request, cl contentClaims) {
	if h.app.BlobStore == nil {
		http.Error(w, "storage not configured", http.StatusServiceUnavailable)
		return
	}
	data, ct, err := h.app.BlobStore.Get(r.Context(), cl.Key)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if cl.ContentType != "" {
		ct = cl.ContentType
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	if cl.Filename != "" {
		w.Header().Set("Content-Disposition", "attachment; filename=\""+cl.Filename+"\"")
	}
	h.cacheHeaders(w, cl)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data) //nolint:gosec // G705: stored object bytes served under their own content type
}

func (h *contentHandlers) serveMap(w http.ResponseWriter, r *http.Request, cl contentClaims) {
	actID, err := domain.ParseUUID[domain.ActivityID](cl.ActivityID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	act, err := h.app.Activities.GetActivity(r.Context(), actID)
	if err != nil || act.IsDeleted() {
		http.NotFound(w, r)
		return
	}
	data, ok := loadOrRenderMap(r.Context(), h.app, act, cl.Trimmed, h.fetchTile, h.logger)
	if !ok {
		http.NotFound(w, r)
		return
	}
	serveMapPNG(w, r, data)
}

func (h *contentHandlers) serveExport(w http.ResponseWriter, r *http.Request, cl contentClaims) {
	actID, err := domain.ParseUUID[domain.ActivityID](cl.ActivityID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	act, err := h.app.Activities.GetActivity(r.Context(), actID)
	if err != nil || act.IsDeleted() {
		http.NotFound(w, r)
		return
	}
	file, err := h.app.Exports.Render(r.Context(), act, cl.Format, cl.Exclude)
	if err != nil {
		writeActionError(w, err, "export failed")
		return
	}
	h.cacheHeaders(w, cl)
	w.Header().Set("Content-Type", file.ContentType)
	w.Header().Set("Content-Disposition", "attachment; filename=\""+file.Filename+"\"")
	w.Header().Set("Content-Length", strconv.Itoa(len(file.Data)))
	_, _ = w.Write(file.Data) //nolint:gosec // G705: non-HTML content type served as an attachment
}

func (h *contentHandlers) put(w http.ResponseWriter, r *http.Request) {
	cl, ok := h.claims(w, r, contentKindUpload)
	if !ok {
		return
	}
	if h.app.BlobStore == nil {
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
	ct := cl.ContentType
	if ct == "" {
		ct = r.Header.Get("Content-Type")
	}
	if err := h.app.BlobStore.Put(r.Context(), cl.Key, raw, ct); err != nil {
		h.logger.Error("content upload: blob put failed", "key", cl.Key, "error", err)
		http.Error(w, "store failed", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// loadOrRenderMap serves the cached course image for the variant or renders
// and caches it. ok=false when there is no renderable track.
func loadOrRenderMap(ctx context.Context, app *App, act domain.Activity, trimmed bool, fetchTile staticmap.TileFetcher, logger *slog.Logger) ([]byte, bool) {
	variant := "full"
	if trimmed {
		variant = "trimmed"
	}
	key := mapImageCacheKey(act.ID, variant)
	if app.BlobStore != nil {
		if data, _, err := app.BlobStore.Get(ctx, key); err == nil && len(data) > 0 {
			return data, true
		}
	}
	data, ok := renderActivityMap(ctx, app, act, trimmed, fetchTile, logger)
	if !ok {
		return nil, false
	}
	if app.BlobStore != nil {
		if err := app.BlobStore.Put(ctx, key, data, "image/png"); err != nil {
			logger.Warn("map image: cache store failed", "activity", act.ID, "error", err)
		}
	}
	return data, true
}

// mapImageETag changes when the course image would render differently.
func mapImageETag(act domain.Activity) string {
	if act.PrimaryStreamSourceID == nil {
		return ""
	}
	return mapImageStyleVersion + "-" + act.PrimaryStreamSourceID.String()[:8] + "-" + strconv.FormatInt(act.MergedAt.Unix(), 36)
}
