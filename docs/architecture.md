# Cairn Architecture

Deeper architecture reference for the layers that `README.md` and the ADRs in
`docs/adr/` only touch on briefly. If you are implementing the NATS-based async layer, the worker
integration, or the blob storage, this is the spec document.

> **Building or maintaining a provider worker?** The normative checklist of
> everything a worker MUST implement lives in the docs site:
> [Provider contract](https://docs.opencairn.org/architecture/provider-contract)
> (source: cairn-docs repo) — verify mechanically with
> `go run ./cmd/worker-conformance`.

What is **not** covered here: domain model, merge engine, SQL schema,
use-case implementations. For those see `docs/adr/`, the migrations under
`internal/db/migrations/`, and the code directly.

**Reading order** if you are building the async layer:

1. `docs/adr/0002-hexagonal-architecture.md` for the layering rules
2. This document
3. `README.md` for Docker-Compose and ENV setup
4. `internal/usecase/activity/ingest.go` as the anchor for the existing ingest path

---

## Contents

1. [NATS as control plane](#1-nats-as-control-plane)
2. [JetStream streams and KV buckets](#2-jetstream-streams-and-kv-buckets)
3. [Delivery semantics and idempotency contract](#3-delivery-semantics-and-idempotency-contract)
4. [Subject auth and permissions](#4-subject-auth-and-permissions)
   - [How is the Strava worker prevented from seeing Garmin credentials?](#how-is-the-strava-worker-prevented-from-seeing-garmin-credentials)
5. [The `port.JobBus` interface](#5-the-portjobbus-interface)
6. [Provider integration using Strava as an example](#6-provider-integration-using-strava-as-an-example)
   - [Phase 1: OAuth initial linking](#phase-1-oauth-initial-linking-http-not-nats)
   - [Phase 2: Initial backfill](#phase-2-initial-backfill)
   - [Phase 3: Ongoing updates via webhook](#phase-3-ongoing-updates-via-webhook)
   - [Rate-limit coordination](#rate-limit-coordination)
   - [6.4 Reconciliation Sync](#64-reconciliation-sync)
   - [6.5 Webhook edge cases](#65-webhook-edge-cases)
7. [Worker lifecycle](#7-worker-lifecycle)
8. [Async follow-ups via domain events](#8-async-follow-ups-via-domain-events)
9. [Blob storage: S3 vs NATS Object Store](#9-blob-storage-s3-vs-nats-object-store)
10. [The `port.BlobStore` interface](#10-the-portblobstore-interface)
11. [Error-handling matrix](#11-error-handling-matrix)
12. [Multi-server coordination](#12-multi-server-coordination)

---

## 1. NATS as control plane

Cairn uses NATS for two tasks:

- **Worker control plane** — jobs from the server to workers, results back,
  heartbeats, manifests, OAuth-token retrieval
- **Internal async job bus** — follow-ups after activity ingest (best-effort
  compute, segment match, training load, PR dispatch), domain events
  for external subscribers

NATS was chosen instead of a DB-based queue (River, Hatchet) because
Cairn workers explicitly should have **no DB connection** — a
self-hosted Cairn operator should be able to run Strava/Garmin workers as separate
processes or even separate pods, without giving them
Postgres credentials.

### Subject naming scheme

```
cairn.<domain>.<verb>[.<scope>]
```

Right-fanning — the further right, the more specific. Wildcard subscribers
catch larger slices without publishers having to know about it.

```
cairn.jobs.fetch_source.strava             ← Server publishes, workers consume
cairn.jobs.parse_blob.fit                  ← Reparse from S3
cairn.jobs.backfill.strava                 ← Bulk activity listing
cairn.jobs.refresh_token.garmin
cairn.results.fetch_source.strava          ← Workers publish, server consumes
cairn.results.parse_blob.fit
cairn.results.backfill.strava
cairn.events.activity.changed              ← Domain events; pipeline follow-ups (§8)
cairn.events.activity.soft_deleted
cairn.events.segment.matched
cairn.events.training_load.recomputed
cairn.events.external_account.connected
cairn.events.external_account.needs_reauth
cairn.events.external_account.reconnected
cairn.events.source.deleted_upstream
cairn.events.job.dead_lettered
cairn.workers.<name>.<inst>.hb             ← Heartbeats (Core NATS, no stream)
cairn.tokens.<provider>.fetch              ← OAuth-token retrieval via req/reply
cairn.blobs.presign_upload.<provider>      ← Presigned-URL retrieval via req/reply
cairn.blobs.presign_download.<provider>
cairn.cmd.worker.<name>.shutdown           ← Imperative worker commands
```

Three useful wildcards:

- `cairn.jobs.fetch_source.*` — all provider workers of one kind
- `cairn.results.>` — server hears all worker results on one sub
- `cairn.events.>` — audit subscriber, analytics replays

---

## 2. JetStream streams and KV buckets

| Stream | Subjects | Retention | Storage | Discard | TTL | Purpose |
|---|---|---|---|---|---|---|
| `CAIRN_JOBS` | `cairn.jobs.>` | **WorkQueue** | file | new | none | Outgoing work; message deleted on ACK |
| `CAIRN_RESULTS` | `cairn.results.>` | **Interest** | file | old | none | Worker results; deleted once all consumers ACKed |
| `CAIRN_EVENTS` | `cairn.events.>` | **Limits** | file | old | 7d | Domain events; replayable for backfills |
| `CAIRN_DLQ` | `cairn.dlq.>` | **Limits** | file | old | 30d | Dead-lettered messages for operator inspection |

`Storage: file` instead of `memory` — worker restarts or server restarts
must not lose jobs. JetStream replication recommended `R=3` in
production clusters, `R=1` for single-node self-hosts.

**Dedup window** per stream:

- `CAIRN_JOBS`: 5 min (standard job lifetime)
- `CAIRN_RESULTS`: 1 min (result comes quickly after receipt)
- `CAIRN_EVENTS`: 1 min

Dedup uses the `Nats-Msg-Id` header. Server or worker set a
deterministic value (e.g. `"fetch:strava:<account_id>:<activity_id>"`)
so that ID-identical double publishes within the window become no-ops.

### KV buckets

For ephemeral or small-but-frequently-read data — no ballast in
streams.

| Bucket | History | TTL | What for |
|---|---|---|---|
| `cairn_worker_presence` | 1 | 60 s | `<name>.<inst>` → `{last_seen, manifest_hash, version}`. TTL clears orphaned workers. |
| `cairn_worker_manifests` | 5 | none | Per `<name>` the last 5 manifests; server diffs on change. |
| `cairn_rate_limits` | 1 | 15 min | Per `<provider>` a counter for API quotas (Strava: 200/15min). |
| `cairn_blob_handles` | 1 | 7 d | Server-signed tokens that workers can exchange for fresh presigned URLs (for long-running jobs). |

### Object-Store buckets

Separate from KV; same JetStream backbone.

| Bucket | Max Object | Max Bucket | TTL | What for |
|---|---|---|---|---|
| `cairn_transient` | 64 MB | 1 GB | 1 h | Spillover for oversized NATS result messages. NOT for long-term storage. |

S3 is the long-term blob layer (section 9), not NATS-OS.

---

## 3. Delivery semantics and idempotency contract

JetStream delivers **at-least-once**. Double delivery actually occurs with:

- Worker `AckWait` timeout (ACK was lost, message gets redelivered)
- Server crash between receive and ack
- Network partitions with re-connect

Every subscriber must be idempotent. The idempotency guarantees in
Cairn:

| Subscriber | Idempotency mechanism |
|---|---|
| Ingest router (`cairn.results.fetch_source.>`) | Stage-1 dedup over `(provider, account_id, ext_id)`. Double push → `handleReimport`. |
| Best-effort compute | `DELETE for source ... INSERT new` pattern in one tx. |
| Segment match | same `DELETE then INSERT` pattern; rank recompute idempotent. |
| Training load | `external_id = "computed:<date>"` + partial-unique-index UPSERT. |
| PR notifications | `dedup_key = "segment_pr:<seg>:<date>"` + CTE coalesce-or-insert. |
| External-account-reauth subscriber | Notification DedupKey `"reauth:<account_id>"`. |

Two-layer dedup architecture:

1. **JetStream layer** over `Nats-Msg-Id` header — catches hot-loop
   double-pubs at the network layer.
2. **Application layer** over content-based dedup keys — catches everything
   else, including legitimate "same workout, different source trigger".

Operator consequence: a consumer can be replayed from the stream start
without concerns. All side effects run deterministically into the same
end state.

---

## 4. Subject auth and permissions

NATS supports fine-grained ACLs per connection. Three identities:

| Identity | Allow Publish | Allow Subscribe |
|---|---|---|
| Server (`cairn-server`) | `cairn.jobs.>`, `cairn.cmd.>`, `cairn.events.>`, `cairn.tokens.*.fetch` replies, `cairn.blobs.presign_*.>` replies | `cairn.results.>`, `cairn.events.>`, `cairn.workers.>`, `cairn.tokens.*.fetch` requests, `cairn.blobs.presign_*.>` requests |
| Worker `strava-fetcher` | `cairn.results.fetch_source.strava`, `cairn.results.parse_blob.strava`, `cairn.results.backfill.strava`, `cairn.workers.strava-fetcher.>`, `cairn.tokens.strava.fetch` requests, `cairn.blobs.presign_*.strava` requests | `cairn.jobs.fetch_source.strava`, `cairn.jobs.parse_blob.strava`, `cairn.jobs.backfill.strava`, `cairn.cmd.worker.strava-fetcher.>` |
| Audit subscriber | (none) | `cairn.events.>` |

### How is the Strava worker prevented from seeing Garmin credentials?

Two-layer defense — **NATS-layer permission isolation** plus
**server-side application-layer validation**. One alone is not enough;
both together protect even when one layer is misconfigured.

**Layer 1 — NATS server config (account/permission separation):**

The Strava worker authenticates with its own set of credentials
(NKey pair or NATS JWT). The permission definition for `strava-fetcher`
allows **explicitly only** subjects with `.strava` as the last token:

```hcl
# nats-server.conf (operator-account variant)
accounts: {
  cairn: {
    users: [
      { user: "cairn-server", password: "...", permissions: {
        publish:   ["cairn.jobs.>", "cairn.events.>", "_INBOX.>"]
        subscribe: ["cairn.results.>", "cairn.events.>", "cairn.workers.>",
                    "cairn.tokens.*.fetch", "cairn.blobs.presign_*.>",
                    "_INBOX.>"]
      }}
      { user: "worker-strava", password: "...", permissions: {
        publish: [
          "cairn.results.fetch_source.strava",
          "cairn.results.parse_blob.strava",
          "cairn.results.backfill.strava",
          "cairn.results.reconcile.strava.*",   # account-suffixed
          "cairn.results.import_metrics.strava",
          "cairn.results.import_gear.strava",
          "cairn.jobs.fetch_source.strava",     # reconcile/webhook sub-jobs
          "cairn.workers.strava-fetcher.>",
          "cairn.tokens.strava.fetch",          # ← ".strava" hard-coded
          "cairn.creds.strava.get",
          "cairn.blobs.presign_upload.strava",
          "cairn.blobs.presign_download.strava"
        ]
        subscribe: [
          "cairn.jobs.fetch_source.strava",
          "cairn.jobs.parse_blob.strava",
          "cairn.jobs.backfill.strava",
          "cairn.jobs.reconcile.strava",
          "cairn.jobs.import_metrics.strava",
          "cairn.jobs.import_gear.strava",
          "cairn.cmd.worker.strava-fetcher.>",
          "cairn.discover.strava",
          "_INBOX.>"                            # for request/reply
        ]
      }}
      { user: "worker-garmin", password: "...", permissions: {
        # ... analogous, only .garmin subjects, no .strava
      }}
    ]
  }
}
```

Enrolled workers get these allow-lists from
`domain.StandardPermissionTemplate` through the auth-callout. Besides the
subjects above, the template grants the JetStream API subjects workers need,
and nothing broader than `$JS.API.STREAM.NAMES`:

- `$JS.API.STREAM.NAMES`, so the bus can tell stream from core subjects
  before publishing.
- Consumer create on `CAIRN_JOBS` / `CAIRN_WEBHOOKS` with the filter limited
  to the worker's own job and webhook-event subjects; the legacy
  `CONSUMER.DURABLE.CREATE.CAIRN_JOBS.*` for nats-py; consumer info,
  `MSG.NEXT` and `$JS.ACK.<stream>.>` on those two streams.
- Info, direct/message get and `$KV.<bucket>.>` put on the
  `cairn_worker_presence`, `cairn_worker_manifests` and `cairn_rate_limits`
  buckets.

The consumer-name token is a wildcard because the SDKs name durables
differently, so a worker can address other durables on those two streams;
scoping it needs deterministic per-worker durable names.

NATS rejects every publish and every subscribe on disallowed subjects
with `-ERR 'Permissions Violation'`. If `worker-strava` tries to
publish `cairn.tokens.garmin.fetch` — it is not even accepted by the
server in the first place. The connection stays open, but that one publish fails.

**Layer 2 — server-side validation in the token handler:**

Even if layer 1 fails (operator configured permissions
incorrectly, NATS server ran without auth by accident), the
server-side handler checks consistency:

```go
// cairn-server's RespondTo handler for cairn.tokens.<provider>.fetch
func (h *TokenHandler) Handle(ctx context.Context, msg port.Message) ([]byte, error) {
    // Subject is e.g. "cairn.tokens.strava.fetch" — parse it.
    parts := strings.Split(msg.Subject, ".")
    if len(parts) != 4 || parts[0] != "cairn" || parts[1] != "tokens" || parts[3] != "fetch" {
        return nil, errors.New("invalid token subject")
    }
    requestedProvider := parts[2]   // "strava"

    var req TokenFetchRequest
    if err := json.Unmarshal(msg.Body, &req); err != nil { ... }

    acct, err := h.accounts.GetExternalAccount(ctx, req.AccountID)
    if err != nil { ... }

    // The critical check: the account's provider must match the subject's.
    if acct.Provider != requestedProvider {
        // A strava-fetcher slipped in a Garmin account UUID.
        // Responds with error, logs as security event.
        h.logger.Warn("token-fetch provider mismatch",
            "subject_provider", requestedProvider,
            "account_provider", acct.Provider,
            "account_id", req.AccountID)
        return nil, errors.New("provider mismatch")
    }

    // Only now decrypt and return.
    token, err := h.decrypt(acct.AccessTokenEncrypted)
    ...
}
```

The worker cannot forge the subject — it publishes to its
allowed `cairn.tokens.strava.fetch`. If it specifies a Garmin account
UUID in the request body, the server responds with an error. The
account IDs are UUIDs, so not guessable; but even if a
malicious Strava worker gets hold of a Garmin account UUID (via
side channels), it gets no token.

**Worker process separation as a third layer:**

Recommendation for deployment: each provider worker runs in its own
container/pod with its own NATS credentials. Compromise of a Strava
worker exfiltrates no Garmin tokens, because it does not even have the
NATS credentials that would be needed to address the subject.

**What actively cannot be enforced:**

- A compromised Strava worker can exfiltrate its **own** tokens
  — it inevitably sees them in-process. Mitigation
  here is token lifetime (Strava: 6h) and audit trail over
  `cairn.events.>` (every token fetch should be logged as an event).
- NATS has no fine-grained "user X can only query account Y"
  permissions. Provider isolation is feasible, per-account isolation
  within a provider is not — the Strava worker sees all Strava
  accounts. Acceptable limitation for single-tenant self-hosted.

---

## 4.5 Worker Enrollment & Dynamic NATS Credentials

Statically configured NATS accounts per worker type are operationally
painful: every new worker (or a new provider, or a
canary migration) requires a NATS server config reload, secrets
distribution, manual synchronization between server and worker setup.
Cairn solves this with **dynamic worker enrollment** based on
NATS Auth Callout (NATS 2.10+; currently 2.14).

**What the admin sees** (two commands, one environment variable):

```
# Admin at the Cairn server:
$ cairn worker enrollment create \
    --provider=strava \
    --worker-name="strava-fetcher" \
    --expires-in=24h \
    --max-uses=1 \
    --note="prod cluster A pod-12"

Enrollment token (copy now — will NOT be shown again):
  cairn_enroll_3kF8vR1nQXjQ-2pZ7mYwL5tH4dGqW6KsM8nVxBcEoUg

Set in the worker container:
  CAIRN_NATS_URL=nats://nats.cairn.example:4222
  CAIRN_WORKER_NAME=strava-fetcher
  CAIRN_WORKER_ENROLLMENT_TOKEN=cairn_enroll_3kF8vR1nQXjQ...
```

**What the worker does** (autonomously, without further admin action):

```go
// cmd/worker-strava/main.go (simplified)
func main() {
    natsURL := os.Getenv("CAIRN_NATS_URL")
    token   := os.Getenv("CAIRN_WORKER_ENROLLMENT_TOKEN")
    name    := os.Getenv("CAIRN_WORKER_NAME")

    // Ephemeral NKey — fresh per worker process, never persisted
    userKP, _ := nkeys.CreateUser()
    userPub, _ := userKP.PublicKey()

    nc, err := nats.Connect(natsURL,
        nats.Name(name),
        nats.Token(token),                            // CONNECT.token
        nats.Nkey(userPub, userKP.Sign),              // CONNECT.nkey + signed nonce
        nats.MaxReconnects(-1),
        nats.ReconnectWait(2*time.Second),
    )
    // from here: nc is connected with cairn.*.strava permissions
}
```

From the worker's point of view this is the **same connect code as always**.
NATS negotiates auth callout with the Cairn server in the background.

### What happens in the background

```
┌─────────────┐    1. CONNECT (token, nkey)     ┌────────────┐
│  Worker     │ ──────────────────────────────> │ nats-server│
│  Container  │                                 └─────┬──────┘
└─────────────┘                                       │
                                                      │ 2. AuthRequest
                                                      │    $SYS.REQ.USER.AUTH
                                                      ▼
                                              ┌─────────────┐
                                              │ cairn-server│
                                              │  callout-   │
                                              │  handler    │
                                              └─────┬───────┘
                                                    │
                                                    │ 3. SELECT FROM worker_enrollments
                                                    │    WHERE token_hash = sha256($token)
                                                    │ 4. validate (expires_at, max_uses, revoked_at)
                                                    │ 5. INSERT worker_credential_grants
                                                    │ 6. mint user-JWT with permissions
                                                    │    signed by account NKey
                                                    │
                                              ┌─────▼───────┐
                                              │ AuthResponse│
                                              │ (user-JWT)  │
                                              └─────┬───────┘
                                                    │ 7. ack
                                                    ▼
┌─────────────┐    8. CONNECT OK +              ┌────────────┐
│  Worker     │       Permissions               │ nats-server│
│  (connected)│ <────────────────────────────── │            │
└─────────────┘                                 └────────────┘
```

The entire round-trip is a NATS RPC, typically <10ms. The worker feels
no difference to a direct connect.

### Three DB tables (Migration 14)

```
worker_enrollments
  id, token_hash (sha256), provider, worker_name_pattern,
  expires_at, max_uses, uses, revoked_at, ...
  Notes: plaintext token NEVER stored. token_hash UNIQUE INDEX.
  provider is required on create (one of core's known providers); older
  rows with an empty provider are refused at connect ("re-enroll required").

worker_credential_grants
  id, enrollment_id (FK), worker_id (FK), user_nkey_public (UNIQUE),
  worker_name, worker_instance_id, worker_version,
  issued_at, expires_at, last_seen_at, revoked_at, ...
  Notes: audit trail of EVERY admitted connection. Revoke can happen at
  grant level (kicks out a single worker) OR at
  enrollment level (all from this enrollment).

instance_signing_keys
  id, purpose='nats_account', public_key, seed_encrypted, active, ...
  Notes: the only long-lived crypto material on the Cairn server side.
  Seed AES-GCM-encrypted with CAIRN_TOKEN_ENCRYPTION_KEY. Rotation
  via a second active=true row, NATS config update, old row
  deactivated.
```

### Token lifecycle options

Three modes, driven by `max_uses` and `expires_in`:

| Mode | max_uses | expires_in | What for |
|---|---|---|---|
| **One-shot bootstrap** | 1 | 24h | High-security single deployment: token is used once, the worker then has its NATS user JWT (valid 24h, then re-enrollment) |
| **N-shot replica bootstrap** | 3..N | 1h..24h | K8s pod replicas: pod comes up, enrolls, other pods of the same set use the same token (stateless) |
| **Long-lived shared** | 0 (unlimited) | 90d | Single-operator setup without CI/CD: token rotation only quarterly, worker re-enrolls on every restart |

Cairn recommends one-shot or N-shot for production. Long-lived is for
hobbyist self-hosted setups, where token rotation is operationally more expensive than
the marginal security plus justifies.

### The NATS server config

The working config is `docker/nats-auth-callout.conf` (server config mode,
no operator). The essentials:

```
accounts {
  CAIRN {
    jetstream: enabled
    users: [
      { user: cairn-core, password: $CAIRN_NATS_PASSWORD }    # core itself
      { nkey: U... }    # seedless; makes nats-server send the connect nonce
    ]
  }
}
authorization {
  auth_callout {
    issuer: $CAIRN_NATS_AUTH_ISSUER   # POST /api/admin/nats/bootstrap-account-key
    account: CAIRN
    auth_users: [ cairn-core ]        # bypass the callout
  }
}
```

Core runs with `CAIRN_NATS_USERNAME=cairn-core`, `CAIRN_NATS_PASSWORD`,
`CAIRN_NATS_AUTH_CALLOUT=true` and `CAIRN_NATS_WORKER_ACCOUNT=CAIRN`. Every
other connection goes through the callout: core replies with a response
whose `aud` is the requesting server's ID, carrying a user JWT signed by the
account key (`iss`), with `aud` = the worker account and no `issuer_account`
(nats-server refuses it in config mode). Workers and core share the CAIRN
account; the per-worker permissions keep workers apart. Admitted users are
also denied publishing to `$SYS.REQ.USER.AUTH` by nats-server.

A worker that cannot present an enrollment token (the Garmin worker) needs a
static user in the CAIRN account, listed in `auth_users`, with the standard
template's subjects as its permissions.

### Backward-compat path: HTTP enrollment

For NATS servers < 2.10 (or setups where the operator does not want to
enable auth callout), there is an HTTP fallback:

```
Worker → POST /api/workers/enroll
  Authorization: Bearer <enrollment-token>
  Body: { worker_name, instance_id, version, user_nkey_public }

Cairn-Server:
  - same validation as auth-callout
  - mints user-JWT with same permissions
  - reply: { nats_url, user_jwt, user_seed?, expires_at }
            (user_seed only if worker did not generate an NKey itself)

Worker:
  nats.Connect(nats_url,
    nats.UserJWTAndSeed(user_jwt, user_seed))
```

Same DB state, same permissions path. Operational trade-off: the
HTTP path requires that worker and Cairn server are directly reachable
(in the same VPC, with TLS). Auth callout runs transparently over NATS, which
typically hangs between worker and server anyway.

### Use-case layer (see `internal/usecase/enrollment/`)

```go
type CreateWorkerEnrollment struct{ ... }
//   Admin command. Generates 32-byte token, hashes, stores. Token
//   plaintext is returned once, after that nowhere anymore.

type ProcessAuthCallout struct{ ... }
//   Hot path. Called from the NATS adapter when an
//   auth-callout request comes in. Validates token, mints JWT,
//   writes audit grant. Refuses enrollments without a provider
//   (reason enrollment_missing_provider); the issuer also refuses
//   to sign empty allow lists, which NATS reads as "allow all".

type RevokeWorkerEnrollment struct{ ... }
//   Admin command. Marks enrollment as revoked. Future connects
//   fail. Already-open connections run until JWT expiry
//   — the admin can additionally do a grant-level revoke +
//   publish cairn.cmd.worker.<name>.shutdown to kick immediately.
```

### Security model summarized

| Threat | Mitigation |
|---|---|
| Enrollment token in worker container compromised | TTL ≤ 24h and max_uses=1 for high-security; revoke via admin command |
| Worker itself compromised (code execution in the worker process) | Worker seed in-RAM only, never persisted. Restart = new NKey. JWT 24h. |
| Malicious worker tries to subscribe to Garmin subjects | NATS rejects with permission violation (JWT contains only strava subjects) |
| Malicious worker forges provider in token-fetch request | Server-side validation: `account.Provider != subject_provider` → error |
| Malicious Cairn server operator mints own JWTs | THIS is the trust anchor. Single-tenant self-hosted: whoever runs the Cairn instance is allowed to do that. Multi-tenant is out of scope. |
| Compromised Cairn server (code execution) | Worst case — attacker has all tokens, can admit arbitrary workers. Mitigation: secrets-at-rest encryption, audit trail in worker_credential_grants is append-only-evident |
| Replay of old enrollment tokens after revoke | revoked_at check in the hot path, before token-use increment |
| Race: two workers use the same token simultaneously | Token increment + grant insert in one transaction; with max_uses=1 the DB constraint admits only one |

### What is not included in Migration 14 (deliberate)

- **Webhook for worker-lifecycle events.** "Worker enrolled" /
  "Worker grant revoked" could publish events on `cairn.events.worker.>`.
  v1 does not do this — operator awareness runs over the
  audit log and admin UI listings.
- **Auto-rotation of account signing keys.** v1 has manual rotation
  via admin command. Auto-rotation would be a cron-job layer on top.
- **Per-user workers.** A "this worker is for user X only" model
  would require per-user NATS accounts. Currently the Strava worker sees
  all Strava accounts (with provider validation in the token handler);
  per-user isolation only makes sense once multi-tenant comes.

---

## 5. The `port.JobBus` interface

```go
// port/job_bus.go

package port

import (
    "context"
    "time"
)

// JobBus is the abstract message bus interface. The NATS adapter lives
// under internal/adapter/secondary/nats/. Use cases depend on JobBus,
// not on nats.Conn directly.
//
// "JobBus" rather than "MessageQueue" because cairn doesn't pretend
// other backends (Kafka, SQS) are drop-in replacements — their stream
// model, ACL story, and durability semantics differ enough that swapping
// would require a different interface anyway. Hexagonal architecture
// doesn't mean "every adapter is swap-able"; it means business logic
// has no idea about the transport.
type JobBus interface {
    // Publish enqueues a message. msgID makes the publish idempotent
    // within the stream's dedup window — pass a deterministic ID
    // (e.g., "fetch:strava:<account_id>:<ext_id>") for retry-safe
    // publishing.
    Publish(ctx context.Context, subject string, msgID string, body []byte, opts ...PublishOpt) error

    // Subscribe registers a durable push consumer. Server uses this for
    // result-routing and event-processing. handler runs synchronously;
    // returning nil ACKs, returning a non-nil error NAKs (with optional
    // delay via TerminalError or NakDelayError).
    Subscribe(ctx context.Context, cfg ConsumerConfig, handler MessageHandler) (Subscription, error)

    // Pull registers a pull consumer. Workers use this — they fetch in
    // their own loop with explicit batch sizes for backpressure control.
    Pull(ctx context.Context, cfg ConsumerConfig) (PullSubscription, error)

    // Request is sync request/reply. Used for token-fetch and
    // presign-URL flows. Rare — most communication is fire-and-forget.
    Request(ctx context.Context, subject string, body []byte, timeout time.Duration) ([]byte, error)

    // RespondTo registers a request/reply handler (the server side of
    // a Request call). The handler returns the reply payload or an
    // error; errors map to NATS-style error responses.
    RespondTo(ctx context.Context, subject string, handler RequestHandler) (Subscription, error)

    // KV gives access to the named KV bucket.
    KV(bucket string) (KV, error)

    // ObjectStore gives access to the named OS bucket.
    ObjectStore(bucket string) (ObjectStore, error)
}

type ConsumerConfig struct {
    Stream          string
    Durable         string     // durable name; persists across restarts
    Subject         string     // filter, supports wildcards
    QueueGroup      string     // for load balancing in pull mode
    AckWait         time.Duration
    MaxDeliver      int
    DeliverPolicy   string     // "all" | "new" | "last" | "by_start_sequence"
    BackoffSchedule []time.Duration
}

type MessageHandler func(ctx context.Context, msg Message) error
type RequestHandler func(ctx context.Context, body []byte) ([]byte, error)

type Message struct {
    Subject     string
    Headers     map[string]string
    Body        []byte
    DeliveryAttempt int
}

// Subscription / PullSubscription / KV / ObjectStore interfaces follow
// the obvious shape — Close, Get/Set/Delete, etc.
```

**Stream bootstrap** runs at server start in `cmd/server/serve.go`,
idempotent:

```go
func bootstrapStreams(ctx context.Context, bus JobBus) error {
    if err := bus.EnsureStream(ctx, StreamConfig{
        Name:     "CAIRN_JOBS",
        Subjects: []string{"cairn.jobs.>"},
        Retention: WorkQueueRetention,
        Storage:  FileStorage,
        Discard:  DiscardNew,
        Replicas: replicasFromEnv("CAIRN_NATS_REPLICAS", 1),
        Duplicates: 5 * time.Minute,
    }); err != nil {
        return err
    }
    // ... CAIRN_RESULTS, CAIRN_EVENTS, CAIRN_DLQ analogous
    // ... KV buckets, OS buckets analogous
    return nil
}
```

The operator can disable this via `CAIRN_NATS_BOOTSTRAP_STREAMS=false`,
if streams are pre-provisioned via `nats stream add`.

---

## 6. Provider integration using Strava as an example

Three phases: (1) user initially links their account, (2) backfill of all
past activities, (3) ongoing updates via webhook.

### Phase 1: OAuth initial linking (HTTP, not NATS)

```
User clicks "Connect Strava" in the UI:

GET /oauth/strava/start
  → Server generates signed state token (short TTL)
  → 302 Redirect: https://www.strava.com/oauth/authorize?
                    client_id=...&response_type=code&
                    redirect_uri=https://cairn.example/oauth/strava/callback&
                    scope=read,activity:read_all&state=<token>

User authorizes in the Strava UI, redirected back:

GET /oauth/strava/callback?code=abc123&state=<token>
  → Server validates state
  → POST https://www.strava.com/oauth/token  (grant_type=authorization_code)
    → Response: {access_token, refresh_token, expires_at, athlete: {id, ...}}
  → INSERT INTO external_accounts (
        user_id, provider='strava', external_id=<athlete.id>,
        access_token_encrypted, refresh_token_encrypted, expires_at,
        scopes='read,activity:read_all', status='active'
    )
  → publish cairn.events.external_account.connected
      headers: Nats-Msg-Id = "connected:<account_id>"
      body:    { account_id, user_id, provider: "strava" }
  → 302 Redirect to the UI ("Strava connected")
```

Token storage: AES-GCM encrypted-at-rest with `CAIRN_TOKEN_ENCRYPTION_KEY`.
The server is the **only** decryption instance; workers see plain tokens
only in-process via the token-fetch API (Phase 2.2).

### Phase 2: Initial backfill

**2.1 Publish backfill job** — the subscriber on
`cairn.events.external_account.connected` is the `EnqueueBackfill`
use-case:

```
publish cairn.jobs.backfill.strava
  headers:
    Nats-Msg-Id    = "backfill:<account_id>"
    Cairn-Reply-To = cairn.results.backfill.strava
    Cairn-Job-Id   = <new-uuid-v7>
  body:
    {
      "job_id":       "0190a1...",
      "account_id":   "84f3-...",
      "user_id":      "abc1-...",
      "provider":     "strava",
      "athlete_id":   "12345",
      "since":        null,
      "page_size":    100,
      "max_activities": null
    }
```

Stream `CAIRN_JOBS` takes it up. Pull consumer `cairn-fetcher-strava`
of the queue group `strava-workers` pulls. One worker gets the job,
`AckWait = 30 min`.

**2.2 Worker fetches OAuth token via request/reply** — the worker has no
DB connection:

```
Worker → request cairn.tokens.strava.fetch
  headers: Cairn-Worker-Name = "strava-fetcher"
           Cairn-Worker-Inst = "abc-123"
  body:    { account_id: "84f3-..." }
  reply-subject: _INBOX.<random>
  timeout: 5s

Server handler:
  - SELECT * FROM external_accounts WHERE id = $1
  - if expires_at - now() < 5min:
      → POST https://www.strava.com/oauth/token (grant_type=refresh_token)
      → UPDATE external_accounts SET ...
  - reply:
      headers: Cairn-Token-Expires-At = <unix>
      body:    { access_token: "...", expires_at: ..., scope: "..." }

Worker caches the token in-process until 1min before expires_at,
then re-requests request/reply.
```

Properties:

- **The token never leaves the Cairn bounded context.** Worker in-memory only,
  persists nothing, worker restart = new fetch.
- **The server is the sole refresher.** Prevents the well-known pitfall that
  two workers redeem the `refresh_token` in parallel and Strava invalidates the first
  one.
- **Request/reply, no stream.** Sync enough for in-worker logic.

**2.3 Worker paginates the activity list:**

```
Worker → GET https://www.strava.com/api/v3/athlete/activities?
           page=1&per_page=100
         Authorization: Bearer <access_token>
       ← 200, [ {id, name, start_date, ...}, ... ]
... until empty page ...
```

For each activity stub, the worker publishes **no** result directly, but
a sub-job:

```
for each activity-stub:
  publish cairn.jobs.fetch_source.strava
    headers:
      Nats-Msg-Id    = "fetch:strava:<athlete_id>:<activity_id>"
      Cairn-Reply-To = cairn.results.fetch_source.strava
      Cairn-Job-Id   = <new-uuid-v7>
    body:
      {
        "job_id":        ...,
        "account_id":    ...,
        "user_id":       ...,
        "provider":      "strava",
        "ext_id":        "11111",
        "fetch_streams": true,
        "reason":        "backfill"
      }
```

After the last page: summary result and ACK:

```
publish cairn.results.backfill.strava
  body: { activities_enqueued: 437, took_seconds: 18 }
ack(backfill-job)
```

Advantage of the split: the backfill worker only does "fetch list", individual
activity fetches parallelize over the queue group. With 437 activities
and 5 worker instances, the backfill is 5× faster. Also
fail isolation: activity 234 unfetchable → the other 436 still finish.

**2.4 Sub-job: fetch activity including streams:**

```
Worker → token-cache check (otherwise request/reply)
Worker → GET /api/v3/activities/11111?include_all_efforts=true
       ← 200, { id, name, distance, moving_time, ... }
Worker → GET /api/v3/activities/11111/streams?keys=time,latlng,altitude,heartrate,watts,cadence,velocity_smooth
       ← 200, { time: {data:[...]}, latlng: {...}, ... }

Worker (in process):
  - map Strava fields → ActivitySourcePayload
  - stream channels → []domain.StreamSample (1Hz)
  - if raw JSON is to be kept:
      see section 9 (blob upload via presigned PUT)

Worker → publish cairn.results.fetch_source.strava
  headers:
    Cairn-Job-Id = <fetch-job-id>
    Nats-Msg-Id  = "result:<job_id>"
  body (protobuf, here as JSON):
    {
      "job_id":    "...",
      "ok":        true,
      "ingest_input": {
        "user_id":              "...",
        "provider":             "strava",
        "external_account_id":  "...",
        "external_id":          "11111",
        "source_worker_name":   "strava-fetcher",
        "source_worker_version": "v0.4.2",
        "source_manifest_hash": "sha256:...",
        "raw_blob_id":          "raw/strava/.../0190a2-....bundle.tar",
        "raw_content_type":     "application/x-tar",
        "raw_size_bytes":       142387,
        "payload":              { /* ActivitySourcePayload */ },
        "stream":               [ /* 3712 sample objects */ ]
      }
    }
```

For an oversized payload (>4 MB after protobuf encode): spillover over
NATS Object Store, see section 9.

**2.5 Server consumes the result:**

```
Server (durable push consumer "ingest-router" on cairn.results.fetch_source.>):
  - decode body → IngestInput
  - call IngestActivityFromWorker.Execute(ctx, IngestInput)
      → identity lookup on (provider, external_account_id, external_id)
      → hit:  handleReimport (replace the source payload in place)
      → miss: handleCreateNew (persist as a new singleton activity)
  - call match.ReclusterBucket over the user's ±24h window around the
    activity start: fuzzy scoring + union-find across every source record
    in the bucket, stable-id reconciliation (split/merge), re-merge of each
    affected activity
  - publish cairn.events.activity.changed (kind=ingested)
      headers: Nats-Msg-Id = "activity-changed:ingested:<activity_id>:<uuid>"
      body:    { kind, user_id, activity_id, source_id, action, msg_id }
  - ack(result-message)
```

Ingest only resolves a source's identity; "which sources are the same
real-world activity" is a derived, re-runnable decision, never an
irreversible attach-on-ingest. The invariant is
`merged = derive(archived sources, priority rules, manual overrides)`.
The matcher (`internal/domain/match`) is pure and deterministic, buckets by
UTC time, allows at most one source per provider per activity (a
same-provider collision is logged as a conflict, not merged), assigns a
confidence band per cluster (high → auto, medium → review queue, low → kept
separate), and honours manual must-link / cannot-link constraints.
Detaching a source also records its provider identity in a denylist; on
re-cluster a denied identity is forced into a standalone singleton, so a
source the user detached does not re-attach when the provider pushes it
again under a new source id.

The follow-up consumer `activity-follow-ups` (stream aggregates, best
efforts, segments, training load, PR dispatch, federation) listens on
`cairn.events.activity.changed`. See section 8.

### Phase 3: Ongoing updates via webhook

Strava calls the Cairn server on every new / edited / deleted
activity:

```
POST /webhooks/strava
  Body: {
    "object_type":   "activity",
    "object_id":     22222,
    "aspect_type":   "create" | "update" | "delete",
    "owner_id":      12345,
    "subscription_id": 1234,
    "event_time":    1715901234
  }

Server:
  - validate Strava verify token
  - SELECT * FROM external_accounts WHERE provider='strava' AND external_id='12345'
  - if not found: 200 OK (orphaned webhook — harmless to ignore)
  - if aspect_type == "delete":
      → publish cairn.events.source.deleted_upstream
          body: { account_id, ext_id }
        Subscriber soft-detaches the source (status='detached',
        reason='deleted_upstream'), triggers recompute.
  - else (create | update):
      → publish cairn.jobs.fetch_source.strava
          headers: Nats-Msg-Id = "fetch:strava:12345:22222"
          body:    { ..., reason: "webhook", aspect_type }
  - 200 OK to Strava (in <500ms; entire path <2s)
```

Stage 1 hits for `update` (source exists), `handleReimport`
replaces the payload. Multi-source guarantee: `recompute` loads all
non-detached sources, re-merges.

On Strava webhook redelivery (Cairn was down): same `Nats-Msg-Id`
→ JetStream dedup throws away the duplicate.

### Rate-limit coordination

Strava: 200 requests / 15 min AND 2000 / day per **app** (not per user).
With multiple worker instances the budget is shared.
Three mechanisms together solve this:

**1. Per-bucket counter in the NATS KV** — the `port.RateLimiter`
(see `internal/port/rate_limiter.go`) is KV-backed. Workers Reserve
**before** every API call, atomically across all worker instances:

```
Bucket cairn_rate_limits, two keys per provider:
  strava:short  = { window_start, count, capacity: 200 }    TTL 15 min
  strava:daily  = { window_start, count, capacity: 2000 }   TTL 24 h

Worker (before every API call):
  ok1, retry1, _ := limiter.Reserve(ctx, "strava:short", 1)
  ok2, retry2, _ := limiter.Reserve(ctx, "strava:daily", 1)
  if !ok1 || !ok2 {
      wait := max(retry1, retry2)
      return NakWithDelay(wait)    ← Job comes back automatically in N seconds
  }
  // proceed with the API call
```

`Reserve` is atomic via `JS KV CompareAndSet` (see `port.KV` interface).
On contention between two worker instances, one wins the
CompareAndSet, the other retries with the new revision. Standard
optimistic-concurrency pattern.

**2. ForceRefill after 429** — Strava's server count is always ground truth.
Our KV counter is a conservative approximation; on drift there is
the 429. Then we sync hard:

```
Worker gets 429:
  - Response header: Retry-After: 587, X-RateLimit-Usage: 200, X-RateLimit-Limit: 200
  - parse → reset_at = now + 587s
  - limiter.ForceRefill(ctx, "strava:short", available=0, windowResetsAt=reset_at)
  - all worker instances immediately see the block (KV revision rises)
  - NakWithDelay(587s) on the current job
  - publish cairn.events.external_account.rate_limited
      body: { account_id, provider, reset_at, retry_after_seconds }
```

The subscriber reaction to `rate_limited`:

```
Server-side subscriber:
  - UPDATE external_accounts SET
        status = 'rate_limited',
        rate_limit = jsonb_build_object(
          'short_window_used',    200,
          'short_window_limit',   200,
          'short_window_reset_at', reset_at,
          'reported',             now()
        )
    WHERE id = ...
  - Scheduler ListAccountsDueForReconcile filters out these accounts via
    IsEligibleForSync until reset_at → no additional jobs in
    the cooldown phase
```

This way **operator UI**, **reconcile scheduler** and **status endpoints**
all see the same rate-limit info, without anyone having to ask Strava
again separately.

**3. Subject priority via separate subjects** — NATS has no native
priority queues (priority groups are NATS-2.11+ experimental). Cairn
solves it via **three separate subjects per job type**, in descending
priority:

```
cairn.jobs.fetch_source.strava.webhook     ← User just uploaded,
                                             tangible in the UI
cairn.jobs.fetch_source.strava.reconcile   ← drift safety, should be timely
cairn.jobs.fetch_source.strava.backfill    ← initial mass import,
                                             tolerates hours of latency
```

Workers pull with different MaxBatch per subject in the same
loop:

```go
// Worker main loop (simplified):
for {
    msgs, _ := sub_webhook.Fetch(ctx, 10)        // drain webhook first
    if len(msgs) == 0 {
        msgs, _ = sub_reconcile.Fetch(ctx, 5)
    }
    if len(msgs) == 0 {
        msgs, _ = sub_backfill.Fetch(ctx, 2)     // small batch so webhooks
                                                 // can interrupt quickly
    }
    process(msgs)
}
```

Under rate-limit pressure, webhook jobs take the token first —
exactly what the user expects ("I just uploaded, that should
appear immediately"). A backfill pause during active webhook season is
acceptable.

**Per-provider tuning** — buckets are provider-specific, because the
limits are. Garmin: 25 / 4s. Polar: 5 / 15s. FIT-parser workers
without an external API: no bucket. The bucket names are convention, not
schema — new providers define them in their worker code.

### 6.4 Reconciliation Sync

Webhooks are **best-effort** on Strava's side — they can get lost on
provider outages or network problems. There must therefore
be a periodic reconcile path that checks "which activities
exist at Strava that we don't have yet". Plus: providers without
webhook support (e.g. older Garmin APIs) need exactly this path
as the **primary** import mechanism.

**Use case:** `usecase/sync.ReconcileExternalAccount` (exists,
see `internal/usecase/sync/reconcile.go`).

**Schedule** — encoded in the repository query
`ListAccountsDueForReconcile`:

| Account state | Reconcile interval |
|---|---|
| `webhook_subscribed = true`, `last_sync_at < now − 24h` | daily (drift safety) |
| `webhook_subscribed = false`, `last_sync_at < now − 5min` | every 5 min (primary) |
| `status != active` (auth_invalid, disabled, …) | not reconciled |
| `status = rate_limited`, `rate_limit.reset_at > now` | not reconciled |

Both intervals are overridable via `instance_settings` (for
operators who want to temporarily reconcile more aggressively during a
Strava-API outage or, conversely, thin out for low API-quota tiers).

**Trigger paths:**

```
1. Scheduler goroutine in cmd/server/serve.go:
     for {
       <-ticker.C  (every 60s default)
       res, _ := reconcileUC.Execute(ctx, ReconcileInput{All: true, BatchSize: 100})
       log.Info("reconcile tick", "scheduled", res.AccountsScheduled,
                "skipped", res.AccountsSkipped, "errors", len(res.Errors))
     }

2. Manual trigger via admin endpoint:
     POST /admin/external-accounts/<id>/reconcile?force=true
     → reconcileUC.Execute(ctx, ReconcileInput{
         AccountID: &id, Force: r.URL.Query().Get("force") == "true"
       })

3. Post-reauth subscriber on cairn.events.external_account.reconnected:
     reconcileUC.Execute(ctx, ReconcileInput{
       AccountID: &accountID, Force: true, Reason: "post_reauth"
     })
```

**Worker-side algorithm** (e.g. in the strava-fetcher):

```
Job: cairn.jobs.reconcile.strava
Body: { account_id, user_id, watermark, webhook_seen, reason }

Worker:
  1. token-fetch via cairn.tokens.strava.fetch
  2. GET /api/v3/athlete/activities?after=<watermark>&per_page=100&page=1
  3. Paginate until empty page. Collects a list [(ext_id, start_time), …]
  4. publish cairn.results.reconcile.strava
       body: {
         account_id, ok: true,
         activities_seen: [<ext_id>, <ext_id>, …],
         newest_start_time: <max start_time>
       }
  5. ACK the reconcile job

Server handler (subscriber on cairn.results.reconcile.strava):
  1. SELECT external_id FROM activity_sources
       WHERE external_account_id = $1
         AND provider = 'strava'
         AND status != 'detached'
  2. diff: activities_seen MINUS db_known = missing
  3. for each missing ext_id:
       publish cairn.jobs.fetch_source.strava.reconcile
         headers: Nats-Msg-Id = "fetch:strava:<athlete>:<ext_id>:reconcile"
         body:    { account_id, ext_id, reason: "reconcile" }
  4. UPDATE external_accounts SET
       sync_watermark = newest_start_time,
       last_sync_at = now(),
       last_successful_sync_at = now()
     WHERE id = $1
```

The reconcile job only lists activity IDs, fetches no data. Only the
per-activity sub-job (`fetch_source.strava.reconcile`) makes the
actual API call with streams. This way the rate-limit budget
is not eaten up in one go — a reconcile listing costs
1 API call per page (say 5 calls for 500 activities), the
actual fetches spread over hours to days.

**Idempotency:** Reconcile may run twice at any time.
- JetStream dedup on the `reconcile` job (minute bucket in the msg_id)
- Stage-1 dedup in the sub-job (`fetch_source`) prevents duplicate sources
- Watermark update is last-write-wins, harmless on race

**Watermark semantics:** `sync_watermark = newest_start_time` means
"all activities with start_time ≤ watermark have been reconciled".
On the next reconcile we ask Strava only for `after=<watermark>`.
A later-submitted activity with start_time < watermark (e.g.
manually uploaded old workout) would be missed without precaution.
Solution: on `aspect_type=create` in the webhook the handler does **not**
set the watermark to the start_time — the webhook sub-job logic
takes care of the import itself. Reconcile is only for what the webhook
missed.

On a provider change "Strava now supports retroactive activities":
admin endpoint `POST /admin/external-accounts/<id>/reset-watermark`
sets `sync_watermark = NULL`, the next reconcile does a full backfill.

### 6.5 Webhook edge cases

The webhook pipeline from Phase 3 is robust against the four common
race conditions / edge cases:

**(a) Webhook for an activity that was never imported.**

Scenario: user connects Strava today, Strava immediately sends a
"create" webhook for an activity the user uploaded 2 weeks
ago. The backfill job is still running.

```
Webhook arriving → server publishes cairn.jobs.fetch_source.strava
  Nats-Msg-Id = "fetch:strava:12345:22222"

Worker fetches, publishes result.
Server ingest handler calls IngestActivityFromWorker.Execute(...):
  - identity lookup (FindSourceByExternalID): not there → ErrNotFound
  - handleCreateNew → new singleton activity
  - ReclusterBucket: no other source in the window → stays standalone

The activity is created **as new**. That is correct — the webhook has
implicitly triggered the import before the backfill got there.

What if the backfill now pulls the same activity in parallel?
  - Backfill worker publishes a second fetch_source.strava
      Nats-Msg-Id = "fetch:strava:12345:22222"        ← same ID!
  - JetStream dedup window (5min) discards the second publish
  - If the window has already expired: the worker fetches twice
    → publishes result, Stage 1 now finds the source → handleReimport
    → idempotent, same end state
```

No special handling needed.

**(b) Webhook update for an activity that was never imported.**

Scenario: Strava lost our "create" webhook or we were
offline. Only the subsequent "update" gets through.

Identical path to (a). Stage 1 misses, Stage 2 misses, fall-through
to handleCreateNew. The `aspect_type=update` detail is only
relevant in the server handler for the dedup-key choice; the consequence for
the pipeline is the same as a create.

**(c) Webhook delete for a source we don't know.**

Scenario: Strava sends "delete" for activity 33333, which we never
imported (missed create, or activity was deleted on the
Strava side before we connected the user).

```
Webhook (delete) → server publishes cairn.events.source.deleted_upstream
  body: { account_id, ext_id: "33333" }

Subscriber:
  - SELECT id FROM activity_sources
      WHERE provider='strava' AND external_account_id=$1 AND external_id='33333'
  - 0 rows → no-op, ACK
  - (optional: log as "phantom delete" — not alarming, harmless)
```

The subscriber must treat "source not found" as a success case, not
as an error. Otherwise we end up in a retry loop and eventually the DLQ without
a real problem.

**(d) Multiple "update" webhooks in rapid succession.**

Scenario: user edits title, then type, then description of an
activity within 30 seconds. Strava sends three update webhooks.

```
Webhook #1 → publish cairn.jobs.fetch_source.strava
  Nats-Msg-Id = "fetch:strava:12345:22222"
JetStream accepts.

Webhook #2 (15s later) → publish same subject
  Nats-Msg-Id = "fetch:strava:12345:22222"           ← SAME
JetStream dedup window (5min) discards. No second processing.

Webhook #3 (30s later) → same. Discarded.
```

The worker fetches **once**, automatically gets all three edits in one
fetch (the Strava API returns the current state, not a diff).
`handleReimport` replaces the payload with the consolidated state.

If the three updates are spread over more than 5 min: the
dedup window expires, each update produces a fetch → three
reimports in succession. Idempotent, albeit unnecessarily API-expensive.
In practice Strava sends updates within seconds; the window
catches that.

**(e) Webhook arrives while the worker fetch of the same job is still
running.**

Scenario: webhook #1 is being processed by the worker right now (API call in
flight). Webhook #2 for the same activity comes in.

```
Worker has job #1 in flight, has not yet sent ACK.
Server publishes job #2 with the same Nats-Msg-Id → JetStream discards
(dedup window).
Worker finishes job #1, ACK.
```

Clean. Even if worker #1 dies before ACK: AckWait timeout →
redelivery → another worker redoes the same job. Worst case
one extra API-quota cost, no data corruption.

**Summary of the edge-case policy:**

- The webhook path is self-healing via Stage-1/2 dedup + JetStream
  dedup window
- The reconcile path is the safety net for everything the webhook loses
- "Source not found" is never an error in the subscriber, always a no-op
- Idempotency is a contract, not a heuristic — every step of the pipeline
  is re-runnable

---

## 7. Worker lifecycle

**Start-up:**

```
Worker → JS KV PUT cairn_worker_manifests/<name>
       → JS KV PUT cairn_worker_presence/<name>.<inst>  {last_seen: now}
       → JS Pull consumer subscribe on cairn.jobs.<provider>.<job_type>
                          in queue group "cairn-workers-<provider>"
```

A KV-PUT on `cairn_worker_manifests/<name>` triggers a server-side
watch — on a manifest-hash change `MarkSourcesOutOfDateForWorker` runs
(see CLAUDE.md Gap 1). Worker version detection is thereby async and
durable, without a server DB poll.

**Heartbeat:** every 10 s `JS KV PUT cairn_worker_presence/<name>.<inst>`
with `last_seen=now`. A 60 s TTL tolerates two lost heartbeats; then
the worker disappears from the presence map. The operator UI queries the map
directly, not the `workers` DB table.

A periodic reconciler (every 30 s) syncs KV → Postgres
`workers.last_heartbeat_at` for historical analysis, but at
runtime the KV is the source of truth.

**Shutdown:**

```
Worker → drain JS consumer (finish processing pendings)
       → JS KV DELETE cairn_worker_presence/<name>.<inst>
       → close NATS connection
```

Crash exit: the TTL does the cleanup work after 60 s.

---

## 8. Async follow-ups via domain events

`internal/usecase/pipeline` is the write path: `IngestSource` (persist →
re-cluster → follow the source into its surviving activity), `Remerge`
(after detach, delete, field pins, account removal, Connect edits),
`BorrowFrom` (source-from, one transaction) and `RecomputeDerived`.
`cmd/server/result_router.go` only decodes the wire format and does the
import bookkeeping around `IngestSource`.

After the durable write, the pipeline does not run the follow-ups itself;
it dispatches a `pipeline.ActivityChanged` event:

| Field | Meaning |
|---|---|
| `kind` | `ingested` / `remerged` / `borrowed` |
| `user_id`, `activity_id` | owner and the activity the follow-ups address (for `ingested`: the surviving activity after re-clustering) |
| `source_id` | the written source (`ingested`, `borrowed`) |
| `action` | `ingested` only; `merged_into_existing` → no created event, no federation |
| `soft_deleted` | `remerged` only; the merge left no source → federation delete |
| `msg_id` | `activity-changed:<kind>:<activity>:<uuid>`, also the `Nats-Msg-Id` |

**With NATS wired**, the event is published to
`cairn.events.activity.changed` (stream `CAIRN_EVENTS`) and the durable
push consumer `activity-follow-ups` (`cmd/server/followup_consumer.go`,
`DeliverAll`) calls `Pipeline.RunFollowUps`:

```
ingested   stream aggregates, best efforts (+ live new-PR events), segment match
           → training load → PR dispatch → federation Create
borrowed   stream aggregates, best efforts, segment match → training load
remerged   federation Delete (if soft-deleted) → training load
```

Every stage logs and recovers (panics included); the joined stage errors
NAK the event and the bus redelivers with back-off (`retryDelay`), until
`MaxDeliver` sends it to `CAIRN_DLQ`. A malformed payload is terminated.
The stages are idempotent (section 3), so a redelivery re-runs all of them;
the federation announcements are not, so they run on the first delivery
only (`Redelivery` from `DeliveryAttempt > 1`). The 48 h federation
recency gate (`FederationMaxAge`) is evaluated when the event is handled.
An activity deleted meanwhile acks the event without work.

**Without NATS** (tests, API-only instances) or when the publish fails,
`RunFollowUps` runs inline before the call returns, so callers and the
integration tests stay deterministic.

Neither path is bound to the caller: dispatch uses
`context.WithoutCancel`, and `RunFollowUps` is bounded by
`Pipeline.FollowUpTimeout` (default 10 min). A client disconnect can no
longer cancel the follow-ups of a committed write, and with NATS the HTTP
response (upload, manual entry, source-from, edits) and the worker-result
ack no longer wait for them.

`RecomputeDerived` (admin "recompute derived data") stays synchronous: it
reports per-stage warnings to its caller.

---

## 9. Blob storage: S3 vs NATS Object Store

Two storage backends with clearly separated tasks:

| Use case | Storage |
|---|---|
| Original FIT/GPX/TCX/JSON files from the provider | **S3** (bucket `cairn-data`, prefix `raw/`) |
| User bulk-export archives | **S3** (prefix `exports/`, TTL 7 days) |
| Map thumbnails, avatars (future) | **S3** (prefix `thumbs/`) |
| Oversized NATS result payloads | **NATS OS** (bucket `cairn_transient`, TTL 1 h) |
| Worker manifest history, OAuth token cache in worker | NATS KV |

**S3 for originals**, because:

- **Direct browser downloads via presigned URLs.** Killer feature for
  a self-hosted tracker: user clicks "Download original FIT" → the stream runs
  browser ↔ S3 directly, no proxy through the API server. With 30 MB FIT files
  the proxy path is a no-go.
- **Lifecycle policies.** "Originals older than 5 years to Glacier."
  "Delete raw blobs of soft-deleted activities after 30 days."
  S3 standard, in MinIO a few lines of YAML.
- **Size class.** Multipart uploads up to 5 TB. Strava FIT < 50 MB,
  but bulk exports can be triple-digit MB.
- **Ecosystem.** `mc` CLI, `rclone`, cross-region replication.

**NATS OS NOT for originals**, because:

- No native presigned-URL concept. Browser downloads would have to be streamed through
  the API server → performance death.
- Stored in JetStream file storage = same volume as streams.
  Pollutes the disk budget.
- Comparatively young (NATS 2.9+), small ecosystem.

**NATS OS IS good for** transient-oversized result messages — workers
spill payloads that exceed the `max_payload` limit into the KV-OS,
the result message only contains the object handle. Not storage, but
transport spillover.

### S3 layout

```
cairn-data/
├── raw/                                  Original files (long retention)
│   └── <provider>/<account_id>/<yyyy>/<mm>/<source_id>.<ext>
│       e.g. raw/strava/84f3.../2026/04/0190a1...bundle.tar
├── exports/                              User-initiated bulk exports
│   └── <user_id>/<export_id>.zip        (TTL 7 days via lifecycle rule)
└── thumbs/                               Future: map snapshots, avatars
    └── ...
```

**Convention** `raw/<provider>/<account_id>/<year>/<month>/<source_id>.<ext>`:

- Provider separation allows per-provider quotas / lifecycle / permissions
- Account separation allows GDPR delete of a provider account without
  affecting others: `mc rm --recursive cairn-data/raw/strava/84f3.../`
- Year/month ensures clean listings and date-based cold-storage
  migration
- `source_id` as UUID-v7 instead of provider-ext-id, because source identity
  stays unique even if the provider hypothetically reassigns the ext-id

**Content-Type** preserved in DB (`activity_sources.raw_content_type`):

| Provider | File | Content-Type |
|---|---|---|
| strava | activity+streams bundle | `application/x-tar` |
| garmin | FIT | `application/vnd.ant.fit` |
| polar | TCX | `application/vnd.garmin.tcx+xml` |
| manual-upload | GPX | `application/gpx+xml` |
| manual-upload | FIT | `application/vnd.ant.fit` |

**Encryption + ACL:**

- SSE-S3 server-side encryption as default (MinIO via `MINIO_KMS_KES_*`)
- Bucket policy: no public access; all access via presigned URLs
- The server has a master service account with full bucket rights
- Workers have **zero** S3 access — only presigned URLs from the server

### Upload path (worker → S3)

```
Worker (after Strava API call):
  - has raw_bytes (e.g. tarball from activity.json + streams.json)
  - computes content_sha256 = sha256(raw_bytes)

Worker → request cairn.blobs.presign_upload.strava
  body: {
    source_id:      "0190a2-...",
    user_id:        "abc1-...",
    content_type:   "application/x-tar",
    content_length: 142387,
    content_sha256: "9f3a1b2c..."
  }
  timeout: 3s

Server handler (subscriber on cairn.blobs.presign_upload.>):
  - parse provider from the subject suffix
  - build s3-key: raw/strava/<account_id>/2026/04/<source_id>.bundle.tar
  - generate presigned PUT URL (TTL 5 min, conditions:
      Content-Length-Range: ±10% of the given value,
      Content-Type: == content_type,
      x-amz-content-sha256: == content_sha256
    )
  - reply: {
      method: "PUT",
      url: "https://s3.cairn.example/cairn-data/raw/strava/.../...tar?X-Amz-...",
      blob_id: "raw/strava/84f3.../2026/04/0190a2-....bundle.tar",
      expires_at: ...,
      required_headers: { ... }
    }

Worker → PUT to S3 directly:
  PUT <presigned_url>
  Headers: content-type, x-amz-content-sha256
  Body:    raw_bytes
  ← 200 OK

Worker → publish cairn.results.fetch_source.strava
  body: {
    ...,
    ingest_input: {
      ...,
      raw_blob_id:      "raw/strava/.../0190a2-....bundle.tar",
      raw_content_type: "application/x-tar",
      raw_size_bytes:   142387
    }
  }
```

### Download path (user → browser → S3)

```
Frontend: GET /api/activities/<id>/sources/<source_id>/raw-download
          Authorization: Bearer <user-jwt>

API server:
  - load source from DB
  - check perms: source.user_id == auth.user_id (or admin)
  - if raw_blob_id == "": 404 "no original file"
  - generate presigned GET URL:
      key:    source.raw_blob_id
      expiry: 5 min
      response-content-disposition: attachment; filename="<friendly>.tar"
      response-content-type: source.raw_content_type
  - return { url, expires_at, filename }

Frontend:
  - <a href={url} download>Download</a>
  - or window.location = url
```

Direct browser stream to S3, the API server sees no byte of the file.

### Reparse path (server → worker reads S3 blob)

When the worker manifest hash changes (CLAUDE.md Gap 1+2): the server
re-parses all sources from S3 without renewed provider-API quota.

```
Server (RequestSourceReimport, variant "from_blob"):
  - load source from DB
  - if raw_blob_id == "": fallback "from_provider" (old path with OAuth)
  - generate presigned GET URL (TTL 1h)
  - publish cairn.jobs.parse_blob.strava
      headers:
        Nats-Msg-Id    = "parse:strava:<source_id>:<target_manifest_hash>"
        Cairn-Reply-To = cairn.results.fetch_source.strava
                         ← same reply subject as from-provider-fetch
      body: {
        job_id, source_id, user_id, provider: "strava",
        blob: {
          url: "<presigned>",
          expires_at: ...,
          fallback_handle: "h_8af2..."    ← on retry-after-1h
        },
        target_manifest_hash: "sha256:..."
      }

Worker (cairn-parser-strava — or same binary with a second subscriber):
  - check blob.url expiry
  - if expired: request cairn.blobs.presign_download.strava
      body:  { handle: "h_8af2..." }
      reply: { url: <fresh-presigned>, expires_at: ... }
  - GET blob.url
  - parse with the NEW worker logic
  - publish cairn.results.fetch_source.strava (same form as backfill!)
```

Elegance: `parse_blob` and `fetch_source` flow into the same result subject
with the same payload schema. The server ingest handler makes no
distinction — Stage-1 dedup finds the existing source,
`handleReimport` replaces the payload. Multi-source merge as usual.

`fallback_handle` is a server-signed HMAC token, valid 7 days, exchangeable against
the S3 key + permission check. Needed for jobs that are retried in DLQ recovery
many hours later, when the original presigned URL
has long since expired.

### Result payloads via S3 claim-check (supersedes the NATS-OS spillover)

> **Decision 2026-07-10:** event-carrying JobResults ALWAYS travel as an
> S3 claim-check, not just when oversized, and the transfer store is S3
> (same bucket, `transfer/` prefix) rather than NATS Object Store. One
> deterministic code path; no "works for short runs, breaks for long
> rides" behavior split; NATS stays a pure control plane. The original
> NATS-OS spillover design was never wired into the result path and is
> retired. Prod motivation: results larger than the server's 1 MiB
> `max_payload` failed to publish, the worker Term'd them, and the stale
> reaper masked the cause ("stale: no result after 5 dispatch attempts").

```
Worker (any handler producing events):
  - build the full protojson JobResult (events, streams, watermark)
  - request cairn.blobs.presign_upload.<provider> with kind: "result"
      → server keys it transfer/<provider>/<uuid>.json
  - HTTP PUT the body to the presigned URL
  - publish the ENVELOPE JobResult (tiny): worker stamp + watermark +
    payload_ref { blob_id, size_bytes, content_type, content_sha256 }

Server result router:
  - payload_ref set → BlobStore.Get(blob_id) → decode full JobResult
      → process events → BlobStore.Delete(blob_id)   ← after success only
  - Get → not found: TerminalError payload_missing (expired or duplicate)
  - processing error → NAK; the object stays for the redelivery
  - results WITHOUT events (reconcile watermark, backfill) stay inline
```

Terminal worker failures publish a small failure envelope instead of
nothing: `JobResult { error: WorkerError, failed_ref: ExternalRef }` —
the router fails the import-queue item immediately with the true reason
instead of letting the stale reaper mask it.

Cleanup is two-layered: delete-on-ingest (above) plus a bucket lifecycle
rule on `transfer/` (`CAIRN_STORAGE_TRANSFER_EXPIRY_DAYS`, default 1 day)
as the orphan backstop — objects whose envelope was never processed
(worker crashed between PUT and publish, core down past redelivery).

### Lifecycle and GDPR

**Activity delete:**
- soft-delete `activities.deleted_at`
- raw blobs remain (user might want to undo)
- Durable consumer on `cairn.events.activity.soft_deleted` with delay
  logic clears the `raw_blob_id` files after retention (e.g. 30 days)

**Provider-account delete (Strava disconnect + "delete all data"):**

```
Use-case PurgeExternalAccount(account_id):
  - SELECT all source.raw_blob_id of the account's sources
  - BlobStore.Delete for each
  - DELETE FROM activity_sources WHERE external_account_id = ...
  - Orphaned activities (0 non-detached sources) → soft-delete cascade
  - DELETE FROM external_accounts WHERE id = ...
```

Lives under `/admin/external-accounts/<id>/purge` or as user self-service.

**Full-account delete:** sequentially `PurgeExternalAccount` per account
of the user, then `DELETE FROM users`. As an async job over NATS, because with
5000 activities it is not feasible in 30 s.

---

## 10. The `port.BlobStore` interface

```go
// port/blob_store.go

package port

import (
    "context"
    "time"
)

type BlobStore interface {
    // PresignUpload generates a one-shot URL the worker can PUT to.
    // Conditions in opts constrain content-type, length, sha256.
    PresignUpload(ctx context.Context, key string, opts PresignUploadOpts) (PresignedURL, error)

    // PresignDownload generates a one-shot URL for direct download.
    // Used both for user-facing download and for worker reparse fetches.
    PresignDownload(ctx context.Context, key string, opts PresignDownloadOpts) (PresignedURL, error)

    // Stat returns size + content-type without downloading.
    Stat(ctx context.Context, key string) (BlobMeta, error)

    // Delete removes a blob. Used by DSGVO-delete and lifecycle cleanup
    // for soft-deleted activities older than retention.
    Delete(ctx context.Context, key string) error
}

type PresignUploadOpts struct {
    ContentType      string
    ContentLengthMin int64
    ContentLengthMax int64
    ContentSHA256    string // optional, S3 enforces if set
    Expiry           time.Duration
}

type PresignDownloadOpts struct {
    Expiry                     time.Duration
    ResponseContentDisposition string // "attachment; filename=..."
    ResponseContentType        string // override the stored content-type
}

type PresignedURL struct {
    URL             string
    Method          string // "GET" | "PUT"
    RequiredHeaders map[string]string
    ExpiresAt       time.Time
}

type BlobMeta struct {
    Key         string
    SizeBytes   int64
    ContentType string
    ETag        string
    UpdatedAt   time.Time
}
```

Adapter under `internal/adapter/secondary/s3/blob_store.go` with
`aws-sdk-go-v2`. MinIO-compatible via custom endpoint
(`AWS_ENDPOINT_URL_S3=http://minio:9000`). The self-hosted operator sets
endpoint, access key, secret via ENV; the code does not distinguish S3 and MinIO.

---

## 11. Error-handling matrix

### Worker-side: API calls to the provider

| Provider response | Worker reaction | NATS consequence |
|---|---|---|
| **200 OK** | parse + result publish | `Ack()` |
| **429 Too Many Requests** | read `Retry-After`, set KV counter hard to max | `NakWithDelay(retry_after)` |
| **401 Unauthorized** | token was invalid despite refresh | publish `cairn.events.external_account.needs_reauth`, `Term()` (permanent) |
| **403 Forbidden** | scope missing | publish `cairn.events.fetch_source.skipped {reason:"forbidden"}`, `Ack()` |
| **404 Not Found** | activity deleted on the provider side | publish `cairn.events.source.deleted_upstream`, `Ack()` |
| **5xx Server Error** | provider down | `NakWithDelay(60s * 2^delivery_attempt)` exp. backoff |
| **Network timeout** | like 5xx | `NakWithDelay(...)` |
| **Parse error** (200 but malformed) | suspicious | `Nak()` once, then `Term()` → DLQ + alert |

`Term()` vs `Nak()`: `Term` is "permanent error, never retry" —
the message goes out, goes into DLQ via advisory. `Nak` is "retry"; on
reaching `MaxDeliver` automatically DLQ.

### Server-side: token-refresh handler

| Scenario | Server reaction |
|---|---|
| Token still valid | reply with cached/DB token |
| Refresh successful | UPDATE DB, reply with new token |
| **Strava 401 on refresh** (user disconnected in the provider app) | UPDATE `external_accounts.status='needs_reauth'`, reply `{error:"needs_reauth"}`, publish `cairn.events.external_account.needs_reauth` |
| Network error to the provider | reply `{error:"transient", retry_after:30}` — worker `NakWithDelay(30s)` |
| Account not found (race: user disconnected while the job was running) | reply `{error:"account_gone"}` — worker `Term()` |

The worker reads the token reply as control information: on `needs_reauth`
or `account_gone` → `Term()`, further retries pointless.

### Server-side: ingest handler on result

| Scenario | Reaction |
|---|---|
| `IngestActivityFromWorker.Execute()` success | re-cluster, follow-ups, `Ack()` |
| Worker reported `ok: false` | log with worker error detail, `Ack()` (worker already retried/term'd) |
| Deterministic DB error (constraint, invalid data) | fail the import-queue item with the reason, `Ack()` |
| Validation error (malformed payload) | `TermWithReason()`; the DLQ listener records it |
| DB connection error / other transient | `NakWithDelay(5s·3^(n-1), max 5 min)`; after `MaxDeliver` → DLQ |
| Handler panic | recovered, treated as transient |
| Re-cluster or follow-up fails or panics | logged, the ingest still `Ack()`s; `recompute_activity_merges` re-clusters as the repair |

Ingest and re-cluster hold a per-user advisory lock (`port.UserLock`,
`pg_advisory_xact_lock`), so two instances never re-cluster one user's
bucket from stale snapshots.

### Blob operations

| Scenario | Reaction |
|---|---|
| S3 not reachable on presign | worker `NakWithDelay(30s)`; operator alert |
| S3 full (507) | worker `NakWithDelay(...)`; operator alert (critical) |
| Presigned URL expired on reparse | worker request `fallback_handle`. If also invalid: `Term()`, DLQ |
| Blob reference in DB but object gone | `Stat` 404 → reparse falls back to the `from_provider` path. Logged warning. |
| Upload size deviates from the given one | S3 rejects with 403 (conditions), worker recomputes, retries once |
| SHA256 mismatch | S3 rejects. `Term()` + DLQ — data corruption on the worker side |

### Dead-letter path

```
NATS publishes $JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.> on reaching MaxDeliver,
and ...MSG_TERMINATED.> on Term (recorded for CAIRN_RESULTS / CAIRN_EVENTS only:
workers term jobs on purpose).

Server (advisory consumer, cmd/server/dlq_listener.go):
  - receive: stream, consumer, stream_seq, deliveries, reason
  - fetch subject/payload/headers by stream_seq while the stream still holds it
  - INSERT INTO dead_lettered_jobs (
        stream, subject, msg_id, payload, last_error, delivered_count,
        first_seen_at, last_seen_at
    )
  - publish cairn.events.job.dead_lettered
      body: { stream, subject, msg_id, payload, deliveries }
  - Operator UI: /admin/dlq shows open entries
  - Operator action: POST /admin/dlq/<id>/replay pushes back into the job stream
    with a fresh Nats-Msg-Id (escape dedup window)
```

### Account-needs-reauth recovery

The only "doesn't work without the user" case:

```
1. Worker 401 from the token refresh → publish cairn.events.external_account.needs_reauth
2. Server (notification-dispatcher subscriber):
   - INSERT notification_events (
        user_id, type=external_account_refresh_failed,
        severity='warn', dedup_key='reauth:<account_id>'
     )
   - Email/Push (once delivery channels are implemented)
3. User UI red badge "Renew Strava connection"
4. User clicks → /oauth/strava/start re-flow
5. Callback UPDATE external_accounts SET ..., status='active'
6. Server publish cairn.events.external_account.reconnected
   → Subscriber re-enqueues all ReimportStatus='failed' sources of the account
```

---

## 12. Multi-server coordination

With two API server instances behind a load balancer:

- **Stateless HTTP** — no problem, both serve everything
- **Webhook reception** — both servers publish into `cairn.jobs.*`,
  the JetStream dedup window over `Nats-Msg-Id` takes care of
  double submissions
- **Result consumer** — both servers have the same durable consumer
  `ingest-router`. Queue-group behavior: only one gets the message.
  Implicit work sharing.
- **KV watches** — both servers react to manifest changes,
  idempotent use cases cause the second reaction to be a no-op

Effectively: multi-server becomes trivial. NATS does the cluster state,
Postgres does the data state, no additional coordination library.
