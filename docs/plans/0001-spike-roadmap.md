# Plan: tmux and ttyd spike

This plan determines whether a thin execution-side cockpit materially improves
operating several Snowcat workers. It intentionally stops at the first useful
answer rather than growing into a second orchestration product.

## Phase 0 — Write the boundary (complete)

- Record the separate-tool decision in
  [ADR-0001](../adr/0001-keep-cockpit-outside-snowcat.md).
- Define purpose, topology, security boundary, vocabulary, and non-goals in the
  [architecture overview](../design/overview.md).
- Pin the small interface in the
  [launcher CLI specification](../specs/launcher-cli.md).
- **Done when:** a reader can say what the spike will prove and name what it
  deliberately will not build before any launcher code exists.

## Phase 1 — Gather terminals with tmux (complete 2026-08-21)

- Implement the generic `start`, `list`, `attach`, and `stop` commands from the
  [launcher contract](../specs/launcher-cli.md).
- Add the three trivial provider mappings in `work`; preserve an unrestricted
  generic `start` path for flags and future providers.
- Keep exited panes for inspection with `remain-on-exit`; add no transcript or
  process database.
- Add focused shell tests using harmless commands and a test-specific tmux
  socket.
- **Done when:** two dummy commands can run concurrently in named slots, the
  launching shell can exit, a new tmux client can inspect both, and stopping one
  does not disturb the other.

Observed 2026-08-21 with two concurrent slots, one live and one exited. The
exited pane retained its output, duplicate creation was refused, and removing
the exited slot left the live slot running. The trial also established that the
tmux server retains its first-launch environment; the architecture now states
the explicit restart rule instead of adding secret synchronization.

## Phase 2 — Put the same console in a browser (complete 2026-08-21)

- Implement `web` as the fixed loopback ttyd invocation in the
  [launcher contract](../specs/launcher-cli.md).
- Test argument construction with a fake ttyd executable and manually exercise
  a real installed ttyd when available.
- Confirm browser disconnect leaves tmux and its commands alive.
- Confirm local tmux and browser clients can view the same windows.
- **Done when:** reconnecting a browser to the loopback ttyd endpoint returns
  to a still-running dummy command without creating another command process.

Transport evidence from 2026-08-21: ttyd 1.7.7 listened only on
`127.0.0.1%lo`; two origin-checked WebSocket connections each received the tmux
screen and disconnected; ttyd terminated each temporary attach process while
the underlying slot remained `running`. The trial corrected the initial
assumption that ttyd `-i` accepts an IP address—on this build it takes an
interface name. The operator then confirmed that a live full-screen `top`
rendered and refreshed, accepted the `1` input, resized, and survived a browser
reload. The underlying tmux pane retained the same live process across the
visual reconnect, satisfying the phase outcome.

## Phase 3 — One real Snowcat operating trial (complete 2026-08-21)

- Prepare two already-isolated repository checkouts; workspace provisioning is
  outside this spike.
- Start one coding-agent worker in each slot using `work`, with repository and
  optional kind filters.
- Observe interactive questions, approvals, tmux resizing, final output, client
  exit behavior, and Snowcat lease/report behavior.
- Record only durable findings in the architecture or contract; do not add a
  generic learning log.
- **Done when:** two workers each claim at most one item through the existing
  Snowcat MCP contract, the operator can intervene from one console, and the
  resulting queue records are indistinguishable from manually started workers.

Observed with Codex and Copilot against isolated Firn worktrees and Snowcat's
HTTP MCP endpoint. Both discovery workers completed one item and proposed one
bounded child. A Codex implementer completed an admitted `quality-gap-fix` and
passed Firn's checks, but the discovery worker had omitted `open-pr` from the
child despite having it in the parent's delegation ceiling. The implementation
therefore correctly stopped with uncommitted changes and no artifact. The
operator committed the retained worktree, opened draft Firn pull request #64,
and attached it to the completed item through Snowcat's supported operator
surface. Snowcat created a `pr-review` item; Copilot selected Claude Opus to
differ from the reported GPT-5.6 author model, returned a structured pass with
advisories, and Snowcat moved the pull request out of draft.

The trial proved terminal intervention, workspace retention, worker-direct HTTP
MCP, operator artifact recovery, independent review, model-diversity guidance,
and the review-gate GitHub write. It also established that production launch
needs canonical Snowcat skill seeding, provider/MCP preflight, workspace
allocation, role-aware live kind selection, and authority warnings. Those
findings motivate the [production roadmap](0002-production-roadmap.md) and
[ADR-0002](../adr/0002-build-a-node-local-cockpit-appliance.md).

## Stop and decide

After Phase 3, choose one outcome:

- **Keep the shell tool:** tmux plus ttyd solves the terminal problem.
- **Keep tmux only:** browser presentation adds no material value.
- **Extend deliberately:** a measured gap justifies one new capability and an
  ADR before implementation.
- **Retire the repository:** the interaction is worse than existing terminals;
  Snowcat remains unchanged.

Decision 2026-08-21: extend deliberately. The terminal and review interaction
was useful; the measured setup and authorization gaps justify the node-local
appliance in [ADR-0002](../adr/0002-build-a-node-local-cockpit-appliance.md).

The spike is successful if it produces a confident decision, including a
decision to stop.

## Later / ideas

These are explicitly unscheduled and must not leak into Phases 1–3:

- A queue snapshot beside the terminal.
- One long-lived token and isolated checkout per slot.
- Workspace allocation and safe dirty-worktree retention.
- Desired concurrency and explicit auto-refill.
- Provider/model presets and a dedicated review slot.
- Structured process status independent of terminal scraping.
- Authenticated shared use by more than one operator.

## Open questions

- **Does writable ttyd behave well with all three full-screen clients?** Decide
  in Phase 3 from actual reconnect, resize, mouse, paste, and approval use.
- **Do clients exit after the bounded prompt or remain at an interactive
  prompt?** Observe in Phase 3; do not add provider-specific automation first.
- **Is a single tmux session with windows preferable to one session per slot?**
  The spike chooses windows for one-console navigation; reverse it only from an
  observed usability problem.
- **Is queue awareness necessary?** Decide only after using the Snowcat surface
  alongside the cockpit. Exact-item dispatch requires a Snowcat contract
  decision and is not assumed.

## References

- Implements: [cockpit architecture](../design/overview.md),
  [launcher CLI](../specs/launcher-cli.md)
- Rationale: [ADR-0001](../adr/0001-keep-cockpit-outside-snowcat.md)
