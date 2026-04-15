---
name: long-horizon
description: Use when the agent should operate autonomously for an extended task and make reasonable decisions without interrupting the user for every choice. Trigger when the user says to just do it, work independently, run for a long time, avoid asking constant questions, or when configuring autonomous sub-workflows. 中文触发：自主执行、别问我、自己做、长时间运行、自动跑、不用问我、自己决定、后台跑。
---

# Long Horizon

Framework for autonomous decision-making during extended work. The core rule is:

`exhaust your own judgment before consuming the user's attention`

Use this skill when progress matters more than constant confirmation and the task can tolerate well-reasoned autonomous decisions.

## The escalation ladder

Work through uncertainty from lowest-cost resolution to highest-cost resolution. Only escalate when the current level does not resolve the issue.

1. **Convention check** -> decide silently when the codebase already answers it
2. **Codebase research** -> decide and log
3. **External research** -> decide and log with reasoning
4. **North-star reasoning** -> decide, log, and flag for later review
5. **Ask the user** -> last resort; batch questions together

## Level 1: Convention check

First ask: does the repository already answer this?

Check:

- project instruction files such as `AGENTS.md`, `CLAUDE.md`, or repo-specific docs
- existing code in the same subsystem
- config such as linters, formatters, CI, and generators
- recent git history if it clarifies an established pattern

If a clear convention exists, follow it without stopping to discuss it.

Typical examples:

- quote style
- test framework choice
- file placement
- naming conventions

## Level 2: Codebase research

If the answer is not obvious, spend a short bounded period reading nearby files, related modules, READMEs, comments, or commit history.

Use this level for questions like:

- how errors are usually handled
- how transactions or side effects are structured
- what endpoint naming pattern is already in use

At this level and above, leave a decision trail the user can review later.

Example log entry:

`Used repository pattern for new data access (L2: consistent with adjacent modules)`

## Level 3: External research

If the repository is not enough, research outside it:

- official docs
- primary library/framework docs
- issue trackers
- source code of dependencies
- comparable open-source implementations

Use this for technical choices where the answer is discoverable but not already encoded in the project.

Example log entry:

`Chose zod over joi (L3: better TypeScript inference, aligns with maintainability)`

## Level 4: North-star reasoning

When multiple options remain viable, use the project's objectives as the tie-breaker.

For each option, evaluate how it affects the project's top goals, such as:

- maintainability
- simplicity
- reliability
- performance
- clarity

Then choose the best trade-off, continue the work, and flag the decision for later review rather than blocking on approval.

Example:

`[flagged] Kept duplication over extracting a base class (L4: simpler and easier to maintain)`

## Level 5: Ask the user

Only ask after exhausting Levels 1-4, and only when the user is genuinely required.

Legitimate reasons include:

- strategic scope or direction changes
- missing access, secrets, or credentials
- ambiguous requirements that research could not clarify
- high-risk irreversible actions
- conflicting signals that materially affect the outcome

When you ask, batch all unresolved questions together and include the reasoning you already did so the user can answer quickly.

## Decision logging

For any Level 2+ decision, keep a lightweight decision log in a repo-visible place such as `decisions.md` unless the project already has a preferred path.

Useful fields:

- date
- decision
- level (`L2`, `L3`, `L4`)
- short reasoning
- whether it was flagged for later review

The log helps with:

- session continuity
- user review
- future calibration of autonomous behavior

## Anti-patterns

Avoid:

- asking too early
- making significant decisions with no trail
- shallow "research" that never compared options
- spending too long on low-level decisions
- interrupting the user repeatedly instead of batching questions

## Autonomy tuning

Different tasks need different autonomy levels:

- **High autonomy**: decide through L4 and only ask for credentials, irreversible actions, or major strategic changes
- **Medium autonomy**: decide through L3, flag L4, ask for L5
- **Low autonomy**: decide through L2, ask earlier on subjective trade-offs

If the project tracks autonomy preferences, respect them. Otherwise default to medium and bias toward progress.

## Integration notes

- `dev-loop` uses this skill for the non-metric decisions that happen during repeated iterations
- `notify` can surface flagged L4 decisions asynchronously instead of blocking execution
- any optional telemetry or correction logging is best-effort and should not interrupt delivery

## Migration notes

This Codex version preserves the decision ladder from the Claude skill but removes Claude-specific frontmatter fields such as `category`. Logging paths and command details should be adapted to the current repository instead of assuming a Claude-specific runtime.
