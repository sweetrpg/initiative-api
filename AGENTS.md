# AGENTS.md

Guidance for AI coding agents working in this repository. `CLAUDE.md` is a symlink to this file.

## About This Project

`initiative-api` is the Go API behind Initiative!, the SweetRPG initiative tracker. It owns all
tracker rules; clients are presentation only. It follows `docs/go-service-conventions.md` in
`sweetrpg/platform`. Depends on `api-core.go` (tracing, status payloads) and `common.go`
(logging).

PADR-0037: no service tokens. Authenticate callers with the user's bearer token only.

## Committing Code

Conventional Commits: `<type>(<scope>): <description>`. Never mark a commit BREAKING; versions
stay 0.x until platform GA.

## Branches and Workflow

`master` + `develop` git-flow (`docs/git-flow.md` in platform). Branch from `develop` as
`feature/*` or `fix/*`, open a PR into `develop`, merge with a merge commit (no squash).

## Running Checks Locally

```bash
make lint   # golangci-lint run ./...
make test   # go test -race -cover ./...
make build
```

## Platform Conventions and Decisions

This repo is a submodule of `sweetrpg/platform`. Platform-wide conventions and Architecture
Decision Records live there:

- checked out inside the platform tree: `../../docs/README.md` (convention index) and
  `../../docs/adr/README.md` (platform ADRs, `PADR-*`)
- standalone or module-only checkout:
  <https://github.com/sweetrpg/platform/tree/master/docs> and
  <https://github.com/sweetrpg/platform/tree/master/docs/adr>

Accepted `PADR-*` records are binding constraints. Read the ADR index before proposing a
structural change; if a task needs to contradict an accepted record, stop and say so -
propose a superseding ADR rather than working around it. This repo's own service-local
decisions are `ADR-*` in `docs/adr/` here. `/adr "<title>"` scaffolds one.
