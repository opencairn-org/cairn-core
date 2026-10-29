// Package s3 implements port.BlobStore against any S3-compatible object
// store via aws-sdk-go-v2. It is the production blob layer for Cairn —
// raw FIT/GPX/TCX/JSON files from worker imports, user-initiated bulk
// exports, future thumbnails.
//
// Compatibility: tested against MinIO via custom endpoint + path-style
// addressing; works against real S3 with the same configuration. See
// docs/architecture.md §9 for the design rationale.
//
// Workers never hold S3 credentials. They request presigned URLs from
// the Cairn server (NATS request/reply over `cairn.blob.presign_{up,down}`)
// and PUT/GET the bytes directly. This package never proxies bytes
// itself — it only mints signatures.
package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/opencairn-org/cairn-core/internal/config"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// BlobStore is the aws-sdk-go-v2-backed implementation of port.BlobStore.
type BlobStore struct {
	cfg       config.StorageConfig
	client    *awss3.Client
	presigner *awss3.PresignClient
	// publicPresigner signs for cfg.PublicEndpoint; nil when unset.
	publicPresigner *awss3.PresignClient
}

// Compile-time interface check.
var _ port.BlobStore = (*BlobStore)(nil)

// New builds a BlobStore from StorageConfig. It does no I/O — construction
// never touches the network. The configured bucket (CAIRN_STORAGE_BUCKET) is
// provisioned separately by EnsureBucket, which wire.go calls once at start-up.
//
// The returned BlobStore is safe for concurrent use.
func New(cfg config.StorageConfig) (*BlobStore, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("storage: bucket is required")
	}
	if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, errors.New("storage: access key id and secret are required")
	}

	awsCfg := aws.Config{
		Region:      cfg.Region,
		Credentials: credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
	}

	clientOpts := []func(*awss3.Options){}
	if cfg.Endpoint != "" {
		clientOpts = append(clientOpts, func(o *awss3.Options) {
			endpoint := cfg.Endpoint
			o.BaseEndpoint = &endpoint
		})
	}
	if cfg.UsePathStyle {
		clientOpts = append(clientOpts, func(o *awss3.Options) {
			o.UsePathStyle = true
		})
	}

	client := awss3.NewFromConfig(awsCfg, clientOpts...)
	b := &BlobStore{
		cfg:       cfg,
		client:    client,
		presigner: awss3.NewPresignClient(client),
	}
	if cfg.PublicEndpoint != "" {
		// Same credentials and bucket, different origin: a presigned URL is
		// bound to the host it was signed for, so client-facing URLs need a
		// client whose endpoint is the public one.
		pubOpts := append([]func(*awss3.Options){}, clientOpts...)
		pubOpts = append(pubOpts, func(o *awss3.Options) {
			endpoint := cfg.PublicEndpoint
			o.BaseEndpoint = &endpoint
		})
		b.publicPresigner = awss3.NewPresignClient(awss3.NewFromConfig(awsCfg, pubOpts...))
	}
	return b, nil
}

// Put stores an object directly via the server's S3 credentials. Used for
// user file-upload archival (the server holds the bytes already).
func (b *BlobStore) Put(ctx context.Context, key string, data []byte, contentType string) error {
	if key == "" {
		return errors.New("blob: empty key")
	}
	ct := contentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	_, err := b.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:      &b.cfg.Bucket,
		Key:         &key,
		Body:        bytes.NewReader(data),
		ContentType: &ct,
	})
	if err != nil {
		return fmt.Errorf("blob put %s: %w", key, err)
	}
	return nil
}

// Get reads an object's bytes + content-type via the server's S3 credentials.
func (b *BlobStore) Get(ctx context.Context, key string) ([]byte, string, error) {
	if key == "" {
		return nil, "", errors.New("blob: empty key")
	}
	out, err := b.client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: &b.cfg.Bucket,
		Key:    &key,
	})
	if err != nil {
		return nil, "", fmt.Errorf("blob get %s: %w", key, err)
	}
	defer func() { _ = out.Body.Close() }()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, "", fmt.Errorf("blob read %s: %w", key, err)
	}
	ct := ""
	if out.ContentType != nil {
		ct = *out.ContentType
	}
	return data, ct, nil
}

// PresignUpload mints a presigned PUT URL bound to the supplied key.
// Content-Type, length range, and optional SHA-256 are baked into the
// signature: an upload that violates the constraints is rejected by the
// backend with 403 SignatureDoesNotMatch.
//
// Required request headers come back in RequiredHeaders — the caller
// MUST forward them verbatim or the signature won't validate.
func (b *BlobStore) PresignUpload(ctx context.Context, key string, opts port.PresignUploadOpts) (port.PresignedURL, error) {
	return b.presignUpload(ctx, b.presigner, key, opts)
}

// PresignPublicUpload is PresignUpload for the client-reachable endpoint.
func (b *BlobStore) PresignPublicUpload(ctx context.Context, key string, opts port.PresignUploadOpts) (port.PresignedURL, error) {
	if b.publicPresigner == nil {
		return port.PresignedURL{}, port.ErrNoPublicEndpoint
	}
	return b.presignUpload(ctx, b.publicPresigner, key, opts)
}

func (b *BlobStore) presignUpload(ctx context.Context, presigner *awss3.PresignClient, key string, opts port.PresignUploadOpts) (port.PresignedURL, error) {
	if key == "" {
		return port.PresignedURL{}, errors.New("blob: empty key")
	}
	expiry := opts.Expiry
	if expiry <= 0 {
		expiry = b.cfg.PresignTTL
	}

	in := &awss3.PutObjectInput{
		Bucket: &b.cfg.Bucket,
		Key:    &key,
	}
	if opts.ContentType != "" {
		in.ContentType = &opts.ContentType
	}
	// The wire contract carries the digest hex-encoded; the S3 checksum
	// header (x-amz-checksum-sha256) wants base64 of the raw digest. A
	// hex value in that header is rejected with 400 before the object is
	// even considered.
	checksumB64 := ""
	if opts.ContentSHA256 != "" {
		raw, err := hex.DecodeString(opts.ContentSHA256)
		if err != nil || len(raw) != sha256.Size {
			return port.PresignedURL{}, fmt.Errorf("blob: content sha256 must be a hex-encoded sha-256 digest")
		}
		checksumB64 = base64.StdEncoding.EncodeToString(raw)
		in.ChecksumSHA256 = &checksumB64
	}

	req, err := presigner.PresignPutObject(ctx, in, func(po *awss3.PresignOptions) {
		po.Expires = expiry
	})
	if err != nil {
		return port.PresignedURL{}, fmt.Errorf("presign put %s: %w", key, err)
	}

	required := map[string]string{}
	if opts.ContentType != "" {
		required["Content-Type"] = opts.ContentType
	}
	if checksumB64 != "" {
		required["x-amz-checksum-sha256"] = checksumB64
	}
	// Copy any headers the SDK marked required for the signed request.
	for k, vs := range req.SignedHeader {
		if len(vs) > 0 {
			required[k] = vs[0]
		}
	}

	return port.PresignedURL{
		URL:             req.URL,
		Method:          req.Method,
		RequiredHeaders: required,
		ExpiresAt:       time.Now().Add(expiry),
	}, nil
}

// PresignDownload mints a presigned GET URL. Content-Disposition /
// Content-Type response overrides are S3's response-header overrides —
// the bucket's stored values stay unchanged.
func (b *BlobStore) PresignDownload(ctx context.Context, key string, opts port.PresignDownloadOpts) (port.PresignedURL, error) {
	return b.presignDownload(ctx, b.presigner, key, opts)
}

// PresignPublicDownload is PresignDownload for the client-reachable endpoint.
func (b *BlobStore) PresignPublicDownload(ctx context.Context, key string, opts port.PresignDownloadOpts) (port.PresignedURL, error) {
	if b.publicPresigner == nil {
		return port.PresignedURL{}, port.ErrNoPublicEndpoint
	}
	return b.presignDownload(ctx, b.publicPresigner, key, opts)
}

func (b *BlobStore) presignDownload(ctx context.Context, presigner *awss3.PresignClient, key string, opts port.PresignDownloadOpts) (port.PresignedURL, error) {
	if key == "" {
		return port.PresignedURL{}, errors.New("blob: empty key")
	}
	expiry := opts.Expiry
	if expiry <= 0 {
		expiry = b.cfg.PresignTTL
	}

	in := &awss3.GetObjectInput{
		Bucket: &b.cfg.Bucket,
		Key:    &key,
	}
	if opts.ResponseContentDisposition != "" {
		in.ResponseContentDisposition = &opts.ResponseContentDisposition
	}
	if opts.ResponseContentType != "" {
		in.ResponseContentType = &opts.ResponseContentType
	}

	req, err := presigner.PresignGetObject(ctx, in, func(po *awss3.PresignOptions) {
		po.Expires = expiry
	})
	if err != nil {
		return port.PresignedURL{}, fmt.Errorf("presign get %s: %w", key, err)
	}

	return port.PresignedURL{
		URL:             req.URL,
		Method:          req.Method,
		RequiredHeaders: map[string]string{},
		ExpiresAt:       time.Now().Add(expiry),
	}, nil
}

// Stat returns size + content-type from a HEAD request. Missing keys
// surface as port.ErrNotFound so callers can distinguish "blob gone"
// from "transport error". Empty content type is left as the zero value;
// the bucket's policy might also strip it.
func (b *BlobStore) Stat(ctx context.Context, key string) (port.BlobMeta, error) {
	if key == "" {
		return port.BlobMeta{}, errors.New("blob: empty key")
	}
	out, err := b.client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: &b.cfg.Bucket,
		Key:    &key,
	})
	if err != nil {
		if isNotFound(err) {
			return port.BlobMeta{}, fmt.Errorf("blob %s: %w", key, domain.ErrNotFound)
		}
		return port.BlobMeta{}, fmt.Errorf("head %s: %w", key, err)
	}

	meta := port.BlobMeta{Key: key}
	if out.ContentLength != nil {
		meta.SizeBytes = *out.ContentLength
	}
	if out.ContentType != nil {
		meta.ContentType = *out.ContentType
	}
	if out.ETag != nil {
		meta.ETag = *out.ETag
	}
	if out.LastModified != nil {
		meta.UpdatedAt = *out.LastModified
	}
	return meta, nil
}

// Delete is idempotent — S3 returns success even when the object never
// existed, so we forward that semantics without an explicit Stat first.
func (b *BlobStore) Delete(ctx context.Context, key string) error {
	if key == "" {
		return errors.New("blob: empty key")
	}
	_, err := b.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: &b.cfg.Bucket,
		Key:    &key,
	})
	if err != nil {
		return fmt.Errorf("delete %s: %w", key, err)
	}
	return nil
}

// EnsureBucket creates the configured bucket (CAIRN_STORAGE_BUCKET) if it
// doesn't already exist. Idempotent — an existing bucket owned by these
// credentials is a no-op. Called once at server start-up (wire.go) so a fresh
// deployment doesn't need an out-of-band `mc mb` / console step; MinIO and S3
// both behave this way.
func (b *BlobStore) EnsureBucket(ctx context.Context) error {
	// Fast path: already present + accessible.
	if _, err := b.client.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: &b.cfg.Bucket}); err == nil {
		return nil
	}

	in := &awss3.CreateBucketInput{Bucket: &b.cfg.Bucket}
	// AWS requires a LocationConstraint for every region except us-east-1.
	// MinIO ignores it; harmless to send when a non-default region is set.
	if b.cfg.Region != "" && b.cfg.Region != "us-east-1" {
		in.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{
			LocationConstraint: s3types.BucketLocationConstraint(b.cfg.Region),
		}
	}
	if _, err := b.client.CreateBucket(ctx, in); err != nil {
		// Concurrent boots / pre-existing bucket → success.
		var owned *s3types.BucketAlreadyOwnedByYou
		var exists *s3types.BucketAlreadyExists
		if errors.As(err, &owned) || errors.As(err, &exists) {
			return nil
		}
		return fmt.Errorf("ensure bucket %q: %w", b.cfg.Bucket, err)
	}
	return nil
}

// EnsureLifecycleRule upserts an expiration rule for keys under prefix,
// preserving any rules with other IDs (PutBucketLifecycleConfiguration
// replaces the whole config, so read-modify-write). Used for the transfer/
// prefix: claim-checked result payloads are deleted after ingest, and this
// rule reaps orphans (worker uploaded but envelope never processed).
func (b *BlobStore) EnsureLifecycleRule(ctx context.Context, ruleID, prefix string, expireDays int32) error {
	var rules []s3types.LifecycleRule
	existing, err := b.client.GetBucketLifecycleConfiguration(ctx, &awss3.GetBucketLifecycleConfigurationInput{Bucket: &b.cfg.Bucket})
	if err != nil {
		// MinIO and S3 signal "no lifecycle configured yet" as an API error.
		var apiErr smithy.APIError
		if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "NoSuchLifecycleConfiguration" {
			return fmt.Errorf("get lifecycle config: %w", err)
		}
	} else {
		for _, r := range existing.Rules {
			if r.ID == nil || *r.ID != ruleID {
				rules = append(rules, r)
			}
		}
	}

	rules = append(rules, s3types.LifecycleRule{
		ID:         &ruleID,
		Status:     s3types.ExpirationStatusEnabled,
		Filter:     &s3types.LifecycleRuleFilter{Prefix: &prefix},
		Expiration: &s3types.LifecycleExpiration{Days: &expireDays},
	})

	if _, err := b.client.PutBucketLifecycleConfiguration(ctx, &awss3.PutBucketLifecycleConfigurationInput{
		Bucket:                 &b.cfg.Bucket,
		LifecycleConfiguration: &s3types.BucketLifecycleConfiguration{Rules: rules},
	}); err != nil {
		return fmt.Errorf("put lifecycle config: %w", err)
	}
	return nil
}

// Healthy issues a HeadBucket — a cheap metadata call that confirms the store
// is reachable and the configured bucket exists/is accessible. Used by the
// readiness probe.
func (b *BlobStore) Healthy(ctx context.Context) error {
	_, err := b.client.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: &b.cfg.Bucket})
	if err != nil {
		return fmt.Errorf("blob store unreachable: %w", err)
	}
	return nil
}

// isNotFound detects S3's missing-object signals. The SDK returns these
// as typed errors after a HeadObject; we check both the modeled error
// and the generic Smithy error code for compatibility across SDK
// versions (and against MinIO, which sometimes only sets the API code).
func isNotFound(err error) bool {
	var nsk *s3types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var notFound *s3types.NotFound
	if errors.As(err, &notFound) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	return false
}
