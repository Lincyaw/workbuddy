import { useEffect, useMemo, useState } from 'preact/hooks';
import { useLocation } from 'preact-iso';
import { Layout } from '../components/Layout';
import { EmptyState } from '../components/EmptyState';
import { fetchSessions, type SessionListItem, type SessionListQuery } from '../api/sessions';
import { copyText, formatTimestamp, shortID } from '../lib/format';
import { distinctValues, groupSessionsByIssue, type DecoratedSession } from '../utils/sessionGroups';
import { SessionStatusBadge } from '../components/DegradedSessionCard';

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
  const search = sp.toString();
  return search ? `?${search}` : '';
}

export function Sessions() {
  const location = useLocation();
  const filter = useMemo(() => parseQuery(location.query), [location.query]);
  const [sessions, setSessions] = useState<SessionListItem[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [form, setForm] = useState({ repo: filter.repo, agent: filter.agent, issue: filter.issue });
  const [collapsed, setCollapsed] = useState<Record<number, boolean>>({});

  useEffect(() => {
    setForm({ repo: filter.repo, agent: filter.agent, issue: filter.issue });
  }, [filter.repo, filter.agent, filter.issue]);

  useEffect(() => {
    let aborted = false;
    setLoading(true);
    setError(null);
    const query: SessionListQuery = {
      repo: filter.repo || undefined,
      agent: filter.agent || undefined,
      issue: filter.issue || undefined,
      limit: PAGE_LIMIT,
      offset: filter.offset,
    };
    fetchSessions(query)
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
  const repoOptions = useMemo(() => distinctValues(rows, (session) => session.repo), [rows]);
  const agentOptions = useMemo(() => distinctValues(rows, (session) => session.agent_name), [rows]);
  const groups = useMemo(() => {
    const grouped = groupSessionsByIssue(rows);
    if (filter.sort === 'issue') return [...grouped].sort((left, right) => right.issueNum - left.issueNum);
    return grouped;
  }, [rows, filter.sort]);

  return (
    <Layout>
      <div class="wb-page-header wb-page-header--tight">
        <div>
          <p class="wb-eyebrow">Runtime</p>
          <h1 class="wb-page-title">Sessions</h1>
          <p class="wb-page-subtitle">Filter by repo, agent, or issue and switch between grouped and flat views without leaving the page.</p>
        </div>
      </div>

      <form class="wb-filters" onSubmit={applyFilters}>
        <div class="wb-filter-inputs">
          <input
            list="wb-repo-options"
            type="text"
            placeholder="Repo (owner/name)"
            value={form.repo}
            onInput={(e) => setForm({ ...form, repo: (e.target as HTMLInputElement).value })}
          />
          <datalist id="wb-repo-options">
            {repoOptions.map((repo) => <option key={repo} value={repo} />)}
          </datalist>
          <input
            list="wb-agent-options"
            type="text"
            placeholder="Agent name"
            value={form.agent}
            onInput={(e) => setForm({ ...form, agent: (e.target as HTMLInputElement).value })}
          />
          <datalist id="wb-agent-options">
            {agentOptions.map((agent) => <option key={agent} value={agent} />)}
          </datalist>
          <input
            type="text"
            placeholder="Issue #"
            value={form.issue}
            onInput={(e) => setForm({ ...form, issue: (e.target as HTMLInputElement).value })}
          />
        </div>

        <div class="wb-filter-actions">
          <button type="submit" class="wb-button wb-button--primary">Apply filters</button>
          {(filter.repo || filter.agent || filter.issue) && (
            <button type="button" class="wb-button wb-button--ghost" onClick={resetFilters}>Reset</button>
          )}
        </div>

        <div class="wb-filter-chips" aria-label="View options">
          <button type="button" class={`wb-filter-chip${filter.view === 'grouped' ? ' is-active' : ''}`} onClick={() => setView('grouped')}>
            Grouped by issue
          </button>
          <button type="button" class={`wb-filter-chip${filter.view === 'flat' ? ' is-active' : ''}`} onClick={() => setView('flat')}>
            Flat list
          </button>
          <button type="button" class={`wb-filter-chip${filter.sort === 'recent' ? ' is-active' : ''}`} onClick={() => setSort('recent')}>
            Most recent
          </button>
          <button type="button" class={`wb-filter-chip${filter.sort === 'issue' ? ' is-active' : ''}`} onClick={() => setSort('issue')}>
            Issue number
          </button>
        </div>
      </form>

      {error && <div class="wb-alert wb-alert--danger">Failed to load sessions: {error}</div>}

      {loading && sessions === null ? (
        <EmptyState icon=".." title="Loading sessions" copy="Fetching the latest session page for the selected filters." />
      ) : rows.length === 0 ? (
        <EmptyState
          icon="[]"
          title="No sessions match these filters"
          copy="Try clearing a filter, switching to the flat view, or queueing a new issue to generate session history."
        />
      ) : filter.view === 'flat' ? (
        <FlatTable rows={rows} onRowClick={rowClick} />
      ) : (
        <div class="wb-stack-md">
          {groups.map((group) => {
            const isCollapsed = !!collapsed[group.issueNum];
            return (
              <div class="wb-card wb-card--flush" key={group.issueNum}>
                <button
                  type="button"
                  class="wb-session-group-header"
                  onClick={() => toggleGroup(group.issueNum)}
                  aria-expanded={!isCollapsed}
                >
                  <span class="wb-session-group-header__caret">{isCollapsed ? '>' : 'v'}</span>
                  <span class="wb-session-group-header__title">#{group.issueNum}</span>
                  <span class="wb-session-group-header__repo">{group.repo}</span>
                  <span class="wb-session-group-header__count">{group.sessions.length} session{group.sessions.length === 1 ? '' : 's'}</span>
                </button>
                {!isCollapsed && <GroupedTable rows={group.sessions} onRowClick={rowClick} />}
              </div>
            );
          })}
        </div>
      )}

      <div class="wb-pager">
        <button type="button" class="wb-button wb-button--ghost" disabled={!hasPrev} onClick={() => gotoOffset(Math.max(0, filter.offset - PAGE_LIMIT))}>Prev</button>
        <button type="button" class="wb-button wb-button--ghost" disabled={!hasNext} onClick={() => gotoOffset(filter.offset + PAGE_LIMIT)}>Next</button>
        {rows.length > 0 && <span class="wb-muted">showing {pageStart}-{pageEnd}</span>}
      </div>
    </Layout>
  );
}

function IDCell({ id }: { id: string }) {
  return (
    <span class="wb-id-cell">
      <code class="wb-code-inline">{shortID(id, 16)}</code>
      <button
        type="button"
        class="wb-button wb-button--ghost wb-button--mini id-copy"
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

function FlatTable({ rows, onRowClick }: { rows: SessionListItem[]; onRowClick: (id: string, e: MouseEvent) => void; }) {
  return (
    <>
      <div class="wb-table-card wb-desktop-only">
        <div class="wb-table-scroll">
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
              {rows.map((session) => (
                <tr key={session.session_id} class="wb-row-link" onClick={(e) => onRowClick(session.session_id, e as unknown as MouseEvent)}>
                  <td><IDCell id={session.session_id} /></td>
                  <td>{session.agent_name}</td>
                  <td>{session.repo}</td>
                  <td class="wb-num">#{session.issue_num}</td>
                  <td><SessionStatusBadge row={session} /></td>
                  <td class="wb-time">{formatTimestamp(session.created_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>
      <div class="wb-mobile-list wb-mobile-only">
        {rows.map((session) => (
          <button type="button" class="wb-mobile-card wb-mobile-card--interactive" key={session.session_id} onClick={(e) => onRowClick(session.session_id, e as unknown as MouseEvent)}>
            <div class="wb-mobile-card__header">
              <span class="wb-code-pill">{shortID(session.session_id, 16)}</span>
              <SessionStatusBadge row={session} />
            </div>
            <div class="wb-mobile-card__grid">
              <span>Agent</span>
              <strong>{session.agent_name}</strong>
              <span>Repo</span>
              <strong>{session.repo}</strong>
              <span>Issue</span>
              <strong class="wb-num">#{session.issue_num}</strong>
              <span>Created</span>
              <strong class="wb-time">{formatTimestamp(session.created_at)}</strong>
            </div>
          </button>
        ))}
      </div>
    </>
  );
}

function GroupedTable({ rows, onRowClick }: { rows: DecoratedSession[]; onRowClick: (id: string, e: MouseEvent) => void; }) {
  return (
    <>
      <div class="wb-table-scroll wb-desktop-only">
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
            {rows.map((session) => (
              <tr key={session.session_id} class="wb-row-link" onClick={(e) => onRowClick(session.session_id, e as unknown as MouseEvent)}>
                <td><IDCell id={session.session_id} /></td>
                <td>
                  <span class={`wb-role-pill wb-role-pill--${session.role}`}>{session.role}</span>
                  <span class="wb-muted wb-num">cycle {session.cycle}</span>
                </td>
                <td>{session.agent_name}</td>
                <td><SessionStatusBadge row={session} /></td>
                <td class="wb-time">{formatTimestamp(session.created_at)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <div class="wb-mobile-list wb-mobile-only">
        {rows.map((session) => (
          <button type="button" class="wb-mobile-card wb-mobile-card--interactive" key={session.session_id} onClick={(e) => onRowClick(session.session_id, e as unknown as MouseEvent)}>
            <div class="wb-mobile-card__header">
              <span class="wb-code-pill">{shortID(session.session_id, 16)}</span>
              <SessionStatusBadge row={session} />
            </div>
            <div class="wb-mobile-card__grid">
              <span>Role</span>
              <strong>
                <span class={`wb-role-pill wb-role-pill--${session.role}`}>{session.role}</span>
                <span class="wb-muted wb-num"> cycle {session.cycle}</span>
              </strong>
              <span>Agent</span>
              <strong>{session.agent_name}</strong>
              <span>Created</span>
              <strong class="wb-time">{formatTimestamp(session.created_at)}</strong>
            </div>
          </button>
        ))}
      </div>
    </>
  );
}

export default Sessions;
