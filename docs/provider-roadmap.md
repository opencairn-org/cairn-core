# Provider roadmap

Status: proposal, research snapshot of 2026-11-01. Sources inline.
"est." = estimate, "unverified" = no primary source found.

Ranking criteria: user base × can a self-hoster get access on their own ×
data value (FIT/GPS > daily metrics) × ToS fit × effort.

## 1. Order at a glance

| # | Provider | Users | Access for a self-hoster | Data | Worker | Client |
|---|---|---|---|---|---|---|
| 0 | file-drop (folder) | covers every export | n/a | FIT/GPX/TCX | Go | fsnotify + muktihari/fit |
| 1 | Google Health (Fitbit, Pixel Watch) | Fitbit 29.6M active (2019, last public), est. | self-serve, ≤100 users unverified | sessions + TCX GPS, HR 1 s, HRV, sleep, weight | Go | `google.golang.org/api/health/v4` (official) |
| 2 | Polar | 5M+ Play installs, €112M rev. 2025 | self-serve | FIT, laps, samples, HRV, sleep | Go | oapi-codegen (v3 + v4 specs) |
| 3 | Oura | 5.0M paid members (2026-06) | self-serve, 10 users before review | metrics only | Go | oapi-codegen (OAS 3.1) |
| 4 | Withings | unknown | self-serve, 10 users (free plan) | weight/body comp, sleep, BP | Go | hand-rolled |
| 5 | Ride with GPS | est. ~220k MAU | open to anyone | original FIT, gear | Go | oapi-codegen (official spec) |
| 6 | intervals.icu | 160k athletes | personal API key | original files, streams, laps, gear, wellness | Go | oapi-codegen (official spec) |
| 7 | Hammerhead | unknown, small | self-serve | original FIT (cycling) | Go | hand-rolled |
| 8 | Wahoo | 1M+ installs | sandbox self-serve | FIT | Go | hand-rolled |
| 9 | WHOOP | unknown | sandbox, 10 members | metrics only | Go | oapi-codegen (OAS 3.0.1) |
| 10 | COROS (via official MCP) | 1M+ installs | self-serve MCP, untested | FIT (50/day), metrics | Go | `modelcontextprotocol/go-sdk` |
| 11 | Concept2 | unknown | self-serve | results, strokes | Go | hand-rolled |
| M | Health Connect / HealthKit | largest overall | on-device only | everything | cairn-kmp | KHealth / HealthKMP |
| X | Komoot, Peloton, Suunto | 45M reg. / 2.55M subs / 1M+ | unofficial only | GPX / perf graph / FIT | Python / Python / Go | kompy / pylotoncycle / suuntool |

Phase 0 comes first. Phase 1 = rows 1–4, phase 2 = rows 5–11, M = after
cairn-kmp 1.0 (imports are a 1.0 non-goal), X = opt-in, labelled unofficial.

## 2. Phase 0: SDK and provider-agnostic core

The NATS layer is generic today (enrollment, subjects, permission template,
pool routing, `cairn.tokens.*`, webhook forwarder, presign, claim-check, result
ingest). These parts are not, and each blocks some provider below:

| Gap | Where | Blocks |
|---|---|---|
| OAuth endpoints compiled into core; manifest `auth_method`/`OAuthConfig`/`config_fields` unused (no worker sets them) | `usecase/providerlink/providers.go` | every OAuth provider = core release |
| Code exchange is Strava-shaped: account id from `athlete.id`, client creds in body, no PKCE, no extra authorize params | `adapter/secondary/provideroauth/exchange.go` | Polar (Basic auth + `POST /v3/users`), Withings (`action=requesttoken`), Google (`access_type=offline`), Oura/WHOOP (id needs a profile call), COROS (DCR) |
| Only `cairn.creds.garmin.get` is answered | `adapter/primary/workerrpc/logins.go` | API-key/login providers: intervals.icu, Concept2 token, RWGPS single-user, Komoot, Peloton |
| `import_metrics` is manual only, not capability-gated, UI button `provider === 'garmin'` | `providerlink/jobs.go`, `AccountsSync.svelte` | Oura, WHOOP, Withings, Google Health |
| `POST /webhooks/{p}` always answers 200; GET verify forwards query, not headers | `rest/sync_webhook.go` | Google Health verification (expects 401/403 on the unauthenticated POST) |
| Rate-limit windows fixed by suffix (15 min / 24 h) | strava `internal/nats/rate_limiter.go` | per-minute (Google, WHOOP, Withings), per-second (intervals.icu) |
| No cross-instance lock around token refresh | strava `internal/workersdk/tokens.go` | Oura (single-use refresh), WHOOP/Withings/Wahoo (rotating) |
| Deauth ignored, `deleted_upstream` not consumed (backlog #2/#3) | strava `webhook.go`, core | Polar/Wahoo ToS require deletion on request (Wahoo: 48 h) |

Changes:

1. **`cairn-worker-sdk-go`** (new repo, release major.minor follows core per
   ADR 0005). Move from cairn-provider-strava `internal/{workersdk,nats,capability,port}`
   (~2.5k lines, already provider-agnostic) plus the generic parts still in
   `cmd/worker-strava` (`publishResultViaBlob`, `publishFailureResult`,
   `putPresigned`, `parserTooOldBackoff`, `enqueueFetchSource` msg-ids,
   `reconcileWindowStart`, `newJobResult` stamping). Ship the generated Go
   proto stubs (drops `scripts/sync-proto.sh`). Add: x/oauth2 `TokenSource`
   over `cairn.tokens` with a KV-CAS refresh lock, configurable rate windows,
   HMAC webhook helpers, a FIT→`ImportedActivity` mapper on muktihari/fit, a
   test kit (embedded nats-server + fake core responders). Port Strava onto it.
   No Python SDK until a second Python worker exists (garmin `runtime.py` is
   Garmin-shaped).
2. **OAuth from the manifest.** Core reads authorize URL, scopes, PKCE and
   extra authorize params from the pool's manifest; keeps state/CSRF, redirect
   and token storage. The worker exchanges the code over a new request/reply
   `cairn.oauth.<provider>.exchange` (add to the permission template) and
   returns tokens + provider account id, so Basic auth, PKCE verifier, user
   registration and non-standard token endpoints stay in the worker. Validate
   manifest URLs (https) and bind them like pool names (admin confirms a
   change). Display name and `ActivityURLTemplate` move to the manifest too.
3. **Generic credentials.** `cairn.creds.*.get` checking the account's
   provider; `config_fields` drive the connect form (replaces the Garmin text
   in `connections/+page.svelte`).
4. **Metrics.** Gate `import_metrics` on a declared metric capability (like
   gear), schedule it with reconcile, show the UI button by capability.
5. **Webhooks.** Optional synchronous POST verify when the worker declares it;
   forward headers on GET verify. (Go 1.22 mux routes HEAD to the GET pattern,
   which covers Withings' HEAD probe.)
6. **Data-use policy in the manifest** (generic flags, no provider names in
   core): `no_llm` (exclude from MCP — Strava API agreement Nov 2024, Oura
   §4(d)), `no_raw_archive` / retention (WHOOP §4.2, Oura §3(m)),
   `attribution` (Polar "Polar Ecosystem", Wahoo logos). Core enforces in MCP,
   export, federation, archive.
7. Backlog #2/#3 (deauth, `deleted_upstream`) before the first new OAuth provider.

**Pilot: file-drop worker** (Go). Watches a folder (fsnotify + periodic
rescan, dedup by content hash); users fill it with Syncthing/rclone/Nextcloud.
No OAuth, so it tests the SDK and FIT mapper in isolation, and it covers
every provider without a usable API: Huawei, Zepp, Suunto and COROS exports,
and Gadgetbridge auto-export (GPX + FIT per sync; original FIT for Garmin and
iGPSPORT devices — https://gadgetbridge.org/internals/automations/auto-export/).

## 3. Phase 1

### 3.1 Google Health API (Fitbit, Pixel Watch)

- **Why first:** largest server-side base. Fitbit Web API turns off
  **2026-11-02** (HTTP 410), its replacement is the Google Health API
  (https://developers.google.com/health/about). Google Fit REST: no sign-ups
  since 2024-05, supported "until end of 2026".
- **Access:** any Google Cloud project, free. All scopes restricted; unverified
  apps capped at 100 users (enough per instance); >100 needs verification plus
  yearly CASA ($500–4.5k). Testing mode issues 7-day refresh tokens → publish
  the app to production (unverified). Users first link their Google account in
  the Google Health app. https://developers.google.com/health/setup
- **Auth:** Google OAuth2, `access_type=offline` + `prompt=consent`; max 100
  refresh tokens per account per client.
- **Data:** exercise sessions (GPS via `exportExerciseTcx`), HR 1 s, HRV, SpO2,
  resting HR, sleep, weight/body fat, VO2max. Pages hold 25 items.
- **Push:** subscriber endpoint; verification = two POSTs (with your auth →
  200/201, without → 401/403, needs Phase 0 item 5); `GOOGLE-HEALTH-API-SIGNATURE`
  (Tink, keys rotate every 30 days); payload names changed data, fetch it;
  retries up to 7 days. https://developers.google.com/health/webhooks
- **Limits:** 300 req/min per user; 250 QPS per unverified app.
- **ToS:** Limited Use (no ads, no sale).
- **Client:** `google.golang.org/api/health/v4` (google-api-go-client v0.310.0,
  2026-10-28, generated, Apache-2.0) + `golang.org/x/oauth2/google`. Dead:
  python-fitbit (2019), Thomas2500/go-fitbit (archived).
- **Practice:** ship polling first, webhooks once Phase 0 item 5 lands; archive
  the TCX + JSON as dataset 1.

### 3.2 Polar (AccessLink v3 + v4)

- **Access:** any Polar Flow user creates a client at admin.polaraccesslink.com;
  free (fees reserved, §6).
- **v3:** tokens never expire, no refresh; `POST /v3/users` registers the user
  (do it in the worker's exchange); transactional exercise/daily-activity
  endpoints deprecated 2025-08-18 → use non-transactional `/v3/exercises` +
  per-exercise FIT/TCX/GPX. Only the last 30 days **and** after registration.
  Webhooks: one per client, `Polar-Webhook-Signature` HMAC-SHA256, secret shown
  once, auto-deactivated after 7 days of failures. Limits 500 + 20×users / 15 min,
  5000 + 100×users / 24 h. https://www.polar.com/accesslink-api/
- **v4 ("AccessLink Dynamic API"):** `auth.polar.com`, Basic client auth, 12 h
  access + refresh, no registration; date-range pulls (90-day windows, 1 day
  with `features=`) of training sessions with laps, samples, routes, zones, RR;
  sleep, nightly recharge, weight. No FIT export. Has a `/subscriptions`
  section — check whether it replaces v3 webhooks. 3000 / 15 min,
  100k / 24 h per client. https://www.polar.com/polar-api-v4/
- **Plan:** v3 for webhooks + original FIT of new sessions, v4 for history and
  metrics. Unverified: one client holding v3 and v4 consents side by side —
  test first.
- **ToS:** credit "Polar Ecosystem", brand guidelines, delete on request and on
  termination, no competing app. https://www.polar.com/en/legal/polar-api-agreement
- **Client:** oapi-codegen from v3 `swagger.yaml` (OAS 3.0.0); v4 is Swagger 2.0
  → convert with kin-openapi `openapi2conv` first. Go libs dead
  (kovacou/go-polar 2021); Python `polar-flow-api` (MIT, 3★, v3 only) too small.
- **Reference:** open-wearables' Polar strategy (MIT).

### 3.3 Oura (API v2)

- **Access:** self-serve, 10 users before Oura review; users need an active
  membership (403 otherwise). PATs dead since Dec 2025.
- **Auth:** code + PKCE S256; **single-use refresh tokens** (SDK refresh lock);
  access ~24 h; `User-Agent` required. https://cloud.ouraring.com/docs/authentication
- **Data:** sleep, HRV, readiness, SpO2, stress, resilience, VO2max, HR series.
  Workouts are summaries — no GPS, no streams. Metrics source only.
- **Push:** per-app subscriptions with `expiration_time` (renew job), GET
  challenge, `x-oura-signature` HMAC, 10 retries.
- **ToS (2026-06-08):** retain "only as strictly necessary" (§3(m)); no AI
  training; LLM use only via Oura's MCP, not the API (§4(d)) → needs the
  `no_llm` policy flag before shipping; read the agreement in full.
  https://cloud.ouraring.com/legal/api-agreement
- **Client:** oapi-codegen from https://cloud.ouraring.com/v2/static/json/openapi-1.43.json
  (OAS 3.1; compiled with oapi-codegen v2.8.2 in this research). Python
  `oura-ring` (MIT, 1.0.1 2026-06) is fine but not needed.

### 3.4 Withings

- **Access:** "Start for Free" plan, 10 users, more after review; a callback not
  on HTTPS:443 with a real domain forces restricted mode. Unverified:
  Withings+ membership required for accounts created after 2026-10-12.
- **Auth:** non-standard `POST wbsapi.withings.net/v2/oauth2`,
  `action=requesttoken`, response wrapped in `{status, body}` → custom
  TokenSource. Code valid 30 s (exchange immediately). Access 3 h, refresh 1 y,
  rotated on every refresh.
- **Data:** weight/body composition (the reason to add it), BP, sleep,
  workout summaries, intraday activity. GPS not exposed (unverified).
- **Push:** Notify per user; HEAD probe at registration; form-encoded POSTs;
  **no redelivery** → reconcile with `lastupdate`.
- **Limits:** 120 req/min per app; over the limit = HTTP 200 with body
  `status: 601` (unverified) — map to rate-limit, not success.
- **Client:** hand-rolled (getmeas, getactivity, getworkouts, sleep summary,
  notify). Python `aiowithings` (MIT, Home Assistant) is healthy but has no
  OAuth; `withings-api`, `nokiahealth` dead.

## 4. Phase 2

**Ride with GPS** — "Anyone may use the API" (ToS 2026-09-11). OAuth2 or
api_key + auth_token. `GET /api/v1/trips/{id}.fit` is the original device file
if the trip is unedited; gear embedded; no segments. Webhooks HMAC-SHA256
(`x-rwgps-signature`), must answer within 1 s (the forwarder answers
immediately); use them as triggers and pull via `/sync.json?since=`. No fixed
rate limits. Client: oapi-codegen from https://ridewithgps.com/api/v1/openapi.yaml.

**intervals.icu** — personal API key (Basic, user `API_KEY`), needs Phase 0
item 3; OAuth apps are owner-only until published. `/activity/{id}/file` =
original file (gz), plus streams, intervals (laps), segments, gear, wellness.
Strava-sourced activities are excluded. Webhooks for OAuth apps only. API key:
2500 / 15 min, 10 req/s per IP. Value: hub for Zwift, MyWhoosh, Rouvy, Wahoo.
Risk: Garmin's new developer terms (announced 2026-10, no date) give Garmin a
veto over downstream sharing; Garmin-sourced data on intervals.icu will likely
disappear from its API (https://the5krunner.com/2026/10/14/garmin-api-intervals-third-party-apps/).
Client: oapi-codegen from https://intervals.icu/api/v1/docs (~250 KB, filter
to the needed operations). Python clients dead.

**Hammerhead (Karoo)** — self-serve dev account; OAuth2 + refresh;
`/activities/{id}/file` = original FIT; webhook `X-Hmac-Signature`. Cycling
only, no metrics read. Spec OAS 3.1 (https://api.hammerhead.io/v1/docs/openapi.yml)
but only three endpoints → hand-roll. Licence text and rate limits unverified.

**Wahoo** — sandbox app immediately (25 / 5 min, 100 / h, 250 / day — enough
for a household); production after review. PKCE; access 2 h; rotating refresh;
max 10 unrevoked tokens per user since 2026-01-01 (refresh lazily, store the
pair atomically). `/v1/workouts` → `file.url` FIT; no third-party-app workouts
since 2025-04-01. Webhook `workout_summary` (`offline_data` scope), verify the
`webhook_token` in the payload. **ToS: may not "aggregate, cache, or store
geographic location information"** and delete within 48 h on request — read
the agreement before building (https://eu.wahoofitness.com/wahoo-api-agreement).
Hand-roll; no spec, libs dead.

**WHOOP (v2)** — sandbox 10 members. `offline` scope for refresh; access 1 h;
refresh rotates and invalidates live access tokens (refresh lock). Recovery,
HRV, sleep, strain, workouts — no GPS. Webhooks `X-WHOOP-Signature`
(HMAC-SHA256 over timestamp + body). 100 / min, 10k / day. **ToS §4.2: no
permanent copies beyond cache headers** → run with `no_raw_archive`,
`supports_local_reparse=false`. Client: oapi-codegen from
https://api.prod.whoop.com/developer/doc/openapi.json.

**COROS** — partner API is company-only; the official MCP server
(https://github.com/coroslab/coros-mcp) is self-serve: OAuth 2.1, PKCE S256,
DCR, regional issuers, refresh 180 d rotating. Activities, laps, original FIT
(**50 FIT per user per 24 h** → throttle backfill), sleep, HRV, training load.
Pull only. ToS grey (MCP is aimed at AI clients); untested whether an
arbitrary DCR client is accepted. Client: `modelcontextprotocol/go-sdk`
v1.8.0; golden tests on tool output (schemas unstable). Experimental.

**Concept2 Logbook** — self-serve, read-only apps need no approval; OAuth2
(`user:read`, `results:read`) or personal token. Results, splits, per-stroke
data, generated FIT/TCX export; webhooks for added/updated/deleted; no rate
limits. Hand-roll.

## 5. Mobile (on-device stores)

- **Health Connect** (Android 14+) also carries Samsung Health, Withings, Oura
  and others that write into it — do not integrate the Samsung Data SDK
  (partner approval). Only 30 days before the first grant unless
  `READ_HEALTH_DATA_HISTORY`; background reads need
  `READ_HEALTH_DATA_IN_BACKGROUND`; Play requires the Health apps declaration.
- **HealthKit** (iOS): workouts with routes, HR, HRV, SpO2, sleep, body; full
  history; needs the HealthKit entitlement.
- **KMP libraries:** KHealth (Apache-2.0, 2.1.0 2026-09, has HRV/SpO2/RHR,
  routes unclear) vs HealthKMP (Apache-2.0, 1.7.0 2026-08, routes/laps, no
  HRV/SpO2/RHR). Decide by spike; maybe both.
- **Path:** cairn-kmp uploads via the existing upload/manual RPCs after 1.0.
- **Optional bridge:** Health Auto Export (iOS, paid) posts JSON incl. workout
  routes to a URL → small Go worker behind `/webhooks/{provider}`, account
  matched by the per-account webhook token. Unverified: custom header support.

## 6. Unofficial (opt-in, labelled, ToS risk)

| Provider | Users | Why unofficial | Client | Notes |
|---|---|---|---|---|
| Komoot | 45M registered (2025) | partner-only; Bending Spoons bought it 2025-03 | `kompy` (Python, MIT, v0.2.0 2026-10-25) | GPX only; ToS §1.4 bans exports outside the export function; avoid KomootGPX (GPL-3.0) |
| Peloton | 2.553M subs (FY2026) | no API; ToS bans scraping | `pylotoncycle` (Python, BSD-2, 0.9.5 2026-06) | no FIT; peloton-to-garmin (GPL) as reference only |
| Suunto | 1M+ installs, €106.5M rev. 2025 | partner API for companies only | `tajchert/suuntool` (Go, MIT, 0.10.0 2026-08) | Sports-Tracker login like garminconnect |

The first Python one triggers extracting a Python SDK from the garmin runtime.

## 7. Skipped

| Provider | Reason | Alternative |
|---|---|---|
| Fitbit Web API | off 2026-11-02 | Google Health |
| Google Fit REST | no sign-ups, ends 2026 | Google Health / Health Connect |
| Samsung Health SDK | partner approval, Android-only | Health Connect |
| Huawei Health Kit | exercise data closed to individuals (partly verified) | file-drop, Gadgetbridge |
| Zepp/Amazfit | enterprise verification | Gadgetbridge → file-drop |
| Zwift | partners only, ToS bans reverse engineering | intervals.icu, RWGPS, file-drop |
| TrainingPeaks | partners only, ToS bans automation | — |
| MyWhoosh, Rouvy | no API | intervals.icu |
| Decathlon | no self-serve access (unverified) | — |
| Terra | $399+/month, data via their cloud | — |

Reference only: open-wearables (MIT, 2.7k★, Python; Polar/Suunto/Oura/WHOOP/
Withings/Google mappings — heavy stack, telemetry on by default),
open-health-sync (AGPL — do not copy).

## 8. Shared libraries

| Need | Go | Python | Note |
|---|---|---|---|
| OAuth2 | `golang.org/x/oauth2` (+ `/google`); PKCE via `GenerateVerifier` / `S256ChallengeOption` | `authlib` 1.9.1 | custom TokenSource for Withings |
| REST codegen | `oapi-codegen` v2 (8.6k★, active) | `openapi-python-client` | kin-openapi `openapi2conv` for Swagger 2.0 |
| FIT | `muktihari/fit` (BSD-3, v0.28.5 2026-10-17) | `fitdecode` (MIT, 0.11.0 2025-08) | core's `tormoder/fit` last released v0.15.0 in 2023-10 → migrate core too; `garmin-fit-sdk` is under the FIT Protocol License (not OSI) — check the license allowlist; `fitparse` dead (2020) |
| GPX | `tkrajina/gpxgo` (Apache-2.0) | `gpxpy` | |
| File watch | `fsnotify` (BSD-3, v1.10.1) | — | plus periodic rescan |
| MCP client | `modelcontextprotocol/go-sdk` v1.8.0 | `mcp` 2.3.0 | COROS |
| Webhook HMAC | stdlib `crypto/hmac` + `hmac.Equal` | `hmac.compare_digest` | Google Health uses Tink signatures |

## 9. To verify before building

- Polar: v3 + v4 consent on one client; whether v4 `/subscriptions` replaces v3 webhooks.
- Withings: plan limits and the Withings+ rule; GPS availability; status 601.
- COROS: DCR acceptance for a self-hosted client; ToS for non-AI use.
- Wahoo: location-storage clause vs storing tracks.
- Oura, WHOOP: retention clauses vs cairn's storage model.
- Hammerhead: licence terms, rate limits, webhook URL approval.
- Health Auto Export: custom headers.
