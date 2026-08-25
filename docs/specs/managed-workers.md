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
| `mcpServer` | string | no | Provider-local direct Snowcat server to replace for this invocation; defaults to `snowcat`, or `snowcat-mcp` for Copilot |
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
  "mcpServer": "snowcat",
  "model": "gpt-5.6-terra or omitted",
  "role": "implementer",
  "repository": "frostyard/firn",
  "source": "/absolute/source",
  "workspace": "/state/workspaces/worker-<hex>/checkout",
  "baseRef": "HEAD",
  "baseCommit": "<commit>",
  "branch": "cockpit/worker-<hex>",
  "itemId": "claimed item UUID when an existing pull request is targeted",
  "workKind": "pr-cure|pr-cure-change|pr-review|pr-review-fix when targeted",
  "pullRequestUrl": "bound pull request when targeted",
  "targetRepository": "GitHub head repository when targeted",
  "targetBranch": "GitHub head branch when targeted",
  "targetHead": "40-hex head bound at claim time when targeted",
  "targetMode": "branch|detached when targeted",
  "targetedAt": "RFC3339 or omitted",
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
snowcat-cockpit worker launch --adapter <host|oci> --provider <name> [--mcp-server <name>] --role <name> --repository <owner/name> --source <directory> [--base-ref <ref>]
snowcat-cockpit worker lease-proxy --worker <id> --workspace <directory>
snowcat-cockpit worker target --worker <id> --repository <owner/name> --item <uuid> --kind <kind> --pull-request <url> --head <sha>
snowcat-cockpit worker push-target --worker <id>
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

1. Launch MUST require a current ready provider receipt for the exact selected
   `mcpServer` plus non-empty `SNOWCAT_MCP_URL` and
   `SNOWCAT_MCP_TOKEN` in the node environment before creating a branch or
   workspace. Optional `SNOWCAT_CF_ACCESS_CLIENT_ID` and
   `SNOWCAT_CF_ACCESS_CLIENT_SECRET` values MUST be present together; when
   configured, the worker-local relay MUST send the corresponding Cloudflare
   Access headers on every upstream request. All credential values MUST remain
   outside argv, files, logs, durable state, and API responses. Launch MUST
   create a unique branch and isolated Git workspace beneath the
   configured Cockpit state directory. `host` MUST use a linked worktree and
   MUST NOT clone, fetch, or pull. `oci` MUST use the self-contained local
   clone defined by the [rootless OCI contract](oci-workers.md), without a
   network fetch or pull. Neither mode may mutate Snowcat during allocation.
2. Launch MUST install the locked skills into `.agents/skills` and
   `.claude/skills` in the isolated worktree and hide only those Cockpit-owned
   paths from the worker's Git commands through process-local Git configuration.
   It MUST also copy the exact running Cockpit executable to the private
   `.agents/bin/snowcat-cockpit` path used by the role prompt, so host and OCI
   workers invoke the node's matching target and lease-relay protocols without
   relying on `PATH` or an independently versioned image copy.
3. The role prompt MUST identify the stable worker ID, select only the role's
   exact bounded kinds, claim at most one item, and tell the provider to stop
   after reporting the result. A discoverer MUST remain read-only, select only
   `*-discovery`, and declare `requiredArtifact` on every proposed child. An
   implementer MUST release a claimed change item before substantive work when
   `write`, `open-pr`, or `requiredArtifact: pull-request` is absent; the role
   requires a deliverable pull-request artifact, never infers `write` from
   `open-pr`, and never widens the item's authority.
   Its claim set is the exact observed claimable kinds—queued plus claimed
   items whose newest attempt is expired—after excluding discovery, review,
   and human-operated `release-needed` work; it MUST NOT use a closed
   implementation-kind whitelist. For ordinary new-pull-request work, the
   worker MUST keep the preallocated current branch. When `write`, `open-pr`, and
   `requiredArtifact: pull-request` are all present, Cockpit's prompt MUST
   state that the operator has authorized committing and the item's required
   delivery without a second permission prompt. Every managed provider MUST
   receive Snowcat tools only through the projected worker-local MCP relay for
   that invocation. The relay MUST replace the configured direct Snowcat
   server, bind `claim_work` and `heartbeat_work` to 120 seconds, and renew the
   one active lease every 30 seconds while the provider retains its stdio
   process. It MUST stop renewing on EOF. A definitive renewal rejection, or
   failure to renew before the last Snowcat-reported expiry, MUST emit
   `SNOWCAT_COCKPIT_LEASE_LOST`, write the credential-free local lifecycle
   marker, and refuse later provider tool calls. A worker that receives that
   signal MUST stop before any further repository or GitHub mutation. The
   relay MUST mark `completeAttempted` before forwarding `complete_work` and
   `completeAcknowledged` only after a successful Snowcat MCP tool response.
   A managed Codex invocation MUST allowlist the upstream URL, Snowcat token,
   and optional Cloudflare Access credential environment-variable names for its
   stdio relay without placing any value in argv.
   Immediately after claiming `pr-cure`, `pr-cure-change`,
   `pr-review`, or `pr-review-fix`, the worker MUST call `worker target` with
   the claimed item's exact non-secret ID, kind, pull-request URL, and bound
   head before inspecting or changing the tree. `pr-cure-change` resolves the
   binding from its root `pr-cure` item. Missing metadata or a moved head MUST
   cause release without substantive work. `worker target` MUST inspect the
   bound pull request's state before fetching and refuse a merged or closed
   pull request with that reason; the worker MUST then `block_work` with the
   same reason rather than release, because no later worker can deliver it.
   A failed fetch MUST name the head branch, its repository, and the last
   line of Git's output so the refusal is diagnosable without retrying the
   helper elsewhere. Review targets MUST be detached at
   the exact head. Writable targets MUST keep the unique local Cockpit branch
   but reset it to the exact bound head; every push MUST use `worker
   push-target`, which rechecks GitHub and uses an exact force-with-lease
   against the last observed head. Ordinary `git push` is forbidden for bound
   work.
4. Each worker MUST use the dedicated tmux topology from
   [ADR-0003](../adr/0003-isolate-each-managed-worker-terminal.md) with
   `remain-on-exit` enabled before the provider starts.
5. Cockpit MUST preserve argv boundaries, MUST NOT use `eval`, and MUST NOT put
   inherited environment values into tmux arguments, logs, records, or API
   responses.
6. A launch record MUST contain no provider credential, MCP credential, lease
   token, terminal content, provider output, environment dump, or complete
   Snowcat queue record. Durable node state MAY persist only the non-secret
   existing-PR target projection listed above after the helper has verified the
   workspace. The private workspace MAY contain the lifecycle marker's version,
   worker ID, item ID, coarse status, `completeAttempted`,
   `completeAcknowledged`, and update time. The node MUST NOT import that marker
   into its record or treat it as queue state.
7. Stop MUST address one exact worker and MUST retain its workspace and record.
8. Cleanup MUST be explicit, MUST refuse a running or dirty workspace, MUST
   remove only Cockpit skill files whose bytes match the kit the worker was
   launched with (the launch records that kit's source revision and per-skill
   digests on the worker record; a record that predates the field is compared
   against the node's current lock), and MUST retain a `cleaned` lifecycle
   record. A skill file matching neither MUST be refused unless the operator
   passes `--discard-drifted-skills`, in which case the record names each
   discarded skill. It MUST NOT delete the worker branch. Before deleting a
   clean OCI checkout, it MUST import that checkout's exact worker branch into
   the source repository.
9. Failed allocation or launch MUST remain recorded and MUST NOT trigger
   automatic worktree or terminal deletion.
10. A launch is local process creation, not evidence that Snowcat work was
    claimed or completed. The lifecycle marker is evidence about relay-local
    transmission and acknowledgement only; Snowcat remains authoritative for
    attempt outcome.
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
| Existing-PR target marker | Private `.agents/cockpit-target.json`, verified and imported into the durable worker record |
| Existing-PR push lease | Last GitHub head observed by `worker target` or a successful `worker push-target` |
| Worker lifecycle marker | Private `.agents/cockpit-lifecycle.json`, written by the worker-local relay and never imported into node state |
| tmux socket | Private short runtime directory plus node and worker IDs |
| Dashboard worker inventory | `GET /api/v1/workers` |
| Live Snowcat work state | Explicit `worker observe` or dashboard action; never persisted |

## References

- Rationale: [ADR-0002](../adr/0002-build-a-node-local-cockpit-appliance.md),
  [ADR-0003](../adr/0003-isolate-each-managed-worker-terminal.md),
  [ADR-0006](../adr/0006-use-self-contained-git-directories-for-oci-workers.md),
  [ADR-0009](../adr/0009-observe-reclaimable-snowcat-work.md),
  [ADR-0010](../adr/0010-bind-managed-leases-to-worker-liveness.md)
- Context: [Cockpit node](../design/node.md)
- Built in: [Production roadmap, Phase 3](../plans/0002-production-roadmap.md#phase-3--launch-one-managed-worker)
- Unattended boundary: [rootless OCI workers](oci-workers.md)
