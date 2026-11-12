
## 0.1.0 - 2026-11-12

### Added
- Add Go service skeleton with config, logging, tracing and status
- Add bearer-token auth middleware on a protected route group
- Add collections, models and indexes for rosters, themes, systems, credentials
- Add owner-scoped repository with audit fields and revision compare-and-set
- Push filter, sort, projection, limit and cursor into the query layer
- Add idempotent game-system seed loader with the six tracker families
- Add encounters collection with owner-scoped access
- Add roster and character rules with roster/encounter transfer
- Add encounter and combatant rules, status transitions, and reset
- Add ordering and tie-break engine, turn advance, and condition expiry
- Bind flag fields to 0/1, fall back to No System, check only changed health
- Replace the template manifests with the dev deployment
- Add entitlement client with TTL cache and fail-closed/open policy
- Add theme CRUD with entitlement slot limit and read-only packaged themes
- Add read-only entitlements endpoint with usage counts
- Integration credentials with scoped auth and external_refs lookup
- Revoke an owner's integration credentials on auth deny or role removal
- Admin endpoint to reopen credentials after a retention-gap close
- Inactive state for items beyond a reduced limit
- Lock encounters that use a gated system without the entitlement
- Set NATS_URL for dev overlay
- Wire initiative-api NATS user


### Documentation
- Add coverage badge


### Fixed
- Use ClusterIP instead of LoadBalancer for api Service (#1)
- Restrict updatable fields, version soft deletes, and close race and test gaps
- Map unique-index rejections to ErrConflict and retry concurrent system inserts
- Scope ErrConflict to the theme slot and surface non-not-found read errors
- Validate encounters and keep combat state out of Update
- Create encounters as fresh drafts and reject duplicate combatant IDs
- Return 403 for other owners' roster edits and test map aliasing
- Classify reset misses, validate combatant health, clear queue on reset
- Validate combat state in SetState and merge partial health patches
- Start the next round when the removed current combatant was last
- Check health on a settings-only game system switch
- Require a defined game system when choosing one
- Order No System encounters by initiative only
- Bound stale values served under FailOpen
- Satisfy errcheck and staticcheck
- Accept only ObjectID-form record IDs in query filters
- Look up external_refs by a derived key
- Revoke on a deny only when it names initiative-api
- Build with Go 1.26.10 base image


# Changelog

## [Unreleased]

- Replace the Python scaffold with a Go service skeleton: config validation, logging with
  correlation IDs, tracing, `/status/ping` and `/status/health`.
- Add CI, release, dependabot, community docs, and ADR scaffolding per `repo-setup-standard`.
