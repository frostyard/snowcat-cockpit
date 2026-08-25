# Spec: Declared node configuration and `node up`

This contract governs the non-secret node configuration file and the
`snowcat-cockpit node up` command that converges a Linux host to it. It is
consumed by operators, by the CLI, and by any automation that brings a node to
a running campaign after a reboot, a Cockpit release, or a Snowcat skill
change. It composes the existing doctor, worker-kit, node-service, managed
repository, provider preflight, and board campaign contracts; it does not
redefine them.

## Interface

```text
snowcat-cockpit node up [--config <file>] [--dry-run]
  [--install-root <directory>] [--unit-dir <directory>]
  [--timeout <duration>] [--json]
```

| Input | Default | Constraints |
| --- | --- | --- |
| `config` | `$XDG_CONFIG_HOME/snowcat-cockpit/node.json`, then `$HOME/.config/snowcat-cockpit/node.json` | regular file, at most 256 KiB |
| `dry-run` | false | report decisions without changing anything |
| `install-root`, `unit-dir` | node service defaults | as `node install` |
| `timeout` | `2m` | per provider preflight; `(0, 10m]` |
| `json` | false | structured result instead of step lines |

### Configuration file (schema version 1)

| Field | Type | Required | Constraints |
| --- | --- | --- | --- |
| `version` | integer | yes | exactly `1` |
| `listen` | string | yes | loopback `host:port` |
| `stateDirectory` | string | no | absolute after resolution; default is the node CLI state-directory default. The worker kit is always `<stateDirectory>/worker-kit` and managed sources `<stateDirectory>/sources` |
| `observerEnv`, `workerEnv` | string | no | credential file **paths**; defaults as `node install` |
| `images` | object | for `oci` | keys `codex`, `claude`, `copilot`; each value pinned by `@sha256:<64 hex>` or a bare `sha256:` image ID; every provider a lane uses needs one |
| `environment` | object | no | keys from the node service allowlist except image variables; single-line non-empty values |
| `providers` | object | yes | keys `codex`, `claude`, `copilot`; each `{ "mcpServer": <name> }` with a provider-local MCP server name |
| `repositories` | array of string | yes | 1–64 unique (case-insensitive) `owner/name` slugs |
| `campaign.adapter` | string | yes | `host` or `oci` |
| `campaign.runtime` | string | for `oci` | `podman` (default) or `docker`; forbidden for `host` |
| `campaign.intervalSeconds` | integer | no | 10–300, default 30 |
| `campaign.<lane>` | object | yes | `discoverer`, `implementer`, `reviewer`: `{ "provider": <declared provider>, "capacity": 1..12 }`; capacities total at most 12 |

Unknown fields are rejected. Any string value anywhere in the file that looks
like a credential (`snowcat_`, `gho_`, `ghp_`, `ghu_`, `ghs_`, `ghr_`,
`github_pat_`, `sk-` prefixes) is rejected and never echoed.

```json
{
  "version": 1,
  "listen": "127.0.0.1:7686",
  "images": {
    "codex": "ghcr.io/frostyard/snowcat-cockpit-worker:codex-v0.2.1@sha256:920a53dec2c9c24b8b0072e04419fc2ae3060f15ccbb0b2aad37693882020c0d",
    "claude": "ghcr.io/frostyard/snowcat-cockpit-worker:claude-v0.2.1@sha256:a9dc6cccde4cfdf7dff9788e415ccec87f9a81c394650fc0a84930c527bdbd61",
    "copilot": "ghcr.io/frostyard/snowcat-cockpit-worker:copilot-v0.2.1@sha256:e6a774b5e67f850b41bd78facae0b56da78a6e405a1a9f8e19c8156566e77aaf"
  },
  "environment": {"CODEX_HOME": "/home/operator/.codex"},
  "providers": {
    "codex": {"mcpServer": "snowcat"},
    "claude": {"mcpServer": "snowcat"},
    "copilot": {"mcpServer": "snowcat-mcp"}
  },
  "repositories": ["frostyard/clix", "frostyard/core", "frostyard/snowcat", "frostyard/updex"],
  "campaign": {
    "adapter": "oci",
    "runtime": "podman",
    "intervalSeconds": 30,
    "discoverer": {"provider": "codex", "capacity": 4},
    "implementer": {"provider": "claude", "capacity": 4},
    "reviewer": {"provider": "copilot", "capacity": 4}
  }
}
```

### Derived campaign request

Each lane's `mcpServer` is the declared provider's `mcpServer`; the request
otherwise maps field-for-field onto `POST /api/v1/campaign`. One provider
therefore names exactly one MCP server per campaign by construction.

### Derived service environment

The environment `node up` hands to `node install` is, in increasing
precedence: ambient values of allowlisted names, `environment`, then every
`images.<provider>` projected to **both** `SNOWCAT_COCKPIT_OCI_<PROVIDER>_IMAGE`
and `SNOWCAT_COCKPIT_DOCKER_<PROVIDER>_IMAGE`. Nothing outside the allowlist
is projected.

## Steps

`node up` performs these steps in order and records one result per step with
status `ok`, `skipped`, `planned` (dry run), or `failed`. A failed step ends
the run; text output prints one line per step, and `--json` writes the whole
result.

1. **doctor** — MUST fail when a required tool is missing, when the `oci`
   runtime's tool is missing, or when a `host` lane's provider executable is
   missing.
2. **kit** — MUST install the embedded worker kit into
   `<stateDirectory>/worker-kit` when it is missing; MUST move a drifted kit
   aside to `worker-kit.pre-<version>.<UTC timestamp>` and install fresh; MUST
   NOT delete anything; MUST leave a ready kit untouched.
3. **install** — MUST compute the node-service install plan (release ID and
   rendered `service.env`) for the configuration and MUST skip the install
   when the install record exists, is healthy, and matches the plan's release,
   listen address, state/skills/source paths, and configuration path, and the
   on-disk `service.env` is byte-identical to the plan. Otherwise it MUST call
   the node-service install contract, which restarts the node and stops any
   active campaign. The step detail names the reason.
4. **repositories** — MUST enroll and set up every declared repository through
   the running node's loopback API with concurrency at most four, report each
   repository's status and pinned base commit, continue past a failed setup,
   and fail only when no repository is ready.
5. **preflight** — for each distinct provider/MCP-server pair the lanes use,
   MUST reuse a `ready` receipt whose MCP server matches, whose kit revision
   is the embedded lock's, and which has not expired; otherwise MUST run the
   provider preflight contract with the first declared repository as the
   filter, retrying once, and MUST fail before the campaign step when any
   provider still lacks a live proof.
6. **campaign** — MUST leave a `starting`, `running`, `degraded`, or
   `stopping` campaign in place and MUST otherwise start one from the derived
   request.
7. **status** — MUST verify node health and report the dashboard URL.

## Rules

- Loading MUST fail closed on schema version, unknown fields, non-loopback
  listen, unknown providers, unpinned images, non-allowlisted environment
  names, image variables under `environment`, malformed or duplicate
  repositories, undeclared lane providers, and credential-shaped values.
- Re-running `node up` against an unchanged configuration on a healthy node
  with an active campaign changes nothing and exits 0.
- `--dry-run` MUST perform only the doctor and kit inspections and MUST report
  what steps 2–7 would do.
- `node up` MUST NOT read, copy, print, or store any credential; it passes
  credential file paths to the node service contract exactly as `node install`
  does and relies on its posture checks.
- The install record MUST carry `configPath`, and `node status` MUST print it
  when present.
- Exit status is 0 on success, 1 when a step fails, and 2 on invalid flags or
  configuration.

## JSON result

```json
{
  "configPath": "/home/operator/.config/snowcat-cockpit/node.json",
  "dryRun": false,
  "steps": [{"name": "doctor", "status": "ok", "detail": "…"}],
  "node": {"install": {"…": "…"}, "service": {"…": "…"}, "health": {"…": "…"}},
  "repositories": [{"repository": "frostyard/clix", "status": "ready", "baseCommit": "…", "detail": "…"}],
  "preflights": [{"provider": "claude", "mcpServer": "snowcat", "status": "ready", "detail": "…", "expiresAt": "…"}],
  "campaign": {"id": "campaign-…", "status": "starting"},
  "dashboardUrl": "http://127.0.0.1:7686"
}
```

`node` uses the node service projection; `repositories` and `campaign` use the
managed repository and board campaign projections.

## Derived artifacts

| Artifact | Derivation |
| --- | --- |
| Service environment | Allowlisted ambient values, then `environment`, then image pins projected to both runtime variable names |
| Campaign request | Declared lanes with each provider's `mcpServer` |
| Worker-kit and source roots | Fixed beneath `stateDirectory` |
| Install record `configPath` | Absolute path of the configuration that produced the install |

## References

- Rationale: [ADR-0013](../adr/0013-converge-the-node-from-a-declared-configuration.md)
- Context: [Cockpit node](../design/node.md)
- Composes: [Linux node user service](node-service.md),
  [node CLI and HTTP API](node-api.md), [provider preflight](provider-preflight.md),
  [managed repositories and board campaigns](repositories-and-board-campaigns.md),
  [worker profiles and locked skill kit](worker-profiles.md)
