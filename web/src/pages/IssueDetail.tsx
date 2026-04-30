import { useEffect, useState } from 'preact/hooks';
import { getIssueDetail, type ApiError } from '../api/client';
import type { IssueDetail as IssueDetailDTO } from '../api/types';
import { EmptyState } from '../components/EmptyState';
import { GitHubIssueLink } from '../components/GitHubIssueLink';
import { Layout } from '../components/Layout';
import { StateBadge } from '../components/StateBadge';
import { formatTimestamp } from '../lib/format';
import { useRoute } from 'preact-iso';

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

  const [state, setState] = useState<FetchState>({ detail: null, error: null, loading: true });

  useEffect(() => {
    let cancelled = false;
    async function load() {
      const num = Number(numRaw);
      if (!owner || !repo || !numRaw || !Number.isFinite(num) || num <= 0) {
        setState({ detail: null, error: 'invalid issue path', loading: false });
        return;
      }
      try {
        const detail = await getIssueDetail(owner, repo, num);
        if (!cancelled) setState({ detail, error: null, loading: false });
      } catch (error) {
        if (cancelled) return;
        const apiError = error as ApiError;
        if (apiError.status === 401) return;
        setState({ detail: null, error: error instanceof Error ? error.message : 'failed to load issue', loading: false });
      }
    }
    void load();
    return () => {
      cancelled = true;
    };
  }, [owner, repo, numRaw]);

  return (
    <Layout>
      <a href="/" class="wb-backlink">← dashboard</a>
      {state.error ? <div class="wb-panel text-state-danger">{state.error}</div> : null}
      {state.loading && state.detail === null ? <EmptyState glyph="loading" title="loading issue detail" copy="Reading transition history, cycle counts, and linked sessions for this issue." /> : null}
      {state.detail ? <IssueDetailBody detail={state.detail} owner={owner || ''} repo={repo || ''} /> : null}
    </Layout>
  );
}

function IssueDetailBody({ detail, owner, repo }: { detail: IssueDetailDTO; owner: string; repo: string }) {
  return (
    <section class="wb-stack">
      <header class="grid gap-3">
        <p class="wb-section-label">issue detail</p>
        <div class="flex flex-wrap items-center gap-3">
          <h1 class="wb-page-title">#{detail.issue_num}</h1>
          <StateBadge state={detail.current_state} />
          <GitHubIssueLink owner={owner} repo={repo} num={detail.issue_num} variant="icon" />
        </div>
        <p class="wb-page-copy">{detail.title || '(no title)'}</p>
      </header>

      <section class="wb-panel">
        <dl class="wb-kv">
          <dt>repo</dt>
          <dd><span class="wb-id-pill">{detail.repo}</span></dd>
          <dt>labels</dt>
          <dd class="flex flex-wrap gap-2">{detail.labels.map((label) => <span class="wb-id-pill" key={label}>{label}</span>)}</dd>
        </dl>
      </section>

      <section class="grid gap-4 lg:grid-cols-2">
        <div class="wb-table-shell">
          <div class="border-b border-border-hairline px-4 py-3">
            <p class="wb-section-label mb-1">cycle counts</p>
            <h2 class="m-0 text-[20px]">transition totals</h2>
          </div>
          {detail.transition_counts.length === 0 ? (
            <EmptyState glyph="timeline" title="no cycles recorded yet" copy="Run the issue through a dev or review pass and the transition counts will appear here." />
          ) : (
            <div class="wb-table-wrap">
              <table class="wb-table">
                <thead>
                  <tr>
                    <th>from</th>
                    <th>to</th>
                    <th>count</th>
                  </tr>
                </thead>
                <tbody>
                  {detail.transition_counts.map((item) => (
                    <tr key={`${item.from_state}-${item.to_state}`}>
                      <td><StateBadge state={item.from_state} /></td>
                      <td><StateBadge state={item.to_state} /></td>
                      <td class="wb-num">{item.count}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>

        <div class="wb-table-shell">
          <div class="border-b border-border-hairline px-4 py-3">
            <p class="wb-section-label mb-1">sessions</p>
            <h2 class="m-0 text-[20px]">linked runs</h2>
          </div>
          {detail.sessions.length === 0 ? (
            <EmptyState glyph="sessions" title="no sessions recorded" copy="Once an agent claims this issue, you will be able to jump into the session timeline from here." />
          ) : (
            <div class="wb-table-wrap">
              <table class="wb-table">
                <thead>
                  <tr>
                    <th>session</th>
                    <th>agent</th>
                    <th>started</th>
                    <th>status</th>
                  </tr>
                </thead>
                <tbody>
                  {detail.sessions.map((session) => (
                    <tr key={session.session_id}>
                      <td><a href={`/sessions/${encodeURIComponent(session.session_id)}`} class="wb-id-pill">{session.session_id}</a></td>
                      <td>{session.agent}</td>
                      <td class="wb-time">{formatTimestamp(session.started_at)}</td>
                      <td>{session.status || '--'}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      </section>

      <section class="wb-table-shell">
        <div class="border-b border-border-hairline px-4 py-3">
          <p class="wb-section-label mb-1">transition rail</p>
          <h2 class="m-0 text-[20px]">chronological state changes</h2>
        </div>
        {detail.transitions.length === 0 ? (
          <EmptyState glyph="timeline" title="no transitions yet" copy="This issue has not moved through the workflow yet." />
        ) : (
          <div class="wb-timeline-rail">
            {detail.transitions.map((transition, index) => (
              <div class="wb-event-row" key={`${transition.at}-${index}`}>
                <div class="wb-event-toggle">
                  <span class="wb-event-icon">→</span>
                  <div class="grid gap-2">
                    <div class="flex flex-wrap items-center gap-2">
                      <StateBadge state={transition.from} />
                      <span class="wb-faint">to</span>
                      <StateBadge state={transition.to} />
                    </div>
                    <span class="text-[12px] text-text-secondary">{transition.by || 'unknown actor'}</span>
                  </div>
                  <span class="wb-time">{formatTimestamp(transition.at)}</span>
                </div>
              </div>
            ))}
          </div>
        )}
      </section>
    </section>
  );
}
