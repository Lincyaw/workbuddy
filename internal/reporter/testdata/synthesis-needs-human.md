## Managed Follow-up

The synthesize run did not return a valid structured decision, so the coordinator stopped the rollout reduce step instead of auto-picking a PR.

**Why it was blocked**: `malformed_or_missing_synthesis_output`

Recommended next step: add `needs-human`, inspect the candidate PRs manually, and decide whether to pick one, build a synth PR, or rewrite the issue.

---
*workbuddy coordinator | 2026-04-29T12:01:30Z*
