import { useEffect, useState } from 'preact/hooks';
import { useRoute } from 'preact-iso';
import { Layout } from '../components/Layout';
import { StateBadge } from '../components/StateBadge';
import { getIssueDetail } from '../api/client';
import type { ApiError } from '../api/client';
import type { IssueDetail as IssueDetailDTO } from '../api/types';
import { formatRelative } from '../utils/time';

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
      <a href="/" class="muted" style="display: inline-block; margin-bottom: 0.6rem;">
        ← Dashboard
      </a>
      {state.error && <div class="error-banner">{state.error}</div>}
      {state.loading && state.detail === null && <div class="empty">Loading…</div>}
      {state.detail && <IssueDetailBody detail={state.detail} />}
    </Layout>
  );
}

function IssueDetailBody({ detail }: { detail: IssueDetailDTO }) {
  return (
    <>
      <h1>
        <span class="code-chip">{detail.repo}#{detail.issue_num}</span>{' '}
        {detail.title || <span class="muted">(no title)</span>}
      </h1>

      <dl class="kv">
        <dt>State</dt>
        <dd><StateBadge state={detail.current_state} /></dd>
        <dt>Labels</dt>
        <dd>
          {detail.labels.length === 0
            ? <span class="muted">—</span>
            : detail.labels.map((label) => (
                <span class="code-chip" style="margin-right: 0.3rem;">{label}</span>
              ))}
        </dd>
      </dl>

      <h2>Cycle counts</h2>
      <div class="panel">
        {detail.transition_counts.length === 0 ? (
          <div class="empty">No transitions recorded yet.</div>
        ) : (
          <table>
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
                  <td>{tc.count}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      <h2>Transitions</h2>
      <div class="panel">
        {detail.transitions.length === 0 ? (
          <div class="empty">No transitions recorded yet.</div>
        ) : (
          <ol class="timeline">
            {detail.transitions.map((t, i) => (
              <li key={`${t.at}-${i}`}>
                <span class="ts">
                  {formatRelative(t.at)}
                  <span class="muted" style="display: block; font-size: 0.75rem;">{t.at}</span>
                </span>
                <span>
                  <StateBadge state={t.from} />
                  <span class="transition-arrow">→</span>
                  <StateBadge state={t.to} />
                </span>
                <span class="muted">{t.by || '—'}</span>
              </li>
            ))}
          </ol>
        )}
      </div>

      <h2>Sessions</h2>
      <div class="panel">
        {detail.sessions.length === 0 ? (
          <div class="empty">No sessions recorded yet.</div>
        ) : (
          <table>
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
              {detail.sessions.map((s) => (
                <tr key={s.session_id}>
                  <td>
                    <a href={`/sessions/${encodeURIComponent(s.session_id)}`} class="code-chip">
                      {s.session_id}
                    </a>
                  </td>
                  <td>{s.agent}</td>
                  <td>{formatRelative(s.started_at)}</td>
                  <td>
                    {s.finished_at
                      ? formatRelative(s.finished_at)
                      : <span class="muted">—</span>}
                  </td>
                  <td>{s.status || <span class="muted">—</span>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </>
  );
}
