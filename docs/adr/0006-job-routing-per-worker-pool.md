# 6. Jobs are routed to named worker pools

Status: accepted (2026-10-25). Replaces "pool by name, one subject per provider" in
`docs/worker-pooling-vs-versioning.md`.

## Context

Jobs were published on `cairn.jobs.<job>.<provider>` and each worker created a durable consumer
named after its `worker_name`. `CAIRN_JOBS` is a work-queue stream, which refuses a second
consumer with the same filter (`err_code=10100 filtered consumer not unique on workqueue
stream`). So a provider could only ever have one worker name, and two implementations of one
provider (e.g. an API importer and a FIT-file importer) could not run side by side. With a shared
subject, whichever worker pulled a re-fetch would also silently move the source to its package.

Several worker implementations per provider are a goal.

## Decision

A **pool** is every instance sharing one `worker_name` for one provider. Jobs go to exactly one
pool:

- Subjects are `cairn.jobs.<job>.<provider>.<worker_name>`, discovery is
  `cairn.discover.<provider>.<worker_name>`. Provider and name are separate subject tokens, so two
  pools never overlap; joining them with `-` would not be unique (`a-b`+`c` vs `a`+`b-c`).
- Instances of one pool share one durable consumer per subject; versions inside a pool follow
  ADR 0005.
- An external account belongs to one pool (`external_accounts.assigned_worker_name`). Every
  account-scoped job (reconcile, fetch_source incl. re-fetch, import_gear, import_metrics,
  discovery) goes to that pool. An unassigned account uses the provider's only live,
  release-compatible pool and is then assigned to it; with several pools the user picks one on
  Connections. An assigned pool that is offline keeps receiving jobs; they wait in the queue.
- Re-parse goes to a pool of the source's own package with an allowed parser (ADR 0005), which
  need not be the account's pool.
- A source changes package only through a re-fetch by the account's pool, i.e. after the account
  was deliberately moved to another implementation.
- The worker key `{name}-{provider}` stays the webhook URL and control subject; enrollment refuses
  a name whose key equals another pool's.
- Pre-0.5 provider-only job subjects stay in the permission template for one minor release so an
  upgraded pool drains jobs queued before the upgrade. Removed in 0.6.

## Refinements (2026-10-25 review)

- **Pools are a registry, not a presence snapshot.** `worker_pools` (provider, name → package)
  is filled from manifests and results and backfilled from existing sources. Routing,
  connect-time choice and the UI pickers use it; presence only says online/offline. A pool
  name is permanent and bound to one package: a worker reporting another package under a known
  name is refused (manifest and results). A different implementation needs a new name.
- **Adoption.** A pre-0.5 connection (no pool) is adopted by the first pool that connects the
  same provider account only when its sources were imported under that pool's name, or the same
  connection reconnects. Otherwise the connect creates a new row and the old one stays
  unassigned until its own pool connects it.
- **Reassignment** (`PUT /api/accounts/{id}/worker`) is allowed only onto a pool of the same
  package as the connection's imports; another implementation is another connection. Unpinning
  is refused while the provider account has another connection.
- **Results and lookups are per pool**: `cairn.results.<job>.<provider>.<name>` (reconcile
  appends the account) and `cairn.accounts.lookup.<provider>.<name>`. Core verifies the sender's
  `worker_name` against the subject, so a pool cannot publish for or look up another pool's
  accounts. Tokens, blobs and credentials stay provider-scoped: pools of one provider are
  mutually trusted for those. The pre-0.5 provider-scoped subjects stay allowed through 0.5.
- **Rate-limit budgets are per pool** (`<provider>.<pool>.<window>` in the shared KV), i.e. per
  provider app.
- **Bulk re-parse**: `POST /api/accounts/{id}/reparse-out-of-date` re-parses every source a
  newer parser of its pool can read; the Connections page shows the counts. A worker too old for
  a re-parse hands the job back with back-off and, after ~35 minutes, reports `parser_too_old`,
  which keeps the source flagged rather than failing it.

## Consequences

- Specialised importers for one provider can run side by side, each owning the accounts assigned
  to it. One provider account may be connected once per pool (`external_accounts` is unique on
  user, provider, provider account id and pool): two pools feeding the same activity are two
  connections, each with its own sync state, queue and tokens, and a user who does not want a
  pool simply does not connect it. The matcher groups their sources into one activity; a merge
  priority entry `<provider>.<worker_name>` prefers one pool, a bare provider covers all of them.
  The webhook account lookup takes the asking pool's name and returns its connection. A gear
  record mapped on one connection maps the same provider gear id on the account's other
  connections.
- Workers derive all job subjects from their own name, and publish their own `fetch_source`
  sub-jobs to their own pool.
- Renaming a worker moves it to a new pool: reassign its accounts, or jobs keep going to the old
  name.
