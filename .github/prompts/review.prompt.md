# Review a Snowcat Cockpit pull request

Review only; never approve, merge, publish, or push to `main`.

1. Read [AGENTS.md](../../AGENTS.md) and the
   [PR review rubric](../../docs/specs/pr-review-rubric.md).
2. Verify each rubric row independently and cite exact file/line evidence for
   failures.
3. Run `make ci`. If workflows changed, also run `actionlint` and confirm every
   `uses:` is a full commit SHA with a version comment.
4. Re-check the credential, Snowcat, loopback, argv, retained-workspace, and
   explicit-cleanup boundaries from the PR template.
5. Report blockers before advisories and state plainly when all rows pass.
