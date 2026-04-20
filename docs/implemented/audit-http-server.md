# Audit HTTP Server

`workbuddy serve` exposes a read-only audit surface for external tooling on the
same HTTP listener as `/health` and the existing session UI. In v0.2.0 the
server is intended for loopback use; token auth is deferred to `REQ-029`.

## Endpoints

### `GET /events`

Returns SQLite-backed event log rows. Supported query parameters:

- `repo`
- `issue`
- `type`
- `since` (`RFC3339`)

Example:

```http
GET /events?repo=owner/repo&issue=40&type=dispatch&since=2026-04-15T00:00:00Z
```

Example response:

```json
{
  "events": [
    {
      "id": 1,
      "ts": "2026-04-15T15:20:00Z",
      "type": "dispatch",
      "repo": "owner/repo",
      "issue_num": 40,
      "payload": {
        "agent": "dev-agent"
      }
    }
  ],
  "filters": {
    "repo": "owner/repo",
    "issue": 40,
    "type": "dispatch",
    "since": "2026-04-15T00:00:00Z"
  }
}
```

### `GET /issues/:repo/:num/state`

Returns the cached issue state derived from SQLite, including labels, aggregate
transition count, and dependency verdict.

Example:

```http
GET /issues/owner/repo/40/state
```

Example response:

```json
{
  "repo": "owner/repo",
  "issue_num": 40,
  "state": "open",
  "labels": [
    "status:reviewing",
    "type:feature"
  ],
  "cycle_count": 2,
  "dependency_verdict": "blocked",
  "dependency_state": {
    "verdict": "blocked",
    "resume_label": "status:developing",
    "blocked_reason_hash": "abc123",
    "override_active": false,
    "graph_version": 7,
    "last_reaction_blocked": false,
    "last_evaluated_at": "2026-04-15T15:20:00Z"
  },
  "transition_counts": [
    {
      "from_state": "developing",
      "to_state": "reviewing",
      "count": 1
    },
    {
      "from_state": "reviewing",
      "to_state": "developing",
      "count": 1
    }
  ]
}
```

### `GET /sessions/:id`

The existing `/sessions/:id` browser page stays intact. Tooling can request
JSON by sending `Accept: application/json` or `?format=json`.

Example:

```http
GET /sessions/session-40?format=json
Accept: application/json
```

Example response:

```json
{
  "session_id": "session-40",
  "task_id": "task-40",
  "repo": "owner/repo",
  "issue_num": 40,
  "agent_name": "dev-agent",
  "created_at": "2026-04-15T15:20:00Z",
  "summary": "summary",
  "artifact_paths": {
    "session_dir": "/repo/.workbuddy/sessions/session-40",
    "events_v1": "/repo/.workbuddy/sessions/session-40/events-v1.jsonl",
    "raw": "/repo/.workbuddy/sessions/session-40/stdout"
  }
}
```
