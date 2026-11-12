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
| `ALLOWED_ORIGINS` | unset | comma-separated; unset disables CORS, `*` is rejected |

Invalid values stop the process at startup with every problem listed.

## Endpoints

- `GET /status/ping`: shallow liveness and readiness probe.
- `GET /status/health`: dependency check (no dependencies yet).
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
  Roles are `gm` (the creator, exactly one), `co_gm`, `player`, `observer`; `domain.Allowed` is the
  one role table. Membership collections are the source of truth for who holds a role. Membership,
  invite, request and block records are hard-deleted (relationship edges); the session is soft-deleted.
- `GET /combatants?ref_key=&ref_value=`: the caller's combatants whose `external_refs` match.

Every response carries `X-Correlation-ID` (the caller's value, or a generated one), also written
to the access log as `correlation_id`.

## Checks

```bash
make lint test
```
