#!/usr/bin/env bash
set -euo pipefail

readonly PROJECT_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
readonly CLAUDE_ENTRYPOINT="$PROJECT_ROOT/oci/claude-entrypoint.sh"
readonly CLAUDE_CONTAINERFILE="$PROJECT_ROOT/oci/Claude.Containerfile"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_contains() {
  local file="$1"
  local text="$2"
  grep -F -- "$text" "$file" >/dev/null || fail "$file does not contain: $text"
}

assert_contains "$CLAUDE_ENTRYPOINT" '--setting-sources user'
assert_contains "$CLAUDE_ENTRYPOINT" '--append-system-prompt-file /usr/local/share/snowcat-cockpit/claude-system-prompt.txt'
assert_contains "$CLAUDE_ENTRYPOINT" 'for skill in work-snowcat-queue work-snowcat-without-reviews review-snowcat-queue; do'
assert_contains "$CLAUDE_ENTRYPOINT" '[[ -f "$source_skill" && ! -L "$source_skill" ]]'
assert_contains "$CLAUDE_CONTAINERFILE" 'COPY --chmod=0644 oci/claude-system-prompt.txt /usr/local/share/snowcat-cockpit/claude-system-prompt.txt'
assert_contains "$CLAUDE_CONTAINERFILE" 'CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1'

printf 'PASS: OCI entrypoints\n'
