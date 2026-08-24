#!/usr/bin/env bash
set -euo pipefail

readonly CODEX_WORKER_PROFILE_NAME="snowcat-cockpit-worker"

write_codex_worker_profile() {
  local profile_path="$1"
  local direct_server="$2"
  local worker_id="$3"

  if [[ ${#direct_server} -gt 80 || ! "$direct_server" =~ ^[A-Za-z0-9_.-]+$ ]]; then
    printf 'cockpit-entrypoint: invalid direct MCP server name\n' >&2
    return 64
  fi
  if [[ ! "$worker_id" =~ ^worker-[0-9a-f]{16}$ ]]; then
    printf 'cockpit-entrypoint: invalid worker identity\n' >&2
    return 64
  fi

  install -m 0600 /dev/null "$profile_path"
  {
    printf '[mcp_servers."%s"]\n' "$direct_server"
    printf 'enabled = false\n\n'
    printf '[mcp_servers."snowcat-cockpit"]\n'
    printf 'command = "/workspace/.agents/bin/snowcat-cockpit"\n'
    printf 'args = ["worker", "lease-proxy", "--worker", "%s", "--workspace", "/workspace"]\n' "$worker_id"
    printf 'required = true\n'
  } >"$profile_path"
}

main() {
  if [[ $# -ne 4 ]]; then
    printf 'cockpit-entrypoint: expected one bounded worker prompt, one pinned model, one worker identity, and one MCP server name\n' >&2
    exit 64
  fi

  ulimit -c 0

  umask 077
  install -d -m 0700 "$CODEX_HOME" "$GH_CONFIG_DIR"
  install -m 0600 /run/cockpit/input/codex/auth.json "$CODEX_HOME/auth.json"
  install -m 0600 /run/cockpit/input/codex/config.toml "$CODEX_HOME/config.toml"
  install -m 0600 /run/cockpit/input/gh/hosts.yml "$GH_CONFIG_DIR/hosts.yml"
  install -m 0600 /run/cockpit/input/gh/config.yml "$GH_CONFIG_DIR/config.yml"
  write_codex_worker_profile "$CODEX_HOME/$CODEX_WORKER_PROFILE_NAME.config.toml" "$4" "$3"

  gh auth setup-git >/dev/null
  git config --global --add safe.directory /workspace

  # Credentials are already fixed at 0600; repository tooling expects ordinary
  # non-secret files to use the conventional process default.
  umask 022

  exec codex exec \
    --dangerously-bypass-approvals-and-sandbox \
    --model "$2" \
    --cd /workspace \
    --profile "$CODEX_WORKER_PROFILE_NAME" \
    "$1"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
