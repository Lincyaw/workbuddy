#!/usr/bin/env python3
"""Build the repo-local Codex plugin from the Claude plugin source tree."""

from __future__ import annotations

import json
import shutil
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SOURCE = ROOT / ".claude" / "plugins" / "workbuddy"
DEST = ROOT / "plugins" / "workbuddy"
MARKETPLACE = ROOT / ".agents" / "plugins" / "marketplace.json"

SKILL_HEADER_DROP_KEYS = {"user_invocable"}

PLUGIN_MANIFEST = {
    "name": "workbuddy",
    "version": "0.1.0",
    "description": "Operate workbuddy from Codex with repo setup, pipeline monitoring, troubleshooting, and operator workflows.",
    "author": {
        "name": "Lincyaw",
        "email": "boxiyu888@proton.me",
        "url": "https://github.com/Lincyaw",
    },
    "homepage": "https://github.com/Lincyaw/workbuddy",
    "repository": "https://github.com/Lincyaw/workbuddy",
    "license": "MIT",
    "keywords": [
        "workbuddy",
        "codex",
        "agent-orchestration",
        "github-issues",
        "devops",
    ],
    "skills": "./skills/",
    "interface": {
        "displayName": "Workbuddy",
        "shortDescription": "Operate the workbuddy issue-driven agent pipeline from Codex.",
        "longDescription": "Installable Codex plugin for workbuddy. Includes guided repo setup, pipeline monitoring, troubleshooting, and operator-facing workflows adapted from the Claude plugin layout.",
        "developerName": "Lincyaw",
        "category": "Productivity",
        "capabilities": ["Read", "Write"],
        "websiteURL": "https://github.com/Lincyaw/workbuddy",
        "defaultPrompt": [
            "Use setup-repo to onboard owner/repo for workbuddy.",
            "Use pipeline-monitor to diagnose a stuck workbuddy issue.",
            "Use workbuddy-guide to explain serve and coordinator-worker modes.",
        ],
        "brandColor": "#0F766E",
    },
}

MARKETPLACE_MANIFEST = {
    "name": "workbuddy-local",
    "interface": {"displayName": "Workbuddy Plugins"},
    "plugins": [
        {
            "name": "workbuddy",
            "source": {"source": "local", "path": "./plugins/workbuddy"},
            "policy": {
                "installation": "AVAILABLE",
                "authentication": "ON_INSTALL",
            },
            "category": "Productivity",
        }
    ],
}

README = """# Workbuddy Plugin for Codex

Codex plugin for operating [workbuddy](https://github.com/Lincyaw/workbuddy) — a GitHub Issue-driven agent orchestration platform.

## Included skills

| Skill | Purpose |
| --- | --- |
| `workbuddy-guide` | Explain deployment modes, operating model, and troubleshooting flow |
| `setup-repo` | Configure a repository for workbuddy orchestration |
| `pipeline-monitor` | Inspect stuck or unhealthy agent execution pipelines |
| `merge-flow` | Merge a batch of approved workbuddy PRs with design-intent checks |

## Source of truth

This Codex plugin is generated from the Claude plugin content in `.claude/plugins/workbuddy/`.
Run `python3 scripts/sync_codex_plugin.py` after updating the Claude plugin files.
"""


def ensure_clean_dest() -> None:
    if DEST.exists():
        shutil.rmtree(DEST)
    DEST.mkdir(parents=True, exist_ok=True)
    (DEST / ".codex-plugin").mkdir(parents=True, exist_ok=True)
    (DEST / "skills").mkdir(parents=True, exist_ok=True)


def strip_frontmatter_keys(text: str) -> str:
    if not text.startswith("---\n"):
        return text
    parts = text.split("---\n", 2)
    if len(parts) < 3:
        return text
    header, body = parts[1], parts[2]
    kept = []
    for line in header.splitlines():
        if not line.strip():
            kept.append(line)
            continue
        key = line.split(":", 1)[0].strip()
        if key in SKILL_HEADER_DROP_KEYS:
            continue
        kept.append(line)
    return "---\n" + "\n".join(kept).rstrip() + "\n---\n" + body


def normalize_setup_repo_skill(text: str) -> str:
    return text.replace(
        "Runtime (`claude-code` | `codex`) is a field on the agent config, not a\nseparate agent.",
        "Runtime (`claude-code` | `codex`) is a field on the agent config, not a\nseparate agent. This Codex plugin distributes the guidance as skills; the Claude\nplugin keeps the matching Claude-specific packaging.",
    )


def normalize_workbuddy_guide_skill(text: str) -> str:
    return text.replace(
        "Only two agents exist: `dev-agent` and `review-agent`. Runtime (`claude-code`\nor `codex`) is a config field, not a separate agent.",
        "Only two agents exist: `dev-agent` and `review-agent`. Runtime (`claude-code`\nor `codex`) is a config field, not a separate agent. This Codex plugin exposes\nthat operational guidance through installable skills.",
    )


def copy_skills() -> None:
    transforms = {
        "setup-repo": normalize_setup_repo_skill,
        "workbuddy-guide": normalize_workbuddy_guide_skill,
    }
    for src in sorted((SOURCE / "skills").glob("*/**")):
        if src.is_dir():
            continue
        rel = src.relative_to(SOURCE / "skills")
        dst = DEST / "skills" / rel
        dst.parent.mkdir(parents=True, exist_ok=True)
        text = src.read_text(encoding="utf-8")
        if src.name == "SKILL.md":
            text = strip_frontmatter_keys(text)
            skill_name = src.parent.name
            if skill_name in transforms:
                text = transforms[skill_name](text)
        dst.write_text(text, encoding="utf-8")


def write_json(path: Path, payload: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(payload, indent=2) + "\n", encoding="utf-8")


def main() -> None:
    if not SOURCE.exists():
        raise SystemExit(f"source plugin not found: {SOURCE}")

    ensure_clean_dest()
    copy_skills()
    write_json(DEST / ".codex-plugin" / "plugin.json", PLUGIN_MANIFEST)
    (DEST / "README.md").write_text(README, encoding="utf-8")
    write_json(MARKETPLACE, MARKETPLACE_MANIFEST)


if __name__ == "__main__":
    main()
