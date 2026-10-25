# Worker pooling vs. version separation — design decision

> **Superseded in part (2026-10-25):** the "matching build" re-parse rule and the
> integer `version` below are replaced by release / dataset / parser versions,
> see [ADR 0005](adr/0005-provider-versioning.md). Pooling by name still holds.

Status: **decided** (revisited 2026-06-05). This records the model Cairn uses for
running multiple worker instances of the same provider, how versions interact
with that, and the consequences for re-fetch / re-parse.

## The question

A provider (e.g. Strava) is served by a fleet of worker processes. Two ways to
organise them:

1. **Pool by name** — every instance that shares a `worker_name` is
   interchangeable. Jobs are delivered to a NATS queue group; any instance can
   take any job. Version is an *attribute* of an instance, not a routing key.
2. **Separate by version** — each worker version gets its own job subject /
   queue, and the server routes a job to a specific version.

## Decision: pool by name; version is an attribute, not a routing key

Jobs for a provider land on `cairn.jobs.<job>.<provider>` and are consumed by a
single durable pull consumer shared across all instances of that worker
(`worker_name` identifies the durable; instances form the implicit queue
group). Any healthy instance handles any job.

- **`worker_name`** is an admin-assigned, NATS-safe routing label (see #55). It
  is *not* code identity.
- **`version`** (a simple incrementing integer, see #54) and **`manifest_hash`**
  (the build/package fingerprint — content hash of the worker's manifest:
  package + version + config) are *reported* by each instance in its heartbeat
  (`cairn_worker_presence`) and stamped onto every source it imports
  (`activity_sources.source_worker_version` / `source_manifest_hash`).

### Why pooling wins for the normal path

- **Elastic throughput** — scale a provider by adding instances; no per-version
  config or routing. Rolling upgrades just replace instances.
- **No version-routing state** — the server doesn't track "which version should
  get this job". Webhooks, reconcile and backfill all fan to the same pool.
- **Manifest drift, not routing** — when a worker's `manifest_hash` changes
  (KV-watch on `cairn_worker_manifests/<name>`), the server marks affected
  sources `update_available` (CLAUDE.md Gap 1) so they *can* be reprocessed —
  it does not re-route live jobs.

## The tension: re-parse needs a *matching* version (added 2026-06-05, #72)

Re-parse-from-archive (#66) re-runs a stored raw blob through worker mapping
logic *without* calling the provider. #72 established that a blob may only be
re-parsed by a worker whose **provider + version + manifest_hash exactly match
the importer that wrote it** — re-parsing with a *different* build risks an
incompatible parse. So re-parse eligibility is "a *compatible* (same-build)
worker is online", matched on `(provider, version, manifest_hash)`, never on
name.

This sits in tension with pooling + rolling upgrades:

> If every instance is upgraded to vN+1, blobs imported by vN can no longer be
> re-parsed (no online worker matches their recorded build).

That is **acceptable** and intentional, because:

- Re-parse is a recovery/maintenance tool, not a hot path. The hot path is
  re-fetch (pull fresh from the provider), which is version-agnostic — any
  pooled instance re-fetches and the new build re-imports.
- A blob imported by vN re-parsed by vN+1 would produce vN+1's mapping anyway —
  which is exactly what a **re-fetch** gives you, durably, from source data.
- Pinning old versions online solely to re-parse old blobs would defeat the
  point of pooling and rolling upgrades.

### Consequences / guidance

- **Prefer re-fetch over re-parse after an upgrade.** Re-parse is for "re-run
  the *same* build" cases (e.g. recover from a transient ingest glitch, or
  re-apply mapping after a server-side recompute) — not for applying *new*
  worker logic to history. New logic → re-fetch.
- **To re-parse historical blobs with a specific old build,** run an instance of
  that exact version/manifest alongside the pool (same `worker_name` is fine —
  it just needs to be online so `(provider, version, manifest_hash)` matches).
  Its presence makes those sources `reparse_eligible` again.
- **`worker_name` stays a routing label.** Do not overload it to encode version;
  version lives in the heartbeat + on the source. Two instances with the same
  name but different versions are both valid pool members (one can re-parse one
  generation of blobs, the other a different generation).
- **Auto-reprocess** (CLAUDE.md Gap 1/3 — `MarkSourcesOutOfDate` +
  `ReimportAllOutOfDate`) should default to **re-fetch** for out-of-date
  sources, falling back to re-parse only when a matching-build worker is online
  and provider quota is the constraint.

## Summary

| Concern | Mechanism |
| --- | --- |
| Job distribution | Pool by `worker_name` (queue group); version-agnostic |
| Code identity | `version` (int) + `manifest_hash` (build fingerprint), reported per heartbeat, stamped on sources |
| Apply new logic to history | **Re-fetch** (version-agnostic, durable) |
| Re-run the *same* build | **Re-parse**, gated on a matching `(provider, version, manifest_hash)` worker being online |
| Drift detection | KV-watch on `cairn_worker_manifests/<name>` → mark sources `update_available` |

Re-pooling by version would only be worth revisiting if re-parse becomes a hot,
high-volume path that routinely needs many old builds online simultaneously —
which the re-fetch-first guidance is designed to avoid.
