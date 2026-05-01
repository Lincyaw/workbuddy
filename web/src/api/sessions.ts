import { apiFetch, apiJSON } from './client';

// DegradedReason mirrors the machine label the audit API attaches to a
// session whose response can't be served normally. Phase 3 of the
// session-data ownership refactor (REQ-122) consolidated the legal
// values:
//
//   * 'no_events_file'  — the worker recorded a DB row and a terminal
//                          status, but events-v1.jsonl never got any
//                          content. Real signal: agent crashed before
//                          producing any tool calls.
//   * 'worker_offline'  — coordinator's sessionproxy could not dial the
//                          owning worker (or the worker did not register
//                          an audit_url). The session data lives on a
//                          worker that is currently unreachable.
//   * 'no_db_row'       — DEPRECATED. Pre-Phase-3 synthesis from disk
//                          metadata. Should not appear on a coordinator
//                          ≥ v0.5.x; kept in the union so the SPA
//                          renders sensibly if it does.
//
// Anything else is rendered with a permissive fallback.
export type DegradedReason =
  | 'no_events_file'
  | 'worker_offline'
  | 'no_db_row'
  | string;

export interface SessionListItem {
  session_id: string;
  task_id?: string;
  repo: string;
  issue_num: number;
  agent_name: string;
  runtime?: string;
  worker_id?: string;
  attempt: number;
  status: string;
  task_status?: string;
  workflow?: string;
  current_state?: string;
  rollout_index?: number;
  rollouts_total?: number;
  rollout_group_id?: string;
  exit_code: number;
  duration: number;
  created_at: string;
  finished_at?: string | null;
  summary?: string;
  // Issue #275: degraded rows are sessions with no events file (or no DB
  // row at all). The SPA renders a ⚠️ badge so operators can spot
  // never-actually-ran sessions in the list at a glance.
  degraded?: boolean;
  degraded_reason?: DegradedReason;
}

export interface SessionDetail {
  session_id: string;
  task_id?: string;
  repo: string;
  issue_num: number;
  agent_name: string;
  runtime?: string;
  worker_id?: string;
  attempt: number;
  status: string;
  exit_code: number;
  duration: number;
  created_at: string;
  finished_at?: string | null;
  summary?: string;
  stdout_summary?: string;
  stderr_summary?: string;
  artifact_paths: {
    session_dir?: string;
    events_v1?: string;
    raw?: string;
  };
  // degraded === true means the API could not produce a normal response.
  // Legitimate reasons are enumerated by DegradedReason above.
  degraded?: boolean;
  degraded_reason?: DegradedReason;
}

export interface SessionEvent {
  index: number;
  kind: string;
  ts?: string;
  session_id?: string;
  turn_id?: string;
  seq: number;
  payload?: unknown;
  truncated?: boolean;
}

export interface SessionEventsResponse {
  events: SessionEvent[];
  total: number;
  start?: number;
  end?: number;
}

export interface SessionListQuery {
  repo?: string;
  agent?: string;
  issue?: string;
  limit?: number;
  offset?: number;
}

export interface SessionListResult {
  rows: SessionListItem[];
  // offlineWorkers carries the IDs the coordinator could not reach
  // during fan-out, sourced from the X-Workbuddy-Worker-Offline response
  // header. Empty array when every worker responded.
  offlineWorkers: string[];
}

export function buildSessionsQuery(q: SessionListQuery): string {
  const sp = new URLSearchParams();
  if (q.repo) sp.set('repo', q.repo);
  if (q.agent) sp.set('agent', q.agent);
  if (q.issue) sp.set('issue', q.issue);
  if (q.limit != null) sp.set('limit', String(q.limit));
  if (q.offset != null) sp.set('offset', String(q.offset));
  const s = sp.toString();
  return s ? `?${s}` : '';
}

// fetchSessions returns both the row list and the offline-workers
// sidecar (lifted from the X-Workbuddy-Worker-Offline header). The
// header was added in Phase 2; Phase 3 surfaces it in the UI.
export async function fetchSessions(q: SessionListQuery): Promise<SessionListResult> {
  const resp = await apiFetch(`/api/v1/sessions${buildSessionsQuery(q)}`);
  const text = await resp.text();
  if (!resp.ok) {
    const err = new Error(`request failed: ${resp.status}`) as Error & {
      status?: number;
      body?: unknown;
    };
    err.status = resp.status;
    err.body = text;
    throw err;
  }
  let rows: SessionListItem[] = [];
  if (text) {
    try {
      const parsed = JSON.parse(text);
      if (Array.isArray(parsed)) rows = parsed as SessionListItem[];
    } catch {
      rows = [];
    }
  }
  const header = resp.headers.get('X-Workbuddy-Worker-Offline') || '';
  const offlineWorkers = header
    .split(',')
    .map((value) => value.trim())
    .filter(Boolean);
  return { rows, offlineWorkers };
}

export function fetchSession(id: string): Promise<SessionDetail> {
  return apiJSON<SessionDetail>(`/api/v1/sessions/${encodeURIComponent(id)}`);
}

export function fetchSessionEvents(
  id: string,
  opts: { limit?: number; offset?: number; tail?: boolean } = {},
): Promise<SessionEventsResponse> {
  const sp = new URLSearchParams();
  if (opts.limit != null) sp.set('limit', String(opts.limit));
  if (opts.offset != null) sp.set('offset', String(opts.offset));
  if (opts.tail) sp.set('tail', '1');
  const qs = sp.toString();
  const suffix = qs ? `?${qs}` : '';
  return apiJSON<SessionEventsResponse>(
    `/api/v1/sessions/${encodeURIComponent(id)}/events${suffix}`,
  );
}

export function sessionStreamURL(id: string, after: number): string {
  return `/api/v1/sessions/${encodeURIComponent(id)}/stream?after=${after}`;
}
