#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
  printf 'cockpit-entrypoint: expected one bounded worker prompt and one pinned model\n' >&2
  exit 64
fi

umask 077
install -d -m 0700 "$CLAUDE_CONFIG_DIR" "$GH_CONFIG_DIR"
install -m 0600 /run/cockpit/input/claude/.credentials.json "$CLAUDE_CONFIG_DIR/.credentials.json"
install -m 0600 /run/cockpit/input/gh/hosts.yml "$GH_CONFIG_DIR/hosts.yml"
install -m 0600 /run/cockpit/input/gh/config.yml "$GH_CONFIG_DIR/config.yml"

gh auth setup-git >/dev/null
git config --global --add safe.directory /workspace

# Credentials are already fixed at 0600; repository tooling expects ordinary
# non-secret files to use the conventional process default.
umask 022

exec claude \
  --print "$1" \
  --model "$2" \
  --no-session-persistence \
  --output-format text \
  --permission-mode bypassPermissions \
  --dangerously-skip-permissions \
  --mcp-config /usr/local/share/snowcat-cockpit/claude-mcp.json \
  --strict-mcp-config \
  --no-chrome
