# 0011 — Run the node as a systemd user service

- **Status:** Accepted
- **Date:** 2026-08-23

## Context

The production Cockpit node is a long-lived process, but its first operating
trial kept that process alive in an operator-created tmux session. That was a
useful deployment scaffold, not a durable operator interface: upgrades require
reconstructing a long command, process status is separate from node health,
and startup and failure behavior are implicit.

Cockpit-managed worker terminals already have their own dedicated tmux servers.
The node must be restartable without killing those terminals, deleting their
workspaces, importing their environment into durable state, or resuming an
interrupted campaign automatically. The node launcher also needs the existing
observer and worker credentials plus provider/GitHub configuration without
copying their secrets into a unit or generated configuration file.

## Decision

On Linux, Cockpit installs and manages its node as the fixed
`snowcat-cockpit.service` systemd user unit. The `snowcat-cockpit node`
interface installs a content-addressed release, converges the unit, reports
status, restarts the service, and explicitly uninstalls the service surface.

Each release contains the exact Cockpit executable and its reviewed credential
wrapper. An atomic `current` symlink selects the release used by
the unit. The unit persists only node arguments and an allowlisted set of
non-secret environment values such as executable search paths, provider config
paths, and pinned OCI image references. It never copies provider credentials,
GitHub tokens, Snowcat MCP credentials, observer tokens, or arbitrary inherited
environment values. The wrapper reads the protected observer and worker
credential files into separate process environment variables and obtains a
GitHub token from the installed GitHub CLI only in the service process
environment.

The service uses `Restart=on-failure` for the node process and
`KillMode=process` so systemd restart and uninstall do not signal descendant
worker tmux servers. A service restart follows the existing restart contract:
an interrupted active campaign becomes stopped and requires an explicit
operator start. Installation and restart require both an active systemd unit
and a matching loopback health response before reporting success.

Uninstall disables and stops the unit and removes only Cockpit's exact unit,
selection symlink, service environment, and install record. It retains
content-addressed releases, node state, managed sources, worker records,
workspaces, branches, and tmux servers.

## Consequences

- Operators receive conventional install, status, restart, logs-through-
  journalctl, and login-start behavior without keeping the node in tmux.
- Binary selection is atomic and prior releases remain available for bounded
  rollback or inspection.
- User service configuration becomes a Cockpit-owned local artifact and must
  remain credential-free and private.
- The Linux implementation depends on a functioning systemd user manager. A
  future macOS implementation needs a separate launchd decision and adapter.
- `KillMode=process` intentionally leaves worker descendants outside normal
  systemd cgroup cleanup; Cockpit's explicit worker lifecycle remains
  authoritative for those retained processes.
- Installing the service does not terminate an existing node process that owns
  the requested port. Health/version and unit-state checks fail closed so the
  operator can migrate that process explicitly.

## Alternatives considered

- **Keep the node in tmux:** rejected as an operating scaffold with no durable
  status, startup, upgrade, or failure contract.
- **Run the node in a container:** rejected for the first slice because host
  tmux, Git, Docker/Podman, provider configuration, and retained source access
  would make the deployment boundary more complex and more credential-bearing.
- **Use a system-wide root service:** rejected because the node and its
  credentials, sources, runtimes, and workers belong to one operator account.
- **Let systemd kill the whole service cgroup:** rejected because node restart
  must not stop or erase retained worker terminals.
- **Persist the inherited environment wholesale:** rejected because it can
  copy credentials and unrelated process state into a durable unit.

## References

- Shapes: [Cockpit node](../design/node.md),
  [node service contract](../specs/node-service.md)
- Builds on: [ADR-0002](0002-build-a-node-local-cockpit-appliance.md),
  [ADR-0003](0003-isolate-each-managed-worker-terminal.md)
