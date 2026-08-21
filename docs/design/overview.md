# Cockpit architecture

Living document. Rationale:
[ADR-0001](../adr/0001-keep-cockpit-outside-snowcat.md),
[ADR-0002](../adr/0002-build-a-node-local-cockpit-appliance.md).
Contracts: [launcher CLI](../specs/launcher-cli.md),
[node CLI and HTTP API](../specs/node-api.md).

## Purpose

Snowcat Cockpit reduces the operator's terminal-management burden while several
external coding agents process Snowcat work. It gathers worker terminals in one
dedicated tmux session and optionally presents that session in a browser through
ttyd. It does not replace Snowcat's queue, operator surface, MCP server, or
authority model.

The completed spike asked one narrow question: is a tmux-backed browser console
enough to operate several interactive workers without building another
orchestration service? The answer was yes for terminal interaction, with
measured gaps in skill/configuration preflight, workspace allocation, role
selection, and local lifecycle visibility. The production
[Cockpit node](node.md) addresses those execution-side gaps without taking over
Snowcat coordination.

## Boundary

Snowcat remains the coordinator:

- it selects eligible work during `claim_work`;
- it issues and renews the attempt lease;
- it constrains allowed and delegable actions;
- it records reports, artifacts, and events; and
- it independently verifies supported artifacts.

Cockpit remains execution-side convenience:

- it starts an operator-selected coding-agent command;
- it gives the command an existing working directory;
- it retains the command's terminal in tmux;
- it lets the operator attach locally or through ttyd; and
- it stops a tmux window when explicitly asked.

The worker remains the coding-agent client. It reads its existing provider
credentials and MCP configuration, calls Snowcat directly, owns the lease token
returned by `claim_work`, changes its checkout, and reports its result. Cockpit
does not sit in that protocol path.

## Architecture

```text
                               existing MCP configuration
                                          │
                                          ▼
operator ──► snowcat-cockpit ──► tmux window ──► coding-agent worker
    │                                  │                 │
    │                                  │                 ├──► Snowcat /mcp
    │                                  │                 ├──► checkout
    │                                  │                 └──► GitHub
    │                                  │
    ├──► tmux attach ──────────────────┤
    │                                  │
    └──► browser ──► ttyd on loopback ─┘
```

There are three runtime components and no cockpit daemon:

1. `bin/snowcat-cockpit` is a short-lived Bash CLI. It validates arguments,
   constructs a provider prompt when requested, and asks tmux to create,
   inspect, attach to, or remove a window.
2. A dedicated tmux server named `snowcat-cockpit` owns one session named
   `cockpit`. Each named window is a **slot**. tmux, not the CLI process, owns
   the worker's pseudo-terminal and lifetime.
3. ttyd is optional. The `web` command starts it in the foreground on
   the platform loopback interface, with one writable browser client, and tells
   it to attach to the same tmux session. Closing ttyd does not close tmux or
   its workers.

## Vocabulary

The project uses a small execution-side vocabulary so it does not overload
Snowcat's domain terms:

- **Slot:** one named tmux window reserved for one launched command.
- **Launch:** creation of a slot and its OS process. A launch can fail before
  any Snowcat work is claimed.
- **Console:** a local tmux client or browser ttyd client attached to the tmux
  session.
- **Worker:** the coding-agent client that calls Snowcat MCP.
- **Work attempt:** Snowcat's lease-bound execution, created only after the
  worker successfully calls `claim_work`.

A tmux session is an implementation detail and is never called a Snowcat worker
session. A slot name is operator convenience and never authority or evidence.

## Launch flow

### Generic command

The `start` command accepts a slot, an existing directory, and an argv after
`--`. It validates the slot and directory, refuses to replace an existing slot,
and starts the argv in a new tmux window. The generic path makes the tmux
lifecycle testable with harmless commands and remains an escape hatch for a
provider not known to the convenience command.

tmux accepts a shell command rather than an argv array. The CLI therefore
serializes each already-separated argument with Bash's `%q` formatting and
passes the resulting command string to tmux. It never uses `eval`, accepts no
command through an HTTP query parameter, and does not reconstruct arguments by
splitting user text.

### Snowcat worker convenience

The `work` command accepts a slot, provider, existing checkout, Snowcat
repository slug, and optional work kind. It generates one bounded prompt:

```text
Work the Snowcat queue for <owner/repo>[, <kind> items only].
Claim at most one item, then stop.
```

It then maps the provider to the installed interactive client:

| Provider | Invocation |
| --- | --- |
| `codex` | `codex <prompt>` |
| `claude` | `claude <prompt>` |
| `copilot` | `copilot -i <prompt>` |

No model, effort, permission, credential, or MCP flags are synthesized during
the spike. The operator's normal client configuration remains authoritative.
The generic `start` command can express a one-off invocation needing extra
flags without expanding the convenience interface.

The repository and kind in the prompt are claim filters, not a preassignment.
The current Snowcat contract does not claim an exact item by ID. If another
worker wins the eligible item first, the launched worker may claim a different
eligible item under the same filters or receive no work and stop.

## tmux lifecycle

The first launch creates the dedicated tmux server and `cockpit` session with
the requested slot as its first window. Later launches add windows. Slot names
are unique within the session and deliberately short enough to remain legible
in the tmux status line.

Each slot has `remain-on-exit` enabled. A completed or failed command therefore
leaves its final screen visible and appears dead in `list` until the operator
uses `stop`. This preserves useful failure output without adding a transcript
database or log files.

Stopping the last slot naturally ends the tmux session and its dedicated
server. `attach`, `list`, `stop`, and `web` report a clear error or empty state
when no session exists; they never create a placeholder shell.

The tmux socket and session names can be overridden by environment variables
for tests, but normal operation uses the fixed names. Cockpit never attaches to
or modifies the operator's ordinary tmux server.

tmux captures the launching environment when its dedicated server is first
created. Later `snowcat-cockpit` invocations can select a new argv and working
directory, but their changed environment is not automatically copied into the
existing server. The spike deliberately does not synchronize arbitrary
environment values: doing so would require copying unknown provider and MCP
secrets into tmux commands or a second state channel. Start the first slot from
the complete worker environment; after credentials or configuration change,
stop every slot so the next launch creates a fresh server environment.

## ttyd presentation

The `web` command is intentionally only a checked ttyd invocation. It does not
embed ttyd, proxy WebSockets, serve a dashboard, or install the dependency.

The spike uses these controls:

- bind the platform loopback interface (`lo` on Linux, `lo0` on macOS), never
  all interfaces;
- enable writable input so the operator can answer agent questions;
- check WebSocket origin;
- allow one browser client to reduce accidental competing input;
- attach only to the fixed cockpit tmux session; and
- do not enable URL-provided command arguments or terminal file transfer.

ttyd stays in the foreground so its lifetime and logs are visible. Remote
access is the operator's deployment decision: SSH forwarding, a private mesh
proxy, or an authenticated edge can expose the loopback listener. The cockpit
does not configure those systems.

## Security model

The browser terminal is more sensitive than Snowcat's operator surface. A
coding-agent TUI may render tool inputs or results containing a live Snowcat
lease token. It can also run commands that expose provider, GitHub, SSH, or MCP
credentials from its environment. Anyone able to type in ttyd effectively has
the worker's execution authority.

Therefore:

- loopback binding is mandatory in the provided command;
- writable ttyd must sit behind an operator-controlled access boundary;
- cockpit does not persist terminal output outside tmux scrollback;
- cockpit does not print environment variables or generated commands containing
  secrets;
- provider and MCP credentials are never accepted as CLI arguments; and
- automatic browser exposure is outside the spike.

tmux is persistence, not isolation. Cockpit trusts the supplied working
directory and inherited environment. The operator remains responsible for
separate worktrees or clones, filesystem permissions, provider sandboxing, and
the actions granted to each client.

## Key patterns

### One explicit launch, one bounded prompt

`work` launches one interactive client and asks it to claim at most one item.
There is no loop, desired-concurrency controller, or retry. The operator can
open several slots explicitly when parallel work is appropriate.

### Existing contracts over integration shortcuts

Workers use the supported Snowcat MCP interface. Cockpit does not open the
queue SQLite database, scrape the operator surface, manipulate priority, or
infer lease state from terminal output.

### Durable terminal, disposable launcher

The short-lived Bash CLI may exit immediately after launch. tmux keeps the
worker alive and provides the reconnection mechanism. No second persistence
layer is justified until a trial demonstrates state that tmux and Snowcat do
not already retain.

### Provider adapters stay tiny

Provider convenience is only an argv prefix plus the common prompt. A provider
requiring lifecycle APIs, output parsing, or credential brokerage does not fit
the spike and should use `start` or motivate a later design decision.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `SNOWCAT_COCKPIT_SOCKET` | `snowcat-cockpit` | Dedicated tmux socket name |
| `SNOWCAT_COCKPIT_SESSION` | `cockpit` | tmux session name |
| `SNOWCAT_COCKPIT_TMUX` | first `tmux` on `PATH` | Test or package override |
| `SNOWCAT_COCKPIT_TTYD` | first `ttyd` on `PATH` | Test or package override |
| `SNOWCAT_COCKPIT_TTYD_INTERFACE` | `lo` on Linux; `lo0` on macOS | Explicit platform loopback interface override |

The CLI reads no Snowcat environment variable. The launched worker inherits the
dedicated tmux server's environment, captured by the first launch, and discovers
Snowcat through its existing MCP configuration.

## Failure modes

- **tmux missing:** every command that needs tmux fails before changing state.
- **ttyd missing:** `web` fails with an installation hint; tmux operation is
  unaffected.
- **Unknown loopback interface:** `web` refuses to start until
  `SNOWCAT_COCKPIT_TTYD_INTERFACE` names it.
- **Duplicate slot:** launch is refused; existing work is never replaced.
- **Bad checkout:** launch is refused before tmux starts the provider.
- **Provider missing:** `work` is refused before creating a slot.
- **Worker exits:** the dead pane remains visible until `stop`.
- **Worker sees stale environment:** stop all slots and launch the first slot
  again from the updated shell; the spike does not synchronize a live tmux
  server's environment.
- **CLI or ttyd exits:** tmux and running workers continue.
- **tmux server exits:** its workers exit with it; Snowcat lease expiry handles
  a worker that disappears without reporting.
- **No eligible work:** the client should report that no item was claimed and
  remain available for inspection until stopped.

## Explicit non-goals for the spike

- A second Snowcat dashboard or operator surface.
- Exact-item dispatch.
- Queue polling or event subscriptions.
- Automatic concurrency, refill, restart, or retry.
- Workspace creation, Git fetching, branch naming, or cleanup.
- Provider/model policy or reviewer-independence enforcement.
- Headless approval bypasses.
- Terminal recording, search, replay, or analytics.
- Multi-user authentication or authorization.
- Running ttyd on a public interface.
- Packaging, auto-update, or a system service.

## Spike observations

On 2026-08-21 the tmux lifecycle test ran multiple slots against an isolated
socket. A command finishing immediately exposed a launch race: tmux could
discard the window before `remain-on-exit` was applied. The launcher now creates
a live placeholder pane, applies the window options, and uses `respawn-pane` to
start the real command. The focused test pins the retained-output behavior.

The same trial exposed tmux's first-launch environment behavior. The project
keeps the restart rule described above rather than copying unknown secrets into
a live server.

A real ttyd 1.7.7 run corrected another assumption: `-i` is an interface name,
not an IP address. Passing `127.0.0.1` logged a failed device bind; passing `lo`
produced a listener reported as `127.0.0.1%lo:17681`. Two WebSocket attach and
disconnect cycles each received terminal data. ttyd killed the temporary tmux
client after each disconnect, while `snowcat-cockpit list` continued to report
the underlying probe slot as running. The operator-observed pass then rendered
a live full-screen `top`, accepted writable input, resized, and reloaded the
browser. The tmux pane kept the same live process across that reconnect. This
completes the generic TUI/browser slice; behavior of the three coding-agent TUIs
remains part of the real Snowcat operating trial.

## References

- Rationale:
  [ADR-0001](../adr/0001-keep-cockpit-outside-snowcat.md)
- Contract: [launcher CLI](../specs/launcher-cli.md)
- Built in:
  [spike roadmap — Phases 1–3](../plans/0001-spike-roadmap.md)
