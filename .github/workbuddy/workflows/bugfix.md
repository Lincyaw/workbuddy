---
name: bugfix
description: Bug fix lifecycle - faster path with mandatory testing
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
      - to: testing
        when: labeled "status:testing"

  testing:
    enter_label: "status:testing"
    agent: codex-test-agent
    transitions:
      - to: reviewing
        when: labeled "status:reviewing"
      - to: developing
        when: labeled "status:developing"

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
    action: add_label "needs-human"
```
