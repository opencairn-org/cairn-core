# Changelog

All notable changes to Cairn are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/), and the project aims to follow
[Semantic Versioning](https://semver.org/). Dates are ISO-8601.

## [Unreleased]

### Added
- `ActivityService.SearchActivities`: the faceted, filterable, sortable list
  behind the web feed (`ActivityFilter` with the full filter bar, facets,
  paging), the Connect counterpart of `GET /api/activities/feed`.
  `ActivitySort` gains `ELEVATION_DESC` and `SPEED_DESC`; `Activity` gains
  `gear_unset`.
- New `StatsService.GetOverview` (dashboard totals, per-sport breakdown,
  newest activities, this/last week with ranges) and `SocialService.ListHomeFeed`
  (own, following and federated rows, projected per the visibility model),
  the counterparts of `GET /api/overview` and `GET /api/feed/home`. Shared
  messages `Facet`, `Totals`, `UserCard`, `ProjectedActivity`, `FeedItem`.
  The REST handlers now render from the same use cases (`usecase/view`).
- Signed binary-content URLs. Photos, static course images and exports are
  handed out as short-lived `ContentUrl`s a client fetches with a plain GET:
  `ActivityService.ListAttachments` (original/medium/thumb + blurhash),
  `GetActivityMapImage`, `GetExportOptions`, `ExportActivity` (rendered on
  GET), and `Activity.map_image` + `map_image_etag` on every activity with a
  track. Stored objects are presigned on `CAIRN_STORAGE_PUBLIC_ENDPOINT` when
  set; otherwise, and always for rendered content, the URL points at the new
  `GET /content/{token}` route (HMAC-signed claims, 15 min). `PUT
  /content/{token}` is the upload counterpart `PresignActivityUpload` uses
  when no public endpoint exists, so the two-step upload works everywhere.
  REST `/api/activities/{id}/attachments` now also carries `urls` per
  variant and `blurhash`.
- Photo variants: uploads and worker-mirrored photos get a 320 px thumb and
  a 1280 px medium JPEG plus a blurhash (migration 00068; pure Go, cgo-free).
  `cairn attachments backfill-variants` derives them for existing photos.
- Cleartext HTTP/2 (h2c, prior knowledge) next to HTTP/1.1, so native Connect
  and gRPC clients multiplex and stream over one connection without a TLS
  proxy in front. Streaming RPCs are exempt from the server-wide
  `CAIRN_HTTP_WRITE_TIMEOUT`; unary calls keep it.
- `AuthService.GetInstanceInfo` (public): release, commit, build time,
  federation/OAuth flags, signup policy and the scope catalog, for a client's
  server picker. `AuthService.GetQuota` (same numbers as `GET /api/quota`),
  `Logout` (cookie session; `all_devices` revokes every session) and
  `Register` (invite-based, mirrors `POST /auth/invite/redeem`; no session is
  issued, the client signs in afterwards).
- OAuth 2.1 loopback redirects per RFC 8252 §7.3: a registered
  `http://127.0.0.1/...` or `http://[::1]/...` redirect matches any port, and
  the authorization code goes to the port the app actually bound. `localhost`
  and custom schemes still match exactly.
- Dynamic client registration stores `software_id` and `software_version`
  (migration 00067) and echoes them, so an authorized-apps view can name the
  application behind a client.
- Scope catalog completed: every family now has `:read` and `:write`
  (`profile:write`, `segments:write`, `training:write`, `gear:read/write`,
  `notifications:read/write`, `connections:read/write`). The read-only default
  grant includes the new read scopes. `docs/api-scopes.md` maps every RPC to
  its scope and each `rpc` carries a `// Scope:` comment.

### Changed
- OAuth tokens are now checked per scope family on Connect: an
  `ActivityService` read needs `activities:read`, `GearService` needs
  `gear:*`, `NotificationService` `notifications:*`, and so on; session-bound
  RPCs (sessions, personal access tokens, `Logout`, `ChangePassword`,
  everything on `AdminService`) refuse OAuth tokens. Before, any read-only
  token could call every read RPC. Tokens issued before this release keep
  working for the families they hold; re-authorize to pick up new scopes.
  REST keeps the coarse read/write rule.
- Declared RPCs without backing code say so in their proto comment and keep
  returning `Unimplemented`: `StreamActivityEvents`, the source-preview trio,
  custom best efforts, `Complete2FA`; browser-only flows (`LoginPassword`,
  `StartOIDCLogin`, `StartOIDCLink`) point at their REST routes.
- Connect `ActivityService` now implements `ListActivitySources`,
  `DeleteActivity`, `DetachSource` (incl. `keep_as_new_activity` = split),
  `LinkSourceToActivity`, `ReimportSource` (`prefer_local_reparse` = re-parse
  from the archive), `ListRecords`, `PresignActivityUpload` and
  `UploadActivity`. They call the same use cases as the REST handlers
  (`/sources/{id}/detach|reimport`, `/split`, `/merge`, `/upload`,
  `/api/records`), so the two surfaces behave identically. Still
  unimplemented: `StreamActivityEvents`, the source-preview trio and custom
  best efforts.
- `DELETE /api/activities/{id}` soft-deletes an activity (every source
  detached and denylisted, remote followers told). Before, the only way was
  detaching the sources one by one.
- `PresignActivityUpload` + `UploadActivity` give native clients a two-step
  upload via object storage (`uploads/<user>/<id>.<ext>`, 15 min PUT URL);
  needs the S3 endpoint to be reachable by the client, which the REST
  multipart upload does not.

### Fixed
- Connect streaming RPCs went through the session, scope and admin
  interceptors unauthenticated (the chain only wrapped unary calls). No
  streaming RPC was implemented, so nothing was exposed; the chain now covers
  every stream type.
- A v* tag now also publishes the GitHub release: the tag's CHANGELOG.md
  section as notes plus the image digest and verify commands, with an SPDX
  SBOM of that digest (syft) attached. Before, the release object was created
  by hand (and skipped for 0.5.x).

## [0.5.1] — 2026-10-25

Core-only patch; the 0.5.0 workers stay compatible.

### Fixed
- "Re-download all" on a full sync re-fetches activities that an earlier sync
  already imported. Before, the queue kept their finished rows and only
  activities never queued were fetched (923 discovered, 15 queued), so the
  Garmin gear backfill after 0.5.0 never ran.

## [0.5.0] — 2026-10-25

Upgrade order: Core first, then both workers to 0.5.0 (the contract changed:
jobs, results and the account lookup are now per worker pool, see
docs/adr/0006; a 0.4 worker keeps running against a 0.5 core but gets no
new jobs). Jobs queued before the upgrade are drained by the 0.5 workers.
Sources imported before 0.5 are flagged "newer parser available"; the new
"Re-parse N out of date" action on Connections brings them up to date
without provider quota. Garmin activities need a re-fetch to pick up gear.

### Added
- Provider versioning (docs/adr/0005): `worker_version` is the parser
  version `N.M` (dataset `N` = archive format, `M` = output revision; a
  legacy bare `N` reads as `N.0`). Re-parse goes to the newest live parser of
  the source's package and dataset and carries `min_parser_version`; ingest
  rejects results that would lower a source's version. `JobResult` gains
  `worker_release`, stamped on sources (migration 00064). Manifest updates
  flag older sources `update_available` with reason `newer_parser` (re-parse)
  or `newer_dataset` (re-fetch); `POST /api/accounts/{id}/reparse-out-of-date`
  re-parses them in bulk, and the Connections page shows the counts.
- Release compatibility: core publishes its release in the `cairn_core` KV
  bucket. Before 1.0 a worker needs the same major.minor; from 1.0 on the
  same major and a minor not above core's. Incompatible workers are flagged
  in admin and ignored for routing.
- Worker pools (docs/adr/0006): jobs on
  `cairn.jobs.<job>.<provider>.<worker_name>`, discovery, results and the
  webhook account lookup per pool; core checks each result against the pool
  named in the subject, the pool's registered package (`worker_pools`,
  migration 00066; a name is bound to one package for good) and the account's
  pool. Admins list and retire pools (`/api/admin/worker-pools`).
- One connection per pool per provider account (migration 00065). Accounts
  are assigned to a pool (automatically when the provider has one; a picker
  when it has several); owners may move a connection
  (`PUT /api/accounts/{id}/worker`, confirmation when the implementation
  changes, which flags the connection's activities `pool_changed`).
- Merge priority entries may name a pool, `<provider>.<worker_name>`; the
  matcher allows one source per pool, so two importers of one provider feed
  one activity. A gear record mapped on one connection maps the same provider
  gear id on the account's other connections.
- Rate budgets per provider app: the token reply carries `app_id`; budgets
  are read from `<provider>.<app>.<window>`.
- Gear: an unknown provider gear id on an activity becomes a pending
  decision (before, it waited for an `import_gear` run). Activities without
  gear show a "Gear?" prompt; "No gear" dismisses it. Payload `gear_unknown`
  keeps a source's previous gear on re-import.
- Shared header with navigation on the activity, edit and manage pages.

### Changed
- The permission template grants pool-scoped subjects; the pre-0.5
  provider-scoped job subjects stay subscribable through 0.5 (removed in
  0.6), results and lookups are pool-scoped only.
- Manage page shows dataset, parser and release per source and whether a
  newer parser or dataset is available; "Last reimported" is "Last parsed".
- A `parse_blob` failure with code `parser_too_old` keeps the source flagged
  for re-parse instead of failing it. Reconcile skips accounts without a
  pool quietly. Presence scans are cached for 5 seconds.

## [0.4.0] — 2026-10-21

Upgrade order: Core before the workers (job results now ignore unknown
fields, so a 0.4.0 worker on a 0.3.x Core would fail every import). Gear on
already imported Strava activities appears after a reparse of their sources;
Garmin activities need a reimport.

### Added
- Gear (docs/gear-v0.4.0.md): user-managed bikes and shoes with provider
  refs, per-type defaults and derived totals. `GearService` (list, get,
  create, update, delete, merge, link a provider record onto chosen gear,
  defaults, sync); `UpdateActivity` takes
  `gear_id` / `reset_gear`; `Activity` carries `gear_set_by` and `gear_name`;
  activities can be listed by gear (`gear_ids`, feed `?gear_id=`). Imported
  activities get their gear from the winning source's provider gear id
  (merge field group `gear`, pinnable); manual and uploaded activities get the
  type default once. GEAR worker events record provider gear by ref; a
  record Cairn has not seen stays pending and raises a "gear needs a
  decision" notification until the user creates gear from it
  (`CreateGearFromRef`), maps it onto existing gear (`LinkGearRef`) or leaves
  it (`ListPendingGearRefs`). Web: a Gear page (pending decisions, create,
  edit, retire, merge, delete, defaults per sport), gear pickers on the
  activity edit and manual-entry forms, the gear line on the activity page,
  a gear filter on the activities list, "Sync gear" per connected account;
  `import_gear` jobs are published on connect, on `POST
  /api/accounts/{id}/sync-gear` and with every reconcile when a live worker
  declares the Gear data type. Admin `POST /api/admin/gear/rebuild-totals`.
  Migration 00063.
- Worker contract (additive): `WORKER_EVENT_TYPE_GEAR` / `ImportedGear`
  events, the `import_gear` job (`cairn.jobs.import_gear.<provider>`, in the
  standard worker permission template and the conformance harness `--gear`
  flag), `cairn.v1.GearKind`, and `gear_external_id` / `gear_name` on
  `ActivitySourcePayload`.

### Changed
- Job results are decoded with unknown fields ignored, so a worker on a
  newer additive contract no longer fails every import on an older Core.
  Upgrade order from here on: Core before workers.

## [0.3.3] — 2026-10-19

### Added
- `make proto-breaking`, part of `make check`: `buf breaking` against the previous release tag, so a breaking change to the proto contract fails before the tag.

### Changed
- The project moved to the `opencairn-org` GitHub organization. The image is
  published as `ghcr.io/opencairn-org/cairn-core` (toolchain:
  `ghcr.io/opencairn-org/cairn-build-base`); the `ghcr.io/johnnycube`
  namespace keeps the tags published so far as a frozen mirror and receives
  no new versions, so image pins and Renovate rules should move. The Go
  module path is `github.com/opencairn-org/cairn-core`.

## [0.3.2] — 2026-10-15

### Changed
- Worker enrollments accept any well-formed provider type (lowercase letters,
  digits, `-`, `_`, at most 48 characters) instead of only the built-in
  providers, so a worker for a new provider can be enrolled without a core
  change. The admin still sets the provider; the format check keeps NATS
  wildcards and subject separators out of the worker's permissions. The
  admin UI suggests the known providers, and `providers` in
  `GET /api/admin/worker-enrollments` is a list of suggestions. Linking user
  accounts for a new provider still needs core support (OAuth connect and
  job dispatch use the built-in provider list).

## [0.3.1] — 2026-10-15

### Fixed
- Worker enrollment under NATS auth never worked: nats-server rejected every
  auth-callout reply (wrong audience, and a user JWT with `issuer_account`
  and no target account). Replies are now accepted and admitted workers get
  the standard permission template. The auth-callout is opt-in with the new
  settings `CAIRN_NATS_AUTH_CALLOUT=true` and `CAIRN_NATS_WORKER_ACCOUNT`
  (the nats.conf account core and workers share; startup fails if it is
  missing). `docker/nats-auth-callout.conf` is a working NATS config; the
  setup in the README, `docs/architecture.md` and the Kubernetes hardening
  notes was corrected to match.

### Security
- For worker enrollments without a provider (every enrollment created
  through the admin API or UI since v0.2.0), the auth-callout issued NATS
  credentials with empty publish and subscribe allow lists, which NATS treats
  as unrestricted. nats-server rejected those replies (see Fixed), so no
  worker was admitted with them. Such enrollments are now refused (reason
  `enrollment_missing_provider`, logged with the enrollment ID), and the
  credential issuer refuses to sign a token with an empty allow list.

### Changed
- `POST /api/admin/worker-enrollments` requires `provider`, one of the
  providers core knows (`garmin`, `strava`); a missing or unknown provider
  is answered with `400`. The admin UI has a provider selector, and
  `GET /api/admin/worker-enrollments` returns the accepted `providers`.
- Enrollments without a provider cannot be backfilled (core does not store
  the provider a worker reports): re-enroll those workers. The enrollment list
  marks them (API field `reenroll_required`, UI badge "re-enroll required").

## [0.3.0] — 2026-10-15

Supersedes v0.2.7, which was withdrawn; its changes are included here.

### Changed
- The Go server moved to connect-go v2 (`connectrpc.com/connect/v2`). The wire
  protocol is unchanged, so the web app and other Connect, gRPC and gRPC-Web
  clients keep working. Stubs are generated with `protoc-gen-connect-go` v2 from
  the build-base image. Connect requests are now capped at 4 MiB per message
  (previously unbounded).
- Renovate no longer automerges Go major-version updates; they are labelled
  `needs-migration` for a manual migration.
- Web UI: upgraded to SvelteKit 3 (`@sveltejs/kit` 3.1.0, `adapter-static`
  4.0.0, `adapter-node` 6.0.0). `svelte.config.js` is gone and its settings
  now live in `web/vite.config.ts`; `web/tsconfig.json` extends
  `$app/tsconfig`; the `$lib` and `$proto` aliases are replaced by the
  `#lib/*` subpath import in `web/package.json` (proto stubs are imported
  from `#lib/proto/...`). The embedded SPA and the dev workflow are unchanged.
- **Breaking:** configuration is read only from `CAIRN_`-prefixed variables.
  Previously a missing `CAIRN_*` variable fell back to the bare name, so host
  variables such as `URL`, `USERNAME`, `PASSWORD` or `SESSION_SECRET` could
  fill Cairn settings. Rename any unprefixed variables you relied on.
- **Breaking:** an empty `CAIRN_DATABASE_URL`, `CAIRN_AUTH_SESSION_SECRET` or
  `CAIRN_AUTH_MASTER_ENCRYPTION_KEY` now fails startup instead of being
  accepted. Setting an optional variable to an empty value still means empty
  (for example `CAIRN_NATS_URL=` keeps NATS disabled).
- `cairn config help` lists each variable with its type, default and whether
  it is required. Configuration loading moved from kelseyhightower/envconfig
  to sethvargo/go-envconfig v2.
- Out-of-range integer conversions saturate instead of wrapping (proto
  int32/int16, Postgres smallint/int), and upload and attachment endpoints
  cap the request body (64/32 MiB file limit plus 1 MiB framing): oversized
  bodies get `400` instead of being truncated.
- Build and CI: the repository is onboarded to fleetlint. `make check` is the
  single gate (proto drift, golangci-lint, ESLint/Prettier/svelte-check, race
  tests, Go unit + Postgres integration coverage gates, web coverage,
  govulncheck, licenses, gitleaks, both builds), run by `check.yml` inside the
  build-base image and as the gate before image publishing. Actions and base
  images are pinned by digest, toolchain downloads are checksum-verified, and
  CodeQL and a weekly mutation run were added. The web app is linted,
  formatted and strictly typed.
- Toolchain: Go 1.27.2 (go.mod toolchain and the build-base image), buf
  1.73.0, protoc-gen-go 1.36.12, protoc-gen-connect-go 1.21.0,
  protoc-gen-openapiv2 2.31.0; the committed stubs are regenerated.
- Dependency updates: connect-go 1.21.1, go-jose 4.1.6, goose 3.29.0,
  aws-sdk-go-v2 (S3 1.114.3), prometheus client_golang 1.25.0,
  golang.org/x/crypto 0.58.0, NATS 2.15.1 (dev compose), maplibre-gl 6.14.0,
  vite 8.3.4, paraglide-js 2.28.0, the routine Go and npm minor/patch bumps,
  and npm/compose/CI dependencies pinned to exact versions and digests. The
  deployment manifests reference cairn-core v0.2.6, cairn-provider-strava
  v0.2.5 and cairn-provider-garmin v0.2.2.

### Security
- The SSRF guard for federation fetches and webhook delivery now blocks every
  IANA special-purpose range: CGNAT `100.64.0.0/10` (including the
  `100.100.100.200` metadata endpoint), `192.0.0.0/24`, the documentation and
  benchmarking ranges, `240.0.0.0/4`, NAT64 (`64:ff9b::/96`,
  `64:ff9b:1::/48`), 6to4 `2002::/16`, Teredo and IPv4-mapped addresses.
  Webhook URL validation uses the same guard.
- Login redirects reject backslashes and control characters, which browsers
  turned into an off-site redirect (`/\evil.com`, `/<TAB>/evil.com`).

### Fixed
- The worker permission template now matches the provider contract, so
  workers under NATS auth are no longer refused: fetch_source consumers use
  the exact `cairn.jobs.fetch_source.<provider>` subject core publishes (the
  `.>` filter matched nothing), and the template adds `import_metrics` jobs and
  results, account-suffixed reconcile results, `fetch_source` sub-job
  enqueues, `cairn.creds.<provider>.get` and serving `cairn.discover.<provider>`.
- Publishing to a subject outside any JetStream stream that has a core NATS
  subscriber no longer waits five seconds for an ack and delivers the message
  twice on retry. The bus looks up whether a stream covers the subject,
  caches the answer and picks JetStream or core publish up front.
- Enrolled workers are granted the JetStream API subjects they use: stream
  lookup by subject, their own pull consumers on `CAIRN_JOBS` and
  `CAIRN_WEBHOOKS` (create filtered to their subjects, fetch, ack) and the
  presence, manifest and rate-limit KV buckets. Before, a worker admitted by
  the auth-callout could not pull jobs at all. Installations running NATS
  without auth are unaffected.
- `GetActivityStream` honours the requested channels (behaviour change: the
  subset is now enforced on the response). Requests for only `TIME` or the
  advanced cycling/running channels used to return every channel; a
  `TIME`-only request now returns just the timestamps.
- The Nominatim geocoder returns `ErrNoPlace` when a coordinate has no place,
  so the start-place backfill records it as resolved instead of relying on an
  empty name.

## [0.2.6] — 2026-09-29

### Changed
- Malformed JSON request bodies are rejected with `400` instead of being
  treated as empty. This applies to password-reset request/confirm, source
  detach/reparse and admin enrollment revoke/prolong. An empty body on the
  endpoints where the body is optional still works as before.
- `CAIRN_NATS_STREAM_RETENTION_DAYS` is removed. Nothing read it, so setting
  it had no effect.
- Dependency updates: nats.go 1.54.0, go-webauthn 0.18.2, aws-sdk-go-v2 (S3
  1.113.4), maplibre-gl 6.11.0 and the routine Go and npm minor/patch bumps.
  The deployment manifests reference cairn-provider-strava v0.2.4 and
  cairn-provider-garmin v0.2.2.

### Fixed
- Error paths that gave up silently now log: federation publish and delete,
  the import-queue processor, the result router, the follow list and the
  worker-presence scans. They log at Warn, or at Debug where the failure is
  expected.
- Privacy & sharing settings no longer ignore a failed load. Each fetch shows
  an inline error, and "Save visibility" stays disabled until the policy has
  loaded, so a failed fetch cannot be saved back as an empty policy.

## [0.2.5] — 2026-09-11

### Added
- Heatmap (`/heatmap`): every GPS track the filter matches, drawn as
  translucent lines so the routes you ride most burn brightest; click a track
  to open the activity. Backed by `GET /api/activities/heatmap`, which takes
  the same filter params as the feed and serves the tracks from the 30 s
  aggregates in one response (newest 5000 activities, flagged when truncated).
- The activity filter bar is now one shared component (`ActivityFilterBar`
  over an `ActivityFilter` model) used by both `/activities` and `/heatmap`,
  so the filter vocabulary is identical everywhere.
- Manage page: "Regenerate map snapshot" re-renders the static `map.png`
  (`POST /api/activities/{id}/map/regenerate`, owner-only) for when the track
  changed under the cache or a render failed.
- `scripts/dev-sample-data.py` seeds a dev instance with synthetic GPX
  activities (favourite loops + one-offs) through the normal upload path.

### Changed
- `CAIRN_FEDERATION_ENABLED=false` (the default) now keeps federation fully
  dormant: the dashboard feed no longer surfaces previously received remote
  activities, and the SPA hides the "Federated" feed filter. The flag is
  exposed to the UI via the new public `GET /api/instance/features`.

### Fixed
- Interactive maps were blank in production builds (no tiles, no route):
  MapLibre ≥6 resolves its web worker relative to `import.meta.url`, which is a
  hashed chunk path in the Vite bundle, so the worker 404'd and nothing was
  parsed. The worker is now pinned to a Vite-bundled asset. Dev mode was
  unaffected, which is why v0.2.4 shipped with it.
- `map.png` responses carry an ETag and a short private max-age instead of a
  day-long public one, so regenerated snapshots show up promptly.

## [0.2.4] — 2026-08-28

### Added
- Export options: `GET /api/activities/{id}/export` accepts
  `?exclude=gps,altitude,distance,speed,heart_rate,power,cadence,temperature,title`
  to strip data from the exported GPX/TCX/FIT file; the manage page exposes the
  same choices as checkboxes.

### Fixed
- Maps showed an "API KEY REQUIRED" watermark: CARTO started stamping its free
  Voyager raster tiles. The interactive map (activity page, privacy-zone picker)
  now defaults to OpenFreeMap's key-less Liberty vector style
  (`VITE_MAP_STYLE_URL` still overrides it). The server-rendered activity
  snapshot (`/api/activities/{id}/map.png`) fetches from the new
  `CAIRN_MAP_TILE_URL` (default: public OSM tiles; set `CAIRN_MAP_USER_AGENT`
  for policy-compliant identification) and its S3 cache key was bumped to v5 so
  watermarked snapshots are re-rendered.
- Scheduled reconcile re-imported the newest activity of every account on each
  tick: the worker window starts at watermark − 1 h, so the activity that *is*
  the watermark always fell inside it, and core never populated the workers'
  `known_ext_ids` skip-list. Core now sends the external IDs it already holds
  for the window; a lookup failure degrades to the old behaviour instead of
  blocking sync.

### Changed
- Dependencies: maplibre-gl 5 → 6.6, SvelteKit/Svelte/Vite/Paraglide minors,
  Go modules (AWS SDK, smithy-go, protobuf, x/crypto, nats.go), dev-stack NATS
  2.14.6 and Mailpit 1.31.

## [0.2.3] — 2026-07-29

### Added
- Activities page: year facet + year filter (sugar over the existing `from`/`to`
  range; the year list is deliberately unfiltered so it always offers every
  year with data) and proper paging instead of endless lists.

### Fixed
- Activities page no longer flickers when filters change.
- CI: images are signed keyless with cosign (Sigstore OIDC) and published with
  provenance attestations + SBOM; the Gitea mirror workflows were dropped.

## [0.2.2] — 2026-07-20

### Fixed
- GitHub Actions: the containerized test job now marks the checkout as a git
  `safe.directory`, fixing the "error obtaining VCS status" build failure that
  broke the v0.2.1 release pipeline. No code changes since 0.2.1 — this release
  exists to publish the images that release never produced.

## [0.2.1] — 2026-07-20

### Fixed
- NATS JetStream now binds an explicit `StorageClass` in the Kubernetes
  manifest. With no default StorageClass the claim stayed Pending and silently
  fell back to an `emptyDir`, losing all JetStream state (streams + KV) on every
  pod reschedule.
- Stale worker manifests are reaped once a worker has no live presence, and the
  `cairn_worker_manifests` KV bucket now carries a 15-minute TTL. Previously a
  decommissioned or replaced worker's manifest lingered indefinitely and
  replayed through the update-available scan on every server restart. Pairs with
  the Strava worker's periodic manifest re-publish ([cairn-provider-strava]
  ≥ v0.2.1); older workers are still cleaned up by the presence-driven reaper.

## [0.2.0] — 2026-07-16

**First public release.** A working, self-hosted multi-source activity
tracker: import → merge → analysis → web UI, plus a social layer and optional
federation. Single Go binary (API + embedded SPA) with separate provider
workers ([cairn-provider-strava], [cairn-provider-garmin]).

### Import & sources
- NATS/JetStream worker control plane + async job/result bus; provider workers
  fetch and push typed results that run through one ingest path.
- **Strava** worker (Go reference implementation) and **Garmin** worker
  (Python) — proving the model is provider- and language-agnostic. The
  normative provider contract is documented on the docs site
  (https://docs.opencairn.org/architecture/provider-contract), and
  `cmd/worker-conformance` verifies a running worker implements it (presence
  heartbeat, durable consumers, discover reply shape, reconcile round-trip).
- Worker results are **claim-checked through the blob store**: event-carrying
  results upload via presigned PUT and travel as a small envelope, so large
  activities (long rides, dense streams, many segment efforts) aren't capped
  by the NATS payload ceiling. Terminal worker failures and deterministic
  ingest errors fail the import-queue item immediately with the true reason.
- File upload (GPX/TCX/FIT) and **manual activity entry**, both fed through the
  same ingest → merge pipeline as worker imports.
- Resumable full-sync (diff-only-vs-redownload), reconciliation scheduler,
  per-account auto-import suspend, webhook ingress (generic forwarder).
- Raw-response archival to S3/MinIO + quota-free re-parse from the archive;
  archived-blob content-type + size recorded for the manage view.
- Stream ingest tolerates provider quirks such as duplicate sample timestamps
  (last sample wins, order preserved).

### Merge & editing
- Per-field-group multi-source merge with a user-configurable policy; identity
  dedup + fuzzy re-clustering (match + union-find) of same-workout sources.
- `preserveUserEdits` (title/description/tags/privacy/gear), per-field source
  pins, and a post-merge user **overlay** for classification (sport/discipline/
  flags/custom subtype) **and** summary metrics (distance/elevation/moving
  time) that survives re-import.
- **Source data from another activity** — derived source carrying selected
  field-groups + the donor's geo stream rebased in time, pinned via overrides.
- Full sport-type set; activity **attachments** (photos) as a first-class
  entity, Strava photos mirrored to S3, visibility-gated.

### Analysis
- Best-effort curves (power/HR/speed/pace/VAM, duration- and distance-windowed)
  with personal records.
- Segment matching (PostGIS bbox + corridor-walking) with per-user/per-instance
  leaderboard ranks.
- Training load (Banister CTL/ATL/TSB, per-user FTP/LTHR as-of each activity),
  time-in-zone (HR + power), per-activity TSS, laps.
- Activity export (GPX/TCX/FIT) generated from the merged stream.

### Web app (SvelteKit, embedded)
- Grouped sidebar navigation (main / Social / Account / Admin) with a mobile
  off-canvas drawer.
- Activities feed (facets/sort/pagination) with dynamic filters — date presets
  + custom range, tri-state classification flags (virtual/e-bike/commute/race),
  and numeric ranges (distance, duration, elevation gain, avg speed/HR/power)
  in the user's display units; rich activity detail (map ↔ stream cross-hair,
  elevation profile, zones, segment efforts, laps, photo gallery), full-screen
  map/stream pages, create/edit activity, per-activity manage page (provenance,
  re-fetch, re-parse-from-archive, detach, split, export).
- Segments landing, similar-routes + best-effort progression, analysis/stats,
  athlete physiology profile, connections, admin.

### Accounts, auth & social
- Password + WebAuthn/passkeys + OIDC; invites; personal access tokens; per-user
  provider OAuth configs (bring-your-own Strava app).
- Social layer: follow graph + following feed, public profiles, share links,
  kudos + comments, clubs, blocking, content reports + moderation queue,
  per-field visibility policy (audience × category) + privacy zones, per-user
  activity quotas, delegated-admin (moderator) role.
- Optional **ActivityPub federation** (off by default; per-user opt-in): remote
  follow, publishing public activities, federated kudos/comments, signed
  delivery queue, instance defederation, per-domain inbox rate-limiting.

### Notifications & ops
- In-app feed + email + signed outbound webhooks; per-type preferences,
  tz-aware quiet hours, per-event delivery audit.
- Prometheus `/metrics`, deep `/readyz`, HTTP rate limiting, master-key +
  NATS-account-key rotation, dead-letter capture/replay.

### Build & CI
- Single distroless prod image (`docker/Dockerfile.core`, embedded SPA) +
  worker images.
- Shared **build-base** toolchain image (Go + Node + buf + protoc + local
  plugins) so `buf generate` runs offline; the core/Strava images build
  `FROM build-base`.
- CI: per-image build + test workflows; images publish only from `main` and
  `v*` tags, MRs build/test as a gate without pushing.

## [0.1.0] — 2026-06-21

Initial development (internal pre-release iterations v0.1.0–v0.1.12).

[0.3.3]: https://github.com/opencairn-org/cairn-core/releases/tag/v0.3.3
[0.3.0]: https://github.com/johnnycube/cairn-core/releases/tag/v0.3.0
[0.2.6]: https://github.com/johnnycube/cairn-core/releases/tag/v0.2.6
[0.2.5]: https://github.com/johnnycube/cairn-core/releases/tag/v0.2.5
[0.2.4]: https://github.com/johnnycube/cairn-core/releases/tag/v0.2.4
[0.2.3]: https://github.com/johnnycube/cairn-core/releases/tag/v0.2.3
[0.2.2]: https://github.com/johnnycube/cairn-core/releases/tag/v0.2.2
[0.2.1]: https://github.com/johnnycube/cairn-core/releases/tag/v0.2.1
[0.2.0]: https://github.com/johnnycube/cairn-core/releases/tag/v0.2.0
[cairn-provider-strava]: https://github.com/johnnycube/cairn-provider-strava
[cairn-provider-garmin]: https://github.com/johnnycube/cairn-provider-garmin
