# 0006 — Use self-contained Git directories for OCI workers

- **Status:** Accepted
- **Date:** 2026-08-21

## Context

The first live rootless-Podman trial launched one Codex implementer and one
Codex reviewer from ordinary Cockpit linked worktrees. Both containers exited
before a Snowcat claim. Their retained terminals reported that Git could not
open the worktree administrative directory beneath the source checkout.

A linked worktree is not self-contained: its `.git` file names an absolute
directory beneath the source repository's common Git directory. The OCI
boundary deliberately mounts only the worker workspace, so that absolute host
path is unavailable. Mounting the source repository's common Git directory
read-write would expose its configuration, hooks, refs, objects, and every
other linked worktree to an unattended worker.

## Decision

Keep linked worktrees for interactive host workers. Allocate an OCI worker as
a self-contained local clone beneath the same retained workspace root. The
clone copies objects without hardlinks and performs no network operation. It
checks out the worker's unique `cockpit/<worker-id>` branch at the exact local
base commit.

Cockpit verifies that the source repository's existing push URL names the
request's exact repository on `github.com`, accepting credential-free HTTPS or
the ordinary GitHub SSH forms. It then gives the clone a canonical
credential-free HTTPS origin so the ephemeral GitHub CLI credential helper can
authenticate without an SSH-agent mount. It does not persist either URL in
Cockpit state. OCI-only Git exclusions live in the clone's private
`.git/info/exclude`, which is visible inside the container.

Explicit cleanup first verifies a clean checkout, removes only byte-matching
Cockpit skill files, and fetches the exact worker branch back into the source
repository. Only then may it recursively remove the exact workspace path
derived from the worker ID. A failed import retains the standalone checkout.

## Consequences

- The OCI container needs only the existing `/workspace` mount and cannot
  reach or mutate the source repository's Git administration files.
- OCI allocation uses additional disk proportional to the repository's Git
  object database; hardlinks and alternates are forbidden because they would
  reconnect the lifecycle to source-owned storage.
- A worker's local branch remains inspectable from the source repository after
  explicit cleanup, matching the host-worker retention contract.
- The first slice supports repositories on `github.com`; the source origin
  must identify the requested repository. Embedded HTTP credentials fail
  closed.
- Host and OCI workers share lifecycle records and UI but intentionally use
  different Git allocation mechanisms.

## Alternatives considered

- **Mount the source common Git directory:** rejected because an unattended
  worker could alter source configuration, hooks, refs, objects, and sibling
  worktrees.
- **Rewrite the linked-worktree pointer inside the container:** rejected
  because the required common objects and refs would still be outside the
  mounted workspace.
- **Clone from the network:** rejected because allocation would depend on
  remote availability and could omit the operator-selected local commit.
- **Use a local clone with hardlinks or alternates:** rejected because it would
  not be a self-contained filesystem boundary.

## References

- Shapes: [Cockpit node](../design/node.md),
  [managed workers](../specs/managed-workers.md),
  [rootless OCI workers](../specs/oci-workers.md),
  [production roadmap](../plans/0002-production-roadmap.md)
- Builds on: [ADR-0003](0003-isolate-each-managed-worker-terminal.md),
  [ADR-0005](0005-isolate-unattended-workers-in-rootless-oci.md)
