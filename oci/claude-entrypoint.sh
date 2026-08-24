#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 4 ]]; then
  printf 'cockpit-entrypoint: expected one bounded worker prompt, one pinned model, one worker identity, and one MCP server name\n' >&2
  exit 64
fi

ulimit -c 0

umask 077
install -d -m 0700 "$CLAUDE_CONFIG_DIR" "$CLAUDE_CONFIG_DIR/skills" "$GH_CONFIG_DIR"
install -m 0600 /run/cockpit/input/claude/.credentials.json "$CLAUDE_CONFIG_DIR/.credentials.json"
install -m 0600 /run/cockpit/input/gh/hosts.yml "$GH_CONFIG_DIR/hosts.yml"
install -m 0600 /run/cockpit/input/gh/config.yml "$GH_CONFIG_DIR/config.yml"

for skill in work-snowcat-queue work-snowcat-without-reviews review-snowcat-queue; do
  source_skill="/workspace/.claude/skills/$skill/SKILL.md"
  [[ -f "$source_skill" && ! -L "$source_skill" ]] || {
    printf 'cockpit-entrypoint: locked skill input is unavailable: %s\n' "$skill" >&2
    exit 78
  }
  install -d -m 0700 "$CLAUDE_CONFIG_DIR/skills/$skill"
  install -m 0600 "$source_skill" "$CLAUDE_CONFIG_DIR/skills/$skill/SKILL.md"
done

gh auth setup-git >/dev/null
git config --global --add safe.directory /workspace

# Credentials are already fixed at 0600; repository tooling expects ordinary
# non-secret files to use the conventional process default.
umask 022

relay_config="{\"mcpServers\":{\"snowcat-cockpit\":{\"type\":\"stdio\",\"command\":\"/workspace/.agents/bin/snowcat-cockpit\",\"args\":[\"worker\",\"lease-proxy\",\"--worker\",\"$3\",\"--workspace\",\"/workspace\"]}}}"

exec claude \
  --print "$1" \
  --model "$2" \
  --no-session-persistence \
  --output-format text \
  --permission-mode bypassPermissions \
  --dangerously-skip-permissions \
  --setting-sources user \
  --append-system-prompt-file /usr/local/share/snowcat-cockpit/claude-system-prompt.txt \
  --mcp-config "$relay_config" \
  --strict-mcp-config \
  --no-chrome
