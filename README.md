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

## Build and release

`make build` writes a version-stamped Linux binary to
`dist/snowcat-cockpit`; `make install` installs the same command through Go.
`make ci` is the complete credential-free repository gate and is also exposed
as `make check` for the frostyard Go-repository convention.

Version tags run GoReleaser Pro for `linux/amd64` and `linux/arm64`, publish
archives and `frostyard-snowcat-cockpit` deb/rpm/apk packages, and separately
publish the three multi-architecture worker images. The nightly workflow
replaces the single `dev` prerelease after the `Tests` workflow succeeds.

## Node quick start

The node requires Go 1.26.6 or newer to build. Inspect this machine without
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

To enable operator-triggered queue snapshots and bounded fleet launch, supply
the Snowcat HTTP MCP endpoint and a dedicated token through the starting
shell's environment, never command-line arguments:

```bash
export SNOWCAT_COCKPIT_MCP_URL=https://snowcat.example/mcp
read -rsp 'Snowcat Cockpit observer token: ' SNOWCAT_COCKPIT_MCP_TOKEN
export SNOWCAT_COCKPIT_MCP_TOKEN
go run ./cmd/snowcat-cockpit serve
```

Mint this dedicated token with Snowcat's server-enforced `observer` profile:

```bash
npm run queue -- token mint member:you@example.com cockpit-observer --profile observer
```

The profile grants only `list_work` and `get_work`. The dashboard observes only
when you press **Observe once**, launch a fleet, or press **Observe work** for
one retained worker. A fleet is capped to eligible work and 12 workers,
launches once, and never refills.

For the standard local observer file at
`~/.config/snowcat/profile-observer.env`, build once and use the checked-in
wrapper. It verifies mode `0600`, reads only the expected export, fixes the MCP
URL to `https://snowcat.goat-snake.ts.net/mcp`, and removes the source variable
before starting Cockpit:

```bash
make build
bin/snowcat-cockpit-serve --listen 127.0.0.1:7682
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
go run ./cmd/snowcat-cockpit worker observe worker-0123456789abcdef
go run ./cmd/snowcat-cockpit worker attach worker-0123456789abcdef
go run ./cmd/snowcat-cockpit worker stop worker-0123456789abcdef
go run ./cmd/snowcat-cockpit worker cleanup worker-0123456789abcdef
```

Launch creates a unique `cockpit/<worker-id>` branch and Git worktree. Stop
retains the workspace; cleanup refuses a running or dirty workspace and leaves
the branch intact. Observe makes one exact, read-only Snowcat correlation call;
the result is displayed but never written into the worker record.

For unattended Codex, Claude, and Copilot workers, build images in the selected
runtime's local store and pin launches to the resulting provider-specific image
IDs. Podman remains the default:

```bash
make oci-image
# Run the three export commands printed by make oci-image.
read -rsp 'Snowcat worker token: ' SNOWCAT_MCP_TOKEN; echo
export SNOWCAT_MCP_TOKEN

go run ./cmd/snowcat-cockpit worker launch \
  --adapter oci \
  --runtime podman \
  --provider copilot \
  --role reviewer \
  --repository frostyard/firn \
  --source /path/to/local/firn
```

For explicit Docker compatibility, run `make docker-image` and use
`--runtime docker` with the printed `SNOWCAT_COCKPIT_DOCKER_*_IMAGE` exports.
Cockpit records whether the Docker daemon is rootless or rootful; rootful Docker
is not described as host isolation. The checked-in serve wrapper supplies
a validated host mapping to Docker bridge workers so the fixed Snowcat tailnet
endpoint resolves without replacing public DNS or using host networking;
direct CLI launches can set `SNOWCAT_COCKPIT_DOCKER_ADD_HOST` to an explicit
`hostname:IPv4` value.

Version tags publish all three worker images for `linux/amd64` and
`linux/arm64` to `ghcr.io/frostyard/snowcat-cockpit-worker`. The
`Publish worker images` workflow writes the immutable manifest reference for
each provider to its run summary; configure Cockpit with that full
`name:tag@sha256:digest` reference rather than the mutable tag alone.

OCI mode requires Linux containers, the selected runtime and its pinned image,
rootless posture when Podman is selected,
private regular non-symlink provider files at either
`${CODEX_HOME:-$HOME/.codex}/{auth.json,config.toml}` or
`${CLAUDE_CONFIG_DIR:-$HOME/.claude}/.credentials.json` or
`${COPILOT_HOME:-$HOME/.copilot}/mcp-config.json`, and
`${GH_CONFIG_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/gh}/{hosts.yml,config.yml}`,
and a current provider preflight. The worker receives only those exact
files, its worktree, `SNOWCAT_MCP_TOKEN`, and `GH_TOKEN` by environment-variable
name. When the checked-in serve wrapper starts with an OCI image configured, it
projects `GH_TOKEN` from the current `gh` keyring login unless the operator
already supplied it. For Claude, it also projects the wrapper's fixed Snowcat
URL by environment-variable name into the image-owned strict MCP configuration.
Host mode remains the default interactive path. See
the [rootless OCI worker contract](docs/specs/oci-workers.md) for the complete
boundary.

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
