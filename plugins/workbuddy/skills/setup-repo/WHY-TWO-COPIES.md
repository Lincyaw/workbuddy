This repo intentionally keeps two `setup-repo` skill copies with different scopes.

- `.codex/skills/setup-repo/` is the repo-local Codex skill used while developing workbuddy itself.
- `.claude/plugins/workbuddy/skills/setup-repo/` is the canonical Claude plugin skill that ships the full onboarding/operator guide.

They share the same trigger surface (`name: setup-repo`) but diverge in body detail because the Codex copy is optimized for repo-local development workflow, while the Claude plugin copy carries plugin packaging and end-user onboarding context. Keep the trigger name aligned; if the copies can be unified later, remove this note and deduplicate them.
