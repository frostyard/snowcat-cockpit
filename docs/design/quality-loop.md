# Quality loop

Living document. Rationale:
[ADR-0007](../adr/0007-use-canonical-aliases-for-acmm-conformance.md).
Contracts: [PR review rubric](../specs/pr-review-rubric.md),
[PR acceptance metric](../specs/pr-acceptance-metric.md).
`docs/quality.md` is the ACMM alias for this file.

## Overview

Cockpit change quality moves through one explicit loop:

```text
PR declaration -> independent review -> CI gates -> correction inbox -> canonical rules/docs
       ^                                                                    |
       +---------------------- acceptance metric ---------------------------+
```

## Design

- **Declare:** `.github/pull_request_template.md` requires risk and boundary
  checks, including Cockpit's credential-bearing terminal surface.
- **Review:** the [rubric](../specs/pr-review-rubric.md) and
  [runbook](../../.github/prompts/review.prompt.md) require independent,
  evidence-backed review.
- **Gate:** `make ci` runs formatting, vet, lint, Go and shell integration
  tests, and `scripts/check-docs.mjs`; `.github/workflows/test.yml` runs the
  same canonical gate on GitHub.
- **Learn:** [`.memory/corrections.jsonl`](../../.memory/README.md) captures
  evidence-backed corrections until they are promoted into canonical policy,
  docs, tests, or skills.
- **Enforce:** `.claude/settings.json` denies self-merge, approval, release,
  force-push, direct main push, and secret reads.
- **Observe:** the [acceptance metric](../specs/pr-acceptance-metric.md)
  describes delivery health without weakening any gate.

## Operational notes

Run `make ci` before every commit described as complete. A docs-integrity
failure is repaired at the canonical target or index; never replace an alias
with duplicate prose. Workflow changes also run `actionlint` locally.

## References

- Rationale: [ADR-0007](../adr/0007-use-canonical-aliases-for-acmm-conformance.md)
- Contracts: [PR review rubric](../specs/pr-review-rubric.md),
  [PR acceptance metric](../specs/pr-acceptance-metric.md)
- Built in: [Production roadmap, Phase 5](../plans/0002-production-roadmap.md#phase-5--harden-container-delivery)
