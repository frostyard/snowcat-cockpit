# Spec: Node CLI and HTTP API

This contract governs the production `snowcat-cockpit` node process, its
side-effect-free readiness command, and the loopback dashboard API.

## CLI interface

```text
snowcat-cockpit doctor [--json]
snowcat-cockpit install-kit [--json] [--skills-dir <directory>]
snowcat-cockpit node <install|status|restart|uninstall> [options]
snowcat-cockpit profiles [--json] [--skills-dir <directory>] [--state-dir <directory>]
snowcat-cockpit preflight --provider <name> --mcp-server <name> --repository <owner/name> [--timeout <duration>]
snowcat-cockpit workers [--json] [--state-dir <directory>]
snowcat-cockpit worker launch --adapter <host|oci> [--runtime <podman|docker>] ...
snowcat-cockpit worker <observe|attach|stop|cleanup> ...
snowcat-cockpit serve [--listen <host:port>] [--state-dir <directory>] [--skills-dir <directory>] [--source-root <directory>]
snowcat-cockpit version
snowcat-cockpit help
```

`version` writes one line containing the release version plus the injected
commit, UTC build date, and builder identity. Development builds use explicit
`none`/`unknown` placeholders rather than omitting provenance.

| Input | Constraints |
| --- | --- |
| `listen` | TCP host and port; host MUST resolve syntactically to `127.0.0.1`, `::1`, or `localhost`; default `127.0.0.1:7682` |
| `state-dir` | Absolute or relative directory; default follows `XDG_STATE_HOME`, then `$HOME/.local/state/snowcat-cockpit` |
| `skills-dir` | Provider-neutral Snowcat worker-kit root; default follows `SNOWCAT_COCKPIT_SKILLS_DIR`, then `$HOME/.agents/skills` |
| `source-root` | Root for retained Cockpit-managed repository sources; default `<state-dir>/sources` |
| `mcp-server` | Provider-local configured MCP server name; contains only letters, digits, `_`, `-`, or `.` |

## Doctor result

`doctor --json` returns:

```json
{
  "status": "ready|degraded",
  "checks": [
    {
      "name": "tmux",
      "category": "runtime",
      "status": "ready|missing|warning",
      "detail": "short non-secret explanation",
      "action": "optional remediation"
    }
  ]
}
```

The text form contains the same checks in a human-readable table.

## HTTP interface

| Method | Path | Result |
| --- | --- | --- |
| `GET` | `/` | Embedded node dashboard |
| `GET` | `/api/v1/health` | Node identity and process health |
| `GET` | `/api/v1/doctor` | Current doctor result |
| `GET` | `/api/v1/profiles` | Current structural worker-profile result |
| `GET` | `/api/v1/repositories` | Node-local managed repository enrollment |
| `POST` | `/api/v1/repositories` | Idempotently enroll one repository slug |
| `POST` | `/api/v1/repositories/{owner}/{name}/setup` | Clone or refresh one retained managed source |
| `GET` | `/api/v1/campaign` | Most recent multi-repository campaign state |
| `POST` | `/api/v1/campaign` | Start one persistent campaign across all enrolled repositories |
| `POST` | `/api/v1/campaign/stop` | Stop future launches without stopping workers |
| `GET` | `/api/v1/workers` | Current managed-worker inventory |
| `POST` | `/api/v1/workers/base` | Read-only selected base commit and local upstream relation |
| `POST` | `/api/v1/queue/snapshot` | One bounded Snowcat queue observation |
| `POST` | `/api/v1/fleets` | One snapshot-capped managed-worker batch |
| `POST` | `/api/v1/workers` | Launch one managed worker |
| `POST` | `/api/v1/workers/{id}/observe` | One exact Snowcat work-attempt observation |
| `POST` | `/api/v1/workers/{id}/stop` | Stop one worker and retain its workspace |
| `POST` | `/api/v1/workers/{id}/console` | Open one loopback worker terminal URL |
| `DELETE` | `/api/v1/workers/{id}` | Explicitly clean one non-running workspace |

`/api/v1/health` returns:

```json
{
  "status": "ok",
  "nodeId": "node-<hex>",
  "startedAt": "RFC3339 timestamp",
  "version": "development or release version"
}
```

Unknown API paths MUST return JSON with a non-2xx status. Non-API unknown paths
MUST return an HTTP 404.

## Rules

1. `doctor` MUST NOT create or modify Cockpit, provider, GitHub, or Snowcat
   state and MUST NOT claim work.
2. `install-kit` MUST write only to the selected `skills-dir`, MUST install
   only embedded content matching the lock, and MUST refuse to replace drifted
   content.
3. Missing optional providers, ttyd, Podman, or Docker MUST produce readiness
   checks and MUST NOT make `serve` fail.
4. `serve` MUST refuse a non-loopback address before creating node state or
   opening a listener.
5. Node state MUST contain no provider credential, MCP credential, lease token,
   environment dump, terminal content, or Snowcat queue record.
6. A newly created state directory MUST be mode `0700`; its node state file
   MUST be mode `0600` on platforms supporting Unix permissions.
7. JSON endpoints MUST set `Content-Type: application/json` and MUST NOT cache
   responses containing live readiness.
8. The dashboard MUST render without a network CDN or third-party script.
9. `preflight` MUST follow the [provider preflight](provider-preflight.md)
   authority and output-retention contract.
10. Worker operations MUST follow the [managed-worker](managed-workers.md)
    lifecycle and retention contract.
11. Queue snapshots, worker observations, and batch launches MUST follow the
    [queue observation and bounded fleets](queue-observation-and-fleets.md)
    contract.
12. An OCI worker launch MUST follow the [rootless OCI worker](oci-workers.md)
    contract and fail before workspace allocation when its boundary is not
    ready.
13. Base inspection MUST resolve only local Git state, report the selected
    immutable commit and configured local upstream relation, and MUST NOT fetch,
    pull, or mutate the source repository.
14. Managed repositories and campaigns MUST follow the
    [repository and board-campaign contract](repositories-and-board-campaigns.md).
15. Linux user-service installation and lifecycle operations MUST follow the
    [node service contract](node-service.md). They MUST NOT change the node's
    loopback, credential-retention, worker-retention, or campaign-restart
    boundaries.

## References

- Rationale: [ADR-0002](../adr/0002-build-a-node-local-cockpit-appliance.md)
- Context: [node architecture](../design/node.md)
- Service lifecycle: [Linux node user service](node-service.md)
- Profile contract: [worker profiles and locked skill kit](worker-profiles.md)
- Live readiness contract: [provider preflight](provider-preflight.md)
- Worker lifecycle contract: [managed workers](managed-workers.md)
- Queue and batch contract: [queue observation and bounded fleets](queue-observation-and-fleets.md)
- Unattended execution contract: [rootless OCI workers](oci-workers.md)
- Persistent campaign contract:
  [managed repositories and board campaigns](repositories-and-board-campaigns.md)
