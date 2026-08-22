# Session summary

Ephemeral session state. Replace the block below at session end; durable
decisions belong in `docs/adr/`, current architecture in `docs/design/`, and
learned corrections in [`.memory/`](../.memory/README.md).

## Current state

- Docker and rootless Podman launch Codex, Claude, and Copilot through the same
  explicit OCI adapter.
- A production-readiness pass is applying the frostyard repository contracts.

## Last landed

- Explicit Docker runtime selection and multi-architecture publication
  workflow.

## Next

- All-repository board-run orchestration with bounded preflight refresh and
  checkout setup.
