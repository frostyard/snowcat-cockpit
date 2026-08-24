# Cockpit node

Living document. Rationale:
[ADR-0002](../adr/0002-build-a-node-local-cockpit-appliance.md),
[ADR-0011](../adr/0011-run-the-node-as-a-systemd-user-service.md).
Contracts: [node CLI and HTTP API](../specs/node-api.md),
[Linux node user service](../specs/node-service.md).

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

On Linux, `snowcat-cockpit node install` makes that process a systemd user
service instead of keeping it in an operator tmux session. The installer copies
the exact executable and reviewed credential wrapper into a content-addressed
release, atomically selects it, and converges one fixed user unit. Status and
restart require both active systemd state and a version-matched loopback health
response. The generated environment contains only explicitly allowlisted
non-secret paths, pinned image references, and the observer and worker
credential paths; credentials remain in their existing protected files and
keyrings.

Systemd owns only the node process. The unit uses `KillMode=process`, so a node
restart or explicit service uninstall leaves dedicated worker tmux servers and
every retained workspace intact. The restarted node marks an interrupted
campaign stopped and never resumes it automatically. Service uninstall also
retains content-addressed releases and all execution state.

Each managed worker has one isolated Git worktree, stable non-secret
worker ID, and dedicated tmux server as decided by
[ADR-0003](../adr/0003-isolate-each-managed-worker-terminal.md). The provider
inherits the node process environment at launch. Cockpit records neither that
environment nor terminal contents. An OCI worker instead receives only the
explicitly projected inputs described below. Exited terminals and workspaces
remain until an explicit stop or cleanup request.

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
connection evidence. OCI launch adds its own fail-closed runtime, image, input
metadata, and token-presence readiness checks before allocating a workspace.

`doctor` is read-only: it does not create the state directory, mutate provider
configuration, register a worker, or call `claim_work`.

## Worker profiles

A worker launch binds one role, provider, and explicit execution adapter plus a
canonical Snowcat skill set, queue selection rule, workspace policy,
permission posture, and directory-trust posture.

The initial roles are:

- **Discoverer:** receives only kinds ending in `-discovery`, performs one
  read-only assessment, and proposes at most one bounded child. The child must
  explicitly declare Snowcat's delivery contract; operator admission remains
  in Snowcat.
- **Implementer:** derives exact kinds from the live queue and accepts every
  worker kind except `*-discovery`, exact `pr-review`, and human-operated
  `release-needed`. Cure and review-fix work prepares the exact pull-request
  head after claim and uses a moved-head-safe push helper; other future worker
  kinds remain eligible through the classifier fallback.
- **Reviewer:** receives only exact `pr-review` and uses Snowcat's canonical
  review-only lifecycle.

Cockpit passes a stable non-secret worker identity containing the node and slot
IDs so its local process can be correlated with Snowcat bookkeeping. The worker
still calls `claim_work` and owns the returned lease token. Its worker-local MCP
relay forces a 120-second lease, renews every 30 seconds while its stdio process
is alive, and fails closed with `SNOWCAT_COCKPIT_LEASE_LOST`. A private
credential-free marker distinguishes `complete_work` attempted from Snowcat
acknowledged; it is execution evidence, not queue authority. See
[ADR-0010](../adr/0010-bind-managed-leases-to-worker-liveness.md).

## Execution adapters

The host adapter runs an installed provider CLI from an isolated worktree and
keeps its terminal in tmux. It is the compatibility path for interactive auth,
trust, and permission flows.

The first host slice creates a unique `cockpit/<worker-id>` branch from an
operator-selected local commit, installs the locked kit into the isolated
worktree, and hides only those generated skill paths from Git through
process-local configuration. It does not fetch or infer a remote default
branch. After a worker directly claims work bound to an existing pull request,
the worker-local Cockpit helper verifies GitHub's exact head, prepares that
tree without changing the unique local branch name, and records the non-secret
target projection. Reviews use a detached exact head; writes push only through
an exact force-with-lease. Its lifecycle follows the
[managed-worker contract](../specs/managed-workers.md).

The OCI adapter runs Codex, Claude, or Copilot once per container with a non-root user,
a runtime-specific SHA-256-pinned local image, a self-contained local Git clone,
and exact provider and GitHub configuration files copied from read-only mounts into tmpfs. The
clone avoids exposing the source repository's common Git directory and copies
objects without hardlinks or network access. Runtime selection is explicit:
rootless Podman is the default and preferred boundary; Docker may be selected
explicitly and its detected rootless or rootful daemon posture is retained and
shown to the operator. Rootful Docker is compatibility, not host isolation.

The projection and unattended-permission boundary is recorded in
[ADR-0005](../adr/0005-isolate-unattended-workers-in-rootless-oci.md) and made
executable by the [rootless OCI worker contract](../specs/oci-workers.md).
Adapter selection is always explicit; an omitted adapter remains interactive
host mode for compatibility.

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

The measured multi-provider and multi-repository OCI trial later justified an
explicit persistent exception to that one-shot boundary. A
[board campaign](board-campaigns.md) prepares every repository enrolled on the
local node, refreshes provider preflight, and refills bounded role capacity from
fresh observations until the operator stops it. Cockpit repository enrollment
is local execution configuration, not a copy of Snowcat control-plane state.
The campaign also refreshes and re-pins a managed repository immediately before
each implementer launch, so a long-running campaign never spends an
implementation attempt from its startup base snapshot.

Worker lifecycle and work-attempt lifecycle are deliberately distinct. A
`running` worker record means that its provider process is still alive; it does
not assert that the provider holds a Snowcat lease or is still performing work.
The two-node Phase 4 trial proved that Snowcat grants one lease when independent
nodes race for one item, while also proving that interactive Codex and Copilot
TUIs may stay alive after completion or a no-work result. Cockpit does not infer
an outcome from an idle terminal, repository and kind, or event timing.

Snowcat's bounded attempt projection now permits an explicit one-shot
reconciliation: Cockpit asks `list_work` for the worker record's exact
repository and stable worker label, then presents `unmatched`, `claimed`, or
the terminal attempt outcome alongside the local process state. It never runs
this call on the dashboard's local inventory refresh timer and never persists
the returned item or attempt. This closes
[snowcat#192](https://github.com/frostyard/snowcat/issues/192) without turning
Cockpit into a queue poller or treating a completed lease as a dead process.

## Operational notes

- Start the node from the complete environment needed by host workers.
- Treat the web surface as administrative even before terminals are embedded.
- Remote access belongs behind SSH forwarding, a private mesh, or an
  authenticated proxy; Cockpit does not weaken its loopback default.
- Stopping Cockpit must not automatically delete a workspace or retained tmux
  session.
- A missing optional provider or container runtime is a visible readiness
  result, not a node startup failure.
- Use `snowcat-cockpit node status` and `node restart` for a Linux service;
  `journalctl --user -u snowcat-cockpit.service` reads its process logs.

## References

- Rationale: [ADR-0002](../adr/0002-build-a-node-local-cockpit-appliance.md),
  [ADR-0011](../adr/0011-run-the-node-as-a-systemd-user-service.md)
- Contracts: [node CLI and HTTP API](../specs/node-api.md),
  [node service](../specs/node-service.md)
- Profile contract: [worker profiles and locked skill kit](../specs/worker-profiles.md)
- Live readiness contract: [provider preflight](../specs/provider-preflight.md)
- Queue and batch contract: [queue observation and bounded fleets](../specs/queue-observation-and-fleets.md)
- OCI boundary: [ADR-0005](../adr/0005-isolate-unattended-workers-in-rootless-oci.md)
- OCI contract: [rootless OCI workers](../specs/oci-workers.md)
- OCI workspace boundary: [ADR-0006](../adr/0006-use-self-contained-git-directories-for-oci-workers.md)
- Built in: [production roadmap](../plans/0002-production-roadmap.md)
- Persistent orchestration:
  [ADR-0008](../adr/0008-run-persistent-multi-repository-board-campaigns.md),
  [board campaigns](board-campaigns.md)
