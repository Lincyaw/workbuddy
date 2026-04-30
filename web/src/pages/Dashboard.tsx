import { useEffect, useState } from 'preact/hooks';
import { useLocation } from 'preact-iso';
import { Layout } from '../components/Layout';
import { StateBadge } from '../components/StateBadge';
import { GitHubIssueLink } from '../components/GitHubIssueLink';
import { EmptyState } from '../components/EmptyState';
import { getInFlightIssues, getStatus } from '../api/client';
import type { ApiError } from '../api/client';
import type { InFlightIssue, StatusResponse } from '../api/types';
import { ONE_HOUR_SECONDS, formatRelative } from '../utils/time';
import { DEFAULT_MAX_REVIEW_CYCLES, devReviewCycleCount } from '../utils/cycle';

const POLL_INTERVAL_MS = 30_000;

interface FetchState {
  status: StatusResponse | null;
  rows: InFlightIssue[] | null;
  error: string | null;
  loading: boolean;
}

const INITIAL_STATE: FetchState = {
  status: null,
  rows: null,
  error: null,
  loading: true,
};

function splitRepo(repo: string): { owner: string; name: string } {
  const slash = repo.indexOf('/');
  if (slash <= 0) return { owner: repo, name: '' };
  return { owner: repo.slice(0, slash), name: repo.slice(slash + 1) };
}

function issueDetailHref(row: InFlightIssue): string {
  const { owner, name } = splitRepo(row.repo);
  return `/issues/${owner}/${name}/${row.issue_num}`;
}

export function Dashboard() {
  const { route } = useLocation();
  const [state, setState] = useState<FetchState>(INITIAL_STATE);

  useEffect(() => {
    let cancelled = false;
    let timer: ReturnType<typeof setInterval> | null = null;

    async function load(): Promise<void> {
      try {
        const [status, rows] = await Promise.all([getStatus(), getInFlightIssues()]);
        if (cancelled) return;
        setState({ status, rows, error: null, loading: false });
      } catch (err) {
        if (cancelled) return;
        const apiErr = err as ApiError;
        if (apiErr.status === 401) return;
        const message = err instanceof Error ? err.message : 'failed to load dashboard';
        setState((prev) => ({ ...prev, error: message, loading: false }));
      }
    }

    void load();
    timer = setInterval(load, POLL_INTERVAL_MS);
    return () => {
      cancelled = true;
      if (timer !== null) clearInterval(timer);
    };
  }, []);

  return (
    <Layout>
      <div class="wb-page-header">
        <div>
          <p class="wb-eyebrow">Overview</p>
          <h1 class="wb-page-title">Dashboard</h1>
          <p class="wb-page-subtitle">Track active issues, cycle pressure, and stuck sessions from one place.</p>
        </div>
      </div>
      {state.error && <div class="wb-alert wb-alert--danger">{state.error}</div>}

      <section class="wb-summary-grid" aria-label="Pipeline health">
        <MetricCard label="In-flight issues" value={state.status?.in_flight_issues} />
        <MetricCard
          label="Stuck longer than 1 hour"
          value={state.status?.stuck_issues_over_1h}
          tone={(state.status?.stuck_issues_over_1h ?? 0) > 0 ? 'danger' : 'neutral'}
        />
        <MetricCard label="Done in 24 hours" value={state.status?.done_24h} tone="success" />
        <MetricCard
          label="Failed in 24 hours"
          value={state.status?.failed_24h}
          tone={(state.status?.failed_24h ?? 0) > 0 ? 'warning' : 'neutral'}
        />
      </section>

      <section class="wb-section">
        <div class="wb-section-heading">
          <div>
            <h2>In-flight issues</h2>
            <p>Rows stay compact on desktop and scroll safely on smaller screens.</p>
          </div>
        </div>
        <div class="wb-table-card">
          {state.loading && state.rows === null ? (
            <EmptyState icon=".." title="Loading issues" copy="Fetching the latest coordinator status and active issue claims." />
          ) : !state.rows || state.rows.length === 0 ? (
            <EmptyState
              icon="--"
              title="Nothing is in flight"
              copy="When the coordinator dispatches work, active issues will show up here with worker ownership and session links."
            />
          ) : (
            <div class="wb-table-scroll">
              <table class="wb-table wb-table--dashboard">
                <thead>
                  <tr>
                    <th>Repo#Num</th>
                    <th>Title</th>
                    <th>State</th>
                    <th>Cycle</th>
                    <th>Last transition</th>
                    <th>Worker</th>
                    <th>Session</th>
                  </tr>
                </thead>
                <tbody>
                  {state.rows.map((row) => (
                    <IssueRow
                      key={`${row.repo}#${row.issue_num}`}
                      row={row}
                      onOpen={() => route(issueDetailHref(row))}
                    />
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      </section>
    </Layout>
  );
}

interface MetricCardProps {
  label: string;
  value: number | undefined;
  tone?: 'neutral' | 'warning' | 'danger' | 'success';
}

function MetricCard({ label, value, tone = 'neutral' }: MetricCardProps) {
  return (
    <div class={`wb-summary-card wb-summary-card--${tone}`}>
      <div class="wb-summary-label">{label}</div>
      <div class="wb-summary-value wb-num">{value ?? '--'}</div>
    </div>
  );
}

interface IssueRowProps {
  row: InFlightIssue;
  onOpen: () => void;
}

function IssueRow({ row, onOpen }: IssueRowProps) {
  const cycleCount = devReviewCycleCount(row.cycle_counts);
  const cap = DEFAULT_MAX_REVIEW_CYCLES;
  const cycleClass = cycleCount > cap ? 'wb-text-danger' : 'wb-text-secondary';
  const stuck = row.stuck_for_seconds > ONE_HOUR_SECONDS;
  const issueHref = issueDetailHref(row);

  function handleRowClick(e: MouseEvent) {
    const target = e.target as HTMLElement;
    if (target.closest('a, button')) return;
    onOpen();
  }

  return (
    <tr class="wb-row-link" onClick={handleRowClick}>
      <td class="wb-code-cell">
        <a href={issueHref} class="wb-code-pill">
          {row.repo}#{row.issue_num}
        </a>{' '}
        <GitHubIssueLink
          owner={splitRepo(row.repo).owner}
          repo={splitRepo(row.repo).name}
          num={row.issue_num}
          variant="icon"
        />
      </td>
      <td>{row.title || <span class="wb-muted">(no title)</span>}</td>
      <td><StateBadge state={row.current_state} /></td>
      <td class={`wb-num ${cycleClass}`} title="dev↔review cycle count / orchestrator cap">
        {cycleCount} / {cap}
      </td>
      <td class={`${stuck ? 'wb-text-danger' : 'wb-time'} wb-nowrap`}>
        {row.last_transition_at ? formatRelative(row.last_transition_at) : <span class="wb-muted">never</span>}
      </td>
      <td>
        {row.claimed_worker_id ? <span class="wb-code-pill">{row.claimed_worker_id}</span> : <span class="wb-muted">--</span>}
      </td>
      <td>
        {row.last_session_id ? <a href={`/sessions/${encodeURIComponent(row.last_session_id)}`}>Open</a> : <span class="wb-muted">--</span>}
      </td>
    </tr>
  );
}
