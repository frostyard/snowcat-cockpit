# Documentation

Docs are split by the question they answer:

| Directory | Question | Contents |
| --- | --- | --- |
| [adr/](adr/) | **Why** did we choose this? | Architecture Decision Records — semantically immutable once accepted |
| [design/](design/) | **How** does it fit together? | Living documents describing the current architecture |
| [specs/](specs/) | **What exactly** is the contract? | Precise, testable interface definitions |
| [plans/](plans/) | **When/in what order** do we build? | Roadmaps and phase plans; updated as work lands |

Organization-wide decisions that bind this repository are indexed separately
in [org-adrs.md](org-adrs.md).

## Index

### Decisions (ADRs)

- [0001 — Keep the cockpit outside Snowcat](adr/0001-keep-cockpit-outside-snowcat.md)
- [0002 — Build a node-local Cockpit appliance](adr/0002-build-a-node-local-cockpit-appliance.md)
- [0003 — Isolate each managed worker terminal](adr/0003-isolate-each-managed-worker-terminal.md)
- [0004 — Observe Snowcat once to plan bounded fleets](adr/0004-observe-snowcat-once-to-plan-bounded-fleets.md)
- [0005 — Isolate unattended workers in rootless OCI containers](adr/0005-isolate-unattended-workers-in-rootless-oci.md)

### Design

- [Cockpit architecture](design/overview.md)
- [Cockpit node](design/node.md)

### Specs

- [Launcher CLI](specs/launcher-cli.md)
- [Node CLI and HTTP API](specs/node-api.md)
- [Provider preflight](specs/provider-preflight.md)
- [Managed workers](specs/managed-workers.md)
- [Rootless OCI workers](specs/oci-workers.md)
- [Queue observation and bounded fleets](specs/queue-observation-and-fleets.md)
- [Worker profiles and locked skill kit](specs/worker-profiles.md)

### Plans

- [tmux and ttyd spike](plans/0001-spike-roadmap.md)
- [Production Cockpit node](plans/0002-production-roadmap.md)

### Organization decisions

- [Binding frostyard/core ADRs](org-adrs.md)

## Conventions

- **New docs start from their category's `TEMPLATE.md`** in each directory.
- New decision → new ADR with the next number; if it reverses an old one, mark
  the old one `Superseded by NNNN` rather than editing it.
- Accepted ADRs are semantically immutable. A semantic change requires a new
  ADR; link-only repairs do not change the decision.
- Design docs are updated in place to reflect current behavior.
- Specs change only alongside the code that implements them.
- Cross-links between ADRs, design docs, specs, and plans are mandatory in both
  directions.
- Adding a doc means adding it to the index above.
- Repo-local decisions belong in `docs/adr/`; organization-wide decisions
  belong in frostyard/core and are listed in [org-adrs.md](org-adrs.md).
