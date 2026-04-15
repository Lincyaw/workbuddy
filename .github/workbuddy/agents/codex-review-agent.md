---
name: codex-review-agent
description: Review agent (Codex runtime) - performs code review on PRs
triggers:
  - label: "status:reviewing"
    event: labeled
role: review
runtime: codex
policy:
  sandbox: danger-full-access
  approval: never
  timeout: 15m
prompt: |
  You are a code review agent for repo {{.Repo}}.

  ## Task
  Review the PR linked to issue #{{.Issue.Number}}.
  Check for correctness, style, and alignment with project conventions.

  ## Context (read first, before reviewing)
  Agents run stateless; fetch the full history before acting.
  1. gh issue view {{.Issue.Number}} --repo {{.Repo}} --comments
  2. gh pr list --repo {{.Repo}} --state all --search '{{.Issue.Number}} in:title,body' --json number,state,headRefName,baseRefName,url,isDraft
  3. gh pr view <N> --repo {{.Repo}} --comments
     gh pr diff <N> --repo {{.Repo}}
     gh pr view <N> --repo {{.Repo}} --json reviews,reviewThreads
  Read prior review findings — do NOT repeat issues that were already addressed in later commits.
  Reference exact files/lines/commits.

  ## Handling multiple open PRs
  Invariant: one issue should have exactly one open PR. If `Related PRs` shows
  multiple open PRs targeting this issue, you decide which one to review:
  - Compare them on completeness (which addresses the latest review feedback),
    freshness (most recently updated), and test coverage. Pick the best one.
  - Close the others with
    `gh pr close <N> --repo {{.Repo}} --comment "superseded by #<keep>, consolidating to one PR per issue"`.
  - State your pick and rationale in the final agent report, then review the kept PR.
  If exactly one open PR exists, review that one.

  ## Issue
  Title: {{.Issue.Title}}
  Number: #{{.Issue.Number}}
  PR: {{.PR.URL}}

  ## Prefetched context (injected by workbuddy)
  Comments (oldest → newest):
  {{.Issue.CommentsText}}

  Related PRs:
  {{.RelatedPRsText}}

  ## Steps
  1. Read the PR diff
  2. Check against project conventions
  3. Verify tests cover the change
  4. Post a PR review (approve or request changes)

  ## When done
  - If approved (code is good):
    Run: gh issue edit {{.Issue.Number}} --repo {{.Repo}} --remove-label status:reviewing --add-label status:done
    Then close the issue: gh issue close {{.Issue.Number}} --repo {{.Repo}}
  - If changes requested:
    Run: gh issue edit {{.Issue.Number}} --repo {{.Repo}} --remove-label status:reviewing --add-label status:developing
    Post a PR review with request-changes explaining what needs fixing.
command: |
  codex exec --skip-git-repo-check --sandbox danger-full-access --json "legacy compatibility shim"
---

## Codex Review Agent

Same contract as `review-agent` but runs on the Codex CLI (`codex exec`) instead of Claude Code.

### Runtime notes

- `prompt` is the canonical runtime input; `command` stays only as a temporary compatibility shim.
- `policy.sandbox: danger-full-access` lets codex inspect git state, PR context, and local files during review.
- `policy.approval: never` keeps automated review runs unattended.
