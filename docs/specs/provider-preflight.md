# Spec: Provider preflight

This contract governs the explicit live check that proves a coding-agent
provider can see Cockpit's locked worker skills and call Snowcat's read-only
`list_work` MCP tool. The CLI, profile readiness model, and future launch
controls consume its expiring receipt.

## Interface

```text
snowcat-cockpit preflight \
  --provider <codex|claude|copilot> \
  --mcp-server <configured-name> \
  --repository <owner/name> \
  [--skills-dir <directory>] \
  [--state-dir <directory>] \
  [--timeout <duration>] \
  [--json]
```

`--timeout` defaults to two minutes and MUST be greater than zero and no more
than ten minutes. The MCP server name and repository slug accept only bounded
identifier characters; neither may carry a command, URL, header, or token.

The provider receives one prompt asking it to confirm the
`work-snowcat-queue` and `review-snowcat-queue` skill names and call
`list_work` once with the supplied repository, `status: "queued"`, and
`limit: 1`. Success requires a zero provider exit and this exact output line:

```text
SNOWCAT_COCKPIT_PREFLIGHT_OK skills=work-snowcat-queue,review-snowcat-queue tool=list_work
```

The provider adapters expose and approve only that read tool:

| Provider | Model-visible restriction | Permission restriction |
| --- | --- | --- |
| Codex | `mcp_servers.<name>.enabled_tools=["list_work"]` | Per-tool approval for `list_work`; read-only sandbox |
| Claude | No built-in tool is pre-approved | `mcp__<name>__list_work` only; `dontAsk` mode |
| Copilot | `<name>-list_work` only | `<name>(list_work)` only |

The successful or failed receipt is stored at
`<state-dir>/preflights/<provider>.json`:

| Field | Type | Required | Constraints |
| --- | --- | --- | --- |
| `version` | integer | yes | Exactly `1` |
| `provider` | string | yes | `codex`, `claude`, or `copilot` |
| `mcpServer` | string | yes | The bounded configured server name used by the check |
| `status` | string | yes | `ready` or `failed` |
| `detail` | string | yes | Sanitized Cockpit-owned summary; at most 200 bytes |
| `checkedAt` | RFC3339 timestamp | yes | When the provider invocation began |
| `expiresAt` | RFC3339 timestamp | yes | Fifteen minutes after a success; equal to `checkedAt` after failure |
| `kitRevision` | full Git commit ID | yes | Exact active Snowcat worker-kit revision served to the check |

## Rules

1. Preflight MUST be an explicit operator action and MAY spend one provider
   inference call.
2. Cockpit MUST make `claim_work` and every other Snowcat tool unavailable to
   the preflight model. Prompt text alone is not an authority boundary.
3. Cockpit MUST seed the exact verified active skills into an ephemeral project-local
   preflight directory for both `.agents/skills` and `.claude/skills` discovery.
4. The provider MUST inherit its existing auth and MCP configuration. Cockpit
   MUST NOT read, copy, print, or persist that configuration or its credentials.
5. Provider stdout and stderr MUST be bounded in memory and MUST NOT enter
   Cockpit output, logs, receipts, or node state.
6. A receipt is `ready` only while unexpired and bound to the current worker-kit
   revision. Failure, expiry, or a kit change MUST fail closed.
7. Temporary preflight files MUST be removed after the provider exits. This is
   not a managed worker workspace and carries no repository checkout.
8. Different providers MAY use different configured MCP server names; Cockpit
   MUST not infer one provider's name from another provider's configuration.

## Derived artifacts

| Artifact | Derivation |
| --- | --- |
| `preflight` text/JSON output | Sanitized provider result and receipt timestamps |
| Provider MCP readiness | Current receipt, current time, and active kit revision |
| Dashboard profile table | `/api/v1/profiles` after receipt application |

## References

- Rationale: [ADR-0002](../adr/0002-build-a-node-local-cockpit-appliance.md)
- Context: [Cockpit node](../design/node.md)
- Related contract: [Worker profiles and locked skill kit](worker-profiles.md)
- Built in: [Production roadmap, Phase 2](../plans/0002-production-roadmap.md#phase-2--make-worker-profiles-reproducible)
