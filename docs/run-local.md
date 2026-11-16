# Running Cairn locally

Local dev runs the **supporting services in docker compose** and the **Cairn
binaries on the host** (server and web), each with live-reload so a
save rebuilds the artifact. Use separate shells for the server and web.

## 1. Supporting services (compose)

`docker-compose.dev.yml` is dependencies-only — Postgres/TimescaleDB/PostGIS,
NATS (JetStream), MinIO, and Mailpit. Pinned image tags, bound to `127.0.0.1`.

```sh
make dev-up        # docker compose -f docker-compose.dev.yml up -d
make dev-logs      # tail
make dev-down      # stop, keep data   (make dev-down ARGS=-v to wipe volumes)
```

MinIO is built locally from its pinned upstream source because the community
container images are no longer available. The first `make dev-up` needs internet
access and takes longer to compile it; subsequent starts reuse the local image.
For an explicit rebuild, run `docker compose -f docker-compose.dev.yml build minio`.

| Service | Host port | Notes |
| --- | --- | --- |
| Postgres | `localhost:5432` | `cairn` / `cairn` / db `cairn` |
| NATS | `localhost:4222` (mon `:8222`) | JetStream; `/varz`, `/jsz` |
| MinIO | `localhost:9000` (console `:9001`) | `cairn` / `cairn-secret` |
| Mailpit | SMTP `localhost:1025` (UI `:8025`) | read notification emails |

## 2. Env

```sh
cp dev.env.example dev.env     # once; matches the compose services + dev secrets
```

`make dev-server` / `make dev-web` source `dev.env` automatically. It sets
`CAIRN_DATABASE_URL`, `CAIRN_NATS_URL`, the `CAIRN_STORAGE_*` (MinIO), the
`CAIRN_EMAIL_SMTP_*` (Mailpit), throwaway auth secrets, and the worker identity.
`CAIRN_DATABASE_AUTO_MIGRATE=true` applies migrations on boot.

## 3. The binaries (host, live-reload)

Install Go (see `go.mod`), Node.js and npm first. The Node version used by the
project is recorded in `web/.nvmrc`. On first setup, install the protobuf
generators in user-writable directories; no global npm install is required.
`make dev-web` installs web dependencies and generates the TypeScript protobuf
files when they are missing.

```sh
make proto-tools
npm install --prefix "$HOME/.local/share/cairn-proto" @bufbuild/protoc-gen-es@2.12.0
go_bin="$(go env GOBIN)"
export PATH="${go_bin:-$(go env GOPATH)/bin}:$HOME/.local/share/cairn-proto/node_modules/.bin:$PATH"
```

Run the `go_bin` and `export PATH` lines in each shell that runs `make dev-web`,
or add the equivalent directories to your shell configuration.

```sh
make dev-server    # shell 2: cmd/server   — rebuilds + restarts on save (air)
make dev-web       # shell 3: web/ vite dev server (HMR)
```

- **API**: http://localhost:8080 (`/healthz`, `/readyz`, `/metrics`, `/api/*`,
  `/cairn.v1.*`).
- **Web**: http://localhost:5173 (or the next free port — vite prints it). The
  vite dev server proxies `/api`, `/auth`, `/webhooks`, `/cairn.v1.*`,
  `/cairn.worker.v1.*` to the Go server on :8080.
- Sign in on `/login`. Bootstrap an admin via env on first boot
  (`CAIRN_BOOTSTRAP_ADMIN_*`) or create one with an invite.

`air` is fetched on first run at the version pinned in the Makefile — no
separate install. Its build output lives in `tmp/` (gitignored). Prefer no
live-reload? `make run` (`go run ./cmd/server serve`) and `cd web && npm run dev`
still work.

## The worker / provider plugin

The Strava worker imports activities from Strava and is maintained in a separate
provider repository. Against the dev NATS (no auth-callout), it connects
**without** an enrollment token, registers presence + heartbeats,
and subscribes to `cairn.jobs.*.strava`. It sits idle until a real fetch job
arrives — that needs genuine Strava OAuth credentials and a linked account. To
exercise the ingest → merge → best-effort → segment → training-load pipeline
**without** Strava, upload a FIT/GPX/TCX file (no worker involved).

Adding another provider is a new `cmd/worker-<provider>/` with its own auth
handler + handlers — no core change.

### Production worker auth

The dev path has no per-worker permission scoping. For a real deployment,
configure NATS auth-callout, mint an enrollment token (`POST
/api/admin/worker-enrollments`), and set `CAIRN_WORKER_ENROLLMENT_TOKEN` on the
worker — the code takes the auth-callout path automatically when the token is
present. See `docs/architecture.md` §4.5.

## Dev vs prod topology

The frontend is a **client-rendered SPA** (`ssr=false`), so dev and prod render
identically — only the packaging differs:

| | Dev (host binaries) | Prod (`docker/Dockerfile.core`) |
| --- | --- | --- |
| Frontend | vite dev server (HMR), proxies to the API | static SPA **embedded in the Go binary** |
| Routing | vite proxies API paths to :8080 | the one binary serves `/` + `/api` + `/cairn.v1.*` |
| Processes | server + worker + vite on the host | **one** distroless container (+ a worker container) + deps |

## Production image (single distroless binary)

```sh
make proto                                  # regenerate gen/ (not committed)
docker build -f docker/Dockerfile.core -t cairn .
```

Three stages: build the SvelteKit static SPA (`npm run build:static`,
adapter-static) → compile the Go server with `-tags embedweb`, embedding the SPA
from `internal/adapter/primary/web/dist` → **`gcr.io/distroless/static-debian12:nonroot`**
runtime (nonroot uid 65532, no shell, ca-certs + tzdata, ~56 MB). The resulting
`cairn serve` answers both `/cairn.v1.*` and `/` (the embedded UI with
client-side-routing fallback). Point it at Postgres/NATS/MinIO via the same
`CAIRN_*` env; no Node, no Caddy, no separate web deployment.

This is exactly what the Gitea **build-core** workflow builds and pushes
(`johnny/cairn-core` → `.../cairn`). The provider workers live in their own
repos and publish their own images (`.../cairn-provider-strava`,
`.../cairn-provider-garmin`). `Dockerfile.core` uses the shared **build-base**
toolchain image as its build stage and runs `make proto` in-image (offline,
local buf plugins) — no separate proto step.

## Secrets

`dev.env` holds throwaway dev secrets so the stack runs with zero setup. **Never
ship them.** For production generate real values (`cairn gen-secrets`), inject as
env, and switch the worker to the enrollment-token path — see
`docs/production-readiness.md`.
