---
name: bugfix
description: Bug fix lifecycle - faster path; reviewer runs tests
trigger:
  issue_label: "type:bug"
max_retries: 3
---

## Bug Fix Workflow

Streamlined lifecycle for bug reports. Skips triage, starts directly in developing.
Agents decide transitions by modifying labels.

```yaml
states:
  developing:
    enter_label: "status:developing"
    agent: codex-dev-agent
    transitions:
      - to: reviewing
        when: labeled "status:reviewing"

  reviewing:
    enter_label: "status:reviewing"
    agent: codex-review-agent
    transitions:
      - to: done
        when: labeled "status:done"
      - to: developing
        when: labeled "status:developing"

  done:
    enter_label: "status:done"
    action: close_issue

  failed:
    enter_label: "status:failed"
```

`failed` 仍然是 workflow schema 中可识别的终态 label，但当前 Go runtime 不会在 retry 超限时直接写入
`status:failed` 或 `needs-human`；它只会 record retry/failure intent，后续 label 写回仍由 agent 或人工执行。
