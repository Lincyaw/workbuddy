import { useEffect, useState } from 'preact/hooks';
import { useLocation } from 'preact-iso';
import { Layout } from '../components/Layout';
import { StateBadge } from '../components/StateBadge';
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
        if (apiErr.status === 401) return; // client.ts already redirected to /login
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
      <h1>Dashboard</h1>
      {state.error && <div class="error-banner">{state.error}</div>}

      <section class="cards" aria-label="Pipeline health">
        <Card label="In-flight issues" value={state.status?.in_flight_issues} />
        <Card
          label="Stuck > 1h"
          value={state.status?.stuck_issues_over_1h}
          tone={(state.status?.stuck_issues_over_1h ?? 0) > 0 ? 'danger' : undefined}
        />
        <Card label="Done (24h)" value={state.status?.done_24h} tone="good" />
        <Card
          label="Failed (24h)"
          value={state.status?.failed_24h}
          tone={(state.status?.failed_24h ?? 0) > 0 ? 'warn' : undefined}
        />
      </section>

      <h2>In-flight issues</h2>
      <div class="panel">
        {state.loading && state.rows === null ? (
          <div class="empty">Loading…</div>
        ) : !state.rows || state.rows.length === 0 ? (
          <div class="empty">No in-flight issues right now.</div>
        ) : (
          <table class="clickable">
            <thead>
              <tr>
                <th>Repo#Num</th>
                <th>Title</th>
                <th>State</th>
                <th>Cycle</th>
                <th>Last Transition</th>
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
        )}
      </div>
    </Layout>
  );
}

interface CardProps {
  label: string;
  value: number | undefined;
  tone?: 'warn' | 'danger' | 'good';
}

function Card({ label, value, tone }: CardProps) {
  const cls = tone ? `card ${tone}` : 'card';
  return (
    <div class={cls}>
      <div class="label">{label}</div>
      <div class="value">{value ?? '—'}</div>
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
  const cycleClass = cycleCount > cap ? 'cell-cap-hit' : '';
  const stuck = row.stuck_for_seconds > ONE_HOUR_SECONDS;
  const issueHref = issueDetailHref(row);

  function handleRowClick(e: MouseEvent) {
    // Avoid hijacking explicit clicks on links/buttons inside the row.
    const target = e.target as HTMLElement;
    if (target.closest('a, button')) return;
    onOpen();
  }

  return (
    <tr onClick={handleRowClick}>
      <td>
        <a href={issueHref} class="code-chip">
          {row.repo}#{row.issue_num}
        </a>
      </td>
      <td>{row.title || <span class="muted">(no title)</span>}</td>
      <td>
        <StateBadge state={row.current_state} />
      </td>
      <td>
        <span class={cycleClass} title="dev↔review cycle count / orchestrator cap">
          {cycleCount} / {cap}
        </span>
      </td>
      <td class={stuck ? 'cell-stuck' : ''}>
        {row.last_transition_at
          ? formatRelative(row.last_transition_at)
          : <span class="muted">never</span>}
      </td>
      <td>
        {row.claimed_worker_id
          ? <span class="code-chip">{row.claimed_worker_id}</span>
          : <span class="muted">—</span>}
      </td>
      <td>
        {row.last_session_id
          ? <a href={`/sessions/${encodeURIComponent(row.last_session_id)}`}>view</a>
          : <span class="muted">—</span>}
      </td>
    </tr>
  );
}
