# 0012 — Provision repository tools before the lease and derive node state from its sources

- **Status:** Accepted
- **Date:** 2026-08-24

## Context

[ADR-0005](0005-isolate-unattended-workers-in-rootless-oci.md) gave OCI
workers a deliberately generic image — Go, Node.js, Git, GitHub CLI, and a
short list of utilities — with the rule that a tool enters the baseline
only after a repository contract or retained terminal demonstrates the
need. The first board campaigns demonstrated it: `std` and `clix` skipped
lint when `golangci-lint` was absent and their gates passed anyway,
`updex` refused to run, and one campaign lost four workers to in-lease
toolchain downloads crossing the PID ceiling. The stopgap of 2026-08-24
(worker images `v0.1.1`: `golangci-lint` 2.13.1 baked in, Go 1.26.7,
`GOTOOLCHAIN=local`) proved the shape — `clix`, `std`, and `updex` each
went from discovery through an independent `make verify` review on that
image — but an image that accumulates every repository's toolchain is
what ADR-0005 was avoiding, and a pin bump would need an image rebuild and
a digest roll on every node.

The same campaigns exposed two more seams in the node itself, each costing
a lane for hours:

- **The worker kit is a release-time copy.** `internal/profile/worker-kit`
  vendors three Snowcat skills under a lock; the snowcat lane's checkout
  carries the canonical copies, and `InstallKit` refuses a drifted skill.
  Every Snowcat skill change therefore broke the snowcat lane until a
  Cockpit release, a `node install` (which stops the board campaign), a
  by-hand replacement of the node's kit directory, a re-preflight of every
  provider, and a campaign restart
  ([node-service spec](../specs/node-service.md), "Upgrading across a
  worker-kit change").
- **The managed-repository catalog is node-local.** `updex` was
  fleet-enabled in core and enrolled in Snowcat with queued work all day,
  but no node had it in its catalog (`POST /api/v1/repositories`), so no
  lane claimed anything there and the queue looked frozen. A campaign also
  snapshots the catalog at start.

Core [ADR-0043](https://github.com/frostyard/core/blob/main/docs/adr/0043-pin-repository-tools-in-mise-and-name-the-verify-gate.md)
now makes each repository declare its tool pins in `mise.toml` and
`mise.lock`, keeps Go only in `go.mod`, and names the `verify` gate;
Snowcat [ADR-0076](https://github.com/frostyard/snowcat/blob/main/docs/adr/0076-pin-repository-tools-in-the-repository-and-qualify-lanes-by-running-them.md)
commits executors to provision from that pin before any lease exists and
to qualify a lane by running the gate.

## Decision

1. **One provider-collapsed base image.** `oci/base.Containerfile`
   carries all three provider CLIs at their pins and one entrypoint that
   selects the provider from Cockpit's launch argument; the per-provider
   image variables keep working and may all name the same digest. The
   image ships `mise` (pinned, checksum-verified) with `MISE_DATA_DIR`
   pointing at a read-only mount, keeps `GOTOOLCHAIN=local`, and publishes
   its baseline as `oci/baseline.json` — the only enumeration of what the
   base guarantees, which the OCI workers spec references instead of
   restating. It publishes as `ghcr.io/frostyard/snowcat-worker-base`.
2. **Repository tools are provisioned at target preparation, never inside
   the lease.** Before any lease exists (OCI workers spec rule 1
   ordering), the node runs `mise install --locked` in the checkout with a
   per-repository cache under its state directory, verified against
   `mise.lock`, and mounts that cache read-only at the base image's
   `MISE_DATA_DIR`. A repository without `mise.toml` provisions nothing
   and is not unready for it.
3. **A lane is ready when the declaration is satisfied, unready with the
   reason named otherwise.** `mise ls --missing` non-empty, a lock
   mismatch, or a Go that does not satisfy `go.mod` under
   `GOTOOLCHAIN=local` marks the lane unready in inventory and the
   dashboard with the tool or version named; nothing is installed inside
   the container. The provisioned tool set (name, version, lock digest) is
   recorded as non-secret worker metadata beside the image digest.
4. **Repository-specific tools leave the base.** Once every enrolled
   repository declares its linter in `mise.lock`, `golangci-lint` is
   removed from the base and `oci/baseline.json`; until then the base
   carries the fleet's pinned release and tracks the fleet's highest `go`
   directive. A repository needing what mise cannot install extends the
   base with its own image, qualified by running `make check` under
   Cockpit's exact launch limits.
5. **The worker kit is refreshed from its source, not vendored per
   release.** The node records the Snowcat source revision it serves and
   refreshes the kit from that revision; where a checkout is the canonical
   skill source (the snowcat repository itself), the checkout wins and the
   kit is not installed over it. A kit change no longer requires a Cockpit
   release, and a `node install` no longer needs the by-hand kit
   replacement.
6. **The managed-repository catalog derives from core.** The node reads
   core's enabled repository declarations (through the same observed
   authority Snowcat enrolls against) and reports the difference between
   that set and its catalog; a campaign picks up a repository added to the
   catalog without a restart. Enrolling a repository by hand remains
   possible and is reported as such.

## Consequences

- A pin bump is one file in the repository; no image rebuild, no digest
  roll. The download moves from the lease to preparation, verified by the
  lock, and a lane that cannot satisfy its repository's declaration says
  which tool before any lease is charged.
- The container mount posture gains exactly one read-only volume; every
  ADR-0005 isolation rule holds.
- The provider matrix collapses from three images to one; `worker-images.yml`
  publishes one manifest per version.
- The node grows two derived states — kit revision and catalog drift —
  each reported, never silently reconciled; the four-step kit-change
  upgrade in the node-service spec becomes unnecessary once §5 lands and
  is retired then.
- Items 1–4 are Phases 2, 4, and 6 of Snowcat's rollout plan; items 5 and
  6 are Cockpit's own follow-ups and are not prerequisites for the fleet
  adopting mise.
- The Codex adapter's projected lease-proxy defect
  ([snowcat-cockpit#6](https://github.com/frostyard/snowcat-cockpit/issues/6))
  is independent of this decision and is fixed in the entrypoint the
  collapsed image inherits.

## Alternatives considered

- **Keep baking fleet tools into the base:** every repository's toolchain
  in one image, a rebuild per bump, and the supply-chain surface ADR-0005
  minimised; kept only as the stopgap §4 retires.
- **Per-repository images as the default:** a bump then needs a build and a
  digest roll on every node; provisioning from the lock makes the common
  case need no image at all.
- **Install inside the container at launch:** charges the lease, needs a
  writable tool path in a read-only image, and is what lost four workers.
- **Keep the kit vendored but automate the upgrade:** still couples every
  Snowcat skill change to a Cockpit release; refreshing from the recorded
  source revision removes the coupling rather than scripting it.
- **Keep the catalog node-local and document it:** the failure mode is a
  repository nobody works that every authority says is enabled; a
  documented gap is still invisible to the operator watching the queue.

## References

- Shapes: [design/overview.md](../design/overview.md),
  [design/node.md](../design/node.md),
  [specs/oci-workers.md](../specs/oci-workers.md),
  [specs/node-service.md](../specs/node-service.md); Snowcat
  [plans/repository-tooling-rollout.md](https://github.com/frostyard/snowcat/blob/main/docs/plans/repository-tooling-rollout.md)
  (Phases 2, 4, 6)
- Builds on: [ADR-0005](0005-isolate-unattended-workers-in-rootless-oci.md),
  [ADR-0008](0008-run-persistent-multi-repository-board-campaigns.md),
  [ADR-0011](0011-run-the-node-as-a-systemd-user-service.md); core
  [ADR-0043](https://github.com/frostyard/core/blob/main/docs/adr/0043-pin-repository-tools-in-mise-and-name-the-verify-gate.md);
  Snowcat [ADR-0076](https://github.com/frostyard/snowcat/blob/main/docs/adr/0076-pin-repository-tools-in-the-repository-and-qualify-lanes-by-running-them.md)
