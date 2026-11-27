# Cairn on Kubernetes

A self-contained kustomize bundle: the core server (API + embedded SPA), the
**Strava** and **Garmin** workers, and the backing services (Postgres/
TimescaleDB/PostGIS, NATS/JetStream, MinIO).

```
namespace.yaml        cairn namespace
config.yaml           non-secret config (ConfigMap cairn-config)
secrets.example.yaml  → copy to secrets.yaml, fill in, do NOT commit
postgres.yaml         TimescaleDB StatefulSet + headless Service + 20Gi PVC
nats.yaml             NATS+JetStream StatefulSet (max_payload 8MB) + 5Gi PVC
minio.yaml            Silo (MinIO fork) StatefulSet + 20Gi PVC (server creates the bucket)
migrate-job.yaml      one-shot `cairn migrate up`
core.yaml             cairn-core Deployment + Service (:80 → :8080)
worker-strava.yaml    Strava worker Deployment
worker-garmin.yaml    Garmin worker Deployment (stateless; emptyDir token cache)
ingress.yaml          TLS entry point (nginx + cert-manager example)
```

## Deploy

1. **Secrets.** Copy and fill:
   ```sh
   cp secrets.example.yaml secrets.yaml
   # generate the two crypto secrets:
   openssl rand -base64 32   # → CAIRN_AUTH_SESSION_SECRET
   openssl rand -base64 32   # → CAIRN_AUTH_MASTER_ENCRYPTION_KEY
   ```
   Keep the DB password identical in `CAIRN_DATABASE_URL` and
   `POSTGRES_PASSWORD`, and the storage creds identical to `MINIO_ROOT_*`.

2. **Host.** Set `CAIRN_HTTP_PUBLIC_BASE_URL` in `config.yaml` and the host in
   `ingress.yaml` to your real domain (they must match — it drives OIDC
   redirects, webhook callbacks, and share/reset links).

3. **Apply.** Secrets first (not in the kustomization), then the bundle:
   ```sh
   kubectl apply -f secrets.yaml
   kubectl apply -k .
   ```
   The `cairn-migrate` Job runs `migrate up` before the server serves traffic
   (the core refuses to start against an older schema). Re-running it is a
   no-op; after a version bump, delete and re-apply the Job.

4. **Verify.**
   ```sh
   kubectl -n cairn get pods
   kubectl -n cairn logs job/cairn-migrate
   kubectl -n cairn rollout status deploy/cairn-core
   ```
   Log in at your Ingress host as the bootstrap admin
   (`CAIRN_INSTANCE_BOOTSTRAP_ADMIN_EMAIL` / `..._PASSWORD`).

## How the pieces talk

- Workers reach the core **only** through NATS (`nats:4222`) — job/result bus +
  blob presign request/reply. They do not need DB or S3 credentials.
- Both workers are **stateless** — they carry no provider credentials. Strava
  OAuth client id/secret are per-user ("bring your own Strava app", entered in
  the Connections UI); Garmin credentials are per-user too and the worker
  fetches them fresh from core over NATS per account. Set
  `CAIRN_STRAVA_WEBHOOK_VERIFY_TOKEN` only if you register a Strava push
  subscription.
- The Garmin worker's `CAIRN_GARTH_HOME` is an **emptyDir token cache** — a
  per-pod optimisation that lets a warm pod skip re-login between jobs. It is
  not durable state; a restarted pod just re-authenticates from the fetched
  credentials.

## Scaling

- **core**, **worker-strava**, and **worker-garmin** are all stateless — raise
  `replicas` freely (JetStream pull consumers share the work across replicas).
  One exception: the Strava **webhook owner** is single-instance. If
  `CAIRN_STRAVA_WEBHOOK_VERIFY_TOKEN` is set, keep that Deployment at
  `replicas: 1`; to scale Strava, run the pull workers token-unset and a
  separate single-replica Deployment as the webhook owner.
- **Postgres/NATS/MinIO** here are single-instance. For HA, swap in a Postgres
  operator (e.g. CloudNativePG), a NATS cluster (Helm chart), and distributed
  MinIO or a managed S3 bucket — point the same env at them.

## Hardening (before internet exposure)

- **NATS auth.** This bundle runs NATS unauthenticated on the cluster network
  (matches the dev path: workers connect anonymously). For untrusted networks,
  enable enrollment-token **auth-callout**:
  1. bootstrap the account key (`POST /api/admin/nats/bootstrap-account-key`);
  2. replace `nats.conf` in `nats.yaml` with `docker/nats-auth-callout.conf`
     and give the NATS pod `CAIRN_NATS_AUTH_ISSUER` (the returned public key)
     and `CAIRN_NATS_PASSWORD`;
  3. set `CAIRN_NATS_USERNAME=cairn-core`, `CAIRN_NATS_PASSWORD`,
     `CAIRN_NATS_AUTH_CALLOUT=true` and `CAIRN_NATS_WORKER_ACCOUNT=CAIRN` on
     core;
  4. create an enrollment per worker (name + provider) and set
     `CAIRN_WORKER_ENROLLMENT_TOKEN` on the Strava worker. The Garmin worker
     has no enrollment support; give it a static user (see the comments in
     `docker/nats-auth-callout.conf`).
  See `docs/architecture.md` §4.5.
- **Secrets management.** `secrets.example.yaml` uses plain `stringData` for
  readability — use SealedSecrets / External Secrets / SOPS for real clusters.
  Back up `CAIRN_AUTH_MASTER_ENCRYPTION_KEY` separately from the database; it
  encrypts every at-rest credential.
- **TLS + CSP.** Terminate TLS at the Ingress and add a strict
  Content-Security-Policy there (a wrong one silently breaks the SPA, so it is
  not baked into the server).
- **Backups.** Postgres is the source of truth (incl. the encrypted NATS
  account key); MinIO holds re-fetchable raw blobs; NATS JetStream is
  transient. See `docs/production-readiness.md`.

> NATS, Postgres and Silo here are the same pinned images as
> `docker-compose.dev.yml`. Treat the single-instance backing services as a
> starting point sized for a small self-hosted instance, not a turnkey HA
> production data tier.
