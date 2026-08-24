#!/usr/bin/env bash
set -euo pipefail

readonly PROJECT_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
readonly CODEX_ENTRYPOINT="$PROJECT_ROOT/oci/entrypoint.sh"
readonly CODEX_CONTAINERFILE="$PROJECT_ROOT/oci/Containerfile"
readonly CLAUDE_ENTRYPOINT="$PROJECT_ROOT/oci/claude-entrypoint.sh"
readonly CLAUDE_CONTAINERFILE="$PROJECT_ROOT/oci/Containerfile"
readonly COPILOT_ENTRYPOINT="$PROJECT_ROOT/oci/copilot-entrypoint.sh"
readonly COPILOT_CONTAINERFILE="$PROJECT_ROOT/oci/Containerfile"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_contains() {
  local file="$1"
  local text="$2"
  grep -F -- "$text" "$file" >/dev/null || fail "$file does not contain: $text"
}

assert_not_contains() {
  local file="$1"
  local text="$2"
  if grep -F -- "$text" "$file" >/dev/null; then
    fail "$file unexpectedly contains: $text"
  fi
}

assert_contains "$CLAUDE_ENTRYPOINT" '--setting-sources user'
assert_contains "$CLAUDE_ENTRYPOINT" '--append-system-prompt-file /usr/local/share/snowcat-cockpit/claude-system-prompt.txt'
assert_contains "$CLAUDE_ENTRYPOINT" 'for skill in work-snowcat-queue work-snowcat-without-reviews review-snowcat-queue; do'
assert_contains "$CLAUDE_ENTRYPOINT" '[[ -f "$source_skill" && ! -L "$source_skill" ]]'
assert_contains "$CLAUDE_CONTAINERFILE" 'COPY --chmod=0644 oci/claude-system-prompt.txt /usr/local/share/snowcat-cockpit/claude-system-prompt.txt'
assert_contains "$CLAUDE_CONTAINERFILE" 'CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1'
assert_contains "$CODEX_ENTRYPOINT" 'write_codex_worker_profile "$CODEX_HOME/$CODEX_WORKER_PROFILE_NAME.config.toml" "$4" "$3"'
assert_contains "$CODEX_ENTRYPOINT" '--profile "$CODEX_WORKER_PROFILE_NAME"'
assert_not_contains "$CODEX_ENTRYPOINT" '--config "mcp_servers.'
assert_contains "$CLAUDE_ENTRYPOINT" '\"snowcat-cockpit\":{\"type\":\"stdio\"'
assert_contains "$CLAUDE_ENTRYPOINT" '--strict-mcp-config'
assert_contains "$COPILOT_ENTRYPOINT" '\"snowcat-cockpit\":{\"type\":\"local\"'
assert_contains "$COPILOT_ENTRYPOINT" '--disable-mcp-server "$4"'
assert_contains "$COPILOT_ENTRYPOINT" '--additional-mcp-config "$relay_config"'
for entrypoint in "$CODEX_ENTRYPOINT" "$CLAUDE_ENTRYPOINT" "$COPILOT_ENTRYPOINT"; do
  assert_contains "$entrypoint" 'ulimit -c 0'
  assert_contains "$entrypoint" 'lease-proxy'
  assert_contains "$entrypoint" '$3'
done
for containerfile in "$CODEX_CONTAINERFILE" "$CLAUDE_CONTAINERFILE" "$COPILOT_CONTAINERFILE"; do
  assert_contains "$containerfile" 'node:26-bookworm-slim@sha256:cd565714d4da3e84bfd341e31448f81d47c6362198f152345297c9c1154e6341'
  assert_contains "$containerfile" '/usr/local/bin/npm'
done

source "$CODEX_ENTRYPOINT"
profile_root="$(mktemp -d)"
trap 'rm -rf -- "$profile_root"' EXIT
profile_path="$profile_root/$CODEX_WORKER_PROFILE_NAME.config.toml"
write_codex_worker_profile "$profile_path" "snowcat.direct" "worker-0123456789abcdef"
assert_contains "$profile_path" '[mcp_servers."snowcat.direct"]'
assert_contains "$profile_path" 'enabled = false'
assert_contains "$profile_path" '[mcp_servers."snowcat-cockpit"]'
assert_contains "$profile_path" 'command = "/workspace/.agents/bin/snowcat-cockpit"'
assert_contains "$profile_path" 'args = ["worker", "lease-proxy", "--worker", "worker-0123456789abcdef", "--workspace", "/workspace"]'
assert_contains "$profile_path" 'required = true'
[[ "$(stat -c '%a' "$profile_path")" == "600" ]] || fail "Codex worker profile is not mode 0600"
if write_codex_worker_profile "$profile_root/invalid.config.toml" 'snowcat"]' "worker-0123456789abcdef" >/dev/null 2>&1; then
  fail "Codex worker profile accepted an invalid direct MCP server name"
fi

printf 'PASS: OCI entrypoints\n'
