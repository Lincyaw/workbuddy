import { useEffect, useState } from 'preact/hooks';
import { useRoute } from 'preact-iso';
import { Layout } from '../components/Layout';
import { StateBadge } from '../components/StateBadge';
import { GitHubIssueLink } from '../components/GitHubIssueLink';
import { EmptyState } from '../components/EmptyState';
import { getIssueDetail } from '../api/client';
import type { ApiError } from '../api/client';
import type { IssueDetail as IssueDetailDTO } from '../api/types';
import { formatRelative } from '../utils/time';
import { splitRepoSlug } from '../utils/github';

interface FetchState {
  detail: IssueDetailDTO | null;
  error: string | null;
  loading: boolean;
}

export function IssueDetail() {
  const { params } = useRoute();
  const owner = params.owner;
  const repo = params.repo;
  const numRaw = params.num;

  const [state, setState] = useState<FetchState>({
    detail: null,
    error: null,
    loading: true,
  });

  useEffect(() => {
    let cancelled = false;
    async function load(): Promise<void> {
      const num = Number(numRaw);
      if (!owner || !repo || !numRaw || !Number.isFinite(num) || num <= 0) {
        setState({ detail: null, error: 'invalid issue path', loading: false });
        return;
      }
      try {
        const detail = await getIssueDetail(owner, repo, num);
        if (cancelled) return;
        setState({ detail, error: null, loading: false });
      } catch (err) {
        if (cancelled) return;
        const apiErr = err as ApiError;
        if (apiErr.status === 401) return;
        const message = err instanceof Error ? err.message : 'failed to load issue';
        setState({ detail: null, error: message, loading: false });
      }
    }
    void load();
    return () => {
      cancelled = true;
    };
  }, [owner, repo, numRaw]);

  return (
    <Layout>
      <a href="/" class="wb-backlink">Back to dashboard</a>
      {state.error && <div class="wb-alert wb-alert--danger">{state.error}</div>}
      {state.loading && state.detail === null && (
        <EmptyState icon=".." title="Loading issue detail" copy="Reading transition history, cycle counts, and linked sessions for this issue." />
      )}
      {state.detail && <IssueDetailBody detail={state.detail} />}
    </Layout>
  );
}

function IssueDetailBody({ detail }: { detail: IssueDetailDTO }) {
  const { owner, name } = splitRepoSlug(detail.repo);
  return (
    <>
      <div class="wb-page-header wb-page-header--tight">
        <div>
          <p class="wb-eyebrow">Issue detail</p>
          <h1 class="wb-page-title wb-inline-title">
            <span class="wb-code-pill">{detail.repo}#{detail.issue_num}</span>
            <span>{detail.title || <span class="wb-muted">(no title)</span>}</span>
            <GitHubIssueLink owner={owner} repo={name} num={detail.issue_num} />
          </h1>
        </div>
      </div>

      <div class="wb-card wb-card--md">
        <dl class="wb-key-value-grid">
          <dt>State</dt>
          <dd><StateBadge state={detail.current_state} /></dd>
          <dt>Labels</dt>
          <dd>
            {detail.labels.length === 0
              ? <span class="wb-muted">--</span>
              : detail.labels.map((label) => <span class="wb-code-pill wb-code-pill--spaced">{label}</span>)}
          </dd>
        </dl>
      </div>

      <section class="wb-section">
        <div class="wb-section-heading">
          <div>
            <h2>Cycle counts</h2>
            <p>Transition counts stay numeric and aligned even when the table scrolls on narrow screens.</p>
          </div>
        </div>
        <div class="wb-table-card">
          {detail.transition_counts.length === 0 ? (
            <EmptyState
              icon="<>"
              title="No cycles recorded yet"
              copy="Run the issue through a dev or review pass and the state transition counts will appear here."
            />
          ) : (
            <div class="wb-table-scroll">
              <table class="wb-table">
                <thead>
                  <tr>
                    <th>From</th>
                    <th>To</th>
                    <th>Count</th>
                  </tr>
                </thead>
                <tbody>
                  {detail.transition_counts.map((tc) => (
                    <tr key={`${tc.from_state}->${tc.to_state}`}>
                      <td><StateBadge state={tc.from_state} /></td>
                      <td><StateBadge state={tc.to_state} /></td>
                      <td class="wb-num">{tc.count}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      </section>

      <section class="wb-section">
        <div class="wb-section-heading">
          <div>
            <h2>Transitions</h2>
            <p>A tighter left rail keeps the history readable while preserving exact timestamps and actor identity.</p>
          </div>
        </div>
        <div class="wb-card wb-card--flush">
          {detail.transitions.length === 0 ? (
            <EmptyState
              icon="->"
              title="No transitions yet"
              copy="This issue has not moved through the workflow yet. When the coordinator edits labels, the history will land here."
            />
          ) : (
            <ol class="wb-timeline-list">
              {detail.transitions.map((transition, index) => (
                <li key={`${transition.at}-${index}`} class="wb-timeline-list__item">
                  <div class="wb-timeline-list__time wb-time">
                    {formatRelative(transition.at)}
                    <span>{transition.at}</span>
                  </div>
                  <div class="wb-timeline-list__body">
                    <div class="wb-inline-flow">
                      <StateBadge state={transition.from} />
                      <span class="wb-timeline-arrow">to</span>
                      <StateBadge state={transition.to} />
                    </div>
                    <div class="wb-muted">{transition.by || '--'}</div>
                  </div>
                </li>
              ))}
            </ol>
          )}
        </div>
      </section>

      <section class="wb-section">
        <div class="wb-section-heading">
          <div>
            <h2>Sessions</h2>
            <p>Every linked session stays accessible from here without forcing a full-width page scroll.</p>
          </div>
        </div>
        <div class="wb-table-card">
          {detail.sessions.length === 0 ? (
            <EmptyState
              icon="[]"
              title="No sessions recorded"
              copy="Once an agent claims this issue, you will be able to jump straight into the session timeline from this panel."
            />
          ) : (
            <div class="wb-table-scroll">
              <table class="wb-table">
                <thead>
                  <tr>
                    <th>Session</th>
                    <th>Agent</th>
                    <th>Started</th>
                    <th>Finished</th>
                    <th>Status</th>
                  </tr>
                </thead>
                <tbody>
                  {detail.sessions.map((session) => (
                    <tr key={session.session_id}>
                      <td>
                        <a href={`/sessions/${encodeURIComponent(session.session_id)}`} class="wb-code-pill">
                          {session.session_id}
                        </a>
                      </td>
                      <td>{session.agent}</td>
                      <td class="wb-time">{formatRelative(session.started_at)}</td>
                      <td class="wb-time">
                        {session.finished_at ? formatRelative(session.finished_at) : <span class="wb-muted">--</span>}
                      </td>
                      <td>{session.status || <span class="wb-muted">--</span>}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      </section>
    </>
  );
}
