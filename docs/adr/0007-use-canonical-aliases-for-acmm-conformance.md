# 0007 — Use canonical aliases for ACMM conformance

- **Status:** Accepted
- **Date:** 2026-08-21

## Context

Before its first GitHub push, Snowcat Cockpit needs the repository surfaces
used by frostyard's agentic fleet and Hive ACMM evaluation. Several accepted
paths already have canonical equivalents. Duplicating those bodies would make
provider instructions and quality contracts drift. Directory criteria cannot
be symlinks because the Git tree API represents a symlink as a blob.

## Decision

Use committed relative symlinks for file aliases and real Git trees for every
directory criterion. Edit these targets, never their aliases:

| Alias | Canonical target | Purpose |
| --- | --- | --- |
| `CLAUDE.md` | `AGENTS.md` | Claude instructions |
| `GEMINI.md` | `AGENTS.md` | Gemini instructions |
| `CONTRIBUTING.md` | `AGENTS.md` | Contributor instructions |
| `.cursorrules` | `AGENTS.md` | Cursor instructions |
| `.github/copilot-instructions.md` | `../AGENTS.md` | Copilot instructions |
| `.claude/skills` | `../.agents/skills` | Portable skill catalog |
| `docs/metrics.md` | `specs/pr-acceptance-metric.md` | PR metric |
| `docs/review-rubric.md` | `specs/pr-review-rubric.md` | Review contract |
| `docs/quality.md` | `design/quality-loop.md` | Quality system |
| `tests/e2e/cockpit.test.sh` | `../../test/cockpit.test.sh` | Existing black-box suite |

Aliases are not canonical docs: they receive no index entry and carry no
independent cross-link obligation. `.agents/skills/`, `.github/ISSUE_TEMPLATE/`,
`.github/prompts/`, `.memory/`, and `tests/e2e/` remain real trees.
`scripts/check-docs.mjs` checks index coverage, relative links, and every
repo-contained symlink against non-relaxing 1.0 thresholds.

If a file-existence evaluator rejects one symlink, replace only that alias
with a real pointer stub in one commit; the single-canonical-body decision
stands.

## Consequences

- Every provider reads one instruction body and every quality alias resolves
  to one contract.
- Windows contributors need WSL or `core.symlinks=true`.
- New aliases require this registry and the integrity gate to change together.
- The pre-publication repository has no ACMM issues to close; a later evaluator
  can map criteria to these paths without manufacturing new content.

## Alternatives considered

- **Duplicate real files:** rejected because their content would drift.
- **Symlink directory criteria:** rejected because GitHub exposes them as
  blobs, not trees.
- **Empty stubs:** rejected because conformance surfaces must do real work.

## References

- Shapes: [quality loop](../design/quality-loop.md),
  [PR review rubric](../specs/pr-review-rubric.md),
  [PR acceptance metric](../specs/pr-acceptance-metric.md)
- Builds on:
  [frostyard/core ADR-0029](https://github.com/frostyard/core/blob/main/docs/adr/0029-acmm-conformance-via-canonical-aliases.md)
