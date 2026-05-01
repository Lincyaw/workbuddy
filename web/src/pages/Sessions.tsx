import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import { useLocation } from 'preact-iso';
import { Layout } from '../components/Layout';
import { EmptyState } from '../components/EmptyState';
import { fetchSessions, type SessionListItem, type SessionListQuery } from '../api/sessions';
import { getIssueRollouts } from '../api/client';
import { copyText, formatClock, formatDuration, formatTimestamp, shortID } from '../lib/format';
import { SessionStatusBadge } from '../components/DegradedSessionCard';
import { RolloutGroupPanel } from '../components/RolloutGroupPanel';
import type { RolloutGroup } from '../api/types';

const PAGE_LIMIT = 50;
const REFRESH_INTERVAL_MS = 20_000;
const FLASH_MS = 400;
const OPTIONS_LIMIT = 200;

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

function uniqueSorted(values: string[]): string[] {
  return Array.from(new Set(values.filter((v) => v && v.length > 0))).sort();
}

interface IssueGroup {
  key: string;
  repo: string;
  issueNum: number;
  rows: SessionListItem[];
  latestCreatedAt: string;
}

function groupByIssue(rows: SessionListItem[]): IssueGroup[] {
  const map = new Map<string, IssueGroup>();
  for (const row of rows) {
    const key = `${row.repo}#${row.issue_num}`;
    let group = map.get(key);
    if (!group) {
      group = {
        key,
        repo: row.repo,
        issueNum: row.issue_num,
        rows: [],
        latestCreatedAt: row.created_at || '',
      };
      map.set(key, group);
    }
    group.rows.push(row);
    if ((row.created_at || '') > group.latestCreatedAt) {
      group.latestCreatedAt = row.created_at || '';
    }
  }
  return Array.from(map.values()).sort((a, b) =>
    b.latestCreatedAt.localeCompare(a.latestCreatedAt),
  );
}

export function Sessions() {
  const location = useLocation();
  const filter = useMemo(() => parseQuery(location.query), [location.query]);
  const [sessions, setSessions] = useState<SessionListItem[]>([]);
  const [offlineWorkers, setOfflineWorkers] = useState<string[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [liveConnected, setLiveConnected] = useState(false);
  const [flashMap, setFlashMap] = useState<Record<string, boolean>>({});
  const [rolloutPanels, setRolloutPanels] = useState<Array<{ key: string; repo: string; issueNum: number; group: RolloutGroup }>>([]);
  const [form, setForm] = useState({ repo: filter.repo, agent: filter.agent, issue: filter.issue });
  const [optionsSource, setOptionsSource] = useState<SessionListItem[]>([]);
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>({});
  const previousRows = useRef<Record<string, SessionListItem>>({});

  useEffect(() => {
    setForm({ repo: filter.repo, agent: filter.agent, issue: filter.issue });
  }, [filter.repo, filter.agent, filter.issue]);

  useEffect(() => {
    let cancelled = false;
    async function loadOptions() {
      try {
        const result = await fetchSessions({ limit: OPTIONS_LIMIT });
        if (!cancelled) setOptionsSource(result.rows || []);
      } catch {
        // dropdown options are best-effort; do not block the page on failure
      }
    }
    void loadOptions();
    const timer = window.setInterval(() => {
      if (!cancelled) void loadOptions();
    }, 60_000);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
  }, []);

  const repoOptions = useMemo(
    () => uniqueSorted(optionsSource.map((s) => s.repo)),
    [optionsSource],
  );
  const agentOptions = useMemo(
    () => uniqueSorted(optionsSource.map((s) => s.agent_name)),
    [optionsSource],
  );
  const issueOptions = useMemo(() => {
    const filtered = form.repo
      ? optionsSource.filter((s) => s.repo === form.repo)
      : optionsSource;
    const numbers = Array.from(
      new Set(filtered.map((s) => s.issue_num).filter((n) => Number.isFinite(n))),
    );
    return numbers.sort((a, b) => b - a).map(String);
  }, [optionsSource, form.repo]);

  async function load(reason: 'poll' | 'sse' = 'poll') {
    const query: SessionListQuery = {
      repo: filter.repo || undefined,
      agent: filter.agent || undefined,
      issue: filter.issue || undefined,
      limit: PAGE_LIMIT,
      offset: filter.offset,
    };
    try {
      const result = await fetchSessions(query);
      const nextRows = result.rows || [];
      setOfflineWorkers(result.offlineWorkers || []);
      if (reason === 'sse') {
        const flashing = nextRows
          .filter((row) => previousRows.current[row.session_id]?.status !== row.status)
          .map((row) => row.session_id);
        if (flashing.length > 0) {
          setFlashMap((prev) => {
            const next = { ...prev };
            for (const id of flashing) next[id] = true;
            return next;
          });
          window.setTimeout(() => {
            setFlashMap((prev) => {
              const next = { ...prev };
              for (const id of flashing) delete next[id];
              return next;
            });
          }, FLASH_MS);
        }
      }
      previousRows.current = Object.fromEntries(nextRows.map((row) => [row.session_id, row]));
      const rolloutIssueKeys = [...new Set(
        nextRows
          .filter((row) => (row.rollout_group_id || '').trim() !== '' && (row.rollouts_total || 0) > 1)
          .map((row) => `${row.repo}#${row.issue_num}`),
      )];
      const panels = await Promise.all(
        rolloutIssueKeys.map(async (key) => {
          const [repo, issueStr] = key.split('#');
          const issueNum = Number(issueStr || '0');
          const { owner, name } = splitRepo(repo);
          try {
            const group = await getIssueRollouts(owner, name, issueNum);
            return { key, repo, issueNum, group };
          } catch {
            return null;
          }
        }),
      );
      setSessions(nextRows);
      setRolloutPanels(
        panels.filter((panel): panel is { key: string; repo: string; issueNum: number; group: RolloutGroup } => panel !== null && panel.group.members.length > 1),
      );
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'request failed');
      setSessions([]);
      setOfflineWorkers([]);
      setRolloutPanels([]);
    } finally {
      setLoading(false);
    }
  }

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    void load('poll');
    const timer = window.setInterval(() => {
      if (!cancelled) void load('poll');
    }, REFRESH_INTERVAL_MS);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [filter.repo, filter.agent, filter.issue, filter.offset]);

  useEffect(() => {
    let stopped = false;
    let source: EventSource | null = null;
    let retryTimer = 0;

    const connect = () => {
      if (stopped) return;
      const params = new URLSearchParams();
      if (filter.repo) params.set('repo', filter.repo);
      if (filter.issue) params.set('issue', filter.issue);
      source = new EventSource(`/tasks/watch${params.toString() ? `?${params.toString()}` : ''}`);
      source.onopen = () => setLiveConnected(true);
      source.addEventListener('task_complete', () => {
        setLiveConnected(true);
        void load('sse');
        source?.close();
        connect();
      });
      source.onerror = () => {
        setLiveConnected(false);
        source?.close();
        retryTimer = window.setTimeout(connect, 4_000);
      };
    };

    connect();
    return () => {
      stopped = true;
      setLiveConnected(false);
      if (source) source.close();
      window.clearTimeout(retryTimer);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [filter.repo, filter.issue]);

  function navigate(next: FilterState) {
    location.route('/sessions' + buildSearch(next));
  }

  function applyFilters(event: Event) {
    event.preventDefault();
    navigate({ ...filter, ...form, offset: 0 });
  }

  function resetFilters() {
    setForm({ repo: '', agent: '', issue: '' });
    navigate({ repo: '', agent: '', issue: '', offset: 0 });
  }

  const groups = useMemo(() => groupByIssue(sessions), [sessions]);
  const hasNext = sessions.length === PAGE_LIMIT;
  const hasPrev = filter.offset > 0;

  return (
    <Layout>
      <section class="page-header">
        <div>
          <p class="page-eyebrow">session telemetry</p>
          <h1>
            Sessions
            <span class={`live-pill${liveConnected ? ' is-live' : ''}`}>
              <span class="live-pill-dot" />
              {liveConnected ? 'live' : 'reconnecting'}
            </span>
          </h1>
        </div>
      </section>

      <form class="filter-toolbar" onSubmit={applyFilters}>
        <select
          value={form.repo}
          onChange={(event) => {
            const repo = (event.target as HTMLSelectElement).value;
            setForm((prev) => ({ ...prev, repo, issue: '' }));
          }}
        >
          <option value="">all repos</option>
          {repoOptions.map((repo) => (
            <option key={repo} value={repo}>
              {repo}
            </option>
          ))}
        </select>
        <select
          value={form.agent}
          onChange={(event) =>
            setForm((prev) => ({ ...prev, agent: (event.target as HTMLSelectElement).value }))
          }
        >
          <option value="">all agents</option>
          {agentOptions.map((agent) => (
            <option key={agent} value={agent}>
              {agent}
            </option>
          ))}
        </select>
        <select
          value={form.issue}
          onChange={(event) =>
            setForm((prev) => ({ ...prev, issue: (event.target as HTMLSelectElement).value }))
          }
        >
          <option value="">all issues</option>
          {issueOptions.map((num) => (
            <option key={num} value={num}>
              #{num}
            </option>
          ))}
        </select>
        <button type="submit" class="wb-button wb-button-primary">Filter</button>
        <button type="button" class="wb-button" onClick={resetFilters}>Reset</button>
      </form>

      <div class="active-pills">
        <span class={`filter-pill${filter.repo ? ' active' : ''}`}>{filter.repo || 'all repos'}</span>
        <span class={`filter-pill${filter.agent ? ' active' : ''}`}>{filter.agent || 'all agents'}</span>
        <span class={`filter-pill${filter.issue ? ' active' : ''}`}>{filter.issue ? `issue #${filter.issue}` : 'all issues'}</span>
      </div>

      {error ? <div class="error-banner">Failed to load sessions: {error}</div> : null}

      {offlineWorkers.length > 0 ? (
        <div class="info-banner wb-offline-banner" role="status">
          {offlineWorkers.length === 1
            ? `1 worker offline: ${offlineWorkers[0]}`
            : `${offlineWorkers.length} workers offline: ${offlineWorkers.join(', ')}`}
          {' — sessions on these workers may be temporarily missing from the list.'}
        </div>
      ) : null}

      {rolloutPanels.map((panel) => (
        <RolloutGroupPanel
          key={panel.key}
          repo={panel.repo}
          issueNum={panel.issueNum}
          group={panel.group}
          compareHref={buildCompareHref(panel)}
          title="Rollout parent issue"
        />
      ))}

      {loading && sessions.length === 0 ? (
        <section class="surface-card table-card">
          <div class="loading-copy">Loading session lanes…</div>
        </section>
      ) : sessions.length === 0 ? (
        <section class="surface-card table-card">
          <EmptyState
            title="no sessions yet"
            detail="open an issue with status:developing and the coordinator will dispatch one."
            ctaHref="/dashboard"
            ctaLabel="back to dashboard →"
            inline
          />
        </section>
      ) : (
        <div class="issue-group-stack">
          {groups.map((group) => {
            const isCollapsed = collapsed[group.key] === true;
            const latest = group.rows[0];
            return (
              <section class="surface-card issue-group-card" key={group.key}>
                <header class="issue-group-header">
                  <button
                    type="button"
                    class="issue-group-toggle"
                    aria-expanded={!isCollapsed}
                    onClick={() =>
                      setCollapsed((prev) => ({ ...prev, [group.key]: !isCollapsed }))
                    }
                  >
                    <span class="issue-group-caret" aria-hidden="true">
                      {isCollapsed ? '▸' : '▾'}
                    </span>
                    <span class="mono-link issue-link">
                      {group.repo}#{group.issueNum}
                    </span>
                    <span class="issue-group-count">
                      {group.rows.length} session{group.rows.length === 1 ? '' : 's'}
                    </span>
                  </button>
                  <span class="issue-group-meta">
                    {latest ? (
                      <>
                        <SessionStatusBadge row={latest} />
                        <span class="table-time-detail">{formatTimestamp(latest.created_at)}</span>
                      </>
                    ) : null}
                  </span>
                </header>
                {isCollapsed ? null : (
                  <div class="table-shell">
                    <table class="mission-table sessions-table">
                      <thead>
                        <tr>
                          <th>Time</th>
                          <th>Session</th>
                          <th>Agent</th>
                          <th>Worker</th>
                          <th>Runtime</th>
                          <th>Status</th>
                          <th>Duration</th>
                        </tr>
                      </thead>
                      <tbody>
                        {group.rows.map((row) => (
                          <tr key={row.session_id}>
                            <td data-label="Time">
                              <a
                                href={`/sessions/${encodeURIComponent(row.session_id)}`}
                                class="mono-link"
                              >
                                {formatClock(row.created_at)}
                              </a>
                              <div class="table-time-detail">
                                {formatTimestamp(row.created_at)}
                              </div>
                            </td>
                            <td data-label="Session">
                              <div class="issue-id-cell">
                                <a
                                  href={`/sessions/${encodeURIComponent(row.session_id)}`}
                                  class="mono-link"
                                >
                                  {shortID(row.session_id, 18)}
                                </a>
                                <button
                                  type="button"
                                  class="icon-copy"
                                  onClick={() => copyText(row.session_id)}
                                  aria-label={`Copy ${row.session_id}`}
                                >
                                  ⧉
                                </button>
                              </div>
                            </td>
                            <td data-label="Agent">{row.agent_name}</td>
                            <td data-label="Worker">
                              <span class="mono-chip">{row.worker_id || 'unclaimed'}</span>
                            </td>
                            <td data-label="Runtime">
                              <span class="mono-chip">{row.runtime || 'unknown'}</span>
                            </td>
                            <td data-label="Status">
                              <span
                                class={`status-cell${flashMap[row.session_id] ? ' status-cell-flash' : ''}`}
                              >
                                <SessionStatusBadge row={row} />
                              </span>
                            </td>
                            <td data-label="Duration">
                              <span class="tabular-detail">{formatDuration(row.duration)}</span>
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                )}
              </section>
            );
          })}
        </div>
      )}

      <div class="pager-row">
        <button type="button" class="wb-button" disabled={!hasPrev} onClick={() => navigate({ ...filter, offset: Math.max(0, filter.offset - PAGE_LIMIT) })}>
          Prev
        </button>
        <button type="button" class="wb-button" disabled={!hasNext} onClick={() => navigate({ ...filter, offset: filter.offset + PAGE_LIMIT })}>
          Next
        </button>
      </div>
    </Layout>
  );
}

function splitRepo(repo: string): { owner: string; name: string } {
  const [owner = '', name = ''] = repo.split('/', 2);
  return { owner, name };
}

function buildCompareHref(panel: { repo: string; issueNum: number; group: RolloutGroup }): string | undefined {
  const { owner, name } = splitRepo(panel.repo);
  if (!owner || !name || panel.group.members.length < 2) return undefined;
  const params = new URLSearchParams();
  const chosen = panel.group.members.slice(0, 3);
  if (chosen[0]) params.set('a', String(chosen[0].rollout_index));
  if (chosen[1]) params.set('b', String(chosen[1].rollout_index));
  if (chosen[2]) params.set('c', String(chosen[2].rollout_index));
  return `/issues/${owner}/${name}/${panel.issueNum}/rollouts/compare?${params.toString()}`;
}

export default Sessions;
