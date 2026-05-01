import { useEffect, useMemo, useState } from 'preact/hooks';
import { Layout } from '../components/Layout';
import { StateBadge } from '../components/StateBadge';
import { GitHubIssueLink } from '../components/GitHubIssueLink';
import { EmptyState } from '../components/EmptyState';
import { getInFlightIssues, getStatus } from '../api/client';
import type { InFlightIssue, StatusResponse } from '../api/types';
import { fetchSessions, type SessionListItem } from '../api/sessions';
import { copyText, formatClock, formatTimestamp, isToday } from '../lib/format';

const POLL_INTERVAL_MS = 30_000;

interface FetchState {
  status: StatusResponse | null;
  rows: InFlightIssue[];
  sessions: SessionListItem[];
  error: string | null;
  loading: boolean;
}

const INITIAL_STATE: FetchState = {
  status: null,
  rows: [],
  sessions: [],
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
  const [state, setState] = useState<FetchState>(INITIAL_STATE);

  useEffect(() => {
    let cancelled = false;

    async function load(): Promise<void> {
      try {
        const [status, rows, sessions] = await Promise.all([
          getStatus(),
          getInFlightIssues(),
          fetchSessions({ limit: 80 }),
        ]);
        if (cancelled) return;
        setState({
          status,
          rows: rows || [],
          // Phase 3 (REQ-122): fetchSessions returns {rows, offlineWorkers}.
          // Dashboard only needs the rows; the offline-worker banner
          // lives on the Sessions page.
          sessions: sessions.rows || [],
          error: null,
          loading: false,
        });
      } catch (err) {
        if (cancelled) return;
        setState((prev) => ({
          ...prev,
          error: err instanceof Error ? err.message : 'failed to load dashboard',
          loading: false,
        }));
      }
    }

    void load();
    const timer = window.setInterval(() => {
      void load();
    }, POLL_INTERVAL_MS);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
  }, []);

  const dispatchesToday = useMemo(
    () => state.sessions.filter((session) => isToday(session.created_at)).length,
    [state.sessions],
  );
  const failedBeforeStart = useMemo(
    () =>
      state.sessions.filter(
        (session) =>
          session.status === 'aborted_before_start' ||
          (session.degraded === true && (session.duration ?? 0) === 0),
      ).length,
    [state.sessions],
  );
  const recentTransitions = useMemo(
    () =>
      [...state.rows]
        .filter((row) => row.last_transition_at)
        .sort((a, b) => (b.last_transition_at || '').localeCompare(a.last_transition_at || ''))
        .slice(0, 8),
    [state.rows],
  );

  return (
    <Layout>
      <section class="page-header">
        <div>
          <p class="page-eyebrow">operator console</p>
          <h1>Mission control</h1>
        </div>
      </section>

      {state.error ? <div class="error-banner">{state.error}</div> : null}

      <section class="hero-grid">
        <LiveCounterCard index={0} label="in-flight sessions" value={state.status?.active_sessions ?? 0} />
        <LiveCounterCard index={1} label="dispatches today" value={dispatchesToday} />
        <LiveCounterCard index={2} label="failed before start" value={failedBeforeStart} href="/sessions" />
      </section>

      <section class="dashboard-grid">
        <article class="surface-card reveal-card" style="--i: 3">
          <div class="section-heading">
            <div>
              <p class="section-kicker">in-flight issues</p>
              <h2>Hot queue</h2>
            </div>
          </div>

          {state.loading && state.rows.length === 0 ? (
            <div class="loading-copy">Syncing coordinator telemetry…</div>
          ) : state.rows.length === 0 ? (
            <EmptyState
              title="no issues moving — coordinator is idle"
              detail="view recent runs or open an issue with a live status label to wake the board."
              ctaHref="/sessions"
              ctaLabel="view recent runs →"
              inline
            />
          ) : (
            <div class="table-shell sticky-shell">
              <table class="mission-table mission-table-dashboard">
                <thead>
                  <tr>
                    <th>Issue</th>
                    <th>State</th>
                    <th>Worker</th>
                    <th>Last transition</th>
                    <th>Session</th>
                  </tr>
                </thead>
                <tbody>
                  {state.rows.map((row) => (
                    <tr key={`${row.repo}#${row.issue_num}`}>
                      <td data-label="Issue">
                        <div class="issue-id-cell">
                          <a href={issueDetailHref(row)} class="mono-link issue-link">
                            {row.repo}#{row.issue_num}
                          </a>
                          <button
                            type="button"
                            class="icon-copy"
                            onClick={() => copyText(`${row.repo}#${row.issue_num}`)}
                            aria-label={`Copy ${row.repo}#${row.issue_num}`}
                          >
                            ⧉
                          </button>
                        </div>
                        <div class="issue-title-row">
                          <span>{row.title || '(untitled issue)'}</span>
                          <GitHubIssueLink
                            owner={splitRepo(row.repo).owner}
                            repo={splitRepo(row.repo).name}
                            num={row.issue_num}
                            variant="icon"
                          />
                        </div>
                      </td>
                      <td data-label="State">
                        <StateBadge state={row.current_state} />
                      </td>
                      <td data-label="Worker">
                        <span class="mono-chip">{row.claimed_worker_id || 'unclaimed'}</span>
                      </td>
                      <td data-label="Last transition">
                        <div class="table-time">{formatClock(row.last_transition_at)}</div>
                        <div class="table-time-detail">{formatTimestamp(row.last_transition_at)}</div>
                      </td>
                      <td data-label="Session">
                        {row.last_session_id ? (
                          <a href={`/sessions/${encodeURIComponent(row.last_session_id)}`} class="mono-link">
                            {row.last_session_id.slice(0, 12)}
                          </a>
                        ) : (
                          <span class="muted">pending</span>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </article>

        <aside class="surface-card dashboard-rail">
          <div class="section-heading compact-heading">
            <div>
              <p class="section-kicker">recent transitions</p>
              <h2>Last 8</h2>
            </div>
          </div>
          {recentTransitions.length === 0 ? (
            <EmptyState
              title="nothing has transitioned yet"
              detail="once the coordinator advances an issue, the latest state entries land here."
              inline
            />
          ) : (
            <ol class="compact-transition-list">
              {recentTransitions.map((row) => (
                <li key={`${row.repo}:${row.issue_num}:${row.last_transition_at}`}>
                  <span class="compact-transition-time">{formatClock(row.last_transition_at)}</span>
                  <div>
                    <strong>{row.repo}#{row.issue_num}</strong>
                    <div class="muted">entered {row.current_state}</div>
                  </div>
                </li>
              ))}
            </ol>
          )}
        </aside>
      </section>
    </Layout>
  );
}

function LiveCounterCard({
  index,
  label,
  value,
  href,
}: {
  index: number;
  label: string;
  value: number;
  href?: string;
}) {
  const body = (
    <>
      <div class="live-counter-value">{value}</div>
      <div class="live-counter-label">{label}</div>
    </>
  );

  return href ? (
    <a href={href} class="surface-card reveal-card live-counter-card" style={`--i: ${index}`}>
      {body}
    </a>
  ) : (
    <div class="surface-card reveal-card live-counter-card" style={`--i: ${index}`}>
      {body}
    </div>
  );
}
