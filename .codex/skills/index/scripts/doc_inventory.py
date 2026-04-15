#!/usr/bin/env python3
import argparse
import json
import re
from pathlib import Path

try:
    import yaml
except Exception as exc:
    raise SystemExit(f"PyYAML is required: {exc}")

PATH_RE = re.compile(r"`([^`]+)`")
PATH_HINTS = (
    "docs/",
    "cmd/",
    "internal/",
    ".github/",
    "project-index.yaml",
)


def load_index(path: Path):
    if not path.exists():
        return {}
    data = yaml.safe_load(path.read_text(encoding="utf-8")) or {}
    return data.get("documentation", {}) or {}


def list_docs(root: Path):
    docs_dir = root / "docs"
    if not docs_dir.exists():
        return []
    return sorted(str(p.relative_to(root)) for p in docs_dir.rglob("*.md"))


def extract_path_refs(path: Path, root: Path):
    refs = []
    text = path.read_text(encoding="utf-8")
    for match in PATH_RE.findall(text):
        if any(h in match for h in PATH_HINTS):
            cleaned = match.split(":", 1)[0].split("#", 1)[0]
            refs.append(cleaned)
    uniq = sorted(set(refs))
    existing = [r for r in uniq if (root / r).exists()]
    missing = [r for r in uniq if not (root / r).exists()]
    return {"all": uniq, "existing": existing, "missing": missing}


def main():
    parser = argparse.ArgumentParser(description="Inventory docs, index entries, and stale references.")
    parser.add_argument("--root", default=".", help="Repository root")
    args = parser.parse_args()

    root = Path(args.root).resolve()
    docs = list_docs(root)
    index = load_index(root / "project-index.yaml")
    indexed_docs = []
    for item in index.get("documents", []) or []:
        path = item.get("path")
        if isinstance(path, str):
            indexed_docs.append(path)

    report = {
        "root": str(root),
        "docs": docs,
        "indexed_docs": sorted(indexed_docs),
        "unindexed_docs": sorted(set(docs) - set(indexed_docs)),
        "indexed_missing_on_disk": sorted(p for p in indexed_docs if not (root / p).exists()),
        "doc_refs": {},
    }

    for rel in docs:
        report["doc_refs"][rel] = extract_path_refs(root / rel, root)

    print(json.dumps(report, indent=2, ensure_ascii=True))


if __name__ == "__main__":
    main()
