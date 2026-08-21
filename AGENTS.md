# Snowcat Cockpit

Snowcat Cockpit is a small execution-side companion for gathering external
Snowcat worker terminals in tmux and optionally presenting them through ttyd.
Read [docs/design/overview.md](docs/design/overview.md) before changing its
boundary or scope.

## Rules

- Keep Snowcat unchanged. Workers use its existing MCP contract directly.
- Keep provider credentials, MCP credentials, and lease tokens out of cockpit
  arguments, files, logs, and state.
- Keep the spike as a Bash CLI over tmux and ttyd. A daemon, dashboard, custom
  PTY server, queue poller, database, workspace manager, or auto-refill loop
  requires a measured trial finding and a new ADR first.
- Never read or write Snowcat's SQLite databases.
- Preserve argv boundaries and never use `eval` for launched commands.
- Bind the provided ttyd command to loopback. A writable agent terminal is a
  credential-bearing administrative surface.
- Do not delete a slot or working tree automatically. Cleanup is an explicit
  operator act.
- Run `make ci` before calling a change done.

## Documentation rules

`docs/` follows core's four-category shape—see the table and conventions in
[docs/README.md](docs/README.md). New docs start from their category's
`TEMPLATE.md` and get indexed there. A repo-local decision gets an ADR in
`docs/adr/`; an organization-wide decision gets an ADR in frostyard/core plus
a line in [docs/org-adrs.md](docs/org-adrs.md).
