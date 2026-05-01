#!/usr/bin/env bash
# Install the workbuddy binary from GitHub Releases.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/Lincyaw/workbuddy/main/install.sh | bash
#
# Options (env vars):
#   WORKBUDDY_VERSION   - version to install (default: latest)
#   INSTALL_DIR         - binary install path (default: ~/.local/bin)
#   GITHUB_TOKEN        - GitHub token for API calls (avoids rate limits)
#
# For the Claude Code plugin, use the native marketplace instead:
#   /plugin marketplace add Lincyaw/workbuddy
#   /plugin install workbuddy

set -euo pipefail

REPO="Lincyaw/workbuddy"
VERSION="${WORKBUDDY_VERSION:-latest}"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
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

# --- main ------------------------------------------------------------------

main() {
  log "workbuddy installer"
  log "==================="
  echo

  need curl
  need tar

  setup_auth
  resolve_version
  log "Version: v${VERSION}"

  local os arch archive url dl_dir
  os=$(detect_os)
  arch=$(detect_arch)
  archive="workbuddy_${VERSION}_${os}_${arch}.tar.gz"
  url="https://github.com/${REPO}/releases/download/v${VERSION}/${archive}"
  dl_dir="${TMPDIR_ROOT}/dl"
  mkdir -p "$dl_dir"

  log "Downloading workbuddy v${VERSION} (${os}/${arch})..."

  curl -fsSL "${auth_header[@]}" "$url" -o "${dl_dir}/${archive}" \
    || die "Download failed. Check that v${VERSION} exists at ${url}"

  tar -xzf "${dl_dir}/${archive}" -C "$dl_dir"

  # Find the binary inside the extracted tree (handles both flat and
  # nested tar layouts produced by different release pipelines).
  local binary_path
  binary_path=$(find "$dl_dir" -name "workbuddy" -type f | head -n1)
  [ -n "$binary_path" ] || die "Archive did not contain a 'workbuddy' binary"

  mkdir -p "$INSTALL_DIR"
  mv "$binary_path" "${INSTALL_DIR}/workbuddy"
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

  echo
  log "Done! Run 'workbuddy version' to verify."
  log "For the Claude Code plugin: /plugin marketplace add Lincyaw/workbuddy"
}

main
