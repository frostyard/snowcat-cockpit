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

The executable slice is now specified and implemented for Codex, Claude, and
Copilot on rootless Podman. The node accepts an explicit `host` or `oci` adapter for one
worker or a bounded fleet, validates the pinned local image and exact private
input-file metadata before workspace allocation, and launches a one-shot Codex
process in a read-only, non-root container with bounded tmpfs and no runtime
socket, host network, capabilities, or container logs. The image entrypoint
copies provider and GitHub configuration into the ephemeral home without
Cockpit reading or persisting it. Docker and multi-architecture publication
remain before Phase 5 is complete.

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

The retained workers also measured two missing inspection tools: their
duplicate-work checks attempted `jq` and ripgrep. The pinned image now carries
those plus the small build/edit baseline documented by the OCI worker contract;
tool growth remains evidence-driven.

The PR #67 reviewer then exposed a login-shell path mismatch: the image carried
Go under `/usr/local/go/bin`, but Codex's Debian login shell reset `PATH` and
reported `go: command not found`. Stable `/usr/local/bin` links plus home-tmpfs
`GOPATH` and `GOCACHE` now make the toolchain and its writable caches invariant
across direct entrypoint and agent-exec shells.

The paired PR #67 review also makes the existing independent-model obligation
executable: OCI implementation uses `gpt-5.6-sol`, while OCI review uses
`gpt-5.6-terra`. The model is visible in worker inventory, and the review skill
still releases any externally authored item whose model matches the reviewer.

The end-to-end pair completed on the corrected boundary. Implementer
`worker-cfa31d1edc9b43ba` completed attempt 4244, pushed commit `531e93f`, and
opened draft Firn PR #67 after `make check` and the redirected help-path test
passed. Independent reviewer `worker-fd0e9655d6a219c6` used `gpt-5.6-terra`
and completed attempt 4250 with a `pass` verdict at the same immutable head.
Both one-shot providers exited while their terminals and self-contained
workspaces remained retained. A final hardened-image smoke test then proved Go
resolution through a login shell, writable home caches and `/var/lib` scratch,
and the complete small-tool baseline. Snowcat's pass moved PR #67 out of draft;
GitHub reported it cleanly mergeable with all eight emitted checks successful.

The next provider slice added a separate checksum-pinned Copilot 1.0.80 image
behind the same OCI adapter. Copilot receives only its exact private
`mcp-config.json`, the two GitHub CLI files, and the existing token environment
names; no Codex input is mounted. Its one-shot invocation disables permission
prompts, remote control, updates, built-in MCP servers, and file logging inside
the established OCI boundary. A hardened no-claim acceptance run authenticated
through the in-memory GitHub token, called Snowcat `list_work` through the
mounted HTTP MCP configuration, made no repository changes, and exited
normally. The observed Firn queue was empty, so a cross-provider delivery pair
remained the next live trial rather than an inferred success.

That live trial subsequently passed. Two Codex and one Copilot OCI discoverer
completed Firn attempts 4256, 4261, and 4257 respectively, after which the
operator admitted bounded implementation proposals. The first admitted quality
gap was already resolved on current main and correctly produced no pull
request. Codex implementer `worker-7b8ecf1802b0d9b1` then completed docs-drift
attempt 4272, opened Firn PR #68 at immutable head `bf8efbd`, and passed all
eight repository checks. Its completion retry exposed transient MCP transport
errors, but independent unauthenticated probes from both the host and the exact
rootless Copilot image resolved the tailnet address and reached Snowcat three
times each; the attempt completed without changing the container network
boundary.

Snowcat's polling verifier emitted review item
`42e20c1d-18b1-402b-b83f-dc7f79593458` after the expected multi-minute delay.
Copilot reviewer `worker-72099702ee60c94a`, launched with model selector
`auto`, completed attempt 4275 under the independent-review contract. The
verifier accepted its pass verdict, moved PR #68 out of draft, and left it
cleanly mergeable. All provider processes exited while Cockpit retained their
terminals and self-contained workspaces.

The trial also exposed an operator-input freshness risk: the selected clean
Firn `main` was behind its local `origin/main` tracking ref, which was itself
behind GitHub. Cockpit intentionally neither fetches nor chooses a remote ref;
the dashboard now resolves and displays the selected immutable commit and its
local ahead/behind relation before every single or fleet launch. A behind or
diverged base requires explicit confirmation. The inspection performs no fetch
and says so, preserving the operator's responsibility for remote freshness.

The third provider slice adds a checksum-pinned Claude Code 2.1.239 image.
It mounts only Claude's exact private OAuth credential plus the two GitHub CLI
files. Snowcat's URL and token enter by environment-variable name and expand
only inside an image-owned strict MCP configuration; no host Claude MCP file is
mounted or parsed. Discoverers and implementers use `sonnet`, while reviewers
use `opus`. An exact hardened-container acceptance run authenticated Claude,
called Snowcat `list_work`, made no claim or repository change, and exited
normally. Claude OCI discoverer `worker-722764f2efb292e4` then claimed Clix
quality-gap attempt 4280 from immutable base `c9942bfa0c35`, completed it, and
exited while its terminal and workspace remained retained.

That discovery proposed a correctly authorized pull-request delivery under
generic kind `implementation`. Admission exposed that Cockpit had incorrectly
treated its earlier successful `*-fix` prompt as a closed Snowcat taxonomy.
Snowcat intentionally permits open work kinds; Cockpit now assigns all
non-discovery, non-review worker kinds to implementers while leaving exact
`release-needed` human-operated. A provenance-rich `quality-gap-fix` would have
been a better child name, but `implementation` is valid and remains claimable.

The same trial proved the base-freshness interaction against Clix: checked-out
`main` was 32 commits behind its refreshed local `origin/main`. The dashboard
displayed both counts and immutable commit `cfecce1e05cd`, required confirmation,
and cancellation allocated no worker.

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
