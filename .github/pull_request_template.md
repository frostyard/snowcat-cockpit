<!-- Reviews apply docs/specs/pr-review-rubric.md. The author never merges or
approves their own work. -->

## Summary

<!-- What changes, why, and which issue or Snowcat item it resolves. -->

## Risk tier

<!-- Declare the highest applicable frostyard tier and justify it. Changes to
credential projection, worker authority, networking, or lifecycle are never
docs-only risk. -->

Risk tier: <!-- tier — justification -->

## Boundary check

- [ ] Snowcat's MCP contract and databases remain untouched
- [ ] No provider, MCP, GitHub, or lease credential enters args, logs, or state
- [ ] Writable terminal surfaces remain loopback-only
- [ ] Cleanup remains an explicit operator action

## Docs housekeeping

- [ ] New docs started from their category `TEMPLATE.md`
- [ ] Every new canonical doc is indexed in `docs/README.md`
- [ ] ADR, design, spec, and plan links run both ways
- [ ] Conformance aliases in ADR-0007 were not edited as content

## Verification

- [ ] `make ci`
- [ ] Workflow changes pass `actionlint`
- [ ] Every action is SHA-pinned with a version comment and checkout disables
      persisted credentials
