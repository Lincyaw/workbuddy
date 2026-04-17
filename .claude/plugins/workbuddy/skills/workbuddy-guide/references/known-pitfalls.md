# Known Pitfalls and Troubleshooting

Lessons learned from real-world testing of workbuddy distributed and
multi-repo modes. Read this when something doesn't work as expected.

## Pitfall 1: Git remote uses SSH but gh uses HTTPS

**Symptom**: `git push` fails with "Permission denied" even though `gh auth status` shows logged in.

**Cause**: The repo's git remote uses `git@github.com:...` (SSH) but `gh` is authenticated via HTTPS tokens. SSH uses a different key that may map to a different GitHub account.

**Fix**:
```bash
git remote set-url origin https://github.com/OWNER/REPO.git
```

**Prevention**: Check `git remote -v` and `gh auth status` early. If `gh auth` shows `Git operations protocol: https`, the remote must also use HTTPS.

## Pitfall 2: .workbuddy/ not in .gitignore

**Symptom**: Agent commits session files (`.workbuddy/sessions/*.jsonl`, `.workbuddy/worker.db`) into the repo, bloating the git history.

**Cause**: The `.workbuddy/` directory is created at runtime for local state, but it's not gitignored by default in the target repo.

**Fix**: Add `.workbuddy/` to `.gitignore` and clean up any committed artifacts.

**Prevention**: Always add `.workbuddy/` to `.gitignore` as part of repo setup (Step 6 in the onboarding checklist).

## Pitfall 3: Conventional commit hooks block agent commits

**Symptom**: Agent's `git commit` fails because the commit message doesn't match the repo's conventional commit format (e.g., `feat:`, `fix:`, `test:`).

**Cause**: Target repo has lefthook/husky/commitlint configured to enforce conventional commits. The agent may write a commit message like "Add unit tests for issue #37" instead of "test: add unit tests for issue #37".

**Impact**: Non-blocking if the agent retries with `--no-verify`, but not ideal.

**Mitigation**: The agent prompt template doesn't currently enforce conventional commit style. Consider adding a hint to the target repo's `CLAUDE.md`:
```markdown
## Commit conventions
This repo uses conventional commits. Always prefix commit messages with a type:
feat, fix, test, docs, chore, refactor, perf, ci, build, style, revert
```

## Pitfall 4: Acceptance Criteria format mismatch

**Symptom**: Dev-agent sets `status:blocked` claiming "missing acceptance criteria" even though the issue has criteria written in bold (`**Acceptance Criteria:**`) or a different heading level.

**Cause**: The agent prompt looks for `## Acceptance Criteria` (h2 heading). Issues created outside the template may use bold text, h3, or other formatting.

**Impact**: The agent is generally smart enough to find criteria in any format, but edge cases exist.

**Prevention**: Use issue templates that enforce the `## Acceptance Criteria` heading. For existing issues, ensure the section uses a markdown heading (`##`), not just bold text.

## Pitfall 5: `rg` (ripgrep) not installed on worker machine

**Symptom**: Agent commands using `rg` fail silently or exit with error. Codex agents frequently use `rg` for code search.

**Cause**: `rg` is not installed on the machine where the worker runs.

**Fix**: Install ripgrep: `apt install ripgrep` or `brew install ripgrep`.

## Pitfall 6: Worker can't find agent config locally

**Symptom**: Worker logs `agent "dev-agent" not found in local config`.

**Cause**: The worker was started from a directory that doesn't have `.github/workbuddy/agents/`. The coordinator only sends `{task_id, repo, issue_num, agent_name}` — the worker loads the full agent prompt template from its local filesystem.

**Fix**: Always `cd` to the target repo's root before starting the worker:
```bash
cd /path/to/target-repo
/path/to/workbuddy worker --coordinator http://... --token "..." --repo Owner/Repo
```

## Pitfall 7: Package-level vs function-level test coverage

**Symptom**: Review-agent fails the "coverage > X%" criterion even though the specific functions being tested have high coverage.

**Cause**: `go test -cover` reports package-level coverage (all statements in all files), which is much lower than the coverage of the specific code being tested. A test file covering 86% of queue functions still shows 2.4% package coverage if the package has many other files.

**Impact**: This creates false review failures and unnecessary retry cycles.

**Mitigation**: Write acceptance criteria that are precise about what coverage means:
- Bad: "Test coverage > 80% for queue package"
- Better: "Test coverage > 80% for functions in `queue.go` (measured by `go test -coverprofile` + `go tool cover -func`)"

## Pitfall 8: Coordinator heartbeat doesn't extend task lease (fixed in latest)

**Symptom**: Long-running agent tasks (>30s) get their lease expired. On the next poll, the worker re-claims the old task instead of picking up new pending tasks. The `developing → reviewing` transition never completes.

**Cause**: The coordinator's heartbeat handler previously only updated the worker registry heartbeat, not the task's `lease_expires_at` in the database.

**Status**: Fixed. If you're running an older binary, rebuild: `go build -o workbuddy .`

**Diagnosis**: Check the task queue:
```bash
sqlite3 .workbuddy/workbuddy.db \
  "SELECT agent_name, status, lease_expires_at, heartbeat_at FROM task_queue WHERE repo='Owner/Repo';"
```
If `lease_expires_at` is far in the past but `status` is still `running`, the heartbeat wasn't extending the lease.

## Pitfall 9: golangci-lint fails on pre-existing issues

**Symptom**: Agent's commit is blocked by pre-commit hooks running `golangci-lint` that finds errors in files the agent didn't modify.

**Cause**: The target repo has lint errors in existing code. Pre-commit hooks run lint on all staged files (or the entire repo), catching pre-existing issues.

**Impact**: Agent may use `--no-verify` to bypass, which works but isn't ideal.

**Mitigation**: Fix pre-existing lint errors before onboarding the repo, or configure lint hooks to only check changed files.

## Pitfall 10: Multiple workers claiming the same task

**Symptom**: Two workers execute the same agent for the same issue simultaneously.

**Cause**: Task lease expired and a second worker claimed the task while the first was still executing.

**Prevention**: Ensure heartbeats are working (Pitfall 8 fix). If running multiple workers for the same repo, the lease mechanism prevents double-claims as long as heartbeats extend the lease correctly.

## Diagnostic Commands

```bash
# Check task queue state
sqlite3 .workbuddy/workbuddy.db \
  "SELECT id, agent_name, status, worker_id, lease_expires_at FROM task_queue ORDER BY created_at DESC LIMIT 10;"

# Check coordinator health
curl -s http://coordinator:8081/health | python3 -m json.tool

# List registered repos
curl -s -H "Authorization: Bearer $TOKEN" http://coordinator:8081/api/v1/repos | python3 -m json.tool

# Check issue labels
gh issue view <N> -R Owner/Repo --json labels --jq '[.labels[].name]'

# Force re-poll an issue
/path/to/workbuddy cache-invalidate --repo Owner/Repo --issue <N>

# View coordinator logs (if running via workbuddy binary)
# Logs go to stderr, check your process manager or terminal output

# Check codex/claude subprocess
ps aux | grep "codex.*exec" | grep -v grep
```
