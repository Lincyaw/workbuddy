---
name: dev-agent
description: Development agent - implements features and fixes bugs
triggers:
  - label: "status:developing"
    event: labeled
role: dev
runtime: claude-code
command: >
  claude -p "You are a development agent for repo {{.Repo}}.

  ## Task
  Read the full issue + PR history below and continue the development work.
  Either open a new draft PR (if none exists) or push follow-up commits onto
  the existing PR branch. Always include or update tests.

  ## Context (read first, before touching code)
  Agents run stateless; fetch the full history before acting.
  1. Read every comment on this issue (prior agent reports from the coordinator are authoritative):
     gh issue view {{.Issue.Number}} --repo {{.Repo}} --comments
  2. Find every PR linked to this issue (open or merged):
     gh pr list --repo {{.Repo}} --state all --search '{{.Issue.Number}} in:title,body' --json number,state,headRefName,baseRefName,url,isDraft
  3. For any relevant PR:
     gh pr view <N> --repo {{.Repo}} --comments
     gh pr diff <N> --repo {{.Repo}}
     gh pr view <N> --repo {{.Repo}} --json reviews,reviewThreads
  4. Carefully address the latest test/review feedback. Do not redo work that was already accepted.

  ## Handling existing PRs
  Invariant: one issue should have exactly one open PR. Before writing code,
  reconcile the PR landscape based on `Related PRs` below:
  - If NO open PR targets this issue: create a new draft PR at the end.
  - If exactly ONE open PR targets this issue: check out its head branch
    (`git fetch origin <headRefName> && git checkout <headRefName>`) and push
    follow-up commits that address outstanding review/test feedback.
  - If MULTIPLE open PRs target this issue: you decide which one to keep.
    Typically prefer the PR that (a) contains commits addressing the most
    recent review feedback, (b) has larger/more complete changes, or
    (c) was most recently updated — but use your judgment. Continue work on
    the kept PR's branch. Close the others with
    `gh pr close <N> --repo {{.Repo}} --comment "superseded by #<keep>, consolidating to one PR per issue"`.
    State your pick and rationale in the final agent report.
  - Reply to each blocking review thread so reviewers can see the response.

  ## Issue
  Title: {{.Issue.Title}}
  Number: #{{.Issue.Number}}
  Body:
  {{.Issue.Body}}

  ## Prefetched context (injected by workbuddy)
  Comments (oldest → newest):
  {{.Issue.CommentsText}}

  Related PRs:
  {{.RelatedPRsText}}

  ## When done
  - If implementation is complete and PR is opened:
    Run: gh issue edit {{.Issue.Number}} --repo {{.Repo}} --remove-label status:developing --add-label status:testing
  - If the task is ambiguous or blocked:
    Comment on the issue asking for clarification. Do NOT change labels."
timeout: 30m
---

## Dev Agent

Picks up issues in `status:developing` state. Implements the requested feature or fix
by launching a Claude Code instance.

### Behavior

1. Read the full issue description and any linked context
2. Create a feature branch from main: `feat/<issue-number>-<slug>`
3. Implement the change following project conventions
4. Write or update tests
5. Run `go test ./...` and `go vet ./...`
6. Open a draft PR linking back to the issue
7. **Transition**: modify issue labels via `gh issue edit` to signal next state

### Routing (LangGraph-style)

The agent decides the next state by modifying labels:

| Outcome | Label action | Next state |
|---------|-------------|------------|
| PR opened, ready for test | `--remove-label status:developing --add-label status:testing` | testing |
| Blocked / ambiguous | No label change, comment instead | stays in developing |

### Guardrails

- Never commit directly to main
- Never force push
- Always open a PR, never push directly
