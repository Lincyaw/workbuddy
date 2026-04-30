import { useEffect, useMemo, useState } from 'preact/hooks';
import { useLocation } from 'preact-iso';
import { getInFlightIssues, getStatus, type ApiError } from '../api/client';
import type { InFlightIssue, StatusResponse } from '../api/types';
import { fetchSessions } from '../api/sessions';
import { EmptyState } from '../components/EmptyState';
import { GitHubIssueLink } from '../components/GitHubIssueLink';
import { Layout } from '../components/Layout';
import { StateBadge } from '../components/StateBadge';
import { copyText, formatTimestamp } from '../lib/format';

const POLL_INTERVAL_MS = 5_000;

interface DashboardState {
  status: StatusResponse | null;
  issues: InFlightIssue[];
  dispatchesToday: number;
  failedBeforeStart: number;
  loading: boolean;
  error: string | null;
  live: boolean;
}

const INITIAL: DashboardState = {
  status: null,
  issues: [],
  dispatchesToday: 0,
  failedBeforeStart: 0,
  loading: true,
  error: null,
  live: false,
};

function issueHref(row: InFlightIssue): string {
  const [owner, repo] = row.repo.split('/');
  return `/issues/${owner}/${repo}/${row.issue_num}`;
}

export function Dashboard() {
  const { route } = useLocation();
  const [state, setState] = useState(INITIAL);

  useEffect(() => {
    let cancelled = false;

    const load = async () => {
      try {
        const [status, issues, sessions] = await Promise.all([
          getStatus(),
          getInFlightIssues(),
          fetchSessions({ limit: 200, offset: 0 }),
        ]);
        if (cancelled) return;
        const today = new Date().toDateString();
        const dispatchesToday = sessions.filter((session) => new Date(session.created_at).toDateString() === today).length;
        const failedBeforeStart = sessions.filter((session) => session.status === 'aborted_before_start').length;
        setState({
          status,
          issues,
          dispatchesToday,
          failedBeforeStart,
          loading: false,
          error: null,
          live: true,
        });
      } catch (error) {
        if (cancelled) return;
        const apiError = error as ApiError;
        if (apiError.status === 401) return;
        setState((current) => ({
          ...current,
          loading: false,
          live: false,
          error: error instanceof Error ? error.message : 'failed to load dashboard',
        }));
      }
    };

    void load();
    const timer = window.setInterval(() => void load(), POLL_INTERVAL_MS);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
  }, []);

  const transitions = useMemo(
    () => [...state.issues].sort((left, right) => Date.parse(right.last_transition_at || '') - Date.parse(left.last_transition_at || '')).slice(0, 8),
    [state.issues],
  );

  return (
    <Layout>
      <section class="grid gap-6 xl:grid-cols-[minmax(0,1fr)_320px]">
        <div class="wb-stack">
          <header class="grid gap-3">
            <p class="wb-section-label">mission control</p>
            <div class="flex flex-wrap items-center justify-between gap-3">
              <div>
                <h1 class="wb-page-title">dashboard</h1>
                <p class="wb-page-copy">Watch active dispatches, count failed launches, and keep the operator lane alive without leaving the console.</p>
              </div>
              <div class="wb-live-indicator">
                <span class={`wb-live-dot${state.live ? ' is-live' : ''}`} aria-hidden="true" />
                {state.live ? 'current' : 'waiting'}
              </div>
            </div>
          </header>

          {state.error ? <div class="wb-panel text-state-danger">{state.error}</div> : null}

          <div class="grid gap-3 md:grid-cols-3">
            <article class="wb-panel wb-reveal wb-counter" style={{ '--i': 0 }}>
              <span class="wb-counter__value">{state.status?.active_sessions ?? '--'}</span>
              <span class="wb-counter__label">in-flight sessions</span>
            </article>
            <article class="wb-panel wb-reveal wb-counter" style={{ '--i': 1 }}>
              <span class="wb-counter__value">{state.dispatchesToday}</span>
              <span class="wb-counter__label">dispatches today</span>
            </article>
            <article class="wb-panel wb-reveal wb-counter" style={{ '--i': 2 }}>
              <a href="/sessions" class="no-underline">
                <span class="wb-counter__value">{state.failedBeforeStart}</span>
                <span class="wb-counter__label">failed before start</span>
              </a>
            </article>
          </div>

          <article class="wb-table-shell wb-reveal" style={{ '--i': 3 }}>
            <div class="flex items-center justify-between gap-3 border-b border-border-hairline px-4 py-3">
              <div>
                <h2 class="m-0 text-[20px]">in-flight issues</h2>
                <p class="wb-page-copy mt-1 text-[13px]">Sticky header, monospace IDs, live state colors, and copy affordances when the row is hot.</p>
              </div>
            </div>
            {state.loading && state.issues.length === 0 ? (
              <EmptyState glyph="loading" title="loading live issues" copy="Polling the coordinator for the current dispatch lane." />
            ) : state.issues.length === 0 ? (
              <EmptyState
                glyph="idle"
                title="no issues moving"
                copy="coordinator is idle"
                cta={<a href="/sessions" class="wb-cta wb-cta--primary">view recent runs →</a>}
              />
            ) : (
              <>
                <div class="wb-table-wrap">
                  <table class="wb-table">
                    <thead>
                      <tr>
                        <th>issue</th>
                        <th>state</th>
                        <th>title</th>
                        <th>transition</th>
                        <th>worker</th>
                        <th>session</th>
                      </tr>
                    </thead>
                    <tbody>
                      {state.issues.map((row) => (
                        <tr key={`${row.repo}#${row.issue_num}`} onClick={() => route(issueHref(row))}>
                          <td>
                            <div class="flex items-center gap-2">
                              <a href={issueHref(row)} class="wb-id-pill" onClick={(event) => event.stopPropagation()}>
                                {row.repo}#{row.issue_num}
                              </a>
                              <button type="button" class="wb-faint" onClick={(event) => {
                                event.stopPropagation();
                                copyText(`${row.repo}#${row.issue_num}`);
                              }}>
                                copy
                              </button>
                            </div>
                          </td>
                          <td><StateBadge state={row.current_state} /></td>
                          <td>{row.title || <span class="wb-faint">(untitled issue)</span>}</td>
                          <td class="wb-time">{formatTimestamp(row.last_transition_at)}</td>
                          <td>{row.claimed_worker_id ? <span class="wb-id-pill">{row.claimed_worker_id}</span> : <span class="wb-faint">idle</span>}</td>
                          <td>{row.last_session_id ? <a href={`/sessions/${encodeURIComponent(row.last_session_id)}`} class="wb-id-pill">open</a> : <span class="wb-faint">--</span>}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
                <div class="wb-mobile-card-list p-4">
                  {state.issues.map((row) => (
                    <button type="button" class="wb-mobile-card text-left" key={`${row.repo}#${row.issue_num}`} onClick={() => route(issueHref(row))}>
                      <div class="flex items-start justify-between gap-3">
                        <div class="grid gap-2">
                          <span class="wb-id-pill">{row.repo}#{row.issue_num}</span>
                          <strong>{row.title || '(untitled issue)'}</strong>
                        </div>
                        <StateBadge state={row.current_state} />
                      </div>
                      <div class="mt-3 grid gap-1 text-[12px] text-text-secondary">
                        <span>worker: {row.claimed_worker_id || 'idle'}</span>
                        <span>last transition: {formatTimestamp(row.last_transition_at) || '--'}</span>
                      </div>
                    </button>
                  ))}
                </div>
              </>
            )}
          </article>
        </div>

        <aside class="wb-stack">
          <section class="wb-panel">
            <p class="wb-section-label">recent transitions</p>
            {transitions.length === 0 ? (
              <EmptyState glyph="timeline" title="transition rail is quiet" copy="New state entries will appear here as soon as the coordinator flips labels." />
            ) : (
              <div class="grid gap-3">
                {transitions.map((row) => {
                  const [owner, repo] = row.repo.split('/');
                  return (
                    <div class="border-b border-border-hairline pb-3 last:border-b-0 last:pb-0" key={`${row.repo}#${row.issue_num}`}>
                      <div class="mb-2 flex items-center justify-between gap-2">
                        <span class="wb-id-pill">#{row.issue_num}</span>
                        <StateBadge state={row.current_state} />
                      </div>
                      <div class="text-[13px] text-text-secondary">{row.repo}</div>
                      <div class="mt-1 text-[13px]">{row.title || '(untitled issue)'}</div>
                      <div class="mt-2 flex items-center justify-between gap-2 text-[12px] text-text-tertiary">
                        <span>{formatTimestamp(row.last_transition_at) || '--'}</span>
                        <GitHubIssueLink owner={owner} repo={repo} num={row.issue_num} variant="icon" />
                      </div>
                    </div>
                  );
                })}
              </div>
            )}
          </section>
        </aside>
      </section>
    </Layout>
  );
}
