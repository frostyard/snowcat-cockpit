# Spec: Managed repositories and board campaigns

This contract governs node-local repository enrollment, retained source setup,
and the operator-started controller that launches bounded Snowcat workers
across every enrolled repository.

## Managed repository interface

`GET /api/v1/repositories` returns records ordered by normalized repository
slug. `POST /api/v1/repositories` accepts:

```json
{"repository":"frostyard/updex"}
```

Enrollment is idempotent ignoring slug case. The node derives `source` beneath
its configured managed-source root; callers cannot supply a filesystem path.
There is no automatic delete or unenroll operation.

`POST /api/v1/repositories/{owner}/{name}/setup` converges one retained source
and returns this shape:

| Field | Type | Constraints |
| --- | --- | --- |
| `repository` | string | normalized `owner/name` |
| `source` | string | node-derived absolute path |
| `baseRef` | string | refreshed `origin/<default>` ref |
| `baseCommit` | string | immutable 40- or 64-hex commit when ready |
| `status` | string | `pending`, `ready`, or `failed` |
| `detail` | string | coarse non-secret result |
| timestamps | RFC3339 | created, updated, and optional prepared time |

Setup MUST:

- create a missing source with `gh repo clone` without putting credentials in
  argv or state;
- accept only a real directory with an origin that names the record's exact
  repository on `github.com` and contains no embedded HTTPS user info;
- refuse a dirty working tree;
- run `git fetch --prune origin`, resolve `origin/HEAD` or GitHub's reported
  default branch, and pin its immutable commit;
- never pull, merge, force-reset, delete, or repurpose the source checkout;
- persist no command output or credential-bearing error text.

## Campaign interface

`GET /api/v1/campaign` returns the most recent campaign record.
`POST /api/v1/campaign` starts one campaign and returns HTTP 202. The request
is:

```json
{
  "adapter": "oci",
  "runtime": "podman",
  "intervalSeconds": 30,
  "discoverer": {"provider": "claude", "mcpServer": "snowcat", "capacity": 4},
  "implementer": {"provider": "claude", "mcpServer": "snowcat", "capacity": 4},
  "reviewer": {"provider": "copilot", "mcpServer": "snowcat-mcp", "capacity": 4}
}
```

| Input | Constraints |
| --- | --- |
| `adapter` | `host` or `oci`; default `host` |
| `runtime` | empty for host; `podman` or `docker` for OCI; default `podman` |
| `intervalSeconds` | 10 through 300; default 30 |
| lane `provider` | non-empty installed provider ID |
| lane `mcpServer` | non-empty provider-local MCP server name |
| lane `capacity` | 1 through 12; all three capacities total at most 12 |

One provider MUST NOT name two MCP servers in one campaign because live
preflight receipts are provider-scoped. `POST /api/v1/campaign/stop` cancels
future controller calls and returns the current stopping or stopped record.

Campaign records contain only campaign ID/status/configuration, coarse
repository and provider status, worker IDs launched by the campaign, and
timestamps. They MUST NOT contain a queue item, attempt payload, environment,
terminal output, provider credential, observer credential, or lease token.
State files are mode `0600` where Unix permissions apply.

## Reconciliation rules

1. Only one campaign may be active on a node. Active states are `starting`,
   `running`, `degraded`, and `stopping`.
2. Start requires at least one locally enrolled repository. It prepares all
   repositories with concurrency at most four.
3. Start refreshes each distinct provider/MCP-server pair. One failed live
   proof receives exactly one immediate retry. A failed pair is retried no
   sooner than five minutes; a ready receipt is refreshed before expiry.
4. Each tick observes every ready repository with concurrency at most four and
   never faster than the configured interval.
5. For each lane, launches are capped by eligible queued work and the lane's
   remaining global capacity after all active node workers in that lane are
   counted. Repository iteration is sorted and round-robin by pass.
6. Every launch uses the repository's prepared immutable base commit and the
   campaign's exact adapter, runtime, and lane provider. The worker independently
   claims at most one item through Snowcat MCP.
7. A setup, preflight, observation, or launch failure records only a sanitized
   message. Setup, preflight, and launch retry no sooner than five minutes.
8. Empty queue observations leave the campaign running. Human proposal
   admission and delayed pull-request verification may add later work.
9. Stop and process shutdown MUST NOT stop a worker, delete a source, delete a
   workspace, or infer a Snowcat outcome from provider exit.
10. On node restart, a previously active durable record becomes `stopped` with
    an interrupted detail. The node MUST NOT resume launches automatically.

## References

- Rationale:
  [ADR-0008](../adr/0008-run-persistent-multi-repository-board-campaigns.md)
- Context: [board campaigns](../design/board-campaigns.md),
  [Cockpit node](../design/node.md)
- Worker contracts: [managed workers](managed-workers.md),
  [provider preflight](provider-preflight.md),
  [queue observation and bounded fleets](queue-observation-and-fleets.md)
