# Spec: Launcher CLI

This contract governs the `bin/snowcat-cockpit` command used by an operator to
place coding-agent processes in a dedicated tmux session and optionally attach
to that session through ttyd.

## Interface

```text
snowcat-cockpit start <slot> <working-directory> -- <command> [argument ...]
snowcat-cockpit work <slot> <codex|claude|copilot> <working-directory> <owner/repo> [kind]
snowcat-cockpit list
snowcat-cockpit attach
snowcat-cockpit stop <slot>
snowcat-cockpit web [port]
snowcat-cockpit help
```

| Input | Constraints |
| --- | --- |
| `slot` | 1–32 characters; starts alphanumeric; remainder alphanumeric, `.`, `_`, or `-` |
| `working-directory` | Existing directory; normalized to an absolute physical path before launch |
| `command` | At least one argv element following a literal `--` |
| `provider` | Exactly `codex`, `claude`, or `copilot` |
| `owner/repo` | Two non-empty slash-separated segments containing letters, digits, `.`, `_`, or `-` |
| `kind` | Optional Snowcat work kind matching lowercase letters, digits, and internal `-` |
| `port` | Optional integer from 1 through 65535; default `7681` |

## Commands

### `start`

- MUST refuse a duplicate slot without changing the existing window.
- MUST refuse a missing directory, delimiter, or command before starting tmux.
- MUST preserve argv boundaries when constructing tmux's shell command.
- MUST create the dedicated session with the slot as its first window when no
  session exists, otherwise add the slot as a new window.
- MUST enable `remain-on-exit` for the slot.

### `work`

- MUST verify the selected provider executable exists before creating a slot.
- MUST generate the bounded prompt described in
  [cockpit architecture](../design/overview.md#snowcat-worker-convenience).
- MUST invoke Codex as `codex <prompt>`, Claude as `claude <prompt>`, and
  Copilot as `copilot -i <prompt>`.
- MUST delegate the actual launch to the same path as `start`.
- MUST NOT synthesize model, effort, permission, credential, or MCP arguments.

### `list`

- MUST exit successfully with a human-readable empty result when no cockpit
  session exists.
- MUST print one row per slot containing at least the slot name, whether its
  pane is running or exited, current command, and current path.
- MUST NOT print pane content or environment variables.

### `attach`

- MUST fail clearly when no cockpit session exists.
- MUST replace the CLI process with a tmux client attached to the dedicated
  session.

### `stop`

- MUST require an exact valid slot name.
- MUST fail clearly when the slot does not exist.
- MUST kill only that tmux window.

### `web`

- MUST fail before starting when no cockpit session or ttyd executable exists.
- MUST run ttyd in the foreground.
- MUST bind ttyd to a named platform loopback interface: `lo` on Linux, `lo0`
  on macOS, or the validated `SNOWCAT_COCKPIT_TTYD_INTERFACE` override.
- MUST enable writable input, origin checking, and a one-client limit.
- MUST attach ttyd to the dedicated tmux session.
- MUST NOT enable URL command arguments or terminal file transfer.

## Rules

1. The CLI MUST use a dedicated tmux socket and MUST NOT modify the default
   tmux server.
2. The CLI MUST NOT read or write a Snowcat SQLite database.
3. The CLI MUST NOT accept, print, persist, or proxy an MCP lease token or
   provider credential.
4. The CLI MUST NOT use `eval` to launch a command.
5. The first slot MUST inherit the environment that creates the dedicated tmux
   server. Later slots inherit that server environment; the CLI MUST NOT copy
   arbitrary changed values or credentials into the live server.
6. A launch MUST NOT be represented as a Snowcat work attempt; only a
   successful `claim_work` creates an attempt.

## Exit status

- `0`: requested operation completed or `list` found no session.
- non-zero: invalid arguments, missing dependency or state, duplicate/missing
  slot, or failure returned by tmux/ttyd.

## References

- Rationale:
  [ADR-0001](../adr/0001-keep-cockpit-outside-snowcat.md)
- Context: [cockpit architecture](../design/overview.md)
- Delivery: [spike roadmap](../plans/0001-spike-roadmap.md)
