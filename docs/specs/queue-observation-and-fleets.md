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
| `SNOWCAT_CF_ACCESS_CLIENT_ID` | with Access secret | Optional Cloudflare Access service-token client ID; process environment only |
| `SNOWCAT_CF_ACCESS_CLIENT_SECRET` | with Access client ID | Optional Cloudflare Access service-token client secret; process environment only |

If both MCP variables are absent, the node starts with queue observation
unavailable and single-worker operations unchanged. If only one is present or
the URL is invalid, `serve` MUST fail without printing the token. The Access
pair is optional for an endpoint without Cloudflare Access, but if either value
is present without the other, `serve` MUST fail without printing either value.
Every observer request with the pair configured MUST send both
`CF-Access-Client-Id` and `CF-Access-Client-Secret` in addition to Snowcat's
Bearer authorization.

The token MUST be minted with Snowcat's server-enforced `observer` profile,
which grants only `list_work` and `get_work`. Cockpit invokes only `list_work`.
A legacy token carrying the synthetic `cockpit-observer-no-claim` kind
restriction is not sufficient because a kind restriction does not restrict MCP
tools.

## Local launch wrapper

`bin/snowcat-cockpit-serve [serve options]` MUST read two separate credential
files, one for queue observation and one for worker MCP access:

- read `${XDG_CONFIG_HOME:-$HOME/.config}/snowcat/profile-observer.env`, unless
  a test supplies `SNOWCAT_COCKPIT_OBSERVER_ENV`, and parse only one literal
  `export SNOWCAT_OBSERVER_TOKEN=<token>` declaration from it;
- read `${XDG_CONFIG_HOME:-$HOME/.config}/snowcat/mcp-token.env`, unless a
  test supplies `SNOWCAT_COCKPIT_WORKER_ENV`, and parse exactly one literal
  declaration each for `export SNOWCAT_MCP_TOKEN=<token>`,
  `export CF_ACCESS_CLIENT_ID=<client-id>`, and
  `export CF_ACCESS_CLIENT_SECRET=<client-secret>`;
- require each credential file to be a regular, non-symlink file owned by the
  current user with mode `0600`, and MUST NOT evaluate either file as shell
  code;
- set `SNOWCAT_COCKPIT_MCP_URL` to
  `https://snowcat.frostyard.org/mcp`;
- set worker-local `SNOWCAT_MCP_URL` to the same fixed endpoint without copying
  its value into an argument or file;
- map the observer token to `SNOWCAT_COCKPIT_MCP_TOKEN` and the worker token to
  `SNOWCAT_MCP_TOKEN`, map the Access pair to
  `SNOWCAT_CF_ACCESS_CLIENT_ID` and `SNOWCAT_CF_ACCESS_CLIENT_SECRET`, remove
  the three source variables, and use `exec` with preserved argument
  boundaries;
- run `dist/snowcat-cockpit serve`, unless a test supplies
  `SNOWCAT_COCKPIT_BIN`.

When any supported `SNOWCAT_COCKPIT_OCI_*_IMAGE` or
`SNOWCAT_COCKPIT_DOCKER_*_IMAGE` variable (or the legacy Codex
`SNOWCAT_COCKPIT_OCI_IMAGE`) is set and `GH_TOKEN` is absent, the wrapper
MUST invoke `gh auth token` and export its non-empty single-line result only to
the node process. It MUST NOT print, persist, or place the token in argv. A
missing GitHub CLI or invalid login MUST fail before the node starts. An
operator-supplied `GH_TOKEN` takes precedence.

The wrapper MUST NOT synthesize `SNOWCAT_COCKPIT_DOCKER_ADD_HOST`; Docker bridge
workers resolve the Cloudflare Tunnel endpoint through ordinary DNS. The worker
manager continues to validate an operator-supplied mapping before workspace
allocation, and host networking remains forbidden.

## HTTP interface

| Method | Path | Result |
| --- | --- | --- |
| `POST` | `/api/v1/queue/snapshot` | Take and return one bounded claimable-work snapshot |
| `POST` | `/api/v1/fleets` | Take one fresh snapshot and launch one bounded batch |
| `POST` | `/api/v1/workers/{id}/observe` | Correlate one retained worker with its Snowcat attempt |

The snapshot and fleet endpoints require `Content-Type: application/json` and
reject unknown fields. Worker observation has no request body.

Snapshot request:

```json
{"repository":"frostyard/firn"}
```

The node makes exactly two Snowcat `list_work` calls with the exact repository
and `limit: 100`: one with `status: "queued"` and one with
`status: "claimed"`. It includes every queued item and only claimed items whose
newest attempt has outcome `expired`. It MUST NOT derive expiry from the node's
clock. The response is request-only and has this shape:

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

`truncated` MUST be true when either source call returns exactly 100 items, so
one response contains at most 200 items. `counts` are launch-eligible counts:
assigned items with `contract: ready`, plus all
unassigned items under `unassigned`. Suspicious or unknown assigned contracts
remain visible in `items`, increment `flagged`, and MUST NOT add launch capacity.

Fleet request:

```json
{
  "adapter": "oci",
  "runtime": "docker",
  "provider": "codex",
  "mcpServer": "snowcat",
  "role": "implementer",
  "repository": "frostyard/firn",
  "source": "/absolute/path/to/firn",
  "baseRef": "HEAD",
  "count": 3
}
```

`adapter` MUST be exact `host` or `oci` and defaults to `host`. `runtime` MUST
be absent for host, or exact `podman` or `docker` for OCI; it defaults to
`podman`. `mcpServer` identifies the provider-local direct Snowcat server that
the managed invocation replaces with its worker-local relay. `count` MUST be
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

Work-kind routing follows the ordered, runtime-generated
[role/kind classifier table](worker-profiles.md#role-boundaries). The first
matching row wins. `release-needed` is the only currently unassigned exact
kind and remains human-operated.

The implementer rule is exclusion-based because Snowcat work kinds are open:
`implementation`, `issue-resolution`, `pr-review-fix`, `pr-cure`,
`pr-cure-change`, and future worker kinds remain eligible. Cockpit does not
turn the human-operated `release-needed` preparation item into fleet capacity.
Existing-pull-request kinds use the exact target protocol in the
[managed-worker contract](managed-workers.md#rules) after claim.

A contract is suspicious when `requiredArtifact: pull-request` lacks either
`write` or `open-pr`, or when `write` authority does not carry both `open-pr`
and `requiredArtifact: pull-request`. `open-pr` never implies `write`. An
absent `requiredArtifact` is unknown.

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
- Every worker in a batch MUST receive the request's exact normalized adapter
  and runtime; Cockpit MUST NOT select or change either from provider or fleet
  size.
- Worker correlation MUST validate the exact repository and label projection;
  it MUST fail closed on an unknown outcome or malformed match.

## References

- Rationale: [ADR-0004](../adr/0004-observe-snowcat-once-to-plan-bounded-fleets.md)
- Context: [Cockpit node](../design/node.md)
- Worker lifecycle: [managed workers](managed-workers.md)
- Unattended execution: [rootless OCI workers](oci-workers.md)
- Upstream contracts: [observer scopes](https://github.com/frostyard/snowcat/issues/191),
  [worker correlation](https://github.com/frostyard/snowcat/issues/192)
