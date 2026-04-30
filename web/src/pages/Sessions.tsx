import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import { useLocation } from 'preact-iso';
import { fetchSessions, type SessionListItem, type SessionListQuery } from '../api/sessions';
import { EmptyState } from '../components/EmptyState';
import { Layout } from '../components/Layout';
import { SessionStatusBadge } from '../components/DegradedSessionCard';
import { copyText, formatTimestamp, shortID } from '../lib/format';

const PAGE_LIMIT = 50;
const POLL_INTERVAL_MS = 5_000;

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
  const search = sp.toString();
  return search ? `?${search}` : '';
}

export function Sessions() {
  const location = useLocation();
  const filter = useMemo(() => parseQuery(location.query), [location.query]);
  const [rows, setRows] = useState<SessionListItem[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [live, setLive] = useState(false);
  const [flashIds, setFlashIds] = useState<Record<string, boolean>>({});
  const [form, setForm] = useState({ repo: filter.repo, agent: filter.agent, issue: filter.issue });
  const previousStatus = useRef<Record<string, string>>({});

  useEffect(() => {
    setForm({ repo: filter.repo, agent: filter.agent, issue: filter.issue });
  }, [filter.repo, filter.agent, filter.issue]);

  useEffect(() => {
    let cancelled = false;
    const timeouts: number[] = [];

    const load = async () => {
      setLoading((value) => (rows.length === 0 ? true : value));
      setError(null);
      try {
        const query: SessionListQuery = {
          repo: filter.repo || undefined,
          agent: filter.agent || undefined,
          issue: filter.issue || undefined,
          limit: PAGE_LIMIT,
          offset: filter.offset,
        };
        const next = await fetchSessions(query);
        if (cancelled) return;
        const changed: Record<string, boolean> = {};
        for (const row of next) {
          const prev = previousStatus.current[row.session_id];
          const current = row.task_status || row.status || '';
          if (prev && prev !== current) {
            changed[row.session_id] = true;
            const timeout = window.setTimeout(() => {
              setFlashIds((value) => {
                const copy = { ...value };
                delete copy[row.session_id];
                return copy;
              });
            }, 400);
            timeouts.push(timeout);
          }
          previousStatus.current[row.session_id] = current;
        }
        setFlashIds((value) => ({ ...value, ...changed }));
        setRows(next);
        setLoading(false);
        setLive(true);
      } catch (err) {
        if (cancelled) return;
        setError(err instanceof Error ? err.message : 'request failed');
        setRows([]);
        setLoading(false);
        setLive(false);
      }
    };

    void load();
    const timer = window.setInterval(() => void load(), POLL_INTERVAL_MS);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
      for (const timeout of timeouts) window.clearTimeout(timeout);
    };
  }, [filter.repo, filter.agent, filter.issue, filter.offset]);

  function navigate(next: FilterState) {
    location.route('/sessions' + buildSearch(next));
  }

  function applyFilters(event: Event) {
    event.preventDefault();
    navigate({ ...filter, ...form, offset: 0 });
  }

  const hasNext = rows.length === PAGE_LIMIT;
  const hasPrev = filter.offset > 0;

  return (
    <Layout>
      <section class="wb-stack">
        <header class="grid gap-3">
          <p class="wb-section-label">session lane</p>
          <div class="flex flex-wrap items-center justify-between gap-3">
            <div>
              <div class="flex items-center gap-3">
                <h1 class="wb-page-title">sessions</h1>
                <span class="wb-live-indicator">
                  <span class={`wb-live-dot${live ? ' is-live' : ''}`} aria-hidden="true" />
                  {live ? 'stream connected' : 'polling'}
                </span>
              </div>
              <p class="wb-page-copy">Compact rows for time, repo, issue, agent, worker, runtime, and status while the queue is moving.</p>
            </div>
          </div>
        </header>

        <form class="wb-panel wb-filter-row" onSubmit={applyFilters}>
          <div class="grid gap-3 md:grid-cols-[minmax(0,1.1fr)_minmax(0,1fr)_120px_auto]">
            <input type="text" placeholder="repo (owner/name)" value={form.repo} onInput={(event) => setForm({ ...form, repo: (event.target as HTMLInputElement).value })} />
            <input type="text" placeholder="agent" value={form.agent} onInput={(event) => setForm({ ...form, agent: (event.target as HTMLInputElement).value })} />
            <input type="text" placeholder="issue #" value={form.issue} onInput={(event) => setForm({ ...form, issue: (event.target as HTMLInputElement).value })} />
            <div class="flex flex-wrap gap-2">
              <button type="submit" class="wb-cta wb-cta--primary">apply</button>
              <button type="button" class="wb-cta wb-cta--ghost" onClick={() => navigate({ repo: '', agent: '', issue: '', offset: 0 })}>reset</button>
            </div>
          </div>
          <div class="flex flex-wrap gap-2">
            {filter.repo ? <span class="wb-filter-pill is-active">repo</span> : null}
            {filter.agent ? <span class="wb-filter-pill is-active">agent</span> : null}
            {filter.issue ? <span class="wb-filter-pill is-active">issue</span> : null}
            {!filter.repo && !filter.agent && !filter.issue ? <span class="wb-filter-pill">all sessions</span> : null}
          </div>
        </form>

        {error ? <div class="wb-panel text-state-danger">Failed to load sessions: {error}</div> : null}

        {loading && rows.length === 0 ? (
          <EmptyState glyph="loading" title="loading sessions" copy="Reading the latest session page for the active filters." />
        ) : rows.length === 0 ? (
          <EmptyState
            glyph="sessions"
            title="no sessions yet"
            copy="open an issue with `status:developing` and the coordinator will dispatch one."
          />
        ) : (
          <>
            <section class="wb-table-shell">
              <div class="wb-table-wrap">
                <table class="wb-table">
                  <thead>
                    <tr>
                      <th>time</th>
                      <th>repo + issue</th>
                      <th>agent</th>
                      <th>worker</th>
                      <th>runtime</th>
                      <th>status</th>
                      <th>duration</th>
                    </tr>
                  </thead>
                  <tbody>
                    {rows.map((row) => (
                      <tr key={row.session_id} onClick={() => location.route(`/sessions/${encodeURIComponent(row.session_id)}`)}>
                        <td class="wb-time">{formatTimestamp(row.created_at)}</td>
                        <td>
                          <div class="grid gap-2">
                            <span class="wb-id-pill">{row.repo}#{row.issue_num}</span>
                            <div class="flex items-center gap-2 text-[12px] text-text-secondary">
                              <span class="wb-code-inline">{shortID(row.session_id, 12)}</span>
                              <button type="button" class="wb-faint" onClick={(event) => {
                                event.stopPropagation();
                                copyText(row.session_id);
                              }}>copy</button>
                            </div>
                          </div>
                        </td>
                        <td>{row.agent_name}</td>
                        <td>{row.worker_id ? <span class="wb-id-pill">{row.worker_id}</span> : <span class="wb-faint">--</span>}</td>
                        <td class="wb-mono text-[12px] uppercase text-text-secondary">{row.runtime || 'unknown'}</td>
                        <td>
                          <div class={flashIds[row.session_id] ? 'wb-status-flash inline-flex' : 'inline-flex'}>
                            <SessionStatusBadge row={row} />
                          </div>
                        </td>
                        <td class="wb-time">{Math.round(row.duration / 1000)}s</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
              <div class="wb-mobile-card-list p-4">
                {rows.map((row) => (
                  <button type="button" class="wb-mobile-card text-left" key={row.session_id} onClick={() => location.route(`/sessions/${encodeURIComponent(row.session_id)}`)}>
                    <div class="flex items-start justify-between gap-3">
                      <div class="grid gap-2">
                        <span class="wb-id-pill">{row.repo}#{row.issue_num}</span>
                        <strong>{row.agent_name}</strong>
                      </div>
                      <div class={flashIds[row.session_id] ? 'wb-status-flash inline-flex' : 'inline-flex'}>
                        <SessionStatusBadge row={row} />
                      </div>
                    </div>
                    <div class="mt-3 grid gap-1 text-[12px] text-text-secondary">
                      <span>{formatTimestamp(row.created_at)}</span>
                      <span>worker: {row.worker_id || '--'} · runtime: {row.runtime || 'unknown'}</span>
                      <span>duration: {Math.round(row.duration / 1000)}s</span>
                    </div>
                  </button>
                ))}
              </div>
            </section>

            <div class="flex items-center justify-between gap-3">
              <button type="button" class="wb-cta wb-cta--ghost" disabled={!hasPrev} onClick={() => navigate({ ...filter, offset: Math.max(0, filter.offset - PAGE_LIMIT) })}>prev</button>
              <span class="wb-faint">showing {filter.offset + 1}-{filter.offset + rows.length}</span>
              <button type="button" class="wb-cta wb-cta--ghost" disabled={!hasNext} onClick={() => navigate({ ...filter, offset: filter.offset + PAGE_LIMIT })}>next</button>
            </div>
          </>
        )}
      </section>
    </Layout>
  );
}

export default Sessions;
