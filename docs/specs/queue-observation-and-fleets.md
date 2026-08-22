# Spec: Queue observation and bounded fleets

This contract governs Cockpit's operator-triggered read projection of Snowcat
and the one-shot batch operation that launches managed workers from that
projection. The embedded dashboard and node HTTP API consume it; coding agents
continue to use Snowcat MCP directly.

## Configuration

| Environment variable | Required | Constraints |
| --- | --- | --- |
| `SNOWCAT_COCKPIT_MCP_URL` | with token | Absolute `http` or `https` MCP endpoint; no user info, query, or fragment |
| `SNOWCAT_COCKPIT_MCP_TOKEN` | with URL | Snowcat-minted bearer token; process environment only |

If both variables are absent, the node starts with queue observation
unavailable and single-worker operations unchanged. If only one is present or
the URL is invalid, `serve` MUST fail without printing the token.

The token MUST be minted with Snowcat's server-enforced `observer` profile,
which grants only `list_work` and `get_work`. Cockpit invokes only `list_work`.
A legacy token carrying the synthetic `cockpit-observer-no-claim` kind
restriction is not sufficient because a kind restriction does not restrict MCP
tools.

## Local launch wrapper

`bin/snowcat-cockpit-serve [serve options]` MUST:

- read `${XDG_CONFIG_HOME:-$HOME/.config}/snowcat/profile-observer.env`, unless a
  test supplies `SNOWCAT_COCKPIT_OBSERVER_ENV`;
- require a regular, non-symlink credential file owned by the current user
  with mode `0600`;
- parse only one literal `export SNOWCAT_OBSERVER_TOKEN=<token>` declaration
  and MUST NOT evaluate the file as shell code;
- set `SNOWCAT_COCKPIT_MCP_URL` to
  `https://snowcat.goat-snake.ts.net/mcp`;
- map the value to `SNOWCAT_COCKPIT_MCP_TOKEN`, remove
  `SNOWCAT_OBSERVER_TOKEN`, and use `exec` with preserved argument boundaries;
- run `dist/snowcat-cockpit serve`, unless a test supplies
  `SNOWCAT_COCKPIT_BIN`.

## HTTP interface

| Method | Path | Result |
| --- | --- | --- |
| `POST` | `/api/v1/queue/snapshot` | Take and return one bounded queued-work snapshot |
| `POST` | `/api/v1/fleets` | Take one fresh snapshot and launch one bounded batch |
| `POST` | `/api/v1/workers/{id}/observe` | Correlate one retained worker with its Snowcat attempt |

The snapshot and fleet endpoints require `Content-Type: application/json` and
reject unknown fields. Worker observation has no request body.

Snapshot request:

```json
{"repository":"frostyard/firn"}
```

The node makes exactly one Snowcat `list_work` call with `status: "queued"`,
the exact repository, and `limit: 100`. The response is request-only and has
this shape:

```json
{
  "repository": "frostyard/firn",
  "observedAt": "RFC3339 timestamp",
  "truncated": false,
  "flagged": 0,
  "counts": {
    "discoverer": 2,
    "implementer": 1,
    "reviewer": 1,
    "unassigned": 0
  },
  "items": [
    {
      "id": "Snowcat UUID",
      "repository": "frostyard/firn",
      "kind": "ci-signal-fix",
      "priority": 0,
      "allowedActions": ["read", "write", "run-tests", "open-pr"],
      "requiredArtifact": "pull-request",
      "contract": "ready|suspicious|unknown",
      "contractDetail": "present only for a warning",
      "role": "discoverer|implementer|reviewer|unassigned"
    }
  ]
}
```

`truncated` MUST be true when Snowcat returns exactly 100 items. `counts` are
launch-eligible counts: assigned items with `contract: ready`, plus all
unassigned items under `unassigned`. Suspicious or unknown assigned contracts
remain visible in `items`, increment `flagged`, and MUST NOT add launch capacity.

Fleet request:

```json
{
  "adapter": "oci",
  "provider": "codex",
  "role": "implementer",
  "repository": "frostyard/firn",
  "source": "/absolute/path/to/firn",
  "baseRef": "HEAD",
  "count": 3
}
```

`adapter` MUST be exact `host` or `oci` and defaults to `host`. `count` MUST be
between 1 and 12. The node takes one new snapshot inside the
fleet request, computes `planned = min(count, eligible-for-role)`, and invokes
the existing managed-worker launch exactly `planned` times. The result includes
`requested`, `eligible`, `planned`, `launched`, `failures`, and the snapshot
used for the decision. Zero planned launches return HTTP 200; a complete batch
returns 201; a partial batch returns 207.

Worker observation makes exactly one Snowcat `list_work` call with the worker
record's exact repository, its exact `worker-<hex>` ID as `label`, and
`limit: 2`. It does not filter by item status. Zero results produce
`unmatched`; two results produce `ambiguous`; one result MUST contain exactly
one matching attempt. A matching attempt with no outcome produces `claimed`;
the only accepted terminal outcomes are `completed`, `blocked`, `released`,
and `expired`.

```json
{
  "workerId": "worker-0123456789abcdef",
  "repository": "frostyard/firn",
  "observedAt": "RFC3339 timestamp",
  "status": "unmatched|ambiguous|claimed|completed|blocked|released|expired",
  "detail": "bounded non-secret explanation",
  "itemId": "Snowcat UUID when matched",
  "kind": "ci-signal-fix when matched",
  "itemStatus": "completed when matched",
  "attempt": {
    "sequence": 4212,
    "claimedAt": "RFC3339 timestamp",
    "label": "worker-0123456789abcdef",
    "outcome": "completed",
    "endedAt": "RFC3339 timestamp"
  }
}
```

The observation response is request-only. Cockpit MUST NOT store the Snowcat
item, attempt, or derived work status in its worker record. Provider process
state and Snowcat work state remain distinct.

## Role classification

Classification is deterministic and case-sensitive:

| Work kind | Role |
| --- | --- |
| suffix `-discovery` | `discoverer` |
| suffix `-fix` | `implementer` |
| exact `pr-cure` or `pr-cure-change` | `implementer` |
| exact `pr-review` | `reviewer` |
| every other kind | `unassigned` |

A contract is suspicious when `requiredArtifact: pull-request` lacks
`open-pr`, or when `write` authority does not carry both `open-pr` and
`requiredArtifact: pull-request`. An absent `requiredArtifact` is unknown.

## Rules

- Queue observation MUST occur only in direct response to an operator request.
- Cockpit MUST NOT schedule a timer, poll, watch, automatically refresh, or
  persist a snapshot or work item.
- The bearer token MUST remain in process memory and HTTP authorization only;
  it MUST NOT enter arguments, state, logs, error bodies, worker records, or a
  managed worker's inherited environment.
- Cockpit MUST NOT follow an HTTP redirect from the configured MCP endpoint.
- Remote error bodies MUST NOT be copied into Cockpit responses or logs.
- Fleet planning MUST use the fresh snapshot taken by that fleet request, not
  a browser's earlier display snapshot.
- A batch MUST stop after its first managed-worker launch failure. Workers and
  workspaces already launched remain retained and are reported without raw
  provider output.
- A batch MUST NOT refill after launch. Each worker independently claims at
  most one item through Snowcat MCP; Snowcat remains authoritative under
  concurrent nodes.
- Every worker in a batch MUST receive the request's exact normalized adapter;
  Cockpit MUST NOT select or change an adapter from provider or fleet size.
- Worker correlation MUST validate the exact repository and label projection;
  it MUST fail closed on an unknown outcome or malformed match.

## References

- Rationale: [ADR-0004](../adr/0004-observe-snowcat-once-to-plan-bounded-fleets.md)
- Context: [Cockpit node](../design/node.md)
- Worker lifecycle: [managed workers](managed-workers.md)
- Unattended execution: [rootless OCI workers](oci-workers.md)
- Upstream contracts: [observer scopes](https://github.com/frostyard/snowcat/issues/191),
  [worker correlation](https://github.com/frostyard/snowcat/issues/192)
