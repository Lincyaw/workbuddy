#!/usr/bin/env bash
# Install the workbuddy Codex plugin from GitHub.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/Lincyaw/workbuddy/main/install-codex-plugin.sh | bash
#
# Options (env vars):
#   WORKBUDDY_REPO    - GitHub repo in owner/name form (default: Lincyaw/workbuddy)
#   WORKBUDDY_REF     - Git ref to install from (default: main)
#   PLUGINS_DIR       - plugin install path (default: ~/plugins)
#   MARKETPLACE_PATH  - marketplace JSON path (default: ~/.agents/plugins/marketplace.json)
#   GITHUB_TOKEN      - GitHub token for API calls or archive downloads

set -euo pipefail

REPO="${WORKBUDDY_REPO:-Lincyaw/workbuddy}"
REF="${WORKBUDDY_REF:-main}"
PLUGINS_DIR="${PLUGINS_DIR:-$HOME/plugins}"
MARKETPLACE_PATH="${MARKETPLACE_PATH:-$HOME/.agents/plugins/marketplace.json}"
PLUGIN_NAME="workbuddy"
TMPDIR_ROOT=$(mktemp -d)
trap 'rm -rf "$TMPDIR_ROOT"' EXIT

auth_header=()
setup_auth() {
  if [ -n "${GITHUB_TOKEN:-}" ]; then
    auth_header=(-H "Authorization: token ${GITHUB_TOKEN}")
  elif command -v gh >/dev/null 2>&1; then
    local token
    token=$(gh auth token 2>/dev/null || true)
    if [ -n "$token" ]; then
      auth_header=(-H "Authorization: token ${token}")
    fi
  fi
}

log()  { printf '\033[1;32m%s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m%s\033[0m\n' "$*" >&2; }
die()  { printf '\033[1;31mError: %s\033[0m\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "'$1' is required but not found"; }

main() {
  need curl
  need tar
  need python3
  setup_auth

  local archive_url archive extract_dir plugin_src plugin_dest marketplace_dir
  archive_url="https://github.com/${REPO}/archive/${REF}.tar.gz"
  archive="${TMPDIR_ROOT}/repo.tar.gz"
  extract_dir="${TMPDIR_ROOT}/extract"
  plugin_dest="${PLUGINS_DIR}/${PLUGIN_NAME}"
  marketplace_dir=$(dirname "$MARKETPLACE_PATH")

  log "Installing ${PLUGIN_NAME} Codex plugin from ${REPO}@${REF}"
  curl -fsSL "${auth_header[@]}" "$archive_url" -o "$archive" \
    || die "Download failed. Check that ${REPO}@${REF} exists and is public."

  mkdir -p "$extract_dir"
  tar -xzf "$archive" -C "$extract_dir"

  plugin_src=$(find "$extract_dir" -path "*/plugins/${PLUGIN_NAME}" -type d | head -1)
  [ -n "$plugin_src" ] || die "Plugin bundle plugins/${PLUGIN_NAME} not found in ${REPO}@${REF}"
  [ -f "$plugin_src/.codex-plugin/plugin.json" ] || die "Plugin manifest missing at ${plugin_src}/.codex-plugin/plugin.json"

  mkdir -p "$PLUGINS_DIR" "$marketplace_dir"
  rm -rf "$plugin_dest"
  cp -R "$plugin_src" "$plugin_dest"

  python3 - "$MARKETPLACE_PATH" <<'PY'
import json
import sys
from pathlib import Path

marketplace_path = Path(sys.argv[1]).expanduser()
entry = {
    "name": "workbuddy",
    "source": {
        "source": "local",
        "path": "./plugins/workbuddy",
    },
    "policy": {
        "installation": "AVAILABLE",
        "authentication": "ON_INSTALL",
    },
    "category": "Productivity",
}

if marketplace_path.exists():
    data = json.loads(marketplace_path.read_text(encoding="utf-8"))
    if not isinstance(data, dict):
        raise SystemExit("marketplace.json must contain a JSON object")
else:
    data = {
        "name": "local-marketplace",
        "interface": {"displayName": "Local Plugins"},
        "plugins": [],
    }

data.setdefault("name", "local-marketplace")
interface = data.setdefault("interface", {})
if not isinstance(interface, dict):
    raise SystemExit("marketplace interface must be a JSON object")
interface.setdefault("displayName", "Local Plugins")
plugins = data.setdefault("plugins", [])
if not isinstance(plugins, list):
    raise SystemExit("marketplace plugins must be a JSON array")

updated = False
for idx, plugin in enumerate(plugins):
    if isinstance(plugin, dict) and plugin.get("name") == entry["name"]:
        plugins[idx] = entry
        updated = True
        break
if not updated:
    plugins.append(entry)

marketplace_path.write_text(json.dumps(data, indent=2) + "\n", encoding="utf-8")
PY

  log "Installed plugin to ${plugin_dest}"
  log "Updated marketplace at ${MARKETPLACE_PATH}"
  echo
  warn "Restart Codex to pick up the new plugin."
}

main
