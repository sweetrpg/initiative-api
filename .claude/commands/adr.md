---
name: "ADR"
description: Create a new Architecture Decision Record from the template
allowed-tools: Bash(ls:*), Bash(date:*), Read, Write, Edit
category: Workflow
tags: [adr, architecture, decision]
---

Create a new Architecture Decision Record.

**Input**: a short title after `/adr` (e.g. `/adr use nats as the platform message bus`). If
omitted, infer it from the current conversation; if that is ambiguous, ask.

**Steps**

1. **Pick the tier.**
   - Working in the `platform` repo (or its worktrees) → platform tier, prefix `PADR`, target
     dir `docs/adr/`.
   - Working inside a service submodule → service-local tier, prefix `ADR`, target dir
     `docs/adr/` of that repo. Create the dir and copy `platform/docs/adr/0000-template.md` and
     a minimal `README.md` index if the service has no `docs/adr/` yet.

2. **Get the next number.** `ls` the target `docs/adr/` directory. Take the highest `NNNN` in a
   `*-*.md` filename (ignoring `0000-template.md`), add 1, zero-pad to 4 digits.

3. **Build the filename.** `<PREFIX>-<NNNN>-<title-kebab-cased>.md`. Lowercase, hyphens for
   spaces, strip punctuation.

4. **Write the file** from the template (`platform/docs/adr/0000-template.md`). Fill frontmatter:
   - `id`: `<PREFIX>-<NNNN>`
   - `title`: the title as given, sentence case
   - `status`: `proposed`
   - `date`: today (`date +%Y-%m-%d`)
   - `scope`: `platform` for a PADR, the service name for an ADR
   - `tags`, `affected_repos`: best guess from context, leave `[]` if unclear
   Leave the body sections as template prompts for the author to fill, but pre-fill Context from
   the conversation if there is enough signal.

5. **Add the index row.** Append a row to the target `docs/adr/README.md` index table:
   `| [NNNN](<filename>) | <title> | proposed | <one-line decision or —> | <date> |`
   Keep rows ordered by number.

6. **Report** the path and remind the author: fill the Options section including rejected
   options, and flip status to `accepted` once the decision survives contact with the code.

**Guardrails**
- Never reuse or guess a number without listing the directory first.
- Never write `status: accepted` on creation — new ADRs start `proposed`.
- Do not edit an existing `accepted` or `superseded` ADR. A changed decision is a new ADR that
  supersedes the old one.
