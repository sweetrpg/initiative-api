# Architecture Decision Records

An ADR records one architectural decision: the context, the options weighed, what was chosen,
and what that costs. It answers **why**, where a spec or the code answers **what**.

Accepted ADRs are binding constraints, not background reading. If a task requires contradicting
one, stop and say so — propose a superseding ADR instead of quietly working around it.

## Two tiers

| Tier | Prefix | Lives in | Scope |
| --- | --- | --- | --- |
| Platform | `PADR-NNNN` | this repo, `docs/adr/` | binding on more than one service |
| Service-local | `ADR-NNNN` | that service repo's `docs/adr/` | that service only; dies with it |

Number sequentially within each tier. A reference is always unambiguous about which tree it is
in because the prefix differs.

## Rules

- **One decision per file. Written once, never rewritten.** When a decision changes, write a new
  ADR and mark the old one `superseded` — do not edit its Context, Options, Decision, or
  Consequences. The `0003 -> superseded by 0009` chain is the record of how the design
  converged, and is often the most useful thing a new contributor reads.
- **Immutability applies to accepted and superseded ADRs.** A `proposed` ADR may be revised
  until it flips to `accepted` or `rejected`.
- **Rejected options are mandatory.** An ADR that justifies only the chosen path has discarded
  most of its value. Write what was not taken and the concrete reason.
- **Keep the count low.** The trigger is *cost to reverse*, not importance. One-way doors get an
  ADR; anything undoable in an afternoon does not. An ADR about a logging library means the bar
  has slipped. Expect roughly one every week or two, most under a page.
- **`rejected` ADRs stay on disk.** A decision considered and declined is worth keeping so it is
  not relitigated in three months.

## Statuses

- `proposed` — made at less than full confidence, not yet proven against the code.
- `accepted` — in force. Binding.
- `accepted (reconstructed)` — backfilled after the fact. Carries a `confirmed_by: <name>,
  <date>` frontmatter line, or states in Consequences that the original rationale was not
  recovered. Never present a reconstructed rationale as if recovered from evidence.
- `superseded` — replaced. `superseded_by` set in frontmatter; `Superseded by PADR-NNNN.` at the
  top of the body.
- `rejected` — considered and declined.

## Frontmatter

```yaml
id: PADR-0031
title: Use NATS as the platform message bus
status: accepted
date: 2026-09-01
scope: platform          # or a service name for an ADR-NNNN
supersedes: PADR-0007     # omit if none
superseded_by:            # set when this ADR is retired
tags: [messaging, async]
affected_repos: [catalog-api, game-room-api]
confirmed_by: Paul Schifferer, 2026-09-01   # required for accepted (reconstructed)
```

## Writing one

- `/adr "<title>"` grabs the next number, copies the template, sets status `proposed`, and adds
  a row to the index below.
- The natural moment to write one is the end of a design conversation, while the reasoning is
  still live — ideally in the same PR as the change it governs, so it is never a separate act of
  virtue.
- A decision that starts life in an OpenSpec change's `design.md` and turns out to constrain
  future work gets promoted to an ADR before that change archives. See `docs/openspec.md`.

## Review

Code review includes a pass checking the diff against accepted ADRs and flagging contradictions.
A contradiction is not a review nit to argue down — it needs a superseding ADR or a changed diff.

## Index

This repo holds service-local `ADR-NNNN` records. Platform-wide `PADR-NNNN` records live in
[`sweetrpg/platform`](https://github.com/sweetrpg/platform/tree/master/docs/adr).

| ID | Title | Status | Decision (one line) | Date |
| --- | --- | --- | --- | --- |
| [0000](0000-template.md) | ADR template | template | — | 2026-09-01 |
