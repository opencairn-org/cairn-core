# Gear (v0.4.0 roadmap)

Gear = the bikes and shoes a user rides and runs in. v0.4.0 makes gear a real
entity: user-managed, imported from providers, attached to activities through
the merge pipeline, with per-gear totals. This document is the design and the
release plan; it is written against core v0.3.3, strava v0.3.2, garmin v0.3.2.

## Where we stand

What exists today is a stub left over from the v0.2.0 schema:

- `gear` table (migration 00005): `kind` (bike/shoe/other), name, brand,
  model, purchase_date, retired, notes, denormalised `total_distance_m` /
  `total_duration_s`. Nothing writes to it.
- `activities.gear_id` (FK, `ON DELETE SET NULL`), round-tripped by the
  activity repo and exposed read-only as `Activity.gear_id` on the API.
  `UpdateActivity` cannot set it. Recompute preserves it as a user-owned
  field (`preserveUserEdits`).
- `capability.DataTypeGear` is in the taxonomy. The Strava worker declares
  `Gear: {Read: true}` in its manifest and its README says it imports gear,
  but `stravaActivity` has no `gear_id` field and nothing is emitted. The
  Garmin worker does not declare it.
- No port, no use case, no service, no UI, no docs.

So the data path is open from both ends and the middle is missing.

## Goals

1. A user can create, edit, retire, merge and delete gear, and set a default
   per activity type.
2. Providers' gear lists are imported and kept in sync; the same physical
   bike known to Strava and Garmin ends up as one Cairn gear.
3. An imported activity gets its gear assigned automatically from the
   winning source, unless the user decided otherwise. Manual and uploaded
   activities get the type default.
4. Per-gear distance, moving time and activity count, correct under
   soft-delete, summary-metric overrides and re-merges.
5. Both workers ship it. The contract change is additive and documented.

Non-goals for 0.4.0: components (chains, wheels, tyres), service reminders,
sharing gear with followers, write-back to providers. These are candidates
for 0.5.

## Domain

### `Gear`

```go
type Gear struct {
    ID           GearID
    UserID       UserID
    Kind         GearKind      // bike | shoe | other
    Name         string
    Brand, Model string
    Notes        string
    PurchaseDate *time.Time
    Retired      bool
    RetiredAt    *time.Time
    // Derived; maintained by the totals follow-up, never by the caller.
    TotalDistanceM  float64
    TotalMovingS    int64
    ActivityCount   int64
    ExternalRefs    []GearExternalRef
    CreatedAt, UpdatedAt time.Time
}

type GearExternalRef struct {
    Provider          string
    ExternalAccountID ExternalAccountID
    ExternalID        string
    ProviderName      string   // display name as the provider has it
    ProviderDistanceM *float64 // provider-reported odometer, audit only
    Primary           bool     // provider's "default" flag for this kind
    LastSeenAt        time.Time
}
```

Invariants (domain, unit-tested): name non-empty; kind valid; retiring
keeps the id valid for attached activities; an external ref belongs to
exactly one gear per `(provider, external_account_id, external_id)`.

`GearKind` maps from activity type: Ride/VirtualRide/EBike… → bike,
Run/Walk/Hike → shoe, everything else → other. The picker filters by it,
the assignment logic does not enforce it (a user may tag a ski tour with
"other" gear).

### Assignment: who decides the activity's gear

`activities.gear_id` stays, plus a new column `gear_set_by` with values
`merge` (default) and `user`.

- `user`: set through `UpdateActivity` (including an explicit "no gear").
  Recompute never touches it. This is the existing `preserveUserEdits`
  behaviour made explicit.
- `merge`: recompute derives it. New field group `gear` in
  `domain.Merge`: each non-detached source carries its provider's gear
  external id (see contract), the recompute use case resolves ids to
  `GearID` through the gear refs of that account, and the policy resolver's
  priority list for the activity type picks the winner among sources that
  have a resolved gear. A `field_source_overrides` row with
  `field_key = 'gear'` pins it to a source like any other group.
- Manual create and file upload: no source has gear, so the create use case
  applies the user's default for the activity type once, with `gear_set_by =
  merge` (so a later provider source can still win, and a default change does
  not rewrite history).

Resolution is a pure function in the domain (`ResolveGear(sources,
refs, policy)`); the use case only supplies the lookup map.

### Defaults

`gear_defaults(user_id, activity_type, gear_id)`, unique per `(user,
type)`. Provider "primary" flags seed a default only when the user has none
for that type.

### Merging two gear records

Two providers produce two records for one bike. `MergeGear(from, to)`:
moves external refs and `gear_defaults`, repoints `activities.gear_id`,
deletes `from`, recomputes totals of `to`. Only within one user. This is the
multi-source story for gear and the reason refs are a separate table rather
than columns on `gear`.

### Totals

A follow-up stage in `pipeline.Pipeline` (nil-tolerant, logs on failure, never
fails ingest): after recompute, if the activity's gear changed or its summary
metrics changed, recompute totals for the old and the new gear with one
aggregate query over live activities (`deleted_at IS NULL`), using the
post-override distance and moving time. Also triggered by `UpdateActivity`,
`DeleteActivity`/undelete, `MergeGear`, and an admin "rebuild gear totals"
op. No triggers: soft-delete and the override overlay make a trigger
either wrong or a second implementation of the override rules.

### Visibility

Gear is owner-only in 0.4.0. It is not part of any visibility category, so
feed, public profile, share links and federation never expose it. Adding a
`gear` category to the per-field model is a 0.5 decision.

## Worker contract (`cairn.worker.v1`, additive)

```proto
enum WorkerEventType { … WORKER_EVENT_TYPE_GEAR = 6; }

message WorkerEvent { oneof entity { … ImportedGear gear = 7; } }

message ImportedGear { ExternalRef ref = 1; GearImport payload = 2; }

message GearImport {
  GearKind kind = 1;          // BIKE | SHOE | OTHER
  string name = 2;
  string brand = 3;
  string model = 4;
  string description = 5;
  bool retired = 6;
  bool primary = 7;           // provider default for this kind
  optional double distance_m = 8;  // provider odometer, audit
  // Provider defaults per activity type (Garmin has them; Strava has
  // one primary per kind). Native activity type strings as the provider
  // names them; the worker maps them to cairn.v1.ActivityType.
  repeated cairn.v1.ActivityType default_for = 9;
}

// cairn.v1.ActivitySourcePayload
string gear_external_id = 22;   // provider gear id on this activity
string gear_name = 23;          // display hint until the gear is imported

enum JobType { … JOB_TYPE_IMPORT_GEAR = 11; }
message ImportGearPayload {}   // full gear list of the account, incl. retired
message Job { oneof payload { … ImportGearPayload import_gear = 18; } }
```

Rules, added to the provider contract doc (§3 subjects, §5 results):

- A worker whose manifest declares `Gear` MUST consume
  `cairn.jobs.import_gear.<provider>` and answer with GEAR events for every
  gear on the account, retired included.
- Any JobResult MAY carry GEAR events; they precede the activity that
  references them, like segments. Core upserts by `(external_account_id,
  external_id)` and memoises them in the ingest bundle.
- `gear_external_id` on the source payload MAY reference gear not present in
  the bundle; core then stores the id and resolves it when the gear arrives
  (an import_gear run, or a later activity that carries it). `gear_name`
  lets the UI show something meanwhile.
- Gear is optional data: a worker MUST NOT fail or delay an activity import
  because the gear lookup failed.

Core publishes `import_gear` on account connect, on the "Sync gear" button
(same pattern as the Garmin health import in `internal/usecase/providerlink/jobs.go`)
and on every scheduled reconcile, since the list is small and the endpoint
cheap.

### Compatibility, and one fix core needs first

`cmd/server/result_router.go` decodes results with plain
`protojson.Unmarshal`, which rejects unknown fields. A 0.4.0 worker sending
`gear` events or `gear_external_id` to a 0.3.x core would fail every
import. Two consequences:

1. Core 0.4.0 switches to `protojson.UnmarshalOptions{DiscardUnknown: true}`
   so this class of additive change is safe from now on.
2. Upgrade order for 0.4.0 is core first, then workers. Docs say so under
   "Upgrading to 0.4.0". Core 0.4.0 with 0.3.x workers is fine: no gear
   events, nothing happens.

`make proto-breaking` (buf against the previous tag) guards the contract;
all changes above are additions.

## Workers

### Strava

Strava's `DetailedActivity` has `gear_id` and a `gear` summary (`id`, `name`,
`primary`, `distance`); `/athlete` lists `bikes[]` and `shoes[]`;
`/gear/{id}` gives `brand_name`, `model_name`, `frame_type`, `description`,
`retired`.

- `fetch_source`: map `gear_id` → `gear_external_id`, `gear.name` →
  `gear_name`, and emit one GEAR event from the summary (kind from the
  `b`/`g` id prefix, name, primary, distance). Zero extra API calls.
- `import_gear`: `/athlete` for the list, `/gear/{id}` per item for
  brand/model/retired. A user has a handful of gear; this stays well inside
  the 100/15 min limit. Rate limiting through the existing `reserveRead`.
- Existing raw blobs already contain `gear_id`, so `parse_blob` (quota-free
  re-parse) backfills gear on every archived Strava activity after the
  upgrade. That makes the README claim true.

### Garmin

`garminconnect` exposes `get_gear(userProfileNumber)`, `get_gear_stats`,
`get_gear_defaults(userProfileNumber)` and `get_activity_gear(activity_id)`.
The activity summary does not carry gear, so attribution costs one extra
call per activity.

- `fetch_source`: call `get_activity_gear`; on any error log and continue
  without gear. Config `CAIRN_GARMIN_FETCH_ACTIVITY_GEAR` (default `true`)
  to switch it off for accounts that hit Garmin's opaque limits.
- `import_gear`: `get_gear` + `get_gear_defaults`, mapping `gearTypeName`
  (Bike/Shoes/Other) to kind, `gearStatusName` to retired,
  `customMakeModel` to brand/model, defaults to `default_for`.
- Raw blobs do not contain gear, so backfill for existing Garmin
  activities needs a reimport, not a reparse. The docs say which.
- Manifest gains `"Gear": {"read": True, "backfill": True, "granularity":
  "per-account"}`.
- Proto regen in the build-base image, as for every contract change.

## Core: API and UI

### Connect

New `GearService` (`api/proto/cairn/v1/gear.proto`): `ListGear`
(`include_retired`), `GetGear`, `CreateGear`, `UpdateGear` (field mask;
`retired` toggles here), `DeleteGear`, `MergeGear`, `SetGearDefault`,
`ClearGearDefault`, `ListGearDefaults`, `SyncGear(external_account_id)`.
`Gear` on the wire carries its refs and totals.

Changes on existing messages: `UpdateActivityRequest.gear_id = 18` under
the field mask (`""` means "no gear", and either sets `gear_set_by =
user`); `UpdateActivityRequest.reset_gear = 19` returns it to `merge`;
`Activity.gear_set_by = 28` and `Activity.gear_name = 29` (denormalised for
list rendering); `ListActivitiesRequest.gear_ids = 9` filter.

Export (`internal/export`) adds the gear name to GPX/FIT metadata where the
format has a slot; nothing else changes.

### Web

- `/gear`: active and retired lists with totals, create/edit dialog,
  retire, merge ("this is the same as…"), delete with a count of affected
  activities, provider badges from refs, "Sync gear" per connected account
  that declares `Gear`, defaults per activity type.
- Activity detail: gear line with "auto" or "set by you" and a reset.
- Activity edit and `/activities/new`: picker filtered by kind, retired
  hidden unless already attached.
- Activities list: gear filter chip.
- Connections: the `Gear` capability shows up through the manifest-driven
  data-type list as it does for the others.
- Paraglide messages in `en` and `de`.

## Persistence

Migration `00063_gear.sql`:

```sql
ALTER TABLE gear
    ADD COLUMN retired_at timestamptz,
    ADD COLUMN activity_count bigint NOT NULL DEFAULT 0;
ALTER TABLE gear RENAME COLUMN total_duration_s TO total_moving_s;

CREATE TABLE gear_external_refs (
    gear_id             uuid NOT NULL REFERENCES gear(id) ON DELETE CASCADE,
    provider            text NOT NULL,
    external_account_id uuid NOT NULL REFERENCES external_accounts(id) ON DELETE CASCADE,
    external_id         text NOT NULL,
    provider_name       text NOT NULL DEFAULT '',
    provider_distance_m double precision,
    is_primary          boolean NOT NULL DEFAULT false,
    last_seen_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, external_account_id, external_id)
);
CREATE INDEX gear_external_refs_gear_idx ON gear_external_refs (gear_id);

CREATE TABLE gear_defaults (
    user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    activity_type text NOT NULL,
    gear_id       uuid NOT NULL REFERENCES gear(id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, activity_type)
);

ALTER TABLE activities
    ADD COLUMN gear_set_by text NOT NULL DEFAULT 'merge'
        CHECK (gear_set_by IN ('merge', 'user'));
UPDATE activities SET gear_set_by = 'user' WHERE gear_id IS NOT NULL;
CREATE INDEX activities_user_gear_idx ON activities (user_id, gear_id)
    WHERE deleted_at IS NULL AND gear_id IS NOT NULL;
```

The source's provider gear id lives in `activity_sources.parsed` (the
payload jsonb) like every other source field; no new column. Resolution
reads it through the domain source type.

Layering, following `attachment`: `internal/domain/gear.go`,
`internal/port/gear.go`, `internal/adapter/secondary/postgres/gear_repo.go`
(+ inmem for tests), `internal/usecase/gear/` (crud, merge, defaults,
totals, sync), `internal/adapter/primary/connect/gear.go`, wiring
in `cmd/server/wire.go` only.

## Tests

- Domain: `ResolveGear` across policy orders, overrides, missing refs;
  `gear_set_by` semantics in recompute; merge-gear invariants; kind mapping.
- Postgres integration: ref uniqueness, `MergeGear` repoints and cascades,
  totals under soft-delete and summary overrides, `ON DELETE SET NULL` on
  gear delete.
- Ingest: GEAR event before/after the activity in one bundle, unknown gear
  id resolved later, `DiscardUnknown` with a payload carrying a field core
  does not know.
- Conformance harness (contract doc §10): fixtures gain a GEAR event and a
  source with `gear_external_id`.
- Workers: Strava `DetailedActivity` fixture with `gear`, `/athlete` and
  `/gear/{id}` fixtures; Garmin `get_gear`, `get_gear_defaults`,
  `get_activity_gear` fixtures plus the error-tolerance path.
- Web: picker filtering and the auto/user state on the detail page.

## Release plan

| Order | Repo | PR | Notes |
|---|---|---|---|
| 1 | cairn-core | `feat(proto): gear events, import_gear job, source gear ref` + `DiscardUnknown` | contract first, so the workers can sync stubs; `make proto-breaking` green |
| 2 | cairn-core | `feat(gear): domain, persistence, GearService, merge assignment, totals` | migration 00063; admin "rebuild gear totals" op |
| 3 | cairn-core | `feat(web): gear page, pickers, filters` | |
| 4 | cairn-core | release **v0.4.0** | changelog: Added gear; Changed: unknown result fields ignored; upgrade note: core before workers |
| 5 | cairn-provider-strava | `feat: gear import and per-activity gear` → **v0.4.0** | `scripts/sync-proto.sh ../cairn-core` |
| 6 | cairn-provider-garmin | `feat: gear import and per-activity gear` → **v0.4.0** | proto regen in build-base; new env var in docs |
| 7 | cairn-docs | **0.4.0**: Using Cairn → Gear; Provider contract §3/§5 gear rules; Providers page per worker; "Upgrading to 0.4.0" | reparse (Strava) vs reimport (Garmin) backfill |
| 8 | homelab | bump pins core → strava → garmin in that order | |

Minor version because the contract and the API grow; nothing breaks for a
user who never touches gear.

## Open decisions

1. Garmin per-activity gear call on by default, or opt-in? Proposed: on,
   with the env switch. Garmin's limits are undocumented; one extra call per
   activity doubles the backfill call count.
2. `kind` stays `bike | shoe | other`. Wheels, trainers, skis and boards
   fall under `other` until components arrive. Alternative: add `ski` and
   `board` now, since the picker filters on kind. Proposed: keep three,
   kind is cheap to extend later.
3. Totals count moving time (post-override). Elapsed is available on the
   activity if anyone wants it later; storing both now is cheap. Proposed:
   moving only.
4. Should `MergeGear` be reversible? Proposed: no; it is a user-confirmed
   action with a preview of what moves.

## Implementation notes (2026-10-21, PR #157)

Deviations from the design above, decided during implementation:

- **Pending provider records.** A provider gear record Cairn has not seen is
  not auto-created. It is stored as a pending ref (`gear_external_refs.gear_id`
  is nullable, `provider_import` keeps the provider's description) and raises
  a `gear_decision_needed` notification. The user creates gear from it
  (`CreateGearFromRef`), maps it onto existing gear (`LinkGearRef`), or leaves
  it (`ListPendingGearRefs`). Defaults are seeded and activities re-derived on
  the decision, not on import. The owner of a ref is the external account's
  user; a worker ref whose `user_id` or provider disagrees is rejected.
- **`gear_set_by` is an enum** on the wire (`GearSetBy`), and `reset_gear` is
  a mask-only key on `UpdateActivity`. Deleting gear hands its activities back
  to the merge and re-derives them.
- **One sync surface each:** `POST /api/accounts/{id}/sync-gear` (no
  `GearService.SyncGear`) and the feed's `?gear_id=` filter (no
  `ListActivitiesRequest.gear_ids`).
- **Totals** refresh inside `RecomputeActivityFromSources` whenever the
  attachment changed, plus after merge, link, create-from-ref and delete; no
  per-handler follow-up.
- **Manual entry** accepts `gear_id` in the create body.
