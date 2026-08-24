# Snowcat Cockpit

This is the canonical repository instruction surface. `CLAUDE.md`,
`GEMINI.md`, `CONTRIBUTING.md`, `.cursorrules`, and
`.github/copilot-instructions.md` are aliases; edit this file, never an alias.
The complete conformance alias registry is in
[ADR-0007](docs/adr/0007-use-canonical-aliases-for-acmm-conformance.md).

Snowcat Cockpit is a small execution-side companion for gathering external
Snowcat worker terminals in tmux and optionally presenting them through ttyd.
Read [docs/design/overview.md](docs/design/overview.md) before changing its
boundary or scope.

## Rules

- Keep Snowcat unchanged. Workers use its existing MCP contract directly.
- Keep provider credentials, MCP credentials, and lease tokens out of cockpit
  arguments, files, logs, and state.
- Keep the legacy spike as a Bash CLI over tmux and ttyd. The production node,
  dashboard, workspace manager, and bounded fleets are governed by the
  accepted ADRs under `docs/adr/`; any queue poller or automatic refill loop
  requires measured trial evidence and a new ADR first.
- Never read or write Snowcat's SQLite databases.
- Preserve argv boundaries and never use `eval` for launched commands.
- Bind the provided ttyd command to loopback. A writable agent terminal is a
  credential-bearing administrative surface.
- Do not delete a slot or working tree automatically. Cleanup is an explicit
  operator act.
- Run `make ci` before calling a change done.

## Build and code map

- `make build` builds `dist/snowcat-cockpit`; `make ci` is the complete local
  and CI gate.
- `cmd/snowcat-cockpit/` owns argument parsing and process startup.
- `internal/worker/` owns isolated checkout and terminal lifecycles;
  `internal/fleet/` only plans one bounded batch.
- `internal/queueview/` is the read-only Snowcat MCP observer;
  `internal/preflight/` and `internal/profile/` prove provider readiness.
- `internal/web/` serves the loopback Pilothouse dashboard.
- Shell integration tests live in `test/`; Go packages use adjacent `_test.go`
  files. Anything that shells out takes a mockable runner.

Go errors stay lowercase without trailing punctuation and wrap their cause.
Filesystem tests use `t.TempDir()`. Keep CLI flags out of lower-level packages;
pass explicit request/config structs instead.

## Repository boundary

`policies/agent-governance.json` is this repository's canonical
agent-governance surface under the frostyard/core repository-surfaces
contract v1; Snowcat reads it (from GitHub, at the observed default-branch
head) when enrolling this repository in the fleet. Deny by default; read,
write, and run-tests allowed; issues, pull requests, and follow-ups
review-required. Review-required at high risk: workflows; release and image
publication (`.goreleaser.yaml`, `.svu.yaml`, the release, snapshot, and
worker-images workflows); everything that launches a worker or handles a
credential (`oci/`, `bin/`, `internal/worker/`, `internal/leaseproxy/`,
`internal/profile/`, `internal/preflight/`); the loopback dashboard
(`internal/web/`, a writable terminal is an administrative surface); and
the node service and CLI entry (`internal/nodeservice/`, `cmd/`). Change it
only alongside the matching ADR or design change.

## Documentation rules

`docs/` follows core's four-category shape—see the table and conventions in
[docs/README.md](docs/README.md). New docs start from their category's
`TEMPLATE.md` and get indexed there. A repo-local decision gets an ADR in
`docs/adr/`; an organization-wide decision gets an ADR in frostyard/core plus
a line in [docs/org-adrs.md](docs/org-adrs.md).
