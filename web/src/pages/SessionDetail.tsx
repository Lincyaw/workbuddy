import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import { useRoute } from 'preact-iso';
import { fetchSession, fetchSessionEvents, sessionStreamURL, type SessionDetail as SessionDetailMeta, type SessionEvent } from '../api/sessions';
import { DegradedSessionCard } from '../components/DegradedSessionCard';
import { EmptyState } from '../components/EmptyState';
import { Layout } from '../components/Layout';
import { StateBadge } from '../components/StateBadge';
import { copyText, formatTimestamp, shortID } from '../lib/format';
import { mergeSessionEvents } from '../utils/sessionEvents';

const KIND_OPTIONS = ['tool_call', 'tool_result', 'message', 'system'] as const;
type KindOption = (typeof KIND_OPTIONS)[number];
const INITIAL_LIMIT = 200;

function classifyKind(kind: string): KindOption | 'other' {
  const value = (kind || '').toLowerCase();
  if (value.includes('tool.call') || value === 'tool_call') return 'tool_call';
  if (value.includes('tool.result') || value === 'tool_result') return 'tool_result';
  if (value.includes('message') || value.includes('reasoning')) return 'message';
  if (value.includes('system') || value.includes('turn.') || value === 'log' || value.includes('token')) return 'system';
  return 'other';
}

function eventTitle(ev: SessionEvent): string {
  const payload = (ev.payload || {}) as Record<string, unknown>;
  const pick = (...keys: string[]): string => {
    for (const key of keys) {
      const value = payload[key];
      if (typeof value === 'string' && value.trim()) return value;
    }
    return '';
  };
  const kind = (ev.kind || '').toLowerCase();
  if (kind.includes('tool.call')) return pick('name', 'tool') || 'tool call';
  if (kind.includes('tool.result')) return pick('name', 'tool') || 'tool result';
  if (kind.includes('message')) return pick('text', 'content') || 'message';
  if (kind === 'reasoning') return pick('summary', 'text') || 'reasoning';
  if (kind === 'command.exec') return pick('command') || 'command';
  if (kind === 'command.output') return pick('chunk', 'output', 'text') || 'command output';
  if (kind === 'error') return pick('message', 'error') || 'error';
  return ev.kind || 'event';
}

function eventIcon(kind: string): string {
  const value = classifyKind(kind);
  if (value === 'tool_call') return 'TC';
  if (value === 'tool_result') return 'TR';
  if (value === 'message') return 'MSG';
  if (value === 'system') return 'SYS';
  return 'EVT';
}

function eventTime(ev: SessionEvent): string {
  if (!ev.ts) return `#${ev.index}`;
  const date = new Date(ev.ts);
  if (Number.isNaN(date.getTime())) return `#${ev.index}`;
  return date.toLocaleTimeString([], { hour12: false });
}

function defaultExpanded(kind: string): boolean {
  const value = (kind || '').toLowerCase();
  return value === 'error';
}

export function SessionDetail() {
  const { params } = useRoute();
  const sessionID = params.id || '';

  const [meta, setMeta] = useState<SessionDetailMeta | null>(null);
  const [metaError, setMetaError] = useState<string | null>(null);
  const [events, setEvents] = useState<SessionEvent[]>([]);
  const [eventsTotal, setEventsTotal] = useState<number | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [follow, setFollow] = useState(true);
  const [streamLive, setStreamLive] = useState(false);
  const [pausedOffset, setPausedOffset] = useState<number | null>(null);
  const [search, setSearch] = useState('');
  const [enabledKinds, setEnabledKinds] = useState<Record<KindOption | 'other', boolean>>({
    tool_call: true,
    tool_result: true,
    message: true,
    system: true,
    other: true,
  });
  const [expanded, setExpanded] = useState<Record<number, boolean>>({});

  const lastIndexRef = useRef(-1);
  const esRef = useRef<EventSource | null>(null);
  const seenRef = useRef<Set<number>>(new Set());
  const timelineRef = useRef<HTMLDivElement | null>(null);
  const followRef = useRef(true);
  followRef.current = follow;

  const closeStream = () => {
    if (esRef.current) {
      esRef.current.close();
      esRef.current = null;
    }
    setStreamLive(false);
  };

  useEffect(() => {
    if (!sessionID) return;
    let aborted = false;
    seenRef.current = new Set();
    lastIndexRef.current = -1;
    setMeta(null);
    setMetaError(null);
    setEvents([]);
    setLoadError(null);

    fetchSession(sessionID)
      .then((detail) => {
        if (!aborted) setMeta(detail);
      })
      .catch((error) => {
        if (!aborted) setMetaError(error instanceof Error ? error.message : 'failed to load session');
      });

    fetchSessionEvents(sessionID, { tail: true, limit: INITIAL_LIMIT })
      .then((data) => {
        if (aborted) return;
        const items = data.events || [];
        for (const event of items) {
          if (event.index > lastIndexRef.current) lastIndexRef.current = event.index;
        }
        setExpanded((current) => {
          const next = { ...current };
          for (const event of items) {
            if (next[event.index] === undefined && defaultExpanded(event.kind)) next[event.index] = true;
          }
          return next;
        });
        setEvents((current) => mergeSessionEvents(current, items, seenRef.current));
        setEventsTotal(typeof data.total === 'number' ? data.total : 0);
      })
      .catch((error) => {
        if (!aborted) setLoadError(error instanceof Error ? error.message : 'failed to load events');
      });

    return () => {
      aborted = true;
      closeStream();
    };
  }, [sessionID]);

  useEffect(() => {
    if (!sessionID) return;
    if (!follow) {
      closeStream();
      return;
    }
    const after = lastIndexRef.current + 1;
    const es = new EventSource(sessionStreamURL(sessionID, after), { withCredentials: true });
    esRef.current = es;
    setStreamLive(true);
    setPausedOffset(null);

    es.addEventListener('evt', (evt: MessageEvent) => {
      try {
        const event: SessionEvent = JSON.parse(evt.data);
        if (seenRef.current.has(event.index)) return;
        if (event.index > lastIndexRef.current) lastIndexRef.current = event.index;
        setEvents((current) => mergeSessionEvents(current, [event], seenRef.current));
        if (followRef.current) {
          requestAnimationFrame(() => {
            timelineRef.current?.scrollTo({ top: timelineRef.current.scrollHeight });
          });
        }
      } catch {
        // ignore malformed stream chunks
      }
    });
    es.addEventListener('error', () => setStreamLive(false));
    return () => {
      es.close();
      if (esRef.current === es) esRef.current = null;
    };
  }, [sessionID, follow]);

  const filteredEvents = useMemo(() => {
    const query = search.trim().toLowerCase();
    return events.filter((event) => {
      const kind = classifyKind(event.kind);
      if (!enabledKinds[kind]) return false;
      if (!query) return true;
      const payloadText = JSON.stringify(event.payload || '').toLowerCase();
      return event.kind.toLowerCase().includes(query) || payloadText.includes(query) || eventTitle(event).toLowerCase().includes(query);
    });
  }, [enabledKinds, events, search]);

  if (!sessionID) {
    return (
      <Layout>
        <div class="wb-panel text-state-danger">missing session id in URL.</div>
      </Layout>
    );
  }

  return (
    <Layout>
      <section class="wb-stack">
        <a href="/sessions" class="wb-backlink">← sessions</a>
        <header class="grid gap-3">
          <p class="wb-section-label">session detail</p>
          <div class="flex flex-wrap items-center gap-3">
            <h1 class="wb-page-title">{shortID(sessionID, 18)}</h1>
            <span class="wb-live-indicator">
              <span class={`wb-live-dot${streamLive ? ' is-live' : ''}`} aria-hidden="true" />
              {streamLive ? 'watch live' : follow ? 'connecting' : 'paused'}
            </span>
          </div>
          <p class="wb-page-copy">Dense event rows, paired tool activity, and a tail-follow toggle for active sessions.</p>
        </header>

        {metaError ? <div class="wb-panel text-state-danger">session metadata: {metaError}</div> : null}
        <DegradedSessionCard meta={meta} eventsTotal={eventsTotal} />

        <section class="wb-detail-grid">
          <aside class="wb-panel wb-detail-sidebar">
            <dl class="wb-kv">
              <dt>session</dt>
              <dd>
                <span class="wb-code-inline">{sessionID}</span>{' '}
                <button type="button" class="wb-faint" onClick={() => copyText(sessionID)}>copy</button>
              </dd>
              <dt>repo</dt>
              <dd>{meta?.repo || '--'}</dd>
              <dt>issue</dt>
              <dd>{meta ? `#${meta.issue_num}` : '--'}</dd>
              <dt>agent</dt>
              <dd>{meta?.agent_name || '--'}</dd>
              <dt>worker</dt>
              <dd>{meta?.worker_id || '--'}</dd>
              <dt>runtime</dt>
              <dd class="wb-mono text-[12px] uppercase">{meta?.runtime || 'unknown'}</dd>
              <dt>started</dt>
              <dd class="wb-time">{meta ? formatTimestamp(meta.created_at) : '--'}</dd>
              <dt>status</dt>
              <dd>{meta ? <StateBadge state={meta.status} /> : '--'}</dd>
              <dt>exit</dt>
              <dd class="wb-num">{meta?.exit_code ?? '--'}</dd>
            </dl>
          </aside>

          <div class="wb-stack">
            <section class="wb-panel wb-session-toolbar">
              {KIND_OPTIONS.map((kind) => (
                <button type="button" class={`wb-chip-button${enabledKinds[kind] ? ' is-active' : ''}`} onClick={() => setEnabledKinds((current) => ({ ...current, [kind]: !current[kind] }))}>
                  {kind.replace('_', ' ')}
                </button>
              ))}
              <button type="button" class={`wb-chip-button${follow ? ' is-active' : ''}`} onClick={() => {
                setFollow((current) => {
                  const next = !current;
                  if (!next) setPausedOffset(lastIndexRef.current);
                  else setPausedOffset(null);
                  return next;
                });
              }}>
                follow tail
              </button>
              <input type="search" placeholder="filter timeline" value={search} onInput={(event) => setSearch((event.target as HTMLInputElement).value)} />
              <button type="button" class="wb-chip-button" onClick={() => setSearch('')}>clear search</button>
            </section>

            {loadError ? <div class="wb-panel text-state-danger">events: {loadError}</div> : null}

            <section class="wb-table-shell">
              <div class="border-b border-border-hairline px-4 py-3">
                <p class="wb-section-label mb-1">timeline</p>
                <div class="flex flex-wrap items-center justify-between gap-3 text-[12px] text-text-secondary">
                  <span>{events.length} total events</span>
                  <span>{filteredEvents.length} visible {pausedOffset != null ? `· paused at offset ${pausedOffset}` : ''}</span>
                </div>
              </div>
              <div class="max-h-[68vh] overflow-auto" ref={timelineRef}>
                {filteredEvents.length === 0 ? (
                  <EmptyState glyph="timeline" title={events.length === 0 ? 'no events yet' : 'no events match these filters'} copy={events.length === 0 ? 'The session exists, but no event payloads have landed yet.' : 'Relax the filters or clear the search to bring rows back.'} />
                ) : (
                  <div class="wb-timeline-rail">
                    {filteredEvents.map((event) => {
                      const isOpen = expanded[event.index] ?? defaultExpanded(event.kind);
                      return (
                        <div key={event.index} class="wb-event-row">
                          <button type="button" class="wb-event-toggle" aria-expanded={isOpen} onClick={() => setExpanded((current) => ({ ...current, [event.index]: !isOpen }))}>
                            <span class="wb-event-icon">{eventIcon(event.kind)}</span>
                            <div class="grid min-w-0 gap-1">
                              <span class="truncate">{eventTitle(event)}</span>
                              <span class="wb-faint text-[12px]">{event.kind}</span>
                            </div>
                            <span class="wb-time">{eventTime(event)}</span>
                          </button>
                          {isOpen ? (
                            <div class="wb-event-body">
                              <pre class="wb-codeblock">{JSON.stringify(event.payload || '(no payload)', null, 2)}</pre>
                            </div>
                          ) : null}
                        </div>
                      );
                    })}
                  </div>
                )}
              </div>
            </section>
          </div>
        </section>
      </section>
    </Layout>
  );
}

export default SessionDetail;
