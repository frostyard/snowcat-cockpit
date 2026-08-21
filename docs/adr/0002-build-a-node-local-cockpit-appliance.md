# 0002 — Build a node-local Cockpit appliance

- **Status:** Accepted
- **Date:** 2026-08-21

## Context

The Bash, tmux, and ttyd spike proved that several interactive coding-agent
terminals can remain durable, share one browser console, and survive browser
reconnection without changing Snowcat. The real Firn operating trial also
exposed the work that a repeatable operator workflow needs beyond terminal
retention:

- the operator manually allocated one isolated worktree per worker;
- the operator had to know exact queued kinds before launch;
- globally configured HTTP MCP access worked, but was not preflighted by
  Cockpit;
- workers did not have Snowcat's canonical queue skills pre-seeded;
- an under-authorized implementation child omitted `open-pr`, so the worker
  correctly completed without a pull request and the operator carried the
  artifact last mile; and
- an independent Copilot reviewer selected a different model, returned a
  structured pass verdict, and Snowcat moved the draft pull request ready.

The experiment therefore answered its narrow question positively while also
showing that production use needs one place to inspect readiness, allocate
workspaces, launch bounded worker batches, attach to terminals, and retain
operator-visible local lifecycle state.

## Decision

Evolve Snowcat Cockpit into a node-local appliance while preserving
[ADR-0001](0001-keep-cockpit-outside-snowcat.md)'s coordination/execution
boundary.

One long-lived Cockpit process owns an embedded loopback web dashboard, local
worker profiles, isolated workspace allocation, local process or container
lifecycle, and terminal attachment. It may observe queue bookkeeping only
through Snowcat's supported HTTP MCP read tools. Coding-agent workers continue
to claim and complete work directly through MCP and remain the sole holders of
lease tokens.

Cockpit may persist a node identity, operator configuration, worker launch
metadata, workspace inventory, and coarse runtime events in its own local
state. It must not persist provider credentials, MCP credentials, lease tokens,
terminal contents, or a copy of Snowcat's authoritative queue.

The production runtime supports two execution adapters behind the same worker
profile contract:

- a host adapter for installed CLIs and interactive/manual permission flows;
- an OCI adapter, preferring rootless Podman, for reproducible and more
  isolated workers.

The first fleet control launches an explicit bounded batch. Each worker claims
at most one item and exits or remains available for inspection. Desired
concurrency, automatic refill, and unattended queue draining remain separate
future decisions.

The Bash launcher remains during migration as the trial harness. It is removed
only after the appliance reproduces its durable-terminal behavior and its
replacement contract is accepted.

## Consequences

- A laptop, desktop, Incus instance, or VM can run the same Cockpit node and
  point it at the same Snowcat service; Snowcat leases arbitrate work across
  nodes.
- The dashboard and launcher share one readiness, profile, workspace, and
  lifecycle model instead of becoming separate products.
- A daemon, dashboard, workspace manager, read-only queue observation, and
  Cockpit-local state are now in scope.
- Provider-specific skill installation, trust, permission, and credential
  projection require explicit profiles and fail-closed preflight.
- Rootless containers can make unattended permissions safer, but provider
  authentication state still needs a separately specified projection boundary.
- Cockpit must expose incomplete or suspicious contracts, such as change work
  without `open-pr`, without silently widening Snowcat authority.
- A continuously refilled fleet is not implied by a batch-launch button.

## Alternatives considered

- **Keep extending the Bash CLI:** rejected because a durable dashboard,
  readiness model, workspace inventory, and concurrent lifecycle reconciliation
  need typed long-lived state and interfaces.
- **Put worker execution into Snowcat:** rejected because it would reverse the
  established credential and execution boundary.
- **Build a central Cockpit scheduler:** rejected because Snowcat already owns
  durable selection and leases; each Cockpit instance needs only node-local
  capacity management.
- **Run Cockpit only as a container:** rejected as the primary shape because
  launching sibling containers would normally require a highly privileged
  runtime socket. A native node process can still launch isolated worker
  containers.
- **Start with automatic refill:** rejected until explicit batch operation has
  measured launch, workspace, provider, and intervention failure modes.

## References

- Shapes: [node architecture](../design/node.md),
  [node CLI and HTTP API](../specs/node-api.md)
- Built in: [production roadmap](../plans/0002-production-roadmap.md)
- Builds on: [ADR-0001](0001-keep-cockpit-outside-snowcat.md)
