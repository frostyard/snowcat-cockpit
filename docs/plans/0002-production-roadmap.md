# Plan: Production Cockpit node

This plan turns the successful tmux/ttyd experiment into a portable node-local
worker appliance. It follows the bounded
[spike roadmap](0001-spike-roadmap.md) and preserves Snowcat as the sole work
coordinator.

## Phase 0 — Record the operating trial (complete 2026-08-21)

- Record the production boundary in
  [ADR-0002](../adr/0002-build-a-node-local-cockpit-appliance.md).
- Fold durable trial findings into the [architecture overview](../design/overview.md)
  and the original [spike roadmap](0001-spike-roadmap.md).
- **Done when:** the repository distinguishes proven terminal/review behavior
  from the missing worker-profile and authorization mechanisms.

## Phase 1 — Boot one node (complete 2026-08-21)

- Implement `doctor` and `serve` from the
  [node API contract](../specs/node-api.md).
- Persist only a non-secret stable node identity.
- Render the same provider/runtime readiness in the embedded loopback
  dashboard described by the [node design](../design/node.md).
- **Done when:** one binary starts on Linux or macOS, renders locally without a
  CDN, and reports missing optional runtimes without failing startup.

The delivered node uses a stable non-secret identity, refuses non-loopback
listeners before state creation, embeds its Pilothouse dashboard, and exposes
the read-only readiness result through both CLI and HTTP. Linux verification
covered unit tests, static analysis, a live server/API smoke test, state modes,
and the retained Bash/tmux lifecycle suite.

## Phase 2 — Make worker profiles reproducible (complete 2026-08-21)

- Define a versioned Snowcat worker kit sourced from the canonical queue skills.
- Implement provider adapters that verify auth, install the kit into an
  isolated provider home, and perform a read-only HTTP MCP smoke test.
- Specify permission, directory-trust, and ephemeral credential-projection
  behavior for host and OCI execution.
- **Done when:** Codex, Claude, and Copilot each pass a profile preflight that
  proves the intended skill and Snowcat MCP path are visible without claiming
  work.

The structural, materialization, and live MCP portions are implemented:
Cockpit embeds and locks all three canonical skills to Snowcat revision
`77239fa7e430`, installs missing files only through an explicit command,
refuses to replace drift, and gives each preflight model only `list_work`.
The local trial passed for Codex (`snowcat`), Copilot (`snowcat-mcp`), and
Claude (`snowcat`). Claude's first non-interactive trial exposed an argv bug:
its variadic `--allowedTools` option consumed a following prompt. The adapter
now places the prompt before that option and pins the ordering in a regression
test. Provider environment, permission, and trust posture remain inherited from
the shell for the first host-worker slice; reproducible projection is deferred
to the OCI hardening phase.

## Phase 3 — Launch one managed worker (complete 2026-08-21)

- Allocate one retained isolated workspace for a repository.
- Launch one discoverer, implementer, or reviewer through the selected execution adapter.
- Correlate its stable worker identity with Snowcat bookkeeping without
  observing its lease token.
- Preserve its terminal and workspace until explicit operator cleanup.
- **Done when:** one dashboard action launches a worker that completes one item
  and remains inspectable after exit.

The lifecycle implementation is in place: one dashboard or CLI action creates
a unique local branch and worktree, installs the locked skills behind a
process-local Git exclusion, launches a dedicated retained tmux terminal, and
records a stable worker ID without observing provider environment or Snowcat
lease state. The real Git+tmux harness proves allocation, provider exit,
attachment, dirty-workspace refusal, and explicit cleanup. It was followed by
a real dashboard launch of Codex worker
`worker-5ca017d21b0952fe`. It claimed one Firn `ci-signal-fix`, passed `make
check` and `git diff --check`, pushed commit `4920697`, and completed the
Snowcat item while its terminal and workspace remained inspectable.

That trial reproduced the under-authorization defect: the admitted change item
lacked `open-pr`, so the worker correctly omitted a pull request. The managed
implementer prompt now releases any change item lacking `open-pr` before
substantive work. Phase 4 must additionally flag such queued contracts before
launch; Cockpit never widens them.

A second implementer trial correctly declined to claim when Firn had only
`dependencies-gap-discovery`, `docs-drift-discovery`, and `pr-review` queued.
Its interactive client remained alive after reporting that it had stopped, so
the operator explicitly stopped the terminal while retaining the workspace.
This measured both the missing execution lane and a process-lifecycle gap:
discovery must run before another fix can exist, and a provider's queue-loop
completion is not necessarily OS-process exit. Cockpit therefore exposes a
distinct read-only discoverer role; it does not broaden the implementer
selector or claim queue work itself.

## Phase 4 — Launch bounded fleets

- Decide the one-shot observation and credential boundary in
  [ADR-0004](../adr/0004-observe-snowcat-once-to-plan-bounded-fleets.md).
- Add explicit discoverer, implementer, and reviewer batch controls.
- Derive exact claim kinds from a bounded live queue snapshot and the selected
  role; flag suspicious authority such as change work lacking `open-pr`.
- Reconcile requested launches to local slots once; do not refill.
- **Done when:** two nodes may each launch a bounded batch against one Snowcat
  service and every worker either claims one distinct item or exits cleanly
  with no work.

The first three-lane host batch launched Codex and Claude discoverers beside a
Copilot reviewer. Claude and Copilot completed their Snowcat work, but both
required repeated interactive permission confirmations and both provider TUIs
remained alive after success until explicitly stopped. Codex continued its
independent discovery item. The permission friction belongs to Phase 5's
provider-specific permission and trust posture; the retained-TUI behavior
belongs to Phase 4 lifecycle reconciliation.

The resulting Firn delivery loop then ran end to end: the operator admitted a
discovery proposal, a fresh Codex implementer completed the authorized fix and
opened its pull request, a fresh Copilot reviewer returned `pass`, and the
operator merged the pull request. This validates all three role contracts and
Snowcat's human admission and merge boundaries. What remains in Phase 4 is to
turn those individually launched workers into one bounded batch operation and
to reconcile Snowcat completion with provider-process state.

The first bounded-batch slice now follows the
[queue and fleet contract](../specs/queue-observation-and-fleets.md): an
operator takes one 100-item `list_work` snapshot, sees deterministic lane and
delivery-contract classification, and launches at most 12 workers capped to
eligible work. The batch observes again at launch time, stops on the first
allocation failure, retains every created workspace, and never refills.
Snowcat issues [#191](https://github.com/frostyard/snowcat/issues/191) and
[#192](https://github.com/frostyard/snowcat/issues/192) track the remaining
server-side observer scope and lifecycle-correlation contracts.

The two-node arbitration trial passed on 2026-08-21. Two Cockpit nodes launched
one Codex and one Copilot implementer from independent state directories within
milliseconds of each other against the same single eligible Firn `pr-cure`.
Snowcat granted exactly one lease; the operator confirmed that Codex won while
Copilot claimed nothing. Codex mechanically updated Firn PR #65 without
changing its patch, all checks emitted by the updated workflow passed, and it
reported the pull-request artifact before Snowcat marked the item completed.
The PR remained unmergeable because Firn branch protection still names a check
that the PR itself renames. That repository-policy mismatch is outside both
Snowcat's arbitration and Cockpit's execution boundary.

Both interactive provider TUIs remained alive after their queue attempt, so
both local worker records correctly continued to describe a running provider
process. The read-only Snowcat projection exposed the authenticated lease
principal but not the winning worker's client label; only the operator's
terminal observation could attribute the lease to Codex. Automatic
completed/no-work reconciliation therefore remains gated on Snowcat
[#192](https://github.com/frostyard/snowcat/issues/192). That reconciliation,
followed by a repeat trial in which terminal worker state settles without
operator inference, remains before Phase 4 is complete.

## Phase 5 — Harden container delivery

- Decide the unattended permission and ephemeral configuration boundary in
  [ADR-0005](../adr/0005-isolate-unattended-workers-in-rootless-oci.md).
- Publish pinned multi-architecture worker images running as non-root.
- Prefer rootless Podman and test Docker explicitly.
- Add ephemeral provider-configuration projection and startup diagnostics.
- **Done when:** the same profile completes one implementation and one review
  in host and rootless-container modes with no credential written to Cockpit
  state or back into the host provider configuration.

## Later / ideas

- Desired concurrency and explicit automatic refill.
- Authenticated non-loopback service mode.
- A dedicated cure profile.
- Multi-user Cockpit nodes.
- Historical terminal transcripts with a separate retention/security decision.

## Open questions

- **Provider configuration projection:** host workers currently inherit the
  node's launch environment. Decide in Phase 5 whether OCI workers receive
  read-only configuration through tmpfs or provider-specific mounts.
- **Cockpit state engine:** Phase 3 keeps bounded atomic JSON records. Revisit
  SQLite only if fleet concurrency or measured history-query needs justify it.

## References

- Implements: [node architecture](../design/node.md),
  [node CLI and HTTP API](../specs/node-api.md)
- Rationale: [ADR-0002](../adr/0002-build-a-node-local-cockpit-appliance.md)
- Queue observation: [ADR-0004](../adr/0004-observe-snowcat-once-to-plan-bounded-fleets.md)
