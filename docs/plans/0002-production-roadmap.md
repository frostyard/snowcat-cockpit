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

## Phase 4 — Launch bounded fleets (complete 2026-08-21)

- Decide the one-shot observation and credential boundary in
  [ADR-0004](../adr/0004-observe-snowcat-once-to-plan-bounded-fleets.md).
- Add explicit discoverer, implementer, and reviewer batch controls.
- Derive exact claim kinds from a bounded live queue snapshot and the selected
  role; flag suspicious authority such as change work lacking `open-pr`.
- Reconcile requested launches to local slots once; do not refill.
- **Done when:** two nodes may each launch a bounded batch against one Snowcat
  service, Snowcat grants at most one lease per item, and an explicit bounded
  observation distinguishes a claimed or completed attempt from no work
  without inferring either from provider-process state.

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
Snowcat's human admission and merge boundaries.

The first bounded-batch slice now follows the
[queue and fleet contract](../specs/queue-observation-and-fleets.md): an
operator takes one 100-item `list_work` snapshot, sees deterministic lane and
delivery-contract classification, and launches at most 12 workers capped to
eligible work. The batch observes again at launch time, stops on the first
allocation failure, retains every created workspace, and never refills.
Snowcat [#191](https://github.com/frostyard/snowcat/issues/191) delivered a
server-enforced observer tool profile, and
[#192](https://github.com/frostyard/snowcat/issues/192) delivered bounded
attempt history plus exact worker-label filtering.

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
process. After the upstream contracts deployed, Cockpit's explicit observation
identified Codex worker `worker-e3bbb7d8995679a8` as attempt 4212 with outcome
`completed` on the exact `pr-cure` item, and identified Copilot worker
`worker-e21e726ad6bb8825` as `unmatched`. The result required no terminal
inspection, did not alter either process record, and was not persisted. Phase 4
therefore closes with bounded operator-triggered correlation; automatic polling
and process termination remain intentionally outside its boundary.

## Phase 5 — Harden container delivery

- Decide the unattended permission and ephemeral configuration boundary in
  [ADR-0005](../adr/0005-isolate-unattended-workers-in-rootless-oci.md).
- Publish pinned multi-architecture worker images running as non-root.
- Prefer rootless Podman and test Docker explicitly.
- Add ephemeral provider-configuration projection and startup diagnostics.
- **Done when:** the same profile completes one implementation and one review
  in host and rootless-container modes with no credential written to Cockpit
  state or back into the host provider configuration.

The first executable slice is now specified and implemented for Codex on
rootless Podman. The node accepts an explicit `host` or `oci` adapter for one
worker or a bounded fleet, validates the pinned local image and exact private
input-file metadata before workspace allocation, and launches a one-shot Codex
process in a read-only, non-root container with bounded tmpfs and no runtime
socket, host network, capabilities, or container logs. The image entrypoint
copies provider and GitHub configuration into the ephemeral home without
Cockpit reading or persisting it. Docker, Claude, Copilot, multi-architecture
publication, and the host/container implementation-and-review trial remain
before Phase 5 is complete.

The first live implementation-and-review launch exposed a linked-worktree
boundary defect before either worker claimed Snowcat work: the worktree's
`.git` pointer named source-owned metadata outside `/workspace`. ADR-0006 keeps
that metadata out of the container by giving OCI workers a self-contained local
clone while preserving linked worktrees for host workers. The failed terminals
and workspaces remain retained as required.

The corrected launch proved self-contained checkouts and Snowcat lifecycle
from the container. Codex reviewer `worker-0e074fea49d493a8` completed review
attempt 4238. Codex implementer `worker-33386683422a140e` claimed attempt 4239
and then correctly blocked without edits because the mounted GitHub CLI file
held an expired fallback token while the host's valid token lived in its OS
keyring. The secure serve wrapper now projects that token in memory as
`GH_TOKEN`, and a no-claim container acceptance check proved both GitHub auth
and Snowcat MCP. The implementation item still needs an operator requeue and
one final OCI delivery run.

## Later / ideas

- Desired concurrency and explicit automatic refill.
- Authenticated non-loopback service mode.
- A dedicated cure profile.
- Multi-user Cockpit nodes.
- Historical terminal transcripts with a separate retention/security decision.

## Open questions
- **Cockpit state engine:** Phase 3 keeps bounded atomic JSON records. Revisit
  SQLite only if fleet concurrency or measured history-query needs justify it.

## References

- Implements: [node architecture](../design/node.md),
  [node CLI and HTTP API](../specs/node-api.md)
- Rationale: [ADR-0002](../adr/0002-build-a-node-local-cockpit-appliance.md)
- Queue observation: [ADR-0004](../adr/0004-observe-snowcat-once-to-plan-bounded-fleets.md)
- OCI isolation: [ADR-0005](../adr/0005-isolate-unattended-workers-in-rootless-oci.md)
- OCI workspace: [ADR-0006](../adr/0006-use-self-contained-git-directories-for-oci-workers.md)
