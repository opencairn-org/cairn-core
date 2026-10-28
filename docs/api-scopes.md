# API scopes

OAuth 2.1 access tokens (`cairn_at_*`) carry a space-separated scope set. Cookie
sessions and personal access tokens are not scoped: they carry the user's full
access. This page is the contract for scoped tokens; every `rpc` in
`api/proto/cairn/v1` repeats its line as a `// Scope:` comment.

## Catalog

Every family has a read and a write scope. A read RPC accepts `family:read` or
`family:write`; a mutating RPC needs `family:write`.

| Family | Covers |
|---|---|
| `activities` | ActivityService: activities, sources, streams, uploads, records |
| `gear` | GearService |
| `notifications` | NotificationService: list, mark read, preferences, webhooks |
| `connections` | ExternalAccountService: provider connections, sync status |
| `segments` | SegmentService |
| `training` | MetricsService: training load, best-effort history, health and athlete metrics |
| `profile` | SettingsService and the AuthService profile/preferences RPCs |
| `social` | follows, feed, kudos, comments, clubs (REST today, SocialService later) |

The default grant for a dynamically registered client is every `:read` scope
(`domain.ReadOnlyScopes`). `scopes_supported` in the authorization-server
metadata and `GetInstanceInfo.supported_scopes` list the catalog.

## Connect RPCs

Read or write is decided by the method name: a name that starts with `Get`,
`List`, `Search`, `Watch`, `Stream`, `Export`, `Resolve`, `Check`, `Count`,
`Lookup` or `Fetch` is a read, everything else is a write. New RPC names must
follow that rule.

| Policy | RPCs |
|---|---|
| none (public) | `AuthService.GetLoginMethods`, `GetInstanceInfo`, `LoginPassword`, `Complete2FA`, `Register`, `RequestPasswordReset`, `ConfirmPasswordReset`, `StartOIDCLogin` |
| any valid credential | `AuthService.GetCurrentUser`, `GetQuota` |
| cookie session only (OAuth tokens refused) | `AuthService.Logout`, `ChangePassword`, `ListSessions`, `RevokeSession`, `CreatePersonalAccessToken`, `ListPersonalAccessTokens`, `RevokePersonalAccessToken`; every `AdminService` RPC |
| `profile:read` / `profile:write` | `AuthService.ListLinkedIdentities` / `UpdateProfile`, `UpdateUserPreferences`, `StartOIDCLink`, `UnlinkOIDCIdentity`; `SettingsService` |
| `activities:read` / `activities:write` | `ActivityService` reads / writes |
| `gear:read` / `gear:write` | `GearService` reads / writes |
| `notifications:read` / `notifications:write` | `NotificationService` reads / writes |
| `connections:read` / `connections:write` | `ExternalAccountService` reads / writes |
| `segments:read` / `segments:write` | `SegmentService` reads / writes |
| `training:read` / `training:write` | `MetricsService` reads / writes |

A service without a family is closed to OAuth tokens until it gets one.

## REST (`/api/*`)

REST handlers only distinguish read from write for OAuth tokens: a token with
no `:write` scope may use `GET`/`HEAD` only; any `:write` scope unlocks the
mutating methods. Family checks apply to Connect only. Cookie sessions and PATs
are unrestricted on both surfaces.

## MCP tools

`list_activities`, `get_activity`, `activity_stats`, `personal_records` need
`activities:read`; `profile` needs `profile:read`.
