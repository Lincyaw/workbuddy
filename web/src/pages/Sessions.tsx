import { useEffect, useMemo, useState } from 'preact/hooks';
import { useLocation } from 'preact-iso';
import { Layout } from '../components/Layout';
import {
  fetchSessions,
  type SessionListItem,
  type SessionListQuery,
} from '../api/sessions';
import { copyText, formatTimestamp, shortID, statusBadgeClass } from '../lib/format';
import {
  distinctValues,
  groupSessionsByIssue,
  type DecoratedSession,
} from '../utils/sessionGroups';

const PAGE_LIMIT = 50;

type ViewMode = 'grouped' | 'flat';
type SortMode = 'recent' | 'issue';

interface FilterState {
  repo: string;
  agent: string;
  issue: string;
  view: ViewMode;
  sort: SortMode;
  offset: number;
}

function parseQuery(query: Record<string, string>): FilterState {
  const view = query.view === 'flat' ? 'flat' : 'grouped';
  const sort = query.sort === 'issue' ? 'issue' : 'recent';
  return {
    repo: query.repo || '',
    agent: query.agent || '',
    issue: query.issue || '',
    view,
    sort,
    offset: parseInt(query.offset || '0', 10) || 0,
  };
}

function buildSearch(filter: FilterState): string {
  const sp = new URLSearchParams();
  if (filter.repo) sp.set('repo', filter.repo);
  if (filter.agent) sp.set('agent', filter.agent);
  if (filter.issue) sp.set('issue', filter.issue);
  if (filter.view !== 'grouped') sp.set('view', filter.view);
  if (filter.sort !== 'recent') sp.set('sort', filter.sort);
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
  const [form, setForm] = useState({
    repo: filter.repo,
    agent: filter.agent,
    issue: filter.issue,
  });
  const [collapsed, setCollapsed] = useState<Record<number, boolean>>({});

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
    navigate({ ...filter, ...form, offset: 0 });
  }

  function resetFilters() {
    setForm({ repo: '', agent: '', issue: '' });
    navigate({ ...filter, repo: '', agent: '', issue: '', offset: 0 });
  }

  function gotoOffset(offset: number) {
    navigate({ ...filter, offset });
  }

  function setView(view: ViewMode) {
    navigate({ ...filter, view });
  }

  function setSort(sort: SortMode) {
    navigate({ ...filter, sort });
  }

  function rowClick(id: string, e: MouseEvent) {
    if ((e.target as HTMLElement).closest('.id-copy')) return;
    location.route(`/sessions/${encodeURIComponent(id)}`);
  }

  function toggleGroup(issueNum: number) {
    setCollapsed((prev) => ({ ...prev, [issueNum]: !prev[issueNum] }));
  }

  const rows = sessions || [];
  const hasNext = rows.length === PAGE_LIMIT;
  const hasPrev = filter.offset > 0;
  const pageStart = filter.offset + 1;
  const pageEnd = filter.offset + rows.length;

  // Distinct values for the dropdown filters. We only see the current page,
  // so the dropdowns are an aid — the text input is preserved as fallback
  // for values that haven't surfaced on this page yet.
  const repoOptions = useMemo(() => distinctValues(rows, (s) => s.repo), [rows]);
  const agentOptions = useMemo(() => distinctValues(rows, (s) => s.agent_name), [rows]);

  const groups = useMemo(() => {
    const g = groupSessionsByIssue(rows);
    if (filter.sort === 'issue') {
      return [...g].sort((a, b) => b.issueNum - a.issueNum);
    }
    return g;
  }, [rows, filter.sort]);

  return (
    <Layout>
      <h1 style={{ fontSize: 22, marginBottom: 12 }}>Agent Sessions</h1>

      <form class="wb-filters" onSubmit={applyFilters}>
        <input
          list="wb-repo-options"
          type="text"
          placeholder="Repo (owner/name)"
          value={form.repo}
          onInput={(e) => setForm({ ...form, repo: (e.target as HTMLInputElement).value })}
          style={{ minWidth: 220 }}
        />
        <datalist id="wb-repo-options">
          {repoOptions.map((r) => (
            <option key={r} value={r} />
          ))}
        </datalist>
        <input
          list="wb-agent-options"
          type="text"
          placeholder="Agent name"
          value={form.agent}
          onInput={(e) => setForm({ ...form, agent: (e.target as HTMLInputElement).value })}
        />
        <datalist id="wb-agent-options">
          {agentOptions.map((a) => (
            <option key={a} value={a} />
          ))}
        </datalist>
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

        <span style={{ flex: 1 }} />

        <select
          value={filter.view}
          onChange={(e) => setView((e.target as HTMLSelectElement).value as ViewMode)}
          title="View mode"
        >
          <option value="grouped">Grouped by issue</option>
          <option value="flat">Flat list</option>
        </select>
        <select
          value={filter.sort}
          onChange={(e) => setSort((e.target as HTMLSelectElement).value as SortMode)}
          title="Sort"
        >
          <option value="recent">Most recent first</option>
          <option value="issue">Issue # (desc)</option>
        </select>
      </form>

      {error && <div class="wb-error">Failed to load sessions: {error}</div>}

      {loading && sessions === null ? (
        <div class="wb-loading">Loading sessions…</div>
      ) : rows.length === 0 ? (
        <div class="wb-empty">No sessions found.</div>
      ) : filter.view === 'flat' ? (
        <FlatTable rows={rows} onRowClick={rowClick} />
      ) : (
        groups.map((g) => {
          const isCollapsed = !!collapsed[g.issueNum];
          return (
            <div class="wb-session-group" key={g.issueNum}>
              <button
                type="button"
                class="wb-session-group-header"
                onClick={() => toggleGroup(g.issueNum)}
                aria-expanded={!isCollapsed}
              >
                <span class="caret">{isCollapsed ? '▶' : '▼'}</span>
                <span class="issue-num">#{g.issueNum}</span>
                <span class="repo">{g.repo}</span>
                <span class="count">{g.sessions.length} session{g.sessions.length === 1 ? '' : 's'}</span>
              </button>
              {!isCollapsed && (
                <GroupedTable rows={g.sessions} onRowClick={rowClick} />
              )}
            </div>
          );
        })
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

function IDCell({ id }: { id: string }) {
  return (
    <span class="id-cell">
      <code>{shortID(id, 16)}</code>
      <button
        type="button"
        class="id-copy"
        title="Copy full session ID"
        onClick={(e) => {
          e.stopPropagation();
          copyText(id);
        }}
      >
        copy
      </button>
    </span>
  );
}

function FlatTable({
  rows,
  onRowClick,
}: {
  rows: SessionListItem[];
  onRowClick: (id: string, e: MouseEvent) => void;
}) {
  return (
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
          <tr key={s.session_id} onClick={(e) => onRowClick(s.session_id, e as unknown as MouseEvent)}>
            <td><IDCell id={s.session_id} /></td>
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
  );
}

function GroupedTable({
  rows,
  onRowClick,
}: {
  rows: DecoratedSession[];
  onRowClick: (id: string, e: MouseEvent) => void;
}) {
  return (
    <table class="wb-table">
      <thead>
        <tr>
          <th>Session ID</th>
          <th>Role / Cycle</th>
          <th>Agent</th>
          <th>Status</th>
          <th>Created</th>
        </tr>
      </thead>
      <tbody>
        {rows.map((s) => (
          <tr key={s.session_id} onClick={(e) => onRowClick(s.session_id, e as unknown as MouseEvent)}>
            <td><IDCell id={s.session_id} /></td>
            <td>
              <span class={`wb-role-badge wb-role-${s.role}`}>{s.role}</span>
              <span class="wb-cycle">cycle {s.cycle}</span>
            </td>
            <td>{s.agent_name}</td>
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
  );
}

export default Sessions;
