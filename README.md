# Cairn

A self-hosted, multi-source activity tracker. Imports activities from Strava and
Garmin (the worker model is provider-agnostic), plus file uploads (GPX/TCX/FIT)
and manual entry; merges multiple sources per user-configurable policy into one
canonical activity, stores per-source 1 Hz time-series streams in TimescaleDB,
and layers on the analysis you'd expect: best-effort curves, segment matching +
leaderboards, training load (CTL/ATL/TSB), a social layer, and a full web UI.
AGPL-3.0 core, single Go binary + separate worker processes (the worker
contract — protobuf + SDKs — and the provider workers are Apache-2.0).

[![License: AGPL v3](https://img.shields.io/badge/License-AGPL_v3-blue.svg)](LICENSE)
&nbsp;Docs: **[docs.opencairn.org](https://docs.opencairn.org)** &nbsp;·&nbsp; Site: **[opencairn.org](https://opencairn.org)**

**Status: working application, not yet 1.0.** Run it locally with one command
(see [docs/run-local.md](docs/run-local.md)) and you get a usable tracker:
sign in, connect a Strava account, import your history, and browse it.

What's built and working end-to-end:

- **Import pipeline** — workers fetch from a provider over NATS/JetStream and
  push typed results (file upload + manual entry feed the same path); the server
  dedups by identity then fuzzily re-clusters same-workout sources (match +
  union-find), merges N sources per-field-group with a configurable policy
  (`preserveUserEdits` + per-field source pins + a post-merge user overlay for
  classification & summary metrics protect your edits), and persists streams
  (TimescaleDB CopyFrom + continuous aggregates).
- **Manual & derived activities** — create an activity by hand, or source
  selected fields/route from another activity (donor stream rebased in time);
  attachments (photos) are a first-class entity, Strava photos mirrored to S3.
- **Full Sync** — exhaustive, resumable discovery of every activity on a
  connected account, with a diff-only-vs-redownload preview.
- **Analysis** — best-effort curves (power/HR/speed/pace/VAM, duration- and
  distance-windowed), segment matching (PostGIS bbox + corridor-walking) with
  per-user/per-instance leaderboard ranks, training load (Banister EWMA,
  per-user FTP/LTHR resolved as-of each activity), time-in-zone (HR + power),
  per-activity TSS, laps.
- **Web app** — SvelteKit SPA: activities feed (facets/sort/pagination),
  rich activity detail (map ↔ streams cross-hairs, elevation profile, zones,
  segment efforts, laps), full-screen map/streams subpages, per-activity
  manage page (provenance, re-fetch, re-parse-from-archive, detach, export),
  segments landing, similar-routes progression, best-effort progression,
  analysis + stats pages, athlete physiology profile, connections, admin.
- **Activity export** — GPX / TCX / FIT, generated from the merged stream.
- **Accounts & auth** — password + WebAuthn/passkeys + OIDC; invites;
  personal access tokens; per-user provider OAuth configs (BYO Strava app).
- **Notifications** — in-app feed + email + signed outbound webhooks, per-type
  preferences, tz-aware quiet hours, per-event delivery audit.
- **Social** — follow graph + following feed, public profiles, share links,
  kudos + comments, clubs, blocking, content reports + moderation queue,
  per-field visibility policy (audience × category) + privacy zones, per-user
  activity quotas, delegated-admin (moderator) roles. Optional ActivityPub
  **federation** (off by default).
- **Ops** — Prometheus `/metrics`, deep `/readyz`, HTTP rate limiting, secret +
  NATS-account-key rotation, blob archival (S3/MinIO) + quota-free re-parse,
  dead-letter capture/replay, reconciliation scheduler.

Primary adapters live: Connect-RPC (user-facing API), REST (`/api/*`), the
generic webhook forwarder, and session-gated operator endpoints (`/api/admin/*`). The
Strava worker + worker SDK are implemented; the HTTP calls are exercised in
unit tests but the full provider round-trip is validated against the real
Strava API in an operator session.

The social layer (sharing, public profiles, follow graph, kudos/comments,
clubs, moderation) and ActivityPub federation are built — both off-by-default-
friendly and self-host-first. See the Status section below.

## Architecture at a glance

Hexagonal: domain at the centre, ports as interfaces, adapters at the edges.

```
                                  ┌──────────────────────┐
                                  │  domain (pure)       │
                                  │  ─ types, invariants │
                                  │  ─ merge engine      │
                                  └──────────────────────┘
                                            ▲
                                            │
                                  ┌──────────────────────┐
                                  │  usecase             │
                                  │  ─ RecomputeActivity │
                                  │  ─ IngestActivity    │
                                  │  ─ besteffort/segment│
                                  │  ─ admin.*           │
                                  └──────────────────────┘
                                            ▲
                          ┌─────────────────┼─────────────────┐
                          │                 │                 │
                ┌──────────────┐ ┌───────────────────┐ ┌───────────────────┐
                │  port (interface)
                │  ─ ActivityRepo, WorkerRepo, UserRepo, …
                │  ─ TxManager
                └──────────────┘
                          ▲                 ▲                 ▲
                          │                 │                 │
   ┌──────────────────────┴────┐  ┌─────────┴──────────┐  ┌───┴──────────────────┐
   │ secondary adapters         │  │ primary adapters   │  │ primary adapters     │
   │  ─ postgres.* (pgx + goose)│  │  ─ Connect-RPC     │  │  ─ NATS workers      │
   │  ─ s3.* (MinIO)            │  │  ─ REST /api/*     │  │    (control plane +  │
   │  ─ nats.* (JetStream)      │  │  ─ webhook fwd     │  │     job/result bus)  │
   │  ─ email.* (SMTP)          │  │  ─ /api/admin/* ops │  │  ─ embedded SPA      │
   │  ─ geocode.* (Nominatim)   │  │                    │  │                      │
   └────────────────────────────┘  └────────────────────┘  └──────────────────────┘
```

The single rule: `domain` imports nothing; `port` imports only `domain`;
`usecase` imports `domain` + `port`; adapters implement ports.
`cmd/server/wire.go` is the only place concrete adapters meet use cases.

Module layout:

```
api/proto/                       Proto schemas (cairn.v1 + cairn.worker.v1)
cmd/server/                      The cairn binary (serve, migrate, REST/admin handlers, wire.go)
cmd/worker-strava/               Reference provider worker (Strava)
internal/
├── adapter/
│   ├── primary/{connect,web}/   Connect-RPC handlers + embedded SPA
│   └── secondary/{postgres,nats,s3,email,geocode}/   Driven adapters
├── auth/                        Argon2id + AES-GCM SecretBox
├── config/                      Envconfig-driven Config
├── db/                          Pool open + goose-embedded migrations
├── domain/                      Pure types + business logic (merge, segments, zones, export inputs)
├── export/                      GPX / TCX / FIT writers
├── port/                        Driven-port interfaces
├── usecase/                     Ingest, Recompute, BestEffort, Segment, TrainingLoad, notification, sync, …
└── workersdk/                   Worker SDK (NATS lifecycle, token cache, presign, webhooks)
web/                             SvelteKit SPA (CSR; embedded in the prod binary via -tags embedweb)
docs/                            architecture.md, run-local.md, worker-pooling-vs-versioning.md
```

## Prerequisites

- Go 1.26+
- Docker + Docker Compose
- [buf](https://buf.build/docs/installation) (only when regenerating proto stubs)

## Run it

Local dev runs the **supporting services in docker compose** and the **binaries
on the host with live-reload** — four shells:

```sh
make dev-up                  # postgres + nats + minio + mailpit (compose)
cp dev.env.example dev.env   # once
make dev-server              # shell 2: Go server   (rebuilds on save)
make dev-worker              # shell 3: Strava worker (rebuilds on save)
make dev-web                 # shell 4: SvelteKit dev server (vite HMR)
# → http://localhost:5173  (web)   ·   http://localhost:8080  (API)
```

Production is a single **distroless** image (`docker/Dockerfile.core`) — one
nonroot binary that serves the API *and* the embedded SPA (`-tags embedweb`),
built + pushed by the `build-core` GitHub Actions workflow. Full walkthrough,
ports, and prod notes are in **[docs/run-local.md](docs/run-local.md)**.

### Running the server by hand

For backend-only work, bring up the deps and run the binary directly:

```sh
docker compose up -d                     # Postgres + MinIO + NATS
export CAIRN_DATABASE_URL="postgres://cairn:cairn@localhost:5432/cairn?sslmode=disable"
export CAIRN_AUTH_MASTER_ENCRYPTION_KEY="$(openssl rand -base64 32)"
export CAIRN_INSTANCE_BOOTSTRAP_ADMIN_EMAIL="admin@example.com"
export CAIRN_INSTANCE_BOOTSTRAP_ADMIN_PASSWORD="changeme"
make tidy && make migrate-up && make run
```

`make run -- config help` prints every `CAIRN_*` variable, its default, and
whether it's required. NATS (`CAIRN_NATS_URL`) and S3 (`CAIRN_STORAGE_*`) are
optional — without them the server runs single-process; the async worker
control-plane, blob archival, and presigned uploads simply stay off.

## Exercising the pipeline

The legacy token-gated `/admin/*` curl smoketest has been removed. The
user-facing surfaces now cover everything it did:

- **The web UI** (`http://localhost:8080`) — sign in, go to **Connections**, add
  a Strava OAuth app + connect your account, then **Full Sync** to import. Browse
  the result: feed, activity detail, segments, analysis, stats.
- **Connect-RPC + REST** (`/cairn.v1.*`, `/api/*`) — the typed API the SPA uses;
  e.g. `GET /api/activities/feed`, `GET /api/segments`, the per-activity manage
  surface (`POST /api/activities/{id}/recompute`, `.../reimport`, `.../sources/{id}/detach`,
  `.../export.{gpx,tcx,fit}`).
- **The worker** (`cmd/worker-strava`) — fetches from the provider over NATS and
  pushes typed `cairn.worker.v1.JobResult` events; the server's result router
  ingests them through the same merge/best-effort/segment/training-load pipeline.

### Operator endpoints (`/api/admin/*`, admin session required)

A small set of maintenance endpoints, gated by an admin session cookie (sign in
as an admin user, then call them with the `cairn_session` cookie):

- `POST /api/admin/nats/bootstrap-account-key` — first-time NATS account signing
  key; returns the public key to paste into `nats-server.conf`.
- `POST /api/admin/nats/rotate-account-key` — rotate it (trust both during overlap).
- `GET  /api/admin/dlq` / `POST /api/admin/dlq/{id}/replay` — dead-letter inspect + replay.
- `POST /api/admin/geocode/backfill` — run one start-location reverse-geocode batch.
- `GET/POST /api/admin/workers`, `/api/admin/worker-enrollments`, `/api/admin/invites` —
  worker onboarding + invites (also surfaced in the web admin page).
- `GET /api/admin/users[/{id}]` plus role/status/quota/password-reset/credential
  actions, `GET /api/admin/audit` — user management with an audit trail.
- `GET|PUT /api/admin/settings`, `POST /api/admin/settings/email/test`,
  `GET /api/admin/health` — instance settings and a component health report.
- `GET /api/admin/jobs[/{id}]` (+ `retry`, `cancel`), `GET /api/admin/sources/orphaned`
  — import-queue introspection.
- `GET /api/admin/webhook-tokens`, `POST /api/admin/webhook-tokens/rotate` — per-account
  webhook signing secrets (users rotate their own via
  `POST /api/accounts/{id}/webhook-token/rotate`).
- `POST /api/admin/maintenance`, `GET /api/admin/maintenance/jobs`,
  `POST /api/admin/maintenance/jobs/{id}/cancel` — instance-wide recomputations
  (confirm with the operation name in upper case). Jobs run one at a time in
  the core process and are persisted: a restart re-queues waiting jobs and
  fails the interrupted one.

Every route has a twin on the Connect `AdminService`; both accept only an admin
session, never an OAuth token.

## NATS one-time setup

> **Operator auth.** The `/api/admin/*` endpoints below require an **admin
> session cookie** (the dev token surface was removed). Sign in once and reuse
> the cookie jar:
>
> ```sh
> export BASE="http://localhost:8081"   # the Go server directly (not the proxy)
> curl -s -c cookies.txt -X POST "$BASE/auth/password" \
>   --data-urlencode "identifier=admin@cairn.local" \
>   --data-urlencode "password=changeme" >/dev/null
> ADMIN="-b cookies.txt"                 # pass to each curl below
> ```

Before workers can enrol, the cairn-server needs an account NKey it
signs user-JWTs with, and the NATS server needs to be configured to
trust it. Done once per instance; the key is encrypted at rest with
`CAIRN_AUTH_MASTER_ENCRYPTION_KEY`.

```sh
# 1. Cairn generates + stores the account key. Idempotent — returns the
#    existing key's public form on second call.
curl -s -X POST "$BASE/api/admin/nats/bootstrap-account-key" $ADMIN | jq

# Response:
# {
#   "public_key": "AAEKQGTAR2GW6FRYKCQI3UQYNCBLP7XQAEFMQNNKGN5IH7BBFOAVTJTV",
#   "created_at": "2026-05-17T19:43:12Z",
#   "action":     "created",
#   "note":       "copy this public key into nats-server.conf auth_callout.issuer"
# }
```

Run NATS with `docker/nats-auth-callout.conf`, which delegates auth to the
cairn server. It reads the returned key from `CAIRN_NATS_AUTH_ISSUER` and
core's password from `CAIRN_NATS_PASSWORD`; core and admitted workers share
its `CAIRN` account. Then start core with:

```sh
CAIRN_NATS_USERNAME=cairn-core
CAIRN_NATS_PASSWORD=...            # same as nats-server's
CAIRN_NATS_AUTH_CALLOUT=true
CAIRN_NATS_WORKER_ACCOUNT=CAIRN
```

From now on, every CONNECT except `cairn-core` itself hits the auth-callout
subject, which routes to the worker-enrollment flow.

## Worker enrollment smoketest

Pre-authorise a worker so it can self-register against NATS. The full
flow lives in `docs/architecture.md §4.5`; this is the operator-facing
piece you'll actually invoke.

```sh
# Generate a 7-day token. The provider scopes the worker's NATS permissions
# and may be any id of lowercase letters, digits, '-' and '_' (max 48 chars);
# the name must equal CAIRN_WORKER_NAME on the worker.
curl -s -X POST "$BASE/api/admin/worker-enrollments" $ADMIN \
  -H 'content-type: application/json' -d '{
    "name":             "strava-fetcher",
    "provider":         "strava",
    "expires_in_hours": 168,
    "note":             "prod cluster A pod-12"
  }' | jq

# Response (token is shown ONCE):
# {
#   "enrollment": { "id": "0192...", "provider": "strava", ... },
#   "token":      "cairn_enroll_3kF8vR1nQXjQ-2pZ7mYwL5tH4dGqW6KsM8nVxBcEoUg",
#   "warning":    "store this token now — it will not be shown again"
# }

# Set on the worker container — that's everything the worker needs:
#   CAIRN_NATS_URL=nats://nats.cairn.local:4222
#   CAIRN_WORKER_NAME=strava-fetcher
#   CAIRN_WORKER_ENROLLMENT_TOKEN=cairn_enroll_3kF8vR1nQXjQ...

# List enrollments, including revoked and expired ones. `providers` lists the
# built-in providers as suggestions only.
curl -s "$BASE/api/admin/worker-enrollments" $ADMIN | jq

# Revoke an enrollment (kills future connects from this token; does NOT
# kick already-open NATS connections — those expire when their user-JWT
# does, max 24h).
curl -s -X POST "$BASE/api/admin/worker-enrollments/$ID/revoke" $ADMIN \
  -H 'content-type: application/json' \
  -d '{"reason": "rotated key after employee departure"}' | jq
```

The `token_hash_hex` in the response is the sha256 of the plaintext
token — operators can use it to identify which enrollment matches a
given worker env-var without the worker having to re-present the
plaintext token. It is NOT a working token itself.

## Strava webhook setup

Webhooks give **real-time imports**: Strava calls Cairn the moment an
athlete finishes an activity, instead of waiting for the next periodic
reconcile. Cairn's core is provider-agnostic — the **worker** owns the
webhook (verification + decoding); the core just forwards the raw event
to it over NATS.

**The URL is worker-scoped.** Each webhook-capable worker owns
`/webhooks/{name}-{provider}` — its NATS worker key. So multiple
independent Strava workers (`worker1-strava`, `worker2-strava`, …) each
have their own URL and don't collide. A worker advertises that it owns a
webhook in its heartbeat; the URL then appears:

- in the **admin UI** under *Worker onboarding → Webhooks*, and
- in the **app** on each connection's card (*Connections → Real-time
  imports*), for any signed-in user to copy into their provider app.

The worker only registers its webhook handlers when
`CAIRN_STRAVA_WEBHOOK_VERIFY_TOKEN` is set (so exactly one logical worker
claims the subject). Example for the dev stack, where the strava worker's
key is `strava-fetcher-strava`:

```
WORKER_KEY=strava-fetcher-strava          # {CAIRN_WORKER_NAME}-{provider}
CALLBACK="https://your-instance.example.com/webhooks/$WORKER_KEY"
VERIFY_TOKEN="$CAIRN_STRAVA_WEBHOOK_VERIFY_TOKEN"

# Each user registers a push subscription with THEIR OWN Strava app
# (the per-user client_id/secret from their connection — provider creds
# are per-user in Cairn, not per-instance):
curl -X POST https://www.strava.com/api/v3/push_subscriptions \
  -F client_id=$YOUR_STRAVA_CLIENT_ID \
  -F client_secret=$YOUR_STRAVA_CLIENT_SECRET \
  -F callback_url="$CALLBACK" \
  -F verify_token="$VERIFY_TOKEN"
```

Strava immediately does a `GET $CALLBACK?hub.mode=subscribe&hub.verify_token=…&hub.challenge=…`
handshake; the worker validates `verify_token` against
`CAIRN_STRAVA_WEBHOOK_VERIFY_TOKEN` and echoes the challenge — all
automatic. After that, `POST $CALLBACK` create/update/delete events flow:
the worker maps Strava's `owner_id` to the right Cairn account (via
`cairn.accounts.lookup_by_provider_ext`) and enqueues a fetch, which runs
through the normal ingest pipeline.

Notes:
- **Per-user subscriptions**: because each user brings their own Strava
  app, each user creates their own push subscription pointing at the same
  instance callback URL. The worker disambiguates by `owner_id`.
- **One subscription per Strava app** is allowed; re-`POST` returns the
  existing one. `GET /api/v3/push_subscriptions` lists it; `DELETE
  .../{id}` removes it.
- Bumping the worker's **version** (a new enrollment) does **not** change
  the webhook URL — the routing key stays `{name}-{provider}`.

## Status

Cairn is feature-complete for a self-hosted single instance: Connect-RPC +
REST API, the NATS/JetStream worker control plane, notification delivery
(in-app/email/webhook with quiet hours, preferences and retry), S3 blob
storage, OIDC + WebAuthn/passkeys, the worker SDK, and the SvelteKit
frontend. `docs/architecture.md` is the canonical spec for the NATS async
layer, worker integration, blob storage, and the error-handling matrix.

**Federation / ActivityPub** is available but off by default
(`CAIRN_FEDERATION_ENABLED` + a per-user opt-in): remote follow, publishing a
user's public activities to remote followers, federated kudos + comments, a
durable signed delivery queue, instance defederation, and per-domain inbox
rate-limiting, verified between two live instances. See
`docs/federation-design.md`.

Two provider workers ship: the **Strava** reference worker (Go,
`cmd/worker-strava/`) and a **Garmin** worker (Python, `workers/garmin/`),
proving the model is language- and provider-agnostic — adding another is a new
`cmd/worker-<provider>/` (or Python equivalent) with its own auth handler, no
core change. Deliberately out of scope for v1: a GraphQL surface (REST +
Connect-RPC cover every UI path).

## Going to production

See **[docs/production-readiness.md](docs/production-readiness.md)** for the full
readiness assessment + backup/restore + migrate-on-deploy runbook. Quick start:

### 1. Generate secrets

```sh
cairn gen-secrets > cairn.secrets.env   # store in your secrets manager; never commit
```

This emits the required random secrets as `CAIRN_*` env lines:
`CAIRN_AUTH_SESSION_SECRET` and `CAIRN_AUTH_MASTER_ENCRYPTION_KEY` (32-byte keys —
the master key encrypts all secrets-at-rest; rotate it later with `cairn
rotate-key`). Set those
plus `CAIRN_DATABASE_URL`, `CAIRN_HTTP_PUBLIC_BASE_URL`, and (for the async
worker plane) `CAIRN_NATS_URL`. `cairn config help` documents every knob.

### 2. Onboard a worker

A worker's identity is **`{name}-{provider}` at an integer `version`**
(e.g. `strava-importer-strava` v1). The name is admin-defined and NATS-safe
(`[a-z0-9-]`); the worker key drives its NATS subjects and its webhook URL.
Running the same `(provider, name, version)` more than once = pooled instances;
**bumping the version is a new worker that needs a new enrollment**.

```sh
# (a) First time only: bootstrap the NATS account signing key. Returns the
#     account public key to add to nats-server.conf's auth-callout block.
curl -s -X POST "$BASE/api/admin/nats/bootstrap-account-key" $ADMIN

# (b) Create an enrollment for this worker (name + provider + version). The
#     token is shown ONCE.
curl -s -X POST "$BASE/api/admin/worker-enrollments" $ADMIN \
  -H 'content-type: application/json' \
  -d '{"name":"strava-importer","provider":"strava","version":1,"expires_in":"8760h"}'
```

Deploy the worker with:

```
CAIRN_NATS_URL=nats://nats.internal:4222
CAIRN_WORKER_ENROLLMENT_TOKEN=<the one-time token from (b)>
CAIRN_WORKER_NAME=strava-importer        # MUST equal the enrollment name
# The worker version + package are compiled into the binary (they define its
# schema), so there is no version env — the enrollment's `version` must match
# the binary's compiled workerVersion.
# provider creds are per-user (set by each user under Connections); none here
```

On connect, NATS's auth-callout validates the token + name against the
enrollment and mints a provider-scoped user-JWT (audited in
`worker_credential_grants`). The worker's webhook URL is then
`$CAIRN_HTTP_PUBLIC_BASE_URL/webhooks/strava-importer-strava` — it appears in
the admin UI (*Worker onboarding → Webhooks*) and on each user's connection
card. See "Strava webhook setup" above for registering it with the provider.

To **upgrade** the worker, build the worker binary with its compiled
`workerVersion` bumped to `2`, create a new enrollment at `version: 2`, and
redeploy that image with the new token. The routing key stays
`strava-importer-strava`, so v1 and v2 share the work queue during the rollout.

## Tests & CI

```sh
make check             # the definition of green; CI runs exactly this
make test              # unit tests (DB-free, -race) + web vitest
make test-db-up        # throwaway TimescaleDB+PostGIS (podman or docker); prints CAIRN_TEST_DATABASE_URL
make test-integration  # integration suites against CAIRN_TEST_DATABASE_URL
```

The integration suites are build-tagged `integration` and skip when
`CAIRN_TEST_DATABASE_URL` is unset: the Postgres adapter tests
(`internal/adapter/secondary/postgres`, shared `requirePool(t)` helper) and an
end-to-end run of the HTTP API and CLI against a fresh database
(`cmd/server/*_integration_test.go`). CI (`.github/workflows/check.yml`) runs
`make check` inside the build-base toolchain image with a
`timescale/timescaledb-ha` service, so coverage is gated on unit + integration
together; see CONTRIBUTING.md.

## Rotating the master encryption key

Cairn encrypts secrets-at-rest (OAuth access/refresh tokens, OIDC + per-user
provider client secrets, the NATS account signing-key seed) with AES-256-GCM
under the master key in `CAIRN_AUTH_MASTER_ENCRYPTION_KEY`. To rotate that key,
re-encrypt all of it with the `rotate-key` subcommand:

```sh
# 1. Validate the CURRENT key can read every secret (no writes):
cairn rotate-key --dry-run

# 2. Rotate: current key from CAIRN_AUTH_MASTER_ENCRYPTION_KEY (the OLD key),
#    new key via --new-key. base64-encoded 32 bytes is used raw; anything else
#    is treated as a passphrase (SHA-256). Re-encrypts every column in place,
#    preserving each column's AAD.
cairn rotate-key --new-key="$(openssl rand -base64 32)"

# 3. Set CAIRN_AUTH_MASTER_ENCRYPTION_KEY to the NEW key and restart the
#    server + workers.
```

The command is **resumable** — a ciphertext that no longer decrypts under the
old key but does under the new one is treated as already-rotated and skipped, so
re-running after an interruption is safe. If a secret can't be decrypted under
either key (e.g. one written under a stale AAD before a refactor), rotation
**fails closed** and names the row; re-enter that secret, or pass
`--skip-unreadable` to rotate everything else and leave the unreadable rows
under the old key (you'll re-enter them afterwards).

> **NATS account key** rotation (the account NKey the auth-callout signs worker
> JWTs with) is a separate operation: the `instance_signing_keys` table keeps one
> active row per purpose, so rotation = insert a new active key + deactivate the
> old, with both account public keys trusted in `nats-server.conf` for the
> overlap. That CLI/endpoint wrapper is not built yet (master-key rotation,
> above, *does* re-encrypt the stored seeds).

## Configuration reference

Every knob is documented inline. Run:

```sh
make run -- config help
```

…or, equivalently, `go run ./cmd/server config help`.

## License

AGPL-3.0 for this repository (`LICENSE`). The provider contract under
`api/proto/` is Apache-2.0 (`api/proto/LICENSE`) so third-party workers and
clients can implement it without copyleft obligations; the provider worker
repos are Apache-2.0 as well.
