package nats

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/opencairn-org/cairn-core/internal/port"
)

// ---------------------------------------------------------------------------
// Stream / KV bootstrap
// ---------------------------------------------------------------------------

// Stream names — single source of truth, referenced by use-case code
// via the BootstrapStreams contract.
const (
	StreamJobs     = "CAIRN_JOBS"
	StreamResults  = "CAIRN_RESULTS"
	StreamEvents   = "CAIRN_EVENTS"
	StreamDLQ      = "CAIRN_DLQ"
	StreamWebhooks = "CAIRN_WEBHOOKS"

	KVWorkerPresence  = "cairn_worker_presence"
	KVWorkerManifests = "cairn_worker_manifests"
	KVRateLimits      = "cairn_rate_limits"
	KVBlobHandles     = "cairn_blob_handles"
	KVCoreInfo        = "cairn_core"

	OSTransient = "cairn_transient"
)

// BootstrapStreams creates (or updates) every canonical JetStream stream,
// KV bucket, and Object-Store bucket Cairn relies on. Idempotent — safe
// to call on every `cairn serve` startup.
//
// Operators that pre-provision via `nats stream add` see this become
// a series of no-ops; the only side-effect on existing assets is
// configuration drift detection (CreateOrUpdate aligns config with the
// declared shape).
//
// See docs/architecture.md §2 for the rationale behind each retention
// policy, dedup window, and TTL.
func (b *Bus) BootstrapStreams(ctx context.Context) error {
	replicas := 1 // adjust via config; single-node default

	streams := natsBootstrapStreamConfigs(replicas)

	for _, sc := range streams {
		if _, err := b.js.CreateOrUpdateStream(ctx, sc); err != nil {
			return fmt.Errorf("bootstrap stream %s: %w", sc.Name, err)
		}
		b.logger.Info("nats: stream ready",
			"stream", sc.Name,
			"retention", sc.Retention.String(),
			"storage", sc.Storage.String(),
		)
	}

	kvBuckets := natsBootstrapKVConfigs(replicas)

	for _, bc := range kvBuckets {
		if _, err := b.js.CreateOrUpdateKeyValue(ctx, bc); err != nil {
			return fmt.Errorf("bootstrap kv %s: %w", bc.Bucket, err)
		}
		b.logger.Info("nats: kv bucket ready", "bucket", bc.Bucket, "ttl", bc.TTL)
	}

	osBuckets := natsBootstrapObjectStoreConfigs(replicas)

	for _, oc := range osBuckets {
		if _, err := b.js.CreateOrUpdateObjectStore(ctx, oc); err != nil {
			return fmt.Errorf("bootstrap object store %s: %w", oc.Bucket, err)
		}
		b.logger.Info("nats: object store ready", "bucket", oc.Bucket, "max_bytes", oc.MaxBytes)
	}

	return nil
}

// natsBootstrapStreamConfigs declares the canonical JetStream streams.
func natsBootstrapStreamConfigs(replicas int) []jetstream.StreamConfig {
	return []jetstream.StreamConfig{
		{
			Name:        StreamJobs,
			Description: "Outbound work the server enqueues for workers; message is deleted on ACK.",
			Subjects:    []string{"cairn.jobs.>"},
			Retention:   jetstream.WorkQueuePolicy,
			Storage:     jetstream.FileStorage,
			Discard:     jetstream.DiscardNew,
			Duplicates:  5 * time.Minute,
			Replicas:    replicas,
		},
		{
			Name:        StreamResults,
			Description: "Worker results bound for the ingest-router; kept until every consumer ACKs.",
			Subjects:    []string{"cairn.results.>"},
			Retention:   jetstream.InterestPolicy,
			Storage:     jetstream.FileStorage,
			Discard:     jetstream.DiscardOld,
			Duplicates:  1 * time.Minute,
			Replicas:    replicas,
		},
		{
			Name:        StreamEvents,
			Description: "Domain events; 7-day replay window for async followups + audit.",
			Subjects:    []string{"cairn.events.>"},
			Retention:   jetstream.LimitsPolicy,
			Storage:     jetstream.FileStorage,
			Discard:     jetstream.DiscardOld,
			MaxAge:      7 * 24 * time.Hour,
			Duplicates:  1 * time.Minute,
			Replicas:    replicas,
		},
		{
			Name:        StreamDLQ,
			Description: "Dead-lettered messages from MaxDeliver advisories; 30-day retention for inspection.",
			Subjects:    []string{"cairn.dlq.>"},
			Retention:   jetstream.LimitsPolicy,
			Storage:     jetstream.FileStorage,
			Discard:     jetstream.DiscardOld,
			MaxAge:      30 * 24 * time.Hour,
			Replicas:    replicas,
		},
		{
			Name: StreamWebhooks,
			Description: "Inbound webhook events forwarded raw from /webhooks/<provider>; " +
				"provider-specific decoding happens in the worker subscriber. " +
				"WorkQueue retention — message dropped on ACK.",
			Subjects:   []string{"cairn.webhooks.>"},
			Retention:  jetstream.WorkQueuePolicy,
			Storage:    jetstream.FileStorage,
			Discard:    jetstream.DiscardOld,
			MaxAge:     24 * time.Hour, // drop unprocessed webhooks after a day
			Duplicates: 5 * time.Minute,
			Replicas:   replicas,
		},
	}
}

// natsBootstrapKVConfigs declares the canonical KV buckets.
func natsBootstrapKVConfigs(replicas int) []jetstream.KeyValueConfig {
	return []jetstream.KeyValueConfig{
		{
			Bucket:      KVWorkerPresence,
			Description: "Per-worker heartbeat record; 60s TTL evicts crashed instances.",
			TTL:         60 * time.Second,
			History:     1,
			Replicas:    replicas,
		},
		{
			Bucket: KVWorkerManifests,
			// TTL backstops the presence-driven reaper: workers re-publish their
			// manifest on a slow cadence (well inside this window), so a live
			// worker's manifest stays fresh while a decommissioned one expires
			// instead of lingering forever and replaying through the
			// update-available watcher on every restart.
			Description: "Worker manifests, last 5 retained for diff-on-rollback; expired if a worker stops re-publishing.",
			TTL:         15 * time.Minute,
			History:     5,
			Replicas:    replicas,
		},
		{
			Bucket:      KVRateLimits,
			Description: "Shared rate-limit counters per provider; 15min TTL covers the typical short rolling window most providers enforce.",
			TTL:         15 * time.Minute,
			History:     1,
			Replicas:    replicas,
		},
		{
			Bucket:      KVCoreInfo,
			Description: "Core's release under key info; workers refuse to run against another major.minor.",
			History:     1,
			Replicas:    replicas,
		},
		{
			Bucket:      KVBlobHandles,
			Description: "Server-signed opaque handles workers exchange for fresh presigned URLs.",
			TTL:         7 * 24 * time.Hour,
			History:     1,
			Replicas:    replicas,
		},
	}
}

// natsBootstrapObjectStoreConfigs declares the canonical Object-Store buckets.
func natsBootstrapObjectStoreConfigs(replicas int) []jetstream.ObjectStoreConfig {
	return []jetstream.ObjectStoreConfig{
		{
			Bucket:      OSTransient,
			Description: "Transport-spillover for oversized JetStream payloads; not for long-term storage.",
			MaxBytes:    1 << 30, // 1 GiB
			TTL:         1 * time.Hour,
			Replicas:    replicas,
		},
	}
}

// ---------------------------------------------------------------------------
// port.KV wrapper
// ---------------------------------------------------------------------------

type kvHandle struct {
	kv jetstream.KeyValue
}

func (h *kvHandle) Get(ctx context.Context, key string) (port.KVEntry, error) {
	entry, err := h.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return port.KVEntry{}, port.ErrKVKeyNotFound
		}
		return port.KVEntry{}, fmt.Errorf("kv get %s: %w", key, err)
	}
	return port.KVEntry{
		Key:       entry.Key(),
		Value:     entry.Value(),
		Revision:  entry.Revision(),
		CreatedAt: entry.Created(),
	}, nil
}

func (h *kvHandle) Keys(ctx context.Context) ([]string, error) {
	keys, err := h.kv.Keys(ctx)
	if err != nil {
		// An empty bucket returns ErrNoKeysFound — treat as no keys.
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("kv keys: %w", err)
	}
	return keys, nil
}

func (h *kvHandle) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	rev, err := h.kv.Put(ctx, key, value)
	if err != nil {
		return 0, fmt.Errorf("kv put %s: %w", key, err)
	}
	return rev, nil
}

// CompareAndSet writes if the current revision matches expectedRev. On
// mismatch returns (newRev=0, ok=false, err=nil) so the caller's retry
// loop sees a clear retry signal.
func (h *kvHandle) CompareAndSet(
	ctx context.Context,
	key string,
	value []byte,
	expectedRev uint64,
) (uint64, bool, error) {
	rev, err := h.kv.Update(ctx, key, value, expectedRev)
	if err == nil {
		return rev, true, nil
	}
	// jetstream's Update returns a wrapped error on rev-mismatch; the
	// API JSAPI code is 10071 ("wrong last sequence"). We use the
	// errors.Is path against ErrKeyExists for the create-vs-update case
	// and otherwise treat any non-nil err as "retry".
	if errors.Is(err, jetstream.ErrKeyExists) {
		return 0, false, nil
	}
	// For other CAS-mismatch causes the jetstream package returns a
	// jetstream.APIError with ErrorCode 10071. Detect heuristically by
	// matching the string — adapter-internal, not on the API surface.
	if isWrongLastSeqErr(err) {
		return 0, false, nil
	}
	return 0, false, fmt.Errorf("kv update %s: %w", key, err)
}

func (h *kvHandle) Delete(ctx context.Context, key string) error {
	if err := h.kv.Delete(ctx, key); err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil
		}
		return fmt.Errorf("kv delete %s: %w", key, err)
	}
	return nil
}

func (h *kvHandle) Watch(ctx context.Context, key string) (port.KVWatcher, error) {
	w, err := h.kv.Watch(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("kv watch %s: %w", key, err)
	}
	out := make(chan port.KVEntry, 8)
	go func() {
		defer close(out)
		for upd := range w.Updates() {
			if upd == nil {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case out <- port.KVEntry{
				Key:       upd.Key(),
				Value:     upd.Value(),
				Revision:  upd.Revision(),
				CreatedAt: upd.Created(),
			}:
			}
		}
	}()
	return &kvWatcher{src: w, ch: out}, nil
}

type kvWatcher struct {
	src jetstream.KeyWatcher
	ch  <-chan port.KVEntry
}

func (w *kvWatcher) Updates() <-chan port.KVEntry { return w.ch }
func (w *kvWatcher) Close(_ context.Context) error {
	if w.src != nil {
		return w.src.Stop()
	}
	return nil
}

// isWrongLastSeqErr classifies the jetstream CAS-failure case.
// jetstream.ErrKeyExists matches the create-collision; a separate
// "wrong last sequence" error matches the update-version-mismatch.
// Both are non-fatal for the CAS contract.
func isWrongLastSeqErr(err error) bool {
	if err == nil {
		return false
	}
	// jetstream.APIError.ErrorCode 10071 = "wrong last sequence"
	var apiErr *jetstream.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode == 10071 {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// port.ObjectStore wrapper
// ---------------------------------------------------------------------------

type objectStoreHandle struct {
	os jetstream.ObjectStore
}

func (h *objectStoreHandle) Put(
	ctx context.Context,
	key string,
	data []byte,
	meta port.ObjectMeta,
) error {
	objMeta := jetstream.ObjectMeta{
		Name:        key,
		Description: "",
	}
	if meta.Headers != nil {
		objMeta.Metadata = meta.Headers
	}
	if _, err := h.os.PutBytes(ctx, key, data); err != nil {
		return fmt.Errorf("object store put %s: %w", key, err)
	}
	return nil
}

func (h *objectStoreHandle) Get(
	ctx context.Context,
	key string,
) ([]byte, port.ObjectMeta, error) {
	data, err := h.os.GetBytes(ctx, key)
	if err != nil {
		return nil, port.ObjectMeta{}, fmt.Errorf("object store get %s: %w", key, err)
	}
	info, _ := h.os.GetInfo(ctx, key)
	meta := port.ObjectMeta{
		SizeBytes: int64(len(data)),
	}
	if info != nil {
		meta.SizeBytes = int64(min(info.Size, math.MaxInt64))
		if info.Metadata != nil {
			meta.Headers = info.Metadata
		}
	}
	return data, meta, nil
}

func (h *objectStoreHandle) Delete(ctx context.Context, key string) error {
	if err := h.os.Delete(ctx, key); err != nil {
		return fmt.Errorf("object store delete %s: %w", key, err)
	}
	return nil
}
