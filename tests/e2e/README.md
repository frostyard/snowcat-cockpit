# End-to-end tests

The `cockpit.test.sh` alias beside this file points at the real tmux-backed
black-box suite in [`test/cockpit.test.sh`](../../test/cockpit.test.sh). It
launches the checked-in Bash CLI, exercises retained tmux slots and the ttyd
argv boundary with fake providers, and explicitly stops every fixture slot.

Run it from the repository root:

```text
make test-spike
```

The observer-wrapper and OCI-entrypoint black-box suites run in the same
`make ci` gate.
