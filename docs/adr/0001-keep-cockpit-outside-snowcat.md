# 0001 — Keep the cockpit outside Snowcat

- **Status:** Accepted
- **Date:** 2026-08-21

## Context

Operating several Snowcat workers currently means keeping several coding-agent
terminals open, remembering which checkout each terminal owns, and returning to
the correct terminal when a client requests input. Snowcat already provides
durable work selection, leases, bounded actions, results, and artifact
verification. It deliberately leaves coding-agent processes, provider
credentials, tools, and execution isolation to the operator.

The experiment needs to reduce terminal handling without turning Snowcat into a
process supervisor or importing provider-specific behavior into its control
path. Its value must be testable before building a dashboard, scheduler, new
queue endpoint, or persistent execution database.

## Decision

Build Snowcat Cockpit as a separate, operator-started execution-side tool. It
uses tmux to retain and gather coding-agent terminals and may use ttyd to expose
that tmux session through a browser. Each coding-agent process remains a
Snowcat worker and uses Snowcat's existing MCP contract directly.

The initial spike is a Bash CLI over installed `tmux` and optional installed
`ttyd`. It starts only work the operator explicitly requests. It does not read
Snowcat's databases, proxy MCP, handle lease tokens, manage provider
credentials, select an exact work item, provision checkouts, or refill idle
capacity automatically.

## Consequences

- Snowcat requires no code, schema, deployment, or authority change.
- Provider credentials and MCP configuration remain in the coding-agent
  process environment where they already live.
- A cockpit crash does not terminate a tmux-owned worker.
- The first spike can test the important interaction—several durable terminals
  in one browser—without a custom PTY server or JavaScript application.
- The operator must supply an existing isolated checkout for each writing
  worker; the cockpit does not prevent two slots from receiving the same path.
- ttyd exposes a credential-bearing terminal and therefore must remain on
  loopback behind an operator-controlled private access boundary.
- Queue-aware dispatch, workspace allocation, structured process status, and
  unattended refill remain possible later, but are not implied by this ADR.

## Alternatives considered

- **Add terminal controls to Snowcat's operator surface:** rejected because it
  would cross Snowcat's coordination/execution boundary and put terminal
  secrets beside a deliberately token-free read model.
- **Build a custom web PTY server first:** rejected because ttyd and tmux can
  test the interaction with much less code and security surface.
- **Start with automatic queue draining:** rejected because process lifecycle,
  checkout isolation, and interactive intervention need to be observed first.
- **Use tmux alone permanently:** retained as a valid outcome; ttyd is useful
  only if browser access is materially better than attaching over a terminal.

## References

- Shapes: [cockpit architecture](../design/overview.md),
  [launcher CLI](../specs/launcher-cli.md)
- Built in: [spike roadmap](../plans/0001-spike-roadmap.md)
- Builds on Snowcat's
  [ADR-0003 — Separate work coordination from execution](https://github.com/frostyard/snowcat/blob/main/docs/adr/0003-separate-work-coordination-from-execution.md)
