# Snowcat Cockpit

Snowcat Cockpit is a node-local control surface for Snowcat execution capacity.
It reports machine and provider readiness, launches one retained managed worker
at a time, and presents lifecycle inventory through one Go binary and an
embedded, loopback-only Pilothouse dashboard. The original Bash, tmux, and ttyd
launcher remains available as the measured trial harness.

Snowcat remains unchanged. Each worker continues to claim and report work
through its existing MCP configuration.

Start with the [architecture](docs/design/overview.md), the
[node design](docs/design/node.md), and the
[production roadmap](docs/plans/0002-production-roadmap.md).

## Node quick start

The node requires Go 1.24 or newer to build. Inspect this machine without
creating state or claiming work:

```bash
go run ./cmd/snowcat-cockpit doctor
go run ./cmd/snowcat-cockpit doctor --json
go run ./cmd/snowcat-cockpit install-kit
go run ./cmd/snowcat-cockpit profiles
```

Prove one provider can see the locked skills and reach Snowcat without giving
it claim authority:

```bash
go run ./cmd/snowcat-cockpit preflight \
  --provider codex \
  --mcp-server snowcat \
  --repository frostyard/firn
```

The result is valid for 15 minutes and is bound to the locked kit revision.
Provider output and configuration are never written to Cockpit state.

Start the embedded dashboard on loopback:

```bash
go run ./cmd/snowcat-cockpit serve \
  --skills-dir /path/to/snowcat/.agents/skills
```

Open `http://127.0.0.1:7682`. The node creates only a stable, non-secret ID
under `${XDG_STATE_HOME:-$HOME/.local/state}/snowcat-cockpit`. It refuses a
non-loopback listen address. A provider's launch control is enabled only while
its live MCP receipt is current. Structural profile readiness verifies the
canonical Snowcat skills byte-for-byte against the revision locked into
Cockpit. `install-kit` is the only command that materializes those embedded
skills outside a Cockpit-owned isolated workspace; it never replaces a file
whose content differs from the lock.

Launch one retained host worker from the dashboard, or from the CLI:

```bash
go run ./cmd/snowcat-cockpit worker launch \
  --provider codex \
  --role discoverer \
  --repository frostyard/firn \
  --source /path/to/local/firn \
  --base-ref HEAD

go run ./cmd/snowcat-cockpit worker launch \
  --provider codex \
  --role implementer \
  --repository frostyard/firn \
  --source /path/to/local/firn \
  --base-ref HEAD

go run ./cmd/snowcat-cockpit workers
go run ./cmd/snowcat-cockpit worker attach worker-0123456789abcdef
go run ./cmd/snowcat-cockpit worker stop worker-0123456789abcdef
go run ./cmd/snowcat-cockpit worker cleanup worker-0123456789abcdef
```

Launch creates a unique `cockpit/<worker-id>` branch and Git worktree. Stop
retains the workspace; cleanup refuses a running or dirty workspace and leaves
the branch intact.

Build a standalone binary with `make build`; the result is
`dist/snowcat-cockpit`.

## Trial harness requirements

- Bash 5 or newer
- tmux
- ttyd only for browser access
- An already configured coding-agent client and an existing isolated checkout
  for every writing worker

tmux retains the environment of the first slot for the life of the cockpit
session. Start that first slot from the complete worker environment. If a
credential or environment value changes, stop all slots and start a fresh one.

## Trial harness quick start

Launch harmless commands first:

```bash
bin/snowcat-cockpit start clock /tmp -- watch -n 1 date
bin/snowcat-cockpit start shell /tmp -- bash
bin/snowcat-cockpit list
bin/snowcat-cockpit attach
```

Launch one bounded Snowcat worker in an existing checkout:

```bash
bin/snowcat-cockpit work updex-1 codex \
  /path/to/updex frostyard/updex issue-resolution
```

The supported provider conveniences are `codex`, `claude`, and `copilot`.
Use `start` when a client needs extra flags:

```bash
bin/snowcat-cockpit start updex-2 /path/to/updex -- \
  codex --model gpt-5.6-terra \
  "Work the Snowcat queue for frostyard/updex. Claim at most one item, then stop."
```

If ttyd is installed, expose the same tmux session on loopback:

```bash
bin/snowcat-cockpit web 7681
```

Then open `http://127.0.0.1:7681`. The browser is a writable,
credential-bearing terminal. Keep it on loopback and place any remote access
behind an operator-controlled private boundary.

Clean up explicitly:

```bash
bin/snowcat-cockpit stop updex-1
bin/snowcat-cockpit stop updex-2
```

## Check

```bash
make ci
```
