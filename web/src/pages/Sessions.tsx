import { useEffect, useMemo, useState } from 'preact/hooks';
import { useLocation } from 'preact-iso';
import { Layout } from '../components/Layout';
import {
  fetchSessions,
  type SessionListItem,
  type SessionListQuery,
} from '../api/sessions';
import { copyText, formatTimestamp, shortID, statusBadgeClass } from '../lib/format';

const PAGE_LIMIT = 50;

interface FilterState {
  repo: string;
  agent: string;
  issue: string;
  offset: number;
}

function parseQuery(query: Record<string, string>): FilterState {
  return {
    repo: query.repo || '',
    agent: query.agent || '',
    issue: query.issue || '',
    offset: parseInt(query.offset || '0', 10) || 0,
  };
}

function buildSearch(filter: FilterState): string {
  const sp = new URLSearchParams();
  if (filter.repo) sp.set('repo', filter.repo);
  if (filter.agent) sp.set('agent', filter.agent);
  if (filter.issue) sp.set('issue', filter.issue);
  if (filter.offset > 0) sp.set('offset', String(filter.offset));
  const s = sp.toString();
  return s ? `?${s}` : '';
}

export function Sessions() {
  const location = useLocation();
  const filter = useMemo(() => parseQuery(location.query), [location.query]);
  const [sessions, setSessions] = useState<SessionListItem[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  // Local form state — mirrors filter but applies on submit, not on each keystroke.
  const [form, setForm] = useState({
    repo: filter.repo,
    agent: filter.agent,
    issue: filter.issue,
  });

  useEffect(() => {
    setForm({ repo: filter.repo, agent: filter.agent, issue: filter.issue });
  }, [filter.repo, filter.agent, filter.issue]);

  useEffect(() => {
    let aborted = false;
    setLoading(true);
    setError(null);
    const q: SessionListQuery = {
      repo: filter.repo || undefined,
      agent: filter.agent || undefined,
      issue: filter.issue || undefined,
      limit: PAGE_LIMIT,
      offset: filter.offset,
    };
    fetchSessions(q)
      .then((items) => {
        if (aborted) return;
        setSessions(items || []);
      })
      .catch((err) => {
        if (aborted) return;
        setError(err?.message || 'request failed');
        setSessions([]);
      })
      .finally(() => {
        if (!aborted) setLoading(false);
      });
    return () => {
      aborted = true;
    };
  }, [filter.repo, filter.agent, filter.issue, filter.offset]);

  function navigate(next: FilterState) {
    location.route('/sessions' + buildSearch(next));
  }

  function applyFilters(e: Event) {
    e.preventDefault();
    navigate({ ...form, offset: 0 });
  }

  function resetFilters() {
    setForm({ repo: '', agent: '', issue: '' });
    navigate({ repo: '', agent: '', issue: '', offset: 0 });
  }

  function gotoOffset(offset: number) {
    navigate({ ...filter, offset });
  }

  function rowClick(id: string, e: MouseEvent) {
    if ((e.target as HTMLElement).closest('.id-copy')) return;
    location.route(`/sessions/${encodeURIComponent(id)}`);
  }

  const rows = sessions || [];
  const hasNext = rows.length === PAGE_LIMIT;
  const hasPrev = filter.offset > 0;
  const pageStart = filter.offset + 1;
  const pageEnd = filter.offset + rows.length;

  return (
    <Layout current="sessions">
      <h1 style={{ fontSize: 22, marginBottom: 12 }}>Agent Sessions</h1>

      <form class="wb-filters" onSubmit={applyFilters}>
        <input
          type="text"
          placeholder="Repo (owner/name)"
          value={form.repo}
          onInput={(e) => setForm({ ...form, repo: (e.target as HTMLInputElement).value })}
          style={{ minWidth: 220 }}
        />
        <input
          type="text"
          placeholder="Agent name"
          value={form.agent}
          onInput={(e) => setForm({ ...form, agent: (e.target as HTMLInputElement).value })}
        />
        <input
          type="text"
          placeholder="Issue #"
          value={form.issue}
          onInput={(e) => setForm({ ...form, issue: (e.target as HTMLInputElement).value })}
          style={{ width: 90 }}
        />
        <button type="submit" class="primary">Filter</button>
        {(filter.repo || filter.agent || filter.issue) && (
          <button type="button" onClick={resetFilters}>Reset</button>
        )}
      </form>

      {error && <div class="wb-error">Failed to load sessions: {error}</div>}

      {loading && sessions === null ? (
        <div class="wb-loading">Loading sessions…</div>
      ) : rows.length === 0 ? (
        <div class="wb-empty">No sessions found.</div>
      ) : (
        <table class="wb-table">
          <thead>
            <tr>
              <th>Session ID</th>
              <th>Agent</th>
              <th>Repo</th>
              <th>Issue</th>
              <th>Status</th>
              <th>Created</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((s) => (
              <tr
                key={s.session_id}
                onClick={(e) => rowClick(s.session_id, e as unknown as MouseEvent)}
              >
                <td>
                  <span class="id-cell">
                    <code>{shortID(s.session_id, 16)}</code>
                    <button
                      type="button"
                      class="id-copy"
                      title="Copy full session ID"
                      onClick={(e) => {
                        e.stopPropagation();
                        copyText(s.session_id);
                      }}
                    >
                      copy
                    </button>
                  </span>
                </td>
                <td>{s.agent_name}</td>
                <td>{s.repo}</td>
                <td>#{s.issue_num}</td>
                <td>
                  <span class={statusBadgeClass(s.task_status || s.status)}>
                    {s.task_status || s.status || 'unknown'}
                  </span>
                </td>
                <td>{formatTimestamp(s.created_at)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <div class="wb-pager">
        <button type="button" disabled={!hasPrev} onClick={() => gotoOffset(Math.max(0, filter.offset - PAGE_LIMIT))}>
          Prev
        </button>
        <button type="button" disabled={!hasNext} onClick={() => gotoOffset(filter.offset + PAGE_LIMIT)}>
          Next
        </button>
        {rows.length > 0 && (
          <span>
            showing {pageStart}–{pageEnd}
          </span>
        )}
      </div>
    </Layout>
  );
}

export default Sessions;
