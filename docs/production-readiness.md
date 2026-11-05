# Production readiness

A candid assessment of what's solid, what's missing, and the runbook for a
real deployment. Last reviewed 2026-06-05.

## Verdict

Cairn is **feature-complete and usable as a self-hosted, multi-user tracker**:
import → merge → analysis → web UI all work end-to-end and have been exercised
against a real ~280-activity import. The code is in good shape —
hexagonal boundaries intact, unit + targeted integration tests in CI, dev
surfaces removed from the binary.

It is **not yet a safe public 1.0**. The gaps below are mostly operational and
validation, not architectural.

## Pre-flight checklist

- [ ] **Secrets** — `cairn gen-secrets > cairn.secrets.env`, store in a secrets
      manager, inject as env. Never commit. The dev compose's fixed
      `dev-only-*` secrets must NOT ship.
  - `CAIRN_AUTH_SESSION_SECRET`, `CAIRN_AUTH_MASTER_ENCRYPTION_KEY` (the master
    key encrypts all secrets-at-rest: OAuth tokens, webhook signing secrets,
    NATS account key — losing it means re-entering them all). OIDC client
    secrets are NOT stored: they come from `CAIRN_OIDC_*` env at runtime.
- [ ] **No dev admin surface** — the legacy token-gated `/admin/*` smoketest
      layer has been **removed from the binary**. Operator actions live on the
      admin-session-gated `/api/admin/*` surface (NATS key bootstrap/rotate,
      DLQ, geocode backfill, worker onboarding, invites). Nothing to disable.
- [ ] **TLS / proxy** — terminate TLS at a reverse proxy; set
      `CAIRN_HTTP_PUBLIC_BASE_URL` to the public https origin (drives OIDC
      redirect URIs, webhook callback URLs, password-reset + share links).
      Confirm `X-Forwarded-*` is set and `CAIRN_INSTANCE_TRUSTED_PROXIES` lists
      the proxy so per-IP rate limiting keys on the real client.
- [ ] **CSP** — a strict Content-Security-Policy belongs at the proxy (a wrong
      one silently breaks the SPA). The Go server + Caddy set the baseline
      security headers (`nosniff`, `DENY`, HSTS-when-TLS, `frame-ancestors`).
- [ ] **Migrations** — see below. Decide auto vs. explicit.
- [ ] **Backups** — see below. Verify a restore before you need one.
- [ ] **Email** — point `CAIRN_EMAIL_SMTP_*` at a real relay
      (`STARTTLS=true` + `SMTP_USERNAME/PASSWORD`), not the dev Mailpit catcher.
- [ ] **NATS auth-callout** — bootstrap the account key
      (`POST /api/admin/nats/bootstrap-account-key`), run NATS with
      `docker/nats-auth-callout.conf` (issuer = the returned public key), set
      `CAIRN_NATS_AUTH_CALLOUT=true` + `CAIRN_NATS_WORKER_ACCOUNT=CAIRN` on
      core, and onboard workers via enrollment tokens (not the dev anonymous
      path). See `docs/architecture.md` §4.5.

## Migrate-on-deploy

The schema is goose migrations embedded in the binary; the running version is
checked at startup (see `cairn serve` doc: current / older+AUTO_MIGRATE /
older / newer).

- **Single-node**: set `CAIRN_DATABASE_AUTO_MIGRATE=true` — startup applies
  pending migrations, then serves.
- **Multi-replica**: leave AUTO_MIGRATE **off**. Run `cairn migrate up` as a
  discrete pipeline step (a pre-deploy job) before rolling out the new binary,
  so two replicas never race the same migration. The binary refuses to start
  against an older schema, which gates the rollout safely.
- TimescaleDB note: migrations run on a dedicated **simple-protocol** pgx
  connection (continuous-aggregate DDL fails over the default prepared
  protocol) — already handled in `db.go`, but relevant if you run migrations
  out-of-band.

## Backup & restore

Three stateful stores; back up all three consistently.

1. **Postgres / TimescaleDB / PostGIS** (the source of truth — activities,
   streams hypertable + continuous aggregates, segments, metrics, users,
   encrypted secrets).
   - Use `pg_dump`/`pg_restore` or a Timescale-aware physical backup
     (`pg_basebackup` + WAL archiving / `pgBackRest`). The streams hypertable
     is large; a logical dump is slow at scale — prefer physical + PITR for
     anything sizeable.
   - **CAggs**: after a logical restore, the continuous aggregates exist but
     their refresh policies cover only a recent window. Historical buckets are
     materialised on demand by `StreamRepo.RefreshAggregates` after each
     ingest; a bulk backfill is a one-time `refresh_continuous_aggregate` over
     the historical range (see Update 2026-06-04f in the session memory).
2. **Object storage (S3/MinIO)** — archived raw blobs (`raw_blob_id`). Back up
   the bucket. Losing it disables per-source download + quota-free re-parse,
   but activities + streams survive (re-fetch from the provider restores blobs).
3. **NATS JetStream** — job/result/event streams + KV (presence, rate limits,
   manifests). These are **transient/operational**, not source of
   truth — a fresh NATS loses in-flight jobs (re-driven on the next reconcile)
   but no durable data. The one thing to preserve is the **NATS account signing
   key**, which lives encrypted in Postgres (`instance_signing_keys`), so a
   Postgres backup already covers it.

**The master key is the crown jewel.** All at-rest secrets are AES-GCM-encrypted
with `CAIRN_AUTH_MASTER_ENCRYPTION_KEY` (AAD-bound to the row). Back it up
*separately* from the database. Rotate with `cairn rotate-key --new-key=<new>`.

## Monitoring

- **Liveness**: `GET /healthz` (cheap; the container healthcheck).
- **Readiness**: `GET /readyz` — checks Postgres + (when wired) NATS + S3;
  returns 503 + the failing dependency. Route traffic on this.
- **Metrics**: `GET /metrics` (Prometheus). Internal-only — not exposed on the
  public origin. `cairn_http_*` request counters/histograms (path-normalised),
  `go_*`/`process_*`, `cairn_build_info`.
- **DLQ**: jobs that exhaust redelivery are captured in `dead_lettered_jobs`;
  inspect/replay via `/api/admin/dlq`. Alert on a growing DLQ.

## Known gaps (before a public 1.0)

| Gap | Status |
| --- | --- |
| **Live-provider validation** | The Strava worker's HTTP paths are unit-tested (httptest), but the full round-trip against the real Strava API — incl. its real rate limits — needs an operator session. This is the single biggest "verify in prod" item. |
| **Rate-limiter under load** | The KV CAS thundering-herd livelock that bit us in dev is **fixed** (jittered backoff + soft-deny, #80) and tested at 300 concurrent reservers. |
| **Notification retry queue** | Email/webhook sends are best-effort (fire-and-forget) with a per-event delivery audit. No automated retry yet (#78 deferred; `notification_deliveries.next_retry_at` column exists). |
| **`worker_offline` alerts** | The notification type exists but nothing creates it (needs a presence-staleness watcher + admin-broadcast recipient model). |
| **HTTP-level integration tests** | Repo-layer integration tests run in CI (real Postgres); there's no automated test that boots the server and exercises the REST surface end-to-end. |
| **Webhook account disambiguation** | When the same provider athlete is linked by two users/connections, the webhook owner-lookup can be ambiguous (#50) — needs a subscription→connection table + live Strava to build/verify. |
| **Social / multi-user** | **Shipped.** Per-field visibility, follow graph + feed, public profiles, share links, kudos + comments, clubs, moderation, quotas, delegated-admin roles (#79, #83–92). **Federation / ActivityPub shipped** (Phases 1–5, off by default — `CAIRN_FEDERATION_ENABLED` + per-user opt-in; see `docs/federation-design.md`). |

## What's deliberately out of scope

Cairn now ships a full **multi-user social layer** (#79 epic) — per-field
visibility, follow graph + feed, public profiles, share links, kudos + comments,
clubs, moderation, quotas, delegated-admin roles — and **ActivityPub federation**
(off by default). Still out of scope: hard multi-tenant isolation beyond
per-user data scoping (no per-tenant resource limits / billing), and the v2/v3
federation polish in `docs/federation-design.md` §12.6 (shared-inbox fan-out,
`Move`/account migration, key-rotation UI).
