import { useEffect, useState } from 'preact/hooks';
import { useRoute } from 'preact-iso';
import { Layout } from '../components/Layout';
import { StateBadge } from '../components/StateBadge';
import { GitHubIssueLink } from '../components/GitHubIssueLink';
import { EmptyState } from '../components/EmptyState';
import { getIssueDetail, getIssueRollouts } from '../api/client';
import type { IssueDetail as IssueDetailDTO, RolloutGroup } from '../api/types';
import { formatTimestamp } from '../lib/format';
import { splitRepoSlug } from '../utils/github';
import { RolloutGroupPanel } from '../components/RolloutGroupPanel';

export function IssueDetail() {
  const { params } = useRoute();
  const owner = params.owner;
  const repo = params.repo;
  const num = Number(params.num || '0');
  const [detail, setDetail] = useState<IssueDetailDTO | null>(null);
  const [rollouts, setRollouts] = useState<RolloutGroup | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    if (!owner || !repo || !Number.isFinite(num) || num <= 0) {
      setError('invalid issue path');
      setLoading(false);
      return;
    }
    let cancelled = false;
    Promise.allSettled([getIssueDetail(owner, repo, num), getIssueRollouts(owner, repo, num)])
      .then(([detailResult, rolloutResult]) => {
        if (!cancelled) {
          if (detailResult.status !== 'fulfilled') {
            throw detailResult.reason;
          }
          setDetail(detailResult.value);
          setRollouts(rolloutResult.status === 'fulfilled' ? rolloutResult.value : null);
          setError(null);
        }
      })
      .catch((err) => {
        if (!cancelled) setError(err instanceof Error ? err.message : 'failed to load issue');
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [num, owner, repo]);

  return (
    <Layout>
      <section class="page-header page-header-tight">
        <div>
          <p class="page-eyebrow">issue audit</p>
          <h1>{detail ? `${detail.repo}#${detail.issue_num}` : 'Issue detail'}</h1>
        </div>
      </section>
      {error ? <div class="error-banner">{error}</div> : null}
      {loading && !detail ? (
        <div class="loading-copy">Loading issue telemetry…</div>
      ) : detail ? (
        <IssueDetailBody detail={detail} rollouts={rollouts} />
      ) : null}
    </Layout>
  );
}

function IssueDetailBody({ detail, rollouts }: { detail: IssueDetailDTO; rollouts: RolloutGroup | null }) {
  const { owner, name } = splitRepoSlug(detail.repo);
  const compareHref = buildCompareHref(owner, name, detail.issue_num, rollouts);
  return (
    <div class="issue-detail-grid">
      <section class="surface-card">
        <div class="section-heading compact-heading">
          <div>
            <p class="section-kicker">current state</p>
            <h2>{detail.title || '(untitled issue)'}</h2>
          </div>
          <GitHubIssueLink owner={owner} repo={name} num={detail.issue_num} />
        </div>
        <dl class="meta-list">
          <dt>State</dt>
          <dd><StateBadge state={detail.current_state} /></dd>
          <dt>Labels</dt>
          <dd class="meta-flow">
            {detail.labels.length > 0 ? detail.labels.map((label) => <span class="mono-chip">{label}</span>) : '—'}
          </dd>
        </dl>
      </section>

      <RolloutGroupPanel
        repo={detail.repo}
        issueNum={detail.issue_num}
        group={rollouts}
        compareHref={compareHref}
      />

      <section class="surface-card">
        <div class="section-heading compact-heading">
          <div>
            <p class="section-kicker">transition history</p>
            <h2>Timeline</h2>
          </div>
        </div>
        {detail.transitions.length === 0 ? (
          <EmptyState
            title="no transitions recorded yet"
            detail="once the coordinator moves this issue through a workflow, entries land here."
            inline
          />
        ) : (
          <div class="timeline-list compact-timeline-list">
            {detail.transitions.map((transition, index) => (
              <article key={`${transition.at}:${index}`} class="timeline-event kind-system">
                <button type="button" class="timeline-row timeline-row-static">
                  <span class="timeline-kind">◦</span>
                  <span class="timeline-title">{transition.from} → {transition.to}</span>
                  <span class="timeline-time">{formatTimestamp(transition.at, true)}</span>
                </button>
              </article>
            ))}
          </div>
        )}
      </section>

      <section class="surface-card issue-sessions-card">
        <div class="section-heading compact-heading">
          <div>
            <p class="section-kicker">sessions</p>
            <h2>Execution trail</h2>
          </div>
        </div>
        {detail.sessions.length === 0 ? (
          <EmptyState
            title="no sessions recorded yet"
            detail="the first dispatch will populate this execution trail."
            inline
          />
        ) : (
          <div class="table-shell">
            <table class="mission-table">
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
                    <td data-label="Session"><a href={`/sessions/${encodeURIComponent(session.session_id)}`} class="mono-link">{session.session_id.slice(0, 16)}</a></td>
                    <td data-label="Agent">{session.agent}</td>
                    <td data-label="Started">{formatTimestamp(session.started_at, true)}</td>
                    <td data-label="Finished">{session.finished_at ? formatTimestamp(session.finished_at, true) : '—'}</td>
                    <td data-label="Status">{session.status || '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </div>
  );
}

function buildCompareHref(owner: string, repo: string, issueNum: number, rollouts: RolloutGroup | null): string | undefined {
  if (!rollouts || rollouts.members.length < 2) return undefined;
  const params = new URLSearchParams();
  const chosen = rollouts.members.slice(0, 3);
  if (chosen[0]) params.set('a', String(chosen[0].rollout_index));
  if (chosen[1]) params.set('b', String(chosen[1].rollout_index));
  if (chosen[2]) params.set('c', String(chosen[2].rollout_index));
  return `/issues/${owner}/${repo}/${issueNum}/rollouts/compare?${params.toString()}`;
}
