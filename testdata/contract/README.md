# Contract fixtures

JSON the API returns for each resource and the error classes a client can see. Clients (web,
desktop, iOS, Foundry module) load these as test fixtures; `domain/contract_test.go` fails when the
service output stops matching them.

- `GENERATED_ID` stands in for a server-assigned ID, and timestamps are fixed; both vary per run.
- `error_classes.json` lists each scenario with its class (`not_found`, `forbidden`,
  `revision_conflict`, `invalid`, `invalid_health` with the offending `key`, `invalid_transition`).
  The class-to-HTTP-status mapping is pinned in `http_errors.json`.
- `http_errors.json` lists each HTTP error scenario with its status and body
  (`server/contract_test.go`; regenerate with `UPDATE_FIXTURES=1 go test ./server -run Contract`).
  Every error is `{"code", "message", "correlation_id"}` plus `key`/`limit`/`current` where they
  apply, including the token middleware's 401 (`unauthorized`), 403 (`forbidden`) and 503
  (`authz_unavailable`, `user_resolution_unavailable`) and the rate limiter's 429 (`rate_limited`)
  and 503 (`rate_limit_unavailable`). `server/typederrors.go` rewrites the shared libraries'
  `{"error", "message"}` bodies.
- After an intentional contract change, run `UPDATE_FIXTURES=1 go test ./domain -run Contract` and
  review the diff.
