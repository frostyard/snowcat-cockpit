# 0008 — Run persistent multi-repository board campaigns

- **Status:** Accepted
- **Date:** 2026-08-21

## Context

The bounded-fleet trial proved that a one-shot queue snapshot and launch are
safe, but it also proved that they do not clear a real board. Discovery
proposals require human admission, Snowcat's pull-request verifier may add
review work several minutes after implementation exits, and released work may
become eligible again. The operator repeatedly refreshed preflight, refreshed
source checkouts, observed each repository, and launched the next lane.

The OCI trial exercised Codex, Claude, and Copilot across Firn, Clix, and
Updex. It measured one transient Copilot preflight failure that passed on the
immediate retry, transient Snowcat transport failures, five concurrent Docker
discoveries, five admitted deliveries, four draft pull requests, one intended
human issue, and delayed worker-attempt correlation after four reviewers
exited. Snowcat remained the effective concurrency and lease authority in all
cases.

Snowcat's observer MCP surface exposes queue work, not its control-plane
enrollment registry. Cockpit may not read Snowcat's databases and a local node
also needs a trusted source path before it can allocate a worker checkout.

## Decision

Add an operator-started, persistent **board campaign** as the measured
exception to ADR-0004's one-shot-only boundary. One campaign covers every
repository explicitly enrolled on the Cockpit node and continually reconciles
bounded local worker capacity with fresh Snowcat queue observations until the
operator stops it.

A Cockpit repository enrollment is non-secret local execution configuration:
an `owner/name` slug, a Cockpit-managed source checkout, and a local base-ref
policy. It is not a copy of, or claim about, Snowcat's control-plane enrollment.
Snowcat still decides whether an item is claimable. Cockpit stores no queue
item, attempt, provider credential, observer credential, or lease token in the
enrollment or campaign record.

Campaign start performs, in order:

1. converge every enrolled managed source checkout without deleting local
   state;
2. refresh each selected provider's live MCP preflight, with at most one
   immediate retry;
3. start one bounded reconciliation loop across all enrolled repositories;
4. observe each repository and fill configured discoverer, implementer, and
   reviewer capacity with ordinary one-item workers.

The loop is level-triggered and bounded. It polls no faster than its configured
interval, subtracts already-running workers from capacity, never claims work
itself, and stops launching a repository after a setup or observation failure
until a later reconciliation. A campaign remains idle while waiting for human
proposal admission or delayed verifier work. Stopping it prevents new launches
but does not stop workers or clean workspaces.

## Consequences

- One dashboard action can operate several repositories and all three lanes
  while preserving Snowcat's leases and human admission/merge boundaries.
- The node now has a small background controller and durable non-secret
  campaign intent; this is more production-shaped than the original spike.
- Operators enroll a repository once on each node. Cockpit cannot automatically
  mirror Snowcat enrollment until Snowcat offers an authorized read contract.
- Managed source checkouts consume disk and are retained. Fetch, repair, and
  cleanup failures remain visible and non-destructive.
- A campaign may intentionally remain idle indefinitely. “No queued work” is
  not completion because human admission or pull-request verification may add
  work later.
- Provider and Snowcat outages produce visible degraded repository/lane state;
  they never cause an unbounded retry storm.

## Alternatives considered

- **Keep clicking one-shot fleets:** rejected because measured admission and
  verifier delays require repeated operator orchestration and do not meet the
  one-button goal.
- **Derive enrollment from unfiltered `list_work`:** rejected because a queue
  item does not prove current Snowcat enrollment and repositories with no work
  disappear from that projection.
- **Read Snowcat's control database:** forbidden by Cockpit's boundary and
  would couple the node to private storage.
- **Add a Snowcat enrollment endpoint first:** deferred; local execution still
  requires node-specific source configuration, and the current contract is
  sufficient for an explicit local enrollment.
- **Stop or delete workers when a campaign stops:** rejected because retained
  terminals and explicit cleanup are established safety contracts.

## References

- Shapes: [board campaigns](../design/board-campaigns.md),
  [Cockpit node](../design/node.md),
  [production roadmap](../plans/0002-production-roadmap.md),
  [board-campaign delivery plan](../plans/0003-board-campaigns.md)
- Builds on: [ADR-0003](0003-isolate-each-managed-worker-terminal.md),
  [ADR-0004](0004-observe-snowcat-once-to-plan-bounded-fleets.md),
  [ADR-0005](0005-isolate-unattended-workers-in-rootless-oci.md)
- Reclaim extension: [ADR-0009](0009-observe-reclaimable-snowcat-work.md)
