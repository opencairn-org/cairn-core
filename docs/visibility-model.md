# Per-field visibility model (multi-user v1)

The foundation for every multi-user feature (feed, public profiles, share links,
kudos/comments). The operator chose a **richer, per-field** model over the
3-state `ActivityPrivacy` enum: a viewer's access to another user's activity is
resolved **per data category**, not all-or-nothing.

Design first; build the resolver before any non-owner read path.

## Audiences

A viewer relates to an activity's owner in exactly one of these ways
(most-privileged wins):

| Audience | Who |
| --- | --- |
| `owner` | the activity's owner — sees everything, always |
| `link` | a holder of a valid, unrevoked share token for this activity |
| `followers` | a viewer the owner has accepted as a follower |
| `public` | anyone (logged-in or not) |

Note `link` is **orthogonal** to public/followers: a share link grants access
even when the activity is otherwise private. That's the point of sharing.

## Categories

Each category can be independently allowed/denied per audience. Start with this
set (extensible):

| Category | Covers |
| --- | --- |
| `summary` | type, title, date, distance, moving/elapsed time, elevation, calories |
| `map` | GPS track / route polyline (subject to privacy zones, below) |
| `location` | start place name + start/end coordinates |
| `hr` | heart-rate stream + avg/max HR |
| `power` | power stream + avg/max/NP |
| `cadence` | cadence stream |
| `pace_speed` | speed/pace stream + avg/max |
| `segments` | segment efforts + leaderboard placement |
| `best_efforts` | best-effort curves |
| `zones` | time-in-zone |
| `social` | who can kudos/comment (gate, not data) |

A denied category is **omitted** from the response (not nulled) so a client
can't infer values.

## Policy

Resolution = **per-user defaults**, overridable **per-activity**.

```
VisibilityPolicy {
  // audience → set of allowed categories
  default: map[Audience]CategorySet     // user-level default
}
ActivityVisibility {
  override: map[Audience]CategorySet     // optional per-activity, merged over default
  shared:   bool                         // any active share token exists
}
```

Recommended **defaults** (sane, privacy-leaning):

- `public`: `summary` only (everything else hidden) — a stranger sees you did a
  90-minute ride, not where or your HR.
- `followers`: `summary, map(zoned), pace_speed, segments, best_efforts, zones`
  — friends see the workout but not necessarily HR/power unless you opt in.
- `link`: same as `followers` by default (an explicit share is a trust signal),
  tunable per share.
- `owner`: all categories.

These are **defaults**; the per-user settings UI lets an athlete open up
(e.g. make HR public) or lock down (e.g. hide map from followers).

## Privacy zones

Independent of audience: the `map` and `location` categories are further masked
near the owner's configured private locations (home, work). A zone is a
(lat, lng, radius) the owner sets. When `map`/`location` would be shown to a
non-owner, GPS points inside any zone are dropped and the start/end are snapped
to the zone edge — so a public ride doesn't reveal the owner's front door.

Zones never apply to the `owner` audience.

## Resolution

```
ResolveVisibility(viewer, owner, activity, follow, shareToken) -> AllowedCategories

  1. owner?            → all categories (+ raw map, no zone masking)
  2. deleted / hidden? → nothing
  3. valid shareToken? → audience = link (opens even a private activity)
  4. follow active?    → audience = followers
  5. else              → audience = public
  6. activity.privacy: private → nothing; followers → nothing unless
     audience = followers; public (or a link) → continue. Unknown → private.
  7. categories = activity.override[audience] ?? owner.default[audience]
  8. if 'map'/'location' allowed and audience != owner: apply privacy zones
  9. return categories
```

Steps 1–7 are `domain.ResolveVisibility`; `view.Visibility.Resolve` (and
`For`, which takes the activity) is the only way the read paths ask. SQL
filters on `privacy`/`hidden_by_admin` are page pre-filters, never the
decision. Cross-user aggregates follow the same rule per row: a segment
leaderboard ranks only efforts whose activity grants the viewer `segments`;
federation sends what the anonymous public audience may see. Non-owners only
ever get re-encoded photo variants: the original keeps its EXIF (GPS).

Every non-owner read path (feed item, public profile activity, shared-activity
view, and a non-owner viewing the normal activity detail) projects the activity
through `AllowedCategories` before serialising. One choke point — a
`projectActivity(activity, allowed)` helper — so no endpoint can leak by
forgetting to filter.

## Schema

- `user_visibility_defaults(user_id PK, policy jsonb)` — the per-audience
  category sets. Absent row → the hardcoded defaults above.
- `activities.visibility_override jsonb NULL` — per-activity override merged over
  the user default. (Add a column; cheap.)
- `privacy_zones(id, user_id, lat, lng, radius_m, label, created_at)` — the
  owner's masked areas.
- Share tokens + follows live in their own tables (#87, #84).

`policy` jsonb shape:

```json
{
  "public":    ["summary"],
  "followers": ["summary","map","pace_speed","segments","best_efforts","zones"],
  "link":      ["summary","map","pace_speed","segments","best_efforts","zones"]
}
```

## Build order

1. Domain: `Category`, `Audience`, `VisibilityPolicy`, `ResolveVisibility`,
   `ProjectActivity` (pure; unit-tested with a table of viewer×category cases).
2. Schema: the three tables/column above (one migration).
3. Repos: `VisibilityRepo` (get/set user defaults, get/set activity override,
   list privacy zones).
4. Wire the resolver into the existing non-owner read paths as they're built:
   feed (#85), public profile (#86), share link (#87), and the activity-detail
   Connect/REST responses when `viewer != owner`.
5. Settings UI: per-audience category toggles + privacy-zone management.

Until the multi-user read paths exist, the resolver is dormant (the only reader
today is the owner, who always sees everything) — so this can land safely ahead
of #84–#88.
