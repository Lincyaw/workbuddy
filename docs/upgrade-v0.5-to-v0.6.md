# Upgrade v0.5 to v0.6

## Worker management tunnel

v0.6 adds a worker-initiated WebSocket tunnel for the worker management and session APIs. In split-host deployments the worker now dials the coordinator at `/api/v1/workers/tunnel` with the same bearer token used by the rest of the coordinator API.

What changes:

- Keep `workbuddy coordinator --auth --token-file ...` as before.
- Start workers normally; `--coordinator-tunnel` is enabled by default.
- Remove reverse SSH tunnel, autossh, ad-hoc reverse proxy, and random public worker-management port setup when every worker is on v0.6 or newer.
- Remove `--mgmt-public-url` unless you intentionally want the legacy fallback path.
- Use `--no-coordinator-tunnel` to keep the old topology; in that mode `--mgmt-public-url` / `--audit-public-url` must still point at a coordinator-reachable worker HTTP endpoint.

The task dispatch long-polling channel is unchanged. If the tunnel disconnects, already-dispatched agent tasks continue running; only session browsing and worker management calls are temporarily unavailable until the worker reconnects.
