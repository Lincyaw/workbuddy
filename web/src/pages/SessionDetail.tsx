import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import { useRoute } from 'preact-iso';
import { Layout } from '../components/Layout';
import { DegradedSessionCard } from '../components/DegradedSessionCard';
import { EmptyState } from '../components/EmptyState';
import { GitHubIssueLink } from '../components/GitHubIssueLink';
import { StreamMessageView } from '../components/StreamMessage';
import {
  fetchSession,
  fetchSessionEvents,
  sessionStreamURL,
  type SessionDetail as SessionDetailMeta,
  type SessionEvent,
} from '../api/sessions';
import { buildPrettyTimeline } from '../lib/sessionPretty';
import { copyText, formatClock, formatDuration, formatTimestamp, shortID, statusBadgeClass } from '../lib/format';
import { splitRepoSlug } from '../utils/github';
import { mergeSessionEvents } from '../utils/sessionEvents';

const KIND_OPTIONS = ['tool_call', 'tool_result', 'message', 'system', 'other'] as const;
type KindOption = (typeof KIND_OPTIONS)[number];
const INITIAL_LIMIT = 200;

function classifyKind(kind: string): KindOption {
  const normalized = (kind || '').toLowerCase();
  if (normalized.includes('tool.call') || normalized === 'tool_call' || normalized === 'command.exec') return 'tool_call';
  if (normalized.includes('tool.result') || normalized === 'tool_result') return 'tool_result';
  if (normalized.includes('message') || normalized === 'reasoning' || normalized === 'agent.message') return 'message';
  if (normalized.includes('system') || normalized.includes('turn.') || normalized === 'log' || normalized.includes('token') || normalized === 'permission') return 'system';
  return 'other';
}

function eventTitle(event: SessionEvent): string {
  const payload = (event.payload || {}) as Record<string, unknown>;
  const text = [payload.text, payload.content, payload.message, payload.summary]
    .find((value) => typeof value === 'string');
  if (typeof text === 'string' && text.trim()) {
    return text.replace(/\s+/g, ' ').trim().slice(0, 112);
  }
  if (typeof payload.name === 'string') return payload.name;
  if (typeof payload.path === 'string') return payload.path;
  return event.kind || 'event';
}

function payloadText(payload: unknown): string {
  if (payload == null) return '(no payload)';
  return JSON.stringify(payload, null, 2);
}

function kindIcon(kind: KindOption): string {
  switch (kind) {
    case 'tool_call':
      return '⌁';
    case 'tool_result':
      return '↳';
    case 'message':
      return '·';
    case 'system':
      return '◦';
    default:
      return '—';
  }
}

function turnLabel(turnId: string, index: number): string {
  if (!turnId) return `turn ${index + 1}`;
  return `turn ${shortID(turnId, 12)}`;
}

export function SessionDetail() {
  const { params } = useRoute();
  const sessionID = params.id || '';

  const [meta, setMeta] = useState<SessionDetailMeta | null>(null);
  const [metaError, setMetaError] = useState<string | null>(null);
  // workerOfflineMeta is set when the detail API returns 503 with a
  // worker_offline payload — Phase 3 (REQ-122). We surface a polite
  // info panel instead of falling into the red "session never produced
  // any events" warning, and skip opening the SSE stream that would
  // immediately fail. nil = no offline shape, render normally.
  const [workerOfflineMeta, setWorkerOfflineMeta] = useState<{
    workerID: string;
    degradedReason: string;
  } | null>(null);
  const [events, setEvents] = useState<SessionEvent[]>([]);
  const [eventsTotal, setEventsTotal] = useState<number | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [follow, setFollow] = useState(true);
  const [streamLive, setStreamLive] = useState(false);
  const [search, setSearch] = useState('');
  const [pretty, setPretty] = useState(true);
  const [enabledKinds, setEnabledKinds] = useState<Record<KindOption, boolean>>({
    tool_call: true,
    tool_result: true,
    message: true,
    system: true,
    other: true,
  });
  const [expanded, setExpanded] = useState<Record<number, boolean>>({});
  const timelineRef = useRef<HTMLDivElement | null>(null);
  const followRef = useRef(true);
  const eventSourceRef = useRef<EventSource | null>(null);
  const seenRef = useRef<Set<number>>(new Set());
  const lastIndexRef = useRef(-1);

  followRef.current = follow;

  useEffect(() => {
    if (!sessionID) return;
    let aborted = false;
    setMeta(null);
    setMetaError(null);
    setWorkerOfflineMeta(null);
    setEvents([]);
    setEventsTotal(null);
    setLoadError(null);
    setExpanded({});
    seenRef.current = new Set();
    lastIndexRef.current = -1;

    fetchSession(sessionID)
      .then((response) => {
        if (!aborted) setMeta(response);
      })
      .catch((err) => {
        if (aborted) return;
        // Phase 3 (REQ-122): the proxy emits HTTP 503 with a
        // {degraded_reason: 'worker_offline', worker_id: ...} payload
        // when it cannot reach the owning worker. Render a polite
        // info panel rather than the red degraded card, and avoid
        // opening the SSE stream (which would 503 too).
        const apiErr = err as { status?: number; body?: unknown };
        const body = (apiErr?.body || {}) as Record<string, unknown>;
        const reason = typeof body.degraded_reason === 'string' ? body.degraded_reason : '';
        if (apiErr?.status === 503 && reason === 'worker_offline') {
          setWorkerOfflineMeta({
            workerID: typeof body.worker_id === 'string' ? body.worker_id : '',
            degradedReason: reason,
          });
          return;
        }
        setMetaError(err instanceof Error ? err.message : 'failed to load session');
      });

    fetchSessionEvents(sessionID, { tail: true, limit: INITIAL_LIMIT })
      .then((response) => {
        if (aborted) return;
        for (const event of response.events || []) {
          if (event.index > lastIndexRef.current) lastIndexRef.current = event.index;
        }
        setEvents((current) => mergeSessionEvents(current, response.events || [], seenRef.current));
        setEventsTotal(typeof response.total === 'number' ? response.total : 0);
      })
      .catch((err) => {
        if (aborted) return;
        const apiErr = err as { status?: number };
        // Don't surface the events-side 503 as a separate error — the
        // worker_offline info panel already covers the situation.
        if (apiErr?.status === 503) return;
        setLoadError(err instanceof Error ? err.message : 'failed to load events');
      });

    return () => {
      aborted = true;
      eventSourceRef.current?.close();
    };
  }, [sessionID]);

  useEffect(() => {
    if (!sessionID || !follow || workerOfflineMeta) {
      // Phase 3 (REQ-122): skip the SSE stream entirely when the
      // proxy already told us the worker is unreachable. Otherwise
      // EventSource opens a connection that 503s on the first byte
      // and the SPA flips into a confusing onerror loop.
      eventSourceRef.current?.close();
      setStreamLive(false);
      return;
    }
    const source = new EventSource(sessionStreamURL(sessionID, lastIndexRef.current + 1), {
      withCredentials: true,
    });
    eventSourceRef.current = source;
    source.onopen = () => setStreamLive(true);
    source.addEventListener('evt', (message: MessageEvent) => {
      const event = JSON.parse(message.data) as SessionEvent;
      if (seenRef.current.has(event.index)) return;
      if (event.index > lastIndexRef.current) lastIndexRef.current = event.index;
      setEvents((current) => mergeSessionEvents(current, [event], seenRef.current));
      if (followRef.current) {
        requestAnimationFrame(() => {
          timelineRef.current?.scrollTo({ top: timelineRef.current.scrollHeight, behavior: 'smooth' });
        });
      }
    });
    source.onerror = () => {
      setStreamLive(false);
    };
    return () => {
      source.close();
      if (eventSourceRef.current === source) eventSourceRef.current = null;
    };
  }, [follow, sessionID]);

  const filteredEvents = useMemo(() => {
    const query = search.trim().toLowerCase();
    return events.filter((event) => {
      const kind = classifyKind(event.kind);
      if (!enabledKinds[kind]) return false;
      if (!query) return true;
      return (
        event.kind.toLowerCase().includes(query) ||
        eventTitle(event).toLowerCase().includes(query) ||
        payloadText(event.payload).toLowerCase().includes(query)
      );
    });
  }, [enabledKinds, events, search]);

  const prettyGroups = useMemo(() => {
    const runtime = meta?.runtime === 'codex' ? 'codex' : meta?.runtime === 'claude' ? 'claude' : 'unknown';
    const items = buildPrettyTimeline(filteredEvents, {
      runtime,
      summarizeEvent: eventTitle,
    });
    const groups: Array<{ key: string; turnId: string; items: typeof items }> = [];
    for (const item of items) {
      const previous = groups[groups.length - 1];
      if (!previous || previous.turnId !== item.turnId) {
        groups.push({ key: `${item.turnId || 'turnless'}:${groups.length}`, turnId: item.turnId, items: [item] });
      } else {
        previous.items.push(item);
      }
    }
    return groups;
  }, [filteredEvents, meta?.runtime]);

  if (!sessionID) {
    return (
      <Layout>
        <div class="error-banner">Missing session id in URL.</div>
      </Layout>
    );
  }

  if (workerOfflineMeta) {
    // Phase 3 (REQ-122): render a polite info panel when the proxy
    // already told us the worker is unreachable. We don't have a
    // useful timeline to show, and the red "never produced any
    // events" warning misrepresents the situation.
    return (
      <Layout>
        <section class="page-header page-header-tight">
          <div>
            <p class="page-eyebrow">session trace</p>
            <h1>{shortID(sessionID, 18)}</h1>
          </div>
        </section>
        <div class="surface-card wb-offline-panel" role="status">
          <h2>This worker is currently unreachable</h2>
          <p>
            The session data lives on{' '}
            <code>{workerOfflineMeta.workerID || 'an unknown worker'}</code>,
            which the coordinator could not contact. Come back when the
            worker reconnects — the session timeline will reload
            automatically.
          </p>
          <p class="muted">
            HTTP 503 · degraded_reason: <code>{workerOfflineMeta.degradedReason}</code>
          </p>
        </div>
      </Layout>
    );
  }

  return (
    <Layout>
      <section class="page-header page-header-tight">
        <div>
          <p class="page-eyebrow">session trace</p>
          <h1>{shortID(sessionID, 18)}</h1>
        </div>
      </section>

      {metaError ? <div class="error-banner">Session metadata: {metaError}</div> : null}
      <DegradedSessionCard meta={meta} eventsTotal={eventsTotal} />

      <section class="session-detail-shell">
        <aside class="surface-card session-sidebar">
          <div class="session-sidebar-header">
            <div>
              <p class="section-kicker">watch</p>
              <h2>Metadata</h2>
            </div>
            <button
              type="button"
              class={`signal-toggle${follow ? ' active' : ''}`}
              onClick={() => setFollow((value) => !value)}
            >
              <span class="live-pill-dot" />
              {follow ? (streamLive ? 'watch live' : 'connecting') : 'watch paused'}
            </button>
          </div>

          <dl class="meta-list">
            <dt>Session ID</dt>
            <dd>
              <span class="mono-chip">{shortID(sessionID, 18)}</span>
              <button type="button" class="icon-copy always-visible" onClick={() => copyText(sessionID)}>⧉</button>
            </dd>
            <dt>Status</dt>
            <dd>{meta ? <span class={statusBadgeClass(meta.status)}>{meta.status}</span> : '—'}</dd>
            <dt>Runtime</dt>
            <dd><span class="mono-chip">{meta?.runtime || 'unknown'}</span></dd>
            <dt>Agent</dt>
            <dd>{meta?.agent_name || '—'}</dd>
            <dt>Worker</dt>
            <dd><span class="mono-chip">{meta?.worker_id || 'unclaimed'}</span></dd>
            <dt>Issue</dt>
            <dd>
              {meta ? (
                <span>
                  #{meta.issue_num}{' '}
                  {meta.repo ? (
                    <GitHubIssueLink
                      owner={splitRepoSlug(meta.repo).owner}
                      repo={splitRepoSlug(meta.repo).name}
                      num={meta.issue_num}
                      variant="icon"
                    />
                  ) : null}
                </span>
              ) : '—'}
            </dd>
            <dt>Repo</dt>
            <dd>{meta?.repo || '—'}</dd>
            <dt>Started</dt>
            <dd>{meta?.created_at ? formatTimestamp(meta.created_at, true) : '—'}</dd>
            <dt>Finished</dt>
            <dd>{meta?.finished_at ? formatTimestamp(meta.finished_at, true) : '—'}</dd>
            <dt>Duration</dt>
            <dd>{meta ? formatDuration(meta.duration) : '—'}</dd>
          </dl>
        </aside>

        <section class="session-timeline-column">
          <div class="surface-card timeline-toolbar-card">
            <div class="timeline-toolbar">
              <div class="timeline-toolbar-title">
                <p class="section-kicker">timeline</p>
                <h2>{pretty ? prettyGroups.reduce((count, group) => count + group.items.length, 0) : filteredEvents.length} visible events</h2>
              </div>
              <input
                type="search"
                placeholder="filter events"
                value={search}
                onInput={(event) => setSearch((event.target as HTMLInputElement).value)}
              />
              <button
                type="button"
                class={`filter-pill button-pill${follow ? ' active' : ''}`}
                onClick={() => setFollow((value) => !value)}
              >
                {follow ? 'follow tail armed' : 'follow tail idle'}
              </button>
              <div class="kind-pill-group">
                <button
                  type="button"
                  class={`filter-pill button-pill${pretty ? ' active' : ''}`}
                  onClick={() => setPretty(true)}
                >
                  Pretty
                </button>
                <button
                  type="button"
                  class={`filter-pill button-pill${pretty ? '' : ' active'}`}
                  onClick={() => setPretty(false)}
                >
                  Raw
                </button>
              </div>
              <div class="kind-pill-group">
                {KIND_OPTIONS.map((kind) => (
                  <button
                    type="button"
                    key={kind}
                    class={`filter-pill button-pill${enabledKinds[kind] ? ' active' : ''}`}
                    onClick={() => setEnabledKinds((prev) => ({ ...prev, [kind]: !prev[kind] }))}
                  >
                    {kind}
                  </button>
                ))}
              </div>
            </div>
          </div>

          {loadError ? <div class="error-banner">Events: {loadError}</div> : null}

          <div class="surface-card session-timeline-card">
            {pretty ? (
              prettyGroups.length === 0 ? (
                <EmptyState
                  title="no events matched this view"
                  detail="clear a filter or leave tail-follow armed and wait for the next tool call."
                  inline
                />
              ) : (
                <div class="timeline-list wb-pretty-timeline" ref={timelineRef}>
                  {prettyGroups.map((group, groupIndex) => (
                    <section key={group.key} class="wb-turn-group">
                      <div class="wb-turn-label">{turnLabel(group.turnId, groupIndex)}</div>
                      <div class="wb-turn-stack">
                        {group.items.map((item) => {
                          const lastEvent = item.events[item.events.length - 1];
                          return (
                            <article key={item.key} class={`wb-pretty-entry kind-${classifyKind(item.kind)}`}>
                              <div class="wb-pretty-meta">
                                <span class="mono-chip">{item.kind}</span>
                                <span class="muted">#{item.events[0].index}</span>
                                {item.events.length > 1 ? <span class="muted">+{item.events.length - 1} merged</span> : null}
                                <span class="timeline-time">{formatClock(lastEvent.ts)}</span>
                              </div>
                              <StreamMessageView msg={item.msg} />
                            </article>
                          );
                        })}
                      </div>
                    </section>
                  ))}
                </div>
              )
            ) : filteredEvents.length === 0 ? (
              <EmptyState
                title="no events matched this view"
                detail="clear a filter or leave tail-follow armed and wait for the next tool call."
                inline
              />
            ) : (
              <div class="timeline-list" ref={timelineRef}>
                {filteredEvents.map((event) => {
                  const kind = classifyKind(event.kind);
                  const isOpen = expanded[event.index] === true;
                  return (
                    <article
                      key={event.index}
                      class={`timeline-event kind-${kind}${kind === 'tool_call' || kind === 'tool_result' ? ' tool-linked' : ''}${isOpen ? ' is-open' : ''}`}
                    >
                      <button
                        type="button"
                        class="timeline-row"
                        onClick={() => setExpanded((prev) => ({ ...prev, [event.index]: !prev[event.index] }))}
                        aria-expanded={isOpen}
                      >
                        <span class="timeline-kind">{kindIcon(kind)}</span>
                        <span class="timeline-title">{eventTitle(event) || '(no summary)'}</span>
                        <span class="timeline-time">{formatClock(event.ts)}</span>
                      </button>
                      {isOpen ? (
                        <div class="timeline-body">
                          <div class="timeline-body-meta">
                            <span class="mono-chip">{event.kind}</span>
                            <span class="muted">#{event.index}</span>
                            <span class="muted">seq {event.seq}</span>
                          </div>
                          <pre>{payloadText(event.payload)}</pre>
                        </div>
                      ) : null}
                    </article>
                  );
                })}
              </div>
            )}
          </div>
        </section>
      </section>
    </Layout>
  );
}

export default SessionDetail;
