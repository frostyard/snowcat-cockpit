#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
  printf 'cockpit-entrypoint: expected one bounded worker prompt\n' >&2
  exit 64
fi

umask 077
install -d -m 0700 "$CODEX_HOME" "$GH_CONFIG_DIR"
install -m 0600 /run/cockpit/input/codex/auth.json "$CODEX_HOME/auth.json"
install -m 0600 /run/cockpit/input/codex/config.toml "$CODEX_HOME/config.toml"
install -m 0600 /run/cockpit/input/gh/hosts.yml "$GH_CONFIG_DIR/hosts.yml"
install -m 0600 /run/cockpit/input/gh/config.yml "$GH_CONFIG_DIR/config.yml"

gh auth setup-git >/dev/null
git config --global --add safe.directory /workspace

exec codex exec \
  --dangerously-bypass-approvals-and-sandbox \
  --cd /workspace \
  "$1"
