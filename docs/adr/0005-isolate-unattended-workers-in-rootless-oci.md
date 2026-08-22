# 0005 — Isolate unattended workers in rootless OCI containers

- **Status:** Accepted
- **Date:** 2026-08-21

## Context

The host-worker trials proved Codex, Claude, and Copilot can complete Snowcat
discovery, implementation, review, and cure work from isolated Git worktrees.
They also required repeated permission and directory-trust confirmations. Both
the successful and no-work interactive clients remained alive after their queue
attempts, so local provider-process state did not settle when Snowcat work did.

Removing those confirmations on the host would give an unattended coding agent
the operator's full filesystem and process authority. Provider guidance reserves
its bypass modes for an externally isolated environment. Cockpit therefore needs
an execution boundary before it can make a fleet unattended.

KubeStellar Hive's contributor launcher demonstrates a useful local/container
workflow: preflight before credential setup, prefer an explicitly selected
rootless runtime, keep real provider configuration read-only, and give the
container a disposable writable copy. Its implementation stages that copy in a
host temporary directory. Cockpit has the stricter requirement that provider and
MCP credentials never enter its files, logs, arguments, or state, so it needs an
in-memory variation of that pattern.

## Decision

Add an explicit OCI execution adapter for unattended one-shot workers. Keep the
host adapter interactive and unchanged. A fleet request selects its adapter; it
never silently promotes a host launch to unattended authority.

The OCI adapter prefers rootless Podman. Docker is supported only when selected
explicitly and Cockpit reports whether its daemon posture is rootless or
rootful. A rootful Docker socket is not described as isolation from the host.

Each OCI worker runs as a non-root user in a pinned multi-architecture image.
The container receives no runtime socket, host network, privileged mode, added
capability, or broad home-directory mount. Its root filesystem is read-only. It
receives only:

- the worker's already allocated Git worktree, read-write at `/workspace`;
- bounded tmpfs mounts for its home, temporary files, and tool caches;
- provider-specific host configuration mounted read-only beneath
  `/run/cockpit/input`;
- the GitHub CLI configuration or SSH agent socket needed by the selected
  repository transport, mounted read-only where applicable; and
- an allowlist of inherited credential environment variable *names*, passed to
  the runtime without their values appearing in argv.

The image entrypoint copies only documented, provider-specific configuration
from `/run/cockpit/input` into the tmpfs home before launch. This gives clients a
writable ephemeral config location without allowing any write back to the host.
Cockpit never reads, parses, copies, logs, or stores the configuration content.
Stopping the container destroys the tmpfs projection. Stopping or cleaning the
worker remains an explicit operator action and never deletes its Git branch.

The container invokes the selected provider in its supported non-interactive,
one-shot mode with provider permission prompts disabled inside the external OCI
boundary. The process exits after completion, failure, or a no-work result. The
host tmux pane runs the foreground container process with `remain-on-exit`; it
retains the final terminal screen without enabling container-runtime log
persistence. Cockpit still does not parse or store provider output.

The first OCI slice permits the ordinary outbound network required by the
provider, Snowcat, GitHub, and repository tests. Egress allowlisting is not
claimed until a repository/profile contract can name every required endpoint.
The worker remains credential-bearing and must be treated as untrusted despite
its filesystem and process isolation.

## Consequences

- Permission and directory-trust prompts disappear only where Cockpit has
  established an external process/filesystem boundary.
- A successful or no-work one-shot provider exits, allowing existing tmux
  reconciliation to settle local process state without reading terminal text.
- The operator can use the same dashboard with host and OCI profiles; host mode
  remains the compatibility path for interactive authentication and debugging.
- Provider and GitHub configuration formats become explicit adapter inputs that
  require versioned tests and fail-closed readiness checks.
- Rootless Podman receives the strongest supported label. Explicit Docker
  support remains useful on laptops and desktops but may have a root-equivalent
  daemon boundary.
- The image and provider CLIs must be pinned, built for `linux/amd64` and
  `linux/arm64`, and updated deliberately.
- Network egress remains a material residual risk because useful workers must
  reach credential-authorized services. Filesystem isolation does not prevent a
  malicious repository instruction from misusing those services.
- Exact worker-to-Snowcat-attempt attribution still depends on
  [snowcat#192](https://github.com/frostyard/snowcat/issues/192); process exit is
  not proof of which item produced the result.

## Alternatives considered

- **Disable approvals on host workers:** rejected because an agent would inherit
  the operator's host authority with no external boundary.
- **Bind-mount real provider configuration read-write:** rejected because a
  worker could persist hooks, MCP servers, or other executable configuration
  that affects later host sessions.
- **Copy credentials into a Cockpit temporary directory:** rejected because it
  violates the rule that credentials do not enter Cockpit-managed files and
  creates deletion and crash-recovery obligations.
- **Mount configuration read-only at its normal home path:** rejected as the
  only mode because some clients legitimately update session or onboarding
  state while running. A tmpfs copy preserves compatibility without host writes.
- **Use only Docker:** rejected because a rootful daemon weakens the node-local
  isolation claim. Docker remains an explicit compatibility choice.
- **Require networkless containers:** rejected because providers, Snowcat,
  GitHub, Git remotes, and many repository tests require outbound access.

## References

- Shapes: [Cockpit node](../design/node.md),
  [production roadmap](../plans/0002-production-roadmap.md)
- Builds on:
  [ADR-0002](0002-build-a-node-local-cockpit-appliance.md),
  [ADR-0003](0003-isolate-each-managed-worker-terminal.md)
- Informed by:
  [Hive v4 contributor launcher](https://github.com/kubestellar/hive/blob/eeaaad4dd5eda9183b536c214735a4dbbbc77a0b/Justfile#L925-L1151),
  [official Codex CLI command reference](https://developers.openai.com/codex/cli/reference)
- Refined by: [ADR-0006](0006-use-self-contained-git-directories-for-oci-workers.md)
