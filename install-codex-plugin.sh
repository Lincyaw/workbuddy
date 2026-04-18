#!/usr/bin/env bash
# Install the workbuddy Codex skills from GitHub into the local Codex skills directory.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/Lincyaw/workbuddy/main/install-codex-plugin.sh | bash
#
# Options (env vars):
#   WORKBUDDY_REPO         - GitHub repo in owner/name form (default: Lincyaw/workbuddy)
#   WORKBUDDY_REF          - Git ref to install from (default: main)
#   WORKBUDDY_ARCHIVE_URL  - Override the archive URL (useful for local testing)
#   CODEX_HOME             - Codex home directory (default: ~/.codex)
#   SKILLS_DIR             - Skills install path (default: $CODEX_HOME/skills)
#   WORKBUDDY_KEEP_REMOVED - Set to 1 to keep previously managed skills that no longer exist upstream
#   GITHUB_TOKEN           - GitHub token for archive downloads

set -euo pipefail

REPO="${WORKBUDDY_REPO:-Lincyaw/workbuddy}"
REF="${WORKBUDDY_REF:-main}"
ARCHIVE_URL="${WORKBUDDY_ARCHIVE_URL:-https://github.com/${REPO}/archive/${REF}.tar.gz}"
CODEX_HOME="${CODEX_HOME:-$HOME/.codex}"
SKILLS_DIR="${SKILLS_DIR:-$CODEX_HOME/skills}"
STATE_FILE="${CODEX_HOME}/.workbuddy-installed-skills.json"
KEEP_REMOVED="${WORKBUDDY_KEEP_REMOVED:-0}"
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

  local archive extract_dir repo_root skills_src
  archive="${TMPDIR_ROOT}/repo.tar.gz"
  extract_dir="${TMPDIR_ROOT}/extract"

  log "Syncing ${PLUGIN_NAME} Codex skills from ${REPO}@${REF}"
  curl -fsSL "${auth_header[@]}" "$ARCHIVE_URL" -o "$archive" \
    || die "Download failed. Check that ${REPO}@${REF} exists and is public."

  mkdir -p "$extract_dir"
  tar -xzf "$archive" -C "$extract_dir"

  repo_root=$(find "$extract_dir" -mindepth 1 -maxdepth 1 -type d | head -1)
  [ -n "$repo_root" ] || die "Could not determine extracted repository root for ${REPO}@${REF}"
  skills_src="${repo_root}/plugins/${PLUGIN_NAME}/skills"
  [ -d "$skills_src" ] || die "Skill bundle plugins/${PLUGIN_NAME}/skills not found in ${REPO}@${REF}"

  mkdir -p "$CODEX_HOME" "$SKILLS_DIR"

  python3 - "$skills_src" "$SKILLS_DIR" "$STATE_FILE" "$REPO" "$REF" "$KEEP_REMOVED" <<'PY'
import json
import shutil
import sys
from pathlib import Path

skills_src = Path(sys.argv[1])
skills_dir = Path(sys.argv[2]).expanduser()
state_file = Path(sys.argv[3]).expanduser()
repo = sys.argv[4]
ref = sys.argv[5]
keep_removed = sys.argv[6] == "1"

if not skills_src.is_dir():
    raise SystemExit(f"skills source directory missing: {skills_src}")

current_skills = []
for child in sorted(skills_src.iterdir()):
    if not child.is_dir():
        continue
    skill_file = child / "SKILL.md"
    if not skill_file.is_file():
        continue
    current_skills.append(child.name)

if not current_skills:
    raise SystemExit("no installable skills found in plugin bundle")

previous_skills = []
if state_file.exists():
    data = json.loads(state_file.read_text(encoding="utf-8"))
    if isinstance(data, dict):
        skills = data.get("skills", [])
        if isinstance(skills, list):
            previous_skills = [str(item) for item in skills]

installed = []
for skill_name in current_skills:
    src = skills_src / skill_name
    dest = skills_dir / skill_name
    tmp_dest = skills_dir / f".{skill_name}.tmp-workbuddy"
    if tmp_dest.exists():
        shutil.rmtree(tmp_dest)
    shutil.copytree(src, tmp_dest)
    if dest.exists():
        shutil.rmtree(dest)
    tmp_dest.replace(dest)
    installed.append(skill_name)

removed = []
managed_skills = list(current_skills)
if not keep_removed:
    current_set = set(current_skills)
    for skill_name in previous_skills:
        if skill_name in current_set:
            continue
        dest = skills_dir / skill_name
        if dest.exists():
            shutil.rmtree(dest)
            removed.append(skill_name)
else:
    managed_skills = sorted(set(previous_skills).union(current_skills))

state = {
    "repo": repo,
    "ref": ref,
    "skills": managed_skills,
}
state_file.write_text(json.dumps(state, indent=2) + "\n", encoding="utf-8")

print(json.dumps({
    "installed": installed,
    "removed": removed,
    "state_file": str(state_file),
}, indent=2))
PY

  echo
  warn "Restart Codex to pick up updated skills."
}

main
