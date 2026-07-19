#!/usr/bin/env bash
set -euo pipefail

OVTEST_USER="${OVTEST_USER:-ovtest}"
OVTEST_HOME="${OVTEST_HOME:-/var/lib/ovtest/home}"
OVTEST_ROOT="${OVTEST_ROOT:-/var/lib/ovtest/release-gate}"
OVTEST_ENV_FILE="${OVTEST_ENV_FILE:-/etc/ovtest/release.env}"

die() {
  echo "setup-ovtest-vps: $*" >&2
  exit 1
}

usage() {
  cat <<EOF
Usage:
  sudo $0 provision
  sudo -H -u $OVTEST_USER $0 auth [all|codex|claude|opencode]
  sudo -H -u $OVTEST_USER $0 check

Overrides: OVTEST_USER, OVTEST_HOME, OVTEST_ROOT, OVTEST_ENV_FILE
EOF
}

require_root() {
  [[ "$(id -u)" -eq 0 ]] || die "$1 must run as root"
}

require_service_user() {
  [[ "$(id -un)" == "$OVTEST_USER" ]] || die "$1 must run as $OVTEST_USER with HOME=$OVTEST_HOME"
  [[ "$HOME" == "$OVTEST_HOME" ]] || die "HOME is $HOME; expected $OVTEST_HOME (use sudo -H -u $OVTEST_USER)"
}

install_base_tools() {
  command -v apt-get >/dev/null || die "provision currently supports Debian/Ubuntu VPS images"
  export DEBIAN_FRONTEND=noninteractive
  apt-get update
  apt-get install -y ca-certificates curl git jq build-essential

  if ! command -v node >/dev/null || [[ "$(node -p 'Number(process.versions.node.split(".")[0])')" -lt 22 ]]; then
    local installer
    installer="$(mktemp)"
    curl -fsSL https://deb.nodesource.com/setup_22.x -o "$installer"
    bash "$installer"
    rm -f "$installer"
    apt-get install -y nodejs
  fi

  if ! command -v uv >/dev/null; then
    local installer
    installer="$(mktemp)"
    curl -LsSf https://astral.sh/uv/install.sh -o "$installer"
    env UV_INSTALL_DIR=/usr/local/bin sh "$installer"
    rm -f "$installer"
  fi
}

create_env_file() {
  [[ ! -e "$OVTEST_ENV_FILE" ]] || return 0
  local group template
  group="$(id -gn "$OVTEST_USER")"
  template="$(mktemp)"
  cat >"$template" <<'EOF'
# OpenClaw, Hermes and Pi share this LLM key.
OV_TEST_HARNESS_LLM_API_KEY=REPLACE_WITH_SHARED_HARNESS_LLM_KEY

# OpenViking uses a separate provider key. Put the same key on both lines.
OPENVIKING_LLM_API_KEY=REPLACE_WITH_OPENVIKING_PROVIDER_KEY
OPENVIKING_LLM_BASE_URL=REPLACE_WITH_LLM_API_ROOT
OPENVIKING_LLM_MODEL=REPLACE_WITH_LLM_MODEL
OPENVIKING_EMBEDDING_API_KEY=REPLACE_WITH_OPENVIKING_PROVIDER_KEY
OPENVIKING_EMBEDDING_BASE_URL=REPLACE_WITH_EMBEDDING_API_ROOT
OPENVIKING_EMBEDDING_MODEL=REPLACE_WITH_EMBEDDING_MODEL

# External OpenViking target only:
# OV_TEST_OPENVIKING_API_KEY=REPLACE_WITH_OPENVIKING_USER_KEY
EOF
  install -o "$OVTEST_USER" -g "$group" -m 0600 "$template" "$OVTEST_ENV_FILE"
  rm -f "$template"
}

provision() {
  require_root provision
  install_base_tools
  if ! id "$OVTEST_USER" >/dev/null 2>&1; then
    useradd --system --user-group --create-home --home-dir "$OVTEST_HOME" --shell /bin/bash "$OVTEST_USER"
  fi
  local group
  group="$(id -gn "$OVTEST_USER")"
  install -d -o "$OVTEST_USER" -g "$group" -m 0700 "$OVTEST_HOME" "$OVTEST_ROOT"
  install -d -o root -g "$group" -m 0750 "$(dirname "$OVTEST_ENV_FILE")"
  create_env_file
  echo "Provisioned. Replace placeholders in $OVTEST_ENV_FILE, then run auth and check."
}

npm_cli() {
  local package="$1"
  shift
  npm exec --yes --package="$package@latest" -- "$@"
}

auth_one() {
  case "$1" in
    codex)
      npm_cli @openai/codex codex login --device-auth
      npm_cli @openai/codex codex login status
      ;;
    claude)
      npm_cli @anthropic-ai/claude-code claude auth login
      npm_cli @anthropic-ai/claude-code claude auth status
      ;;
    opencode)
      npm_cli opencode-ai opencode auth login
      npm_cli opencode-ai opencode auth list
      ;;
    *) die "unknown auth target: $1" ;;
  esac
}

auth() {
  require_service_user auth
  case "${1:-all}" in
    all)
      auth_one codex
      auth_one claude
      auth_one opencode
      ;;
    codex|claude|opencode) auth_one "$1" ;;
    *) die "auth target must be all, codex, claude, or opencode" ;;
  esac
}

check() {
  require_service_user check
  local failed=0 key mode value
  for command in git uv node npm; do
    if ! command -v "$command" >/dev/null; then
      echo "missing: $command" >&2
      failed=1
    fi
  done
  if [[ ! -r "$OVTEST_ENV_FILE" ]]; then
    echo "missing or unreadable: $OVTEST_ENV_FILE" >&2
    failed=1
  else
    for key in OV_TEST_HARNESS_LLM_API_KEY OPENVIKING_LLM_API_KEY OPENVIKING_LLM_BASE_URL OPENVIKING_LLM_MODEL OPENVIKING_EMBEDDING_API_KEY OPENVIKING_EMBEDDING_BASE_URL OPENVIKING_EMBEDDING_MODEL; do
      value="$(sed -n "s/^${key}=//p" "$OVTEST_ENV_FILE" | tail -n 1)"
      if [[ -z "$value" || "$value" == REPLACE_WITH_* ]]; then
        echo "missing value: $key" >&2
        failed=1
      fi
    done
    mode="$(stat -c '%a' "$OVTEST_ENV_FILE" 2>/dev/null || stat -f '%Lp' "$OVTEST_ENV_FILE")"
    [[ "$mode" == "600" ]] || { echo "$OVTEST_ENV_FILE must have mode 600" >&2; failed=1; }
  fi
  if command -v node >/dev/null && [[ "$(node -p 'Number(process.versions.node.split(".")[0])')" -lt 22 ]]; then
    echo "Node.js 22 or newer is required" >&2
    failed=1
  fi
  [[ -s "$HOME/.codex/auth.json" ]] || { echo "missing Codex login" >&2; failed=1; }
  [[ -s "$HOME/.claude/.credentials.json" || -s "$HOME/.claude.json" ]] || { echo "missing Claude Code login" >&2; failed=1; }
  [[ -s "$HOME/.local/share/opencode/auth.json" ]] || { echo "missing OpenCode login" >&2; failed=1; }
  [[ "$failed" -eq 0 ]] || exit 1
  echo "VPS prerequisites, credential file and machine logins are present."
  echo "ovtest release preflight will validate live authentication and model access."
}

case "${1:-}" in
  provision) provision ;;
  auth) shift; auth "${1:-all}" ;;
  check) check ;;
  -h|--help|help) usage ;;
  *) usage >&2; exit 2 ;;
esac
