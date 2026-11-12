---
id: PADR-0000
title: ADR template
status: template
date: 2026-09-01
scope: platform
supersedes:
superseded_by:
tags: [meta]
affected_repos: []
confirmed_by:
---

# PADR-0000: ADR template

Copy this file to `PADR-NNNN-short-kebab-title.md` (platform-wide decision, this repo) or
`ADR-NNNN-short-kebab-title.md` (service-local, in that repo's `docs/adr/`). Take the next free
number from `docs/adr/README.md`. Fill every section. Do not delete the Options section.

## Status

One of: `proposed`, `accepted`, `accepted (reconstructed)`, `superseded`, `rejected`.

- `proposed` — decision made at less than full confidence, not yet proven against the code. Flip
  to `accepted` once it survives contact, or to `rejected` (keep the file).
- `accepted (reconstructed)` — backfilled after the fact. Requires a `confirmed_by: <name>,
  <date>` frontmatter line, or an explicit statement in Consequences that the original rationale
  was not recovered.
- `superseded` — replaced by a later ADR. Set `superseded_by` in frontmatter and add a line at
  the top of the body: `Superseded by PADR-NNNN.` Never edit anything else in a superseded ADR.

## Context

The forces at play. What problem, what constraints, what pressure made this a decision rather
than a default. State facts, not justification.

## Options

Every option that was genuinely on the table, including the one chosen. For each: what it is,
and the concrete reason it was or was not taken. The rejected options are the load-bearing part
of this section — an ADR that lists only the chosen path has thrown away most of its future
value.

- **Option A — <name>**: description. Rejected because ...
- **Option B — <name>**: description. Rejected because ...
- **Option C — <name> (chosen)**: description. See Decision.

## Decision

What was chosen, stated plainly. One paragraph.

## Consequences

What is now true, including the costs being accepted. What becomes harder. What a future change
would have to overcome to reverse this. For a reconstructed ADR with lost rationale, say so here.
