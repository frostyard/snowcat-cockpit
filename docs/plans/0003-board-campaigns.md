# Plan: Multi-repository board campaigns

This plan delivers the persistent campaign accepted by ADR-0008 on top of the
production node and bounded fleet contracts.

## Phase 1 — Enroll managed repositories

- Add a non-secret repository catalog and explicit enroll/list/setup APIs.
- Create or refresh only Cockpit-owned source checkouts and retain failures.
- Present every enrolled repository and its immutable prepared base in the
  dashboard.
- **Done when:** one node can prepare two repositories from one action without
  modifying or deleting an operator-owned checkout.

## Phase 2 — Start one campaign

- Add the campaign state machine, bounded setup, and two-attempt provider
  preflight refresh.
- Reconcile all enrolled repositories and all three worker lanes at a bounded
  interval using ordinary managed-worker launches.
- Add one dashboard start/stop control and coarse per-repository/lane status.
- **Done when:** one action launches eligible work in two repositories, waits
  through human admission, and later launches implementation or review work
  without another action.

## Phase 3 — Harden restart and observability

- Mark an interrupted durable campaign explicitly on process restart; require
  an operator to start a new run.
- Show sanitized setup, preflight, observation, and launch failures without
  terminal output or credentials.
- Exercise host, rootless Podman, and Docker campaign paths.
- **Done when:** killing and restarting the node loses no workspace, launches
  no surprise worker, and presents an actionable interrupted campaign record.

## Later / ideas

- Import repository slugs from Snowcat if it adds a scoped enrollment-read MCP
  contract.
- Per-repository provider or concurrency overrides.
- Explicit quiet hours and inference budgets.

## References

- Implements: [board campaigns](../design/board-campaigns.md),
  [Cockpit node](../design/node.md)
- Rationale:
  [ADR-0008](../adr/0008-run-persistent-multi-repository-board-campaigns.md)
- Existing contracts:
  [provider preflight](../specs/provider-preflight.md),
  [queue observation and bounded fleets](../specs/queue-observation-and-fleets.md),
  [managed workers](../specs/managed-workers.md)
