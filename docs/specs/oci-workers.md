# Spec: Rootless OCI workers

This contract governs Cockpit's unattended execution adapter: one Codex or
Copilot worker in a provider-specific OCI image launched by rootless Podman. The managed-worker
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
| `SNOWCAT_COCKPIT_OCI_CODEX_IMAGE` | for Codex | Exact `sha256:<64 lowercase hex>` image ID or an image reference suffixed by `@sha256:<64 lowercase hex>`; legacy `SNOWCAT_COCKPIT_OCI_IMAGE` remains a Codex-only fallback |
| `SNOWCAT_COCKPIT_OCI_COPILOT_IMAGE` | for Copilot | Same immutable-image constraint |
| `CODEX_HOME` | no | Host Codex configuration root; defaults to `$HOME/.codex` |
| `COPILOT_HOME` | no | Host Copilot configuration root; defaults to `$HOME/.copilot` |
| `GH_CONFIG_DIR` | no | Host GitHub CLI configuration root; defaults to `${XDG_CONFIG_HOME:-$HOME/.config}/gh` |
| `SNOWCAT_MCP_TOKEN` | yes | Inherited by name; its value MUST NOT enter OCI argv, state, or logs |
| `GH_TOKEN` | yes | Inherited by name; may be supplied by the operator or projected from the host GitHub CLI keyring by the secure serve wrapper |

The implemented slice supports `provider: codex|copilot`, `adapter: oci`, and
the `podman` runtime. Claude and Docker OCI requests MUST fail before a
workspace is allocated.

Codex model selection is role-pinned: discoverers and implementers use
`gpt-5.6-sol`; reviewers use `gpt-5.6-terra`. The selected model is recorded as
non-secret worker metadata and shown in inventory. This makes Cockpit-authored
implementation/review pairs structurally different-model; a reviewer still
MUST release work when the origin reports the same model. Copilot launches
with model selector `auto`, recorded exactly that way, so its canonical review
skill can choose an available non-origin model after reading the claimed
contract; it MUST release a review if independent selection is impossible.

Build both local images with `make oci-image`, or one with
`make oci-image-codex` or `make oci-image-copilot`. The image sources pin Codex
CLI `0.149.0`, Copilot CLI `1.0.80`, Go `1.26.6`, multi-architecture base-image
manifest digests, and the official amd64/arm64 Copilot release checksums.
Launch uses a pre-existing image with `--pull=never`.

The first-slice command baseline is deliberately small: the base shell and
Unix utilities, Go and Node.js, Git, GitHub CLI, OpenSSH client, curl, make,
patch, jq, ripgrep, unzip, and `column`. A new tool enters this list only after
a repository contract or retained worker terminal demonstrates the need.
Commands that a provider runs through a login shell MUST still resolve `go` and
`gofmt` through `/usr/local/bin`; `GOPATH` and `GOCACHE` MUST live beneath the
bounded writable home tmpfs rather than the read-only image filesystem.

## Rules

1. OCI readiness MUST be checked before allocating a branch or worktree:
   Podman exists, reports `rootless: true`, the pinned image exists locally,
   `SNOWCAT_MCP_TOKEN` and `GH_TOKEN` are present, and every required input
   file passes the checks below.
2. Both providers require exact `hosts.yml` and `config.yml` beneath
   `GH_CONFIG_DIR`. Codex additionally requires exact `auth.json` and
   `config.toml` beneath `CODEX_HOME`; Copilot requires only exact
   `mcp-config.json` beneath `COPILOT_HOME` because `GH_TOKEN` supplies its
   authentication. Each file MUST be a regular, non-symlink file whose group
   and other permission bits are zero and whose owner is the current user.
   Cockpit MUST inspect metadata only; it MUST NOT read or parse their content.
3. The Podman invocation MUST use `--rm`, `--pull=never`, `--read-only`,
   `--userns=keep-id`, non-root UID/GID 1000, `--cap-drop=ALL`,
   `no-new-privileges`, a bounded PID limit, and no container log driver.
   It MUST NOT pass a runtime socket, privileged mode, added capability, or
   host networking.
4. The only host filesystem mounts are the exact worker workspace read-write at
   `/workspace` and the provider's exact input files read-only below
   `/run/cockpit/input`. The container home, `/tmp`, and test scratch at
   `/var/lib` MUST be bounded tmpfs mounts. The tmpfs roots MAY be mode `1777`
   for rootless-runtime
   portability; the non-root entrypoint MUST create the actual provider and
   GitHub configuration directories as mode `0700` beneath it.
5. The runtime receives `--env SNOWCAT_MCP_TOKEN` and `--env GH_TOKEN` with no
   values. No other inherited credential environment name enters the
   first-slice container.
6. The image entrypoint MUST copy only the provider's declared input files into the
   tmpfs home, run `gh auth setup-git`, mark `/workspace` safe in the ephemeral
   Git config, restore a conventional `022` process umask after writing
   credentials at mode `0600`, and invoke
   its provider once with Cockpit's bounded role prompt. Codex uses
   `codex exec --dangerously-bypass-approvals-and-sandbox` and its role-pinned
   model. Copilot uses non-interactive `--prompt`, `--allow-all`, disabled
   remote control, built-in MCP servers, logs and updates, plus model selector
   `auto`. These unattended permission modes are permitted only inside the
   complete OCI boundary above.
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
| Codex OCI image | [`oci/Containerfile`](../../oci/Containerfile) and [`oci/entrypoint.sh`](../../oci/entrypoint.sh) |
| Copilot OCI image | [`oci/Copilot.Containerfile`](../../oci/Copilot.Containerfile) and [`oci/copilot-entrypoint.sh`](../../oci/copilot-entrypoint.sh) |
| Worker record adapter | Exact normalized request adapter |
| Podman credential projection | Fixed provider/GitHub paths and the single environment-variable name above |

## References

- Rationale: [ADR-0005](../adr/0005-isolate-unattended-workers-in-rootless-oci.md)
- Workspace rationale: [ADR-0006](../adr/0006-use-self-contained-git-directories-for-oci-workers.md)
- Context: [Cockpit node](../design/node.md)
- Base lifecycle: [managed workers](managed-workers.md)
- Built in: [Production roadmap, Phase 5](../plans/0002-production-roadmap.md#phase-5--harden-container-delivery)
- Codex flags: [official OpenAI CLI command reference](https://developers.openai.com/codex/cli/reference)
- Copilot release and flags: [official GitHub Copilot CLI repository](https://github.com/github/copilot-cli)
