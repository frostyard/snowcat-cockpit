---
name: work-snowcat-without-reviews
description: Use when working the Snowcat queue as a worker that may perform discovery, implementation, fixes, and cures but must not claim pull-request review work.
---

# Work the Snowcat queue without reviews

Claim non-review work without duplicating Snowcat's canonical worker lifecycle.

**REQUIRED SUB-SKILL:** Use `work-snowcat-queue` for every rule except the
`claim_work` call in step 2 of its claim section. The selection gate below
replaces that call; after a claim, follow the canonical skill exactly.

## Selection gate

1. Call `list_work` exactly once with `status: "queued"`, `limit: 100`, and any
   repository restriction the operator supplied. Call it exactly once more
   with the same bound and repository but `status: "claimed"`.
2. Build a deduplicated array from the queued kinds plus claimed items whose
   newest attempt outcome is exactly `expired`, excluding only the exact kind
   `pr-review`. Intersect it with any kinds the operator named. Do not use a
   fixed kind whitelist: a future non-review kind remains eligible. Never
   include a claimed item whose newest attempt has no outcome or any outcome
   other than `expired`.
3. If the array is empty, stop cleanly: the bounded listing exposed no eligible
   kind. Do not claim a review merely because it is urgent, first, or the only
   visible queued item.
4. Call `claim_work` exactly once with `kinds` set to that array and the same
   repository restriction. If it returns `null`, stop; do not retry with
   widened or omitted kinds.
5. Inspect and complete the claimed item through `work-snowcat-queue`. In an
   explicitly requested loop, repeat this selection gate before every claim.

## Boundary

| Kind | Action |
| --- | --- |
| `pr-review` | Never claim |
| `pr-review-fix` | Eligible implementation work |
| `pr-cure` / `pr-cure-change` | Eligible |
| `*-discovery`, `issue-resolution`, fixes | Eligible |
| A future non-review kind | Eligible when queued or reclaimable after an expired attempt |

## Example

Claimable kinds are `pr-review`, `quality-gap-discovery`, and `new-fix-kind`.
Call `claim_work` once with
`kinds: ["quality-gap-discovery", "new-fix-kind"]`.

## Rationalizations

| Thought | Correction |
| --- | --- |
| "No concrete kinds were named, so the claim must be unrestricted." | The two bounded `list_work` calls supply the current positive kind set. |
| "Claim the urgent review and release it immediately." | Filter before claiming; claim-and-release creates avoidable lease churn. |
| "Use the implementation kinds I already know." | Derive kinds from the live queue so a new non-review kind is not hidden. |

## Red flags

- Omitting `kinds` from `claim_work`.
- Planning to release an item solely because it is `pr-review`.
- Hard-coding Snowcat's kind taxonomy.

Any red flag means stop and return to the selection gate.

## Common mistakes

- Retrying an empty filtered claim without `kinds`: stop on `null` instead.
- Reimplementing the worker lifecycle here: defer duplicate detection,
  permissions, leases, evidence, artifacts, follow-ups, and completion to
  `work-snowcat-queue`.

## Execution target

Honor the claimed item's `executionTarget` (ADR-0073) before touching the
repository — the bound pull request's branch at its recorded head for
`existing-pull-request`, a fresh branch from a fresh base for
`new-pull-request`, a detached never-mutated checkout for `read-only` — and
declare one on every follow-up you propose.
