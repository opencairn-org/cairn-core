# 5. Providers carry three versions: release, dataset, parser

Status: accepted (2026-10-25); implementation pending. Supersedes the "same build" re-parse rule in
`docs/worker-pooling-vs-versioning.md`.

## Context

Every source was stamped `worker_version = "1"`, a hand-bumped integer that meant "same build".
Strava and Garmin v0.4.0 started parsing gear without bumping it, so a re-parse of an old archive
produced different data under the same version, and nothing showed which code had parsed a
source. The integer also had to answer two different questions: can this worker read the
archive, and is its output newer than what is stored.

## Decision

A provider worker has three independent versions.

**Release** (`v0.4.1`, semver, the git tag). States the core contract the worker implements
(proto messages, NATS subjects, job and result shapes). A worker is compatible with core when
both have the same major and minor; patch levels are free. A dev build (no tag) is exempt.

**Dataset version** (`N`, integer, per provider package). The format of the archived raw blob
the importer writes: the `version` field of the archive envelope. Bumped only when a parser can
no longer read the archives of the previous dataset version. A source keeps the dataset version
it was imported with until it is re-fetched.

**Parser version** (`N.M`, sent as `worker_version`). `N` is the dataset version the parser
reads, `M` counts changes to its output. Rules:

- Parser `N.x` reads every dataset `N` archive, including ones written by older importers, and
  treats fields missing from them as absent. It cannot read dataset `N-1` or `N+1`.
- `M` is bumped on every change to the parsed output: a new field, a fixed mapping, a changed
  unit. Refactors that keep the output byte-identical do not bump it.
- Versions only move forward. A source parsed by `N.M` is only re-parsed or re-fetched by a
  parser `>= N.M`; core refuses a result that would lower a source's version. A new dataset
  version resets `M` to 0 (`1.4` → `2.0`).
- A legacy bare integer `N` reads as `N.0`.
- The archive holds every input the parser uses. A parser change that needs a new input (e.g. a
  separate gear call) first makes the importer archive it; that is additive and stays dataset
  `N`, archives written before it lack the input and need a re-fetch to gain it.

Consequences for core:

| Live worker vs. stored source | Meaning | Action offered |
| --- | --- | --- |
| same `N`, same `M` | current | re-parse (re-run) |
| same `N`, higher `M` | newer parser | re-parse from archive (quota-free) |
| higher `N` | newer dataset | re-fetch from provider; archive is not readable |
| lower `N`, or same `N` and lower `M` | older worker | refused (would downgrade) |

The `parse_blob` job carries the source's current parser version (`min_parser_version`). A pooled
worker older than that NAKs the job, so a newer instance in the pool takes it; core still refuses
the result if one gets through.

## Consequences

- Workers declare `PARSER_VERSION` (`N.M`) and `DATASET_VERSION` (`N`) as constants and check
  that the archive envelope version equals `N`. A golden-file test per worker parses fixture
  archives of every supported dataset; changing the golden output without bumping `M` fails it.
- Every core minor release needs worker releases with the same minor, even without a contract
  change.
- Re-parse becomes the normal way to apply new parser logic to history; re-fetch is needed only
  after a dataset bump or when no archive exists.
- `source_worker_version` keeps the raw string; `dataset_version` and `parser_version` are
  stored as integers for comparison, and the release that wrote the source is stamped for
  diagnostics.
