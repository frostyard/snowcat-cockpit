# 0013 — Converge the node from a declared configuration

- **Status:** Proposed
- **Date:** 2026-08-25

## Context

Bringing a Cockpit node to a running campaign is seven manual steps spread
across two surfaces: `doctor`, `install-kit`, and `node install` on the CLI;
enroll and set up each repository through `POST /api/v1/repositories` and
`…/setup`; `preflight` once per provider; then `POST /api/v1/campaign`. The
four-step procedure for rolling out a Snowcat skill change in the node
service spec is the same choreography again, and the campaign contract
(ADR-0008, spec rule 11) means it repeats after every node restart, release,
and reboot, because a campaign never resumes on its own.

Two of those steps carry state in places that are easy to get wrong, and both
failed on 2026-08-24 during the first day of live campaigns:

- `node install` snapshots the **ambient shell environment** into
  `service.env` and bakes every path into the unit's `ExecStart` argv. A
  reinstall from a shell that lacked the pinned image variables silently
  dropped them; a Podman campaign then failed every launch with a message the
  campaign reduced to "launch failed; retry is backed off". The same snapshot
  had earlier carried only the `SNOWCAT_COCKPIT_DOCKER_*_IMAGE` names while the
  campaign runtime was Podman, which reads `SNOWCAT_COCKPIT_OCI_*_IMAGE`.
- The node's state directory was a hand-made `mktemp -d` path under `/tmp`
  from a trial run. Because the unit hard-codes the path, every later
  reinstall inherited it. That tree held retained worker workspaces, the
  provisioned tool caches, and the managed sources — none of which survive a
  reboot, although the retention rules (ADR-0003, ADR-0011) exist precisely so
  that nothing deletes them implicitly.

The node service already has a durable, non-secret install record
(`install.json`) and a fixed environment allowlist (ADR-0011). What it lacks
is a declared input: the operator's intent lives only in whichever shell last
ran the commands.

## Decision

A declared, non-secret node configuration file is the source of truth for a
node, and `snowcat-cockpit node up` converges the host to it idempotently.

The configuration (`$XDG_CONFIG_HOME/snowcat-cockpit/node.json` by default,
schema version 1) names the loopback listen address, the state directory, the
observer and worker credential file **paths**, the pinned worker image
references, allowlisted provider configuration paths, each provider's
MCP-server name, the enrolled repositories, and the campaign lanes. Lanes name
a provider only; the MCP server is looked up from the provider table, so one
provider names exactly one server per campaign. The worker-kit and source
roots are fixed beneath the state directory. Any value shaped like a
credential is refused at load time and never echoed.

`node up` runs the existing pieces in a fixed order — doctor, worker kit,
service install, repository enrollment and setup, provider preflight, campaign
start, status — and changes only what differs from the declared state. It
reinstalls the service only when the release, the paths, the recorded config
path, or the rendered `service.env` differ from what the configuration
produces, or when the service is unhealthy; otherwise a running campaign is
left untouched. A drifted worker kit is moved aside, never deleted. Live
proofs are reused while current and re-run once with one retry otherwise. An
active campaign is left in place; a stopped or absent one is started from the
declared lanes. `--dry-run` reports the same decisions without acting.

`service.env` and the unit's argv become derived artifacts regenerated from
the configuration and the install record names the configuration that
produced it; neither is hand-edited. Campaigns still never resume
automatically — `node up` is the sanctioned way to start one after a reboot,
a release, or a Snowcat skill change.

## Consequences

- One command replaces the seven-step bring-up and the four-step
  skill-rollout procedure, and it is safe to re-run: an unchanged, healthy
  node with a running campaign is a no-op.
- The image pins, credential paths, and state directory live in one reviewed
  file, so a reinstall from a bare shell can no longer lose them.
- There is now a file to keep in step with `install.json`. `node status`
  prints the recorded configuration path so drift between the two is visible,
  and `node up --dry-run` reports what would change.
- Repository enrollment and campaign start still go through the running
  node's loopback API, because the node owns that state; `node up` therefore
  requires the service to be healthy before those steps and stops with a
  clear step result when it is not.
- A service reinstall still restarts the node and stops the active campaign
  (ADR-0011). `node up` says so before doing it and starts a new campaign
  afterwards; workers and workspaces are retained as before.
- The configuration is Linux-and-systemd shaped like the node service it
  drives; a future launchd adapter inherits the same file.

## Alternatives considered

- **A bash wrapper in `bin/`:** rejected because it would re-implement
  credential-file validation and environment projection outside the governed
  Go path (`internal/nodeservice`), and the only sanctioned Bash surface is the
  legacy spike.
- **`serve --config` reading the file at runtime:** rejected for now because
  it widens the inputs of the long-running service process; the install-time
  boundary keeps `serve` argv-driven. It can be revisited in a later ADR.
- **systemd drop-in units or `Environment=` directives:** rejected because
  they are still ambient, hand-edited state with no validation and no link to
  the campaign or repository declarations.
- **Auto-resuming the campaign on node start:** rejected; ADR-0008 keeps
  campaign start an explicit operator act, and `node up` is that act.

## References

- Shapes: [specs/node-up.md](../specs/node-up.md),
  [specs/node-service.md](../specs/node-service.md),
  [design/node.md](../design/node.md)
- Builds on: [ADR-0008](0008-run-persistent-multi-repository-board-campaigns.md),
  [ADR-0011](0011-run-the-node-as-a-systemd-user-service.md),
  [ADR-0012](0012-provision-repository-tools-before-the-lease-and-derive-node-state-from-its-sources.md)
