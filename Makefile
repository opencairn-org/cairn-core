# Task-runner contract (https://fleetlint.org/baseline/#task-runner-contract). `make check` is the definition of green.
#
# Fresh checkout:
#   make tools proto-tools   # pinned Go tools into $(go env GOPATH)/bin (protoc-gen-es: see docker/Dockerfile.build-base)
#   make proto               # Go + TS stubs (gen/proto is committed, web/src/lib/proto is not)
#   make check               # lint, tests, coverage, audit, build, fleetlint
#
# Runtime prerequisites: Go (go.mod), Node (web/.nvmrc), Postgres 16+ with TimescaleDB + PostGIS.
# CI runs `make check` inside ghcr.io/opencairn-org/cairn-build-base, which carries the same toolchain.
SHELL := bash
.SHELLFLAGS := -euo pipefail -c
.ONESHELL:
.DEFAULT_GOAL := check-fast
MAIN_BRANCH ?= main

GO        ?= go
BUF       ?= buf
BIN_DIR   := bin
# web/node_modules can carry stray Go sources; keep them out of every Go gate.
PKGS       = $(shell $(GO) list ./... | grep -v /web/)
WEB       := web
# Go statement coverage across all packages (-coverpkg), gen/ excluded. COVER_MIN gates unit + integration suites
# (CAIRN_TEST_DATABASE_URL set, as in CI); COVER_MIN_UNIT gates a DB-less local run. Honest floors just below today's
# numbers, under the usual 80: NATS/S3/OIDC paths have no test infrastructure yet. Raise them as tests land.
COVER_MIN      ?= 57
COVER_MIN_UNIT ?= 19
# Test database for the integration suite (make test-db-up); keep in step with docker-compose.dev.yml and check.yml.
TEST_DB_IMAGE   ?= docker.io/timescale/timescaledb-ha:pg17-ts2.27
TEST_DB_PORT    ?= 55432
TEST_DB_URL     := postgres://cairn:cairn@127.0.0.1:$(TEST_DB_PORT)/cairn?sslmode=disable
CONTAINER       ?= $(shell command -v podman 2>/dev/null || command -v docker 2>/dev/null)
LICENSES  ?= MIT,Apache-2.0,BSD-2-Clause,BSD-3-Clause,ISC,MPL-2.0
# Each pin carries a `# renovate:` comment so Renovate bumps it (customManagers in renovate.json).
# renovate: datasource=go depName=github.com/golangci/golangci-lint/v2
GOLANGCI_VERSION      ?= v2.15.0
# renovate: datasource=go depName=golang.org/x/vuln
GOVULNCHECK_VERSION   ?= v1.9.0
# renovate: datasource=go depName=github.com/google/go-licenses/v2
GO_LICENSES_VERSION   ?= v2.0.1
# renovate: datasource=go depName=github.com/go-gremlins/gremlins
GREMLINS_VERSION      ?= v0.6.0
# Mutation test efficacy floor for the pure domain package (58.8% today).
MUTATION_MIN ?= 55
# renovate: datasource=go depName=github.com/fleetlint/fleetlint
FLEETLINT_VERSION     ?= v0.8.1
# Proto toolchain; keep in step with docker/Dockerfile.build-base.
# renovate: datasource=go depName=github.com/bufbuild/buf
BUF_VERSION           ?= v1.74.0
# renovate: datasource=go depName=google.golang.org/protobuf
PROTOC_GEN_GO_VERSION ?= v1.36.12
# renovate: datasource=go depName=connectrpc.com/connect/v2
PROTOC_GEN_CONNECT_GO_VERSION ?= v2.0.1
# renovate: datasource=go depName=github.com/grpc-ecosystem/grpc-gateway/v2
PROTOC_GEN_OPENAPIV2_VERSION ?= v2.32.0
# Tools are built with this module's toolchain: golangci-lint refuses code that targets a newer Go than it was built with.
TOOLCHAIN := $(shell $(GO) env GOVERSION)

LDFLAGS := -X main.Version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: help tools proto-tools fmt lint vet test cover audit build dist check-fast check release tidy clean \
	install mutate test-race test-integration test-db-up test-db-down web-deps web-lint web-check web-test web-cover web-audit web-build \
	proto proto-lint proto-format proto-check proto-breaking migrate-up migrate-status migrate-redo run \
	dev-up dev-down dev-logs dev-server dev-web all

help: ## list targets
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | awk -F':.*##' '{printf "  %-16s %s\n", $$1, $$2}'

all: build

# ----------------------------------------------------------------------------
# Contract targets
# ----------------------------------------------------------------------------

tools: ## install the pinned Go gate tools (gitleaks comes from its own release; Node tools from web/package-lock.json)
	GOTOOLCHAIN=$(TOOLCHAIN) $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)
	GOTOOLCHAIN=$(TOOLCHAIN) $(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	GOTOOLCHAIN=$(TOOLCHAIN) $(GO) install github.com/google/go-licenses/v2@$(GO_LICENSES_VERSION)
	$(GO) install github.com/fleetlint/fleetlint/cmd/fleetlint@$(FLEETLINT_VERSION)
	GOTOOLCHAIN=$(TOOLCHAIN) $(GO) install github.com/go-gremlins/gremlins/cmd/gremlins@$(GREMLINS_VERSION)

proto-tools: ## install the pinned buf + Go proto plugins
	$(GO) install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)
	$(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	$(GO) install connectrpc.com/connect/v2/cmd/protoc-gen-connect-go@$(PROTOC_GEN_CONNECT_GO_VERSION)
	$(GO) install github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-openapiv2@$(PROTOC_GEN_OPENAPIV2_VERSION)

fmt: web-deps ## format in place (Go + web)
	golangci-lint fmt
	cd $(WEB) && npm run format

lint: proto-lint web-lint ## format check + linters + vet, warnings as errors
	golangci-lint fmt --diff
	golangci-lint run
	$(GO) vet -tags integration,embedweb $(PKGS)

vet: ## go vet only
	$(GO) vet $(PKGS)

test: web-test ## unit tests: Go with the race detector (shuffled) + web
	$(GO) test -race -shuffle=on -count=1 $(PKGS)

cover: web-cover ## coverage gates: Go total (gen/ excluded) >= COVER_MIN (with DB) / COVER_MIN_UNIT, web in vite.config.ts
	# Build covdata once up front: parallel first builds race ("text file busy") on a cold cache.
	$(GO) tool covdata help >/dev/null 2>&1 || true
	if [ -n "$${CAIRN_TEST_DATABASE_URL:-}" ]; then tags=integration; min=$(COVER_MIN); \
	elif [ -n "$${CI:-}" ]; then echo "CI must set CAIRN_TEST_DATABASE_URL"; exit 1; \
	else tags=; min=$(COVER_MIN_UNIT); echo "CAIRN_TEST_DATABASE_URL unset: unit suite only (make test-db-up for the full gate)"; fi
	coverpkg=$$($(GO) list ./... | grep -v -e /web/ -e /gen/ | paste -sd, -)
	# -coverpkg makes each line list every package; keep the summary short.
	$(GO) test -count=1 -tags "$$tags" -coverpkg="$$coverpkg" -coverprofile=cover.raw.out $(PKGS) | sed 's/ of statements in .*//'
	grep -v '/gen/' cover.raw.out > cover.out
	total=$$($(GO) tool cover -func=cover.out | awk '/^total:/ {sub("%", "", $$3); print $$3}')
	echo "coverage $$total% (minimum $$min%)"
	if [ "$${total%.*}" -lt "$$min" ]; then echo "coverage below $$min%"; exit 1; fi

# go-licenses spots stdlib packages by GOROOT; pass the active toolchain's, not the one it was built with
audit: web-audit ## vulnerabilities, licenses, tidy module graph, secrets
	govulncheck $(PKGS)
	GOROOT=$$($(GO) env GOROOT) go-licenses check $(PKGS) --ignore github.com/opencairn-org/cairn-core --allowed_licenses=$(LICENSES)
	$(GO) mod tidy -diff
	gitleaks git --no-banner --redact .

build: web-build ## build the cairn binary into ./bin/cairn (+ the web app)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags='$(LDFLAGS)' -o $(BIN_DIR)/cairn ./cmd/server

mutate: ## mutation testing of internal/domain (weekly in CI, not part of check)
	gremlins unleash --threshold-efficacy $(MUTATION_MIN) ./internal/domain

dist: ## the release image, locally (CI builds and pushes it in build-core.yml)
	docker build -f docker/Dockerfile.core -t ghcr.io/opencairn-org/cairn-core:dev .

check-fast: ## before every commit (< 30 s)
	golangci-lint fmt --diff
	golangci-lint run --new-from-rev=origin/$(MAIN_BRANCH) || golangci-lint run
	$(GO) test -short $(PKGS)

check: proto-check proto-breaking lint test cover audit build ## definition of green (< 10 min)
	fleetlint check --fail-on error

release: ## refuse dirty/non-main -> check -> signed tag V (its CHANGELOG.md section must exist); pushing the tag publishes the image
	@test -n "$(V)" || { echo "usage: make release V=vX.Y.Z"; exit 1; }
	@test -z "$$(git status --porcelain)" || { echo "dirty tree"; exit 1; }
	@test "$$(git branch --show-current)" = "$(MAIN_BRANCH)" || { echo "not on $(MAIN_BRANCH)"; exit 1; }
	@grep -q "^## \[$(V:v%=%)\]" CHANGELOG.md || { echo "CHANGELOG.md has no '## [$(V:v%=%)]' section"; exit 1; }
	$(MAKE) check
	git tag -s "$(V)" -m "$(V)"
	@echo "tagged $(V) — push with: git push origin $(V)"

tidy: ## go mod tidy
	$(GO) mod tidy

clean: ## remove build and coverage output
	rm -rf $(BIN_DIR) cover.out cover.raw.out $(WEB)/coverage

# ----------------------------------------------------------------------------
# Go extras
# ----------------------------------------------------------------------------

install: ## go install the server
	$(GO) install -ldflags='$(LDFLAGS)' ./cmd/server

test-race: ## Go unit tests with the race detector
	$(GO) test -race $(PKGS)

# Integration tests run against a real Postgres (TimescaleDB+PostGIS). Set CAIRN_TEST_DATABASE_URL to a throwaway DB
# whose user may CREATE DATABASE; the suites migrate it (cmd/server uses a fresh database per run). Build-tagged
# `integration` so plain `make test` stays DB-free. `make test-db-up` starts one with podman or docker.
test-integration: ## Go integration tests (needs CAIRN_TEST_DATABASE_URL)
	$(GO) test -count=1 -tags integration $(PKGS)

test-db-up: ## start a throwaway TimescaleDB+PostGIS for the integration suite and print its URL
	test -n "$(CONTAINER)" || { echo "need podman or docker"; exit 1; }
	$(CONTAINER) run -d --rm --name cairn-test-db -e POSTGRES_USER=cairn -e POSTGRES_PASSWORD=cairn -e POSTGRES_DB=cairn \
		-p 127.0.0.1:$(TEST_DB_PORT):5432 $(TEST_DB_IMAGE) >/dev/null
	for _ in $$(seq 60); do $(CONTAINER) exec cairn-test-db pg_isready -q -h 127.0.0.1 -U cairn -d cairn && break; sleep 2; done
	echo "export CAIRN_TEST_DATABASE_URL='$(TEST_DB_URL)'"

test-db-down: ## stop the test database
	$(CONTAINER) rm -f cairn-test-db >/dev/null

# ----------------------------------------------------------------------------
# Web (SvelteKit SPA in web/; tool versions pinned by web/package-lock.json)
# ----------------------------------------------------------------------------

# The TS proto stubs are not committed; generate them on first use. npm ci reruns when the lockfile changes.
web-deps: $(WEB)/node_modules/.package-lock.json
	test -d $(WEB)/src/lib/proto || $(MAKE) proto

$(WEB)/node_modules/.package-lock.json: $(WEB)/package-lock.json
	cd $(WEB) && npm ci

web-lint: web-deps ## prettier + eslint + svelte-check
	cd $(WEB)
	npm run lint
	npm run check

web-test: web-deps ## vitest
	cd $(WEB) && npm test

web-cover: web-deps ## vitest with coverage thresholds
	cd $(WEB) && npm run cover

web-audit: web-deps ## npm audit (high+), licenses, unused dependencies
	cd $(WEB)
	npm audit --audit-level=high
	npm run licenses
	npm run unused

web-build: web-deps ## vite build (node adapter) + static SPA build
	cd $(WEB)
	npm run build
	npm run build:static

# ----------------------------------------------------------------------------
# Proto generation
#
# Generates gen/proto/cairn/{v1,worker/v1} (Go + Connect, committed), web/src/lib/proto (TS, not committed) and
# gen/openapi/openapi.yaml. Plugins are local binaries (see api/proto/buf.gen.yaml); `make proto-tools` installs the Go
# ones, docker/Dockerfile.build-base carries all of them. Provider-worker stubs live in their own repos.
# ----------------------------------------------------------------------------

proto: ## generate proto stubs
	cd api/proto && $(BUF) generate

proto-lint: ## buf lint
	cd api/proto && $(BUF) lint

proto-format: ## buf format in place
	cd api/proto && $(BUF) format -w

proto-check: proto ## regenerate and fail if the committed Go stubs drifted
	git diff --exit-code -- gen/proto

# The proto contract is the public API (workers build against it); gorelease does not apply to a binary.
proto-breaking: ## buf breaking against the previous release tag (needs tags: fetch-depth 0 in CI)
	base=$$(git describe --tags --abbrev=0 --match 'v*')
	$(BUF) breaking api/proto --against ".git#tag=$$base,subdir=api/proto"

# ----------------------------------------------------------------------------
# Database migrations (delegates to the cairn binary's `migrate` subcommand)
# ----------------------------------------------------------------------------

migrate-up: ## apply pending migrations (needs CAIRN_DATABASE_URL)
	$(GO) run ./cmd/server migrate up

migrate-status:
	$(GO) run ./cmd/server migrate status

migrate-redo:
	$(GO) run ./cmd/server migrate redo

run: ## run the server
	$(GO) run ./cmd/server serve

# ----------------------------------------------------------------------------
# Local dev: supporting services in docker compose, binaries on the host with live-reload.
#
#   make dev-up                  # postgres + nats + minio + mailpit (compose)
#   cp dev.env.example dev.env   # once; tweak if needed
#   make dev-server              # shell 2: Go server, rebuilds on save (air)
#   make dev-web                 # shell 3: SvelteKit dev server (vite HMR)
#   make dev-down                # stop services (add ARGS=-v to wipe data)
#
# dev-server sources dev.env (if present) so the binary picks up the compose connection strings + dev secrets.
# ----------------------------------------------------------------------------
DEV_COMPOSE := docker compose -f docker-compose.dev.yml
# renovate: datasource=go depName=github.com/air-verse/air
AIR_VERSION ?= v1.67.4
AIR         := $(GO) run github.com/air-verse/air@$(AIR_VERSION)

dev-up:
	$(DEV_COMPOSE) up -d

dev-down:
	$(DEV_COMPOSE) down $(ARGS)

dev-logs:
	$(DEV_COMPOSE) logs -f

dev-server:
	set -a; [ -f dev.env ] && . ./dev.env; set +a; $(AIR) -c .air.server.toml

dev-web:
	set -a; [ -f dev.env ] && . ./dev.env; set +a; cd $(WEB) && npm run dev
