#!/usr/bin/env bash
set -euo pipefail

readonly PROJECT_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
readonly WRAPPER="$PROJECT_ROOT/bin/snowcat-cockpit-serve"
readonly TEST_ROOT="$(mktemp -d /tmp/snowcat-cockpit-observer-wrapper.XXXXXX)"
readonly CREDENTIAL_FILE="$TEST_ROOT/profile-observer.env"
readonly FAKE_COCKPIT="$TEST_ROOT/snowcat-cockpit"
readonly FAKE_BIN="$TEST_ROOT/bin"

cleanup() {
  rm -rf -- "$TEST_ROOT"
}
trap cleanup EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

cat >"$CREDENTIAL_FILE" <<'EOF'
# token id fixture; revoke with the Snowcat queue CLI
export SNOWCAT_OBSERVER_TOKEN=snowcat_0123456789abcdef_0123456789abcdef0123456789abcdef
EOF
chmod 0600 "$CREDENTIAL_FILE"

cat >"$FAKE_COCKPIT" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == "serve" ]]
[[ "$2" == "--listen" ]]
[[ "$3" == "127.0.0.1:17682" ]]
[[ "$SNOWCAT_COCKPIT_MCP_URL" == "https://snowcat.goat-snake.ts.net/mcp" ]]
[[ "$SNOWCAT_COCKPIT_MCP_TOKEN" == "snowcat_0123456789abcdef_0123456789abcdef0123456789abcdef" ]]
[[ -z "${SNOWCAT_OBSERVER_TOKEN+x}" ]]
printf 'wrapper fixture passed\n'
EOF
chmod 0700 "$FAKE_COCKPIT"

output="$(
  SNOWCAT_COCKPIT_OBSERVER_ENV="$CREDENTIAL_FILE" \
    SNOWCAT_COCKPIT_BIN="$FAKE_COCKPIT" \
    "$WRAPPER" --listen 127.0.0.1:17682
)"
[[ "$output" == "wrapper fixture passed" ]] || fail "unexpected wrapper output: $output"

mkdir -p "$FAKE_BIN"
cat >"$FAKE_BIN/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == "auth" && "$2" == "token" ]]
printf 'github-token-fixture\n'
EOF
chmod 0700 "$FAKE_BIN/gh"

cat >"$FAKE_COCKPIT" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == "serve" ]]
[[ "$GH_TOKEN" == "github-token-fixture" ]]
printf 'OCI wrapper fixture passed\n'
EOF
chmod 0700 "$FAKE_COCKPIT"

output="$(
  PATH="$FAKE_BIN:$PATH" \
    SNOWCAT_COCKPIT_OCI_COPILOT_IMAGE="sha256:fixture" \
    SNOWCAT_COCKPIT_OBSERVER_ENV="$CREDENTIAL_FILE" \
    SNOWCAT_COCKPIT_BIN="$FAKE_COCKPIT" \
    "$WRAPPER"
)"
[[ "$output" == "OCI wrapper fixture passed" ]] || fail "unexpected OCI wrapper output: $output"

chmod 0644 "$CREDENTIAL_FILE"
if SNOWCAT_COCKPIT_OBSERVER_ENV="$CREDENTIAL_FILE" SNOWCAT_COCKPIT_BIN="$FAKE_COCKPIT" \
  "$WRAPPER" >/dev/null 2>&1; then
  fail "wrapper accepted a credential file with mode 0644"
fi

printf 'PASS: observer wrapper\n'
