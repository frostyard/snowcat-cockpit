# Spec: Linux node user service

This contract governs installation and lifecycle control of the long-running
Cockpit node through a Linux systemd user manager. It does not govern managed
worker processes, retained terminals, workspaces, or Snowcat work attempts.

## Interface

```text
snowcat-cockpit node install [--listen <host:port>] [--state-dir <directory>]
  [--skills-dir <directory>] [--source-root <directory>]
  [--observer-env <file>] [--worker-env <file>]
  [--install-root <directory>]
  [--unit-dir <directory>] [--json]
snowcat-cockpit node status [--install-root <directory>]
  [--unit-dir <directory>] [--json]
snowcat-cockpit node restart [--install-root <directory>]
  [--unit-dir <directory>] [--json]
snowcat-cockpit node uninstall [--install-root <directory>]
  [--unit-dir <directory>] [--json]
```

The fixed unit name is `snowcat-cockpit.service`. Defaults are:

| Input | Default |
| --- | --- |
| `listen` | `127.0.0.1:7682` |
| `state-dir` | the node CLI state-directory default |
| `skills-dir` | the node CLI worker-kit default |
| `source-root` | `<state-dir>/sources` |
| `observer-env` | `$SNOWCAT_COCKPIT_OBSERVER_ENV`, then `$XDG_CONFIG_HOME/snowcat/profile-observer.env`, then `$HOME/.config/snowcat/profile-observer.env` |
| `worker-env` | `$SNOWCAT_COCKPIT_WORKER_ENV`, then `$XDG_CONFIG_HOME/snowcat/mcp-token.env`, then `$HOME/.config/snowcat/mcp-token.env` |
| `install-root` | `$HOME/.local/libexec/snowcat-cockpit` |
| `unit-dir` | `$XDG_CONFIG_HOME/systemd/user`, then `$HOME/.config/systemd/user` |

`node install` returns the selected content-addressed release, exact unit path,
dashboard URL, service state, and health projection. `node status` returns the
installed record, systemd `LoadState`, `ActiveState`, `SubState`, main PID and
exit status, plus live health when the service is active. `node restart`
returns the same state after a successful restart. JSON output uses the same
field names as these projections. Text output is a concise human-readable
rendering.

## Installed artifacts

```text
<install-root>/
  releases/<version>-<executable-sha256-prefix>/
    bin/snowcat-cockpit-serve
    dist/snowcat-cockpit
  current -> releases/<selected-release>
  service.env
  install.json

<unit-dir>/snowcat-cockpit.service
```

`install.json` is versioned non-secret JSON containing the unit name, selected
release, loopback listen address, state, source, skills, install and unit paths,
dashboard URL, and install time. `service.env` contains only the observer and
worker credential **paths** and present values from this fixed allowlist:

```text
PATH XDG_CONFIG_HOME XDG_RUNTIME_DIR CODEX_HOME CLAUDE_CONFIG_DIR
COPILOT_HOME GH_CONFIG_DIR
SNOWCAT_COCKPIT_OCI_IMAGE SNOWCAT_COCKPIT_OCI_CODEX_IMAGE
SNOWCAT_COCKPIT_OCI_CLAUDE_IMAGE SNOWCAT_COCKPIT_OCI_COPILOT_IMAGE
SNOWCAT_COCKPIT_DOCKER_CODEX_IMAGE SNOWCAT_COCKPIT_DOCKER_CLAUDE_IMAGE
SNOWCAT_COCKPIT_DOCKER_COPILOT_IMAGE SNOWCAT_COCKPIT_DOCKER_ADD_HOST
```

The generated unit includes:

```ini
[Service]
Type=simple
EnvironmentFile=<install-root>/service.env
ExecStart=<install-root>/current/bin/snowcat-cockpit-serve <serve arguments>
Restart=on-failure
RestartSec=5s
KillMode=process
TimeoutStopSec=15s
UMask=0077
Delegate=yes
```

## Rules

1. Every command MUST fail as unavailable outside Linux and MUST invoke
   `systemctl` only with `--user` and the fixed unit name.
2. Install MUST validate the loopback address and both credential files'
   regular-file, non-symlink, current-owner, and `0600` posture before changing
   service state. It MUST NOT read or copy either token.
3. Install MUST copy the exact running executable and companion reviewed
   wrapper into a content-addressed release using private directories and
   atomic file replacement. It MUST atomically switch `current` only after the
   release is complete.
4. Unit and environment generation MUST preserve argv boundaries, escape
   systemd syntax, and reject newline, control-character, glob, quote,
   backslash, or whitespace injection in the exact environment-file path.
5. No generated artifact may contain an observer token, Snowcat MCP token,
   provider credential, GitHub token, lease token, arbitrary environment dump,
   terminal output, or Snowcat queue record. Environment projection is exactly
   the allowlist above.
6. Install MUST run `daemon-reload`, `enable`, and `restart`, then require the
   unit to be active and the loopback health response to report the installed
   binary version. It MUST retain prior releases when verification fails.
7. Restart MUST use the installed record, restart the fixed unit, and apply the
   same active-state and version-matched health verification.
8. Status is read-only apart from systemd and loopback health queries. A loaded
   but inactive service or unavailable/mismatched health is not healthy.
9. Uninstall MUST disable and stop the fixed unit, remove only the exact unit,
   `current` symlink, `service.env`, and `install.json`, then run
   `daemon-reload`. It MUST retain releases, state, sources, workspaces,
   branches, worker records, and worker tmux servers.
10. The unit MUST use `KillMode=process`. Node stop, restart, failure, or
    uninstall MUST NOT intentionally signal worker tmux servers or infer a
    Snowcat outcome. An interrupted campaign follows the node restart contract
    and does not resume automatically.

## Derived artifacts

| Artifact | Derivation |
| --- | --- |
| Release ID | Build version plus first 16 lowercase hex characters of the executable-and-wrapper content SHA-256 |
| Dashboard URL | Validated loopback listen address |
| Unit `ExecStart` | Atomic `current` selection plus explicit serve arguments |
| Service environment | Fixed non-secret allowlist plus observer and worker credential paths |

## References

- Rationale: [ADR-0011](../adr/0011-run-the-node-as-a-systemd-user-service.md)
- Context: [Cockpit node](../design/node.md)
- Node runtime: [node CLI and HTTP API](node-api.md)
- Worker retention: [managed workers](managed-workers.md)
