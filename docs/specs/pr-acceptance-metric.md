# Spec: PR acceptance metric

This contract defines the observational pull-request acceptance metric for
Snowcat Cockpit. It is consumed by the
[quality loop](../design/quality-loop.md); `docs/metrics.md` is the ACMM alias
registered by [ADR-0007](../adr/0007-use-canonical-aliases-for-acmm-conformance.md).

## Definition

```text
acceptance_rate = merged pull requests not subsequently reverted / pull requests opened
```

The rolling window is the last 30 non-draft pull requests opened, clipped to
90 days. The data source after repository publication is:

```text
gh pr list --repo frostyard/snowcat-cockpit --state all --limit 30 \
  --json number,state,mergedAt,title,isDraft
```

## Rules

- Drafts enter the denominator only when marked ready.
- Closed-unmerged pull requests count as not accepted.
- A later commit or pull request explicitly reverting the original counts the
  original as reverted even if that revert is later reverted; churn is the
  signal.
- The metric observes the stream and never replaces the
  [review rubric](pr-review-rubric.md) or CI gates.

## References

- Rationale: [ADR-0007](../adr/0007-use-canonical-aliases-for-acmm-conformance.md)
- Context: [quality loop](../design/quality-loop.md)
