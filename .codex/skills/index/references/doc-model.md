# Documentation Model

This repository uses a three-bucket documentation model.

## Buckets

### implemented

Use for material that already matches the current codebase closely enough to act as the working source of truth.

Typical contents:

- current architecture
- current config or schema behavior
- current runtime behavior
- current operational or storage behavior

### planned

Use for future-state designs that are intentionally ahead of the code.

Typical contents:

- vNext interface design
- migration targets
- new schema proposals
- distributed topology plans not yet implemented

### mismatch

Use for drift between code and docs.

Typical contents:

- stale config samples
- broad design docs that overstate capability
- roadmap structure that no longer matches the repository
- runtime claims not supported by the current implementation

## Rewrite Guidelines

- Split broad docs into smaller topic-based files.
- Do not keep hybrid docs that mix current facts and future design.
- Prefer one topic per file when possible.
- Use `project-index.yaml` as the canonical document-to-code registry.

## Minimum Migration Checklist

1. inventory existing docs
2. inspect actual code paths
3. classify each topic
4. rewrite bucket indexes
5. update `project-index.yaml`
6. remove superseded docs
7. validate references and YAML parsing
