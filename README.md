# initiative-api

[![License](https://img.shields.io/github/license/sweetrpg/initiative-api.svg)](LICENSE)
[![Issues](https://img.shields.io/github/issues/sweetrpg/initiative-api.svg)](https://github.com/sweetrpg/initiative-api/issues)
[![PRs](https://img.shields.io/github/issues-pr/sweetrpg/initiative-api.svg)](https://github.com/sweetrpg/initiative-api/pulls)
[![Coverage](https://img.shields.io/endpoint?url=https://sweetrpg.github.io/initiative-api/coverage-badge.json)](https://sweetrpg.github.io/initiative-api/coverage.html)

Go API behind Initiative!, the SweetRPG initiative tracker. All tracker rules live here; clients
are presentation only (PADR-0033, proposed). Design and task list:
`openspec/changes/initiative-api` in [sweetrpg/platform](https://github.com/sweetrpg/platform).

Status: skeleton. Config validation, logging, tracing, and `/status/*` are in place; auth,
data layer, and domain routes are not.

## Running

```bash
cp .env.example .env
make run
```

| Variable | Default | Notes |
| --- | --- | --- |
| `BIND_ADDRESS` | `:8000` | `host:port` |
| `ENV` | `dev` | |
| `LOG_LEVEL` | `WARN` | `DEBUG`, `INFO`, `WARN`, `ERROR` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | unset | absolute URL of the trace collector |
| `AUTH_API_URL` | required | auth-api base URL; tokens are verified via `/authz/check` |
| `USERS_API_URL` | required | users-api base URL; resolves the caller to `users._id` |
| `ENTITLEMENTS_API_URL` | required | entitlements-api base URL; the caller's bearer is forwarded on checks |
| `DB_OPTS` / `DB_URI` query | driver timeouts added | `serverSelectionTimeoutMS=5000`, `connectTimeoutMS=5000` and `socketTimeoutMS=30000` are added when the connection settings do not set them |
| `ALLOWED_ORIGINS` | unset | comma-separated; unset disables CORS, `*` is rejected |
| `SESSION_MEMBERS_MAX` | `20` | maximum of a session's member limit; an entitled `members_per_session` above it has no effect; positive integer |
| `REQUEST_TIMEOUT` | `10s` | deadline for every request except the event stream upgrade; past it a store call fails and the answer is 503 `timeout` with `Retry-After`; 1s-1m |
| `STREAM_ALLOWED_ORIGINS` | unset | comma-separated host patterns a browser `Origin` may match to open an event stream (no `Origin` is always allowed; `*` is rejected); CORS `ALLOWED_ORIGINS` still applies first, so a cross-origin stream client must be in both lists |
| `STREAM_TICKET_TTL`, `STREAM_MAX_AGE`, `STREAM_HEARTBEAT` | `30s`, `1h`, `20s` | stream ticket lifetime, maximum connection age, heartbeat and membership re-check interval; Go durations: ticket TTL 1s-2m, max age 1m-24h, heartbeat 1s-1m (it is also how often a stream re-checks membership and credentials) |
| `CREDENTIAL_EVENTS_MAX_DISCONNECT` | `10m` | how long NATS may be unreachable before integration credentials answer 503 (the rest of the API keeps serving); they reopen after the consumer reconnects and catches up; 30s-1h |
| `STREAM_CONCURRENT_MAX`, `STREAM_CREDENTIAL_MAX`, `STREAM_MAX_PER_POD` | `10`, `3`, `1000` | most event streams one user may hold across all pods (429 `too_many_streams` with `Retry-After`), most one integration credential may (its own count; it never uses up its owner's), and most one pod serves (close code 1013, ticket not spent) |
| `STREAM_LEASE_TTL`, `STREAM_GRACE` | `60s`, `30s` | how long a stream's seat lasts without a renewal (at least twice `STREAM_HEARTBEAT`), and how long a dropped client keeps it for a reconnect; seats are compared across pods by their clocks, so keep node clocks in sync. If the seat store is down, new streams get 503 `streams_unavailable` and open streams stay up (seats lapse after the TTL) |
| `OWNER_ENTITLEMENTS_MAX_AGE` | `168h` | how long a recorded owner entitlement serves integration credentials; positive Go duration |
| `REDIS_HOST`, `REDIS_PORT`, `REDIS_PASS` | `REDIS_HOST` required | rate-limit counters; the service stops at startup without `REDIS_HOST` |
| `RATE_LIMIT_STANDARD`, `RATE_LIMIT_STANDARD_WINDOW_SECONDS`, `RATE_LIMIT_CHEAP`, `RATE_LIMIT_CHEAP_WINDOW_SECONDS` | api-core defaults | per-client budgets (PADR-0036) |

Invalid values stop the process at startup with every problem listed.

## Endpoints

- `GET /status/ping`: shallow liveness and readiness probe.
- `GET /status/health`: deep dependency check (database); 503 while one is down. Not used by the probes (PADR-0017).
- `GET /themes`, `GET /themes/packaged`, `GET /themes/{id}`, `POST /themes`,
  `PATCH /themes/{id}` (body carries `revision`), `DELETE /themes/{id}?revision=N`. Custom themes
  take the lowest free slot below `initiative.theme_slots`; packaged themes (`pkg-` IDs) are
  read-only. Errors are `{code, message, correlation_id, key?, limit?}`.
- `PUT /entitlements/active/{themes|rosters|encounters|credentials}` (body `ids`): choose which items stay active when a limit is below the number owned.
  Over-limit items go inactive (`inactive_at`), never deleted: readable and deletable, not editable (403 `item_inactive`),
  and an inactive credential stops authenticating. Limits are re-read on the owner's own requests (at most every 30 s).
- Without the `initiative.game_systems_extra` flag, encounters using a game system other than No System are locked (`locked_at`):
  readable and deletable, not editable or runnable (403 `entitlement_required`), and unlocked unchanged when the flag returns.
  Creating or switching to a gated system without the flag gets the same error.
- `POST /credentials` (body `label`, `scopes`; the secret is returned once), `GET /credentials`,
  `POST /credentials/{id}/revoke`. Integrations send the secret as `Authorization: Bearer ic_...`;
  it acts as its owner with the user role, `read` allows GET and `write` allows other methods.
  Credentials cannot reach `/credentials`, `/entitlements` or `/themes`.
  A deny or role removal in auth-api ends the owner's credentials (PADR-0039): the service consumes
  the `AUTH_EVENTS` stream (`NATS_URL`, optional `NATS_DURABLE_NAME`, `NATS_CREDS` or
  `NATS_USER`/`NATS_PASSWORD`). Credentials answer 503 `credentials_unavailable` until the consumer
  has caught up, and stay off if `NATS_URL` is unset or the consumer fell behind the stream's
  retention. `POST /admin/credentials/reopen` (admin bearer only) clears that close and logs an audit
  entry; the gate records the actor.
  Encounter events are also published to the `INITIATIVE_EVENTS` stream on
  `initiative.events.<entity>.<action>` (`NATS_URL`, same login), fail-open: a broker outage drops
  the event with a log line and never fails the write. `Nats-Msg-Id` is the event id.
  Addressed notices are also published, one event per unmuted addressee, on
  `initiative.user.<user_id>.<type>` (`turn.yours` stays two tokens: `initiative.user.u1.turn.yours`),
  with `user_id` in the envelope, for a future push service.
  Administrator routes (admin bearer only, no credentials): `GET /admin/credentials[?owner=]` lists
  any owner's credentials (never the secret or hash); `DELETE /admin/credentials/{id}` with
  `{"reason": "..."}` removes one for good, at once; `GET /admin/credentials/deletions[?owner=]`
  lists the audit entries (paged with `limit` and `cursor`). A 500 `deletion_incomplete` means retry the same request. The entry (administrator, owner, credential id, label, scopes, reason,
  time) goes to `integration_credential_deletions` before the credential is removed, and has no
  update or delete path. `Credentials.Live(id)` is what an open stream re-checks.
  Delete class (PADR-0027): `integration_credentials` are soft delete for the owner's own removal
  (revoke) and hard delete only through the administrator route above.
- Sessions (user bearer only, no integration credentials): `POST /sessions`, `GET /sessions`,
  `GET|DELETE /sessions/{id}` (delete is the GM's, with `?revision=`), `POST /sessions/{id}/leave`.
  Members: `GET /sessions/{id}/members`, `PATCH|DELETE /sessions/{id}/members/{user}`. Invites:
  `POST|GET /sessions/{id}/invites` (the code is returned once, only its hash is stored),
  `DELETE /sessions/{id}/invites/{invite}` revokes. Joining: `POST /sessions/join {code}` makes a
  pending request that grants nothing; `GET /sessions/{id}/requests`, `POST .../requests/{id}/approve
  {role}` or `/reject {reason}`. Blocks: `GET|POST /sessions/{id}/blocks`, `DELETE .../blocks/{user}`.
  Notifications: `turn.yours`, `turn.next` and the change-request events (`change_request.submitted`
  to the GM and co-GMs, `.approved` / `.rejected` to the submitter) are delivered only to the
  addressee's open streams of that session. `GET|PUT /sessions/{id}/notification-preferences
  {"muted": [...]}` mutes types per member; state events are unaffected.
  Roles are `gm` (the creator, exactly one), `co_gm`, `player`, `observer`; `domain.Allowed` is the
  one role table. Membership collections are the source of truth for who holds a role. Membership,
  invite, request and block records and the session are all soft-deleted (`deleted_at`, `deleted_by`;
  PADR-0027), and a `live` marker lets the unique indexes ignore deleted rows.
  Limits follow the GM: creating a session checks `initiative.sessions_per_gm` (403
  `entitlement_limit_reached`, 503 when entitlements are down); sessions beyond a reduced limit go
  inactive (readable, deletable, leavable; no invites, approvals, or joins; 403 `item_inactive`). Each
  GM request re-reads `initiative.members_per_session` with the GM's token and records it on the
  session; member checks use that record only (503 `owner_entitlements_pending` until one exists).
- `GET /combatants?ref_key=&ref_value=`: the caller's combatants whose `external_refs` match.

OpenFGA session tuples are rebuilt from the membership collection by `cmd/reconciler` (a CronJob every 5 minutes
running as the `initiative-reconciler` user with the `openfga-writer` role; PADR-0041 covers how it signs in). For each session it reads the
tuples first and then that session's current members, so a grant or revoke between the two reads is at worst
corrected by the next pass (PADR-0040). After the tuple pass it closes session mints older than 15 minutes
whose session is missing or soft-deleted in Mongo (auth-api `GET /internal/openfga/session-mints`, paged by
`after`/`next`, then `POST .../{id}/close`); a failed sessions lookup skips that batch until the next pass. A live session with no live GM record is soft-deleted as the system, with its membership records
(records first, session last, so a failed pass is retried), and its mint closed. Nothing in this repo removes a
live GM record today, so this only reaps double-failure orphans (`Open` stored the session but neither its GM
record nor its own removal) and sessions an operator stripped of a GM; the latter lose their co_gm and player
data 15 minutes later. One extra `$in` query per mint page finds them, covered by the partial `session_user` index
(`session_id` prefix, `live` exists). A pass stops after removing 10 and fails the job once (a wrong database or an
empty members collection would otherwise wipe every old session; earlier pages may already have removed up to 10).
A session younger than 15 minutes, or one that gained a GM, is left alone; a failed query leaves live sessions alone
and fails the job. The manifests are in
`kubernetes/overlays/dev/reconciler.yaml`; `reconciler-alert.yaml` alerts when no run has succeeded in 15 minutes
(token expired, job failing, or CronJob suspended).

The reconciler signs in to auth-api as an Auth0 machine-to-machine client: `RECONCILER_CLIENT_ID`, `RECONCILER_CLIENT_SECRET`,
`RECONCILER_TOKEN_URL` (https) and `RECONCILER_AUDIENCE` fetch a client-credentials token, cached in Redis as `reconciler:token`
until 120 seconds before it expires. A 401 from auth-api clears the cache, fetches once more and retries once. Exit 1 is a failed
pass or an Auth0 or Redis failure, 2 missing or invalid configuration, 3 auth-api rejected a token. A 401 on a token fetched in the
same run sets `reconciler:rejected` for an hour so a persistent 401 cannot spend the Auth0 token allowance; fix the cause, then
`DEL reconciler:rejected` in the initiative cache to retry at once.

A failed Auth0 token request (429, 5xx, 401/403 from Auth0, network error, malformed answer) backs off further fetches: 5, 10,
20, 40, then 60 minutes (or Auth0's `Retry-After` on a 429 when longer, at most 60), tracked in `reconciler:auth0-failures`
(2 hour TTL, cleared by a successful fetch) and `reconciler:auth0-backoff`. While it exists a run fails with exit 1 without
calling Auth0, but a cached token is still used. After the cause is fixed, `DEL reconciler:auth0-backoff reconciler:auth0-failures`
to retry at once.

Requests are rate limited per client (PADR-0036): the `X-API-Key` header, else the client IP, and an `ic_` integration
credential counts as its own client. Over the budget is 429 with `Retry-After`; if Redis is down, `/status/*` still
answers and every other route returns 503.

Every response carries `X-Correlation-ID` (the caller's value, or a generated one), also written
to the access log as `correlation_id`.

## Checks

```bash
make lint test
```
