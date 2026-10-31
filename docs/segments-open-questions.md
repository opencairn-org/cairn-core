# Segments — design decisions and open questions

How segment matching works today, and the decisions still open.

## Current behaviour

- **Geometric matching runs for every source, every provider.** The
  matcher (`internal/usecase/segment/match.go`) is a follow-up of every
  ingest (import, re-import, webhook, file upload, manual) via
  `pipeline.Pipeline` (`IngestSource`), and of the manage-endpoint recompute
  (`Pipeline.RecomputeDerived`). It works off the stored stream, so a
  Garmin activity matches Strava-mirrored segments and vice versa.
- **Candidates cover all segment origins** (`ListSegmentCandidatesForActivity`):
  provider-mirrored (`source='external'`, scoped to the user's own external
  accounts), user-private natives, and instance-shared natives.
- **Provider references are kept.** External segments carry
  `(external_account_id, external_id)`; re-imports find-or-create by that key
  and refresh geometry in place (`ingestSegmentEvent`).
- **Match quality**: corridor walk with per-segment-overridable tolerances
  (default corridor 15 m, start/end 25 m), monotonic vertex progression,
  ≤5-sample drift budget, true point-to-edge distance (works for sparse
  polylines). Efforts are wiped-and-rewritten per source, so re-runs are
  idempotent.

## Open: reacting to segment changes

When a segment is created or its geometry/tolerances change, existing
activities are not re-evaluated; matching only runs when a *source* is
(re)ingested or explicitly recomputed. Open decisions:

- Backfill matching for a NEW segment: run the reverse query (activities
  whose bbox intersects the segment) once on segment creation? Bounded by
  activity count; needs a job/queue, not a request-path loop.
- Geometry edit: wipe the segment's efforts and re-match affected
  activities, or version the geometry and keep old efforts?
- Deleted segment: cascade efforts (current FK behavior?) and ranks.
- Cost control: instance-wide re-match is O(activities × segments); needs
  batching + the import-queue-style pacing.

## Decided: provider-reported vs matcher-found efforts

Canonical rule, implemented in `ingestSegmentEffortEvent`:

- **GPS-bearing source → the matcher owns the efforts.** A provider-reported
  effort never inserts a row; it only stamps its external id onto the
  overlapping matcher effort (`AttachProviderEffortRef`, start-time proximity
  ≤60 s). A provider effort with no overlapping matcher effort is dropped and
  logged (`provider effort has no matching matcher effort`) — that log line is
  the tuning signal for segment geometry/tolerances.
- **Streamless / no-GPS source → the provider effort is stored as-is** (the
  matcher cannot run there; wipe-and-rewrite doesn't apply since the NoStream
  path only clears efforts when a stream was lost on reimport).

One traversal, one row — leaderboards can't double-count. Note the provider
id enrichment is lost when the matcher rewrites a source's efforts and is
re-applied on the next provider seg-message (reimport); acceptable since the
id is informational.
