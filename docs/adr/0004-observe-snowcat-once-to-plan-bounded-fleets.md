# 0004 — Observe Snowcat once to plan bounded fleets

- **Status:** Accepted
- **Date:** 2026-08-21

## Context

The managed-worker trial proved all three execution lanes against
`frostyard/firn`: discovery produced an operator-admitted proposal,
implementation delivered its pull request, independent review passed, and the
operator merged. It also proved that launching without queue awareness wastes
an isolated worktree and an inference invocation: a fix-only worker found only
discovery and review work, made no claim, and left its interactive provider
process running.

Phase 4 needs enough queue visibility to label the three lanes, bound one
requested batch, and avoid obviously empty launches. Snowcat remains the sole
coordinator. Cockpit must not read Snowcat's database, claim on behalf of a
worker, retain a queue copy, observe lease tokens, or continuously refill
capacity.

Snowcat's supported Streamable HTTP MCP surface exposes `list_work` as a
bounded read that never returns a lease token. Its minted tokens identify a
member and client and may restrict the work kinds that `claim_work` can select,
but they cannot currently be scoped to a set of MCP tools. Cockpit credentials
must remain out of arguments, files, logs, API responses, and local state.

## Decision

Cockpit performs an **operator-triggered, one-shot queue observation** through
Snowcat's supported HTTP MCP `list_work` tool.

- The operator supplies the MCP URL as non-secret configuration and supplies a
  dedicated minted bearer token only through the Cockpit process environment.
  Cockpit never accepts the token as a CLI argument or HTTP field and never
  writes, logs, returns, or persists it.
- The dedicated token is minted with the claim-kind restriction
  `cockpit-observer-no-claim`. Frostyard reserves that synthetic kind and never
  seeds or admits an item with it. Cockpit refuses a snapshot if that kind is
  observed. This makes `claim_work` select nothing under the deployed Snowcat
  contract while leaving `list_work` available.
- One observation calls `list_work` exactly once with an operator-selected
  repository, logical status `queued`, and limit `100`. A result of 100 items
  is explicitly `truncated`; Cockpit never implies a complete count beyond the
  MCP bound.
- Cockpit validates the structured MCP result and projects only non-secret
  planning fields needed by the dashboard: item ID, repository, kind,
  priority, `allowedActions`, `requiredArtifact`, and role classification. It
  holds that projection only for the request and does not write it to node or
  worker state.
- Classification is deterministic and local: `*-discovery` is discoverer,
  `*-fix` plus exact `pr-cure` and `pr-cure-change` is implementer, exact
  `pr-review` is reviewer, and everything else is unassigned. Contract
  warnings are derived from Snowcat fields and never widen authority.
- A fleet launch consumes the operator's explicit repository, role, provider
  policy, and maximum count. It may cap the batch to the observed eligible
  count, launches once, reports every per-slot outcome, and never refills.
  Workers still call `claim_work` directly; the snapshot is advisory and a
  concurrent node may win every lease.
- No timer, watch loop, background refresh, queue database, or durable queue
  cache is introduced. The page load and an explicit Refresh action may each
  request one new snapshot.

The first implementation does not claim worker-to-item correlation. With
authenticated HTTP MCP, Snowcat stores the token principal as lease owner and
the worker-supplied Cockpit ID only as event provenance; MCP does not expose
that event stream. Cockpit must display OS-process state and aggregate queue
state separately rather than infer a relationship it cannot prove.

## Consequences

- The dashboard can show bounded eligible counts and avoid known-empty fleet
  launches without becoming a scheduler or queue replica.
- Snowcat leases remain the concurrency authority across multiple Cockpit
  nodes; a snapshot can become stale immediately and the UI must say so.
- The token is absent from Cockpit state and files, but Snowcat does not yet
  enforce tool-level read-only scope. The never-seeded claim kind is a narrow
  compatibility construction, not a general authorization primitive. If
  Snowcat adds tool-scoped tokens, Cockpit must migrate to a token limited to
  `list_work` and `get_work`.
- Direct observation avoids spending model inference on queue rendering and
  avoids treating model-parsed, potentially prompt-injected work text as a
  control-plane response.
- Completion-to-process reconciliation remains operator-visible and explicit.
  Reliable automatic correlation requires a future supported Snowcat read
  projection that exposes the authenticated claim's client label without a
  lease token.
- Failure to configure or authenticate observation disables queue counts and
  batch planning but does not disable the already-proven single-worker launch.

## Alternatives considered

- **Launch the requested count blindly:** rejected as the only fleet workflow
  because the trial measured unnecessary worktrees and inference when a lane
  has no eligible work. It remains the single-worker escape hatch.
- **Ask a coding agent to read and summarize the queue:** rejected because it
  spends inference, adds provider variability, and lets untrusted queue text
  influence parsing of a control response.
- **Store an unrestricted Snowcat token in Cockpit state:** rejected because it
  violates the credential boundary and gives a stolen state directory a
  reusable bearer credential.
- **Continuously poll or automatically refill:** rejected because one bounded
  operator action is the accepted Phase 4 scope; ongoing scheduling needs a
  separate measured finding and ADR.
- **Read Snowcat's SQLite database:** rejected because it bypasses Snowcat's
  supported contract and breaks the coordination/execution boundary.
- **Wait for Snowcat tool-scoped tokens:** rejected as a blocker for the bounded
  trial. The restricted synthetic kind makes claims unavailable for the
  dedicated token under the current contract and has an explicit migration
  path.

## References

- Shapes: [Cockpit node](../design/node.md),
  [production roadmap](../plans/0002-production-roadmap.md)
- Builds on: [ADR-0001](0001-keep-cockpit-outside-snowcat.md),
  [ADR-0002](0002-build-a-node-local-cockpit-appliance.md)
- Snowcat contracts: ADR-0063 (authenticated HTTP MCP) and ADR-0069
  (`requiredArtifact`) in `frostyard/snowcat`
