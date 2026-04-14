---
name: feature-dev
description: Full feature development lifecycle
trigger:
  issue_label: "type:feature"
max_retries: 3
---

## Feature Development Workflow

Complete lifecycle for feature issues. Each agent is a node in the graph;
agents decide the next state by modifying issue labels via `gh issue edit`.

The state machine only reacts to label changes — it doesn't care whether a
human or an agent changed the label.

```yaml
states:
  triage:
    enter_label: "status:triage"
    transitions:
      - to: developing
        when: labeled "status:developing"

  developing:
    enter_label: "status:developing"
    agent: dev-agent
    transitions:
      - to: testing
        when: labeled "status:testing"

  testing:
    enter_label: "status:testing"
    agent: test-agent
    transitions:
      - to: reviewing
        when: labeled "status:reviewing"
      - to: developing
        when: labeled "status:developing"

  reviewing:
    enter_label: "status:reviewing"
    agent: review-agent
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

### State graph

```
triage → developing ⇄ testing ⇄ reviewing → done
              ↑_________________________↓
              (back-edges: retry count tracked, max 3)

Any back-edge exceeding max_retries → failed → needs-human
```
