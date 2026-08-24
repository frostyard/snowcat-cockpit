---
name: review-snowcat-queue
description: Claim and complete one Snowcat `pr-review` item — judge a draft pull request against its origin contract and return a structured verdict — without touching GitHub or doing any other kind of work. Use whenever asked to review Snowcat queue pull requests, act as an independent reviewer, or operate as a Snowcat review-only worker.
---

# Review the Snowcat queue (review-only)

Use the configured `snowcat` MCP tools to claim and judge **`pr-review`** items
and nothing else. This is the review half of ADR-0029/ADR-0065: an independent
worker reads a draft pull request, tries to falsify its claims, and returns a
verdict Snowcat acts on deterministically. The operator owns this worker client
and its sandbox; Snowcat owns queue authorization and bookkeeping.

This skill never writes code, opens or edits a pull request, comments on GitHub,
or creates follow-ups. Its only output is a `review` verdict. If you need to
also *do* repository work, use `work-snowcat-queue` instead.

## Claim one review

1. Choose a non-secret worker identity stable for this invocation, such as
   `<client>:review:<session>`.
2. Call `claim_work` **once with `kinds: ["pr-review"]`** (and `repository` when
   the operator named one). Restricting kinds keeps this worker review-only even
   if its credential is broader; prefer a token minted `--kinds pr-review`
   (or a stdio server with `SNOWCAT_MCP_KINDS=pr-review`) so the credential
   itself enforces the scope.
3. Stop cleanly when `claim_work` returns `null` — no review is queued. Do not
   poll or loop unless the operator explicitly requested continuous review.
   `proposed` items are awaiting admission and are not available work.
4. If a claim ever yields a kind other than `pr-review` (an unrestricted
   credential), `release_work` it immediately with that reason — this worker
   only reviews.
5. Read `operatorNotes` and `previousResults` before starting: each note is an
   operator or policy `requeue`, `defer`, or `note` with its reason, and each
   previous result is a prior round's or a blocked verdict's reason. They
   override nothing in the definition; if a note conflicts with the definition,
   block and say so.
6. Keep the lease token private. Never write it into files, logs, or evidence.

## Judge the pull request (`pr-review`)

A `pr-review` item binds one draft pull request at one head to one review round
(`sourceRef` is `pr-review:<url>@<head SHA>`; the item's `review` record carries
the head, the round — at most three per pull request — the origin item, the
previous round's blockers, and the models the author and previous reviewer
reported). Adversarial means trying to falsify the pull request's claims, not
maximizing comments.

- Read the origin item with `get_work(review.originItemId)`: its objective,
  acceptance criteria, instructions, and `sourceRef` issue are the contract the
  pull request claims to satisfy. Read the pull request and its diff at exactly
  `review.headSha` (`gh pr diff <n>` or `gh api repos/<owner>/<repo>/pulls/<n>/files`);
  check the head out and run the repository's non-mutating gate — `make
  verify` where it exists — never `make check` or any target that formats or
  rewrites files (`run-tests` is allowed; `write` and anything on GitHub are
  not).
- **Cognitive diversity.** If you completed the origin item yourself in this
  session or otherwise authored the pull request, `release_work` before judging
  so an independent worker reviews it. Prefer a different model or provider
  from `review.authorModel` when your client can choose.
- A **blocker** is only a concrete correctness or security defect, an unmet
  acceptance criterion of the origin item, unauthorized or out-of-scope
  behaviour, false or materially insufficient evidence, missing required
  validation, or a compatibility or contract break. Style, alternative valid
  designs, speculative concerns, and "while you are here" work are advisories at
  most — never blockers. On round 2 or 3, examine `review.priorBlockers` and the
  diff since the previous head: reuse a fingerprint for a blocker still open, and
  name a new one only when the diff introduced it or made it newly assessable.
- **Description blockers (ADR-0067).** A blocker whose only cure is an edit to
  the pull request's *description* — not the diff — for example a missing or
  wrong required template section, risk tier, or evidence claim, MUST carry the
  fingerprint prefix `contract:pr-body:` and name the description, not a file,
  as its `location`. Use this prefix only when a description edit alone would
  cure the defect; a defect in the diff is a normal blocker even if the
  description also needs updating. Snowcat routes `contract:pr-body:` blockers
  straight to a human instead of a `pr-review-fix` — never mis-fingerprint a
  tree defect this way just because it is also documented in the description.
- You are **read-only on GitHub**: no comment, review, approval, push, edit, or
  ready-for-review change, and no follow-ups. Snowcat acts on your verdict — a
  `pass` marks the draft ready for a human, a `block` schedules one bounded
  `pr-review-fix`, a third-round block goes to a human — and never merges.
- If the head moved since the item was created, `block_work` with that reason;
  Snowcat refuses a verdict for a moved head.

## Finish

- Complete with `review` on `complete_work`: `decision` `pass` | `block` |
  `unable-to-review`; at most five `blockers`, each `{ fingerprint (stable,
  e.g. `defect:<path>:<slug>`), location, contract, impact, resolution,
  verification }`; at most three `advisories` `{ fingerprint, text }`. A block
  needs at least one blocker; a pass carries none.
- Put the head SHA you reviewed and the checks you ran in `result.evidence`, and
  the model you ran in `result.model` (provenance, never verified). Do **not**
  report the pull request as an artifact — it is not yours — and create no
  `followUps` (a `pr-review` creates none): send `result.artifacts: []` and
  `followUps: []`, or omit both — the `complete_work` schema defaults each
  to an empty array, so either form is accepted.
- Call `release_work` when you have not begun judging and another worker can
  safely retry, including every self-authored item and any kind other than
  `pr-review`. Reserve `unable-to-review` for an independent reviewer who began
  judging but lacks a required input or capability to produce a verdict. Call
  `block_work` only when operator input or an external state change is required,
  such as a moved head.
- Stop after this one review unless the operator explicitly requested another.
