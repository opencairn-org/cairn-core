// Package content mints and verifies the short-lived signed URLs binary
// content (photos, static course images, exports, client uploads) is
// delivered under, so image loaders and share sheets fetch with a plain GET.
// A stored object is presigned on the public object-store endpoint when one
// is configured; otherwise, and always for rendered content, the URL points at
// the server's /content/{token} route. The token is an HMAC-signed, base64url
// JSON claim set bound to one object and an expiry; it is the whole
// authorization.
package content

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Kinds of a token.
const (
	KindBlob   = "blob"
	KindMap    = "map"
	KindExport = "export"
	KindUpload = "upload"
)

// Claims is the signed payload of a content token.
type Claims struct {
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

// ErrToken covers every verification failure; callers answer 404 for all of
// them so the route leaks nothing about why.
var ErrToken = errors.New("content: invalid token")

// ErrNoStorage: object storage is not configured.
var ErrNoStorage = port.NewActionError(port.ActionErrorUnavailable, "object storage is not configured")

// Codec signs and verifies tokens with a key derived from the session secret
// (domain-separated, so a token can never pass as a session).
type Codec struct {
	key []byte
	now func() time.Time
}

// NewCodec derives the signing key; now nil is the wall clock.
func NewCodec(sessionSecret string, now func() time.Time) *Codec {
	mac := hmac.New(sha256.New, []byte(sessionSecret))
	mac.Write([]byte("cairn-content-url-v1"))
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Codec{key: mac.Sum(nil), now: now}
}

// Encode returns payload.signature in base64url.
func (c *Codec) Encode(cl Claims) (string, error) {
	payload, err := json.Marshal(cl)
	if err != nil {
		return "", err
	}
	p := base64.RawURLEncoding.EncodeToString(payload)
	return p + "." + c.sign(p), nil
}

func (c *Codec) sign(payload string) string {
	mac := hmac.New(sha256.New, c.key)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Decode verifies the signature and expiry.
func (c *Codec) Decode(token string) (Claims, error) {
	p, sig, ok := strings.Cut(token, ".")
	if !ok || p == "" || sig == "" {
		return Claims{}, ErrToken
	}
	if !hmac.Equal([]byte(c.sign(p)), []byte(sig)) {
		return Claims{}, ErrToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return Claims{}, ErrToken
	}
	var cl Claims
	if err := json.Unmarshal(raw, &cl); err != nil {
		return Claims{}, ErrToken
	}
	if cl.Expires <= c.now().Unix() {
		return Claims{}, ErrToken
	}
	return cl, nil
}

// Remaining is the token's life left in seconds, never negative.
func (c *Codec) Remaining(cl Claims) int64 { return max(0, cl.Expires-c.now().Unix()) }

// Signer implements port.ContentSigner over the codec and, when the store has
// a public endpoint, object-store presigning; it also serves the route's
// stored-object reads and upload writes.
type Signer struct {
	Codec   *Codec
	baseURL string
	ttl     time.Duration
	blobs   port.BlobStore // nil when storage is not configured
}

var _ port.ContentSigner = (*Signer)(nil)

// NewSigner wires the signer; ttl <= 0 is 15 minutes, blobs may be nil.
func NewSigner(codec *Codec, publicBaseURL string, ttl time.Duration, blobs port.BlobStore) *Signer {
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	return &Signer{Codec: codec, baseURL: strings.TrimRight(publicBaseURL, "/"), ttl: ttl, blobs: blobs}
}

func (s *Signer) route(cl Claims) (port.ContentURL, error) {
	exp := s.Codec.now().Add(s.ttl)
	cl.Expires = exp.Unix()
	tok, err := s.Codec.Encode(cl)
	if err != nil {
		return port.ContentURL{}, err
	}
	return port.ContentURL{URL: s.baseURL + "/content/" + tok, ExpiresAt: exp}, nil
}

// Blob signs a stored object.
func (s *Signer) Blob(ctx context.Context, key, contentType, filename string) (port.ContentURL, error) {
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
	return s.route(Claims{Kind: KindBlob, Key: key, ContentType: contentType, Filename: filename})
}

// MapImage signs an activity's course image.
func (s *Signer) MapImage(_ context.Context, activityID domain.ActivityID, trimmed bool) (port.ContentURL, error) {
	return s.route(Claims{Kind: KindMap, ActivityID: activityID.String(), Trimmed: trimmed})
}

// Export signs an interchange-file download.
func (s *Signer) Export(_ context.Context, activityID domain.ActivityID, format string, exclude []string) (port.ContentURL, error) {
	return s.route(Claims{Kind: KindExport, ActivityID: activityID.String(), Format: format, Exclude: strings.Join(exclude, ",")})
}

// Upload mints a PUT target for a client file upload.
func (s *Signer) Upload(ctx context.Context, key, contentType string, maxBytes int64) (port.PresignedURL, error) {
	if s.blobs == nil {
		return port.PresignedURL{}, ErrNoStorage
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
	u, err := s.route(Claims{Kind: KindUpload, Key: key, ContentType: contentType, MaxBytes: maxBytes})
	if err != nil {
		return port.PresignedURL{}, err
	}
	return port.PresignedURL{
		URL: u.URL, Method: "PUT",
		RequiredHeaders: map[string]string{"Content-Type": contentType},
		ExpiresAt:       u.ExpiresAt,
	}, nil
}

// HasStorage reports whether stored objects can be read or written.
func (s *Signer) HasStorage() bool { return s.blobs != nil }

// Read returns a blob token's object and the content type to serve it under.
func (s *Signer) Read(ctx context.Context, cl Claims) ([]byte, string, error) {
	if s.blobs == nil {
		return nil, "", ErrNoStorage
	}
	data, ct, err := s.blobs.Get(ctx, cl.Key)
	if err != nil {
		return nil, "", domain.ErrNotFound
	}
	if cl.ContentType != "" {
		ct = cl.ContentType
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	return data, ct, nil
}

// Write stores an upload token's body; the claimed content type wins over
// the request's.
func (s *Signer) Write(ctx context.Context, cl Claims, data []byte, requestCT string) error {
	if s.blobs == nil {
		return ErrNoStorage
	}
	ct := cl.ContentType
	if ct == "" {
		ct = requestCT
	}
	return s.blobs.Put(ctx, cl.Key, data, ct)
}
