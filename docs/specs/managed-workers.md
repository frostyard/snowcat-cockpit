# Spec: Managed workers

This contract governs one Cockpit-owned worker launch, its isolated Git
worktree, retained tmux terminal, local lifecycle record, and explicit stop and
cleanup operations. The node CLI, HTTP API, and dashboard consume the same
manager.

## Interface

A launch request contains:

| Field | Type | Required | Constraints |
| --- | --- | --- | --- |
| `adapter` | string | no | Exact `host` or `oci`; defaults to `host` |
| `runtime` | string | no | With `oci`, exact `podman` or `docker`; defaults to `podman`. Forbidden with `host` |
| `provider` | string | yes | Exact `codex`, `claude`, or `copilot`; its current profile is `ready` |
| `role` | string | yes | Exact `discoverer`, `implementer`, or `reviewer` |
| `repository` | string | yes | Snowcat `owner/name` slug |
| `source` | string | yes | Existing local Git working tree used only as the worktree source |
| `baseRef` | string | no | Local commit-ish to verify; defaults to `HEAD` |

The manager returns a non-secret record:

```json
{
  "version": 1,
  "id": "worker-<hex>",
  "nodeId": "node-<hex>",
  "adapter": "host",
  "runtime": "podman or docker when adapter is oci",
  "runtimePosture": "rootless or rootful when adapter is oci",
  "provider": "claude",
  "model": "gpt-5.6-terra or omitted",
  "role": "implementer",
  "repository": "frostyard/firn",
  "source": "/absolute/source",
  "workspace": "/state/workspaces/worker-<hex>/checkout",
  "baseRef": "HEAD",
  "baseCommit": "<commit>",
  "branch": "cockpit/worker-<hex>",
  "status": "allocating|running|exited|failed|stopped|cleaned",
  "createdAt": "RFC3339",
  "startedAt": "RFC3339 or omitted",
  "stoppedAt": "RFC3339 or omitted",
  "cleanedAt": "RFC3339 or omitted",
  "detail": "short non-secret lifecycle detail"
}
```

CLI operations:

```text
snowcat-cockpit workers [--json] [--state-dir <directory>]
snowcat-cockpit worker launch --adapter <host|oci> --provider <name> --role <name> --repository <owner/name> --source <directory> [--base-ref <ref>]
snowcat-cockpit worker observe [--json] [--state-dir <directory>] <worker-id>
snowcat-cockpit worker attach [--state-dir <directory>] <worker-id>
snowcat-cockpit worker stop [--state-dir <directory>] <worker-id>
snowcat-cockpit worker cleanup [--state-dir <directory>] <worker-id>
```

HTTP operations:

| Method | Path | Result |
| --- | --- | --- |
| `GET` | `/api/v1/workers` | Current records reconciled with tmux |
| `POST` | `/api/v1/workers/base` | Resolve the selected commit and compare it with its configured local upstream |
| `POST` | `/api/v1/workers` | Launch one worker from a JSON request |
| `POST` | `/api/v1/workers/{id}/observe` | Take one exact Snowcat attempt observation |
| `POST` | `/api/v1/workers/{id}/stop` | Stop that worker's tmux server |
| `POST` | `/api/v1/workers/{id}/console` | Start or reuse one loopback ttyd console and return its local URL |
| `DELETE` | `/api/v1/workers/{id}` | Clean a non-running worker workspace |

## Rules

1. Launch MUST create a unique branch and isolated Git workspace beneath the
   configured Cockpit state directory. `host` MUST use a linked worktree and
   MUST NOT clone, fetch, or pull. `oci` MUST use the self-contained local
   clone defined by the [rootless OCI contract](oci-workers.md), without a
   network fetch or pull. Neither mode may mutate Snowcat during allocation.
2. Launch MUST install the locked skills into `.agents/skills` and
   `.claude/skills` in the isolated worktree and hide only those Cockpit-owned
   paths from the worker's Git commands through process-local Git configuration.
3. The role prompt MUST identify the stable worker ID, select only the role's
   exact bounded kinds, claim at most one item, and tell the provider to stop
   after reporting the result. A discoverer MUST remain read-only, select only
   `*-discovery`, and declare `requiredArtifact` on every proposed child. An
   implementer MUST release a claimed change item before substantive work when
   `open-pr` or `requiredArtifact: pull-request` is absent; the role requires a
   deliverable pull-request artifact and never widens the item's authority.
   Its claim set is the exact observed claimable kinds—queued plus claimed
   items whose newest attempt is expired—after excluding discovery, review,
   and human-operated `release-needed` work; it MUST NOT use a closed
   implementation-kind whitelist. The worker MUST keep the preallocated
   current branch. When `open-pr` and `requiredArtifact: pull-request` are both present,
   Cockpit's prompt MUST state that the operator has authorized committing,
   pushing that branch, and opening the required draft pull request without a
   second permission prompt. Every role prompt MUST request a 3600-second
   claim lease, renew it immediately after claim, and renew it before and after
   installs, builds, tests, and network steps. A worker that learns its lease
   is no longer active MUST stop before any further repository or GitHub
   mutation.
4. Each worker MUST use the dedicated tmux topology from
   [ADR-0003](../adr/0003-isolate-each-managed-worker-terminal.md) with
   `remain-on-exit` enabled before the provider starts.
5. Cockpit MUST preserve argv boundaries, MUST NOT use `eval`, and MUST NOT put
   inherited environment values into tmux arguments, logs, records, or API
   responses.
6. A launch record MUST contain no provider credential, MCP credential, lease
   token, terminal content, provider output, environment dump, or Snowcat queue
   record.
7. Stop MUST address one exact worker and MUST retain its workspace and record.
8. Cleanup MUST be explicit, MUST refuse a running or dirty workspace, MUST
   remove only byte-matching Cockpit skill files, and MUST retain a `cleaned`
   lifecycle record. It MUST NOT delete the worker branch. Before deleting a
   clean OCI checkout, it MUST import that checkout's exact worker branch into
   the source repository.
9. Failed allocation or launch MUST remain recorded and MUST NOT trigger
   automatic worktree or terminal deletion.
10. A launch is local process creation, not evidence that Snowcat work was
    claimed or completed.
11. Work observation MUST be explicit, MUST follow the
    [queue observation contract](queue-observation-and-fleets.md), and MUST NOT
    modify the worker record. Process state and Snowcat attempt state are
    independent. After an explicit observation, the dashboard MUST present an
    expired Snowcat attempt beside a still-running provider process as a
    lease/process conflict rather than treating either state as the other's
    outcome.
12. The dashboard console MUST bind ttyd to the platform loopback interface,
    MUST allow at most one writable client, MUST attach only to the selected
    worker socket, and MUST stop with the Cockpit node without stopping tmux.
    Cockpit MUST NOT proxy, capture, or persist its terminal contents.
13. `host` retains the interactive provider behavior above. `oci` additionally
    MUST follow the [rootless OCI worker](oci-workers.md) contract. The adapter
    is explicit in each request and persisted in the non-secret worker record.
14. A runtime-selected model MAY be persisted as non-secret worker metadata.
    OCI role models MUST follow the rootless OCI contract; Cockpit MUST NOT
    silently claim a different-model review when the selected model matches the
    review origin.
15. Before dashboard launch, Cockpit MUST show the selected immutable commit
    and its ahead/behind relation to the base ref's configured local upstream.
    A behind or diverged relation requires explicit operator confirmation.
    Inspection MUST NOT fetch, pull, choose a different ref, or treat a local
    tracking ref as proof of current remote state.

## Derived artifacts

| Artifact | Derivation |
| --- | --- |
| Worker role prompt | Worker ID, repository, and locked profile role |
| Workspace branch | `cockpit/<worker-id>` |
| tmux socket | Private short runtime directory plus node and worker IDs |
| Dashboard worker inventory | `GET /api/v1/workers` |
| Live Snowcat work state | Explicit `worker observe` or dashboard action; never persisted |

## References

- Rationale: [ADR-0002](../adr/0002-build-a-node-local-cockpit-appliance.md),
  [ADR-0003](../adr/0003-isolate-each-managed-worker-terminal.md),
  [ADR-0006](../adr/0006-use-self-contained-git-directories-for-oci-workers.md),
  [ADR-0009](../adr/0009-observe-reclaimable-snowcat-work.md)
- Context: [Cockpit node](../design/node.md)
- Built in: [Production roadmap, Phase 3](../plans/0002-production-roadmap.md#phase-3--launch-one-managed-worker)
- Unattended boundary: [rootless OCI workers](oci-workers.md)
