# 0003 — Isolate each managed worker terminal

- **Status:** Accepted
- **Date:** 2026-08-21

## Context

The Phase 3 worker manager must retain a worker terminal after its provider
exits, identify that terminal after a Cockpit restart, and stop or clean up one
worker without disturbing another. A shared tmux server also captures its
environment only when the first worker starts, so later workers could inherit
stale provider or MCP environment from an unrelated launch.

## Decision

Give every managed worker its own dedicated tmux server and one session named
`worker` containing one window named `console`. Put its socket beneath a short
`0700` node-specific runtime directory derived from the non-secret node and
worker IDs. This avoids platform Unix-socket path limits without using tmux's
user-global socket namespace. The server inherits the Cockpit process
environment at launch and is retained after the provider exits.

Stopping or cleaning up a worker addresses only that worker's socket. Cockpit
does not capture terminal contents and never copies environment values into
launch arguments or persisted state.

The Bash spike keeps its shared-session topology until the managed path fully
replaces it.

## Consequences

- Worker launch, attachment, stop, and cleanup are independently addressable.
- A fresh server captures the current inherited environment for every worker.
- One failed tmux server cannot take down other workers.
- Socket access follows a private per-node runtime-directory boundary instead
  of tmux's user-global temporary directory.
- A fleet creates more tmux server processes than a shared-session design.
- A combined terminal wall requires a separate presentation layer; the worker
  manager exposes individual attachment first.

## Alternatives considered

- **One shared tmux session:** uses fewer processes and matches the spike, but
  couples cleanup and environment lifetime across unrelated workers.
- **One server with one session per worker:** improves naming but retains the
  shared environment and server-failure boundary.
- **Custom PTY ownership in the node:** duplicates the durable terminal behavior
  already proven with tmux and expands the credential-bearing attack surface.

## References

- Shapes: [Cockpit node](../design/node.md),
  [managed workers](../specs/managed-workers.md)
- Built in: [Production roadmap, Phase 3](../plans/0002-production-roadmap.md#phase-3--launch-one-managed-worker)
- Builds on: [ADR-0002](0002-build-a-node-local-cockpit-appliance.md)
