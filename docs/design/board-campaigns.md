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
launchable = min(queued eligible items, configured capacity - active workers)
```

It launches ordinary managed workers, preserving the selected adapter,
runtime, provider, model, source, and immutable base. A launch failure stops
that repository/role for the current tick. The next tick may retry after the
minimum interval.

Provider proof is demand-driven after campaign start. A receipt approaching
expiry is refreshed only when fresh observation finds eligible work for that
provider; an idle campaign does not periodically invoke coding agents merely
to keep receipts warm.

Exited workers do not prove that Snowcat work completed. Attempt correlation is
an independent, delayed observation and may remain unmatched. Fresh queue state
is the only input to later capacity decisions.

A newly launched worker remains under a startup probe until one later
reconciliation observes it running. If it exits or loses its retained terminal
before that point, the controller applies the existing five-minute
repository/role launch backoff. This bounds a broken provider or container
entrypoint without interpreting the exit as a queue result.

### Lifecycle

Campaign state is `starting`, `running`, `degraded`, `stopping`, or `stopped`.
It records configuration, coarse per-repository/lane status, worker IDs it
launched, timestamps, and sanitized errors. It does not persist queue items,
terminal output, environment, tokens, or attempt payloads.

An idle running campaign waits for human proposal admission and Snowcat's
pull-request verifier. Explicit stop cancels future setup, preflight,
observation, and launch calls. Already-launched workers and every source and
workspace remain retained for explicit operator action.

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
  [ADR-0008](../adr/0008-run-persistent-multi-repository-board-campaigns.md)
- Existing contracts:
  [managed repositories and board campaigns](../specs/repositories-and-board-campaigns.md),
  [queue observation and bounded fleets](../specs/queue-observation-and-fleets.md),
  [managed workers](../specs/managed-workers.md),
  [provider preflight](../specs/provider-preflight.md)
- Built in:
  [board-campaign delivery plan](../plans/0003-board-campaigns.md)
