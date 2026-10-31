# Federation (ActivityPub)

Status: **shipped (Phases 1–5), merged to `main` 2026-06-08.** This document is
both the original scoping design and the as-built reference. Federation is **off
by default** — gated by the instance flag `CAIRN_FEDERATION_ENABLED` *and* a
per-user opt-in (`users.federation_enabled`). It is **proven interoperating
between two live instances** (see §14). The code lives in
`cmd/server/federation*.go`, `internal/httpsig`, `internal/safehttp`, and
migrations 45–51; §12 tracks per-phase status. The sections below keep the
original design prose — where it reads "will" / "planned", §12 and §14 record
what actually landed.

Cairn today is a single self-hosted instance: users, a follow graph, per-field
visibility, kudos + comments, clubs, moderation. Federation lets a user on
instance A follow and interact with a user on instance B over **ActivityPub**
(the W3C standard the Fediverse runs on — Mastodon, etc.). The win: one user can
self-host and still be followed by friends on other Cairn instances (or any AP
server) without everyone sharing one server.

---

## 1. Goals / non-goals

**Goals**
- Remote **follow** of a Cairn user from another Cairn/AP instance (and vice versa).
- **Outbound** publishing of a user's activities (workouts) to remote followers,
  **honouring the existing per-field visibility model and privacy zones**.
- Remote **kudos** (Like) and **comments** (reply Note) federating both ways.
- Discovery via **WebFinger** (`acct:alice@cairn.example`).
- Server-to-server auth via **HTTP Signatures**; instance metadata via **NodeInfo**.
- **Opt-in** per instance (operator flag) and per user (a profile toggle) — never on by default.

**Non-goals (v2 scope boundary)**
- Full Mastodon-microblogging parity (hashtags, boosts-as-primary, polls). Cairn
  is an activity tracker; the timeline object is a *workout*, not a status.
- Federated **clubs** (groups). Clubs stay local in v2; revisit as v3.
- Federated **segments/leaderboards** (cross-instance ranking is a hard,
  abuse-prone consistency problem — explicitly out).
- Migrating accounts between instances (`Move`/aliases) — v3.
- Real-time streams / live segments over federation.

---

## 2. Standards

| Concern | Standard |
|---|---|
| Activity model + delivery | ActivityPub (server-to-server, the S2S half) |
| Vocabulary | Activity Streams 2.0 + `schema.org` extensions for sport data |
| Actor discovery | WebFinger (`/.well-known/webfinger`) |
| Request auth | HTTP Signatures (draft-cavage) — sign + verify `Signature` header |
| Object integrity (optional) | Linked-Data Signatures / object `proof` — defer to v3 |
| Instance metadata | NodeInfo 2.1 (`/.well-known/nodeinfo`) |
| Content type | `application/activity+json` (+ the AP profile param) |

---

## 3. Actor model

Each **local Cairn user that opts in** becomes an ActivityPub **Actor** of type
`Person`. Actor id is stable and dereferenceable:

```
id:        {PublicBaseURL}/users/{username}
inbox:     {PublicBaseURL}/users/{username}/inbox
outbox:    {PublicBaseURL}/users/{username}/outbox
followers: {PublicBaseURL}/users/{username}/followers
following: {PublicBaseURL}/users/{username}/following
publicKey: {actor-id}#main-key      (PEM, RSA-2048 or Ed25519)
preferredUsername: {username}
endpoints.sharedInbox: {PublicBaseURL}/inbox   (optional, for fan-out efficiency)
```

Notes:
- Actor ids use `/users/{username}` (a NEW route) distinct from the SPA's
  human page `/u/{username}` — content negotiation could merge them, but a
  separate path avoids HTML/JSON ambiguity and SPA-routing collisions.
- An actor is only published when **both** the instance flag and the user's
  `profile_is_public` + a new `federation_enabled` toggle are on. A non-federated
  user returns 404 on the actor route (indistinguishable from "no such user").
- `username` is the federation handle; it's already `citext UNIQUE`. Renames
  would break remote follows, so federation pins the handle (disallow rename once
  federated, or issue `Move` in v3).

---

## 4. Discovery (WebFinger + NodeInfo)

- `GET /.well-known/webfinger?resource=acct:alice@cairn.example` →
  links the `acct:` to the actor id (`rel="self"`,
  `type="application/activity+json"`). Only resolves federation-enabled users.
- `GET /.well-known/nodeinfo` → NodeInfo discovery → `/nodeinfo/2.1` with
  software name "cairn", version, `openRegistrations`, user/post counts (counts
  may be coarse or omitted for privacy).

---

## 5. Activity mapping

Cairn's existing social verbs map onto AP activities. Inbound (received in an
inbox) and outbound (delivered from an outbox) both go through the existing NATS
async layer (§8).

| Cairn action | ActivityPub | Direction | Maps to existing |
|---|---|---|---|
| Follow a user | `Follow` → `Accept`/`Reject` | both | `follows` (status pending→accepted) |
| Unfollow | `Undo{Follow}` | both | `follows` delete |
| Publish a workout | `Create{Object=Workout}` | out | a new `Activity` projection |
| Edit a workout | `Update{Workout}` | out | re-merge / edit |
| Delete a workout | `Delete{Workout}` → Tombstone | out | soft-delete |
| Kudos | `Like` | both | `kudos` (migration 35) |
| Remove kudos | `Undo{Like}` | both | kudos delete |
| Comment | `Create{Note, inReplyTo}` | both | `comments` (migration 35) |
| Delete comment | `Delete{Note}` | both | comment delete |
| Block a remote user | `Block` (server-internal) | out | `blocks` (migration 36) |
| Report content | `Flag` | out | `content_reports` (migration 36) |

Boost/`Announce` (re-sharing someone's workout) is **deferred** — it complicates
the visibility model and isn't a core tracker verb.

**Inbound auto-accept policy:** `Follow` is auto-`Accept`ed when the target's
profile is public; for a private/followers-gated profile it becomes a **pending
follow request** the user approves (the `follows.status='pending'` machinery
already exists — federation just creates pending rows from remote actors).

---

## 6. The Workout object

A workout is richer than a Note. Strategy: a **custom object type that degrades**.

```jsonc
{
  "@context": ["https://www.w3.org/ns/activitystreams",
               {"sport": "https://cairn.example/ns#"}],
  "type": "Note",                       // fallback type for generic AP servers
  "sport:type": "Workout",              // Cairn-aware servers read this
  "id": "{PublicBaseURL}/activities/{uuid}",
  "attributedTo": "{actor-id}",
  "published": "2026-06-07T08:00:00Z",
  "to":  ["{actor-id}/followers"],      // audience — see §7
  "cc":  [],
  "name": "Morning Run",                // title
  "content": "<p>10.2 km · 52:13 · 312 m climb</p>",  // human summary (HTML)
  "url": "{PublicBaseURL}/a/{uuid}",    // link back to the rich SPA view
  "sport:summary": {                    // structured, only fields visibility allows
    "discipline": "run", "distance_m": 10200, "moving_s": 3133,
    "elevation_gain_m": 312
    /* hr/power/pace included ONLY when the audience's CategorySet permits */
  }
}
```

- Generic AP servers (Mastodon) render the `content` HTML summary + the `url`
  link — graceful degradation, no map/streams.
- Cairn-aware servers read `sport:summary` and can render richer cards.
- **GPS track / streams are NOT embedded.** A non-owner never gets the polyline
  today (owner-only `GetActivityStream`); federation keeps that — remote viewers
  get the summary + a link back, never the raw track. (Optional v3: a signed,
  zone-scrubbed, downsampled polyline behind the `map` category.)

---

## 7. Visibility & privacy mapping — the hard part

Cairn's model is **per-field** (`CategorySet` per audience: summary/map/location/
hr/power/…). ActivityPub addressing is **coarse**: an object is addressed `to`
some set of actors/collections (public, or your followers). Reconciling them:

1. **Federation only ever uses two audiences:** the activity is delivered either
   to `as:Public` (maps to Cairn's `public` audience) or to the actor's
   `followers` collection (maps to the `followers` audience). `link` and `owner`
   audiences never federate.
2. **The projected object is built through the EXISTING choke-point.** Reuse
   `view.Visibility.For` (domain.ResolveVisibility) against an anonymous
   viewer to compute the allowed `CategorySet`, then emit only those
   fields into `sport:summary`. An empty CategorySet ⇒ the object is **not
   federated at all** (no delivery). This guarantees federation can never leak
   more than a same-tier local viewer sees.
3. **Privacy zones apply identically.** `start_lat/lng/place` are run through
   `PointInAnyZone` before emit — a geofenced start is dropped, exactly as in the
   feed/projected paths. Same code, same guarantee.
4. **Per-remote-follower granularity is lost** (AP can't express "this follower
   sees HR, that one doesn't"). Federation therefore uses the **followers-audience
   default** for all remote followers — a deliberate, documented coarsening. A
   user who needs per-follower control keeps those followers local.
5. **`hidden_by_admin` / moderation** suppresses federation (no emit; inbound
   `Delete`/`Flag` honoured).

This makes the visibility story defensible: *federation is a transport, not a new
policy* — it rides the same resolver, so the blast radius equals the existing
public/followers projection, never more.

---

## 8. Delivery & processing (reuse the NATS async layer)

Federation is I/O-heavy (sign + POST to N remote inboxes, with retries) — a
perfect fit for the existing JetStream worker model.

**Outbound:** a social action publishes a `cairn.federation.deliver` job per
target inbox (deduped by shared-inbox where advertised). A federation worker (or
a server goroutine, like the notification retry processor) signs the request
(HTTP Signatures with the actor's key) and POSTs `application/activity+json`,
with capped-backoff retries — mirror the **notification delivery + retry** design
(`next_retry_at`, `failed_retryable`/`failed_permanent`, the same lease pattern).

**Inbound:** `POST /users/{username}/inbox` (and the shared `/inbox`):
1. Verify the HTTP Signature against the sender's fetched `publicKey` (cache keys).
2. Dedup on activity `id` (an `inbox_seen` table / NATS KV) — AP delivery is
   at-least-once.
3. Enqueue `cairn.federation.inbound`; a handler maps the activity to a Cairn
   usecase (Follow→create pending/accepted follow, Like→kudos, Create{Note}→comment,
   etc.), re-using the existing usecases + the blocks/moderation gates.
4. Respond `202 Accepted` fast; process async.

---

## 9. Security & abuse

- **HTTP Signature verification is mandatory** on every inbound activity; reject
  unsigned/invalid. Fetch + cache actor keys; re-fetch on `keyId` rotation.
- **Defederation:** an instance allowlist *or* blocklist (operator config) +
  per-domain block. Reuse the moderation model; a blocked domain's inbox POSTs
  are dropped pre-processing.
- **The existing `blocks` table extends to remote actors** — a user blocking a
  remote actor stops inbound activities from them and outbound delivery to them.
- **Rate-limit inboxes** per remote domain (reuse the IP/token-bucket limiter);
  cap object/collection sizes; bound fetch depth (no unbounded `inReplyTo` chains).
- **SSRF on actor/key/object fetches:** every outbound dereference (actor fetch,
  key fetch, object fetch) MUST go through the same dial-time IP guard added for
  webhooks (`isBlockedWebhookIP`) — remote-supplied URLs are attacker-controlled.
- **Key management:** per-user keypair, private key encrypted at rest with the
  existing `auth.SecretBox` (AAD `federation_key:<userID>`), rotatable via the
  existing `rotate-key` machinery.
- **Spam/flooding:** unsolicited inbound `Create` from non-followed actors is
  dropped (only followers' / mentioned content is accepted); `Flag` feeds the
  existing admin moderation queue.

---

## 10. Data model (new migrations)

- `federation_actors` — cached remote actors: actor_id (PK), inbox, shared_inbox,
  public_key_pem, preferred_username, domain, fetched_at.
- `federation_keys` — local per-user keypair (user_id, public_pem, private_encrypted, created_at).
- `remote_follows` — extends the local follow graph to remote actor ids on either
  side (or add nullable `remote_actor_id` columns to `follows`).
- `federation_inbound_seen` — activity_id dedup (or NATS KV with TTL).
- `federation_delivery` — outbound delivery audit + retry (mirror `notification_deliveries`).
- `federation_domain_policy` — allow/block per domain + defederation reasons.
- `instance_federation_settings` — instance opt-in, mode (allowlist/blocklist), shared-inbox on/off.
- `users.federation_enabled boolean` — per-user opt-in.

---

## 11. Hexagonal placement

- **domain/**: `FederatedActor`, `RemoteFollow`, `ActivityPubActivity` value types;
  the AS2 ↔ domain mapping is pure (like `protoconv`).
- **port/**: `FederationActorRepo`, `FederationKeyRepo`, `FederationDeliveryRepo`,
  `RemoteFetcher` (fetch actor/key/object — the SSRF-guarded HTTP client),
  `ActivityDeliverer` (sign + POST).
- **adapter/secondary/**: postgres repos; an `activitypub` HTTP client adapter
  (signing, fetching) reusing the webhook SSRF dial guard.
- **adapter/primary/**: a new HTTP surface — `/users/{username}` (actor),
  `/users/{username}/{inbox,outbox,followers,following}`, `/inbox`,
  `/.well-known/{webfinger,nodeinfo}`, `/nodeinfo/2.1`. Content-negotiated
  `application/activity+json`. Mounted like the other `mountXxx` handlers.
- **usecase/federation/**: `PublishActivity`, `ProcessInbound{Follow,Like,Note,…}`,
  `DeliverActivity`, `FetchActor`, all wired in `wire.go`. Inbound handlers call
  the EXISTING social usecases (follow/kudos/comment) so policy stays in one place.

The key architectural principle: **federation is a new primary+secondary adapter
pair around the existing domain + social usecases.** It adds transport and a
remote-actor concept; it does not fork the visibility, follow, or moderation
logic.

---

## 12. Phased implementation plan

1. **[DONE] Read-only actor + discovery.** WebFinger, NodeInfo, the `Person`
   actor doc, followers/following/outbox collections (public projections only).
   A remote server can *discover and view* a public Cairn user. Lowest risk.
2. **[DONE] HTTP Signatures + inbox.** Verify signatures, dedup, `202`. Accept
   `Follow`→`Accept` (auto for opted-in users) → write `federation_follows`;
   `Accept` marks our outbound Follow accepted; `Create` from a followed actor
   lands in the home feed. Outbound `Follow` via `POST /api/federation/follow`.
   (`internal/httpsig`, `cmd/server/federation*.go`.)
3. **[DONE] Workout `Create` outbound.** A federation-enabled user's fresh,
   public activities are pushed to followers' inboxes off the ingest pipeline's
   follow-ups (`internal/usecase/federation/publish.go`). Gated on: instance flag +
   user opt-in + `Privacy=public` + `!HiddenByAdmin` + recency (≤48 h, so a
   historical backfill doesn't blast followers) + action `imported_new`. The
   object carries summary + map snapshot only — never the GPS track (§7).
   Round-trip with the Phase-2 inbox parser is unit-tested. The outbox is a
   paged `OrderedCollection` of the user's public activities (same Create
   shape) so a new follower's server can backfill history. Delivery is durable:
   each (activity, inbox) push is a `federation_deliveries` row drained by a
   scheduler that signs + POSTs and retries transient failures (5xx / 429 /
   network) with capped exponential backoff (30 s → 6 h, 8 attempts), marking a
   permanent 4xx reject dead immediately (`internal/usecase/federation/publish.go`).
   Deleting an activity (last source detached → soft-delete) enqueues a `Delete`
   carrying the same object id to followers, so the federated copy is removed.
4. **Interactions both ways.** `Like`→kudos, `Create{Note}`→comment, plus the
   `Undo`/`Delete` inverses, all through existing usecases + gates.
   - *[DONE] Follow/feed lifecycle:* inbound `Undo{Follow}` drops the inbound
     edge (stops delivery fan-out); inbound `Delete{object}` removes that item
     from the recipient's federated feed, and an actor self-`Delete` removes
     all of that actor's items + both follow edges. Scoped to the signer so a
     remote can only remove its own content. (`internal/usecase/federation/inbox.go`.)
   - *[DONE] `Like`→kudos:* an inbound `Like` on a local public activity is
     recorded in a parallel `activity_remote_kudos` table (separate from
     `activity_kudos`, which FKs local `users`); the kudos read endpoint unions
     the counts + lists remote likers by `@user@domain`. `Undo{Like}` removes
     it. (Migration 49, `engagement_repo.go`.)
   - *[DONE] `Create{Note,inReplyTo}`→comment:* an inbound reply Note whose
     `inReplyTo` resolves to a local public activity is stored in a parallel
     `activity_remote_comments` table (body HTML-stripped, capped at
     `MaxCommentLength`); the comments read endpoint merges local + remote,
     time-sorted, attributing remote replies to `@user@domain`. `Delete{Note}`
     soft-deletes it. (Migration 50, `engagement_repo.go`.)

   Phase 4 is complete. Inbound `Like`/`Create{Note}` and their `Undo`/`Delete`
   inverses all land through the engagement read paths; remote actors are
   represented in parallel `activity_remote_{kudos,comments}` tables rather than
   forced into the local-user-FK'd originals.
5. **Moderation + defederation.** Domain allow/blocklist, `Flag`→admin queue,
   remote blocks, inbox rate-limits, SSRF-guarded fetches hardened.
   - *[DONE] Instance defederation (domain blocklist):* operator-managed
     `federation_blocked_domains` (admin CRUD at `/api/admin/federation/blocks`).
     Enforced on all three edges — inbound activities signed by a blocked domain
     are rejected `403` before the signer is even fetched, `fetchRemoteActor`
     refuses blocked domains (guards outbound Follow + actor dereference), and
     the delivery fan-out skips blocked inboxes. SSRF fetches were already
     guarded by `internal/safehttp`. (Migration 51, `federation_repo.go`.)
   - *[DONE] Inbox rate-limiting:* a per-domain in-memory token bucket
     (~120 activities/min, burst 240) bounds inbound work per remote instance
     before the expensive signer fetch; over-limit → `429 Retry-After`. Idle
     buckets are swept so forged-domain floods can't grow the map.
     (`InboxAllow` in `internal/adapter/primary/activitypub/handlers.go`, wired in `cmd/server/mount_federation.go`.)
   - *Remaining:* per-user remote-actor blocks, inbound `Flag`→admin queue.
6. **Polish.** Shared-inbox fan-out, key rotation UI, `Update{Workout}`,
   followers-collection paging, NodeInfo counts.

Ship 1–2 behind the instance flag as an alpha; gate 3+ on real interop testing
against a Mastodon instance and a second Cairn.

---

## 13. Decisions needed from the operator before building

- **Actor handle policy:** lock `username` once federated, or implement `Move`
  aliases from day one? (Recommend: lock in v2, `Move` in v3.)
- **Default federation mode:** allowlist (closed, safe) vs blocklist (open,
  Mastodon-like)? (Recommend: allowlist for a fitness instance — smaller, safer.)
- **What of a workout is *ever* allowed off-instance?** Confirm the §7 rule
  (summary + link, never the GPS track) is the privacy bar you want.
- **Signature key type:** RSA-2048 (max interop) vs Ed25519 (modern, smaller)?
  (Recommend: RSA-2048 for Mastodon interop; revisit.)
- **Inbound `Create` from strangers:** drop entirely (recommended) or hold for
  moderation?

Once these are decided, Phase 1 (read-only actor + WebFinger) is a self-contained,
low-risk first PR.

---

## 14. Interop testing

Federation spans two instances, so the unit tests can't prove it alone. Two
complementary layers:

- **Wire protocol (CI, no infra)** — `internal/adapter/secondary/activitypub/interop_test.go`
  drives the cross-instance path that's most likely to break against another
  implementation: a simulated remote serves its actor + public key over real
  HTTP, signs an activity, and the receiving side runs the exact inbound auth
  steps (keyId → fetch actor → parse key → `httpsig.Verify`) plus the object →
  feed-item parse. Both directions; runs in `go test ./...`.

- **Full DB-backed round-trip (manual)** — `docker-compose.interop.yml` +
  `scripts/federation-interop.sh` stand up two real Cairn instances (A + B,
  own Postgres each) on a Docker network and assert: discovery (WebFinger +
  actor), the Follow → auto-Accept handshake (checked on both DBs), a public
  activity on B appearing in B's outbox for A to backfill, and defederation.
  Requires `CAIRN_FEDERATION_ALLOW_PRIVATE_HOSTS=true` (the peers sit on private
  Docker IPs the SSRF guard would otherwise block — that flag is also what a
  LAN self-hoster needs).

**Publish triggers (two paths).** A new activity defaults to `privacy=private`,
so the ingest-time push (`pipeline.Pipeline.IngestSource`, gated on a fresh import + ≤48 h
recency) only reaches followers for activities public *at import*. The common
"upload private, then share" flow is covered by the second path: the Connect
`UpdateActivity` edit path injects a `port.FederationPublisher` and, on a
privacy transition, publishes a `Create` when an activity becomes public (any
age — an explicit share is intentional, so no recency gate) and a `Delete` when
it leaves public. The recency gate therefore lives only at the ingest call
site, not inside `publishActivityCreate`.

*Still open:* `Update{Workout}` for non-privacy edits (title/description changes
don't re-notify), and the same publisher isn't yet hooked into the REST manage
path (only the soft-delete propagation is).
