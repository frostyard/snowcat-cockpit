#!/usr/bin/env bash
set -euo pipefail

readonly PROJECT_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
readonly COCKPIT="$PROJECT_ROOT/bin/snowcat-cockpit"
readonly TEST_ROOT="$(mktemp -d /tmp/snowcat-cockpit-test.XXXXXX)"
readonly TEST_SOCKET="snowcat-cockpit-test-$$"
readonly TEST_SESSION="cockpit-test"
readonly REAL_TMUX="$(command -v tmux)"

export SNOWCAT_COCKPIT_SOCKET="$TEST_SOCKET"
export SNOWCAT_COCKPIT_SESSION="$TEST_SESSION"
export SNOWCAT_COCKPIT_TMUX="$REAL_TMUX"
export SNOWCAT_COCKPIT_TTYD_INTERFACE="lo-test"
export TMUX_TMPDIR="$TEST_ROOT"
export COCKPIT_TEST_PROVIDER_CAPTURE="$TEST_ROOT/provider-arguments"

cleanup() {
  "$REAL_TMUX" -L "$TEST_SOCKET" kill-server 2>/dev/null || true
  rm -rf -- "$TEST_ROOT"
}
trap cleanup EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_contains() {
  local haystack=$1
  local needle=$2
  [[ "$haystack" == *"$needle"* ]] || fail "expected output to contain '$needle': $haystack"
}

wait_for_dead_pane() {
  local slot=$1
  local attempt
  local dead
  for attempt in {1..50}; do
    dead="$($REAL_TMUX -L "$TEST_SOCKET" display-message -p \
      -t "$TEST_SESSION:$slot" '#{pane_dead}' 2>/dev/null || true)"
    [[ "$dead" == "1" ]] && return 0
    sleep 0.05
  done
  fail "slot did not exit: $slot"
}

mkdir -p "$TEST_ROOT/bin"
cat >"$TEST_ROOT/bin/codex" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$@" >"$COCKPIT_TEST_PROVIDER_CAPTURE"
EOF
chmod +x "$TEST_ROOT/bin/codex"

empty="$($COCKPIT list)"
assert_contains "$empty" "no cockpit slots"

$COCKPIT start alpha "$TEST_ROOT" -- sh -c 'printf "alpha-ready\n"; sleep 30'
$COCKPIT start beta "$TEST_ROOT" -- sh -c 'printf "beta-done\n"'
wait_for_dead_pane beta

listing="$($COCKPIT list)"
assert_contains "$listing" "alpha | running"
assert_contains "$listing" "beta | exited"

beta_output="$($REAL_TMUX -L "$TEST_SOCKET" capture-pane -p -S - -t "$TEST_SESSION:beta")"
assert_contains "$beta_output" "beta-done"

if $COCKPIT start alpha "$TEST_ROOT" -- true >/dev/null 2>&1; then
  fail "duplicate slot was accepted"
fi

PATH="$TEST_ROOT/bin:$PATH" $COCKPIT work gamma codex "$TEST_ROOT" \
  frostyard/updex issue-resolution
wait_for_dead_pane gamma

provider_arguments="$(<"$COCKPIT_TEST_PROVIDER_CAPTURE")"
assert_contains "$provider_arguments" "Work the Snowcat queue for frostyard/updex, issue-resolution items only."
assert_contains "$provider_arguments" "Claim at most one item, then stop."

cat >"$TEST_ROOT/bin/ttyd" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$@" >"$COCKPIT_TEST_TTYD_CAPTURE"
EOF
chmod +x "$TEST_ROOT/bin/ttyd"
export COCKPIT_TEST_TTYD_CAPTURE="$TEST_ROOT/ttyd-arguments"

SNOWCAT_COCKPIT_TTYD="$TEST_ROOT/bin/ttyd" $COCKPIT web 9876
ttyd_arguments="$(<"$COCKPIT_TEST_TTYD_CAPTURE")"
assert_contains "$ttyd_arguments" "lo-test"
assert_contains "$ttyd_arguments" "9876"
assert_contains "$ttyd_arguments" "-W"
assert_contains "$ttyd_arguments" "-O"
assert_contains "$ttyd_arguments" "attach-session"
assert_contains "$ttyd_arguments" "$TEST_SESSION"

$COCKPIT stop beta
$COCKPIT stop gamma
remaining="$($COCKPIT list)"
assert_contains "$remaining" "alpha | running"
$COCKPIT stop alpha

empty_again="$($COCKPIT list)"
assert_contains "$empty_again" "no cockpit slots"

printf 'PASS: cockpit lifecycle\n'
