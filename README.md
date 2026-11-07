# initiative-api

[![License](https://img.shields.io/github/license/sweetrpg/initiative-api.svg)](LICENSE)
[![Issues](https://img.shields.io/github/issues/sweetrpg/initiative-api.svg)](https://github.com/sweetrpg/initiative-api/issues)
[![PRs](https://img.shields.io/github/issues-pr/sweetrpg/initiative-api.svg)](https://github.com/sweetrpg/initiative-api/pulls)

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
| `ALLOWED_ORIGINS` | unset | comma-separated; unset disables CORS, `*` is rejected |

Invalid values stop the process at startup with every problem listed.

## Endpoints

- `GET /status/ping`: shallow liveness and readiness probe.
- `GET /status/health`: dependency check (no dependencies yet).

Every response carries `X-Correlation-ID` (the caller's value, or a generated one), also written
to the access log as `correlation_id`.

## Checks

```bash
make lint test
```
