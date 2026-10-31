# 2. Hexagonal architecture: domain, port, use case, adapter

Status: accepted

## Context

Cairn merges activities from several providers and stores them in Postgres, NATS and S3. The merge
engine and visibility rules must be testable without that infrastructure.

## Decision

- `internal/domain` is pure: it imports nothing from port, usecase or adapter.
- `internal/port` holds driven-port interfaces and imports only domain.
- `internal/usecase` orchestrates domain and ports and never imports adapters.
- Primary adapters (`internal/adapter/primary/{connect,rest,activitypub,mcp,workerrpc,web}`) decode a
  protocol and call use cases; they never import a secondary adapter. HTTP handlers live in `rest`, not
  in `cmd/server`.
- Secondary adapters (`internal/adapter/secondary/{postgres,nats,s3,inmem,email,geocode,activitypub,
  provideroauth}`) implement ports.
- `cmd/server` is the composition root: `wire.go` constructs adapters and use cases, the `mount_*.go`
  files hand handler groups their use cases, and the NATS subscriptions and background loops are thin
  shells over use cases.
- `internal/archtest` enforces these import rules in `go test`.

## Consequences

Domain and use-case tests run DB-free (`make test`); adapter behaviour is covered by the
`integration`-tagged suite against a real TimescaleDB+PostGIS. A layer crossing in review is sent
back for restructuring.
