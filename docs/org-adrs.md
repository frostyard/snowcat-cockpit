# Org-wide decisions (frostyard/core ADRs)

Conventions this repository follows that are decided at the organization level
are recorded as ADRs in
[frostyard/core](https://github.com/frostyard/core/tree/main/docs/adr).
The ones that currently bind Snowcat Cockpit are:

- [ADR-0018 — Org-wide agent instruction and knowledge surfaces](https://github.com/frostyard/core/blob/main/docs/adr/0018-org-wide-agent-instruction-and-knowledge-surfaces.md) — canonical `AGENTS.md` and portable aliases
- [ADR-0022 — `make ci` is the canonical gate](https://github.com/frostyard/core/blob/main/docs/adr/0022-make-ci-gate-and-test-naming-filter.md) — the repository exposes its complete check as `make ci`
- [ADR-0025 — One `docs/` tree per repository](https://github.com/frostyard/core/blob/main/docs/adr/0025-consolidate-repository-docs-into-docs.md) — this four-category documentation shape and templates

When changing behavior covered by one of these, update or supersede the ADR in
frostyard/core first, then change this repository in the same effort.
