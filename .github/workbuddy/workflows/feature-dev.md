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
`status:failed` 或 `needs-human`；它只记录 retry/failure intent，后续 label 写回仍由 agent 或人工执行。

### State graph

```
triage → developing ⇄ reviewing → done
              ↑_____________↓
              (back-edges: retry count tracked, max 3)

The reviewing agent runs the test suite (go build / go test / go vet)
itself before approving. A separate test stage is no longer needed.

Any back-edge exceeding max_retries → record retry/failure intent
```
