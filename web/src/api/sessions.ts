import { apiJSON } from './client';

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
  exit_code: number;
  duration: number;
  created_at: string;
  finished_at?: string | null;
  summary?: string;
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

export function fetchSessions(q: SessionListQuery): Promise<SessionListItem[]> {
  return apiJSON<SessionListItem[]>(`/api/v1/sessions${buildSessionsQuery(q)}`);
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
