# Spec: PR review rubric

This checklist governs every Snowcat Cockpit pull-request review. It is
consumed by the [review runbook](../../.github/prompts/review.prompt.md), the
PR template, and the [quality loop](../design/quality-loop.md).
`docs/review-rubric.md` is the ACMM alias registered by
[ADR-0007](../adr/0007-use-canonical-aliases-for-acmm-conformance.md).

## Interface

| Check | Verification |
| --- | --- |
| Risk declared | PR body names the highest applicable frostyard tier and explains changes to credentials, authority, network, lifecycle, or cleanup. |
| Boundary preserved | Diff does not make Cockpit a Snowcat protocol proxy, access Snowcat databases, expose secrets, widen listener addresses, use `eval`, or delete workspaces implicitly. |
| Contract synchronized | Behavior changes update their `docs/specs/` contract; a new ownership boundary has an ADR first and corresponding design/plan links. |
| Repository gate green | `make ci` passes from a clean checkout. |
| Workflows hardened | `actionlint` passes; actions are full-SHA pinned with version comments, top-level permissions are empty, job permissions are least privilege, and checkout credentials are not persisted. |
| Docs integrity green | `node scripts/check-docs.mjs` reports all three rates at 1.0. |
| Agent separation | The author did not approve, merge, release, or push directly to `main`; `.claude/settings.json` backs the limit mechanically. |

## Rules

- Verify every applicable row independently from the diff and named command.
- Any failed row is a blocker, not an advisory.
- Review the immutable proposed head; a changed head requires a new review.
- Edit canonical content, never an alias from ADR-0007.

## References

- Rationale: [ADR-0007](../adr/0007-use-canonical-aliases-for-acmm-conformance.md)
- Context: [quality loop](../design/quality-loop.md)
