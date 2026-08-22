# Plan: Multi-repository board campaigns

This plan delivers the persistent campaign accepted by ADR-0008 on top of the
production node and bounded fleet contracts.

## Phase 1 — Enroll managed repositories (complete 2026-08-21)

- Add a non-secret repository catalog and explicit enroll/list/setup APIs.
- Create or refresh only Cockpit-owned source checkouts and retain failures.
- Present every enrolled repository and its immutable prepared base in the
  dashboard.
- **Done when:** one node can prepare two repositories from one action without
  modifying or deleting an operator-owned checkout.

The live no-claim acceptance run enrolled Core and Mill, cloned and refreshed
both Cockpit-owned sources concurrently, and pinned commits `0ffddbc8993e` and
`70eecde7aed6`. The original development checkouts were not used or modified.

## Phase 2 — Start one campaign

- Add the campaign state machine, bounded setup, and two-attempt provider
  preflight refresh.
- Reconcile all enrolled repositories and all three worker lanes at a bounded
  interval using ordinary managed-worker launches.
- Add one dashboard start/stop control and coarse per-repository/lane status.
- **Done when:** one action launches eligible work in two repositories, waits
  through human admission, and later launches implementation or review work
  without another action.

The first live campaign selected host Codex and Copilot, refreshed their
provider-local `snowcat` and `snowcat-mcp` proofs, observed Core and Mill in one
tick, launched no workers from their empty queues, and stopped without deleting
state. A second campaign selected Docker, prepared Clix, Core, Mill, and Std,
refreshed Claude and Copilot, and launched four Claude discoverers round-robin:
two against Clix and two against Std. It refilled the remaining three items as
capacity became available. All seven workers exited and correlate to completed
Snowcat attempts: three for Clix and four for Std. The campaign remains running
with every observed lane empty, ready for work admitted from those proposals.

## Phase 3 — Harden restart and observability

- Mark an interrupted durable campaign explicitly on process restart; require
  an operator to start a new run.
- Show sanitized setup, preflight, observation, and launch failures without
  terminal output or credentials.
- Exercise host, rootless Podman, and Docker campaign paths.
- **Done when:** killing and restarting the node loses no workspace, launches
  no surprise worker, and presents an actionable interrupted campaign record.

The durable-state unit test covers interrupted active-state recovery, and the
live stopped campaign survived a node rebuild/restart without launching work.
Host no-work and Docker active-work paths are exercised. Rootless Podman and an
actual killed-while-active acceptance run remain.

## Later / ideas

- Import repository slugs from Snowcat if it adds a scoped enrollment-read MCP
  contract.
- Per-repository provider or concurrency overrides.
- Explicit quiet hours and inference budgets.

## References

- Implements: [board campaigns](../design/board-campaigns.md),
  [Cockpit node](../design/node.md),
  [managed repositories and board campaigns](../specs/repositories-and-board-campaigns.md)
- Rationale:
  [ADR-0008](../adr/0008-run-persistent-multi-repository-board-campaigns.md)
- Existing contracts:
  [provider preflight](../specs/provider-preflight.md),
  [queue observation and bounded fleets](../specs/queue-observation-and-fleets.md),
  [managed workers](../specs/managed-workers.md)
