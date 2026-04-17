#!/usr/bin/env bash
# Install workbuddy binary and Claude Code plugin.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/Lincyaw/workbuddy/main/install.sh | bash
#
# Options (env vars):
#   WORKBUDDY_VERSION   - version to install (default: latest)
#   INSTALL_DIR         - binary install path (default: ~/.local/bin)
#   SKIP_PLUGIN         - set to 1 to skip Claude Code plugin install
#   SKIP_BINARY         - set to 1 to skip binary install (plugin only)
#   GITHUB_TOKEN        - GitHub token for API calls (avoids rate limits)

set -euo pipefail

REPO="Lincyaw/workbuddy"
VERSION="${WORKBUDDY_VERSION:-latest}"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
SKIP_PLUGIN="${SKIP_PLUGIN:-0}"
SKIP_BINARY="${SKIP_BINARY:-0}"
TMPDIR_ROOT=$(mktemp -d)
trap 'rm -rf "$TMPDIR_ROOT"' EXIT

# --- helpers ---------------------------------------------------------------

log()  { printf '\033[1;32m%s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m%s\033[0m\n' "$*" >&2; }
die()  { printf '\033[1;31mError: %s\033[0m\n' "$*" >&2; exit 1; }

# Build auth header array for curl. Tries GITHUB_TOKEN env, then gh CLI token.
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

need() {
  command -v "$1" >/dev/null 2>&1 || die "'$1' is required but not found"
}

detect_os() {
  case "$(uname -s)" in
    Linux*)  echo "linux" ;;
    Darwin*) echo "darwin" ;;
    *)       die "Unsupported OS: $(uname -s)" ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)  echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    *)             die "Unsupported architecture: $(uname -m)" ;;
  esac
}

resolve_version() {
  if [ "$VERSION" = "latest" ]; then
    need curl
    VERSION=$(curl -fsSL "${auth_header[@]}" "https://api.github.com/repos/${REPO}/releases/latest" \
      | grep '"tag_name"' | head -1 | sed 's/.*"v\(.*\)".*/\1/')
    [ -n "$VERSION" ] || die "Could not determine latest version"
  else
    VERSION="${VERSION#v}"
  fi
}

# --- binary install --------------------------------------------------------

install_binary() {
  if [ "$SKIP_BINARY" = "1" ]; then
    log "Skipping binary install (SKIP_BINARY=1)"
    return
  fi

  need curl
  need tar

  local os arch archive url dl_dir
  os=$(detect_os)
  arch=$(detect_arch)
  archive="workbuddy_${VERSION}_${os}_${arch}.tar.gz"
  url="https://github.com/${REPO}/releases/download/v${VERSION}/${archive}"
  dl_dir="${TMPDIR_ROOT}/binary"
  mkdir -p "$dl_dir"

  log "Downloading workbuddy v${VERSION} (${os}/${arch})..."

  curl -fsSL "${auth_header[@]}" "$url" -o "${dl_dir}/${archive}" \
    || die "Download failed. Check that v${VERSION} exists at ${url}"

  tar -xzf "${dl_dir}/${archive}" -C "$dl_dir"

  mkdir -p "$INSTALL_DIR"
  mv "${dl_dir}/workbuddy" "${INSTALL_DIR}/workbuddy"
  chmod +x "${INSTALL_DIR}/workbuddy"

  log "Installed workbuddy to ${INSTALL_DIR}/workbuddy"

  # Check if INSTALL_DIR is in PATH
  case ":$PATH:" in
    *":${INSTALL_DIR}:"*) ;;
    *)
      warn "Note: ${INSTALL_DIR} is not in your PATH."
      warn "Add it with:  export PATH=\"${INSTALL_DIR}:\$PATH\""
      ;;
  esac
}

# --- plugin install --------------------------------------------------------

install_plugin() {
  if [ "$SKIP_PLUGIN" = "1" ]; then
    log "Skipping plugin install (SKIP_PLUGIN=1)"
    return
  fi

  need git

  local plugin_dir="$HOME/.claude/plugins/workbuddy"
  local clone_dir="${TMPDIR_ROOT}/plugin"

  log "Installing Claude Code plugin..."

  git clone --depth 1 --filter=blob:none --sparse \
    "https://github.com/${REPO}.git" "$clone_dir" 2>/dev/null

  (
    cd "$clone_dir"
    git sparse-checkout set .claude/plugins/workbuddy 2>/dev/null
  )

  # Copy plugin files
  if [ -d "$clone_dir/.claude/plugins/workbuddy" ]; then
    mkdir -p "$plugin_dir"
    rm -rf "${plugin_dir:?}/"*
    cp -R "$clone_dir/.claude/plugins/workbuddy/." "$plugin_dir/"
    log "Installed Claude Code plugin to ${plugin_dir}"
  else
    warn "Plugin files not found in repo, skipping plugin install"
    return
  fi

  # Write marketplace entry for local discovery
  local marketplace_dir="$HOME/.claude/plugins/workbuddy/.claude-plugin"
  mkdir -p "$marketplace_dir"
  cat > "$marketplace_dir/marketplace.json" <<'MKJSON'
{
  "name": "workbuddy",
  "owner": { "name": "lincyaw" },
  "plugins": [
    {
      "name": "workbuddy",
      "source": "./",
      "description": "Operate workbuddy — GitHub Issue-driven agent orchestration platform. Covers repo setup, deployment, issue creation, pipeline monitoring, and troubleshooting."
    }
  ]
}
MKJSON

  log "Plugin marketplace entry written"
}

# --- main ------------------------------------------------------------------

main() {
  log "workbuddy installer"
  log "==================="
  echo

  setup_auth
  resolve_version
  log "Version: v${VERSION}"
  echo

  install_binary
  echo

  install_plugin
  echo

  log "Done! Run 'workbuddy version' to verify."
}

main
