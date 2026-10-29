package port

import (
	"context"
	"errors"
	"time"
)

// ---------------------------------------------------------------------------
// BlobStore
//
// The persistent-blob layer for raw provider files (FIT/GPX/TCX/JSON)
// from worker imports, plus user-initiated bulk-exports and future
// thumbnails. See docs/architecture.md §9 for the design rationale
// (why S3 over NATS Object Store, key naming, content-types).
//
// Workers never hold S3 credentials. They call PresignUpload /
// PresignDownload via NATS request/reply against the server, which
// uses this port to mint a short-lived URL. The worker then PUTs or
// GETs directly to that URL — the API server never proxies blob bytes.
//
// Adapters: internal/adapter/secondary/s3/blob_store.go (AWS-SDK-Go-v2,
// MinIO-compatible via custom endpoint). The interface deliberately
// abstracts over the AWS SDK so we can swap to GCS or a self-contained
// storage if needed; in practice only the S3 adapter exists.
// ---------------------------------------------------------------------------

type BlobStore interface {
	// Put stores an object directly. Used SERVER-SIDE — the server has S3
	// credentials, so for a user file-upload it archives the raw bytes itself
	// rather than minting a presign URL for a round-trip. (Workers, which have
	// no credentials, still use PresignUpload.)
	Put(ctx context.Context, key string, data []byte, contentType string) error

	// Get reads an object's bytes + content-type. Used SERVER-SIDE for the
	// user-facing raw-file download, which proxies through the server (so it
	// works regardless of whether the object-store endpoint is browser-
	// reachable). Fine for the small raw activity files; large/streaming reads
	// should use PresignDownload instead.
	Get(ctx context.Context, key string) (data []byte, contentType string, err error)

	// PresignUpload mints a one-shot URL the worker can PUT to. Opts
	// constrain content-type, length range, and an optional sha256
	// digest the server enforces via S3 conditions.
	//
	// The returned URL is opaque to the worker — it includes any signature
	// the backend needs. The caller copies RequiredHeaders into the PUT
	// request verbatim.
	PresignUpload(ctx context.Context, key string, opts PresignUploadOpts) (PresignedURL, error)

	// PresignDownload mints a one-shot URL the caller can GET. Used
	// both for user-facing browser downloads (via the API server's
	// /raw-download endpoint that redirects) and for worker reparse
	// fetches (the parse_blob.<provider> job carries the URL).
	PresignDownload(ctx context.Context, key string, opts PresignDownloadOpts) (PresignedURL, error)

	// Stat returns size + content-type without downloading. Used by the
	// reparse path to detect "blob reference in DB but object gone"
	// without paying the GET cost.
	Stat(ctx context.Context, key string) (BlobMeta, error)

	// Delete removes a blob. Called by:
	//   * the lifecycle worker, for blobs of soft-deleted activities
	//     past retention
	//   * the PurgeExternalAccount use case, for all blobs of an
	//     account being disconnected
	//   * the full-user-delete use case (DSGVO)
	//
	// Idempotent: deleting a non-existent key returns nil.
	Delete(ctx context.Context, key string) error

	// Healthy reports whether the backing store + bucket are reachable.
	// Used by the readiness probe; should be a cheap metadata call (HeadBucket),
	// not a list or object operation.
	Healthy(ctx context.Context) error

	// PresignPublicDownload / PresignPublicUpload are PresignDownload /
	// PresignUpload signed for the client-reachable public endpoint
	// (CAIRN_STORAGE_PUBLIC_ENDPOINT). ErrNoPublicEndpoint when none is
	// configured; callers then serve the content through the server.
	PresignPublicDownload(ctx context.Context, key string, opts PresignDownloadOpts) (PresignedURL, error)
	PresignPublicUpload(ctx context.Context, key string, opts PresignUploadOpts) (PresignedURL, error)
}

// ErrNoPublicEndpoint: the store has no client-reachable endpoint to sign for.
var ErrNoPublicEndpoint = errors.New("blob: no public endpoint configured")

type PresignUploadOpts struct {
	// ContentType the worker will send. The presigned URL constrains
	// the PUT to this value; mismatch causes the backend to reject.
	ContentType string

	// ContentLengthMin/Max bound the PUT size. ±10% of the worker's
	// declared size is a reasonable default — defends against runaway
	// uploads.
	ContentLengthMin int64
	ContentLengthMax int64

	// ContentSHA256, when set, is enforced by the backend's
	// x-amz-content-sha256 check; mismatch rejects the upload.
	ContentSHA256 string

	// Expiry is the URL's validity window. 5 minutes is the default
	// for upload (worker uploads immediately after receiving the URL).
	Expiry time.Duration
}

type PresignDownloadOpts struct {
	Expiry time.Duration

	// ResponseContentDisposition overrides the response header so the
	// browser sees `attachment; filename="..."` for user-facing
	// downloads.
	ResponseContentDisposition string

	// ResponseContentType overrides what the backend stored. Useful
	// when serving a `.fit` file with a friendlier MIME type than the
	// canonical `application/vnd.ant.fit`.
	ResponseContentType string
}

type PresignedURL struct {
	// URL is the full backend URL the caller hits. Opaque shape —
	// includes the signature query params for S3-style backends.
	URL string

	// Method is "GET" for downloads, "PUT" for uploads. Caller must
	// match.
	Method string

	// RequiredHeaders the caller MUST send with the request. For uploads,
	// typically: content-type, x-amz-content-sha256. Empty map for
	// downloads.
	RequiredHeaders map[string]string

	// ExpiresAt is the wall-clock time the URL stops working. Use this
	// for cache eviction in workers that re-request fresh URLs.
	ExpiresAt time.Time
}

type BlobMeta struct {
	Key         string
	SizeBytes   int64
	ContentType string

	// ETag is the backend's content-hash (S3 returns the MD5 for small
	// objects; multipart uploads return a synthetic ETag). Useful for
	// detecting whether two blobs are identical without downloading.
	ETag string

	UpdatedAt time.Time
}
