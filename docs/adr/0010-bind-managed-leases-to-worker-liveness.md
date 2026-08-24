# 0010 — Bind managed leases to worker liveness

- **Status:** Accepted
- **Date:** 2026-08-23

## Context

Managed-worker prompts previously requested one 3,600-second lease and asked the
model to renew around long steps. Provider exits, tool-call omissions, and long
front-loaded leases could therefore leave Snowcat work claimed long after the
worker process that could finish it was gone. Conversely, a live worker could
lose a lease because renewal depended on the model remembering a prompt
instruction.

Cockpit also could not distinguish a provider that merely described completion
from one whose `complete_work` call reached Snowcat and received a successful
response. The node must not solve either problem by owning a lease token,
polling Snowcat, or copying MCP credentials into its durable state.

## Decision

Every managed provider connects to Snowcat through an exact worker-local copy
of the running Cockpit executable over MCP stdio. The relay forwards the
provider's MCP protocol to Snowcat's existing HTTP endpoint and keeps the
upstream URL, bearer credential, and active lease token only in its process
memory and HTTP requests.

The relay bounds `claim_work` and `heartbeat_work` to 120 seconds and renews an
active lease every 30 seconds while the provider retains the relay's stdio
process. EOF stops renewal. A definitive renewal rejection, or failure to
renew before the last Snowcat-reported expiry, marks the lease lost, writes a
credential-free local marker, emits `SNOWCAT_COCKPIT_LEASE_LOST` on stderr, and
refuses later provider tool calls with the same explicit signal.

The relay records `completeAttempted` before forwarding `complete_work` and
records `completeAcknowledged` only after Snowcat returns a successful MCP tool
result. Its private workspace marker contains only the worker ID, item ID,
coarse lifecycle status, those two booleans, and an update time. The Cockpit
node does not import this marker into durable node state and never receives the
lease token.

Provider launch configuration disables the previously configured direct
Snowcat server for that invocation and exposes only the worker-local relay.
`SNOWCAT_MCP_URL` and `SNOWCAT_MCP_TOKEN` remain inherited by name and never
enter Cockpit argv, files, logs, records, or API responses.

## Consequences

- Lease duration now follows worker MCP liveness instead of a one-hour claim.
- A live worker no longer depends solely on model-authored heartbeat timing.
- Retained terminal evidence and the local marker distinguish completion
  attempted from completion acknowledged without treating either as Snowcat's
  queue authority.
- The worker-local relay is now in the managed worker's MCP transport path.
  Relay failure is intentionally fail-closed and can make Snowcat temporarily
  unavailable to that worker.
- A hard crash can still leave at most the remaining short lease interval; no
  worker-side process can synchronously release a lease after it has died.
- Existing provider preflight still proves the provider's configured Snowcat
  access before launch. The managed invocation then replaces that direct path
  with the relay for the single worker process.

## Alternatives considered

- **Keep prompt-directed 3,600-second renewal:** rejected because it couples
  correctness to model compliance and leaves long orphaned claims.
- **Have the Cockpit node renew leases:** rejected because the node would need
  lease tokens and become a second coordinator with liveness inference.
- **Persist lease tokens for a restartable relay:** rejected because Cockpit's
  credential boundary forbids lease tokens in files or state.
- **Infer completion from provider exit or terminal text:** rejected because
  neither proves that Snowcat accepted `complete_work`.

## References

- Shapes: [Cockpit architecture](../design/overview.md),
  [Cockpit node](../design/node.md),
  [managed workers](../specs/managed-workers.md),
  [OCI workers](../specs/oci-workers.md)
- Builds on: [ADR-0001](0001-keep-cockpit-outside-snowcat.md),
  [ADR-0002](0002-build-a-node-local-cockpit-appliance.md),
  [ADR-0009](0009-observe-reclaimable-snowcat-work.md)
