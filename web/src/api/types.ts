// Hand-written TS shapes for the Go JSON audit API. Mirrors the structs in
// `internal/auditapi/handler.go`. Update both sides together when extending
// the API contract.

export interface StatusResponse {
  active_sessions: number;
  workers: number;
  last_poll: string | null;
  in_flight_issues: number;
  stuck_issues_over_1h: number;
  done_24h: number;
  failed_24h: number;
}

export interface InFlightIssue {
  repo: string;
  issue_num: number;
  title: string;
  current_state: string;
  current_label: string;
  labels: string[];
  cycle_counts: Record<string, number>;
  last_transition_at?: string | null;
  stuck_for_seconds: number;
  claimed_worker_id?: string;
  last_session_id?: string;
  last_session_url?: string;
}

export interface IssueTransition {
  from: string;
  to: string;
  at: string;
  by?: string;
}

export interface TransitionCount {
  from_state: string;
  to_state: string;
  count: number;
}

export interface IssueSessionRef {
  session_id: string;
  agent: string;
  started_at: string;
  finished_at?: string | null;
  status?: string;
  exit_code: number;
  worker_id?: string;
  task_id?: string;
  rollout_index?: number;
  rollouts_total?: number;
  rollout_group_id?: string;
}

export interface RolloutMember {
  rollout_index: number;
  pr_number: number;
  status: string;
  session_id?: string;
  worker_id?: string;
  started_at: string;
  ended_at?: string | null;
  duration_seconds: number;
}

export interface RolloutSynthOutcome {
  decision: string;
  chosen_pr?: number;
  synth_pr?: number;
  chosen_rollout_index?: number;
  reason?: string;
  ts: string;
}

export interface RolloutGroup {
  group_id: string;
  rollouts_total: number;
  members: RolloutMember[];
  synth_outcome?: RolloutSynthOutcome | null;
}

export interface IssueDetail {
  repo: string;
  issue_num: number;
  title: string;
  current_state: string;
  labels: string[];
  transitions: IssueTransition[];
  transition_counts: TransitionCount[];
  sessions: IssueSessionRef[];
}
