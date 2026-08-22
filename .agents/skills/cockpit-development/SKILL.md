---
name: cockpit-development
description: Develop and verify Snowcat Cockpit without widening its execution-side boundary or exposing worker credentials.
---

# Develop Snowcat Cockpit

1. Read `AGENTS.md` and `docs/design/overview.md` before changing scope.
2. Preserve request argv boundaries, loopback listeners, explicit cleanup, and
   the rule that workers call Snowcat MCP directly.
3. Add runner-backed tests for commands and lifecycle changes. Add browser
   coverage for dashboard contracts.
4. Update the governing spec beside behavior and write an ADR first for a new
   ownership boundary.
5. Run `make ci`; workflow changes additionally run `actionlint`.
