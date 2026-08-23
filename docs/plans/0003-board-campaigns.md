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

The durable-state unit test covers interrupted active-state recovery. The live
node was then killed while its empty Docker campaign remained active. Restart
changed the durable record to `stopped` with the interrupted explanation,
retained all seven exited workers, and launched nothing. A new explicit start
returned it to the empty-board running state. Host no-work and Docker
active-work paths are exercised; a rootless-Podman campaign remains.

The continuation trial exposed a Docker-specific Copilot 1.0.80 startup
failure: the standalone CLI unpacked a native module beneath its cache, while
Docker's home tmpfs was `noexec`. Three still-queued reviews caused 31 retained
failed terminals before the operator stopped refill. Copilot now receives one
narrow executable cache tmpfs, and a worker that exits before surviving one
later reconciliation backs off that repository/role for five minutes. Worker
terminals and workspaces remain retained, and the startup exit still does not
assert a Snowcat outcome.

A later Claude trial retained three exited workers whose Snowcat attempts
expired. Each provider had moved tests or subagent work into the background;
Claude print mode reached its ten-minute background wait ceiling and exited
before commit, pull-request creation, or completion reporting. The campaign's
queued-only projection then hid those reclaimable claimed items. ADR-0009
records the durable corrections: observe Snowcat's expired-attempt projection
alongside queued work, and disable background tasks in the unattended Claude
image.

A follow-on Snowcat campaign exposed a second, independent overrun path. Four
foreground Claude providers remained alive after two 15-minute attempts
expired without a successful heartbeat. The Claude image did not contain the
Node.js baseline, so each worker downloaded its own Node.js 24 toolchain before
testing. Snowcat's parallel coverage suite then crossed the OCI adapter's
512-PID ceiling hundreds of times (`pids.events:max`), causing Node processes
to abort and emit dozens of large core files into retained workspaces. Cockpit
now preinstalls Node.js 26 and npm in every provider image, gives bounded test
workloads a four-CPU quota and measured 1024-PID ceiling, disables core files,
and tells every role to claim and renew a 3600-second lease around long
execution steps. An
explicit dashboard action correlates active workers and highlights the
authoritative Snowcat `expired` outcome without giving Cockpit a lease token.

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
  and [ADR-0009](../adr/0009-observe-reclaimable-snowcat-work.md)
- Existing contracts:
  [provider preflight](../specs/provider-preflight.md),
  [queue observation and bounded fleets](../specs/queue-observation-and-fleets.md),
  [managed workers](../specs/managed-workers.md)
