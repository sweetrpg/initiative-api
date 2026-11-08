# Contract fixtures

JSON the API returns for each resource and the error classes a client can see. Clients (web,
desktop, iOS, Foundry module) load these as test fixtures; `domain/contract_test.go` fails when the
service output stops matching them.

- `GENERATED_ID` stands in for a server-assigned ID, and timestamps are fixed; both vary per run.
- `error_classes.json` lists each scenario with its class (`not_found`, `forbidden`,
  `revision_conflict`, `invalid`, `invalid_health` with the offending `key`, `invalid_transition`).
  The class-to-HTTP-status mapping belongs to the handlers and is not pinned here yet.
- After an intentional contract change, run `UPDATE_FIXTURES=1 go test ./domain -run Contract` and
  review the diff.
