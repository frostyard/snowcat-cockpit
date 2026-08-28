# Board campaigns

Living document. Rationale:
[ADR-0008](../adr/0008-run-persistent-multi-repository-board-campaigns.md).
Contracts: [managed repositories and board campaigns](../specs/repositories-and-board-campaigns.md).

## Overview

A board campaign turns one operator action into bounded, persistent execution
across every repository enrolled on one Cockpit node. It prepares managed
sources and provider profiles, then fills discovery, implementation, and review
capacity from fresh Snowcat observations.

```text
operator ──► campaign controller ──► managed repository sources
                    │
                    ├──bounded read──► Snowcat list_work
                    ├──launch────────► discoverer workers
                    ├──launch────────► implementer workers
                    └──launch────────► reviewer workers
```

Workers continue to claim and complete work through Snowcat MCP. The campaign
never receives their lease tokens.

## Design

### Local repository enrollment

The repository catalog owns non-secret, node-local execution inputs. Each
record has a normalized GitHub slug, a source checkout beneath the configured
managed-source root, a base ref, setup status, and timestamps. Adding a record
does not assert that Snowcat currently enrolls the repository.

Setup creates a missing checkout through the installed GitHub CLI and refreshes
an existing Cockpit-owned checkout from its credential-free GitHub origin. It
resolves a new immutable base only after the refresh succeeds. Setup never
deletes, force-resets, or repurposes an operator-owned checkout.

### Start sequence

Campaign start takes a complete configuration snapshot and rejects a second
active campaign. It converges all repository sources with bounded concurrency,
then refreshes each distinct provider/MCP-server profile. A failed live
preflight receives exactly one immediate retry. Any repository or provider
that is still not ready is visible in the campaign status; ready combinations
continue.

### Reconciliation

One scheduler tick observes every ready repository with bounded concurrency.
For each role it calculates:

```text
launchable = min(claimable eligible items, configured capacity - active workers)
```

It launches ordinary managed workers, preserving the selected adapter,
runtime, provider, model, source, and immutable base. A launch failure stops
that repository/role for the current tick. The next tick may retry after the
minimum interval.

The immutable base is refreshed at the point where staleness costs work:
immediately before every implementer launch, the controller reruns managed
repository setup and launches from the commit pinned by that successful
refresh. It never spends an implementation attempt from the campaign-start
snapshot. Refresh failure is a visible repository blocker and backs off that
implementer lane; it does not fall back to the older commit. Discoverers remain
read-only, while reviewers replace the prepared base with the claimed pull
request's exact head before review.

Provider proof is demand-driven after campaign start. A receipt approaching
expiry is refreshed only when fresh observation finds eligible work for that
provider; an idle campaign does not periodically invoke coding agents merely
to keep receipts warm.

Claimable means a logically queued item or a logically claimed item whose
newest Snowcat attempt is expired. Cockpit reads both bounded projections and
uses Snowcat's attempt outcome; it never computes lease expiry or requeues the
item itself.

Exited workers do not prove that Snowcat work completed. Cockpit retains a
campaign worker probe after startup and performs one exact, read-only Snowcat
attempt correlation when the provider exits. A terminal outcome permits lane
refill. An active claimed attempt fails and backs off the whole role lane,
regardless of what the provider printed before exit. An unmatched worker that
survived startup is a normal no-claim exit; an unmatched startup exit remains a
launch failure. If correlation itself fails, refill pauses and the lane remains
degraded until a later reconciliation can determine the outcome.

Campaign state records only the coarse lane failure on its repository status.
The correlated Snowcat item, attempt, and derived outcome remain request-local
and are never written to worker or campaign state.

### Lifecycle

Campaign state is `starting`, `running`, `degraded`, `stopping`, or `stopped`.
It records configuration, coarse per-repository/lane status, worker IDs it
launched, timestamps, and sanitized errors. It does not persist queue items,
terminal output, environment, tokens, or attempt payloads.

The top-level state is a rollup, not an independent heartbeat. A current
repository or provider blocker makes it degraded even when another ready lane
continues launching. The dashboard pairs that state with cumulative launches,
currently active node workers counted against lane capacity, and the most
recent queue-observation time so a polling-but-idle campaign is distinguishable
from a blocked one.

An idle running campaign waits for human proposal admission and Snowcat's
pull-request verifier. Explicit stop cancels future setup, preflight,
observation, and launch calls. Stop itself stops no already-launched worker and
deletes no source, terminal, or workspace: whatever execution state the campaign
still holds at stop remains retained for explicit operator action.

While a campaign runs, it does clean workspaces automatically, but only where
nothing about them can still matter. A worker becomes a cleanup candidate when
its provider process exited cleanly — never one reported failed — and its exact
attempt correlation reached a terminal Snowcat outcome, or it was a stabilized
worker whose lane found nothing to claim. `SNOWCAT_COCKPIT_RETAIN_WORKSPACES`
bounds how many candidates accumulate before cleanup catches up: a non-negative
integer count of the newest candidates to keep, defaulting to 20, or a duration
such as `6h` that cleans a candidate once it has been eligible that long. An
explicit `0` cleans every eligible candidate on each sweep. A failed worker, a
worker whose lane never reached a terminal outcome, and any candidate whose
cleanup the managed-worker contract refuses — an unclean tree, for example —
stay retained for explicit operator action; a refused candidate is simply
reconsidered on the next tick. The campaign record reports `workspacesCleaned`
and `lastCleanupAt`.

## Operational notes

- Run the node from an environment where GitHub and each selected provider are
  already authenticated; campaign configuration never contains credentials.
- Provider MCP server names are provider-local (`snowcat` for Claude in the
  measured setup and `snowcat-mcp` for Copilot).
- Start with conservative per-role capacity. Snowcat prevents duplicate
  claims, but inference and GitHub limits remain node-local concerns.
- A stopped process resumes no campaign automatically in the first slice. The
  durable record explains the interrupted state and requires a new operator
  start.

## References

- Rationale:
  [ADR-0008](../adr/0008-run-persistent-multi-repository-board-campaigns.md),
  [ADR-0009](../adr/0009-observe-reclaimable-snowcat-work.md)
- Existing contracts:
  [managed repositories and board campaigns](../specs/repositories-and-board-campaigns.md),
  [queue observation and bounded fleets](../specs/queue-observation-and-fleets.md),
  [managed workers](../specs/managed-workers.md),
  [provider preflight](../specs/provider-preflight.md)
- Built in:
  [board-campaign delivery plan](../plans/0003-board-campaigns.md)
