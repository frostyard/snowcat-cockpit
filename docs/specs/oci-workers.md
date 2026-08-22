# Spec: Rootless OCI workers

This contract governs Cockpit's first unattended execution adapter: one Codex
worker in a prebuilt OCI image launched by rootless Podman. The managed-worker
CLI, fleet controller, dashboard, and worker manager consume it. Interactive
host workers remain governed by the managed-worker contract.

## Interface

Worker and fleet launch requests add one field:

| Field | Type | Required | Constraints |
| --- | --- | --- | --- |
| `adapter` | string | no | Exact `host` or `oci`; absent means `host` |

The CLI exposes the same choice as `worker launch --adapter host|oci`. The
dashboard MUST label the choices **Host · interactive** and
**OCI · unattended**. It MUST NOT infer OCI from provider, runtime presence, or
fleet size.

The node reads OCI configuration only from its starting environment:

| Variable | Required for `adapter: oci` | Constraints |
| --- | --- | --- |
| `SNOWCAT_COCKPIT_OCI_IMAGE` | yes | Exact `sha256:<64 lowercase hex>` image ID or an image reference suffixed by `@sha256:<64 lowercase hex>` |
| `CODEX_HOME` | no | Host Codex configuration root; defaults to `$HOME/.codex` |
| `GH_CONFIG_DIR` | no | Host GitHub CLI configuration root; defaults to `${XDG_CONFIG_HOME:-$HOME/.config}/gh` |
| `SNOWCAT_MCP_TOKEN` | yes | Inherited by name; its value MUST NOT enter OCI argv, state, or logs |
| `GH_TOKEN` | yes | Inherited by name; may be supplied by the operator or projected from the host GitHub CLI keyring by the secure serve wrapper |

The first slice supports only `provider: codex`, `adapter: oci`, and the
`podman` runtime. Claude, Copilot, and Docker OCI requests MUST fail before a
worktree is allocated.

Build the local image with `make oci-image`. The image source pins Codex CLI
`0.149.0`, Go `1.26.6`, and both multi-architecture base-image manifest
digests. Launch uses a pre-existing image with `--pull=never`.

## Rules

1. OCI readiness MUST be checked before allocating a branch or worktree:
   Podman exists, reports `rootless: true`, the pinned image exists locally,
   `SNOWCAT_MCP_TOKEN` and `GH_TOKEN` are present, and every required input
   file passes the checks below.
2. Required input files are exact `auth.json` and `config.toml` beneath
   `CODEX_HOME`, plus exact `hosts.yml` and `config.yml` beneath
   `GH_CONFIG_DIR`. Each MUST be a regular, non-symlink file whose group and
   other permission bits are zero and whose owner is the current user. Cockpit
   MUST inspect metadata only; it MUST NOT read or parse their content.
3. The Podman invocation MUST use `--rm`, `--pull=never`, `--read-only`,
   `--userns=keep-id`, non-root UID/GID 1000, `--cap-drop=ALL`,
   `no-new-privileges`, a bounded PID limit, and no container log driver.
   It MUST NOT pass a runtime socket, privileged mode, added capability, or
   host networking.
4. The only host filesystem mounts are the exact worker workspace read-write at
   `/workspace` and the four exact input files read-only below
   `/run/cockpit/input`. The container home and `/tmp` MUST be bounded tmpfs
   mounts. The home tmpfs root MAY be mode `1777` for rootless-runtime
   portability; the non-root entrypoint MUST create the actual provider and
   GitHub configuration directories as mode `0700` beneath it.
5. The runtime receives `--env SNOWCAT_MCP_TOKEN` and `--env GH_TOKEN` with no
   values. No other inherited credential environment name enters the
   first-slice container.
6. The image entrypoint MUST copy only the four declared input files into the
   tmpfs home, run `gh auth setup-git`, mark `/workspace` safe in the ephemeral
   Git config, and invoke `codex exec --dangerously-bypass-approvals-and-sandbox`
   once with Cockpit's bounded role prompt. The bypass is permitted only
   inside the complete OCI boundary above.
7. The foreground Podman process runs in the worker's dedicated tmux pane with
   `remain-on-exit`. Cockpit MUST NOT call `podman logs` or persist provider
   output. A normal one-shot exit reconciles to the existing `exited` process
   state.
8. Explicit stop MUST address the exact derived container name before stopping
   tmux. Workspace cleanup remains explicit and retains the Git branch.
9. A failed readiness or container launch MUST return a bounded explanation
   without provider output, configuration content, environment values, or
   runtime error bodies.
10. The OCI workspace MUST be a self-contained local clone whose `.git`
    directory is inside `/workspace`. Allocation MUST use copied objects, not
    hardlinks or alternates, and MUST perform no network operation. The source
    push URL MUST identify the request's exact repository on `github.com` via
    credential-free HTTPS or an ordinary GitHub SSH form. The clone's `origin`
    MUST be the canonical credential-free HTTPS URL. Local paths, mismatched
    repositories, unsupported hosts or schemes, HTTP user information, and URL
    passwords MUST fail before clone allocation.
11. Cockpit-owned skill exclusions MUST live in the clone's private
    `.git/info/exclude`. Explicit cleanup MUST first import the exact worker
    branch into the source repository; failure MUST retain the OCI workspace.

## Derived artifacts

| Artifact | Derivation |
| --- | --- |
| Container name | `cockpit-<worker-id>` |
| OCI image | [`oci/Containerfile`](../../oci/Containerfile) and [`oci/entrypoint.sh`](../../oci/entrypoint.sh) |
| Worker record adapter | Exact normalized request adapter |
| Podman credential projection | Fixed provider/GitHub paths and the single environment-variable name above |

## References

- Rationale: [ADR-0005](../adr/0005-isolate-unattended-workers-in-rootless-oci.md)
- Workspace rationale: [ADR-0006](../adr/0006-use-self-contained-git-directories-for-oci-workers.md)
- Context: [Cockpit node](../design/node.md)
- Base lifecycle: [managed workers](managed-workers.md)
- Built in: [Production roadmap, Phase 5](../plans/0002-production-roadmap.md#phase-5--harden-container-delivery)
- Codex flags: [official OpenAI CLI command reference](https://developers.openai.com/codex/cli/reference)
