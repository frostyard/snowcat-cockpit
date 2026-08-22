# 0009 — Observe reclaimable Snowcat work

- **Status:** Accepted
- **Date:** 2026-08-22

## Context

ADR-0004 bounded one queue snapshot to a single `list_work` call filtered to
logical status `queued`. The board-campaign trial later retained three Claude
workers whose foreground provider processes exited while their background
tasks were still running. Their Snowcat leases expired, and Snowcat correctly
made those logical `claimed` items eligible for a later `claim_work` call.

Cockpit continued to observe zero queued implementation items. Its campaign
therefore launched no replacement even though Snowcat would have allowed a
worker to reclaim each expired attempt. The observer and the implementation
worker prompt both hid the same reclaimable work.

## Decision

An operator-triggered Cockpit queue observation makes two bounded Snowcat
`list_work` calls for the exact repository: one for logical status `queued` and
one for logical status `claimed`, each with limit 100.

The claimable projection contains every queued item plus only claimed items
whose newest attempt has Snowcat's terminal outcome `expired`. A claimed item
with a live newest attempt or any malformed attempt history fails closed or is
excluded; Cockpit does not calculate lease expiry from its own clock. The
snapshot is marked truncated when either bounded source projection reaches its
limit.

Implementation-worker selection derives its open kind set from the same two
projections. Snowcat remains the claim authority: Cockpit stores no item or
attempt, receives no lease token, and a worker still calls `claim_work` exactly
once for its bounded kinds.

Unattended Claude workers disable background-task functionality through the
provider's supported image-owned environment setting. Tests and subagents must
therefore remain inside the one foreground provider process whose exit Cockpit
retains and observes.

## Consequences

- A campaign can replace an exited worker after Snowcat expires its lease
  without an operator manually requeueing the item.
- One snapshot now performs two MCP reads and can contain up to 200 projected
  items. It remains request-only and bounded; no poller or queue replica enters
  Cockpit.
- Near-simultaneous claims can still make the advisory snapshot stale. Snowcat,
  not Cockpit, decides whether the subsequent worker receives work.
- Disabling Claude background tasks removes useful within-session concurrency
  in exchange for making print-mode process completion trustworthy.

## Alternatives considered

- **Requeue expired items manually:** rejected because Snowcat already makes
  them reclaimable and a persistent campaign should not require a redundant
  operator mutation.
- **Calculate expiry from `leaseExpiresAt`:** rejected because local clock skew
  would duplicate Snowcat's authority. The attempt projection already carries
  Snowcat's `expired` outcome.
- **Set Claude's print background wait ceiling to zero:** rejected because a
  stuck background task could retain an unattended provider indefinitely and
  still lose its lease. Foreground execution gives the worker a chance to
  heartbeat around long steps.

## References

- Shapes: [board campaigns](../design/board-campaigns.md),
  [queue observation and bounded fleets](../specs/queue-observation-and-fleets.md),
  [managed workers](../specs/managed-workers.md),
  [OCI workers](../specs/oci-workers.md)
- Extends: [ADR-0004](0004-observe-snowcat-once-to-plan-bounded-fleets.md),
  [ADR-0008](0008-run-persistent-multi-repository-board-campaigns.md)
