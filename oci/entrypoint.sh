#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
  printf 'cockpit-entrypoint: expected one bounded worker prompt and one pinned model\n' >&2
  exit 64
fi

ulimit -c 0

umask 077
install -d -m 0700 "$CODEX_HOME" "$GH_CONFIG_DIR"
install -m 0600 /run/cockpit/input/codex/auth.json "$CODEX_HOME/auth.json"
install -m 0600 /run/cockpit/input/codex/config.toml "$CODEX_HOME/config.toml"
install -m 0600 /run/cockpit/input/gh/hosts.yml "$GH_CONFIG_DIR/hosts.yml"
install -m 0600 /run/cockpit/input/gh/config.yml "$GH_CONFIG_DIR/config.yml"

gh auth setup-git >/dev/null
git config --global --add safe.directory /workspace

# Credentials are already fixed at 0600; repository tooling expects ordinary
# non-secret files to use the conventional process default.
umask 022

exec codex exec \
  --dangerously-bypass-approvals-and-sandbox \
  --model "$2" \
  --cd /workspace \
  "$1"
