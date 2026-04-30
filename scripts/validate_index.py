#!/usr/bin/env python3
"""Inline equivalent of ~/.autoharness/scripts/validate_index.py.

Vendored into the repo so CI runners do not depend on the autoharness
checkout. Validates project-index.yaml requirements:
  - required fields present
  - IDs unique
  - priority / status values valid
  - depends_on references exist
  - referenced code/test paths exist on disk
"""

import json
import sys
from pathlib import Path

try:
    import yaml
except ImportError:
    print(json.dumps({"pass": False, "violations": ["PyYAML is not installed"]}))
    sys.exit(1)

VALID_PRIORITIES = {"P0", "P1", "P2"}
VALID_STATUSES = {"active", "implemented", "tested", "deprecated", "pending", "open"}
REQUIRED_FIELDS = {"id", "title", "description", "priority", "status"}


def validate(index_path: Path, project_root: Path) -> list[str]:
    violations: list[str] = []
    try:
        data = yaml.safe_load(index_path.read_text(encoding="utf-8"))
    except Exception as e:
        return [f"Failed to parse YAML: {e}"]

    if not isinstance(data, dict):
        return ["Root must be a YAML mapping"]

    requirements = data.get("requirements")
    if not isinstance(requirements, list):
        return ["Missing or invalid 'requirements' list"]

    seen_ids: set[str] = set()
    for i, req in enumerate(requirements):
        if not isinstance(req, dict):
            violations.append(f"requirements[{i}] is not a mapping")
            continue
        rid = req.get("id", f"<index {i}>")
        missing = REQUIRED_FIELDS - set(req.keys())
        if missing:
            violations.append(f"{rid}: missing fields {sorted(missing)}")
        if rid in seen_ids:
            violations.append(f"{rid}: duplicate id")
        seen_ids.add(rid)
        prio = req.get("priority")
        if prio is not None and prio not in VALID_PRIORITIES:
            violations.append(f"{rid}: invalid priority {prio!r}")
        status = req.get("status")
        if status is not None and status not in VALID_STATUSES:
            violations.append(f"{rid}: invalid status {status!r}")
        for key in ("code", "tests"):
            paths = req.get(key) or []
            if not isinstance(paths, list):
                violations.append(f"{rid}: {key} must be a list")
                continue
            for p in paths:
                if not isinstance(p, str):
                    continue
                if not (project_root / p).exists():
                    violations.append(f"{rid}: {key} path missing: {p}")

    for req in requirements:
        if not isinstance(req, dict):
            continue
        for dep in req.get("deps", []) or []:
            if dep not in seen_ids:
                violations.append(f"{req.get('id')}: depends_on unknown id {dep}")

    return violations


def main() -> int:
    if len(sys.argv) < 2:
        print("usage: validate_index.py project-index.yaml [project_root]", file=sys.stderr)
        return 2
    index_path = Path(sys.argv[1])
    project_root = Path(sys.argv[2]) if len(sys.argv) > 2 else index_path.parent
    violations = validate(index_path, project_root)
    result = {"pass": not violations, "violations": violations}
    print(json.dumps(result, indent=2))
    return 0 if not violations else 1


if __name__ == "__main__":
    sys.exit(main())
