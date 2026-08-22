#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
  printf 'cockpit-entrypoint: expected one bounded worker prompt and one model selector\n' >&2
  exit 64
fi

umask 077
install -d -m 0700 "$COPILOT_HOME" "$GH_CONFIG_DIR"
install -m 0600 /run/cockpit/input/copilot/mcp-config.json "$COPILOT_HOME/mcp-config.json"
install -m 0600 /run/cockpit/input/gh/hosts.yml "$GH_CONFIG_DIR/hosts.yml"
install -m 0600 /run/cockpit/input/gh/config.yml "$GH_CONFIG_DIR/config.yml"

gh auth setup-git >/dev/null
git config --global --add safe.directory /workspace

# Credentials are already fixed at 0600; repository tooling expects ordinary
# non-secret files to use the conventional process default.
umask 022

exec copilot \
  --prompt "$1" \
  --model "$2" \
  -C /workspace \
  --allow-all \
  --no-ask-user \
  --no-remote \
  --no-remote-export \
  --no-auto-update \
  --disable-builtin-mcps \
  --output-format text \
  --no-color \
  --log-level none
