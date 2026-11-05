package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// startManifestWatcher launches a goroutine watching the
// cairn_worker_manifests KV bucket. Every time a worker writes a fresh
// manifest (typically on startup; possibly on hash-change during a
// rolling deploy), the watcher fires the mark-out-of-date pipeline.
//
// Concretely: when a worker publishes a manifest with a HIGHER parser version
// (docs/adr/0005) than the one that last parsed a source (same provider + package), every such
// activity_source is flipped to reimport_status='update_available'. The
// next reconcile / admin-trigger then enqueues parse_blob.<provider>
// jobs to bring those sources up to date.
//
// Gated on app.NATSBus; returns (nil, nil) when NATS isn't wired.
//
// The watcher is intentionally fail-soft: any error in a single
// update is logged and the loop continues. The KV-watch reconnects
// transparently across NATS reconnects.
func startManifestWatcher(
	ctx context.Context,
	app *App,
	logger *slog.Logger,
) (port.KVWatcher, error) {
	if app.NATSBus == nil {
		return nil, nil
	}
	log := logger.With("component", "manifest_watcher")

	kv, err := app.NATSBus.KV("cairn_worker_manifests")
	if err != nil {
		return nil, fmt.Errorf("kv worker_manifests: %w", err)
	}

	// "*" matches every key in the bucket (each key is a worker_name).
	// The watch starts from "current state" — every active worker's
	// manifest immediately replays, which is what we want at boot
	// (catch any drift that accumulated while the server was down).
	watcher, err := kv.Watch(ctx, "*")
	if err != nil {
		return nil, fmt.Errorf("watch worker_manifests: %w", err)
	}

	goSafe(log, "manifest watcher", func() { manifestWatchLoop(ctx, app, log, watcher) })

	return watcher, nil
}

// manifestWatchLoop applies manifest updates until ctx ends or the watch closes.
func manifestWatchLoop(ctx context.Context, app *App, log *slog.Logger, watcher port.KVWatcher) {
	log.Info("manifest watcher started")
	for {
		select {
		case <-ctx.Done():
			log.Info("manifest watcher shutting down")
			return
		case entry, ok := <-watcher.Updates():
			if !ok {
				return
			}
			if len(entry.Value) == 0 {
				continue // tombstone (worker offboarded) — nothing to do
			}
			handleManifestUpdate(ctx, app, log, entry)
		}
	}
}

func handleManifestUpdate(ctx context.Context, app *App, log *slog.Logger, entry port.KVEntry) {
	var m manifestPayloadWire
	if err := json.Unmarshal(entry.Value, &m); err != nil {
		log.Warn("decode manifest payload failed", "key", entry.Key, "error", err)
		return
	}
	// The update-available trigger keys on (provider, package, version) — the
	// routing name/alias is irrelevant. Legacy manifests without provider/
	// package can't drive it; skip them (their sources stay 'current').
	if m.Provider == "" || m.Package == "" {
		return
	}
	// A pool name is bound to one package for good (docs/adr/0006).
	if app.WorkerPools != nil {
		if err := app.WorkerPools.Upsert(ctx, domain.WorkerPool{Provider: m.Provider, Name: m.WorkerName, Package: m.Package}); err != nil {
			log.Error("worker pool refused", "worker_name", m.WorkerName, "provider", m.Provider, "package", m.Package, "error", err)
			return
		}
	}
	applyProviderMeta(ctx, app, log, m)
	version, err := domain.ParseParserVersion(m.Version)
	if err != nil {
		log.Warn("manifest version is not a parser version N.M; cannot drive update-available",
			"worker_name", m.WorkerName, "version", m.Version)
		return
	}

	// Pure-SQL: flag every source from the same package+provider parsed by an
	// older version. Idempotent — re-running for the same version is a no-op.
	updated, err := app.Activities.MarkSourcesOutOfDate(ctx, m.Provider, m.Package, version)
	if err != nil {
		log.Warn("mark sources out-of-date failed",
			"provider", m.Provider, "package", m.Package, "version", version, "error", err)
		return
	}
	if updated > 0 {
		log.Info("flagged sources out of date (newer parser)",
			"provider", m.Provider, "package", m.Package, "version", version, "count", updated)
	}
}

// applyProviderMeta stores the manifest's provider_meta (display, auth,
// OAuth authorize endpoint, connect-form fields), bound to the package like
// a pool name. Invalid or foreign-package meta is logged and ignored.
func applyProviderMeta(ctx context.Context, app *App, log *slog.Logger, m manifestPayloadWire) {
	if m.ProviderMeta == nil || app.Providers == nil {
		return
	}
	meta := *m.ProviderMeta
	meta.ID = m.Provider
	if err := meta.Validate(); err != nil {
		log.Warn("provider meta rejected", "worker_name", m.WorkerName, "provider", m.Provider, "error", err)
		return
	}
	if err := app.Providers.Upsert(ctx, meta, m.Package); err != nil {
		log.Warn("provider meta refused", "worker_name", m.WorkerName, "provider", m.Provider, "package", m.Package, "error", err)
		return
	}
	app.invalidateProviders()
	log.Info("provider meta stored", "provider", m.Provider, "package", m.Package, "auth_method", meta.AuthMethod)
}

// reapplyProviderMeta stores the provider_meta of the provider's live
// manifests again, after an admin unbound the provider: the pool serving it
// now binds right away instead of on its next publish.
func reapplyProviderMeta(ctx context.Context, app *App, log *slog.Logger, provider string) {
	if app.NATSBus == nil {
		return
	}
	kv, err := app.NATSBus.KV(domain.WorkerManifestBucket)
	if err != nil {
		log.Warn("reapply provider meta: manifests bucket", "provider", provider, "error", err)
		return
	}
	keys, err := kv.Keys(ctx)
	if err != nil {
		log.Warn("reapply provider meta: list manifests", "provider", provider, "error", err)
		return
	}
	for _, key := range keys {
		entry, err := kv.Get(ctx, key)
		if err != nil {
			continue
		}
		var m manifestPayloadWire
		if json.Unmarshal(entry.Value, &m) != nil || m.Provider != provider {
			continue
		}
		applyProviderMeta(ctx, app, log, m)
	}
}

// manifestPayloadWire is the JSON shape workers write to the
// cairn_worker_manifests KV bucket. Keep in sync with the worker SDKs'
// manifest payload.
type manifestPayloadWire struct {
	WorkerName string `json:"worker_name"`
	Version    string `json:"version"`
	Provider   string `json:"provider"`
	Package    string `json:"package"`
	UpdatedAt  string `json:"updated_at"`
	// ProviderMeta is what the worker declares about connecting its
	// provider; optional (older workers, parser-only pools).
	ProviderMeta *domain.ProviderMeta `json:"provider_meta,omitempty"`
}
