# Org-wide decisions (frostyard/core ADRs)

Conventions this repository follows that are decided at the organization level
are recorded as ADRs in
[frostyard/core](https://github.com/frostyard/core/tree/main/docs/adr).
The ones that currently bind Snowcat Cockpit are:

- [ADR-0018 — Org-wide agent instruction and knowledge surfaces](https://github.com/frostyard/core/blob/main/docs/adr/0018-org-wide-agent-instruction-and-knowledge-surfaces.md) — canonical `AGENTS.md` and portable aliases
- [ADR-0019 — Governance as code and risk tiers](https://github.com/frostyard/core/blob/main/docs/adr/0019-governance-as-code-and-risk-tiers.md) — non-relaxing gates, explicit risk, and agent limits
- [ADR-0021 — SHA-pinned least-privilege CI](https://github.com/frostyard/core/blob/main/docs/adr/0021-sha-pinned-actions-and-least-privilege-ci.md) — workflow actions and permissions
- [ADR-0025 — One `docs/` tree per repository](https://github.com/frostyard/core/blob/main/docs/adr/0025-consolidate-repository-docs-into-docs.md) — this four-category documentation shape and templates
- [ADR-0029 — ACMM via canonical aliases](https://github.com/frostyard/core/blob/main/docs/adr/0029-acmm-conformance-via-canonical-aliases.md) — relative file aliases, real directory trees, and integrity checks
- [ADR-0038 — `make ci` stays canonical](https://github.com/frostyard/core/blob/main/docs/adr/0038-scope-the-test-name-filter-to-chairlift.md) — this repository exposes its complete credential-free gate as `make ci`

When changing behavior covered by one of these, update or supersede the ADR in
frostyard/core first, then change this repository in the same effort.
