# Spec: Worker profiles and locked skill kit

This contract governs the provider-neutral worker profiles exposed by the
Cockpit CLI and dashboard, and the structural checks performed against
Snowcat's canonical queue skills. It is consumed by the node process, future
execution adapters, and operators preparing a machine for worker launches.

## Interface

The locked kit manifest is embedded at build time from
`internal/profile/worker-kit.lock.json`:

| Field | Type | Required | Constraints |
| --- | --- | --- | --- |
| `version` | integer | yes | Exactly `1` for this contract |
| `source.repository` | URL string | yes | Canonical Snowcat repository |
| `source.revision` | string | yes | Full Git commit ID |
| `skills[].name` | string | yes | Skill directory beneath the configured kit root |
| `skills[].sha256` | lowercase hex string | yes | SHA-256 of that skill's `SKILL.md` bytes |

Operators inspect structural readiness with:

```text
snowcat-cockpit profiles [--json] [--skills-dir <directory>]
snowcat-cockpit serve ... [--skills-dir <directory>]
```

Cockpit ships the exact locked skill bytes and materializes them only through
the explicit operator action:

```text
snowcat-cockpit install-kit [--json] [--skills-dir <directory>]
```

Installation creates missing skill directories and `SKILL.md` files with
private modes. A matching file is left intact. A missing file is installed. A
different, unreadable, non-regular, or oversized file stops the operation and
is never replaced.

`--skills-dir` defaults to `SNOWCAT_COCKPIT_SKILLS_DIR` when set, then
`$HOME/.agents/skills`. It is the directory whose immediate children are the
locked skill directories.

The HTTP representation is available from `GET /api/v1/profiles`:

```json
{
  "status": "missing|failed|preflight-required|ready",
  "kit": {
    "status": "ready|missing|drifted",
    "revision": "<full commit>",
    "checks": []
  },
  "roles": [],
  "providers": []
}
```

Each provider contains separate `executable`, `skillKit`, and `mcp` checks.
MCP starts `unchecked`; a current successful
[provider preflight](provider-preflight.md) changes only that provider to
`ready`. Failed, expired, or wrong-kit receipts fail closed.

## Role boundaries

| Role | Canonical skill | Queue selection |
| --- | --- | --- |
| `discoverer` | `work-snowcat-queue` | Kinds ending in `-discovery` only; read-only assessment with at most one proposed child |
| `implementer` | `work-snowcat-queue` | Every non-discovery worker kind except exact `pr-review`, `pr-review-fix`, `pr-cure`, `pr-cure-change`, and `release-needed` |
| `reviewer` | `review-snowcat-queue` | Exact `pr-review` only |

The locked kit also carries `work-snowcat-without-reviews` for canonical
Snowcat completeness, but the default implementer profile does not use it: its
selection gate admits discovery work, while the proven Cockpit operating rule
is deliberately fix-only plus cure work.

## Rules

1. Structural inspection MUST NOT invoke a provider, contact Snowcat, claim
   work, install a skill, or modify provider or Cockpit state.
2. Skill content MUST be compared byte-for-byte through the locked SHA-256;
   presence without a matching digest is `drifted`, never `ready`.
3. Kit installation MUST verify embedded bytes against the lock before writing,
   MUST preflight all existing targets, and MUST refuse to replace drifted
   content.
4. Missing provider executables and missing or drifted skills MUST prevent the
   affected provider from becoming `preflight-required` or `ready`.
5. `unchecked` MCP means no live connection claim has been made. It MUST NOT
   be presented as healthy, configured, or ready.
6. Structural output MUST NOT contain provider credentials, MCP credentials,
   lease tokens, environment dumps, or provider configuration contents.
7. All providers support all three roles; the role controls the canonical skill and
   queue selection, not the provider executable.
8. A provider launch control MUST remain disabled unless every launch-critical
   check, including its current live MCP preflight, is `ready`. Another
   provider's failure MUST NOT invalidate a ready provider.

## Derived artifacts

| Artifact | Derivation |
| --- | --- |
| `profiles` text/JSON output | Locked manifest, selected skill root, and executable lookup |
| `install-kit` text/JSON output | Embedded locked skill payloads and selected kit root |
| `/api/v1/profiles` | Same structural snapshot used by the CLI |
| Dashboard worker-profile table | `/api/v1/profiles` provider and kit checks |

## References

- Rationale: [ADR-0002](../adr/0002-build-a-node-local-cockpit-appliance.md)
- Context: [Cockpit node design](../design/node.md)
- Built in: [Production roadmap, Phase 2](../plans/0002-production-roadmap.md#phase-2--make-worker-profiles-reproducible)
- Live readiness: [provider preflight](provider-preflight.md)
