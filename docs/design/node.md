# Cockpit node

Living document. Rationale:
[ADR-0002](../adr/0002-build-a-node-local-cockpit-appliance.md).
Contracts: [node CLI and HTTP API](../specs/node-api.md).

## Overview

A Cockpit node is one machine-local control process and embedded web dashboard.
It reports local readiness, manages worker execution capacity and isolated
workspaces, and presents durable worker terminals. Snowcat remains the remote
work coordinator and every coding-agent worker uses Snowcat MCP directly.

```text
browser ──► Cockpit node ──read-only bookkeeping──► Snowcat MCP
                 │
                 ├──► host worker ────────────────► Snowcat MCP / checkout / GitHub
                 └──► OCI worker  ────────────────► Snowcat MCP / checkout / GitHub
```

Multiple nodes may point at one Snowcat service. They do not coordinate with
each other; Snowcat claims and leases prevent duplicate assignment.

## Node process

`snowcat-cockpit serve` is a long-lived Go process with an embedded dashboard.
It binds loopback by default and refuses a non-loopback listen address. Its
first shipped slice exposes node health and the same side-effect-free readiness
report as `snowcat-cockpit doctor`.

The process owns only non-secret local state:

- a random stable node ID;
- creation and update timestamps;
- worker profile references, launch records, workspace inventory, and coarse
  lifecycle events.

It never stores terminal output, provider or MCP credentials, Snowcat lease
tokens, or authoritative queue records.

Each managed host worker has one isolated Git worktree, stable non-secret
worker ID, and dedicated tmux server as decided by
[ADR-0003](../adr/0003-isolate-each-managed-worker-terminal.md). The provider
inherits the node process environment at launch. Cockpit records neither that
environment nor terminal contents. Exited terminals and workspaces remain until
an explicit stop or cleanup request.

The dashboard opens an individual terminal by starting one loopback-only ttyd
process attached to that worker's tmux socket. The URL is runtime-only and the
node stops ttyd on shutdown without stopping the retained tmux server. The
terminal remains a writable credential-bearing administrative surface.

## Readiness

Readiness is a list of individually actionable checks rather than one boolean.
The node checks host tools separately from worker profiles. Structural profile
inspection verifies provider executables and byte-exact canonical Snowcat
skills against a locked source revision. An explicit live preflight then gives
one provider access only to `list_work`, proves both intended role skills are
visible, and records a coarse 15-minute receipt. A different provider's failure
does not erase that readiness. Presence of configuration alone is never
connection evidence. Later slices add workspace, container-isolation, trust,
and permission checks through the same profile model.

`doctor` is read-only: it does not create the state directory, mutate provider
configuration, register a worker, or call `claim_work`.

## Worker profiles

A worker profile binds one role and provider now, and will add an execution
adapter,
canonical Snowcat skill set, queue selection rule, workspace policy,
permission posture, and directory-trust posture.

The initial roles are:

- **Discoverer:** receives only kinds ending in `-discovery`, performs one
  read-only assessment, and proposes at most one bounded child. The child must
  explicitly declare Snowcat's delivery contract; operator admission remains
  in Snowcat.
- **Implementer:** receives exact kinds derived from the live queue under an
  operator-selected implementation rule and uses Snowcat's canonical general
  worker lifecycle. The default rule accepts kinds ending in `-fix` plus exact
  `pr-cure` and `pr-cure-change`. Exact `pr-review` and discovery kinds are
  excluded.
- **Reviewer:** receives only exact `pr-review` and uses Snowcat's canonical
  review-only lifecycle.

Cockpit passes a stable non-secret worker identity containing the node and slot
IDs so its local process can be correlated with Snowcat bookkeeping. The worker
still calls `claim_work` and owns the returned lease token.

## Execution adapters

The host adapter runs an installed provider CLI from an isolated worktree and
keeps its terminal in tmux. It is the compatibility path for interactive auth,
trust, and permission flows.

The first host slice creates a unique `cockpit/<worker-id>` branch from an
operator-selected local commit, installs the locked kit into the isolated
worktree, and hides only those generated skill paths from Git through
process-local configuration. It does not fetch or infer a remote default
branch. Its lifecycle follows the [managed-worker contract](../specs/managed-workers.md).

The OCI adapter runs one worker per container with a non-root user, a pinned
image, an isolated workspace, and provider configuration projected through a
separately specified ephemeral boundary. Rootless Podman is preferred when it
is available; Docker is supported explicitly rather than selected as a claim
about stronger isolation.

The dashboard and state model do not depend on which adapter owns a worker.

## Dashboard

The dashboard grows in vertical slices:

1. node identity and readiness;
2. provider and worker-profile readiness;
3. explicit discoverer, implementer, and reviewer batch controls;
4. worker cards with state, Snowcat item, workspace, pull request, and terminal
   attachment;
5. retained workspace inspection and explicit cleanup.

The visual shell follows Frostyard's
[canonical Pilothouse admin-console kit](https://github.com/frostyard/core/tree/main/.agents/skills/frostyard-design/ui_kits/pilothouse):
a compact sidebar, cold-blue ink surfaces, square gradient cards, hairline
separators, mono status furniture, and typographic symbols. Cockpit copies the
required styles into its embedded assets so the node remains self-contained;
it does not load the design kit, fonts, or scripts from a CDN at runtime.

Snowcat proposal admission, prioritization, requeue, and other authoritative
queue mutations remain in Snowcat's operator surface. Cockpit links there
rather than reproducing those controls.

Phase 4's accepted [queue-observation decision](../adr/0004-observe-snowcat-once-to-plan-bounded-fleets.md)
adds only an operator-triggered, bounded `list_work` projection for planning a
single fleet batch. It introduces no poller, refill loop, queue copy, or
worker-side lease handling; Snowcat remains authoritative when each launched
worker claims. The exact projection, role classifier, contract warning, and
12-worker batch ceiling are defined by the
[queue and fleet contract](../specs/queue-observation-and-fleets.md).

## Operational notes

- Start the node from the complete environment needed by host workers.
- Treat the web surface as administrative even before terminals are embedded.
- Remote access belongs behind SSH forwarding, a private mesh, or an
  authenticated proxy; Cockpit does not weaken its loopback default.
- Stopping Cockpit must not automatically delete a workspace or retained tmux
  session.
- A missing optional provider or container runtime is a visible readiness
  result, not a node startup failure.

## References

- Rationale: [ADR-0002](../adr/0002-build-a-node-local-cockpit-appliance.md)
- Contracts: [node CLI and HTTP API](../specs/node-api.md)
- Profile contract: [worker profiles and locked skill kit](../specs/worker-profiles.md)
- Live readiness contract: [provider preflight](../specs/provider-preflight.md)
- Queue and batch contract: [queue observation and bounded fleets](../specs/queue-observation-and-fleets.md)
- Built in: [production roadmap](../plans/0002-production-roadmap.md)
